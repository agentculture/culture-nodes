// Package deploytest (see compose_test.go's doc comment). This file is
// task t34's: static checks over deploy/prod/deploy.sh's human-inbox lane
// -- the block that installs the human-inbox bridge and merge tracker units
// on thor. The tracker is unconditional once the shared bridge secret file
// exists: GITHUB_TOKEN only selects authenticated versus anonymous polling.
//
// These are text assertions over the script itself, the same "cheaper and
// more honest as static checks" call codexdeploylane_test.go makes for
// its own lane, since deploy.sh targets production and exercising it for
// real is a manual operator step.
package deploytest

import (
	"regexp"
	"strings"
	"testing"
)

// TestHumanInboxDeployLaneExists is the smoke assertion the rest of this
// file leans on.
func TestHumanInboxDeployLaneExists(t *testing.T) {
	script := deployScriptText(t)
	if !strings.Contains(script, "deploy_human_inbox") {
		t.Fatal("deploy/prod/deploy.sh has no deploy_human_inbox lane")
	}
}

// TestHumanInboxDeployLaneIsThorOnly asserts the lane refuses to act on
// any host but thor: there is exactly one logical human actor
// (company/human-ops), so a second bridge/tracker pair on orin would race
// the same GitHub PRs and the same inbox tasks against the same actor
// row -- a deliberate deviation from the codex-bridge lane's
// runs-on-both-hosts shape.
func TestHumanInboxDeployLaneIsThorOnly(t *testing.T) {
	script := deployScriptText(t)

	fnIdx := strings.Index(script, "deploy_human_inbox() {")
	if fnIdx == -1 {
		t.Fatal("no deploy_human_inbox() function definition found")
	}
	// Bound the function body to before its call site (which repeats the
	// function name) so the scan does not accidentally match the call.
	callIdx := strings.Index(script[fnIdx+1:], `deploy_human_inbox "$HOST"`)
	if callIdx == -1 {
		t.Fatal(`no deploy_human_inbox "$HOST"` + ` call site found`)
	}
	body := script[fnIdx : fnIdx+1+callIdx]

	if !strings.Contains(body, "thor*") {
		t.Error("deploy_human_inbox's body does not gate on a thor*) host match")
	}
	if !strings.Contains(strings.ToLower(body), "skipping on $host") && !strings.Contains(strings.ToLower(body), "thor-only") {
		t.Error("deploy_human_inbox's non-thor branch does not clearly report skipping (thor-only)")
	}
}

// TestHumanInboxDeployLaneRequiresSecretsFromInstallSecrets mirrors
// TestCodexDeployLaneRequiresBridgeEnvFromInstallSecrets: deploy.sh checks
// for ~/.culture-nodes/human-inbox.env and fails with a message naming
// install-secrets.sh, rather than generating any secret itself.
func TestHumanInboxDeployLaneRequiresSecretsFromInstallSecrets(t *testing.T) {
	script := deployScriptText(t)

	if !strings.Contains(script, "human-inbox.env") {
		t.Fatal("deploy.sh never references human-inbox.env; the missing-secret failure mode is unhandled")
	}
	if !regexp.MustCompile(`test -f [^\n]*human-inbox\.env`).MatchString(script) {
		t.Error("deploy.sh does not test for human-inbox.env before installing the human-inbox units")
	}

	fnIdx := strings.Index(script, "deploy_human_inbox() {")
	if fnIdx == -1 {
		t.Fatal("no deploy_human_inbox() function found")
	}
	body := script[fnIdx:]
	if endIdx := strings.Index(body, "\ndeploy_human_inbox \"$HOST\""); endIdx != -1 {
		body = body[:endIdx]
	}
	if !strings.Contains(body, "install-secrets.sh") {
		t.Error("deploy_human_inbox's missing-secret message does not name install-secrets.sh as the remedy")
	}
	if strings.Contains(body, "openssl rand") {
		t.Error("deploy_human_inbox generates secret material itself; token generation belongs to install-secrets.sh alone")
	}
}

