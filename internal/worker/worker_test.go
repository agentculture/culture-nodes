package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/engine"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// End-to-end worker tests: a real PostgreSQL, a real engine, a real HTTP
// actor, and the real Worker loop. Nothing is stubbed except the actor's
// business logic, which is the one thing that legitimately belongs to
// somebody else.

const testSecret = "0123456789abcdef0123456789abcdef"

type harness struct {
	t         *testing.T
	ctx       context.Context
	store     *storepg.Store
	ns        storepg.Namespace
	engine    *engine.Engine
	signer    *actors.TokenSigner
	callbacks *storepg.CallbackStore

	callbackServer *httptest.Server
	actorServer    *httptest.Server
	worker         *worker.Worker

	mu       sync.Mutex
	requests []actors.InvocationRequest
	cancels  []actors.CancelRequest
	errs     []error
}

func newHarness(t *testing.T, actorHandler func(h *harness, w http.ResponseWriter, req actors.InvocationRequest)) *harness {
	t.Helper()
	s := pgtest.RequireStore(t, testStore)

	ns := pgtest.MustNamespace(t, s, "worker")
	eng, err := storepg.NewEngine(s, ns.ID, engine.WithRetryDelays(0, 0))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	signer, err := actors.NewTokenSigner([]byte(testSecret))
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}
	callbacks, err := storepg.NewCallbackStore(s, ns.ID)
	if err != nil {
		t.Fatalf("NewCallbackStore: %v", err)
	}

	h := &harness{
		t: t, ctx: context.Background(), store: s, ns: ns,
		engine: eng, signer: signer, callbacks: callbacks,
	}

	// The callback endpoint §13.1 advertises, served by the real ingest
	// handler — so an async actor in these tests reports through exactly the
	// path a deployed actor would.
	h.callbackServer = httptest.NewServer(actors.NewCallbackHandler(actors.CallbackDeps{
		Store:  callbacks,
		Engine: eng,
		Signer: signer,
	}))
	t.Cleanup(h.callbackServer.Close)

	h.actorServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// §13.6 cancellation shares the actor's base URL with §13.1
		// invocation, so the one test server answers both. Cancels are
		// recorded separately: a cancel is not an invocation, and a test
		// counting dispatches must not see one.
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			var cancel actors.CancelRequest
			if err := json.NewDecoder(r.Body).Decode(&cancel); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			h.mu.Lock()
			h.cancels = append(h.cancels, cancel)
			h.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			return
		}

		var req actors.InvocationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		h.requests = append(h.requests, req)
		h.mu.Unlock()
		actorHandler(h, w, req)
	}))
	t.Cleanup(h.actorServer.Close)

	registry := worker.StaticRegistry{
		"actor://company/analyzer":    {URL: h.actorServer.URL},
		"actor://company/long-runner": {URL: h.actorServer.URL},
	}
	wk, err := worker.New(s, eng, worker.Options{
		WorkerID:          "worker-" + t.Name(),
		NamespaceID:       ns.ID,
		ClaimBatch:        4,
		LeaseDuration:     30 * time.Second,
		HeartbeatInterval: 200 * time.Millisecond,
		PollInterval:      20 * time.Millisecond,
		Registry:          registry,
		Signer:            signer,
		CallbackBaseURL:   h.callbackServer.URL,
		OnError: func(err error) {
			h.mu.Lock()
			h.errs = append(h.errs, err)
			h.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	h.worker = wk
	return h
}

func (h *harness) compile(name string) *compiler.CompiledWorkflow {
	h.t.Helper()
	path := filepath.Join("testdata", name)
	source, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatalf("read %s: %v", path, err)
	}
	cw, diags, err := compiler.Compile(source, compiler.FormatForPath(path))
	if err != nil {
		h.t.Fatalf("compile %s: %v", path, err)
	}
	for _, d := range diags {
		if d.Level == compiler.LevelError {
			h.t.Fatalf("compile %s: %s at %s: %s", path, d.Code, d.Path, d.Message)
		}
	}
	return cw
}

func (h *harness) createRun(name, input string) engine.Run {
	h.t.Helper()
	run, err := h.engine.CreateRun(h.ctx, h.compile(name), json.RawMessage(input))
	if err != nil {
		h.t.Fatalf("CreateRun: %v", err)
	}
	return run
}

// runUntil ticks the worker until predicate holds or the deadline passes.
// Driving Tick directly rather than Run keeps the test deterministic about
// how many claim passes happened.
func (h *harness) runUntil(timeout time.Duration, predicate func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		if _, err := h.worker.Tick(h.ctx); err != nil {
			h.t.Fatalf("Tick: %v", err)
		}
		if predicate() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("condition not reached within %s (worker errors: %v)", timeout, h.workerErrors())
}

func (h *harness) workerErrors() []error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]error(nil), h.errs...)
}

