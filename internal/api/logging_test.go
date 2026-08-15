package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
)

// This file's tests are white-box (package api, not api_test) because they
// exercise (*Server).wrap/(*Server).writeAPIError and
// (*Server).logCallbackFailures directly, against a bare *Server carrying
// only a test-sink logger — none of the logging behavior added by this
// task depends on Store, Engine, or Ledger being real, so a fake handler
// (for the generic funnel) or a fake actors.CallbackStore/actors.Completer
// (for the callback route, both public interfaces internal/actors already
// declares for exactly this purpose) stands in for "the engine/store
// failed" without requiring PostgreSQL.

// testLogger returns a JSON-handler slog.Logger writing to buf, so a test
// can assert on the exact attributes a log line carries rather than just a
// message substring.
func testLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, nil))
}

// decodeLastJSONLine decodes the last non-empty line of logged (one JSON
// object per slog.JSONHandler line) into v, so a test can assert on a
// specific log call's attributes even when an earlier, unrelated line
// preceded it.
func decodeLastJSONLine(logged string, v any) error {
	lines := strings.Split(strings.TrimRight(logged, "\n"), "\n")
	if len(lines) == 0 || lines[len(lines)-1] == "" {
		return fmt.Errorf("no log line found in %q", logged)
	}
	return json.Unmarshal([]byte(lines[len(lines)-1]), v)
}

// ---- the generic 5xx funnel: (*Server).wrap / (*Server).writeAPIError ----

func TestWrapLogsErrorLevelOn5xxWithFullChainAndRequestLine(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{log: testLogger(&buf)}

	rootCause := errors.New("connection reset by peer")
	failing := func(w http.ResponseWriter, r *http.Request) error {
		// A handler simulating what a failing engine/store returns: an
		// internalError wrapping the underlying infrastructure failure,
		// exactly what runs.go/workflows.go/ledger.go etc. all do on a
		// genuine Store or Engine error (see errors.go's internalError).
		return internalError(fmt.Errorf("query workflows: %w", rootCause))
	}

	ts := httptest.NewServer(s.wrap(failing))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1alpha1/workflows")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}

	logged := buf.String()
	var rec struct {
		Level  string `json:"level"`
		Method string `json:"method"`
		Path   string `json:"path"`
		Status int    `json:"status"`
		Error  string `json:"error"`
	}
	if err := decodeLastJSONLine(logged, &rec); err != nil {
		t.Fatalf("decode log line %q: %v", logged, err)
	}
	if rec.Level != "ERROR" {
		t.Errorf("level = %q, want ERROR", rec.Level)
	}
	if rec.Method != http.MethodGet {
		t.Errorf("method = %q, want GET", rec.Method)
	}
	if rec.Path != "/v1alpha1/workflows" {
		t.Errorf("path = %q, want /v1alpha1/workflows", rec.Path)
	}
	if rec.Status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Status)
	}
	if !strings.Contains(rec.Error, "connection reset by peer") {
		t.Errorf("error = %q, want it to contain the wrapped root cause", rec.Error)
	}
	if !strings.Contains(rec.Error, "query workflows") {
		t.Errorf("error = %q, want it to contain the wrapping context too (the full chain)", rec.Error)
	}
}

func TestWrapLogsNothingAtErrorLevelOn2xxOr4xx(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{log: testLogger(&buf)}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ok", s.wrap(func(w http.ResponseWriter, r *http.Request) error {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return nil
	}))
	mux.HandleFunc("GET /bad", s.wrap(func(w http.ResponseWriter, r *http.Request) error {
		// A domain/user outcome, not an infrastructure failure — never
		// paged on, per errors.go's writeAPIError doc.
		return badRequest("fix the request", "malformed input")
	}))

	ts := httptest.NewServer(mux)
	defer ts.Close()

	for _, path := range []string{"/ok", "/bad"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
	}

	if strings.Contains(buf.String(), `"level":"ERROR"`) {
		t.Fatalf("expected no Error-level log line for a 2xx or 4xx response, got: %s", buf.String())
	}
}

// ---- the actor callback ingest route: logCallbackFailures ----

const testCallbackSecret = "test-callback-token-secret-32by"

// fakeCallbackStore is the minimal actors.CallbackStore a terminal
// completed/failed event needs to reach commitTerminal: every method just
// reports success except Invocation, which returns the fixed record the
// test configured (matching the token minted for the same attempt id).
type fakeCallbackStore struct {
	inv actors.PendingInvocation
}

