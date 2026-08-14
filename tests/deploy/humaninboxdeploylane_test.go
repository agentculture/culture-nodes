// Package deploytest (see compose_test.go's doc comment). This file is
// task t34's, revised by task t10: static checks over deploy/prod/deploy.sh's
// human-inbox lane -- the block that installs the human-inbox bridge and
// merge tracker units. The tracker is unconditional once the shared bridge
// secret file exists: GITHUB_TOKEN only selects authenticated versus
// anonymous polling.
//
// t34 shipped this lane as THOR ONLY. That was a declaration, and
// company/human-ops was registered somewhere else, so the engine parked human
// tasks on the bridge at the registered endpoint while thor's tracker watched
// thor's empty state directory (issue #72). The lane now DERIVES its host
// from the registration; humaninboxplacement_test.go exercises the library
// that does it, and the tests here pin the lane's use of it.
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

// humanInboxLaneBody returns just deploy_human_inbox's body, bounded by the
// column-0 closing brace that ends a shell function. Every test below scans
// the lane rather than the whole script, so a match from the codex or notify
// lane can never stand in for one here.
func humanInboxLaneBody(t *testing.T, script string) string {
	t.Helper()
	start := strings.Index(script, "deploy_human_inbox() {")
	if start == -1 {
		t.Fatal("no deploy_human_inbox() function definition found")
	}
	end := strings.Index(script[start:], "\n}\n")
	if end == -1 {
		t.Fatal("could not bound the deploy_human_inbox() body")
	}
	return script[start : start+end]
}

// executableLines drops comment and blank lines. A lane comment that recounts
// an incident must be free to name the host it happened on without reading as
// the bug still being present -- the same distinction
// TestDeployLaneInstallsTheAdapterAsAUvTool draws for the agent checkout.
func executableLines(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// TestHumanInboxDeployLaneExists is the smoke assertion the rest of this
// file leans on.
func TestHumanInboxDeployLaneExists(t *testing.T) {
	script := deployScriptText(t)
	if !strings.Contains(script, "deploy_human_inbox") {
		t.Fatal("deploy/prod/deploy.sh has no deploy_human_inbox lane")
	}
}

// TestHumanInboxDeployLaneDerivesItsHostFromTheRegistration is issue #72's
// pin. The bridge and tracker belong on the host serving company/human-ops,
// and there is exactly one artifact that says which host that is: the actor's
// registered endpoint_ref. A second hardcoded host name is a second config
// value that has to agree with the first, and the whole defect was those two
// agreeing only by luck.
func TestHumanInboxDeployLaneDerivesItsHostFromTheRegistration(t *testing.T) {
	script := deployScriptText(t)
	body := humanInboxLaneBody(t, script)

	if !strings.Contains(script, ". \"$SCRIPT_DIR/actor-placement.sh\"") &&
		!strings.Contains(script, "actor-placement.sh") {
		t.Fatal("deploy.sh does not source deploy/prod/actor-placement.sh; the lane has no shared way to resolve where an actor is served")
	}
	if !strings.Contains(body, "actor_registration") {
		t.Error("deploy_human_inbox never calls actor_registration — it cannot know which host serves the actor")
	}
	if !strings.Contains(body, "endpoint_address") {
		t.Error("deploy_human_inbox never derives a host from the registered endpoint_ref")
	}
	if !strings.Contains(body, "endpoint_port") {
		t.Error("deploy_human_inbox never derives the bridge port from the registered endpoint_ref; a hardcoded port is the same defect one field over")
	}

	for i, line := range executableLines(body) {
		for _, host := range []string{"thor", "orin"} {
			if strings.Contains(line, host) {
				t.Errorf("executable line %d of deploy_human_inbox names %q: %s\n\tthe lane's host comes from the registration, never from a name written here", i+1, host, strings.TrimSpace(line))
			}
		}
	}
	if strings.Contains(body, "case \"$host\" in") && strings.Contains(body, "thor*") {
		t.Error("deploy_human_inbox still gates on a thor*) host match")
	}
}

// TestHumanInboxDeployLaneAssertsColocationBeforeInstalling is acceptance
// criterion 2. Deriving the host is not enough on its own: the refusal has to
// run before anything is installed, so a lane that drifts back to a declared
// host fails the deploy instead of shipping the split.
func TestHumanInboxDeployLaneAssertsColocationBeforeInstalling(t *testing.T) {
	body := humanInboxLaneBody(t, deployScriptText(t))

	assertIdx := strings.Index(body, "assert_human_inbox_colocated ")
	if assertIdx == -1 {
		t.Fatal("deploy_human_inbox never calls assert_human_inbox_colocated; nothing fails the deploy when the pair would be split")
	}
	installIdx := strings.Index(body, "~/.config/systemd/user/human-inbox-bridge.service")
	if installIdx == -1 {
		t.Fatal("deploy_human_inbox does not install human-inbox-bridge.service")
	}
	if assertIdx > installIdx {
		t.Error("assert_human_inbox_colocated runs AFTER the bridge unit is installed; the assertion has to refuse before anything lands on the host")
	}
}

// TestHumanInboxBridgeAndTrackerInstallOnOneHost is acceptance criterion 1
// read literally: both units are installed through the same host variable, so
// there is no arrangement of this script in which they land apart.
func TestHumanInboxBridgeAndTrackerInstallOnOneHost(t *testing.T) {
	body := humanInboxLaneBody(t, deployScriptText(t))

	hostVar := regexp.MustCompile(`actor_host_exec\s+"(\$[A-Za-z_][A-Za-z0-9_]*)"`)
	seen := map[string][]string{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, "~/.config/systemd/user/human-inbox-") {
			continue
		}
		m := hostVar.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("a unit-install line does not run through actor_host_exec \"$host\": %s", strings.TrimSpace(line))
			continue
		}
		for _, unit := range []string{"human-inbox-bridge.service", "human-inbox-tracker.service"} {
			if strings.Contains(line, unit) {
				seen[unit] = append(seen[unit], m[1])
			}
		}
	}
	for _, unit := range []string{"human-inbox-bridge.service", "human-inbox-tracker.service"} {
		if len(seen[unit]) == 0 {
			t.Fatalf("no install line found for %s", unit)
		}
	}
	bridgeHost, trackerHost := seen["human-inbox-bridge.service"][0], seen["human-inbox-tracker.service"][0]
	if bridgeHost != trackerHost {
		t.Errorf("the bridge installs on %s and the tracker on %s — the tracker reads the bridge's state directory off the local filesystem, so two host variables is a split by construction", bridgeHost, trackerHost)
	}
}