func (h *harness) invocations() []actors.InvocationRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]actors.InvocationRequest(nil), h.requests...)
}

// cancellations are the §13.6 cancel requests the actor received.
func (h *harness) cancellations() []actors.CancelRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]actors.CancelRequest(nil), h.cancels...)
}

func (h *harness) run(runID string) engine.Run {
	h.t.Helper()
	run, err := h.engine.Store().Run(h.ctx, runID)
	if err != nil {
		h.t.Fatalf("read run %s: %v", runID, err)
	}
	return run
}

func (h *harness) workItemStates(runID string) map[string]string {
	h.t.Helper()
	rows, err := h.store.Pool().Query(h.ctx, `
		SELECT nr.node_key, wi.state
		FROM work_items AS wi
		JOIN node_runs AS nr ON nr.id = wi.node_run_id
		WHERE nr.run_id = $1
	`, runID)
	if err != nil {
		h.t.Fatalf("read work items: %v", err)
	}
	defer rows.Close()
	states := map[string]string{}
	for rows.Next() {
		var node, state string
		if err := rows.Scan(&node, &state); err != nil {
			h.t.Fatalf("scan work item: %v", err)
		}
		states[node] = state
	}
	return states
}

func writeSyncResult(w http.ResponseWriter, outcome, output string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"outcome":%q,"output":%s,"ledger_delta":{"records":[]}}`, outcome, output)
}

// The whole synchronous path: claim, resolve /run/input, invoke a real HTTP
// actor, complete, transition to a decision node, evaluate it in-process,
// transition to the end node, and produce the run result.
func TestWorkerDrivesASynchronousRunToCompletion(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, req actors.InvocationRequest) {
		// The actor sees what the node's binding resolved to, not the whole
		// run surface.
		if !bytes.Contains(req.Input, []byte(`"widget"`)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		writeSyncResult(w, "completed", `{"score":0.91,"summary":"looks good"}`)
	})

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	final := h.run(run.ID)
	if final.State != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", final.State, h.workerErrors())
	}
	if !bytes.Contains(final.Output, []byte(`0.91`)) {
		t.Errorf("run output = %s, want the analyzer's score", final.Output)
	}

	// The invocation carried the §13.1 envelope, filled in from real state.
	invocations := h.invocations()
	if len(invocations) != 1 {
		t.Fatalf("actor was invoked %d times, want 1 (a decision node is evaluated in-process)", len(invocations))
	}
	inv := invocations[0]
	if inv.ProtocolVersion != actors.ProtocolVersion {
		t.Errorf("protocol_version = %q, want %q", inv.ProtocolVersion, actors.ProtocolVersion)
	}
	if inv.RunID != run.ID {
		t.Errorf("run_id = %q, want %q", inv.RunID, run.ID)
	}
	if inv.Node.ID != "analyze" {
		t.Errorf("node.id = %q, want analyze", inv.Node.ID)
	}
	if inv.Node.ContractDigest == "" {
		t.Error("node.contract_digest is empty; the node declares a contract")
	}
	if inv.Workflow.VersionDigest != run.WorkflowDigest {
		t.Errorf("workflow.version_digest = %q, want the pinned digest %q", inv.Workflow.VersionDigest, run.WorkflowDigest)
	}
	if inv.TokenID == "" || inv.NodeRunID == "" || inv.AttemptID == "" {
		t.Errorf("invocation is missing an identifier: %+v", inv)
	}
	if inv.Deadline == nil {
		t.Error("invocation carried no deadline; the node declares policy.timeout")
	}
	// The callback block is present even for a synchronous dispatch: the
	// actor decides which it is, and cannot ask for the block afterwards.
	if inv.Callback.URL == "" || inv.Callback.Token == "" {
		t.Errorf("callback block = %+v, want a URL and an attempt-scoped token", inv.Callback)
	}
	if got, err := h.signer.Verify(inv.Callback.Token); err != nil || got != inv.AttemptID {
		t.Errorf("callback token names attempt %q (err %v), want %q", got, err, inv.AttemptID)
	}

	// The decision node's recorded output is the payload its ports were
	// evaluated against, and it routed through the `ship` port.
	var routeOutcome string
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT outcome FROM node_runs WHERE run_id = $1 AND node_key = 'route'`, run.ID).Scan(&routeOutcome); err != nil {
		t.Fatalf("read decision node run: %v", err)
	}
	if routeOutcome != "ship" {
		t.Errorf("decision outcome = %q, want ship (score 0.91 >= 0.8)", routeOutcome)
	}

	for node, state := range h.workItemStates(run.ID) {
		if state != "completed" {
			t.Errorf("work item for node %q is %q, want completed", node, state)
		}
	}
}

