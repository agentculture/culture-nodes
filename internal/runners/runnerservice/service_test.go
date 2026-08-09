package runnerservice_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/internal/runners/runnerservice"
)

const (
	testSecret   = "conformance-kit-runner-secret-do-not-reuse"
	testRevision = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testDigest   = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	testPolicyD  = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

// runnerFunc adapts a function to runners.Runner, so each test states the
// exact execution behaviour it is measuring the service against.
type runnerFunc func(context.Context, runners.Operation) (runners.Result, error)

func (f runnerFunc) Execute(ctx context.Context, op runners.Operation) (runners.Result, error) {
	return f(ctx, op)
}

func testOperation(id string) runners.Operation {
	return runners.Operation{
		OperationID:    id,
		Runner:         "fake",
		RunnerRevision: testRevision,
		Execution:      runners.Execution{Kind: runners.ExecutionContainer, ImageDigest: testDigest},
		Command:        runners.Command{Argv: []string{"true"}},
		Policy: runners.Policy{
			TimeoutSeconds:     30,
			Network:            runners.NetworkNone,
			AllowedOutputPaths: []string{},
		},
		Evidence: runners.EvidenceRequest{CaptureExit: true, CaptureLogs: true},
	}
}

// completedResult is what an honest runner returns for a trivial successful
// execution: a real exit code and observations that say it was measured.
func completedResult(op runners.Operation) runners.Result {
	code := 0
	now := time.Now().UTC()
	return runners.Result{
		OperationID: op.OperationID,
		State:       runners.StateCompleted,
		Exit:        &runners.Exit{Code: &code},
		Timing:      runners.Timing{StartedAt: now, FinishedAt: now.Add(time.Millisecond), DurationMs: 1},
		Environment: runners.Environment{
			RunnerRevision: op.RunnerRevision,
			ImageDigest:    op.Execution.ImageDigest,
			PolicyDigest:   testPolicyD,
		},
		Changes: runners.Changes{Complete: true},
		Observations: runners.Observations{
			ExitStatus:    runners.Observation{Measured: true, Complete: true, Method: "fake_wait_status"},
			ChangedPaths:  runners.Observation{Measured: true, Complete: true, Method: "fake_snapshot"},
			Logs:          runners.Observation{Measured: true, Complete: true, Method: "fake_capture"},
			ResourceUsage: runners.Observation{Measured: false, Complete: false, Note: "the fake runner measures nothing"},
		},
	}
}

type harness struct {
	svc    *runnerservice.Service
	server *httptest.Server
}

func newHarness(t *testing.T, runner runners.Runner, mutate func(*runnerservice.Config)) *harness {
	t.Helper()
	cfg := runnerservice.Config{
		Runner: runner,
		Store:  runnerservice.NewMemoryStore(),
		Secret: testSecret,
		OnError: func(err error) {
			t.Logf("service diagnostic: %v", err)
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	svc, err := runnerservice.New(cfg)
	if err != nil {
		t.Fatalf("runnerservice.New: %v", err)
	}
	server := httptest.NewServer(svc.Handler())
	t.Cleanup(func() {
		server.Close()
		svc.Close()
	})
	return &harness{svc: svc, server: server}
}

func (h *harness) request(t *testing.T, method, path, token string, body []byte, headers map[string]string) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, h.server.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set(runners.AuthorizationHeader, "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, payload
}

func (h *harness) execute(t *testing.T, op runners.Operation, token string) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(op)
	if err != nil {
		t.Fatalf("marshal operation: %v", err)
	}
	return h.request(t, http.MethodPost, runners.OperationsPath, token, body,
		map[string]string{runners.IdempotencyKeyHeader: op.OperationID})
}

func (h *harness) status(t *testing.T, id, token string) (int, []byte) {
	t.Helper()
	return h.request(t, http.MethodGet, runners.OperationsPath+"/"+id, token, nil, nil)
}

func (h *harness) waitTerminal(t *testing.T, id string) runners.OperationStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, body := h.status(t, id, testSecret)
		if code != http.StatusOK {
			t.Fatalf("status answered %d while polling: %s", code, body)
		}
		var observed runners.OperationStatus
		if err := json.Unmarshal(body, &observed); err != nil {
			t.Fatalf("decode status: %v (%s)", err, body)
		}
		if err := observed.Validate(); err != nil {
			t.Fatalf("status envelope invalid: %v (%s)", err, body)
		}
		if observed.Terminal() {
			return observed
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s never reached a terminal state (last %s)", id, observed.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func completingRunner() runners.Runner {
	return runnerFunc(func(_ context.Context, op runners.Operation) (runners.Result, error) {
		return completedResult(op), nil
	})
}

// ------------------------------------------------------------------- auth

func TestExecuteRefusesAnUnauthenticatedCaller(t *testing.T) {
	h := newHarness(t, completingRunner(), nil)
	op := testOperation("op-auth-1")

	for _, tc := range []struct{ name, token string }{
		{"no credential", ""},
		{"wrong credential", "not-the-secret"},
		{"prefix of the secret", testSecret[:10]},
		{"secret with a suffix", testSecret + "x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := h.execute(t, op, tc.token)
			if code != http.StatusUnauthorized && code != http.StatusForbidden {
				t.Fatalf("execute with %s answered %d, want 401/403: %s", tc.name, code, body)
			}
			if bytes.Contains(body, []byte(testSecret)) {
				t.Fatal("the refusal echoed the secret in its body")
			}
		})
	}
}

func TestStatusRefusesAnUnauthenticatedCallerBeforeCheckingExistence(t *testing.T) {
	h := newHarness(t, completingRunner(), nil)

	// An unknown operation: an unauthenticated caller must not be able to
	// tell "no such operation" from "not yours" — the endpoint would be an
	// operation-id oracle.
	code, body := h.status(t, "op-never-dispatched", "")
	if code != http.StatusUnauthorized && code != http.StatusForbidden {
		t.Fatalf("unauthenticated status answered %d, want 401/403 (never 404): %s", code, body)
	}
}

func TestNewRefusesAServiceWithoutASecret(t *testing.T) {
	_, err := runnerservice.New(runnerservice.Config{
		Runner: completingRunner(),
		Store:  runnerservice.NewMemoryStore(),
	})
	if err == nil {
		t.Fatal("New accepted a service with no bearer secret; an unauthenticated runner executes code for anyone who can reach it")
	}
}

// -------------------------------------------------------------- dispatch

func TestExecuteAnswers202WithAnAcceptanceTheRuntimeAccepts(t *testing.T) {
	h := newHarness(t, completingRunner(), nil)
	op := testOperation("op-accept-1")

	code, body := h.execute(t, op, testSecret)
	if code != http.StatusAccepted {
		t.Fatalf("execute answered %d, want 202: %s", code, body)
	}
	var acceptance runners.Acceptance
	if err := json.Unmarshal(body, &acceptance); err != nil {
		t.Fatalf("decode acceptance: %v (%s)", err, body)
	}
	if err := acceptance.Validate(op.OperationID); err != nil {
		t.Fatalf("the acceptance is not one the runtime would accept: %v", err)
	}
	if time.Duration(acceptance.StatusRetentionSeconds)*time.Second < runners.MinStatusRetention {
		t.Fatalf("declared retention %ds is below the protocol minimum %s",
			acceptance.StatusRetentionSeconds, runners.MinStatusRetention)
	}
}

func TestStatusIsAnswerableImmediatelyAfterAcceptance(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, runnerFunc(func(_ context.Context, op runners.Operation) (runners.Result, error) {
		<-release
		return completedResult(op), nil
	}), nil)
	defer close(release)

	op := testOperation("op-immediate-1")
	if code, body := h.execute(t, op, testSecret); code != http.StatusAccepted {
		t.Fatalf("execute answered %d: %s", code, body)
	}

	code, body := h.status(t, op.OperationID, testSecret)
	if code != http.StatusOK {
		t.Fatalf("status answered %d immediately after acceptance, want 200: %s", code, body)
	}
	var observed runners.OperationStatus
	if err := json.Unmarshal(body, &observed); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if err := observed.Validate(); err != nil {
		t.Fatalf("status envelope invalid: %v", err)
	}
	if observed.Terminal() {
		t.Fatal("the operation is terminal while the runner is still blocked; the service answered for work it never did")
	}
	if observed.Result != nil {
		t.Fatal("a non-terminal status carries a result")
	}
}