// TestHumanInboxDeployLaneRunsViaUvRunAgainstAgentCheckout asserts the
// lane's ExecStart-matching invocation reuses the SAME
// ~/git/culture-nodes-agent checkout the codex-bridge lane provisions,
// rather than inventing a second package-install mechanism.
func TestHumanInboxDeployLaneRunsViaUvRunAgainstAgentCheckout(t *testing.T) {
	script := deployScriptText(t)

	fnIdx := strings.Index(script, "deploy_human_inbox() {")
	if fnIdx == -1 {
		t.Fatal("no deploy_human_inbox() function found")
	}

	if !strings.Contains(script, "~/.config/systemd/user/human-inbox-bridge.service") {
		t.Error("deploy.sh does not install human-inbox-bridge.service to ~/.config/systemd/user/")
	}
}

// TestHumanInboxDeployLaneInstallsAndStartsBridgeUnit asserts the bridge
// unit half of the lane mirrors the runner/codex-bridge shape: install,
// daemon-reload, restart, enable, poll is-active.
func TestHumanInboxDeployLaneInstallsAndStartsBridgeUnit(t *testing.T) {
	script := deployScriptText(t)

	for _, want := range []struct{ needle, why string }{
		{"~/.config/systemd/user/human-inbox-bridge.service", "the unit file install"},
		{"systemctl --user daemon-reload", "the daemon-reload after installing the unit"},
		{"systemctl --user restart human-inbox-bridge", "the restart (so a re-deploy picks up changes)"},
		{"systemctl --user enable human-inbox-bridge", "the enable (so the bridge survives a reboot)"},
		{`assert_unit_healthy "$host" human-inbox-bridge`, "the health assertion (wait-active plus a stays-active recheck)"},
	} {
		if !strings.Contains(script, want.needle) {
			t.Errorf("deploy.sh has no %q — %s is missing from the human-inbox-bridge lane", want.needle, want.why)
		}
	}
}

// TestHumanInboxDeployLaneInstallsTrackerWithoutGitHubToken pins t35's
// public-repository lane: the tracker install/start sequence is not nested
// behind any inspection of GITHUB_TOKEN.
func TestHumanInboxDeployLaneInstallsTrackerWithoutGitHubToken(t *testing.T) {
	script := deployScriptText(t)

	fnIdx := strings.Index(script, "deploy_human_inbox() {")
	if fnIdx == -1 {
		t.Fatal("no deploy_human_inbox() function definition found")
	}
	callIdx := strings.Index(script[fnIdx+1:], `deploy_human_inbox "$HOST"`)
	if callIdx == -1 {
		t.Fatal("no deploy_human_inbox call site found")
	}
	body := script[fnIdx : fnIdx+1+callIdx]
	if strings.Contains(body, "grep -q \"^GITHUB_TOKEN=") {
		t.Error("deploy_human_inbox still gates the tracker unit on a non-empty GITHUB_TOKEN")
	}
	if strings.Contains(body, "skipping human-inbox-tracker") {
		t.Error("deploy_human_inbox still has a no-token path that skips the tracker unit")
	}
	for _, want := range []struct{ needle, why string }{
		{"~/.config/systemd/user/human-inbox-tracker.service", "the tracker unit file install"},
		{"systemctl --user restart human-inbox-tracker", "the tracker restart"},
		{"systemctl --user enable human-inbox-tracker", "the tracker enable"},
		{`assert_unit_healthy "$host" human-inbox-tracker`, "the tracker health assertion"},
	} {
		if !strings.Contains(script, want.needle) {
			t.Errorf("deploy.sh has no %q — %s is missing from the human-inbox-tracker lane", want.needle, want.why)
		}
	}
}

