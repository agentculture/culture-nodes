package worker_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/runners"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// Asynchronous runner dispatch (task t9): a `code` node whose registry entry
// is a runner SERVICE is dispatched over api/runner-protocol, parks as
// waiting_external, and is completed from an authenticated status sample.
//
// Everything below runs against a real PostgreSQL, the real engine, the real
// Worker loop, and a real HTTP runner service. The service's business logic is
// scripted — the one part that genuinely belongs to somebody else's process —
// but it speaks the actual protocol, including refusing an unauthenticated
// request, so nothing here proves the wiring against a mock of itself.

const (
	runnerServiceSecretRef = "runner/test/execute-token"
	runnerServiceSecret    = "test-execute-token-not-the-ref"
	// codeWorkflowRunnerRef is testdata/code.workflow.yaml's `uses` value: the
	// name a code node's placement is registered under.
	codeWorkflowRunnerRef = "runner://headspace/docker@sha256:5555555555555555555555555555555555555555555555555555555555555555"
	codeWorkflowDigest    = "sha256:57cd7c3a7a273101a6485ba99423ee568157882804b1124b4dd04266317710de"
)

// fakeRunnerService is an HTTP runner service that speaks
// api/runner-protocol. It answers 202 to an execute, holds the operation's
// status in memory, and turns terminal only when the test says so — which is
// what makes "the outcome is learned by sampling, later" a thing this test can
// actually observe rather than assume.
type fakeRunnerService struct {
	mu sync.Mutex

	// dispatched records every operation submitted, keyed by operation id.
	dispatched map[string]runners.Operation
	// callbacks records the callback offer each dispatch carried.
	callbacks map[string][2]string
	// terminal, once set for an operation id, is the result its status
	// reports from then on.
	terminal map[string]*runners.Result

	dispatchCount int
	statusCount   int
	// inFlight and maxInFlight gauge concurrent requests, so a test can prove
	// the runtime never holds more than one connection per sample.
	inFlight, maxInFlight int
	unauthenticated       int

	// pollAfterSeconds is what the acceptance asks for.
	pollAfterSeconds int
	// dispatchStatus overrides the execute response status when non-zero.
	dispatchStatus int
}

func newFakeRunnerService() *fakeRunnerService {
	return &fakeRunnerService{
		dispatched: map[string]runners.Operation{},
		callbacks:  map[string][2]string{},
		terminal:   map[string]*runners.Result{},
	}
}

