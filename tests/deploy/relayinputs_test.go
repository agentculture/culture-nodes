// Package deploytest -- this file is issue #134's containment, kept in one
// place because the mitigation is a property of the HARNESS rather than of any
// one lane.
//
// The near-miss. deploy/prod/install-secrets.sh relays values it never mints:
// a webhook URL, a GitHub PAT, a Jira Basic-auth pair, the claude bridge's
// bearer. Each is read out of the script's OWN environment and written to a
// host file. During the #128 cycle an agent probed the script against this
// package's fake-ssh cluster with DISCORD_WEBHOOK_URL set in the operator's
// session; the probe wrote the LIVE webhook into its throwaway prod.env files
// and then printed them while verifying its work. A webhook URL is a bearer
// credential -- possession is authorisation to post to that channel. Nothing
// was committed or pushed, and the owner adjudicated it note-and-continue.
//
// CONTAINED, NOT FIXED. The relay itself stays: install-secrets.sh:554 still
// writes `DISCORD_WEBHOOK_URL` from whatever environment invokes it, because
// that is the lane's job -- the script cannot invent a webhook Discord issued.
// What is contained is the TEST HARNESS: every script here runs with an
// environment built from scratch (`scrubbedEnv`), so a probe run through this
// package cannot relay an operator credential no matter what the operator
// holds. A probe run OUTSIDE the harness -- a bare `./install-secrets.sh` in a
// live session -- reproduces the near-miss exactly, and is meant to.
//
// Three guards, each closing a different way the containment could rot:
//
//  1. TestProbingTheLanesRelaysNoLiveOperatorCredential -- the canary. The
//     operator's session is poisoned with values shaped like the live ones;
//     nothing the probe writes anywhere may contain any of them.
//  2. TestEveryEnvironmentDerivedInputIsCanariedOrDeclaredHarmless -- the
//     completeness guard. The canary list above is hand-written, and a new
//     relay lane added to the script would not be in it. So the list is
//     checked against one DERIVED from the scripts themselves: every name they
//     can only get from the invoking environment is either canaried or
//     declared, by name, as a knob that carries no credential.
//  3. TestNoTestPathRunsARelayingScriptWithTheOperatorEnvironment -- the reach
//     guard. A new _test.go file that runs a relaying script with os.Environ()
//     would sit outside the harness while looking like part of it.
package deploytest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// poisonedRelayInputs are the credentials the two relaying scripts read out of
// their own environment, each mapped to a canary value shaped like the live
// one the near-miss involved. Every value here is a marker: it exists only to
// be searched for afterwards, and none of it is a real credential.
//
// NODES_INBOUND_ISSUANCE_TOKEN_SECRET is the odd one out and is here
// deliberately: issue-dialin-credential.sh reads that bearer from the CONTROL
// PLANE HOST's own prod.env and must never relay the operator's copy, so
// poisoning it asserts a relay that should not exist rather than one that
// should be scrubbed.
var poisonedRelayInputs = map[string]string{
	"DISCORD_WEBHOOK_URL":                 "https://discord.example/api/webhooks/live-operator-value",
	"CULTURE_NODES_WEBHOOK_URL":           "https://hooks.example/live-operator-value",
	"NODES_ACTOR_CLAUDE_TOKEN":            "live-operator-claude-token",
	"GITHUB_TOKEN":                        "live-operator-github-token",
	"GITHUB_TOKEN_WORKER":                 "live-operator-github-worker-token",
	"JIRA_API_TOKEN":                      "live-operator-jira-token",
	"JIRA_ACCOUNT_EMAIL":                  "live-operator@example.invalid",
	"NODES_INBOUND_ISSUANCE_TOKEN_SECRET": "live-operator-issuance-secret",
}