// The synchronous completion path (completeFromResult) persists whatever
// §13.2 Usage block the actor's InvocationResult carried, on the attempt row
// it commits — task t1's sync-path half of the completion seam. Before t1
// this block was decoded off the wire (internal/actors/client.go) and then
// silently dropped: no non-test code consumed it.
func TestWorkerPersistsSynchronousUsage(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"outcome": "completed",
			"output": {"score": 0.5, "summary": "ok"},
			"ledger_delta": {"records": []},
			"usage": {"input_tokens": 120, "output_tokens": 340, "cost": 0.0021, "currency": "USD"}
		}`)
	})

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", state, h.workerErrors())
	}

	var (
		inputTokens, outputTokens int64
		cost                      float64
		currency                  string
	)
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT a.usage_input_tokens, a.usage_output_tokens, a.usage_cost, a.usage_currency
		FROM attempts AS a JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = 'analyze'
	`, run.ID).Scan(&inputTokens, &outputTokens, &cost, &currency); err != nil {
		t.Fatalf("read attempt usage: %v", err)
	}
	if inputTokens != 120 || outputTokens != 340 {
		t.Errorf("tokens = %d/%d, want 120/340", inputTokens, outputTokens)
	}
	if cost != 0.0021 {
		t.Errorf("cost = %v, want 0.0021", cost)
	}
	if currency != "USD" {
		t.Errorf("currency = %q, want USD", currency)
	}
}