func TestExecuteRefusesASchemaInvalidOperation(t *testing.T) {
	h := newHarness(t, completingRunner(), nil)
	code, body := h.request(t, http.MethodPost, runners.OperationsPath, testSecret,
		[]byte(`{"operation_id":"op-bad","runner":"fake"}`), nil)
	if code != http.StatusBadRequest {
		t.Fatalf("a schema-invalid operation answered %d, want 400: %s", code, body)
	}
}

func TestExecuteRefusesAnUnsupportedProtocolVersion(t *testing.T) {
	h := newHarness(t, completingRunner(), nil)
	op := testOperation("op-version-1")
	body, err := json.Marshal(op)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	code, resp := h.request(t, http.MethodPost, runners.OperationsPath, testSecret, body,
		map[string]string{runners.ProtocolVersionHeader: "2.0"})
	if code != http.StatusBadRequest {
		t.Fatalf("a 2.0 protocol declaration answered %d, want 400: %s", code, resp)
	}
}

func TestExecuteRefusesAnIdempotencyKeyThatDisagreesWithTheBody(t *testing.T) {
	h := newHarness(t, completingRunner(), nil)
	op := testOperation("op-key-1")
	body, err := json.Marshal(op)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	code, resp := h.request(t, http.MethodPost, runners.OperationsPath, testSecret, body,
		map[string]string{runners.IdempotencyKeyHeader: "op-some-other-id"})
	if code != http.StatusBadRequest {
		t.Fatalf("a mismatched Idempotency-Key answered %d, want 400: %s", code, resp)
	}
}