// TestProbingTheLanesRelaysNoLiveOperatorCredential is issue #134 in
// executable form, for BOTH scripts that read credentials out of their own
// environment. The operator's session is poisoned with values that look
// exactly like the live ones the near-miss involved; nothing the probe writes
// may contain any of them.
func TestProbingTheLanesRelaysNoLiveOperatorCredential(t *testing.T) {
	for key, value := range poisonedRelayInputs {
		t.Setenv(key, value)
	}

	issuer := newFakeIssuer(t)
	c := dialInCluster(t, issuer)

	if out, code := c.run(t, installSecretsPath(t), []string{"thor", "orin"}); code != 0 {
		t.Fatalf("install-secrets.sh exited %d; output:\n%s", code, out)
	}
	if stdout, stderr, code := runIssue(t, c, issuer, []string{"company/codex-thor"}); code != 0 {
		t.Fatalf("issue exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	for key, value := range poisonedRelayInputs {
		assertAbsentEverywhere(t, c, "the operator's live "+key, value)
	}
}

// --- the completeness guard ----------------------------------------------

// relayingScripts are the deploy/prod scripts a test in this package executes
// that can read a value out of the invoking environment. actor-placement.sh is
// included because install-secrets.sh SOURCES it, so its environment reads are
// install-secrets.sh's own.
func relayingScripts(t *testing.T) []string {
	t.Helper()
	dir := filepath.Dir(installSecretsPath(t))
	return []string{
		filepath.Join(dir, "install-secrets.sh"),
		filepath.Join(dir, "actor-placement.sh"),
		filepath.Join(dir, "issue-dialin-credential.sh"),
	}
}

// envDefaultExpansion matches the ONE idiom these scripts use to read a value
// that may come from outside: `${NAME:-...}`, `${NAME-...}`, `${NAME:=...}`.
//
// That idiom is the whole definition, and deliberately so. All three scripts
// run under `set -u`, so a name that could come from the environment MUST
// carry a default or the script aborts on an unset one -- which makes the
// defaulted names an exhaustive list rather than a heuristic. Matching bare
// `$NAME` instead would sweep in every variable the scripts assign themselves
// (`POSTGRES_PASSWORD=$(gen)` and its five siblings), and those are minted
// here, not relayed.
var envDefaultExpansion = regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*):?[-=]`)

// environmentDerivedNames returns every name *path* can only obtain from the
// environment that invoked it.
func environmentDerivedNames(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	seen := map[string]bool{}
	for _, match := range envDefaultExpansion.FindAllStringSubmatch(string(body), -1) {
		seen[match[1]] = true
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// nonCredentialKnobs are the environment-derived names that carry no
// credential, each with the reason it is harmless. A name is listed here only
// because someone read the lane and decided; the map's job is to make that
// decision explicit and reviewable, so the completeness guard below fails on a
// name nobody has classified rather than on a name nobody has poisoned.
var nonCredentialKnobs = map[string]string{
	// Rotation gates: 0/1 flags read locally and prefixed into a remote
	// command. They authorise a rewrite; they carry nothing.
	"FORCE":             "rotation gate for the codex bridge lane",
	"FORCE_CODEX":       "rotation gate for the codex actor-token lane",
	"FORCE_HUMAN_INBOX": "rotation gate for the human-inbox lane",
	"FORCE_ISSUANCE":    "rotation gate for the issuance-secret lane",
	"FORCE_NOTIFY":      "rotation gate for the notify actor-token lane",
	"FORCE_PROD":        "rotation gate for the generated prod.env block",
	"FORCE_RUNNER":      "rotation gate for the runner lane",

	// The destructive-confirmation protocol: where the confirmation file is
	// read from and how long a verdict stays valid.
	"CONFIRM_DIR":            "directory the destructive-confirmation file is read from",
	"CONFIRM_WINDOW_SECONDS": "how long a confirmation verdict stays valid",

	// Targeting: which actor, which host, which control plane. These decide
	// WHERE a secret is installed, never WHAT is installed.
	"CLAUDE_PUSH_ACTOR_KEY":     "actor key whose registered host receives the push credential",
	"HUMAN_INBOX_ACTOR_KEY":     "actor key whose registered host receives the human-inbox secret",
	"HUMAN_INBOX_HOST":          "bootstrap override for the human-inbox host, before an actor row exists",
	"NODES_API_URL":             "control-plane base URL the actor registry is read from",
	"NODES_API_TIMEOUT_SECONDS": "how long an actor-registry read may take",
	"NODES_CONTROL_HOST":        "ssh target of the control-plane host",
	"DIALIN_CONTROL_PLANE_URL":  "control-plane base URL the dial-in lane mints against",
	"DIALIN_DESTINATION":        "which bridge host the issued dial-in credential is delivered to",
	"DIALIN_HOST":               "ssh target the dial-in lane delivers to",
	"DIALIN_PREFIX":             "name prefix of the per-bridge dial-in env file",
}

// TestEveryEnvironmentDerivedInputIsCanariedOrDeclaredHarmless is the guard
// that keeps acceptance criterion 2 true after the next lane is written.
//
// The canary list is hand-maintained, and issue #134's own text names only
// DISCORD_WEBHOOK_URL -- while the scripts today relay seven values, and a
// future lane will relay an eighth. So the list is not trusted: it is checked
// against the scripts. Every name they can only get from the environment must
// be either poisoned (and therefore proven absent from everything a probe
// writes) or declared, by name and with a reason, as a knob carrying no
// credential. A new relay whose name is in neither map fails here, at the
// moment it is added, rather than in a transcript months later.
func TestEveryEnvironmentDerivedInputIsCanariedOrDeclaredHarmless(t *testing.T) {
	for _, script := range relayingScripts(t) {
		name := filepath.Base(script)
		for _, input := range environmentDerivedNames(t, script) {
			if _, canaried := poisonedRelayInputs[input]; canaried {
				continue
			}
			if _, declared := nonCredentialKnobs[input]; declared {
				continue
			}
			t.Errorf("%s reads %s from the environment that invokes it, and the harness has not "+
				"classified it: add it to poisonedRelayInputs if it can carry a credential (so a "+
				"probe is proven not to relay it), or to nonCredentialKnobs with the reason it "+
				"cannot (issue #134)", name, input)
		}
	}
}

// TestTheCanaryListIsNotPaddedWithNamesNothingReads is the guard's other
// direction. A canary for a name no script reads proves nothing and makes the
// list look more thorough than it is -- the failure mode where a relay is
// renamed and the old canary keeps passing over a lane that no longer exists.
//
// NODES_INBOUND_ISSUANCE_TOKEN_SECRET is the one deliberate exception, and the
// exception is the assertion: it is poisoned precisely BECAUSE no script may
// read it from the environment (issue-dialin-credential.sh reads it on the
// control-plane host). If it ever appears in a script's environment reads,
// TestEveryEnvironmentDerivedInputIsCanariedOrDeclaredHarmless still passes
// and this test still passes -- but TestIssuanceSecretNeverLeavesTheControlPlaneHost
// fails, which is where that fact belongs.
func TestTheCanaryListIsNotPaddedWithNamesNothingReads(t *testing.T) {
	const readOnTheControlPlaneHost = "NODES_INBOUND_ISSUANCE_TOKEN_SECRET"
	read := map[string]bool{readOnTheControlPlaneHost: true}
	for _, script := range relayingScripts(t) {
		for _, input := range environmentDerivedNames(t, script) {
			read[input] = true
		}
	}
	for input := range poisonedRelayInputs {
		if !read[input] {
			t.Errorf("the harness poisons %s, but no deploy script reads it from its environment "+
				"any more; a canary over a lane that no longer exists passes without proving "+
				"anything (issue #134)", input)
		}
	}
}

// --- the reach guard ------------------------------------------------------

// relayRisk is what one _test.go file's AST says about the two facts that
// matter: does it name a credential-relaying deploy script, and does it inherit
// the operator's environment. Split out of the test below so the walk is one
// small function with one job -- the test then reads as the rule it enforces.
type relayRisk struct {
	namesARelayingScript   bool
	inheritsTheEnvironment bool
}

// inspectRelayRisk parses one file and reports both facts.
//
// Parsed rather than grepped: every file here DISCUSSES the hazard in prose,
// and a scan that could not tell a comment from a call would flag the comment
// explaining why the call is forbidden.
func inspectRelayRisk(t *testing.T, path string) relayRisk {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var risk relayRisk
	ast.Inspect(parsed, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.Ident:
			if n.Name == "installSecretsPath" || n.Name == "issueDialInPath" {
				risk.namesARelayingScript = true
			}
		case *ast.SelectorExpr:
			if n.Sel.Name != "Environ" {
				return true
			}
			if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name == "os" {
				risk.inheritsTheEnvironment = true
			}
		}
		return true
	})
	return risk
}

// TestNoTestPathRunsARelayingScriptWithTheOperatorEnvironment is the harness
// half of #134's fix, and the reason the issue says the mitigation belongs
// here rather than in each agent brief. install-secrets.sh and
// issue-dialin-credential.sh both read credentials out of their own
// environment; a test that hands them os.Environ() hands them whatever the
// operator running `go test` happens to hold.
func TestNoTestPathRunsARelayingScriptWithTheOperatorEnvironment(t *testing.T) {
	dir := deployTestDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		risk := inspectRelayRisk(t, filepath.Join(dir, entry.Name()))
		if risk.namesARelayingScript && risk.inheritsTheEnvironment {
			t.Errorf("%s runs a credential-relaying deploy script and inherits the operator's "+
				"environment; build it from scratch (scrubbedEnv / fakeCluster.env) so a probe "+
				"cannot relay a live operator credential into a throwaway file (issue #134)",
				entry.Name())
		}
	}
}