// A synchronous result that carries no Usage block leaves the attempt's
// usage columns NULL — the plain writeSyncResult fixture every other
// synchronous test already uses, made an explicit assertion here rather
// than an incidental one (no fabricated zero, task t1 acceptance).
func TestWorkerWithoutUsageLeavesAttemptUsageNull(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, req actors.InvocationRequest) {
		writeSyncResult(w, "completed", `{"score":0.91,"summary":"looks good"}`)
	})

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	var (
		inputTokens, outputTokens, cost, currency any
	)
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT a.usage_input_tokens, a.usage_output_tokens, a.usage_cost, a.usage_currency
		FROM attempts AS a JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = 'analyze'
	`, run.ID).Scan(&inputTokens, &outputTokens, &cost, &currency); err != nil {
		t.Fatalf("read attempt usage: %v", err)
	}
	if inputTokens != nil || outputTokens != nil || cost != nil || currency != nil {
		t.Errorf("usage columns = (%v, %v, %v, %v), want all NULL", inputTokens, outputTokens, cost, currency)
	}
}

// §12.6: an asynchronous acceptance releases worker capacity. The work item
// must be parked — not leased — while the actor works, and the run must
// complete from the callback alone.
func TestWorkerParksAsyncInvocationAndCompletesFromCallback(t *testing.T) {
	var (
		mu       sync.Mutex
		captured actors.InvocationRequest
		accepted = make(chan struct{})
	)

	h := newHarness(t, func(_ *harness, w http.ResponseWriter, req actors.InvocationRequest) {
		mu.Lock()
		captured = req
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"invocation_id":"external_slow","heartbeat_after_seconds":30,"supports_cancellation":true}`))
		close(accepted)
	})

	run := h.createRun("async.workflow.yaml", `{"subject":"slow"}`)

	if _, err := h.worker.Tick(h.ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	select {
	case <-accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("the actor was never invoked")
	}

	// The worker holds nothing: the item is parked, not leased, and has no
	// lease owner to expire.
	var (
		state      string
		leaseOwner *string
	)
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT wi.state, wi.lease_owner
		FROM work_items AS wi JOIN node_runs AS nr ON nr.id = wi.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = 'work'
	`, run.ID).Scan(&state, &leaseOwner); err != nil {
		t.Fatalf("read work item: %v", err)
	}
	if state != storepg.WaitingWorkState {
		t.Fatalf("work item state = %q, want %q: an async invocation must release worker capacity",
			state, storepg.WaitingWorkState)
	}
	if leaseOwner != nil {
		t.Errorf("work item lease_owner = %q, want NULL", *leaseOwner)
	}

	// Further ticks find nothing — a parked item is invisible to ClaimWork —
	// which is what "the worker holds no goroutine for it" means in practice.
	for i := 0; i < 3; i++ {
		dispatched, err := h.worker.Tick(h.ctx)
		if err != nil {
			t.Fatalf("Tick: %v", err)
		}
		if dispatched != 0 {
			t.Fatalf("tick %d claimed %d items, want 0 while the invocation is parked", i, dispatched)
		}
	}
	if h.run(run.ID).State != engine.RunRunning {
		t.Fatalf("run state = %s, want still running while the actor works", h.run(run.ID).State)
	}

	// Now the actor reports, through the real §13.1 callback URL.
	mu.Lock()
	callback := captured.Callback
	mu.Unlock()

	postEvent := func(ev actors.CallbackEvent) string {
		t.Helper()
		body, _ := json.Marshal(ev)
		req, err := http.NewRequest(http.MethodPost, callback.URL, bytes.NewReader(body))
		if err != nil {
			t.Fatalf("build callback request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+callback.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST callback: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("callback %s answered %d, want 202", ev.Kind, resp.StatusCode)
		}
		var decoded struct {
			Disposition string `json:"disposition"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&decoded)
		return decoded.Disposition
	}

	postEvent(actors.CallbackEvent{EventID: "ev-1", Sequence: 1, Kind: actors.EventAccepted})
	postEvent(actors.CallbackEvent{EventID: "ev-2", Sequence: 2, Kind: actors.EventHeartbeat})

	completedPayload, _ := json.Marshal(actors.CompletedPayload{
		Outcome: "completed",
		Output:  json.RawMessage(`{"summary":"finished eventually"}`),
	})
	if got := postEvent(actors.CallbackEvent{
		EventID: "ev-3", Sequence: 3, Kind: actors.EventCompleted, Payload: completedPayload,
	}); got != string(actors.DispositionCommitted) {
		t.Fatalf("terminal callback disposition = %q, want committed", got)
	}

	final := h.run(run.ID)
	if final.State != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", final.State, h.workerErrors())
	}
	if !bytes.Contains(final.Output, []byte("finished eventually")) {
		t.Errorf("run output = %s, want the actor's callback payload", final.Output)
	}
}

