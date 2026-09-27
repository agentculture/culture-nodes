// Package testslint holds lint-as-Go-test checks that are cheaper to run as
// part of `go test ./...` than to stand up as a separate tool -- the same
// rationale internal/actors/neutrality_test.go documents for the provider-
// neutrality guard: a fast tripwire enforced by `go test`, not a
// sophisticated static-analysis pass.
package testslint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// gitHubControlPlaneBoundary is the core principle: the control-plane
// process (internal/ and cmd/ packages) holds no GitHub credential and never
// calls the GitHub API. The GitHub credential (GITHUB_TOKEN) and API calls
// live only in adapters/human-inbox/tracker.py, outside the deployment
// (spec claim c13, issue #54).
//
// This test scans Go source files in internal/ and cmd/ and fails if any of
// them reference a GitHub credential or the GitHub API host. It allows
// documentation comments that mention these concepts (e.g., explaining why
// they are NOT used), but catches actual code-level references: env var reads,
// API host strings, function/constant names that directly use them.
const gitHubControlPlaneBoundary = "the control-plane process holds no GitHub credential and never calls api.github.com (spec claim c13; tracker lives in adapters/human-inbox)"

// testGitHubIsolationPatterns are regex patterns that detect real usage of
// GitHub credentials or API calls in code. Each pattern matches something
// that would only make sense in control-plane code if someone broke the
// boundary.
//
// The patterns are ordered from highest-confidence (env var reads) to
// lowest (package names). Allow false positives in comments/strings by
// using loose patterns; the test is a tripwire, not a statement of
// confidence in the regex.
var testGitHubIsolationPatterns = []*regexp.Regexp{
	// Env var reads: os.Getenv("GITHUB_TOKEN"), or similar with variants
	regexp.MustCompile(`(?i)(?:os\.Getenv|os\.LookupEnv|Getenv|LookupEnv)\s*\(\s*["']GITHUB_TOKEN["']\s*\)`),
	// Hardcoded GitHub API host: "api.github.com", or similar patterns
	regexp.MustCompile(`["']api\.github\.com["']`),
	regexp.MustCompile(`["']github\.com/repos["']`),
	// Direct auth/client construction hinting at GitHub use: NewClient, WithAuth, etc.
	// (Keep this loose to catch indirect imports too.)
	regexp.MustCompile(`(?:github|GitHub).*Client|github.*API|GitHub.*auth`),
}

// gitHubHighConfidencePatterns is how many leading entries of
// testGitHubIsolationPatterns are the high-confidence ones (a GITHUB_TOKEN
// read, an api.github.com or github.com/repos host). An inbound-webhook
// exemption below lifts only the loose name heuristic after them.
const gitHubHighConfidencePatterns = 3

// testGitHubInboundWebhookExceptions narrows the boundary for exactly one
// kind of file: an INBOUND GitHub webhook receiver. It holds a webhook
// signing secret (the same shape as the Jira receiver's) and verifies
// X-Hub-Signature-256 on requests GitHub sends it; it holds no GitHub API
// credential and makes no outbound call. The owner decided this narrowing
// for issue #328 task t23 (ADR 0014). Such a file is still held to the
// high-confidence patterns, so a GITHUB_TOKEN read or an api.github.com call
// added to it fails this test like anywhere else.
var testGitHubInboundWebhookExceptions = map[string]string{
	"internal/api/githubwebhook.go": "inbound GitHub webhook receiver: signing secret only, no API credential, no outbound call (#328 t23)",
}

