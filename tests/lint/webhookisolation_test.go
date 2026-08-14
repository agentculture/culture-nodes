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
	"sort"
	"strings"
	"testing"
)

// webhookControlPlaneBoundary is the core principle issue #68 introduces
// alongside c13's GitHub boundary: the control-plane process (internal/ and
// cmd/ packages) holds no Discord webhook credential and never posts to a
// Discord webhook host directly. The webhook URL (CULTURE_NODES_WEBHOOK_URL
// / DISCORD_WEBHOOK_URL) and the transport that posts it live only in the
// two already-approved lanes: internal/notify (task t13's Go port of
// devex's webhook design, consumed by internal/notifier's out-of-process
// SSE daemon, cmd/nodes-notifier -- task t14) and adapters/notify (issue
// #68's actor-protocol bridge, outside the deployment entirely). Every
// other internal/ or cmd/ package must never gain webhook egress of its
// own -- that would be exactly the control-plane-holds-Discord-egress
// regression issue #68's design section exists to prevent.
const webhookControlPlaneBoundary = "the control-plane process holds no Discord webhook egress " +
	"outside internal/notify and internal/notifier (issue #68; tasks t13/t14); a workflow-step " +
	"send lives in adapters/notify, outside the deployment"

// webhookSanctionedPackageDirs are the only repo-relative package
// directories (forward-slash, relative to the repo root) permitted to
// reference a Discord webhook env var, a Discord webhook host, or a
// Discord webhook path -- the two lanes issue #68's design section names
// as already-approved:
//
//   - internal/notify -- the webhook transport (task t13's Go port of
//     devex's design): env-resolved URL, one bounded 5s POST, no retries,
//     no redirects, fail-open.
//   - internal/notifier -- the out-of-process run-lifecycle daemon (task
//     t14) that calls internal/notify.Notify over the cross-run SSE feed.
//
// adapters/notify (issue #68's actor-protocol bridge) is NOT listed here:
// it is Python, outside this Go module's tree entirely, and holds the
// credential precisely BECAUSE it runs outside the control-plane process
// -- the same reason adapters/human-inbox sits outside
// tests/lint/github_isolation_test.go's sanctioned-package list rather
// than inside it.
var webhookSanctionedPackageDirs = map[string]bool{
	"internal/notify":   true,
	"internal/notifier": true,
}

// webhookIsolationPatterns are regex patterns that detect real webhook
// egress in Go source: reading one of the two webhook env vars, or
// referencing a Discord webhook host/path directly. Each pattern matches
// something that would only make sense in control-plane code if someone
// broke the boundary. Loose patterns are deliberate (a comment or string
// literal can false-positive); this is a tripwire, not a proof.
var webhookIsolationPatterns = []*regexp.Regexp{
	// Env var reads: os.Getenv("CULTURE_NODES_WEBHOOK_URL") / ("DISCORD_WEBHOOK_URL"),
	// or the LookupEnv variant.
	regexp.MustCompile(`(?:os\.Getenv|os\.LookupEnv)\s*\(\s*["'](?:CULTURE_NODES_WEBHOOK_URL|DISCORD_WEBHOOK_URL)["']\s*\)`),
	// Hardcoded Discord webhook hosts (internal/notify/webhook.go's own
	// discordHosts set).
	regexp.MustCompile(`["'](?:discord\.com|discordapp\.com|ptb\.discord\.com|canary\.discord\.com)["']`),
	// The Discord webhook path segment, wherever it shows up.
	regexp.MustCompile(`["'][^"']*/api/webhooks/[^"']*["']`),
}

// TestWebhookEgressIsolation scans every non-test .go file in internal/ and
// cmd/ (the control-plane process) and fails if any of them outside
// webhookSanctionedPackageDirs contain patterns suggesting Discord webhook
// credential handling or a direct Discord webhook host/path reference.
// Mirrors TestGitHubCredentialAndAPIIsolation's shape, generalized with a
// sanctioned-directory allowlist (github_isolation_test.go has none because
// GitHub credentials are sanctioned nowhere in internal/ or cmd/; this
// check needs one because the run-lifecycle notifier -- t13/t14 -- is a
// deliberate, already-reviewed exception).
func TestWebhookEgressIsolation(t *testing.T) {
	repoRoot := repoRoot(t)
	scanned := 0
	violations := 0
	var violationsList []string

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
				if strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			return checkGoFileForWebhookEgress(t, path, rel, &scanned, &violations, &violationsList)
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", packagePath, walkErr)
		}
	}

	if scanned == 0 {
		t.Fatal("the webhook isolation lint scanned no files in internal/ or cmd/; it is not proving anything")
	}

	for _, violation := range violationsList {
		t.Error(violation)
	}

	t.Logf("scanned %d non-test .go files in internal/ and cmd/ (outside %s) for Discord webhook egress, found %d violation(s)",
		scanned, sortedWebhookSanctionedDirs(), violations)
}

// checkGoFileForWebhookEgress applies the isolation rule to one regular
// file: a non-test .go file in internal/ or cmd/, outside the sanctioned
// package dirs, must not contain patterns suggesting Discord webhook
// egress.
func checkGoFileForWebhookEgress(t *testing.T, path, rel string, scanned, violations *int, violationsList *[]string) error {
	t.Helper()
	if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
		return nil
	}
	*scanned++

	dir := filepath.ToSlash(filepath.Dir(rel))
	if webhookSanctionedPackageDirs[dir] {
		return nil
	}

	content, readErr := os.ReadFile(path)
	if readErr != nil {
		return fmt.Errorf("read %s: %w", rel, readErr)
	}

	for _, pattern := range webhookIsolationPatterns {
		matches := pattern.FindAllString(string(content), -1)
		for _, match := range matches {
			*violations++
			violation := fmt.Sprintf(
				"%s: contains %q, which suggests Discord webhook egress. "+
					"Webhook code must live in internal/notify, internal/notifier, or "+
					"adapters/notify (outside the control-plane process), not here. (%s)",
				rel, match, webhookControlPlaneBoundary)
			*violationsList = append(*violationsList, violation)
		}
	}
	return nil
}

// TestWebhookSanctionedPackagesActuallyExist proves
// webhookSanctionedPackageDirs is not quietly stale -- every directory it
// names must exist as a real package directory, so a rename or removal
// that forgets to update this list fails loudly here instead of the
// allowlist silently protecting nothing (mirrors
// awsisolation_test.go's TestSanctionedPackagesActuallyExist).
func TestWebhookSanctionedPackagesActuallyExist(t *testing.T) {
	repoRoot := repoRoot(t)
	for dir := range webhookSanctionedPackageDirs {
		full := filepath.Join(repoRoot, filepath.FromSlash(dir))
		info, err := os.Stat(full)
		if err != nil {
			t.Errorf("sanctioned webhook package dir %q: %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("sanctioned webhook package dir %q is not a directory", dir)
		}
	}
}

// sortedWebhookSanctionedDirs returns webhookSanctionedPackageDirs's keys,
// sorted, for a deterministic log/failure message.
func sortedWebhookSanctionedDirs() []string {
	dirs := make([]string, 0, len(webhookSanctionedPackageDirs))
	for dir := range webhookSanctionedPackageDirs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}
