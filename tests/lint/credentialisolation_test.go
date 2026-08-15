// Package testslint holds lint-as-Go-test checks that are cheaper to run as
// part of `go test ./...` than to stand up as a separate tool -- the same
// rationale internal/actors/neutrality_test.go documents for the provider-
// neutrality guard: a fast tripwire enforced by `go test`, not a
// sophisticated static-analysis pass.
package testslint

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// committedIdentityBoundary is the core principle spec claim c25 turns into a
// boundary: a committed file carries no real account identity and no live
// credential. Recorded fixtures are DEMOS others load, read and re-record --
// examples/pr-upkeep/fixtures/sonarcloud-issues.json's four `assignee` values
// and adapters/human-inbox/tests/test_tracker.py's three reply-author logins
// were personal handles in committed files until the v0.17.0 scrub replaced
// them with neutral placeholders.
//
// The scrub was a one-time edit; this lint is the durable half. It fails a
// build the moment a new fixture, doc or test re-introduces an account email,
// a credential-shaped API token, or a known personal handle.
const committedIdentityBoundary = "committed files carry no real account identity and no live credential " +
	"(spec claim c25; the v0.17.0 fixture scrub is the one-time edit, this lint is the durable half)"

// Rule names, used in failure messages and by the planted-fixture tests below
// so a rename shows up as a compile error rather than a silently unasserted
// string.
const (
	ruleAccountEmail   = "account-email"
	ruleAPIToken       = "api-token"
	rulePersonalHandle = "personal-handle"
)

// identifierRule is one tripwire: a named pattern, an optional allowance for
// matches that are placeholders rather than identities, and the remediation
// line the failure prints.
type identifierRule struct {
	name    string
	pattern *regexp.Regexp
	// allow reports whether a raw match is a placeholder this rule
	// deliberately tolerates. nil means every match is a violation.
	allow  func(match string) bool
	remedy string
}

// identifierFinding is one rule firing at one line of one file. The excerpt is
// redacted (see redactMatch) so a failing CI log never reprints the identity or
// credential in full -- printing it would be the leak this lint exists to stop.
type identifierFinding struct {
	rule    string
	line    int
	excerpt string
	remedy  string
}

// accountEmailPattern matches an email-shaped string. It is deliberately broad
// -- every real mail domain is in scope -- and narrowed afterwards by
// emailDomainIsReserved rather than by an allowlist of "personal" providers,
// because a work address at a company domain is an account identity too.
var accountEmailPattern = regexp.MustCompile(`(?i)[a-z0-9._%+-]+@[a-z0-9-]+(?:\.[a-z0-9-]+)*\.[a-z]{2,}`)

// reservedEmailDomains are the domains a committed fixture may legitimately
// use, because they can never route to a real mailbox: RFC 2606's reserved
// example domains and second-level labels, RFC 6761's special-use names, and
// `.internal`, the ICANN-reserved private-use TLD (internal/runners's
// registry_test.go builds endpoint URLs under it). Matched as the domain
// itself or as any subdomain of it.
var reservedEmailDomains = []string{
	"example.com", "example.net", "example.org", "example.edu",
	"example", "invalid", "test", "localhost", "local", "internal",
}

// protocolLocalParts are email-SHAPED strings that are not addresses at all:
// they are the fixed usernames git puts before the host in a remote URL.
// `git@github.com` is the SSH user every GitHub clone URL carries, and
// `x-access-token@github.com` is the HTTPS user a token-authenticated push
// carries (scripts/verify-token-scope.sh documents that form, and the
// own-the-work-end-to-end spec cites it). Neither can route to a mailbox and
// neither identifies a person, so treating them as account identity would make
// the rule fire on the very documentation that teaches the safe push command.
//
// Kept as an exact set of full local@domain strings rather than a bare
// local-part allowance: `git@` at some other domain IS a plausible real
// address, and this rule should still catch it.
var protocolLocalParts = map[string]bool{
	"git@github.com":            true,
	"x-access-token@github.com": true,
}