// TestGitHubCredentialAndAPIIsolation scans every non-test .go file in
// internal/ and cmd/ (the control-plane process) and fails if any of them
// contain patterns suggesting GitHub credential handling or API calls. The
// tracker and all GitHub-aware code lives in adapters/, outside the
// deployment (spec claim c13, issue #54).
//
// The one exception class is an inbound webhook receiver
// (testGitHubInboundWebhookExceptions), still held to the high-confidence
// patterns; anything else that must mention GitHub stays out of internal/
// and cmd/.
func TestGitHubCredentialAndAPIIsolation(t *testing.T) {
	repoRoot := repoRoot(t)
	scanned := 0
	violations := 0
	var violationsList []string

	// Walk internal/ and cmd/ only
	for _, packageRoot := range []string{"internal", "cmd"} {
		packagePath := filepath.Join(repoRoot, packageRoot)
		if _, err := os.Stat(packagePath); err != nil {
			continue // gracefully handle missing directories
		}
		walkErr := filepath.WalkDir(packagePath, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, relErr := filepath.Rel(repoRoot, path)
			if relErr != nil {
				return relErr
			}
			rel = filepath.ToSlash(rel)

			if d.IsDir() {
				// Skip dotdirs and test-specific subdirs
				if strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			return checkGoFileForGitHubRef(t, path, rel, &scanned, &violations, &violationsList)
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", packagePath, walkErr)
		}
	}

	if scanned == 0 {
		t.Fatal("the GitHub isolation lint scanned no files in internal/ or cmd/; it is not proving anything")
	}

	for _, violation := range violationsList {
		t.Error(violation)
	}

	t.Logf("scanned %d non-test .go files in internal/ and cmd/ for GitHub credential/API references, found %d violation(s)",
		scanned, violations)
}

// checkGoFileForGitHubRef applies the isolation rule to one regular file:
// non-test .go files in internal/ and cmd/ must not contain patterns
// suggesting GitHub credential handling or API calls.
func checkGoFileForGitHubRef(t *testing.T, path, rel string, scanned, violations *int, violationsList *[]string) error {
	t.Helper()
	if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
		return nil
	}
	*scanned++

	content, readErr := os.ReadFile(path)
	if readErr != nil {
		return fmt.Errorf("read %s: %w", rel, readErr)
	}

	// Search for each pattern in the file. An inbound-webhook exception is
	// checked against the high-confidence patterns only.
	patterns := testGitHubIsolationPatterns
	if _, ok := testGitHubInboundWebhookExceptions[rel]; ok {
		patterns = patterns[:gitHubHighConfidencePatterns]
	}
	for _, pattern := range patterns {
		matches := pattern.FindAllString(string(content), -1)
		for _, match := range matches {
			*violations++
			violation := fmt.Sprintf(
				"%s: contains %q, which suggests GitHub credential/API usage. "+
					"GitHub code must live in adapters/human-inbox, not the control-plane process. (%s)",
				rel, match, gitHubControlPlaneBoundary)
			*violationsList = append(*violationsList, violation)
		}
	}
	return nil
}

// TestGitHubInboundWebhookExceptionStillForbidsTheAPI proves the narrowing
// is narrow: an exempted file reading GITHUB_TOKEN or naming api.github.com
// still fails, while its loose "GitHub ... auth" wording does not.
func TestGitHubInboundWebhookExceptionStillForbidsTheAPI(t *testing.T) {
	dir := t.TempDir()
	rel := "internal/api/githubwebhook.go"
	for name, body := range map[string]string{
		"token read": "package api\nvar _ = os.Getenv(\"GITHUB_TOKEN\")\n",
		"api host":   "package api\nconst host = \"api.github.com\"\n",
	} {
		path := filepath.Join(dir, "f.go")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		scanned, violations := 0, 0
		var list []string
		if err := checkGoFileForGitHubRef(t, path, rel, &scanned, &violations, &list); err != nil {
			t.Fatal(err)
		}
		if violations == 0 {
			t.Errorf("%s: exempted webhook file was not flagged", name)
		}
	}
	path := filepath.Join(dir, "g.go")
	if err := os.WriteFile(path, []byte("package api\nconst hint = \"GitHub webhook auth\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scanned, violations := 0, 0
	var list []string
	if err := checkGoFileForGitHubRef(t, path, rel, &scanned, &violations, &list); err != nil {
		t.Fatal(err)
	}
	if violations != 0 {
		t.Errorf("inbound webhook wording flagged: %v", list)
	}
}
