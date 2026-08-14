// Package deploytest (see compose_test.go's doc comment for the package's
// purpose). This file is task t34's: definition tests for
// deploy/prod/human-inbox-bridge.service and
// deploy/prod/human-inbox-tracker.service, the two systemd user units
// that run adapters/human-inbox's bridge server and its GitHub merge
// tracker as host-resident processes beside codex-bridge.service. Modeled
// on codexbridgeunit_test.go's own style: parse the real files, assert
// the properties the task's acceptance criteria (and this file's
// implementing comments) promise, fail loudly if either drifts.
package deploytest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// humanInboxUnitFile is a minimal Key=Value scanner over a systemd unit
// file, mirroring codexbridgeunit_test.go's unitFile/loadUnitFile but
// parameterized on filename so it can load either of this task's two
// units without colliding with that file's codex-bridge.service-specific
// loadUnitFile.
type humanInboxUnitFile struct {
	raw    string
	values map[string][]string
}

func loadHumanInboxUnitFile(t *testing.T, filename string) humanInboxUnitFile {
	t.Helper()
	path := filepath.Join(codexBridgeDir(t), filename) // codexBridgeDir == deploy/prod (shared helper)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	uf := humanInboxUnitFile{raw: string(raw), values: map[string][]string{}}
	for _, line := range strings.Split(uf.raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") || strings.HasPrefix(trimmed, "[") {
			continue
		}
		key, value, found := strings.Cut(trimmed, "=")
		if !found {
			continue
		}
		uf.values[strings.TrimSpace(key)] = append(uf.values[strings.TrimSpace(key)], strings.TrimSpace(value))
	}
	if len(uf.values) == 0 {
		t.Fatalf("%s: no Key=Value directives parsed; this test is not proving anything", path)
	}
	return uf
}

func (uf humanInboxUnitFile) first(key string) (string, bool) {
	vals, ok := uf.values[key]
	if !ok || len(vals) == 0 {
		return "", false
	}
	return vals[0], true
}

// --- human-inbox-bridge.service ---------------------------------------

// assertNotRunFromAgentCheckout is the regression guard for a live incident.
//
// These units used to exec `uv run --directory ~/git/culture-nodes-agent/...`.
// That directory is the CODEX AGENT WORKSPACE, and the codex-bridge lane
// fast-forwards it to its upstream tracking branch — main. So deploying any
// branch installed units that exec code living only on that branch, out of a
// checkout pinned to a different one. The tracker died on
// `No module named human_inbox_bridge.tracker` and systemd restarted it 6272
// times over nine hours while merge-as-action silently did nothing.
//
// An agent workspace and a deployment artifact source are different things.
// The units must exec a console script installed by `uv tool install`, which
// copies the package into its own venv and is therefore independent of both
// the archive and anybody's checked-out branch.
func assertNotRunFromAgentCheckout(t *testing.T, unit, execStart string) {
	t.Helper()
	if strings.Contains(execStart, "culture-nodes-agent") {
		t.Errorf("%s ExecStart=%q execs out of the codex agent checkout — that directory tracks main, so a branch deploy installs a unit whose code is not there", unit, execStart)
	}
	if strings.Contains(execStart, "uv run") {
		t.Errorf("%s ExecStart=%q runs `uv run` against a source tree; it must exec a uv-tool-installed console script so the unit survives the next deploy's rm -rf and does not depend on a checkout's branch", unit, execStart)
	}
	if !strings.HasPrefix(execStart, "/") && !strings.HasPrefix(execStart, "%h/") {
		t.Errorf("%s ExecStart=%q is not an absolute (or %%h-rooted) path; a systemd ExecStart takes no PATH lookup", unit, execStart)
	}
}

func TestHumanInboxBridgeUnitRunsAnInstalledConsoleScript(t *testing.T) {
	uf := loadHumanInboxUnitFile(t, "human-inbox-bridge.service")

	execStart, ok := uf.first("ExecStart")
	if !ok {
		t.Fatal("human-inbox-bridge.service declares no ExecStart=")
	}
	assertNotRunFromAgentCheckout(t, "human-inbox-bridge.service", execStart)
	if !strings.Contains(execStart, "human-inbox-bridge serve") {
		t.Errorf("ExecStart=%q does not invoke the `human-inbox-bridge serve` console script", execStart)
	}
}

func TestHumanInboxBridgeUnitCarriesTokenOnlyViaEnvironmentFile(t *testing.T) {
	uf := loadHumanInboxUnitFile(t, "human-inbox-bridge.service")

	envFiles := uf.values["EnvironmentFile"]
	if len(envFiles) == 0 {
		t.Fatal("human-inbox-bridge.service declares no EnvironmentFile=")
	}
	joined := strings.Join(envFiles, " ")
	if !strings.Contains(joined, "human-inbox.env") {
		t.Errorf("EnvironmentFile directives %v do not reference human-inbox.env (the secret file install-secrets.sh writes)", envFiles)
	}
	if !strings.Contains(joined, "human-inbox-bridge.env") {
		t.Errorf("EnvironmentFile directives %v do not reference human-inbox-bridge.env (the non-secret config file deploy.sh writes)", envFiles)
	}
	if _, ok := uf.values["Environment"]; ok {
		t.Error("human-inbox-bridge.service declares a literal Environment= line; secrets must ride EnvironmentFile only")
	}
	assertNoTokenLiteral(t, "human-inbox-bridge.service", uf.raw)
}

