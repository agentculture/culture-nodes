package main

// Tests for the `nodes run` verb (task t19, issue #36): the ad-hoc run
// lane. The flag/help scenarios run everywhere; the end-to-end scenarios
// exec the built binary against a real API server over the pgtest-provided
// PostgreSQL, and skip (via pgtest.RequireStore) when none is available —
// the same posture internal/api's own suite takes.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/api"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// testStore is set by TestMain (conformance_test.go) via pgtest.Run; nil
// when no PostgreSQL is available, in which case the end-to-end tests skip.
var testStore *storepg.Store

const testRunActorRef = "actor://company/codex-thor@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// runAPIServer boots a real API server over a fresh namespace for the CLI
// under test to talk to.
func runAPIServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := pgtest.RequireStore(t, testStore)
	ns := pgtest.MustNamespace(t, s, "cli-run")
	srv, err := api.NewServer(s, ns.ID,
		// the ad-hoc lane is bearer-gated (t15); tests present testAdhocToken
		api.WithAdhocRunSecret(testAdhocToken))
	if err != nil {
		t.Fatalf("api.NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

const testAdhocToken = "cli-test-adhoc-token-long-enough"

func TestRunHelpDocumentsLane(t *testing.T) {
	dir := t.TempDir()
	r := runNodes(t, dir, "run", "--help")

	assertNeverMixed(t, r)
	if r.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 for --help\nstderr=%s", r.ExitCode, r.Stderr)
	}
	if r.Stderr != "" {
		t.Fatalf("stderr = %q, want empty (--help output is a result)", r.Stderr)
	}
	for _, want := range []string{"--instruction", "--actor", "--repo", "--watch", "ad-hoc"} {
		if !strings.Contains(r.Stdout, want) {
			t.Fatalf("run --help output does not mention %q:\n%s", want, r.Stdout)
		}
	}
}

func TestRunIsNoLongerAStub(t *testing.T) {
	dir := t.TempDir()
	r := runNodes(t, dir, "run")

	if strings.Contains(r.Stderr, "not implemented yet") {
		t.Fatalf("`nodes run` still reports the stub error:\n%s", r.Stderr)
	}
}

func TestRunMissingFlagsIsUserError(t *testing.T) {
	dir := t.TempDir()
	r := runNodes(t, dir, "run")

	assertNeverMixed(t, r)
	if r.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1\nstderr=%s", r.ExitCode, r.Stderr)
	}
	assertErrorHintShape(t, r.Stderr)
	if !strings.Contains(r.Stderr, "--instruction") {
		t.Fatalf("stderr = %q, want it to point at the missing --instruction flag", r.Stderr)
	}
}

func TestRunEndToEndAgainstTestServer(t *testing.T) {
	ts := runAPIServer(t)
	dir := t.TempDir()

	r := runNodes(t, dir, "run", "--token", testAdhocToken,
		"--api", ts.URL,
		"--instruction", "review the CHANGELOG",
		"--actor", testRunActorRef,
		"--repo", "/tmp/culture-nodes",
		"--json")

	assertNeverMixed(t, r)
	if r.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr=%s", r.ExitCode, r.Stderr)
	}
	var payload runResultPayload
	assertSingleLineJSON(t, r.Stdout, &payload)
	if payload.RunID == "" {
		t.Fatalf("run_id missing from payload: %s", r.Stdout)
	}
	if !strings.HasPrefix(payload.WorkflowDigest, "sha256:") {
		t.Fatalf("workflow_digest = %q, want a sha256: digest", payload.WorkflowDigest)
	}
	if payload.State != "running" {
		t.Fatalf("state = %q, want running", payload.State)
	}

	// The run is a normal run readable from the ordinary run API.
	resp, err := http.Get(ts.URL + "/v1alpha1/runs/" + payload.RunID)
	if err != nil {
		t.Fatalf("GET run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET run: status = %d, want 200", resp.StatusCode)
	}

	// Identical parameters land on the identical published digest.
	r2 := runNodes(t, dir, "run", "--token", testAdhocToken,
		"--api", ts.URL,
		"--instruction", "review the CHANGELOG",
		"--actor", testRunActorRef,
		"--repo", "/tmp/culture-nodes",
		"--json")
	if r2.ExitCode != 0 {
		t.Fatalf("second run: exit code = %d, want 0\nstderr=%s", r2.ExitCode, r2.Stderr)
	}
	var payload2 runResultPayload
	assertSingleLineJSON(t, r2.Stdout, &payload2)
	if payload2.WorkflowDigest != payload.WorkflowDigest {
		t.Fatalf("digest changed across identical invocations: %q vs %q", payload2.WorkflowDigest, payload.WorkflowDigest)
	}
	if payload2.RunID == payload.RunID {
		t.Fatal("second invocation reused the first run id")
	}
}

func TestRunTextOutputPrintsRunID(t *testing.T) {
	ts := runAPIServer(t)
	dir := t.TempDir()

	r := runNodes(t, dir, "run", "--token", testAdhocToken,
		"--api", ts.URL,
		"--instruction", "say hello",
		"--actor", testRunActorRef,
		"--repo", "/tmp/culture-nodes")

	assertNeverMixed(t, r)
	if r.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr=%s", r.ExitCode, r.Stderr)
	}
	if !strings.Contains(r.Stdout, "run: ") {
		t.Fatalf("stdout = %q, want a 'run: <id>' line", r.Stdout)
	}
	if !strings.Contains(r.Stdout, "digest: sha256:") {
		t.Fatalf("stdout = %q, want a 'digest: sha256:...' line", r.Stdout)
	}
}

// TestRunWatchReportsTerminalState drives --watch to a terminal state by
// cancelling the run out from under it: watch must notice the transition,
// report the final state as a result on stdout, and carry the non-success
// domain outcome in the exit code (1), the same way `nodes validate` does
// for an invalid document.
func TestRunWatchReportsTerminalState(t *testing.T) {
	ts := runAPIServer(t)
	dir := t.TempDir()

	cmd := exec.Command(binPath, "run", "--token", testAdhocToken,
		"--api", ts.URL,
		"--instruction", "wait to be cancelled",
		"--actor", testRunActorRef,
		"--repo", "/tmp/culture-nodes",
		"--watch")
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start nodes run --watch: %v", err)
	}

	// Find the run through the ordinary list API, then cancel it.
	runID := waitForSingleRun(t, ts.URL)
	resp, err := http.Post(ts.URL+"/v1alpha1/runs/"+runID+"/cancel", "application/json", nil)
	if err != nil {
		t.Fatalf("cancel run: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel run: status = %d, want 200", resp.StatusCode)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("nodes run --watch did not exit within 30s of the run being cancelled\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}

	if code := cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("exit code = %d, want 1 for a cancelled run (domain outcome)\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "cancelled") {
		t.Fatalf("stdout = %q, want the final cancelled state reported as a result", stdout.String())
	}
}

// waitForSingleRun polls the runs list until exactly one run exists and
// returns its id.
func waitForSingleRun(t *testing.T, baseURL string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1alpha1/runs", nil)
		if err != nil {
			t.Fatalf("new list-runs request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("list runs: %v", err)
		}
		var list struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&list)
		resp.Body.Close()
		if decodeErr != nil {
			t.Fatalf("decode runs list: %v", decodeErr)
		}
		if len(list.Items) == 1 {
			return list.Items[0].ID
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for the watched run to appear; have %d runs", len(list.Items))
		case <-time.After(100 * time.Millisecond):
		}
	}
}