// emailDomainIsReserved reports whether an email-shaped match cannot be a real
// account -- either because its domain can never route, or because the match is
// one of the git protocol usernames above.
func emailDomainIsReserved(match string) bool {
	if protocolLocalParts[strings.ToLower(match)] {
		return true
	}
	at := strings.LastIndex(match, "@")
	if at < 0 {
		return false
	}
	domain := strings.ToLower(match[at+1:])
	for _, reserved := range reservedEmailDomains {
		if domain == reserved || strings.HasSuffix(domain, "."+reserved) {
			return true
		}
	}
	return false
}

// apiTokenPattern matches a credential-SHAPED token, not a bare prefix: each
// alternative requires a long run of token characters after the prefix. That
// length requirement is the whole design of this rule, and it is what lets the
// same tree hold both the gate and its own documentation --
// adapters/human-inbox/tests/test_tracker.py's `ghp_example`, deploy/prod's
// `GITHUB_TOKEN='ghp_...'`, and task t3's own plan entry naming
// "ATATT/gho_/ghp_ token prefixes" are placeholders and prose, not secrets, and
// a rule that reddened on the prefix alone would fail over its own README the
// day it merged.
//
// The prefixes: GitHub's classic PAT/OAuth/user-to-server/server-to-server/
// refresh family (36-character bodies), GitHub's fine-grained `github_pat_`,
// and Atlassian's `ATATT` API tokens (the Jira Cloud credential issue #76's
// node-loop will need). 20 characters is the floor -- comfortably under every
// real body length, comfortably over every placeholder in the tree.
var apiTokenPattern = regexp.MustCompile(
	`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|ATATT[A-Za-z0-9_=+/-]{20,})`)

// personalHandleDenylist names the account handles that have actually leaked
// into committed fixtures here, lowercase; personalHandlePattern matches them
// case-insensitively along with any handle suffix, so one entry covers
// `OriNachum`, `orinach` and `OriNachum-g2k3F` alike.
//
// A handle has no distinguishing shape, so a denylist is the only mechanism
// available -- and naming one here does not leak anything, since the entry is
// the public account this repo's own commit history is authored by. The job is
// to stop the NEXT fixture from carrying it. Deliberately narrow: it matches
// handle-shaped tokens, not prose mentions of a person's first name, which
// several .devague frames make legitimately when describing who approves work.
var personalHandleDenylist = []string{"orinach"}

var personalHandlePattern = regexp.MustCompile(
	`(?i)\b(?:` + strings.Join(personalHandleDenylist, "|") + `)[a-z0-9_-]*`)

// credentialIdentifierRules is the rule set the scan applies. Loose patterns
// are deliberate, as in the neighbouring isolation lints: this is a tripwire a
// reviewer adjudicates, not a proof that the tree holds no secret.
var credentialIdentifierRules = []identifierRule{
	{
		name:    ruleAccountEmail,
		pattern: accountEmailPattern,
		allow:   emailDomainIsReserved,
		remedy: "recorded fixtures use a neutral placeholder or a reserved domain " +
			"(example.com, RFC 2606), never a routable account address",
	},
	{
		name:    ruleAPIToken,
		pattern: apiTokenPattern,
		remedy: "credentials are read from the environment at run time, never committed; " +
			"rotate this token immediately if it was ever real, then replace it with an " +
			"inert placeholder such as ghp_example",
	},
	{
		name:    rulePersonalHandle,
		pattern: personalHandlePattern,
		remedy: "fixture logins and assignees use neutral placeholders (human-reviewer, " +
			"maintainer@github), never a real account handle",
	},
}

// scanForCommittedIdentifiers applies every rule to content, line by line, and
// returns one finding per match. Line-by-line (rather than whole-file) scanning
// is what makes the failure message a `file:line` a reviewer can jump to.
func scanForCommittedIdentifiers(content string) []identifierFinding {
	var findings []identifierFinding
	for index, line := range strings.Split(content, "\n") {
		for _, rule := range credentialIdentifierRules {
			for _, match := range rule.pattern.FindAllString(line, -1) {
				if rule.allow != nil && rule.allow(match) {
					continue
				}
				findings = append(findings, identifierFinding{
					rule:    rule.name,
					line:    index + 1,
					excerpt: redactMatch(match),
					remedy:  rule.remedy,
				})
			}
		}
	}
	return findings
}