func TestReDispatchReturnsTheSameAcceptanceAndRunsTheWorkOnce(t *testing.T) {
	var runs atomic.Int64
	h := newHarness(t, runnerFunc(func(_ context.Context, op runners.Operation) (runners.Result, error) {
		runs.Add(1)
		return completedResult(op), nil
	}), nil)

	op := testOperation("op-idem-1")
	_, first := h.execute(t, op, testSecret)
	h.waitTerminal(t, op.OperationID)
	code, second := h.execute(t, op, testSecret)
	if code != http.StatusAccepted {
		t.Fatalf("re-dispatch answered %d, want 202: %s", code, second)
	}
	if !bytes.Equal(bytes.TrimSpace(first), bytes.TrimSpace(second)) {
		t.Errorf("re-dispatch returned a different acceptance\nfirst:  %s\nsecond: %s", first, second)
	}
	time.Sleep(50 * time.Millisecond)
	if got := runs.Load(); got != 1 {
		t.Fatalf("the work ran %d times; at-least-once delivery must not become at-least-twice execution", got)
	}
}

func TestReDispatchWithADifferentDocumentIsAConflict(t *testing.T) {
	h := newHarness(t, completingRunner(), nil)
	op := testOperation("op-conflict-1")
	if code, body := h.execute(t, op, testSecret); code != http.StatusAccepted {
		t.Fatalf("first dispatch answered %d: %s", code, body)
	}

	changed := op
	changed.Command = runners.Command{Argv: []string{"false"}}
	code, body := h.execute(t, changed, testSecret)
	if code != http.StatusConflict {
		t.Fatalf("re-dispatching a different document under the same operation_id answered %d, want 409: %s", code, body)
	}
}

func TestStatusOfAnUnknownOperationIs404(t *testing.T) {
	h := newHarness(t, completingRunner(), nil)
	code, body := h.status(t, "op-never-dispatched", testSecret)
	if code != http.StatusNotFound {
		t.Fatalf("status of an undispatched operation answered %d, want 404: %s", code, body)
	}
}

func TestOversizeOperationIs413(t *testing.T) {
	h := newHarness(t, completingRunner(), func(cfg *runnerservice.Config) {
		cfg.MaxOperationBytes = 256
	})
	op := testOperation("op-large-1")
	op.Command.Argv = []string{"true", strings.Repeat("x", 4096)}
	code, body := h.execute(t, op, testSecret)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversize operation answered %d, want 413: %s", code, body)
	}
}

func TestAFullQueueIsRateLimitedRatherThanQueuedForever(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, runnerFunc(func(_ context.Context, op runners.Operation) (runners.Result, error) {
		<-release
		return completedResult(op), nil
	}), func(cfg *runnerservice.Config) {
		cfg.Concurrency = 1
		cfg.QueueDepth = 1
	})
	defer close(release)

	// One operation occupies the single worker and one fills the queue; the
	// next has nowhere to go. Whether the worker has already taken the first
	// job off the channel is a race the test does not need to win, so it
	// dispatches until the service pushes back.
	var accepted int
	for i := range 8 {
		op := testOperation("op-queue-" + string(rune('a'+i)))
		code, body := h.execute(t, op, testSecret)
		switch code {
		case http.StatusAccepted:
			accepted++
		case http.StatusTooManyRequests:
			if accepted == 0 {
				t.Fatal("the service rate-limited its very first dispatch")
			}
			return
		default:
			t.Fatalf("dispatch %d answered %d, want 202 or 429: %s", i, code, body)
		}
	}
	t.Fatal("a service with one worker and a queue depth of one accepted eight operations without pushing back; " +
		"a bounded pool that never says 429 is unbounded")
}

// ------------------------------------------------------- terminal outcomes