func (f *fakeCallbackStore) Invocation(ctx context.Context, attemptID string) (actors.PendingInvocation, error) {
	return f.inv, nil
}
func (f *fakeCallbackStore) ClaimCallbackEvent(ctx context.Context, inv actors.PendingInvocation, eventID string) (bool, error) {
	return true, nil
}
func (f *fakeCallbackStore) ReleaseCallbackEvent(ctx context.Context, inv actors.PendingInvocation, eventID string) error {
	return nil
}
func (f *fakeCallbackStore) AdvanceCallbackSequence(ctx context.Context, attemptID string, sequence int64) (bool, error) {
	return true, nil
}
func (f *fakeCallbackStore) TouchInvocation(ctx context.Context, attemptID, invocationID string, at time.Time) error {
	return nil
}
func (f *fakeCallbackStore) RollbackCallbackSequence(ctx context.Context, attemptID string, from, to int64) error {
	return nil
}
func (f *fakeCallbackStore) ReparkResumedWork(ctx context.Context, inv actors.PendingInvocation) error {
	return nil
}
func (f *fakeCallbackStore) CloseInvocation(ctx context.Context, attemptID, state string) error {
	return nil
}
func (f *fakeCallbackStore) RecordSupersedingAttempt(
	ctx context.Context, inv actors.PendingInvocation, callbackEventID string, req engine.CompletionRequest,
) (actors.SupersedingAttempt, error) {
	return actors.SupersedingAttempt{AttemptID: "att_superseding"}, nil
}
func (f *fakeCallbackStore) EmitSignalEvent(ctx context.Context, inv actors.PendingInvocation, in actors.EmitSignalInput) (actors.EmitSignalResult, error) {
	return actors.EmitSignalResult{}, nil
}
func (f *fakeCallbackStore) ResumeWaitingWork(ctx context.Context, inv actors.PendingInvocation, lease time.Duration) error {
	return nil
}
func (f *fakeCallbackStore) AppendRunEvent(ctx context.Context, namespaceID, runID, eventType string, data map[string]any) error {
	return nil
}

var _ actors.CallbackStore = (*fakeCallbackStore)(nil)

// fakeCompleter simulates the engine's commit succeeding or failing, so a
// terminal-commit failure (this task's c2 log half) is reproducible without
// a real engine or PostgreSQL.
type fakeCompleter struct {
	err error
}

func (f *fakeCompleter) CompleteAttempt(ctx context.Context, req engine.CompletionRequest) (engine.CompletionResult, error) {
	if f.err != nil {
		return engine.CompletionResult{}, f.err
	}
	return engine.CompletionResult{}, nil
}

var _ actors.Completer = (*fakeCompleter)(nil)

// newCallbackTestServer mounts actors.NewCallbackHandler behind
// logCallbackFailures exactly as Handler does in server.go, wired to a
// fake store/engine so the test controls whether the terminal commit
// succeeds or fails. It returns the running server, a valid token for
// attemptID, and the buffer every log line lands in.
func newCallbackTestServer(t *testing.T, attemptID string, completeErr error) (ts *httptest.Server, token string, logs *bytes.Buffer) {
	t.Helper()

	signer, err := actors.NewTokenSigner([]byte(testCallbackSecret))
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}
	token, err = signer.Mint(attemptID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	store := &fakeCallbackStore{inv: actors.PendingInvocation{
		AttemptID:    attemptID,
		NamespaceID:  "ns-test",
		RunID:        "run-test",
		NodeRunID:    "noderun-test",
		NodeID:       "node-test",
		WorkID:       "work-test",
		WorkerID:     "worker-test",
		FencingToken: 1,
		Attempt:      1,
	}}

	logs = &bytes.Buffer{}
	s := &Server{log: testLogger(logs)}

	mux := http.NewServeMux()
	mux.Handle("POST "+callbackRoutePattern, s.logCallbackFailures(actors.NewCallbackHandler(actors.CallbackDeps{
		Store:  store,
		Engine: &fakeCompleter{err: completeErr},
		Signer: signer,
	})))

	ts = httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, token, logs
}

func postCallbackEvent(t *testing.T, ts *httptest.Server, attemptID, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+fmt.Sprintf("/v1/attempts/%s/events", attemptID), strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST callback event: %v", err)
	}
	return resp
}

func TestLogCallbackFailuresLogsAttemptIDOnTerminalCommitFailure(t *testing.T) {
	attemptID := "att_test-terminal-commit-failure"
	ts, token, logs := newCallbackTestServer(t, attemptID, errors.New("engine: commit attempt: db connection reset"))

	resp := postCallbackEvent(t, ts, attemptID, token,
		`{"event_id":"ev-1","sequence":1,"kind":"completed","payload":{"outcome":"ok"}}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}

	var rec struct {
		Level     string `json:"level"`
		Method    string `json:"method"`
		Path      string `json:"path"`
		Status    int    `json:"status"`
		AttemptID string `json:"attempt_id"`
		Error     string `json:"error"`
	}
	if err := decodeLastJSONLine(logs.String(), &rec); err != nil {
		t.Fatalf("decode log line %q: %v", logs.String(), err)
	}
	if rec.Level != "ERROR" {
		t.Errorf("level = %q, want ERROR", rec.Level)
	}
	if rec.AttemptID != attemptID {
		t.Errorf("attempt_id = %q, want %q", rec.AttemptID, attemptID)
	}
	if rec.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", rec.Method)
	}
	if rec.Status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Status)
	}
	if !strings.Contains(rec.Error, "db connection reset") {
		t.Errorf("error = %q, want it to mention the terminal-commit failure", rec.Error)
	}
}

func TestLogCallbackFailuresLogsNothingAtErrorLevelOn2xx(t *testing.T) {
	attemptID := "att_test-terminal-commit-success"
	ts, token, logs := newCallbackTestServer(t, attemptID, nil)

	resp := postCallbackEvent(t, ts, attemptID, token,
		`{"event_id":"ev-1","sequence":1,"kind":"completed","payload":{"outcome":"ok"}}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Fatalf("expected no Error-level log line for a committed terminal event, got: %s", logs.String())
	}
}