// redactMatch renders a match for a failure message without reprinting it: the
// first few characters plus a length, which is enough to recognize what fired
// next to the `file:line` that locates it exactly. Every rule's pattern is
// ASCII-only, so slicing by byte cannot split a rune here.
func redactMatch(match string) string {
	const keep = 4
	if len(match) <= keep {
		return fmt.Sprintf("%s (%d chars)", strings.Repeat("*", len(match)), len(match))
	}
	return fmt.Sprintf("%s... (%d chars)", match[:keep], len(match))
}

// TestCredentialLintFlagsPlantedIdentities is the lint's own red test: each
// fixture below is a line that would be a genuine leak in a committed file, and
// each must be caught by the named rule. Without this, a regex typo would turn
// the whole check into a green no-op over a tree that happens to be clean.
//
// The token fixtures are assembled at run time rather than written as literals
// so this file itself never carries a credential-shaped string -- both because
// that is the rule it enforces, and because upstream secret scanners (GitHub
// push protection, gitleaks) reasonably object to one. The email and handle
// fixtures stay literal: they are readable that way, and the handle is named by
// personalHandleDenylist a few lines up regardless.
func TestCredentialLintFlagsPlantedIdentities(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fixture  string
		wantRule string
	}{
		{
			name:     "account email in a recorded fixture",
			fixture:  `      "author": "some.person@gmail.com",`,
			wantRule: ruleAccountEmail,
		},
		{
			name:     "account email at a company domain",
			fixture:  `git config user.email "some.person@agentculture.org"`,
			wantRule: ruleAccountEmail,
		},
		{
			// The protocolLocalParts allowance is an exact full-address set,
			// not a bare `git@` local-part rule -- so a plausible real mailbox
			// that happens to start `git@` must still trip. Without this case
			// the allowance could be loosened to a prefix match and no test
			// would notice.
			name:     "git@ at a domain that is not github.com",
			fixture:  `Contact: git@agentculture.org`,
			wantRule: ruleAccountEmail,
		},
		{
			name:     "github classic personal access token",
			fixture:  "GITHUB_TOKEN=" + plantedToken("ghp_", 36),
			wantRule: ruleAPIToken,
		},
		{
			name:     "github oauth token",
			fixture:  `{"token": "` + plantedToken("gho_", 36) + `"}`,
			wantRule: ruleAPIToken,
		},
		{
			name:     "github fine-grained personal access token",
			fixture:  "GITHUB_TOKEN=" + plantedToken("github_pat_", 40),
			wantRule: ruleAPIToken,
		},
		{
			name:     "atlassian api token",
			fixture:  "JIRA_API_TOKEN=" + plantedToken("ATATT", 60),
			wantRule: ruleAPIToken,
		},
		{
			name:     "personal handle as a fixture login",
			fixture:  `        {"user": {"login": "OriNachum"}, "body": "human comment"},`,
			wantRule: rulePersonalHandle,
		},
		{
			name:     "personal handle as a sonarcloud assignee",
			fixture:  `      "assignee": "OriNachum-g2k3F@github",`,
			wantRule: rulePersonalHandle,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			findings := scanForCommittedIdentifiers(tc.fixture)
			if len(findings) == 0 {
				t.Fatalf("the lint found nothing in a planted fixture it must catch; "+
					"expected rule %q to fire (%s)", tc.wantRule, committedIdentityBoundary)
			}
			for _, finding := range findings {
				if finding.rule == tc.wantRule {
					return
				}
			}
			t.Fatalf("planted fixture fired %v, want rule %q", findingRules(findings), tc.wantRule)
		})
	}
}