func TestATerminalStatusCarriesASchemaValidResultThatAgreesWithTheEnvelope(t *testing.T) {
	h := newHarness(t, completingRunner(), nil)
	op := testOperation("op-terminal-1")
	if code, body := h.execute(t, op, testSecret); code != http.StatusAccepted {
		t.Fatalf("execute answered %d: %s", code, body)
	}

	_, body := func() (int, []byte) {
		h.waitTerminal(t, op.OperationID)
		return h.status(t, op.OperationID, testSecret)
	}()

	var envelope struct {
		State  runners.State   `json:"state"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if envelope.State != runners.StateCompleted {
		t.Fatalf("state = %s, want completed", envelope.State)
	}
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatalf("build validator: %v", err)
	}
	if err := validator.ValidateJSON(runners.ResultSchemaPath, envelope.Result); err != nil {
		t.Fatalf("the served result is not schema-valid: %v\n%s", err, envelope.Result)
	}
}

// A refusal the wrapped runner can only decide after the service has already
// answered 202 has no HTTP error channel left. The honest answer is a
// terminal `rejected` status whose result declares that nothing was measured
// — never a fabricated failure claiming the work ran.
func TestAPostAcceptanceRefusalBecomesATerminalRejectedStatus(t *testing.T) {
	h := newHarness(t, runnerFunc(func(_ context.Context, op runners.Operation) (runners.Result, error) {
		return runners.Result{}, &runners.DispatchError{
			Kind:        runners.ErrorAuthOrPolicy,
			OperationID: op.OperationID,
			Detail:      "operation declares requires_shell; policy rejects it",
		}
	}), nil)

	op := testOperation("op-refused-1")
	if code, body := h.execute(t, op, testSecret); code != http.StatusAccepted {
		t.Fatalf("execute answered %d: %s", code, body)
	}
	observed := h.waitTerminal(t, op.OperationID)

	if observed.State != runners.StateRejected {
		t.Fatalf("state = %s, want rejected", observed.State)
	}
	if observed.Result.Error == nil || observed.Result.Error.Kind != runners.ErrorAuthOrPolicy {
		t.Fatalf("result error = %+v, want kind auth_or_policy", observed.Result.Error)
	}
	for _, name := range []string{"exit_status", "changed_paths", "logs", "resource_usage"} {
		obs, ok := observed.Result.Observations.Get(name)
		if !ok {
			t.Fatalf("the rejected result declares no %q observation", name)
		}
		if obs.Measured || obs.Complete {
			t.Errorf("observation %q claims measured=%v complete=%v on an operation that never ran",
				name, obs.Measured, obs.Complete)
		}
	}
	if observed.Result.Exit != nil {
		t.Error("a rejected result reports an exit; nothing exited")
	}
}

func TestAnUnclassifiedRunnerErrorIsNotReportedAsAMeasuredFailure(t *testing.T) {
	h := newHarness(t, runnerFunc(func(_ context.Context, _ runners.Operation) (runners.Result, error) {
		return runners.Result{}, io.ErrUnexpectedEOF
	}), nil)

	op := testOperation("op-unclassified-1")
	h.execute(t, op, testSecret)
	observed := h.waitTerminal(t, op.OperationID)

	if observed.State != runners.StateFailed {
		t.Fatalf("state = %s, want failed", observed.State)
	}
	if obs, _ := observed.Result.Observations.Get("exit_status"); obs.Measured {
		t.Error("an unclassified runner error produced a measured exit status")
	}
}

// ------------------------------------------------------------ cancellation

func TestCancellationStopsAnInFlightOperation(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	h := newHarness(t, runnerFunc(func(ctx context.Context, op runners.Operation) (runners.Result, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		result := completedResult(op)
		result.State = runners.StateCancelled
		result.Exit = nil
		result.Error = &runners.ResultError{Kind: runners.ErrorCancellation, Retryable: false, Message: "cancelled"}
		result.Observations.ExitStatus = runners.Observation{Measured: false, Complete: false, Note: "cancelled before exit"}
		return result, nil
	}), nil)

	op := testOperation("op-cancel-1")
	if code, body := h.execute(t, op, testSecret); code != http.StatusAccepted {
		t.Fatalf("execute answered %d: %s", code, body)
	}
	<-started

	code, body := h.request(t, http.MethodPost, runners.OperationsPath+"/"+op.OperationID+"/cancel", testSecret, nil, nil)
	if code != http.StatusAccepted && code != http.StatusNoContent {
		t.Fatalf("cancel answered %d, want 202/204: %s", code, body)
	}

	observed := h.waitTerminal(t, op.OperationID)
	if observed.State != runners.StateCancelled {
		t.Fatalf("state = %s, want cancelled", observed.State)
	}
}

// Cancelling something already finished is accepted and changes nothing: the
// request was valid, there was simply nothing left to stop. The runtime's own
// cancellation is durable before this call is ever made.
func TestCancellingAFinishedOperationIsAcceptedAndChangesNothing(t *testing.T) {
	h := newHarness(t, completingRunner(), nil)
	op := testOperation("op-cancel-late-1")
	h.execute(t, op, testSecret)
	before := h.waitTerminal(t, op.OperationID)

	code, body := h.request(t, http.MethodPost, runners.OperationsPath+"/"+op.OperationID+"/cancel", testSecret, nil, nil)
	if code != http.StatusAccepted && code != http.StatusNoContent {
		t.Fatalf("cancelling a finished operation answered %d, want 202/204: %s", code, body)
	}

	_, statusBody := h.status(t, op.OperationID, testSecret)
	var after runners.OperationStatus
	if err := json.Unmarshal(statusBody, &after); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if after.State != before.State {
		t.Fatalf("cancelling a finished operation changed its state from %s to %s", before.State, after.State)
	}
}

func TestCancellingAnUnknownOperationIs404(t *testing.T) {
	h := newHarness(t, completingRunner(), nil)
	code, _ := h.request(t, http.MethodPost, runners.OperationsPath+"/op-nope/cancel", testSecret, nil, nil)
	if code != http.StatusNotFound {
		t.Fatalf("cancelling an unknown operation answered %d, want 404", code)
	}
}

// --------------------------------------------------------------- callback

func TestTheCompletionCallbackCarriesNoResult(t *testing.T) {
	type received struct {
		auth string
		body []byte
	}
	notifications := make(chan received, 4)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		notifications <- received{auth: r.Header.Get(runners.AuthorizationHeader), body: payload}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer receiver.Close()

	h := newHarness(t, completingRunner(), nil)
	op := testOperation("op-callback-1")
	body, err := json.Marshal(op)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	code, resp := h.request(t, http.MethodPost, runners.OperationsPath, testSecret, body, map[string]string{
		runners.IdempotencyKeyHeader: op.OperationID,
		runners.CallbackURLHeader:    receiver.URL + "/events",
		runners.CallbackTokenHeader:  "callback-token-abc",
	})
	if code != http.StatusAccepted {
		t.Fatalf("execute answered %d: %s", code, resp)
	}

	select {
	case got := <-notifications:
		if got.auth != "Bearer callback-token-abc" {
			t.Errorf("callback Authorization = %q, want the token the dispatch issued", got.auth)
		}
		var notification map[string]any
		if err := json.Unmarshal(got.body, &notification); err != nil {
			t.Fatalf("decode notification: %v (%s)", err, got.body)
		}
		if _, present := notification["result"]; present {
			t.Error("the completion callback carries a result; it is a hint, and nothing may be committed on its word")
		}
		if notification["operation_id"] != op.OperationID {
			t.Errorf("notification names operation %v, want %s", notification["operation_id"], op.OperationID)
		}
		if notification["state"] != string(runners.StateCompleted) {
			t.Errorf("notification state = %v, want completed", notification["state"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no completion callback arrived within 5s")
	}
}

func TestNoCallbackIsSentWhenTheDispatchOfferedNone(t *testing.T) {
	h := newHarness(t, completingRunner(), nil)
	op := testOperation("op-nocallback-1")
	h.execute(t, op, testSecret)
	observed := h.waitTerminal(t, op.OperationID)
	if observed.State != runners.StateCompleted {
		t.Fatalf("state = %s, want completed; completion must work with no callback configured", observed.State)
	}
}

func TestACallbackToAnUnusableURLNeverFailsTheOperation(t *testing.T) {
	h := newHarness(t, completingRunner(), nil)
	op := testOperation("op-badcallback-1")
	body, err := json.Marshal(op)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	code, resp := h.request(t, http.MethodPost, runners.OperationsPath, testSecret, body, map[string]string{
		runners.IdempotencyKeyHeader: op.OperationID,
		runners.CallbackURLHeader:    "ftp://not-a-callback/events",
		runners.CallbackTokenHeader:  "token",
	})
	if code != http.StatusAccepted {
		t.Fatalf("execute answered %d: %s", code, resp)
	}
	observed := h.waitTerminal(t, op.OperationID)
	if observed.State != runners.StateCompleted {
		t.Fatalf("state = %s; a callback is best-effort and may never turn a completed operation into a failure",
			observed.State)
	}
}