// An actor that refuses the input is a §13.5 classification, recorded as a
// technical status with the class preserved — never as a domain outcome.
func TestWorkerRecordsClassifiedInvocationFailures(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"workload token rejected"}`))
	})

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunFailed {
		t.Fatalf("run state = %s, want failed", state)
	}

	var (
		status string
		result []byte
	)
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT a.status, a.result
		FROM attempts AS a JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = 'analyze'
	`, run.ID).Scan(&status, &result); err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	if engine.TechStatus(status) != engine.StatusPolicyDenied {
		t.Errorf("attempt status = %q, want policy_denied for a 401 (PRD §13.5)", status)
	}
	if !bytes.Contains(result, []byte(string(actors.ClassAuthOrPolicy))) {
		t.Errorf("attempt result = %s, want the §13.5 class recorded", result)
	}
}

// Issue #32, task t5: a synchronous bridge failure whose 500 error body
// carries the §13.2 usage block — a failed session that still produced a
// parseable terminal result, the bridges' `{error, class,
// workspace_measured, usage}` sync_response failure shape from task t4 —
// persists that usage on the failed attempt. A failed session burned real
// tokens, and retries compound that burn, so the rollups must see it.
func TestWorkerPersistsSyncFailureUsage(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `{
			"error": "session ended with an error result",
			"class": "execution",
			"workspace_measured": {"measured": false},
			"usage": {"input_tokens": 850, "output_tokens": 60, "cost": 0.017, "currency": "USD"}
		}`)
	})

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunFailed {
		t.Fatalf("run state = %s, want failed (worker errors: %v)", state, h.workerErrors())
	}

	var (
		status                    string
		result                    []byte
		inputTokens, outputTokens int64
		cost                      float64
		currency                  string
	)
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT a.status, a.result, a.usage_input_tokens, a.usage_output_tokens, a.usage_cost, a.usage_currency
		FROM attempts AS a JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = 'analyze'
	`, run.ID).Scan(&status, &result, &inputTokens, &outputTokens, &cost, &currency); err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	if engine.TechStatus(status) != engine.StatusFailed {
		t.Errorf("attempt status = %q, want failed for a 500 (PRD §13.5 execution)", status)
	}
	if !bytes.Contains(result, []byte(string(actors.ClassExecution))) {
		t.Errorf("attempt result = %s, want the §13.5 class recorded", result)
	}
	if inputTokens != 850 || outputTokens != 60 {
		t.Errorf("tokens = %d/%d, want 850/60 from the 500 error body", inputTokens, outputTokens)
	}
	if cost != 0.017 {
		t.Errorf("cost = %v, want 0.017", cost)
	}
	if currency != "USD" {
		t.Errorf("currency = %q, want USD", currency)
	}
}

// The honest-null rule on the same path: a 500 error body without a usage
// block — a result-less crash — leaves every usage column NULL, never a
// fabricated zero (the h24 narrowing, stated in migrations/README.md's 0012
// entry).
func TestWorkerSyncFailureWithoutUsageLeavesAttemptUsageNull(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `{"error":"session crashed before any result","class":"execution","workspace_measured":{"measured":false}}`)
	})

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunFailed {
		t.Fatalf("run state = %s, want failed (worker errors: %v)", state, h.workerErrors())
	}

	var inputTokens, outputTokens, cost, currency any
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT a.usage_input_tokens, a.usage_output_tokens, a.usage_cost, a.usage_currency
		FROM attempts AS a JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = 'analyze'
	`, run.ID).Scan(&inputTokens, &outputTokens, &cost, &currency); err != nil {
		t.Fatalf("read attempt usage: %v", err)
	}
	if inputTokens != nil || outputTokens != nil || cost != nil || currency != nil {
		t.Errorf("usage columns = (%v, %v, %v, %v), want all NULL", inputTokens, outputTokens, cost, currency)
	}
}

