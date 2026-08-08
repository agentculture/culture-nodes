package actors_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
)

// Every test here drives the real client against a real HTTP server. There is
// no injected round-tripper: the client's job is to speak PRD §13 over HTTP,
// and a fake transport would let a header or a status-code mistake pass.

func testRequest() actors.InvocationRequest {
	return actors.InvocationRequest{
		RunID:     "run_test",
		TokenID:   "tok_test",
		NodeRunID: "nr_test",
		AttemptID: "att_test",
		Attempt:   1,
		Workflow:  actors.WorkflowRef{Name: "deliver-change", VersionDigest: "sha256:abc"},
		Node:      actors.NodeRef{ID: "build", ContractDigest: "sha256:def"},
		Input:     json.RawMessage(`{"subject":"x"}`),
		Callback:  actors.Callback{URL: "https://nodes.example/v1/attempts/att_test/events", Token: "tk"},
	}
}

// newClient returns a client whose retry sleeps are instant, so a test can
// prove the retry *policy* without paying for the backoff schedule.
func newClient(t *testing.T, opts ...actors.Option) *actors.Client {
	t.Helper()
	base := []actors.Option{
		actors.WithSleep(func(ctx context.Context, _ time.Duration) error { return ctx.Err() }),
	}
	return actors.NewClient(append(base, opts...)...)
}

func TestInvokeSynchronousResult(t *testing.T) {
	var got struct {
		body   actors.InvocationRequest
		path   string
		idem   string
		auth   string
		accept string
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.idem = r.Header.Get(actors.IdempotencyKeyHeader)
		got.auth = r.Header.Get("Authorization")
		got.accept = r.Header.Get("Accept")
		_ = json.NewDecoder(r.Body).Decode(&got.body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"outcome": "completed",
			"output": {"scope": "narrow"},
			"ledger_delta": {"records": []},
			"usage": {"input_tokens": 12, "output_tokens": 34, "cost": null, "currency": null}
		}`))
	}))
	defer server.Close()

	resp, err := newClient(t).Invoke(context.Background(),
		actors.Endpoint{URL: server.URL, AuthToken: "secret-workload-token"}, testRequest())
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if got.path != actors.InvocationPath {
		t.Errorf("POST path = %q, want %q (PRD §13.1)", got.path, actors.InvocationPath)
	}
	if got.idem != "att_test" {
		t.Errorf("%s = %q, want the attempt id", actors.IdempotencyKeyHeader, got.idem)
	}
	if got.auth != "Bearer secret-workload-token" {
		t.Errorf("Authorization = %q, want a bearer workload token", got.auth)
	}
	if got.accept != "application/json" {
		t.Errorf("Accept = %q, want application/json", got.accept)
	}
	if got.body.ProtocolVersion != actors.ProtocolVersion {
		t.Errorf("protocol_version = %q, want %q", got.body.ProtocolVersion, actors.ProtocolVersion)
	}
	if got.body.Node.ContractDigest != "sha256:def" {
		t.Errorf("node.contract_digest = %q, want it carried through", got.body.Node.ContractDigest)
	}
	if got.body.Callback.Token != "tk" {
		t.Errorf("callback.token = %q, want the attempt-scoped token", got.body.Callback.Token)
	}

	if resp.Async {
		t.Fatal("200 was reported as an asynchronous acceptance")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if resp.Requests != 1 {
		t.Errorf("Requests = %d, want 1 for a first-try success", resp.Requests)
	}
	if resp.Result.Outcome != "completed" {
		t.Errorf("outcome = %q, want completed", resp.Result.Outcome)
	}
	if string(resp.Result.Output) != `{"scope": "narrow"}` {
		t.Errorf("output = %s, want the actor's payload verbatim", resp.Result.Output)
	}
	if resp.Result.Usage == nil || resp.Result.Usage.InputTokens != 12 {
		t.Errorf("usage was not decoded: %+v", resp.Result.Usage)
	}
	if resp.Result.Usage.Cost != nil {
		t.Error("a null cost decoded as a non-nil pointer; §13.2 shows it nullable")
	}
}

func TestInvokeAsynchronousAcceptance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"invocation_id":"external_123","heartbeat_after_seconds":30,"supports_cancellation":true}`))
	}))
	defer server.Close()

	resp, err := newClient(t).Invoke(context.Background(), actors.Endpoint{URL: server.URL}, testRequest())
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !resp.Async {
		t.Fatal("202 was not reported as an asynchronous acceptance")
	}
	if resp.Result != nil {
		t.Error("an asynchronous acceptance carried a synchronous result")
	}
	if resp.Accepted.InvocationID != "external_123" {
		t.Errorf("invocation_id = %q, want external_123", resp.Accepted.InvocationID)
	}
	if resp.Accepted.HeartbeatAfterSeconds != 30 {
		t.Errorf("heartbeat_after_seconds = %d, want 30", resp.Accepted.HeartbeatAfterSeconds)
	}
	if !resp.Accepted.SupportsCancellation {
		t.Error("supports_cancellation was not decoded")
	}
}