func (f *fakeRunnerService) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.enter()
		defer f.leave()

		// Caller authentication is mandatory on every request, including
		// status reads. A runner that serves an unauthenticated one executes
		// code for anyone who can reach it.
		if r.Header.Get("Authorization") != "Bearer "+runnerServiceSecret {
			f.mu.Lock()
			f.unauthenticated++
			f.mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		switch {
		case r.Method == http.MethodPost && r.URL.Path == runners.OperationsPath:
			f.execute(w, r)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, runners.OperationsPath+"/"):
			f.status(w, strings.TrimPrefix(r.URL.Path, runners.OperationsPath+"/"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (f *fakeRunnerService) enter() {
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	f.mu.Unlock()
}

func (f *fakeRunnerService) leave() {
	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()
}

func (f *fakeRunnerService) execute(w http.ResponseWriter, r *http.Request) {
	var op runners.Operation
	if err := json.NewDecoder(r.Body).Decode(&op); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	f.dispatchCount++
	f.dispatched[op.OperationID] = op
	f.callbacks[op.OperationID] = [2]string{
		r.Header.Get(runners.CallbackURLHeader), r.Header.Get(runners.CallbackTokenHeader),
	}
	pollAfter := f.pollAfterSeconds
	status := f.dispatchStatus
	f.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(runners.Acceptance{
		OperationID:            op.OperationID,
		PollAfterSeconds:       pollAfter,
		StatusRetentionSeconds: 86400,
		SupportsCallback:       true,
	})
}

func (f *fakeRunnerService) status(w http.ResponseWriter, operationID string) {
	f.mu.Lock()
	f.statusCount++
	_, known := f.dispatched[operationID]
	result := f.terminal[operationID]
	f.mu.Unlock()

	if !known {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	envelope := runners.OperationStatus{OperationID: operationID, State: runners.StateRunning}
	if result != nil {
		envelope.State = result.State
		envelope.Result = result
	}
	_ = json.NewEncoder(w).Encode(envelope)
}

// finish makes every dispatched operation report a terminal result.
func (f *fakeRunnerService) finish(exitCode int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, op := range f.dispatched {
		result := codeRunResult(op, exitCode)
		f.terminal[id] = &result
	}
}

func (f *fakeRunnerService) counts() (dispatches, statuses, maxInFlight, unauthenticated int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dispatchCount, f.statusCount, f.maxInFlight, f.unauthenticated
}

func (f *fakeRunnerService) operationIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.dispatched))
	for id := range f.dispatched {
		ids = append(ids, id)
	}
	return ids
}

func (f *fakeRunnerService) callbackFor(operationID string) (url, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pair := f.callbacks[operationID]
	return pair[0], pair[1]
}

// runnerServiceHarness is worker_test.go's harness with the async
// runner-protocol path wired in: a registered ServiceIdentity, a protocol
// client over a fake secret source, and no in-process CodeRunner at all — a
// deployment that dispatches its code to a runner service configures exactly
// this and nothing else.
type runnerServiceHarness struct {
	*harness
	service       *fakeRunnerService
	serviceServer *httptest.Server
	registry      *runners.FunctionRegistry
	worker        *worker.Worker
	sampleWorker  *worker.Worker
	hooks         *runnerHooks
	actorID       string
}

// runnerHooks holds the test-only injection point the racing-commit test
// needs. It mirrors internal/scheduler's Hooks: a failure/interleaving point
// that would otherwise require killing a real process at an exact instant.
type runnerHooks struct {
	mu     sync.Mutex
	before func(attemptID string)
}

func (h *runnerHooks) beforeCommit(attemptID string) {
	h.mu.Lock()
	fn := h.before
	h.mu.Unlock()
	if fn != nil {
		fn(attemptID)
	}
}

func (h *runnerHooks) set(fn func(attemptID string)) {
	h.mu.Lock()
	h.before = fn
	h.mu.Unlock()
}

func newRunnerServiceHarness(t *testing.T) *runnerServiceHarness {
	t.Helper()
	base := newHarness(t, refusingActor)

	service := newFakeRunnerService()
	server := httptest.NewServer(service.handler())
	t.Cleanup(server.Close)

	registry := runners.NewFunctionRegistry()
	if err := registry.RegisterService(codeWorkflowRunnerRef, runners.ServiceIdentity{
		Endpoint:               server.URL,
		ImageDigest:            codeWorkflowDigest,
		SecretRef:              runnerServiceSecretRef,
		AllowInsecureTransport: true, // httptest is plaintext http to 127.0.0.1
	}); err != nil {
		t.Fatalf("RegisterService: %v", err)
	}

	// The runner's evidence is attributed to a registered actor identity
	// (ledger_records.origin_actor_id is a real foreign key), so that actor
	// has to exist before a completion can append it — the same obligation a
	// real deployment has.
	h := &runnerServiceHarness{
		harness: base, service: service, serviceServer: server, registry: registry,
		hooks: &runnerHooks{}, actorID: mustHookRunnerActor(t, base.store, base.ns.ID),
	}
	h.worker = h.newWorker(t, "runner-worker-a")
	h.sampleWorker = h.newWorker(t, "runner-worker-b")
	return h
}

func (h *runnerServiceHarness) newWorker(t *testing.T, id string) *worker.Worker {
	t.Helper()
	client, err := runners.NewProtocolClient(runners.StaticSecrets{runnerServiceSecretRef: runnerServiceSecret})
	if err != nil {
		t.Fatalf("NewProtocolClient: %v", err)
	}
	wk, err := worker.New(h.store, h.engine, worker.Options{
		WorkerID:           id + "-" + t.Name(),
		NamespaceID:        h.ns.ID,
		ClaimBatch:         4,
		LeaseDuration:      30 * time.Second,
		HeartbeatInterval:  200 * time.Millisecond,
		PollInterval:       20 * time.Millisecond,
		Signer:             h.signer,
		CallbackBaseURL:    h.callbackServer.URL,
		CodeRunnerName:     fakeCodeRunnerName,
		CodeRunnerActorID:  h.actorID,
		CodeRunnerRevision: fakeCodeRunnerRevision,
		RunnerService: worker.RunnerServiceOptions{
			Registry:     h.registry,
			Client:       client,
			PollInterval: 10 * time.Millisecond,
			SampleBatch:  8,
			Hooks:        worker.RunnerHooks{BeforeCommit: h.hooks.beforeCommit},
		},
		OnError: func(err error) {
			h.mu.Lock()
			h.errs = append(h.errs, err)
			h.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	return wk
}

func (h *runnerServiceHarness) runnerOperations() []storepg.RunnerOperation {
	h.t.Helper()
	rows, err := h.store.Pool().Query(h.ctx,
		`SELECT attempt_id FROM runner_invocations WHERE namespace_id = $1 ORDER BY created_at`, h.ns.ID)
	if err != nil {
		h.t.Fatalf("list runner operations: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			h.t.Fatalf("scan runner operation: %v", err)
		}
		ids = append(ids, id)
	}
	var ops []storepg.RunnerOperation
	for _, id := range ids {
		op, err := h.store.RunnerOperation(h.ctx, h.ns.ID, id)
		if err != nil {
			h.t.Fatalf("RunnerOperation %s: %v", id, err)
		}
		ops = append(ops, op)
	}
	return ops
}

func (h *runnerServiceHarness) attemptCount(runID string) int {
	h.t.Helper()
	var n int
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT count(*) FROM attempts AS a
		JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1`, runID).Scan(&n); err != nil {
		h.t.Fatalf("count attempts: %v", err)
	}
	return n
}

// sampleUntil drives the sampler (not the claim loop) until predicate holds.
func (h *runnerServiceHarness) sampleUntil(wk *worker.Worker, timeout time.Duration, predicate func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		if _, err := wk.SampleRunnerOperations(h.ctx); err != nil {
			h.t.Fatalf("SampleRunnerOperations: %v", err)
		}
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("condition not reached within %s (worker errors: %v)", timeout, h.workerErrors())
}

// --- acceptance criterion 1 ------------------------------------------------

// Runner-protocol dispatch parks the work item as waiting_external, holding no
// lease and no goroutine between status samples.
//
// The three assertions that carry the criterion:
//
//  1. after the dispatch tick the work item is 'waiting' with no lease owner
//     and no expiry — invisible to ClaimWork and to ReclaimExpired, so no
//     worker anywhere holds it;
//  2. the process's goroutine count returns to its pre-dispatch baseline and
//     stays there across every sample — there is no per-operation goroutine,
//     which is the whole reason sampling load scales with runners × interval
//     rather than with how long an operation runs;
//  3. the runner service never sees more than one concurrent request, and sees
//     no status request at all until a sampler asks — the dispatch connection
//     closed at the 202 and nothing was held open waiting for the answer.
func TestRunnerServiceDispatchParksHoldingNoLeaseAndNoGoroutine(t *testing.T) {
	h := newRunnerServiceHarness(t)

	run := h.createRun("code.workflow.yaml", `{}`)
	if _, err := h.worker.Tick(h.ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// 1. parked, unleased, invisible to reclaim.
	var state string
	var leaseOwner, leaseExpires any
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT wi.state, wi.lease_owner, wi.lease_expires_at
		FROM work_items AS wi JOIN node_runs AS nr ON nr.id = wi.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = 'test'`, run.ID).Scan(&state, &leaseOwner, &leaseExpires); err != nil {
		t.Fatalf("read parked work item: %v", err)
	}
	if state != storepg.WaitingWorkState {
		t.Fatalf("work item state = %q, want %q (worker errors: %v)", state, storepg.WaitingWorkState, h.workerErrors())
	}
	if leaseOwner != nil || leaseExpires != nil {
		t.Fatalf("parked work item still holds a lease (owner=%v expires=%v)", leaseOwner, leaseExpires)
	}
	if got := h.workItemStates(run.ID)["test"]; got != storepg.WaitingWorkState {
		t.Fatalf("work item state = %q, want %q", got, storepg.WaitingWorkState)
	}

	ops := h.runnerOperations()
	if len(ops) != 1 {
		t.Fatalf("runner_invocations rows = %d, want 1", len(ops))
	}
	if ops[0].State != storepg.RunnerOperationWaiting || ops[0].DeadlineTimerID == "" {
		t.Fatalf("parked operation = %+v, want waiting_external with a deadline timer", ops[0])
	}

	// 3a. the dispatch learned nothing about the outcome. The proof is the
	// state above, not a status-request count: the operation is still running
	// on the runner, yet the work item is already parked with no lease — a
	// dispatch that waited for the outcome could not leave that state behind.
	// (Tick's own trailing sampler pass MAY legitimately read a status the
	// instant the operation becomes due, so "zero status requests so far" is
	// a clock race, not an invariant — it flaked exactly that way in CI.)
	dispatches, _, _, unauthenticated := h.service.counts()
	if dispatches != 1 {
		t.Fatalf("execute requests = %d, want 1", dispatches)
	}
	if unauthenticated != 0 {
		t.Fatalf("the runner refused %d unauthenticated requests; every protocol request must carry the bearer", unauthenticated)
	}

	// 2. no goroutine survives the park, and no goroutine is added by having
	// MORE parked operations.
	//
	// The baseline is taken after the first operation has parked and settled,
	// because that is what isolates the claim being made. A process's absolute
	// goroutine count includes the runtime's own workers, a pgx pool's health
	// checker and the harness's HTTP servers — none of which this test has
	// anything to say about. What it does say is that going from one parked
	// operation to four, and sampling each of them repeatedly, costs nothing:
	// a per-operation goroutine would show up as growth here immediately.
	settleGoroutines(t, runtime.NumGoroutine(), 3*time.Second)
	baseline := runtime.NumGoroutine()

	const extraRuns = 3
	for i := 0; i < extraRuns; i++ {
		h.createRun("code.workflow.yaml", `{}`)
	}
	for i := 0; i < extraRuns; i++ {
		if _, err := h.worker.Tick(h.ctx); err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
	}
	if got := len(h.runnerOperations()); got != 1+extraRuns {
		t.Fatalf("parked operations = %d, want %d (worker errors: %v)", got, 1+extraRuns, h.workerErrors())
	}
	settleGoroutines(t, baseline+2, 3*time.Second)

	// Sample repeatedly while every operation is still running: neither the
	// goroutine count nor the connection count may grow with the number of
	// samples or with the number of parked operations.
	for i := 0; i < 5; i++ {
		if _, err := h.worker.SampleRunnerOperations(h.ctx); err != nil {
			t.Fatalf("SampleRunnerOperations: %v", err)
		}
		time.Sleep(15 * time.Millisecond)
		if got := runtime.NumGoroutine(); got > baseline+2 {
			t.Fatalf("goroutines = %d after sample round %d over %d parked operations, want no more than %d: "+
				"a parked operation must own no goroutine", got, i+1, 1+extraRuns, baseline+2)
		}
	}
	if _, statuses, maxInFlight, _ := h.service.counts(); statuses == 0 || maxInFlight > 1 {
		t.Fatalf("samples = %d with max %d concurrent requests, want repeated samples one connection at a time",
			statuses, maxInFlight)
	}

	// The work item stayed parked the whole time: sampling is not claiming.
	if got := h.workItemStates(run.ID)["test"]; got != storepg.WaitingWorkState {
		t.Fatalf("work item state after sampling = %q, want it still parked", got)
	}
}

// The completion is learned only from an authenticated status sample, and
// committing it routes the node's domain outcome exactly as the in-process
// runner path does.
func TestRunnerServiceCompletionIsCommittedFromAStatusSample(t *testing.T) {
	h := newRunnerServiceHarness(t)

	run := h.createRun("code.workflow.yaml", `{}`)
	if _, err := h.worker.Tick(h.ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if h.attemptCount(run.ID) != 0 {
		t.Fatal("an attempt was committed at dispatch time; the outcome is not knowable yet")
	}

	h.service.finish(0)
	h.sampleUntil(h.worker, 10*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	final := h.run(run.ID)
	if final.State != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", final.State, h.workerErrors())
	}
	if !strings.Contains(string(final.Output), `"completed"`) {
		t.Errorf("run output = %s, want the code node's result surface", final.Output)
	}

	// The parked record is closed, so nothing samples it again.
	ops := h.runnerOperations()
	if len(ops) != 1 || ops[0].State != storepg.RunnerOperationCompleted {
		t.Fatalf("runner operations = %+v, want one completed record", ops)
	}
	if ops[0].PollCount < 1 {
		t.Errorf("poll_count = %d, want at least one recorded sample", ops[0].PollCount)
	}

	// The evidence half of the life cycle still landed: the typed operation
	// and the result it produced are recorded against the committed attempt.
	var evidenceRows int
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT count(*) FROM runner_operations AS ro
		JOIN attempts AS a ON a.id = ro.attempt_id
		JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND ro.operation_kind = 'code'`, run.ID).Scan(&evidenceRows); err != nil {
		t.Fatalf("count runner_operations evidence: %v", err)
	}
	if evidenceRows != 1 {
		t.Errorf("runner_operations evidence rows = %d, want 1", evidenceRows)
	}
}

// A nonzero exit is a DOMAIN answer, not an engine failure, on the async path
// exactly as on the in-process one — the technical-status/domain-outcome split
// must not depend on which side of a network the runner is.
func TestRunnerServiceNonzeroExitRoutesTheDomainOutcome(t *testing.T) {
	h := newRunnerServiceHarness(t)

	run := h.createRun("code.workflow.yaml", `{}`)
	if _, err := h.worker.Tick(h.ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	h.service.finish(3)
	h.sampleUntil(h.worker, 10*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if got := h.run(run.ID).State; got != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed: a failing test suite is an answer, not an engine failure", got)
	}
	var status, outcome string
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT status, outcome FROM node_runs WHERE run_id = $1 AND node_key = 'test'`, run.ID,
	).Scan(&status, &outcome); err != nil {
		t.Fatalf("read node run: %v", err)
	}
	if status != "completed" || outcome != "failed" {
		t.Fatalf("node run = (%q, %q), want (completed, failed)", status, outcome)
	}
}

// --- acceptance criterion 2 ------------------------------------------------

// Duplicate and racing completion reports are harmless under the fencing
// discipline.
//
// The interleaving is forced, not hoped for: worker A's sample reads the
// terminal status and stops just before committing, worker B then runs a
// complete sample-and-commit of the SAME operation, and only then is A allowed
// to proceed. Both learned the same terminal result from the same runner;
// exactly one may change workflow state.
//
// What makes the loser harmless is not a lock and not a "have I already done
// this" flag — it is the fencing tuple. A commits through
// ResumeWaitingWork, which re-leases the work item only while it is still
// parked under the tuple recorded at dispatch. B's commit moved the item out
// of 'waiting', so A's UPDATE matches nothing, A never reaches the engine, and
// the duplicate leaves a late diagnostic instead of a second attempt.
func TestRacingRunnerCompletionReportsCommitExactlyOnce(t *testing.T) {
	h := newRunnerServiceHarness(t)

	run := h.createRun("code.workflow.yaml", `{}`)
	if _, err := h.worker.Tick(h.ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	h.service.finish(0)

	var (
		racing    bool
		bDone     bool
		bSampled  int
		hookCalls int
	)
	// The guard is a plain bool, not a sync.Once: worker B's own sample runs
	// INSIDE this hook and reaches it again on the same goroutine, and a Once
	// re-entered from within its own Do deadlocks on itself.
	h.hooks.set(func(attemptID string) {
		hookCalls++
		if racing {
			return
		}
		racing = true

		// Worker B is a different process's sampler, as far as the store is
		// concerned. Bring the operation forward so B's claim finds it due
		// even though A's claim just pushed it out — which is exactly what a
		// completion callback arriving mid-sample does.
		if _, err := h.store.TightenRunnerPoll(h.ctx, h.ns.ID, attemptID, time.Now().UTC()); err != nil {
			t.Errorf("TightenRunnerPoll: %v", err)
			return
		}
		n, err := h.sampleWorker.SampleRunnerOperations(h.ctx)
		if err != nil {
			t.Errorf("worker B SampleRunnerOperations: %v", err)
			return
		}
		bSampled = n
		bDone = true
	})

	// Worker A's own sample. Its commit is now the LOSER of the race.
	if _, err := h.store.TightenRunnerPoll(h.ctx, h.ns.ID, h.runnerOperations()[0].AttemptID, time.Now().UTC()); err != nil {
		t.Fatalf("TightenRunnerPoll: %v", err)
	}
	if sampled, err := h.worker.SampleRunnerOperations(h.ctx); err != nil {
		t.Fatalf("worker A SampleRunnerOperations: %v", err)
	} else if sampled != 1 {
		t.Fatalf("worker A sampled %d operations, want 1", sampled)
	}
	if !bDone || bSampled != 1 {
		t.Fatalf("the racing sampler did not run (done=%v sampled=%d); the test proved nothing", bDone, bSampled)
	}
	if hookCalls < 2 {
		t.Fatalf("the commit hook fired %d times, want at least 2 (both samplers reached a terminal status)", hookCalls)
	}

	// Exactly one attempt exists, and the run completed once.
	if got := h.attemptCount(run.ID); got != 1 {
		t.Fatalf("attempts = %d, want exactly 1: two racing completion reports must commit once", got)
	}
	if got := h.run(run.ID).State; got != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed", got)
	}
	ops := h.runnerOperations()
	if len(ops) != 1 {
		t.Fatalf("runner operations = %d, want 1", len(ops))
	}
	if ops[0].State == storepg.RunnerOperationWaiting {
		t.Fatalf("the operation is still waiting_external after both samplers committed")
	}

	// A third sample after everything is settled is a no-op, not an error: at
	// this point the operation is closed and out of the due queue entirely.
	h.hooks.set(nil)
	sampled, err := h.worker.SampleRunnerOperations(h.ctx)
	if err != nil {
		t.Fatalf("post-settlement sample: %v", err)
	}
	if sampled != 0 {
		t.Errorf("a closed operation was sampled %d more times, want 0", sampled)
	}
	if got := h.attemptCount(run.ID); got != 1 {
		t.Fatalf("attempts = %d after a redundant sample, want 1", got)
	}
}

// The optional callback tightens the next sample's timing and commits
// NOTHING. A notification carries no result, so the worst a forged or replayed
// one can do is cost an extra authenticated status read.
func TestRunnerCallbackOnlyTightensTheNextSample(t *testing.T) {
	h := newRunnerServiceHarness(t)

	run := h.createRun("code.workflow.yaml", `{}`)
	if _, err := h.worker.Tick(h.ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	ops := h.runnerOperations()
	if len(ops) != 1 {
		t.Fatalf("runner operations = %d, want 1", len(ops))
	}
	attemptID := ops[0].AttemptID

	// The dispatch offered a callback, and the token is attempt-scoped.
	callbackURL, callbackToken := h.service.callbackFor(ops[0].OperationID)
	if callbackURL == "" || callbackToken == "" {
		t.Fatalf("the dispatch offered no callback (url=%q token=%q)", callbackURL, callbackToken)
	}
	if !strings.Contains(callbackURL, ops[0].OperationID) {
		t.Errorf("callback url = %q, want it to name the operation", callbackURL)
	}

	// Push the next sample far out, so only a tightening can bring it back.
	if err := h.store.RescheduleRunnerPoll(h.ctx, h.ns.ID, attemptID, time.Now().UTC().Add(time.Hour), "running", ""); err != nil {
		t.Fatalf("RescheduleRunnerPoll: %v", err)
	}
	if n, err := h.worker.SampleRunnerOperations(h.ctx); err != nil || n != 0 {
		t.Fatalf("expected nothing due, sampled %d (%v)", n, err)
	}

	// A forged token is refused, and changes nothing.
	if _, err := h.worker.HandleRunnerCallback(h.ctx, "not-a-real-token",
		runners.CallbackNotification{OperationID: ops[0].OperationID, State: runners.StateCompleted}); err == nil {
		t.Fatal("a callback with an unverifiable token was accepted")
	}
	if n, err := h.worker.SampleRunnerOperations(h.ctx); err != nil || n != 0 {
		t.Fatalf("a refused callback still tightened the schedule (sampled %d, %v)", n, err)
	}
	if h.attemptCount(run.ID) != 0 {
		t.Fatal("a callback committed an attempt; nothing may ever be committed on a callback's word")
	}

	// The real token tightens — and, critically, the operation is still
	// waiting_external afterwards: the callback ingested no outcome.
	tightened, err := h.worker.HandleRunnerCallback(h.ctx, callbackToken,
		runners.CallbackNotification{OperationID: ops[0].OperationID, State: runners.StateCompleted})
	if err != nil {
		t.Fatalf("HandleRunnerCallback: %v", err)
	}
	if !tightened {
		t.Fatal("a valid callback did not bring the next sample forward")
	}
	if got := h.runnerOperations()[0].State; got != storepg.RunnerOperationWaiting {
		t.Fatalf("operation state after a callback = %q, want it still %q", got, storepg.RunnerOperationWaiting)
	}
	if h.attemptCount(run.ID) != 0 {
		t.Fatal("the callback itself committed an attempt")
	}

	// ...and the next sample, which the callback merely made happen sooner, is
	// what actually learns the outcome.
	h.service.finish(0)
	h.sampleUntil(h.worker, 10*time.Second, func() bool { return h.run(run.ID).State.Terminal() })
	if got := h.run(run.ID).State; got != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed", got)
	}
}

// A runner that forgets an operation it accepted answers 404. That is a
// dispatch error, never a completion: the sampler keeps sampling and the
// attempt's own deadline is what eventually decides.
func TestRunnerServiceForgottenOperationIsNeverReadAsACompletion(t *testing.T) {
	h := newRunnerServiceHarness(t)

	run := h.createRun("code.workflow.yaml", `{}`)
	if _, err := h.worker.Tick(h.ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// The runner loses every record of the operation.
	h.service.mu.Lock()
	h.service.dispatched = map[string]runners.Operation{}
	h.service.mu.Unlock()

	for i := 0; i < 3; i++ {
		if _, err := h.worker.SampleRunnerOperations(h.ctx); err != nil {
			t.Fatalf("SampleRunnerOperations: %v", err)
		}
		if _, err := h.store.TightenRunnerPoll(h.ctx, h.ns.ID, h.runnerOperations()[0].AttemptID, time.Now().UTC()); err != nil {
			t.Fatalf("TightenRunnerPoll: %v", err)
		}
	}

	if h.attemptCount(run.ID) != 0 {
		t.Fatal("a 404 status was read as a completion; a forgotten operation is not an outcome")
	}
	op := h.runnerOperations()[0]
	if op.State != storepg.RunnerOperationWaiting {
		t.Fatalf("operation state = %q, want it still waiting for its deadline to decide", op.State)
	}
	if op.LastSampleError == "" {
		t.Error("no sample error was recorded; a wait that is failing to make progress must say why")
	}
	if !strings.Contains(op.LastSampleError, string(runners.ErrorRunnerUnavailable)) {
		t.Errorf("last sample error = %q, want it classified %s", op.LastSampleError, runners.ErrorRunnerUnavailable)
	}
}

// A dispatch the runner refuses outright never parks: there is nothing to
// sample, so the attempt fails now with the refusal's own classification
// rather than waiting for a deadline to notice.
func TestRunnerServiceRefusedDispatchFailsTheAttemptWithoutParking(t *testing.T) {
	h := newRunnerServiceHarness(t)
	h.service.mu.Lock()
	h.service.dispatchStatus = http.StatusForbidden
	h.service.mu.Unlock()

	run := h.createRun("code.workflow.yaml", `{}`)
	if _, err := h.worker.Tick(h.ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if ops := h.runnerOperations(); len(ops) != 0 {
		t.Fatalf("a refused dispatch parked %d operations, want 0", len(ops))
	}
	var status string
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT a.status FROM attempts AS a
		JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = 'test'`, run.ID).Scan(&status); err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	if status != string(engine.StatusPolicyDenied) {
		t.Fatalf("attempt status = %q, want %q (a 403 is an auth/policy refusal)", status, engine.StatusPolicyDenied)
	}
}

// settleGoroutines waits for the process's goroutine count to fall to at most
// max, failing the test if it never does. Goroutine counts are inherently
// noisy (the runtime's own workers, an httptest server's accept loop), so the
// assertion is a ceiling that a per-operation goroutine would blow through,
// not an exact match.
func settleGoroutines(t *testing.T, max int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got int
	for time.Now().Before(deadline) {
		runtime.Gosched()
		got = runtime.NumGoroutine()
		if got <= max {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("goroutines = %d after %s, want no more than %d: a parked operation is holding a goroutine", got, timeout, max)
}
