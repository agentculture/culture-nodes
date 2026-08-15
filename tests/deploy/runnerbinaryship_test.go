package deploytest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #17, reproduced rather than argued.
//
// Observed live on the 2026-08-11 thor deploy: the runner-binary fallback
// path failed with `scp: dest open ".culture-nodes/bin/nodes-runner":
// Failure` -- ETXTBSY, because the destination is the binary of the RUNNING
// nodes-runner unit -- and the deploy carried on regardless, restarting the
// unit on the previous build. A re-deploy that changes cmd/nodes-runner
// silently keeps shipping nothing.
//
// `set -euo pipefail` did not stop it, and that is not a bug in bash. The
// fallback was a `{ ... }` group on the right of `||`, containing an `&&`
// chain. Every command in an `&&` list except the last runs with -e ignored,
// and the manual is explicit that when a compound command returns non-zero
// "because a command failed while -e was being ignored, the shell does not
// exit". So the scp failure propagated as the group's status and was then
// discarded by the very rule that exempted it.
//
// These tests drive the REAL deploy/prod/deploy.sh with ssh, scp and go
// stubbed on PATH. Nothing leaves the machine: the stub ssh answers every
// command locally, and the one it is told to fail is the remote `go build`,
// which is exactly what puts the script on the fallback path the incident
// took.

// deployScriptPath locates deploy/prod/deploy.sh from this test file.
func deployScriptPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(prodComposeDir(t), "deploy.sh")
}

// stubBin writes the fake ssh/scp/go this test runs deploy.sh against.
//
// The stub ssh understands three commands and treats every other one as a
// success, because this test is about ONE step: everything before it must be
// allowed to happen, and the step after it is the tripwire.
func stubBin(t *testing.T, dir string, marker string) {
	t.Helper()
	write := func(name, body string) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}

	write("ssh", `#!/usr/bin/env bash
cmd="$*"
case "$cmd" in
  *"go build -o ~/.culture-nodes/bin/nodes-runner"*)
    # "remote Go missing" -- the condition that sends the script down the
    # local-build-and-copy fallback the incident took.
    echo "bash: line 1: go: command not found" >&2
    exit 127
    ;;
  *"command -v headspace"*)
    # THE TRIPWIRE. This is the very next step after the runner binary is
    # shipped. Reaching it means the deploy continued past a failed ship.
    : > "$MARKER_FILE"
    exit 97
    ;;
esac
# Consume any piped stdin (deploy.sh pipes a git archive into the first ssh).
cat >/dev/null 2>&1 || true
exit 0
`)

	write("scp", `#!/usr/bin/env bash
# The reported failure, verbatim: overwriting the binary of the running unit.
echo 'scp: dest open ".culture-nodes/bin/nodes-runner": Failure' >&2
exit 1
`)

	write("go", `#!/usr/bin/env bash
# Produce the artifact `+"`go build -o`"+` promises, then succeed: the local
# build is not what fails in this scenario.
out=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$out" ] && : > "$out"
exit 0
`)
	_ = marker
}

// TestFailedRunnerBinaryShipAbortsTheDeploy is the regression test for #17.
// It fails on the parent commit: the marker file exists there, because the
// deploy walked straight past a runner binary that never landed.
func TestFailedRunnerBinaryShipAbortsTheDeploy(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir stub bin: %v", err)
	}
	marker := filepath.Join(dir, "continued-past-a-failed-ship")
	stubBin(t, binDir, marker)

	cmd := exec.Command("bash", deployScriptPath(t), "thor")
	cmd.Dir = filepath.Dir(filepath.Dir(deployScriptPath(t))) // repo root's deploy/ parent
	cmd.Dir = filepath.Dir(cmd.Dir)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"MARKER_FILE="+marker,
	)
	out, err := cmd.CombinedOutput()

	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatalf("the deploy continued past a FAILED runner-binary ship (issue #17): the next "+
			"step ran, so the unit would be restarted on the previous build.\n--- output ---\n%s",
			out)
	}
	if err == nil {
		t.Fatalf("deploy.sh exited 0 after the runner binary failed to ship\n--- output ---\n%s", out)
	}
	if !strings.Contains(string(out), "scp") && !strings.Contains(string(out), "nodes-runner") {
		t.Errorf("the abort names neither scp nor nodes-runner, so an operator cannot see what "+
			"failed:\n--- output ---\n%s", out)
	}
}

// TestRunnerBinaryIsRenamedIntoPlace pins the OTHER half of the fix. Aborting
// loudly on ETXTBSY would be an improvement and a permanently broken deploy:
// the running unit's binary can never be overwritten in place. A rename can
// -- the old inode stays valid while it executes -- so the ship writes a
// temp name and moves it over.
func TestRunnerBinaryIsRenamedIntoPlace(t *testing.T) {
	body, err := os.ReadFile(deployScriptPath(t))
	if err != nil {
		t.Fatalf("read deploy.sh: %v", err)
	}
	script := string(body)
	if !strings.Contains(script, "nodes-runner.new") {
		t.Error("deploy.sh still writes the runner binary straight over the running unit's " +
			"path; ETXTBSY is not an error condition to report, it is one to avoid (issue #17)")
	}
	if !strings.Contains(script, "mv") {
		t.Error("deploy.sh ships a temp binary but never moves it into place")
	}
}
