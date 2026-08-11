// Package deploytest (this file) statically checks deploy/prod/deploy.sh's
// codex-bridge lane (plan t3) — the block that installs the bridge, the
// host query CLI, the agent checkout, the preflight, the rendered config,
// and the systemd user unit on each production host.
//
// These are text assertions over the script itself rather than a live ssh
// run: deploy.sh targets production, so exercising it for real is a manual
// operator step (plan t10's live acceptance), and the same "cheaper and
// more honest as static checks" call codexsecrets_test.go, compose_test.go
// and helm_test.go all make for their own artifacts applies here.
//
// The load-bearing assertions, and why each exists:
//   - the lane runs for BOTH thor and orin (both hosts run a managed codex
//     actor), which a host-agnostic call outside the case satisfies;
//   - the bridge is installed with `uv tool install` (copies the package
//     into its own tool venv) and never as an editable/in-place install
//     from the shipped archive, because the next deploy deletes that
//     archive with rm -rf and the bridge must keep serving (c21/h19);
//   - the checkout lane fast-forwards a clean checkout and refuses a dirty
//     or diverged one, without failing the deploy;
//   - the preflight is installed at exactly the path codex-bridge.service's
//     ExecStartPre names — cross-checked against the unit file, not
//     hard-coded twice;
//   - no token-bearing variable is ever interpolated into an ssh argv, the
//     same discipline codexsecrets_test.go enforces on install-secrets.sh.
package deploytest

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// deployScriptText reads deploy/prod/deploy.sh, located via the shared
// codexBridgeDir helper (codexbridgeunit_test.go) so this file stays
// independent of the working directory `go test` runs from.
func deployScriptText(t *testing.T) string {
	t.Helper()
	path := filepath.Join(codexBridgeDir(t), "deploy.sh")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(raw) == 0 {
		t.Fatalf("%s is empty; this test is not proving anything", path)
	}
	return string(raw)
}

// codexLaneMarkers are strings that only the codex-bridge lane introduces.
// Any one of them appearing in a region of the script means the lane is
// present there.
var codexLaneMarkers = []string{"codex-bridge", "deploy_codex_bridge", "codex-preflight"}

func containsAnyMarker(region string) bool {
	for _, m := range codexLaneMarkers {
		if strings.Contains(region, m) {
			return true
		}
	}
	return false
}

// TestCodexDeployLaneExists is the smoke assertion the rest of this file
// leans on: deploy.sh has a codex-bridge lane at all.
func TestCodexDeployLaneExists(t *testing.T) {
	script := deployScriptText(t)
	if !containsAnyMarker(script) {
		t.Fatal("deploy/prod/deploy.sh has no codex-bridge lane (none of the lane markers appear)")
	}
	if !strings.Contains(script, "deploy_codex_bridge") {
		t.Error("no deploy_codex_bridge function; the task asks for the lane to be factored into one function shared by both hosts")
	}
}

// TestCodexDeployLaneRunsForBothHosts asserts the lane reaches thor AND
// orin. Two shapes satisfy this: a host-agnostic invocation outside the
// `case "$HOST"` block (what the script does today — one call with the
// named host), or the lane appearing inside both the thor*) and orin*)
// branches. Anything else would deploy a bridge to only one machine.
func TestCodexDeployLaneRunsForBothHosts(t *testing.T) {
	script := deployScriptText(t)

	caseIdx := strings.Index(script, `case "$HOST" in`)
	if caseIdx == -1 {
		t.Fatal(`no 'case "$HOST" in' block found in deploy.sh; the per-host structure this test reasons about is gone`)
	}

	beforeCase := script[:caseIdx]
	if strings.Contains(beforeCase, `deploy_codex_bridge "$HOST"`) {
		// Host-agnostic: one call, before the per-host case. Nothing may
		// gate it on a single host name.
		callLine := ""
		for _, line := range strings.Split(beforeCase, "\n") {
			if strings.Contains(line, `deploy_codex_bridge "$HOST"`) {
				callLine = line
			}
		}
		if strings.Contains(callLine, "thor") || strings.Contains(callLine, "orin") {
			t.Errorf("the host-agnostic bridge call is gated on a specific host: %q", callLine)
		}
		return
	}

	// Otherwise the lane must appear in both branches of the case.
	caseBody := script[caseIdx:]
	thorIdx := strings.Index(caseBody, "thor*)")
	orinIdx := strings.Index(caseBody, "orin*)")
	if thorIdx == -1 || orinIdx == -1 {
		t.Fatal("could not locate both the thor*) and orin*) branches of the case block")
	}
	thorBranch := caseBody[thorIdx:orinIdx]
	orinBranch := caseBody[orinIdx:]
	if !containsAnyMarker(thorBranch) {
		t.Error("the thor*) branch contains no codex-bridge lane, and there is no host-agnostic call before the case")
	}
	if !containsAnyMarker(orinBranch) {
		t.Error("the orin*) branch contains no codex-bridge lane, and there is no host-agnostic call before the case")
	}
}