// TestCredentialLintAcceptsNeutralPlaceholders pins the other edge. Every
// fixture below is a line the committed tree actually carries today -- the
// placeholders the v0.17.0 scrub landed, the reserved-domain git identities the
// adapter suites configure, the obviously-inert token placeholders in
// documentation, and the prose that names token prefixes without carrying one.
// A tightening that turns any of these red would make the lint noise, and noise
// is how a tripwire gets disabled.
func TestCredentialLintAcceptsNeutralPlaceholders(t *testing.T) {
	for _, fixture := range []string{
		// The v0.17.0 scrub's replacements.
		`      "assignee": "maintainer@github",`,
		`        {"user": {"login": "human-reviewer"}, "body": "human comment"},`,
		"live probes show user@host accepts the current key",
		// Reserved documentation domains (RFC 2606 / RFC 6761), used as git
		// identities by every adapter conformance kit.
		`git config user.email "conformance@example.com"`,
		`    _git(repo, "config", "user.email", "preserve-test@example.com")`,
		`			identity: with(func(s *runners.ServiceIdentity) { s.Endpoint = "https://user:pass@runner.thor.internal" }),`,
		// Inert token placeholders: a prefix with no credential-shaped body.
		`    cfg = tracker.TrackerConfig(github_token="ghp_example")`,
		"GITHUB_TOKEN='ghp_...' \\",
		// Prose naming the prefixes this lint looks for -- including this
		// task's own plan entry, which must not trip the lint it describes.
		"Patterns: account emails, ATATT/gho_/ghp_ token prefixes, known personal handles.",
		// Git's protocol usernames. Email-shaped, but they name a transport
		// role rather than a person, and the documentation that teaches the
		// safe push command cannot be written without them.
		"git clone git@github.com:agentculture/culture-nodes.git",
		`git push https://x-access-token@github.com/agentculture/culture-nodes.git owe/batch`,
	} {
		t.Run(fixture, func(t *testing.T) {
			if findings := scanForCommittedIdentifiers(fixture); len(findings) != 0 {
				t.Errorf("neutral placeholder flagged by %v; the tree carries this line today, "+
					"so flagging it makes the lint noise rather than a gate", findingRules(findings))
			}
		})
	}
}

// TestNoCommittedCredentialOrPersonalIdentifier is the gate: it scans every
// committed text file in the repo and fails on any account email, API token or
// known personal handle. It walks `git ls-files` rather than the filesystem
// because the rule is about what is COMMITTED -- a filesystem walk would sweep
// in .venv/, web/node_modules/ and every other ignored tree, whose third-party
// package metadata is full of real author emails that are neither this repo's
// leak nor this repo's to fix.
func TestNoCommittedCredentialOrPersonalIdentifier(t *testing.T) {
	repoRoot := repoRoot(t)
	scanned := 0
	violations := 0

	for _, rel := range committedFiles(t, repoRoot) {
		if skipCommittedPath(rel) {
			continue
		}
		content, readErr := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if readErr != nil {
			if os.IsNotExist(readErr) {
				// Tracked in the index but absent from the worktree (a
				// staged deletion, a sparse checkout). Nothing to scan.
				continue
			}
			t.Fatalf("read %s: %v", rel, readErr)
		}
		if isBinaryContent(content) {
			continue
		}
		scanned++

		for _, finding := range scanForCommittedIdentifiers(string(content)) {
			violations++
			t.Errorf("%s:%d: %s match %s -- %s (%s)",
				rel, finding.line, finding.rule, finding.excerpt, finding.remedy, committedIdentityBoundary)
		}
	}

	if scanned == 0 {
		t.Fatal("the credential/identifier lint scanned no committed files; it is not proving anything")
	}
	t.Logf("scanned %d committed text files for account emails, API tokens and personal handles, found %d violation(s)",
		scanned, violations)
}

// scrubbedFixturePaths are the three committed files the v0.17.0 scrub edited.
// TestCredentialLintCoversTheScrubbedFixtures proves the walk above actually
// reaches them, so "the lint passes over the current tree" means the tree is
// clean rather than that the scan never looked -- the same anti-staleness job
// TestSanctionedPackagesActuallyExist does for the AWS allowlist. A rename
// fails here loudly instead of quietly shrinking the lint's reach.
var scrubbedFixturePaths = []string{
	"examples/pr-upkeep/fixtures/sonarcloud-issues.json",
	"adapters/human-inbox/tests/test_tracker.py",
	"tests/test_pr_upkeep_sweep.py",
}