// TestHumanInboxLaneArmsTheTrackerIdentityCheck pins the other half of issue
// #72's invariant. Task t8 made the tracker refuse to start when its bridge
// does not serve the actor it watches -- but that check is DISABLED unless
// the tracker knows a control plane to resolve the actor against, and it
// resolves its configured actor id as an actor_KEY. The bridge's copy of that
// same variable has to be the actors(id) ROW ID instead, because the bridge
// stamps it as origin.actor_id on a ledger record whose foreign key points
// there. One variable name, two required values, two separate env files.
func TestHumanInboxLaneArmsTheTrackerIdentityCheck(t *testing.T) {
	body := humanInboxLaneBody(t, deployScriptText(t))

	trackerEnvIdx := strings.Index(body, "> ~/.culture-nodes/human-inbox-tracker.env")
	if trackerEnvIdx == -1 {
		t.Fatal("deploy_human_inbox writes no ~/.culture-nodes/human-inbox-tracker.env")
	}
	if !strings.Contains(body, "HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL=") {
		t.Error("the tracker env carries no HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL — t8's startup identity check is left disabled, and a split deployment would run silently again")
	}
	if !strings.Contains(body, "HUMAN_INBOX_BRIDGE_ACTOR_ID=") {
		t.Error("no HUMAN_INBOX_BRIDGE_ACTOR_ID is written at all")
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

	body := humanInboxLaneBody(t, script)
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

	body := humanInboxLaneBody(t, script)
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
// tokenVarPattern/tokenishName check, scoped to the lines in the human-inbox
// lane that hand a command to another host, so a token-bearing variable is
// never interpolated into that command's argv.
//
// Both dispatch forms count: the lane reaches its host through
// actor_host_exec (which runs the command locally when the actor's address
// belongs to this machine, and over ssh otherwise), and the argv discipline
// is identical either way.
func TestHumanInboxDeployLaneNeverInterpolatesTokenIntoSSHArgv(t *testing.T) {
	script := deployScriptText(t)
	body := humanInboxLaneBody(t, script)

	checked := 0
	for i, line := range strings.Split(body, "\n") {
		idx := strings.Index(line, "ssh ")
		if idx == -1 {
			idx = strings.Index(line, "actor_host_exec ")
		}
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

// TestBridgeLanesResolveActorRowIdsNotKeys pins a bug that shipped twice.
//
// A bridge stamps `origin.actor_id` on the ledger claim it emits, and
// ledger_records.origin_actor_id is a FOREIGN KEY into actors(id). Give a
// bridge the human-readable actor_key instead of the row id and the actor
// does its real work, answers correctly, and every terminal commit then
// rolls back on a foreign-key violation — a symptom that points nowhere near
// identity.
//
// The codex lane hit this live and fixed it inline. The human-inbox lane then
// shipped `company/human-ops` and the notify lane shipped
// `company/notify-discord`, both keys, both broken the same way, because the
// fix was inlined rather than shared. The notify one was caught by an actual
// live run:
//
//	violates foreign key constraint "ledger_records_origin_actor_id_fkey"
//
// Each lane still resolves a row id rather than writing a key: the notify
// lane through resolve_actor_row_id, the human-inbox lane through the same
// actor_registration read that tells it which host to deploy to (task t10 --
// one registry read, so the id and the endpoint it pairs with can never come
// from different revisions).
func TestBridgeLanesResolveActorRowIdsNotKeys(t *testing.T) {
	script := deployScriptText(t)

	if !strings.Contains(script, "resolve_actor_row_id() {") {
		t.Fatal("deploy.sh defines no resolve_actor_row_id helper; each bridge lane resolving its own row id inline is exactly how this bug shipped twice")
	}

	for _, lane := range []struct{ envVar, actorKey, resolver string }{
		{"HUMAN_INBOX_BRIDGE_ACTOR_ID", "company/human-ops", "actor_registration"},
		{"NOTIFY_BRIDGE_ACTOR_ID", "company/notify-discord", `resolve_actor_row_id "company/notify-discord"`},
	} {
		// The assignment that writes the env file must not hardcode the key.
		bad := lane.envVar + "=" + lane.actorKey
		if strings.Contains(script, bad) {
			t.Errorf("deploy.sh writes %q — that is an actor_key, but ledger_records.origin_actor_id references actors(id); resolve the row id instead", bad)
		}
		if !strings.Contains(script, lane.resolver) {
			t.Errorf("no %s call for %q — the lane cannot know its registered row id", lane.resolver, lane.actorKey)
		}
	}
}