// uvToolInstallBridge matches an `uv tool install` whose target is the
// shipped adapters/codex source tree.
var uvToolInstallBridge = regexp.MustCompile(`uv tool install[^\n'"]*adapters/codex`)

// TestCodexDeployLaneInstallsBridgeWithUvToolInstall is the c21/h19 check:
// the bridge must be installed with `uv tool install` (which builds the
// package and copies it into its own tool venv under ~/.local/share/uv, so
// ~/.local/bin/codex-bridge survives the next deploy's `rm -rf` of the
// shipped archive), never `uv run --project` (runs straight out of the
// archive) and never an editable install (a .pth pointing back into the
// archive).
func TestCodexDeployLaneInstallsBridgeWithUvToolInstall(t *testing.T) {
	script := deployScriptText(t)

	if !uvToolInstallBridge.MatchString(script) {
		t.Fatal("no `uv tool install ... adapters/codex` found; the bridge install is not archive-independent (c21)")
	}

	for _, line := range strings.Split(script, "\n") {
		if !strings.Contains(line, "adapters/codex") {
			continue
		}
		if strings.Contains(line, "uv run") {
			t.Errorf("bridge line uses `uv run` against the shipped archive, which the next deploy deletes (c21): %q", line)
		}
		if strings.Contains(line, "--editable") || regexp.MustCompile(`uv tool install\s+(-\w*e\b|\S*\s+-e\b)`).MatchString(line) {
			t.Errorf("bridge install is editable, so it would point back at the deleted archive (c21): %q", line)
		}
	}
}

// TestCodexDeployLaneInstallsPythonNodesCLI covers deviation d1: the host
// query CLI is the PYTHON `nodes` CLI installed from PyPI
// (`uv tool install culture-nodes`), not the Go cmd/nodes binary — cmd/nodes
// has no query verbs, so nothing Go-side may be built or scp'd for it.
func TestCodexDeployLaneInstallsPythonNodesCLI(t *testing.T) {
	script := deployScriptText(t)

	pypiInstall := regexp.MustCompile(`uv tool install[^\n'"]*\bculture-nodes\b`)
	if !pypiInstall.MatchString(script) {
		t.Error("no `uv tool install ... culture-nodes` found; the Python nodes CLI (deviation d1) is not installed on the host")
	}

	// The Go CLI must not be built or shipped. `./cmd/nodes-runner` (the
	// pre-existing runner lane) is explicitly NOT a match: the character
	// class excludes a following word character or hyphen.
	goCLIBuild := regexp.MustCompile(`go build[^\n]*\./cmd/nodes([^\w-]|$)`)
	if goCLIBuild.MatchString(script) {
		t.Error("deploy.sh builds ./cmd/nodes; deviation d1 says the host query CLI is the Python nodes CLI, and cmd/nodes has no query verbs")
	}
	scpGoCLI := regexp.MustCompile(`scp[^\n]*bin/nodes([^\w-]|$)`)
	if scpGoCLI.MatchString(script) {
		t.Error("deploy.sh scps a Go nodes binary to ~/.culture-nodes/bin/nodes; deviation d1 removed that step")
	}

	// h17: the success path must name the absolute path, since ~/.local/bin
	// is on PATH only in login shells on orin.
	if !strings.Contains(script, "/.local/bin/nodes") {
		t.Error("deploy.sh never mentions the absolute ~/.local/bin/nodes path; h17 asks the lane to surface it for non-login shells")
	}
}