// An unresolvable input binding refuses to dispatch: an actor must never be
// handed data the definition did not ask for.
func TestWorkerRefusesToDispatchAnUnresolvableBinding(t *testing.T) {
	var invoked int
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		invoked++
		writeSyncResult(w, "completed", `{"summary":"x"}`)
	})

	// The `work` node binds /run/input, and this run's input has no member
	// the binding needs — but the binding itself points deeper than the
	// document goes, which is the failure under test.
	cw := h.compile("async.workflow.yaml")
	run, err := h.engine.CreateRun(h.ctx, cw, json.RawMessage(`{"subject":"present"}`))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// Rewrite the pinned IR's binding to one that cannot resolve. This is the
	// only way to reach the branch without publishing a definition the
	// compiler would reject, and it is honest about what it is doing: the run
	// still executes whatever IR its pinned version carries.
	if _, err := h.store.Pool().Exec(h.ctx, `
		UPDATE workflow_versions
		SET normalized_ir = jsonb_set(normalized_ir, '{spec,nodes,work,input,from}', '"/nodes/absent/output"')
		WHERE id = (SELECT workflow_version_id FROM runs WHERE id = $1)
	`, run.ID); err != nil {
		t.Fatalf("rewrite pinned IR: %v", err)
	}

	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if invoked != 0 {
		t.Errorf("the actor was invoked %d times; an unresolvable binding must not dispatch", invoked)
	}
	var status string
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT a.status FROM attempts AS a JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = 'work'
	`, run.ID).Scan(&status); err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	if engine.TechStatus(status) != engine.StatusContractRejected {
		t.Errorf("attempt status = %q, want contract_rejected", status)
	}
}

// A kind whose seam is not registered fails with a diagnostic naming the
// missing capability. It must never quietly succeed — a node that
// auto-succeeded because its executor was unregistered would be the worst
// possible failure mode for a system whose premise is that claims are
// earned. This exercises the generic path via the `approval` kind (renaming
// an already-dispatched node's pinned kind — see
// TestApprovalWorkItemThatSomehowExistsIsRefusedNotProcessed in
// approval_invariant_test.go for the same refusal proven against a real
// approval node's own node run). For `code` and `wait` this diagnostic is a
// temporary gap later tasks close; for `approval` it is the permanent,
// correct behaviour, because no approval work item legitimately reaches the
// worker at all (see seams.go's HumanDispatcher doc comment).
func TestUnwiredKindsFailWithADiagnosticRatherThanSucceeding(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		writeSyncResult(w, "completed", `{"summary":"x"}`)
	})

	cw := h.compile("async.workflow.yaml")
	run, err := h.engine.CreateRun(h.ctx, cw, json.RawMessage(`{"subject":"present"}`))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := h.store.Pool().Exec(h.ctx, `
		UPDATE workflow_versions
		SET normalized_ir = jsonb_set(normalized_ir, '{spec,nodes,work,kind}', '"approval"')
		WHERE id = (SELECT workflow_version_id FROM runs WHERE id = $1)
	`, run.ID); err != nil {
		t.Fatalf("rewrite pinned IR: %v", err)
	}

	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunFailed {
		t.Fatalf("run state = %s, want failed", state)
	}
	var result []byte
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT a.result FROM attempts AS a JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = 'work'
	`, run.ID).Scan(&result); err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	if !bytes.Contains(result, []byte("not_implemented")) {
		t.Errorf("attempt result = %s, want a not_implemented diagnostic", result)
	}
	if !bytes.Contains(result, []byte("human-task")) {
		t.Errorf("attempt result = %s, want the missing capability named", result)
	}
}

