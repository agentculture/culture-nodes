package deploytest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The rotation lanes of install-secrets.sh destroy credentials that cannot be
// recovered, and the damage is LATENT: processes already running keep their
// values in memory and only fail on the next restart. These tests pin the two
// guards added after a live incident on thor, where a single global FORCE=1 —
// passed to add one key to one file — rotated prod.env, codex-bridge.env and
// runner.secret at once.
//
// Guard 1 (scoping): each lane reads its own FORCE_* variable, so authorizing
// one rotation cannot authorize another.
// Guard 2 (confirmation): the destructive lane refuses the first time, writes
// a file naming what breaks, and proceeds only after a human or agent edits
// that file's verdict — within a window, and once per confirmation.

// installSecretsPath is provided by codexsecrets_test.go in this package.

// TestForceIsScopedPerLane asserts no lane reads a bare global FORCE. A single
// switch across every lane is what turned "add a key" into three rotations.
func TestForceIsScopedPerLane(t *testing.T) {
	body, err := os.ReadFile(installSecretsPath(t))
	if err != nil {
		t.Fatalf("reading script: %v", err)
	}
	script := string(body)

	if strings.Contains(script, "FORCE=${FORCE:-0}") {
		t.Error("install-secrets.sh still reads a bare global FORCE — one lane's authorization must never authorize another")
	}
	for _, want := range []string{
		"FORCE_PROD", "FORCE_RUNNER", "FORCE_CODEX", "FORCE_HUMAN_INBOX",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("install-secrets.sh has no %s — that lane cannot be authorized independently", want)
		}
	}
}

// runInstallSecrets invokes the script against an unresolvable host with an
// isolated confirmation directory. It never reaches ssh when the guard holds,
// which is the point: the refusal happens before anything leaves the machine.
func runInstallSecrets(t *testing.T, confirmDir string) string {
	t.Helper()
	cmd := exec.Command("bash", installSecretsPath(t), "host.invalid")
	cmd.Env = append(os.Environ(),
		"FORCE_PROD=1",
		"CONFIRM_DIR="+confirmDir,
	)
	out, _ := cmd.CombinedOutput() // a non-zero exit is the expected refusal
	return string(out)
}

func TestDestructiveRotationRefusesUntilConfirmed(t *testing.T) {
	dir := t.TempDir()

	out := runInstallSecrets(t, dir)
	if !strings.Contains(out, "REFUSED") {
		t.Fatalf("first rotation attempt was not refused; output:\n%s", out)
	}

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected exactly one confirmation file, got %v (err %v)", entries, err)
	}
	file := filepath.Join(dir, entries[0].Name())

	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading confirmation file: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "verdict: hold") {
		t.Error("confirmation file does not default to 'verdict: hold'")
	}
	// The file must state the consequence, not merely ask for a keystroke:
	// an agent that reads it should learn why the action is dangerous.
	for _, want := range []string{"POSTGRES_PASSWORD", "restart", "NOT recoverable"} {
		if !strings.Contains(text, want) {
			t.Errorf("confirmation file never mentions %q — it must name what breaks", want)
		}
	}

	// Re-running with the verdict untouched must refuse again.
	if out := runInstallSecrets(t, dir); !strings.Contains(out, "REFUSED") {
		t.Errorf("second attempt with an unedited verdict was not refused; output:\n%s", out)
	}
}

func TestConfirmedRotationIsSingleUse(t *testing.T) {
	dir := t.TempDir()
	runInstallSecrets(t, dir) // writes the confirmation file

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one confirmation file, got %v (err %v)", entries, err)
	}
	file := filepath.Join(dir, entries[0].Name())

	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading confirmation file: %v", err)
	}
	edited := strings.Replace(string(body), "verdict: hold", "verdict: rotate", 1)
	if err := os.WriteFile(file, []byte(edited), 0o600); err != nil {
		t.Fatalf("writing edited confirmation: %v", err)
	}

	out := runInstallSecrets(t, dir)
	if !strings.Contains(out, "confirmed rotation") {
		t.Fatalf("an edited verdict did not authorize the rotation; output:\n%s", out)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Error("confirmation file survived its use — a second rotation must need a second confirmation")
	}
}