// A 202 with no invocation id is refused: without one there is nothing to
// cancel (§13.6) and nothing to correlate an operator's question against.
func TestInvokeAsyncWithoutInvocationIDIsAContractFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"heartbeat_after_seconds":30}`))
	}))
	defer server.Close()

	_, err := newClient(t).Invoke(context.Background(), actors.Endpoint{URL: server.URL}, testRequest())
	assertClass(t, err, actors.ClassContract)
}

// A 200 with no outcome is a contract failure, not an empty success: §13.2's
// result body exists to carry a domain outcome, and an actor that omitted it
// has not answered the question the node asked.
func TestInvokeSyncWithoutOutcomeIsAContractFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"output":{"done":true}}`))
	}))
	defer server.Close()

	_, err := newClient(t).Invoke(context.Background(), actors.Endpoint{URL: server.URL}, testRequest())
	assertClass(t, err, actors.ClassContract)
	if got, _ := actors.ClassOf(err); got.Retryable() {
		t.Error("a contract failure was marked retryable")
	}
}

// The retry loop resends the SAME Idempotency-Key. That is the whole reason
// resending is safe (§20.3), so it is asserted rather than assumed.
func TestInvokeRetriesRetryableClassesWithTheSameIdempotencyKey(t *testing.T) {
	var (
		mu   sync.Mutex
		keys []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get(actors.IdempotencyKeyHeader))
		n := len(keys)
		mu.Unlock()

		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"outcome":"completed","output":{}}`))
	}))
	defer server.Close()

	resp, err := newClient(t, actors.WithMaxRequests(3)).
		Invoke(context.Background(), actors.Endpoint{URL: server.URL}, testRequest())
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Requests != 3 {
		t.Errorf("Requests = %d, want 3 (two 503s then a 200)", resp.Requests)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 3 {
		t.Fatalf("server saw %d requests, want 3", len(keys))
	}
	for i, key := range keys {
		if key != "att_test" {
			t.Errorf("request %d carried Idempotency-Key %q, want the attempt id on every retry", i+1, key)
		}
	}
}

func TestInvokeDoesNotRetryNonRetryableClasses(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"input does not satisfy the contract"}`))
	}))
	defer server.Close()

	_, err := newClient(t, actors.WithMaxRequests(5)).
		Invoke(context.Background(), actors.Endpoint{URL: server.URL}, testRequest())
	assertClass(t, err, actors.ClassActorRejectedInput)
	if requests != 1 {
		t.Errorf("server saw %d requests, want 1: a rejected input is not retried", requests)
	}

	var invErr *actors.InvocationError
	if !errors.As(err, &invErr) {
		t.Fatalf("error is not an *InvocationError: %v", err)
	}
	if !strings.Contains(invErr.Body, "does not satisfy") {
		t.Errorf("Body = %q, want the actor's error body captured for debugging", invErr.Body)
	}
}

