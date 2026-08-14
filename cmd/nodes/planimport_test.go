package main

// Tests for the `nodes plan-import` verb (task t22, issue #45). Like
// run_test.go, the flag/help scenarios run everywhere; the end-to-end
// scenarios exec the built binary against a real API server over the
// pgtest-provided PostgreSQL and skip when none is available.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// devagueTestdataPath resolves an absolute path into
// internal/devague/testdata — the same real `devague` fixtures
// internal/devague's own tests exercise MapPlanShow/MapDeviations against
// (see internal/devague/testdata/README.md). Absolute, because runNodes
// execs the built binary with cmd.Dir set to a fresh t.TempDir(), so a
// relative path would resolve against the wrong directory.
func devagueTestdataPath(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "internal", "devague", "testdata", name))
	if err != nil {
		t.Fatalf("resolve testdata path for %s: %v", name, err)
	}
	return abs
}

func TestPlanImportHelpDocumentsTheVerb(t *testing.T) {
	dir := t.TempDir()
	r := runNodes(t, dir, "plan-import", "--help")

	assertNeverMixed(t, r)
	if r.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 for --help\nstderr=%s", r.ExitCode, r.Stderr)
	}
	if r.Stderr != "" {
		t.Fatalf("stderr = %q, want empty (--help output is a result)", r.Stderr)
	}
	for _, want := range []string{"--plan", "--deviations", "devague"} {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("--help output does not mention %q:\n%s", want, r.Stdout)
		}
	}
}

func TestPlanImportMissingPlanFlagIsUserError(t *testing.T) {
	dir := t.TempDir()
	r := runNodes(t, dir, "plan-import")

	assertNeverMixed(t, r)
	if r.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1\nstderr=%s", r.ExitCode, r.Stderr)
	}
	assertErrorHintShape(t, r.Stderr)
	if !strings.Contains(r.Stderr, "--plan") {
		t.Fatalf("stderr = %q, want it to point at the missing --plan flag", r.Stderr)
	}
}

func TestPlanImportUnreadablePlanFileIsEnvError(t *testing.T) {
	dir := t.TempDir()
	r := runNodes(t, dir, "plan-import", "--plan", filepath.Join(dir, "does-not-exist.json"))

	assertNeverMixed(t, r)
	if r.ExitCode != 2 {
		t.Fatalf("exit code = %d, want 2 (unreadable file)\nstderr=%s", r.ExitCode, r.Stderr)
	}
	assertErrorHintShape(t, r.Stderr)
}

// TestPlanImportEndToEndAgainstTestServer is the t22 acceptance test,
// exercised through the CLI verb end to end: real per-task status and real
// dependency edges round-trip, deviations carry their origin, and the
// created import is readable back from the ordinary plan-imports API.
func TestPlanImportEndToEndAgainstTestServer(t *testing.T) {
	ts := runAPIServer(t)
	dir := t.TempDir()

	r := runNodes(t, dir, "plan-import",
		"--api", ts.URL,
		"--plan", devagueTestdataPath(t, "plan-show.json"),
		"--deviations", devagueTestdataPath(t, "deviations.json"),
		"--json")

	assertNeverMixed(t, r)
	if r.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr=%s", r.ExitCode, r.Stderr)
	}
	var payload planImportResultPayload
	assertSingleLineJSON(t, r.Stdout, &payload)
	if payload.ID == "" {
		t.Fatalf("id missing from payload: %s", r.Stdout)
	}
	if payload.Slug != "t22fixture" || payload.SourceSlug != "t22fixture" {
		t.Fatalf("payload = %+v, want slug/source_slug t22fixture", payload)
	}
	if payload.TaskCount != 5 {
		t.Fatalf("task_count = %d, want 5", payload.TaskCount)
	}
	if payload.DeviationCount != 3 {
		t.Fatalf("deviation_count = %d, want 3", payload.DeviationCount)
	}

	// The created import is a normal plan import readable from the
	// ordinary API.
	resp, err := http.Get(ts.URL + "/v1alpha1/plan-imports/" + payload.ID)
	if err != nil {
		t.Fatalf("GET plan import: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET plan import: status = %d, want 200", resp.StatusCode)
	}
}

// TestPlanImportMalformedPlanIsRefusedWithAHint is the malformed-input half
// of the t22 acceptance, exercised through the CLI: refused with a hint on
// stderr, exit 1, never a panic.
func TestPlanImportMalformedPlanIsRefusedWithAHint(t *testing.T) {
	ts := runAPIServer(t)
	dir := t.TempDir()

	malformed := filepath.Join(dir, "malformed-plan.json")
	malformedJSON := `{"slug": "p", "tasks": [
		{"id": "t1", "summary": "a", "origin": "user", "status": "confirmed", "deps": ["t99"]}
	]}`
	if err := os.WriteFile(malformed, []byte(malformedJSON), 0o600); err != nil {
		t.Fatalf("write malformed plan fixture: %v", err)
	}

	r := runNodes(t, dir, "plan-import", "--api", ts.URL, "--plan", malformed)

	assertNeverMixed(t, r)
	if r.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1 (the control plane refused a malformed plan)\nstderr=%s", r.ExitCode, r.Stderr)
	}
	assertErrorHintShape(t, r.Stderr)
}
