package main

// Conformance test — the acceptance heart of this package. It builds the
// real nodes binary and execs it as a subprocess for every scenario, so
// what's asserted is exactly what an external caller (a human or an agent
// parsing the CLI's output) would see: nothing here is possible to fake by
// calling internal Go functions directly.
//
// It locks:
//   - stdout/stderr are never mixed, for both success and failure paths;
//   - the text error shape has both "error:" and "hint:" lines;
//   - exit codes are 0/1/2 exactly as documented;
//   - --json yields valid single-line JSON on the *correct* stream for: a
//     success verb, an unknown verb, an unknown flag (parse-time), and
//     doctor (both its healthy and unhealthy paths).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var binPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "nodes-conformance-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "TestMain: MkdirTemp:", err)
		os.Exit(1)
	}

	binPath = filepath.Join(dir, "nodes-under-test")
	build := exec.Command("go", "build", "-o", binPath, ".")
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		fmt.Fprintln(os.Stderr, "TestMain: failed to build nodes binary for conformance tests:", buildErr)
		fmt.Fprintln(os.Stderr, string(out))
		os.RemoveAll(dir)
		os.Exit(1)
	}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type runResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// runNodes execs the built binary with args, in dir, capturing stdout and
// stderr on separate buffers (never combined) so the test can assert on
// each stream independently.
func runNodes(t *testing.T, dir string, args ...string) runResult {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Dir = dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("failed to run nodes binary %v: %v", args, runErr)
		}
	}
	return runResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitCode}
}

// assertNeverMixed fails the test unless at most one of stdout/stderr is
// non-empty — the never-mixed contract at its most literal.
func assertNeverMixed(t *testing.T, r runResult) {
	t.Helper()
	if r.Stdout != "" && r.Stderr != "" {
		t.Fatalf("stdout and stderr both non-empty (streams mixed):\nstdout=%q\nstderr=%q", r.Stdout, r.Stderr)
	}
}

// assertSingleLineJSON fails the test unless s is exactly one JSON value
// followed by exactly one trailing newline, and decodes it into target.
func assertSingleLineJSON(t *testing.T, s string, target any) {
	t.Helper()
	if s == "" {
		t.Fatal("expected single-line JSON, got empty string")
	}
	if strings.Count(s, "\n") != 1 || !strings.HasSuffix(s, "\n") {
		t.Fatalf("expected exactly one trailing newline (single-line JSON), got %q", s)
	}
	if err := json.Unmarshal([]byte(strings.TrimSuffix(s, "\n")), target); err != nil {
		t.Fatalf("invalid JSON %q: %v", s, err)
	}
}

func TestConformance_NoArgsPrintsUsageToStdout(t *testing.T) {
	dir := t.TempDir()
	r := runNodes(t, dir)

	assertNeverMixed(t, r)
	if r.Stdout == "" {
		t.Fatal("stdout is empty for a bare `nodes` invocation")
	}
	if r.Stderr != "" {
		t.Fatalf("stderr = %q, want empty (usage is not an error)", r.Stderr)
	}
	if r.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", r.ExitCode)
	}
}

func TestConformance_SuccessVerbText(t *testing.T) {
	dir := t.TempDir() // isolated: no culture.yaml above this, so whoami falls back deterministically
	r := runNodes(t, dir, "whoami")

	assertNeverMixed(t, r)
	if r.Stderr != "" {
		t.Fatalf("stderr = %q, want empty on a successful verb", r.Stderr)
	}
	if !strings.Contains(r.Stdout, "nick: culture-nodes") {
		t.Fatalf("stdout = %q, want it to contain the fallback nick", r.Stdout)
	}
	if r.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", r.ExitCode)
	}
}

func TestConformance_SuccessVerbJSON(t *testing.T) {
	dir := t.TempDir()
	r := runNodes(t, dir, "whoami", "--json")

	assertNeverMixed(t, r)
	if r.Stderr != "" {
		t.Fatalf("stderr = %q, want empty", r.Stderr)
	}
	if r.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", r.ExitCode)
	}

	var payload struct {
		Nick    string `json:"nick"`
		Version string `json:"version"`
		Backend string `json:"backend"`
		Model   string `json:"model"`
	}
	assertSingleLineJSON(t, r.Stdout, &payload)
	if payload.Nick != "culture-nodes" {
		t.Fatalf("nick = %q, want %q", payload.Nick, "culture-nodes")
	}
}