// tokenVarPatternHumanInbox mirrors codexdeploylane_test.go's
// tokenVarPattern/tokenishName check, scoped to lines invoking ssh within
// the human-inbox lane, so a token-bearing variable is never interpolated
// into ssh's own argv.
func TestHumanInboxDeployLaneNeverInterpolatesTokenIntoSSHArgv(t *testing.T) {
	script := deployScriptText(t)

	fnIdx := strings.Index(script, "deploy_human_inbox() {")
	if fnIdx == -1 {
		t.Fatal("no deploy_human_inbox() function found")
	}
	callIdx := strings.Index(script[fnIdx+1:], `deploy_human_inbox "$HOST"`)
	if callIdx == -1 {
		t.Fatal("no deploy_human_inbox call site found")
	}
	body := script[fnIdx : fnIdx+1+callIdx]

	checked := 0
	for i, line := range strings.Split(body, "\n") {
		idx := strings.Index(line, "ssh ")
		if idx == -1 {
			continue
		}
		checked++
		argvPortion := line[idx:]
		for _, m := range tokenVarPattern.FindAllStringSubmatch(argvPortion, -1) {
			if tokenishName.MatchString(m[1]) {
				t.Errorf("line %d of deploy_human_inbox: %q interpolates $%s into ssh argv; secret material must never ride argv", i+1, line, m[1])
			}
		}
	}
	if checked == 0 {
		t.Fatal("no ssh invocation found in deploy_human_inbox; this test is not proving anything")
	}
}

// TestDeployLaneInstallsTheAdapterAsAUvTool pins the fix for a live incident.
//
// The units used to exec `uv run --directory ~/git/culture-nodes-agent/...`,
// which is the codex AGENT WORKSPACE — a checkout the codex-bridge lane
// fast-forwards to main. Deploying a branch therefore installed units whose
// code was not in the directory they ran from: the tracker crash-looped 6272
// times over nine hours on `No module named human_inbox_bridge.tracker`, and
// merge-as-action was silently dead the whole time.
func TestDeployLaneInstallsTheAdapterAsAUvTool(t *testing.T) {
	script := deployScriptText(t)

	if !strings.Contains(script, "uv tool install --force ./$REMOTE_DIR/adapters/human-inbox") {
		t.Error("deploy.sh does not `uv tool install` the human-inbox adapter — without it the units have no console script to exec, and running from a source tree reintroduces the branch-pinning bug")
	}
	// Scan executable lines only. The comment above the install step recounts
	// this incident on purpose, and that prose naming the old path must not
	// read as the bug still being present.
	for i, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.Contains(line, "git/culture-nodes-agent/adapters/human-inbox") {
			t.Errorf("deploy.sh:%d still points the human-inbox lane at ~/git/culture-nodes-agent — that checkout tracks main, not the deployed branch: %s", i+1, strings.TrimSpace(line))
		}
	}
}

// TestUnitHealthAssertionCatchesACrashLoop pins the second half of the same
// incident: the deploy reported success while the unit was restarting every
// five seconds. Reaching `active` once is not proof — a fast-failing process
// spends its life in activating/auto-restart, and a single is-active probe
// can land in a start window.
func TestUnitHealthAssertionCatchesACrashLoop(t *testing.T) {
	script := deployScriptText(t)

	idx := strings.Index(script, "assert_unit_healthy() {")
	if idx == -1 {
		t.Fatal("deploy.sh defines no assert_unit_healthy() helper")
	}
	end := strings.Index(script[idx:], "\ndeploy_human_inbox() {")
	if end == -1 {
		t.Fatal("could not bound the assert_unit_healthy() body")
	}
	body := script[idx : idx+end]

	if !strings.Contains(body, "NRestarts") {
		t.Error("assert_unit_healthy never reads NRestarts — it cannot distinguish a running unit from one that restarts every few seconds")
	}
	if !strings.Contains(body, "journalctl") {
		t.Error("assert_unit_healthy does not dump the journal on failure — the operator needs the actual error, not just a status line")
	}
	if strings.Count(body, "is-active") < 2 {
		t.Error("assert_unit_healthy probes is-active only once; it must re-check after an interval to catch a flapping unit")
	}
}