func TestHumanInboxBridgeUnitAlwaysRestarts(t *testing.T) {
	uf := loadHumanInboxUnitFile(t, "human-inbox-bridge.service")

	restart, ok := uf.first("Restart")
	if !ok || restart != "always" {
		t.Errorf("Restart=%q, want %q (a long-running server the operator wants back regardless of exit reason)", restart, "always")
	}
	if _, ok := uf.first("RestartSec"); !ok {
		t.Error("human-inbox-bridge.service declares Restart= but no RestartSec=; a bare Restart=always with no backoff can hot-loop")
	}
	wantedBy, ok := uf.first("WantedBy")
	if !ok || !strings.Contains(wantedBy, "default.target") {
		t.Errorf("WantedBy=%q does not include default.target", wantedBy)
	}
}

// --- human-inbox-tracker.service ---------------------------------------

func TestHumanInboxTrackerUnitRunsTrackerModuleContinuously(t *testing.T) {
	uf := loadHumanInboxUnitFile(t, "human-inbox-tracker.service")

	execStart, ok := uf.first("ExecStart")
	if !ok {
		t.Fatal("human-inbox-tracker.service declares no ExecStart=")
	}
	if !strings.Contains(execStart, "human-inbox-tracker") {
		t.Errorf("ExecStart=%q does not invoke the `human-inbox-tracker` console script", execStart)
	}
	// The unit runs the tracker in its own continuous poll-loop mode, not
	// the one-shot --once probe: a systemd Restart=always PERSISTENT unit
	// is the chosen shape (task t34's decision), not a --once timer, so
	// the invocation must not pass --once.
	if strings.Contains(execStart, "--once") {
		t.Errorf("ExecStart=%q passes --once; task t34 chose a persistent unit (the tracker's own internal poll loop), not a --once timer", execStart)
	}
	assertNotRunFromAgentCheckout(t, "human-inbox-tracker.service", execStart)
}

// TestTrackerConsoleScriptIsDeclared closes the loop the unit alone cannot:
// the unit may name a console script that the package never publishes, which
// is a 203/EXEC at runtime and green in every file-parsing test.
func TestTrackerConsoleScriptIsDeclared(t *testing.T) {
	repoRoot := filepath.Dir(filepath.Dir(codexBridgeDir(t)))
	body, err := os.ReadFile(filepath.Join(repoRoot, "adapters", "human-inbox", "pyproject.toml"))
	if err != nil {
		t.Fatalf("reading the human-inbox pyproject: %v", err)
	}
	for _, script := range []string{"human-inbox-bridge =", "human-inbox-tracker ="} {
		if !strings.Contains(string(body), script) {
			t.Errorf("adapters/human-inbox/pyproject.toml declares no [project.scripts] entry %q — the systemd unit execs a binary uv tool install would never create", script)
		}
	}
}

func TestHumanInboxTrackerUnitIsPersistentNotATimer(t *testing.T) {
	path := filepath.Join(codexBridgeDir(t), "human-inbox-tracker.service")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist as a persistent systemd unit: %v", path, err)
	}
	// A systemd .timer unit is a companion unit type entirely -- assert
	// this task did not instead ship a .timer file wrapping `--once`.
	timerPath := filepath.Join(codexBridgeDir(t), "human-inbox-tracker.timer")
	if _, err := os.Stat(timerPath); err == nil {
		t.Errorf("found %s: task t34 chose a persistent Restart=always unit, not a systemd timer wrapping --once", timerPath)
	}

	uf := loadHumanInboxUnitFile(t, "human-inbox-tracker.service")
	restart, ok := uf.first("Restart")
	if !ok || restart != "always" {
		t.Errorf("Restart=%q, want %q for the persistent-unit shape", restart, "always")
	}
}

func TestHumanInboxTrackerUnitCarriesSecretsOnlyViaEnvironmentFile(t *testing.T) {
	uf := loadHumanInboxUnitFile(t, "human-inbox-tracker.service")

	envFiles := uf.values["EnvironmentFile"]
	if len(envFiles) == 0 {
		t.Fatal("human-inbox-tracker.service declares no EnvironmentFile=")
	}
	joined := strings.Join(envFiles, " ")
	if !strings.Contains(joined, "human-inbox.env") {
		t.Errorf("EnvironmentFile directives %v do not reference human-inbox.env (carries HUMAN_INBOX_BRIDGE_AUTH_TOKEN and optional GITHUB_TOKEN)", envFiles)
	}
	if _, ok := uf.values["Environment"]; ok {
		t.Error("human-inbox-tracker.service declares a literal Environment= line; secrets must ride EnvironmentFile only")
	}
	assertNoTokenLiteral(t, "human-inbox-tracker.service", uf.raw)
}

func TestHumanInboxTrackerUnitWantsTheBridgeUnit(t *testing.T) {
	uf := loadHumanInboxUnitFile(t, "human-inbox-tracker.service")

	after, ok := uf.first("After")
	if !ok || !strings.Contains(after, "human-inbox-bridge.service") {
		t.Errorf("After=%q does not include human-inbox-bridge.service; the tracker submits to the sibling bridge and should order after it", after)
	}
}