func TestConformance_UnknownVerbText(t *testing.T) {
	dir := t.TempDir()
	r := runNodes(t, dir, "frobnicate")

	assertNeverMixed(t, r)
	if r.Stdout != "" {
		t.Fatalf("stdout = %q, want empty on failure", r.Stdout)
	}
	if r.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", r.ExitCode)
	}
	assertErrorHintShape(t, r.Stderr)
}

func TestConformance_UnknownVerbJSON(t *testing.T) {
	dir := t.TempDir()
	r := runNodes(t, dir, "frobnicate", "--json")

	assertNeverMixed(t, r)
	if r.Stdout != "" {
		t.Fatalf("stdout = %q, want empty on failure", r.Stdout)
	}
	if r.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", r.ExitCode)
	}

	var payload struct {
		Code        int    `json:"code"`
		Message     string `json:"message"`
		Remediation string `json:"remediation"`
	}
	assertSingleLineJSON(t, r.Stderr, &payload)
	if payload.Code != 1 {
		t.Fatalf("code = %d, want 1", payload.Code)
	}
	if payload.Message == "" || payload.Remediation == "" {
		t.Fatalf("payload = %+v, want non-empty message and remediation", payload)
	}
}

func TestConformance_UnknownFlagParseTimeText(t *testing.T) {
	dir := t.TempDir()
	r := runNodes(t, dir, "whoami", "--bogus")

	assertNeverMixed(t, r)
	if r.Stdout != "" {
		t.Fatalf("stdout = %q, want empty on a parse-time failure", r.Stdout)
	}
	if r.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", r.ExitCode)
	}
	assertErrorHintShape(t, r.Stderr)
}

func TestConformance_UnknownFlagParseTimeJSON(t *testing.T) {
	dir := t.TempDir()
	// --json placed after the bad flag: this is exactly the scenario the
	// pre-scan exists for — flag.FlagSet.Parse fails on --bogus before it
	// ever sees --json, so only a pre-scan of raw argv can know jsonMode.
	r := runNodes(t, dir, "whoami", "--bogus", "--json")

	assertNeverMixed(t, r)
	if r.Stdout != "" {
		t.Fatalf("stdout = %q, want empty on a parse-time failure", r.Stdout)
	}
	if r.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", r.ExitCode)
	}

	var payload struct {
		Code        int    `json:"code"`
		Message     string `json:"message"`
		Remediation string `json:"remediation"`
	}
	assertSingleLineJSON(t, r.Stderr, &payload)
	if payload.Code != 1 {
		t.Fatalf("code = %d, want 1", payload.Code)
	}
}

func TestConformance_DoctorUnhealthyText(t *testing.T) {
	dir := t.TempDir() // no culture.yaml anywhere above this
	r := runNodes(t, dir, "doctor")

	assertNeverMixed(t, r)
	// This is the domain-outcome-vs-technical-status distinction: doctor's
	// unhealthy verdict is a *result*, so it prints to stdout and stderr
	// stays empty, even though the exit code is non-zero.
	if r.Stderr != "" {
		t.Fatalf("stderr = %q, want empty — an unhealthy doctor verdict is a result, not a CliError", r.Stderr)
	}
	if r.Stdout == "" {
		t.Fatal("stdout is empty, want the {check,status,detail} table")
	}
	if !strings.Contains(r.Stdout, "culture_yaml_present") {
		t.Fatalf("stdout does not mention the culture_yaml_present check:\n%s", r.Stdout)
	}
	if r.ExitCode != 2 {
		t.Fatalf("exit code = %d, want 2", r.ExitCode)
	}
}