// A registered seam is still used instead of the diagnostic when one is
// configured, proving DispatchApproval is a working mechanism rather than
// dead code — even though no real deployment should ever register one: an
// approval node never produces a work item in the first place (see
// seams.go's HumanDispatcher doc comment), so this test manufactures one, the
// same way TestUnwiredKindsFailWithADiagnosticRatherThanSucceeding does.
func TestRegisteredSeamIsUsed(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		writeSyncResult(w, "completed", `{"summary":"x"}`)
	})

	cw := h.compile("async.workflow.yaml")
	run, err := h.engine.CreateRun(h.ctx, cw, json.RawMessage(`{"subject":"present"}`))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := h.store.Pool().Exec(h.ctx, `
		UPDATE workflow_versions
		SET normalized_ir = jsonb_set(normalized_ir, '{spec,nodes,work,kind}', '"approval"')
		WHERE id = (SELECT workflow_version_id FROM runs WHERE id = $1)
	`, run.ID); err != nil {
		t.Fatalf("rewrite pinned IR: %v", err)
	}

	seam := &recordingHuman{}
	wk, err := worker.New(h.store, h.engine, worker.Options{
		WorkerID:      "worker-seam-" + t.Name(),
		NamespaceID:   h.ns.ID,
		LeaseDuration: 30 * time.Second,
		Human:         seam,
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && !h.run(run.ID).State.Terminal() {
		if _, err := wk.Tick(h.ctx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !seam.called {
		t.Fatal("the registered human dispatcher was never called")
	}
	if seam.approverRef != "" {
		t.Logf("approverRef passed through as %q", seam.approverRef)
	}
	if state := h.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed via the seam", state)
	}
}

type recordingHuman struct {
	called      bool
	approverRef string
}

func (r *recordingHuman) DispatchApproval(_ context.Context, dc worker.DispatchContext, approverRef string, _ time.Duration) (worker.SeamResult, error) {
	r.called = true
	r.approverRef = approverRef
	if dc.AttemptID == "" {
		return worker.SeamResult{}, fmt.Errorf("seam received no attempt id")
	}
	return worker.SeamResult{
		TechStatus: engine.StatusSucceeded,
		Outcome:    "completed",
		Output:     json.RawMessage(`{"summary":"approved by seam"}`),
	}, nil
}

var _ worker.HumanDispatcher = (*recordingHuman)(nil)

// A synchronous §13.2 result carrying a workspace_measured block (issue
// #33a) lands inside the node's persisted output, where downstream nodes
// receive it: in this workflow the decision node binds
// /nodes/analyze/output (and still routes on the agent's own score), and
// the end node's identical binding makes it the run result. Authority stays
// actor-reported — the landing path writes no observed evidence.
func TestWorkerFoldsSyncWorkspaceMeasuredIntoNodeOutput(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"outcome": "completed",
			"output": {"score": 0.91, "summary": "looks good"},
			"ledger_delta": {"records": []},
			"workspace_measured": {
				"measured": true, "repo": "/work/repo", "reason": null, "branch": "main",
				"head_before": "aaa111", "head_after": "bbb222", "status_porcelain": " M x.go",
				"changed_files": ["x.go"], "diffstat": " x.go | 2 +-"
			}
		}`)
	})

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	final := h.run(run.ID)
	if final.State != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", final.State, h.workerErrors())
	}

	// The node's persisted output — the same NodeOutput statement a
	// /nodes/<id>/output binding resolves with — carries the block.
	nodeOutput, err := h.store.NodeOutput(h.ctx, run.ID, "analyze")
	if err != nil {
		t.Fatalf("read node output: %v", err)
	}
	var folded struct {
		Score             float64 `json:"score"`
		WorkspaceMeasured *struct {
			Measured  bool     `json:"measured"`
			HeadAfter string   `json:"head_after"`
			Changed   []string `json:"changed_files"`
			Diffstat  string   `json:"diffstat"`
		} `json:"workspace_measured"`
	}
	if err := json.Unmarshal(nodeOutput, &folded); err != nil {
		t.Fatalf("node output is not an object: %v\noutput: %s", err, nodeOutput)
	}
	if folded.Score != 0.91 {
		t.Errorf("the actor's own output was disturbed: %s", nodeOutput)
	}
	if folded.WorkspaceMeasured == nil {
		t.Fatalf("node output carries no workspace_measured block: %s", nodeOutput)
	}
	if !folded.WorkspaceMeasured.Measured || folded.WorkspaceMeasured.HeadAfter != "bbb222" ||
		len(folded.WorkspaceMeasured.Changed) != 1 || folded.WorkspaceMeasured.Diffstat == "" {
		t.Errorf("workspace_measured lost facts in transit: %s", nodeOutput)
	}

	// Downstream: the decision node bound /nodes/analyze/output and still
	// routed on the agent's score, and the end node's identical binding put
	// the block in the run result.
	var routeOutcome string
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT outcome FROM node_runs WHERE run_id = $1 AND node_key = 'route'`, run.ID).Scan(&routeOutcome); err != nil {
		t.Fatalf("read decision node run: %v", err)
	}
	if routeOutcome != "ship" {
		t.Errorf("decision outcome = %q, want ship: the merge must not disturb what downstream nodes route on", routeOutcome)
	}
	if !bytes.Contains(final.Output, []byte(`"workspace_measured"`)) || !bytes.Contains(final.Output, []byte(`"bbb222"`)) {
		t.Errorf("run output (bound from /nodes/analyze/output) = %s, want the workspace_measured block", final.Output)
	}

	// No observed-authority ledger records were written from the block:
	// actor-reported data is a claim, not evidence (§10.4).
	var observed int
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT count(*) FROM ledger_records WHERE run_id = $1 AND authority = 'observed'`,
		run.ID).Scan(&observed); err != nil {
		t.Fatalf("count observed records: %v", err)
	}
	if observed != 0 {
		t.Errorf("observed-authority ledger records = %d, want 0", observed)
	}
}

// The unmeasured shape — measured:false, every fact null — round-trips
// through the synchronous path exactly as the bridge sent it, and a result
// with no block at all leaves the output without the key: absent and
// unmeasured are different facts and neither may impersonate the other.
func TestWorkerSyncUnmeasuredBlockRoundTripsAndAbsentStaysAbsent(t *testing.T) {
	const unmeasured = `{"measured":false,"repo":null,"reason":"no repo configured","branch":null,` +
		`"head_before":null,"head_after":null,"status_porcelain":null,"changed_files":[],"diffstat":null}`

	var sendBlock atomic.Bool
	sendBlock.Store(true)
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		w.Header().Set("Content-Type", "application/json")
		if sendBlock.Load() {
			_, _ = fmt.Fprintf(w, `{"outcome":"completed","output":{"score":0.91},"ledger_delta":{"records":[]},"workspace_measured":%s}`, unmeasured)
			return
		}
		writeSyncResult(w, "completed", `{"score":0.91}`)
	})

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })
	if state := h.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", state, h.workerErrors())
	}

	nodeOutput, err := h.store.NodeOutput(h.ctx, run.ID, "analyze")
	if err != nil {
		t.Fatalf("read node output: %v", err)
	}
	var folded map[string]json.RawMessage
	if err := json.Unmarshal(nodeOutput, &folded); err != nil {
		t.Fatalf("node output is not an object: %v", err)
	}
	block, ok := folded["workspace_measured"]
	if !ok {
		t.Fatalf("the unmeasured block was dropped: %s", nodeOutput)
	}
	var sent, got any
	if err := json.Unmarshal([]byte(unmeasured), &sent); err != nil {
		t.Fatalf("fixture block: %v", err)
	}
	if err := json.Unmarshal(block, &got); err != nil {
		t.Fatalf("persisted block: %v", err)
	}
	if !reflect.DeepEqual(sent, got) {
		t.Errorf("the unmeasured block was altered in transit:\n sent: %s\n got: %s", unmeasured, block)
	}

	// Second run, no block on the wire: the key must not appear at all.
	sendBlock.Store(false)
	runAbsent := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(runAbsent.ID).State.Terminal() })
	if state := h.run(runAbsent.ID).State; state != engine.RunCompleted {
		t.Fatalf("absent-block run state = %s, want completed (worker errors: %v)", state, h.workerErrors())
	}
	absentOutput, err := h.store.NodeOutput(h.ctx, runAbsent.ID, "analyze")
	if err != nil {
		t.Fatalf("read node output: %v", err)
	}
	if bytes.Contains(absentOutput, []byte(`"workspace_measured"`)) {
		t.Errorf("a result with no block grew one: %s", absentOutput)
	}
}