// TestCodexDeployLaneProvisionsAgentCheckout asserts the ~/git/culture-nodes-agent
// provisioning contract: clone when absent, fast-forward ONLY when clean,
// and refuse (leaving the checkout untouched) when dirty or diverged.
func TestCodexDeployLaneProvisionsAgentCheckout(t *testing.T) {
	script := deployScriptText(t)

	for _, want := range []struct{ needle, why string }{
		{"git/culture-nodes-agent", "the agent checkout path the bridge config's repo_allowlist names"},
		{"git clone https://github.com/agentculture/culture-nodes", "the clone-when-absent branch"},
		{"status --porcelain", "the dirty-checkout detection"},
		{"merge --ff-only", "the fast-forward-only update (never a rebase or reset)"},
		{"fetch", "the fetch that precedes the fast-forward"},
	} {
		if !strings.Contains(script, want.needle) {
			t.Errorf("deploy.sh has no %q — %s is missing from the checkout lane", want.needle, want.why)
		}
	}

	if !strings.Contains(script, "refusing to touch it") {
		t.Error("the checkout lane has no explicit refusal message; a dirty/diverged checkout must be refused with a clear message")
	}
}

// TestCodexDeployLaneWarnsButDoesNotFailOnDirtyCheckout asserts the
// warn-don't-fail choice: a dirty checkout is an expected operator state
// (write sessions leave diffs to harvest, then reset), so a refusal must
// not abort a deploy whose real job is getting the bridge itself running.
// Mechanically: the ssh invocation carrying the checkout script is followed
// by a `||` fallback that warns, and is not followed by an exit.
func TestCodexDeployLaneWarnsButDoesNotFailOnDirtyCheckout(t *testing.T) {
	script := deployScriptText(t)

	idx := strings.Index(script, "CODEX_AGENT_CHECKOUT_REMOTE")
	if idx == -1 {
		t.Skip("checkout lane is not factored into a CODEX_AGENT_CHECKOUT_REMOTE variable; the shape assertion below does not apply")
	}

	// Find the ssh invocation that runs the checkout script and inspect the
	// logical command (the ssh line plus a possible continuation).
	var invocation string
	lines := strings.Split(script, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "ssh") || !strings.Contains(line, "CODEX_AGENT_CHECKOUT_REMOTE") {
			continue
		}
		invocation = line
		for strings.HasSuffix(strings.TrimSpace(invocation), `\`) && i+1 < len(lines) {
			i++
			invocation += "\n" + lines[i]
		}
		break
	}
	if invocation == "" {
		t.Fatal("no ssh invocation running the checkout script found")
	}
	if !strings.Contains(invocation, "||") {
		t.Errorf("the checkout ssh invocation has no `||` fallback, so a refusal aborts the whole deploy under set -e: %q", invocation)
	}
	if regexp.MustCompile(`\|\|[^|]*exit\b`).MatchString(invocation) {
		t.Errorf("the checkout ssh invocation exits on failure; a dirty checkout must warn and continue: %q", invocation)
	}
	if !strings.Contains(strings.ToUpper(invocation), "WARNING") {
		t.Errorf("the checkout fallback does not warn; the refusal must be visible to the operator: %q", invocation)
	}
}

// unitExecPaths pulls the preflight script path and the bridge config path
// out of codex-bridge.service's ExecStartPre, translating systemd's %h
// specifier to the shell's ~ so the strings can be compared with what
// deploy.sh writes. Cross-checking against the unit (rather than hard-coding
// the path in this test too) is the point: if t2 ever moves the preflight,
// this test fails instead of silently passing on a stale literal.
func unitExecPaths(t *testing.T) (preflightPath, configPath string) {
	t.Helper()
	uf := loadUnitFile(t)
	pre, ok := uf.first("ExecStartPre")
	if !ok {
		t.Fatal("codex-bridge.service has no ExecStartPre directive")
	}
	fields := strings.Fields(pre)
	if len(fields) < 2 {
		t.Fatalf("ExecStartPre does not carry both a script and a config argument: %q", pre)
	}
	toShell := func(s string) string { return strings.Replace(s, "%h/", "~/", 1) }
	return toShell(fields[0]), toShell(fields[1])
}

// TestCodexDeployLaneInstallsPreflightWhereUnitExpectsIt asserts deploy.sh
// installs codex-preflight.sh to exactly the path the unit's ExecStartPre
// names, and makes it executable. A mismatch here means every bridge start
// fails with a systemd 203/EXEC.
func TestCodexDeployLaneInstallsPreflightWhereUnitExpectsIt(t *testing.T) {
	script := deployScriptText(t)
	preflightPath, configPath := unitExecPaths(t)

	if !strings.Contains(script, preflightPath) {
		t.Errorf("deploy.sh never writes %s, the path codex-bridge.service's ExecStartPre invokes", preflightPath)
	}
	installsFromArchive := regexp.MustCompile(`cp [^\n]*codex-preflight\.sh ` + regexp.QuoteMeta(preflightPath))
	if !installsFromArchive.MatchString(script) {
		t.Errorf("deploy.sh does not copy deploy/prod/codex-preflight.sh to %s", preflightPath)
	}
	if !regexp.MustCompile(`chmod \+x [^\n]*codex-preflight\.sh`).MatchString(script) {
		t.Error("deploy.sh does not chmod +x the installed preflight; systemd would fail ExecStartPre with 203/EXEC")
	}

	// The rendered config must land at the path the unit passes to the
	// preflight (and, via ExecStart --config, to the bridge itself).
	if !strings.Contains(script, "> "+configPath) {
		t.Errorf("deploy.sh does not render the bridge config to %s (the path the unit names)", configPath)
	}
	if !strings.Contains(script, "__HOME__") {
		t.Error("deploy.sh never substitutes the template's __HOME__ placeholder")
	}
	if !regexp.MustCompile(`sed [^\n]*__HOME__[^\n]*\$HOME`).MatchString(script) {
		t.Error("the __HOME__ substitution does not resolve to the TARGET's $HOME; the config must be rendered with an already-expanded absolute home")
	}
}

// TestCodexDeployLaneRunsPreflightAtDeployTime asserts the preflight also
// runs once during the deploy (fast fail at deploy rather than only at unit
// start), with SKIP_CODEX_PREFLIGHT=1 downgrading it to a warning for
// bootstrap ordering.
func TestCodexDeployLaneRunsPreflightAtDeployTime(t *testing.T) {
	script := deployScriptText(t)
	preflightPath, configPath := unitExecPaths(t)

	runsIt := false
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, "ssh") && strings.Contains(line, preflightPath) && strings.Contains(line, configPath) {
			runsIt = true
		}
	}
	if !runsIt {
		t.Errorf("no ssh invocation runs %s %s at deploy time; the lane would only discover a broken host at unit start", preflightPath, configPath)
	}
	if !strings.Contains(script, "SKIP_CODEX_PREFLIGHT") {
		t.Error("no SKIP_CODEX_PREFLIGHT escape hatch; bootstrap ordering (codex not yet logged in) would have no way through")
	}
}

// TestCodexDeployLaneInstallsAndStartsUnit asserts the unit half of the
// lane mirrors the runner's shape: install the unit file, daemon-reload,
// restart, enable, then poll systemctl is-active until it comes up.
func TestCodexDeployLaneInstallsAndStartsUnit(t *testing.T) {
	script := deployScriptText(t)

	for _, want := range []struct{ needle, why string }{
		{"codex-bridge.service ~/.config/systemd/user/", "the unit file install"},
		{"systemctl --user daemon-reload", "the daemon-reload after installing the unit"},
		{"systemctl --user restart codex-bridge", "the restart (so a re-deploy picks up the new bridge)"},
		{"systemctl --user enable codex-bridge", "the enable (so the bridge survives a reboot)"},
		{"is-active codex-bridge", "the wait-active poll"},
		{"XDG_RUNTIME_DIR=/run/user/", "the XDG_RUNTIME_DIR export every systemctl --user call over ssh needs"},
	} {
		if !strings.Contains(script, want.needle) {
			t.Errorf("deploy.sh has no %q — %s is missing from the bridge lane", want.needle, want.why)
		}
	}

	// The wait-active loop must actually fail the deploy when the bridge
	// never comes up, exactly like the runner's loop does.
	waitBlock := ""
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, "is-active codex-bridge") {
			waitBlock = line
		}
	}
	if waitBlock == "" {
		t.Fatal("no wait-active line for codex-bridge found")
	}
	if !strings.Contains(waitBlock, "exit 1") {
		t.Errorf("the codex-bridge wait-active loop does not exit non-zero on timeout: %q", waitBlock)
	}
	if !strings.Contains(waitBlock, "status codex-bridge") {
		t.Errorf("the codex-bridge wait-active loop prints no status on failure, unlike the runner's: %q", waitBlock)
	}
}

// TestCodexDeployLaneRequiresBridgeEnvFromInstallSecrets asserts deploy.sh
// treats ~/.culture-nodes/codex-bridge.env as install-secrets.sh's
// responsibility — it checks for the file and fails with a message naming
// the script to run, rather than generating a token itself.
func TestCodexDeployLaneRequiresBridgeEnvFromInstallSecrets(t *testing.T) {
	script := deployScriptText(t)

	if !strings.Contains(script, "codex-bridge.env") {
		t.Fatal("deploy.sh never references codex-bridge.env; the missing-secret failure mode is unhandled")
	}
	if !regexp.MustCompile(`test -f [^\n]*codex-bridge\.env`).MatchString(script) {
		t.Error("deploy.sh does not test for codex-bridge.env before installing the unit")
	}
	if !strings.Contains(script, "install-secrets.sh") {
		t.Error("the missing-codex-bridge.env message does not name install-secrets.sh as the remedy")
	}
	// deploy.sh must never mint a bridge token itself — that is
	// install-secrets.sh's job (and its FORCE=1 rotation guard).
	if strings.Contains(script, "openssl rand") {
		t.Error("deploy.sh generates secret material; token generation belongs to install-secrets.sh alone")
	}
}

// tokenVarPattern finds every interpolated shell variable ($VAR / ${VAR})
// so the test below can check whether any of them is token-bearing.
var tokenVarPattern = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?`)

// tokenishName matches variable names that would carry secret material.
var tokenishName = regexp.MustCompile(`(?i)(TOKEN|SECRET|PASSWORD)`)

// TestCodexDeployLaneNeverInterpolatesTokenIntoSSHArgv mirrors
// codexsecrets_test.go's core discipline check onto deploy.sh: for every
// line that invokes ssh, no token-bearing variable may be interpolated into
// the portion at or after the "ssh" keyword — that portion becomes ssh's
// own argv, which is visible in the process table on both ends. Bearer
// tokens reach the hosts only through install-secrets.sh's stdin lane and
// are read back from mode-0600 files by systemd's EnvironmentFile.
//
// Note this checks *interpolated* names: a literal env-var NAME on the
// remote side (e.g. NODES_RUNNER_SECRET_FILE=$HOME/...) is a filename, not
// a secret, and is deliberately allowed.
func TestCodexDeployLaneNeverInterpolatesTokenIntoSSHArgv(t *testing.T) {
	script := deployScriptText(t)

	checked := 0
	for i, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		idx := strings.Index(line, "ssh ")
		if idx == -1 {
			continue
		}
		checked++
		argvPortion := line[idx:]
		for _, m := range tokenVarPattern.FindAllStringSubmatch(argvPortion, -1) {
			if tokenishName.MatchString(m[1]) {
				t.Errorf("line %d: %q interpolates $%s into ssh argv; secret material must never ride argv", i+1, line, m[1])
			}
		}
	}
	if checked == 0 {
		t.Fatal("no ssh invocation found in deploy.sh; this test is not proving anything")
	}
}