func TestConformance_DoctorUnhealthyJSON(t *testing.T) {
	dir := t.TempDir()
	r := runNodes(t, dir, "doctor", "--json")

	assertNeverMixed(t, r)
	if r.Stderr != "" {
		t.Fatalf("stderr = %q, want empty", r.Stderr)
	}
	if r.ExitCode != 2 {
		t.Fatalf("exit code = %d, want 2", r.ExitCode)
	}

	var payload struct {
		Healthy bool `json:"healthy"`
		Checks  []struct {
			Check  string `json:"check"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	assertSingleLineJSON(t, r.Stdout, &payload)
	if payload.Healthy {
		t.Fatal("healthy = true, want false with no culture.yaml present")
	}
	if len(payload.Checks) != 2 {
		t.Fatalf("len(checks) = %d, want 2", len(payload.Checks))
	}
}

func TestConformance_DoctorHealthyFromRepoRoot(t *testing.T) {
	repoRoot := repoRootForTest(t)
	r := runNodes(t, repoRoot, "doctor", "--json")

	assertNeverMixed(t, r)
	if r.Stderr != "" {
		t.Fatalf("stderr = %q, want empty", r.Stderr)
	}
	if r.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (repo root has culture.yaml and go is on PATH)\nstdout=%s", r.ExitCode, r.Stdout)
	}

	var payload struct {
		Healthy bool `json:"healthy"`
	}
	assertSingleLineJSON(t, r.Stdout, &payload)
	if !payload.Healthy {
		t.Fatal("healthy = false, want true when run from the repo root")
	}
}

func TestConformance_StubModeIsCliErrorNotResult(t *testing.T) {
	// "scheduler" stands in for a still-stubbed process mode; "serve" and
	// "all" are real as of this task (cmd/nodes/serve.go) and are exercised
	// by internal/api's own suite and TestConformance_ServeRequiresDatabaseURL
	// below instead.
	dir := t.TempDir()
	r := runNodes(t, dir, "scheduler")

	assertNeverMixed(t, r)
	if r.Stdout != "" {
		t.Fatalf("stdout = %q, want empty — a not-implemented mode is a CliError", r.Stdout)
	}
	if r.ExitCode != 2 {
		t.Fatalf("exit code = %d, want 2", r.ExitCode)
	}
	assertErrorHintShape(t, r.Stderr)
	if !strings.Contains(r.Stderr, "not implemented yet") {
		t.Fatalf("stderr = %q, want it to mention 'not implemented yet'", r.Stderr)
	}
}

// TestConformance_ServeRequiresDatabaseURL proves `nodes serve` is real
// (not a stub) without needing an actual PostgreSQL instance for this
// package's conformance suite: run with NODES_DATABASE_URL scrubbed from
// the environment, it must fail fast as a genuine CliError (code 2) rather
// than hang trying to listen, and rather than reporting "not implemented
// yet" (see TestConformance_StubModeIsCliErrorNotResult, which now covers a
// mode that still is one). The database-backed path itself — actually
// serving requests — is internal/api's own suite's job.
func TestConformance_ServeRequiresDatabaseURL(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(binPath, "serve")
	cmd.Dir = dir
	cmd.Env = scrubEnv(os.Environ(), "NODES_DATABASE_URL")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("failed to run nodes serve: %v", runErr)
		}
	}
	r := runResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitCode}

	assertNeverMixed(t, r)
	if r.Stdout != "" {
		t.Fatalf("stdout = %q, want empty", r.Stdout)
	}
	if r.ExitCode != 2 {
		t.Fatalf("exit code = %d, want 2\nstderr=%s", r.ExitCode, r.Stderr)
	}
	assertErrorHintShape(t, r.Stderr)
	if !strings.Contains(r.Stderr, "no database URL configured") {
		t.Fatalf("stderr = %q, want it to mention the missing database URL", r.Stderr)
	}
	if strings.Contains(r.Stderr, "not implemented yet") {
		t.Fatalf("stderr = %q, serve is implemented now — this should not be the stub error", r.Stderr)
	}
}

// scrubEnv returns env with every entry named key removed.
func scrubEnv(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func TestConformance_ExplainUnknownPathListsKnownPaths(t *testing.T) {
	dir := t.TempDir()
	r := runNodes(t, dir, "explain", "bogus")

	assertNeverMixed(t, r)
	if r.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", r.ExitCode)
	}
	assertErrorHintShape(t, r.Stderr)
	if !strings.Contains(r.Stderr, "whoami") {
		t.Fatalf("stderr = %q, want the hint to list known paths (e.g. whoami)", r.Stderr)
	}
}

// assertErrorHintShape checks the two-line text error rubric:
//
//	error: <message>
//	hint: <remediation>
func assertErrorHintShape(t *testing.T, stderr string) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("stderr = %q, want at least an error: line and a hint: line", stderr)
	}
	if !strings.HasPrefix(lines[0], "error: ") {
		t.Fatalf("stderr first line = %q, want it to start with %q", lines[0], "error: ")
	}
	foundHint := false
	for _, line := range lines[1:] {
		if strings.HasPrefix(line, "hint: ") {
			foundHint = true
		}
	}
	if !foundHint {
		t.Fatalf("stderr = %q, want a hint: line", stderr)
	}
}

// repoRootForTest resolves the repo root from this test file's own
// location (cmd/nodes) rather than hard-coding a path, so it is unaffected
// by the worktree's location on disk.
func repoRootForTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root := filepath.Join(wd, "..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("resolved repo root %q does not contain go.mod: %v", root, err)
	}
	return root
}