// A rate-limited actor's Retry-After is honoured over the client's own
// schedule: it is the only party that knows when it will be ready.
func TestInvokeHonoursRetryAfter(t *testing.T) {
	var (
		mu     sync.Mutex
		delays []time.Duration
		calls  int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"outcome":"completed","output":{}}`))
	}))
	defer server.Close()

	client := actors.NewClient(
		actors.WithMaxRequests(2),
		actors.WithRetryBackoff(10*time.Millisecond, time.Minute),
		actors.WithSleep(func(_ context.Context, d time.Duration) error {
			mu.Lock()
			delays = append(delays, d)
			mu.Unlock()
			return nil
		}),
	)
	if _, err := client.Invoke(context.Background(), actors.Endpoint{URL: server.URL}, testRequest()); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(delays) != 1 || delays[0] != 2*time.Second {
		t.Errorf("retry delays = %v, want [2s] from the actor's Retry-After", delays)
	}
}

func TestInvokeTransportFailureIsRetryableTransport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close() // nothing is listening now

	_, err := newClient(t, actors.WithMaxRequests(1)).
		Invoke(context.Background(), actors.Endpoint{URL: url}, testRequest())
	assertClass(t, err, actors.ClassRetryableTransport)
	if got, _ := actors.ClassOf(err); !got.Retryable() {
		t.Error("a transport failure was not marked retryable")
	}
}

func TestInvokeCancelledContextIsCancelled(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte(`{"outcome":"completed"}`))
	}))
	defer server.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := newClient(t).Invoke(ctx, actors.Endpoint{URL: server.URL}, testRequest())
	assertClass(t, err, actors.ClassCancelled)
}

func TestInvokeDeadlineIsTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte(`{"outcome":"completed"}`))
	}))
	defer server.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := newClient(t).Invoke(ctx, actors.Endpoint{URL: server.URL}, testRequest())
	assertClass(t, err, actors.ClassTimeout)
}

// An invocation with no attempt id has no Idempotency-Key, which would make a
// retry indistinguishable from a second dispatch. It is refused before any
// request is made.
func TestInvokeRequiresAnAttemptID(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()

	req := testRequest()
	req.AttemptID = ""
	_, err := newClient(t).Invoke(context.Background(), actors.Endpoint{URL: server.URL}, req)
	assertClass(t, err, actors.ClassContract)
	if requests != 0 {
		t.Errorf("server saw %d requests, want 0: the invocation is refused before it is sent", requests)
	}
}

func TestEndpointURLNormalization(t *testing.T) {
	for _, tc := range []struct{ name, url string }{
		{"bare host", ""},
		{"trailing slash", "/"},
		{"full invocation URL", actors.InvocationPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var path string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path = r.URL.Path
				_, _ = w.Write([]byte(`{"outcome":"completed"}`))
			}))
			defer server.Close()

			_, err := newClient(t).Invoke(context.Background(),
				actors.Endpoint{URL: server.URL + tc.url}, testRequest())
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if path != actors.InvocationPath {
				t.Errorf("POST path = %q, want %q", path, actors.InvocationPath)
			}
		})
	}
}

func TestCancelIsBestEffort(t *testing.T) {
	t.Run("acknowledged", func(t *testing.T) {
		var (
			path string
			body actors.CancelRequest
		)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusNoContent)
		}))
		defer server.Close()

		if err := newClient(t).Cancel(context.Background(), actors.Endpoint{URL: server.URL}, "external_123", "run cancelled"); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if want := actors.InvocationPath + "/external_123/cancel"; path != want {
			t.Errorf("cancel path = %q, want %q", path, want)
		}
		if body.InvocationID != "external_123" || body.Reason != "run cancelled" {
			t.Errorf("cancel body = %+v, want the invocation id and reason", body)
		}
	})

	t.Run("refused", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotImplemented)
		}))
		defer server.Close()

		// A refusal is reported, but §13.6 makes it advisory: the caller has
		// already recorded the cancellation durably.
		err := newClient(t).Cancel(context.Background(), actors.Endpoint{URL: server.URL}, "external_123", "")
		if err == nil {
			t.Fatal("a refused cancellation reported no error at all")
		}
		var invErr *actors.InvocationError
		if !errors.As(err, &invErr) || invErr.Op != "cancel" {
			t.Fatalf("error is not a classified cancel failure: %v", err)
		}
	})
}

func assertClass(t *testing.T, err error, want actors.ErrorClass) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a %s failure, got no error", want)
	}
	got, ok := actors.ClassOf(err)
	if !ok {
		t.Fatalf("error is not classified: %v", err)
	}
	if got != want {
		t.Fatalf("class = %s, want %s (%v)", got, want, err)
	}
	if !errors.Is(err, actors.ErrInvocation) {
		t.Errorf("errors.Is(err, ErrInvocation) = false for %v", err)
	}
}

// A compile-time reminder that the request type carries every §13.1 field.
// A field removed from the struct breaks this, which is cheaper than
// discovering it in an adapter author's integration.
var _ = fmt.Sprintf("%v", actors.InvocationRequest{
	ProtocolVersion: actors.ProtocolVersion,
	RunID:           "", TokenID: "", NodeRunID: "", AttemptID: "", Attempt: 0,
	Workflow: actors.WorkflowRef{}, Node: actors.NodeRef{},
	Input: nil, ArtifactRefs: nil, ContextRefs: nil, Deadline: nil, Callback: actors.Callback{},
})