// TestCredentialLintCoversTheScrubbedFixtures checks both the three specific
// scrubbed files and the two whole classes task t3 names -- examples/ fixtures
// and adapters/*/tests -- are inside the scanned set.
func TestCredentialLintCoversTheScrubbedFixtures(t *testing.T) {
	repoRoot := repoRoot(t)

	scannable := scannableCommittedPaths(t, repoRoot)
	examples, adapterTests := 0, 0
	for rel := range scannable {
		if strings.HasPrefix(rel, "examples/") {
			examples++
		}
		if isAdapterTestPath(rel) {
			adapterTests++
		}
	}

	for _, rel := range scrubbedFixturePaths {
		if !scannable[rel] {
			t.Errorf("scrubbed fixture %q is not in the scanned set; either it moved (update "+
				"scrubbedFixturePaths) or the walk stopped reaching it (%s)", rel, committedIdentityBoundary)
		}
	}
	if examples == 0 {
		t.Error("no committed file under examples/ was scanned; task t3 requires the walk to cover examples/ fixtures")
	}
	if adapterTests == 0 {
		t.Error("no committed file under adapters/*/tests was scanned; task t3 requires the walk to cover adapter test fixtures")
	}
	t.Logf("scanned %d committed files under examples/ and %d under adapters/*/tests", examples, adapterTests)
}

// scannableCommittedPaths is the set of committed paths the credential lint
// actually reads: skip-listed paths and binaries drop out. Split from its
// caller so the "did the walk reach what it must" assertions read as a flat
// list rather than sitting under the walk's own filtering.
func scannableCommittedPaths(t *testing.T, repoRoot string) map[string]bool {
	t.Helper()
	scannable := map[string]bool{}
	for _, rel := range committedFiles(t, repoRoot) {
		if skipCommittedPath(rel) {
			continue
		}
		content, readErr := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if readErr != nil || isBinaryContent(content) {
			continue
		}
		scannable[rel] = true
	}
	return scannable
}

// isAdapterTestPath reports whether rel is a fixture under adapters/*/tests —
// one of the two trees task t3 requires the walk to cover.
func isAdapterTestPath(rel string) bool {
	segments := strings.Split(rel, "/")
	return len(segments) > 3 && segments[0] == "adapters" && segments[2] == "tests"
}

// committedFiles returns every path in the git index, repo-relative and
// forward-slash. It fails the test rather than degrading to an empty list when
// git is unavailable: a credential lint that silently scans nothing is worse
// than no lint, because it reports green.
func committedFiles(t *testing.T, repoRoot string) []string {
	t.Helper()
	cmd := exec.Command("git", "-C", repoRoot, "ls-files", "-z")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files in %s: %v (%s); this lint scans the committed tree, so it fails "+
			"loudly rather than reporting green over nothing", repoRoot, err, strings.TrimSpace(stderr.String()))
	}

	var files []string
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel != "" {
			files = append(files, filepath.ToSlash(rel))
		}
	}
	if len(files) == 0 {
		t.Fatalf("git ls-files in %s listed no files; this lint is not proving anything", repoRoot)
	}
	return files
}

// skipCommittedPath excludes this lint's own package. tests/lint necessarily
// names the handles it denies and carries the planted fixtures that prove the
// rules fire, so scanning it would guarantee a violation. awsisolation_test.go's
// skipDirDecision excludes the same directory for the same reason.
func skipCommittedPath(rel string) bool {
	return rel == "tests/lint" || strings.HasPrefix(rel, "tests/lint/")
}

// isBinaryContent reports whether content looks binary, using git's own
// heuristic: a NUL byte inside the first 8000 bytes. Committed PNGs and the
// like are skipped rather than regex-scanned, where a byte run could match a
// rule by accident.
func isBinaryContent(content []byte) bool {
	const head = 8000
	if len(content) > head {
		content = content[:head]
	}
	return bytes.IndexByte(content, 0) >= 0
}

// plantedToken builds a credential-shaped token body of length bodyLen after
// prefix, at run time, so no committed line of this file is itself a
// credential-shaped literal.
func plantedToken(prefix string, bodyLen int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	body := make([]byte, bodyLen)
	for i := range body {
		body[i] = alphabet[i%len(alphabet)]
	}
	return prefix + string(body)
}

// findingRules names the rules a scan fired, for failure messages.
func findingRules(findings []identifierFinding) []string {
	names := make([]string, 0, len(findings))
	for _, finding := range findings {
		names = append(names, finding.rule)
	}
	return names
}
