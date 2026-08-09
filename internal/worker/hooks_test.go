package worker_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/runners"
	idstore "github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// Pre-run/post-run code hook integration tests (task t14, spec claim c37,
// honesty condition h32): a real PostgreSQL, a real engine, a real HTTP
// actor exactly like worker_test.go's harness, plus one addition — a FAKE
// Runner implementing internal/runners.Runner in-process. The task brief is
// explicit that the no-in-process rule this whole package otherwise enforces
// (the runner boundary exists so code never executes inside the control
// plane) applies to *production* dispatch; a fake standing in for a runner
// in a test is a test double, not a violation of it.

// fakeHookAnswer is what one Execute call returns: either a Result, or an
// error standing in for runners.Runner's documented "no execution happened"
// contract (a *runners.DispatchError in production; a plain error is enough
// for a test double to prove the same branch).
type fakeHookAnswer struct {
	result runners.Result
	err    error
}

// fakeRunner is an in-process runners.Runner test double. It records every
// operation it was asked to execute and answers from a caller-supplied
// sequence, one answer per call in order (the last entry repeats once the
// sequence is exhausted, so a test only has to size it exactly when the
// exact count matters).
type fakeRunner struct {
	mu      sync.Mutex
	calls   []runners.Operation
	results []fakeHookAnswer
}

func (f *fakeRunner) Execute(_ context.Context, op runners.Operation) (runners.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, op)
	idx := len(f.calls) - 1
	if idx >= len(f.results) {
		idx = len(f.results) - 1
	}
	if idx < 0 {
		return runners.Result{}, nil
	}
	answer := f.results[idx]
	if answer.err != nil {
		return runners.Result{}, answer.err
	}
	res := answer.result
	res.OperationID = op.OperationID
	return res, nil
}

func (f *fakeRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

var _ runners.Runner = (*fakeRunner)(nil)

// fakeHookResult builds a Result honest enough for
// internal/runners.BuildCompletion to turn into real observed evidence: a
// completed operation with a reported exit code and every observation
// declared measured.
func fakeHookResult(exitCode int) runners.Result {
	now := time.Now().UTC()
	code := exitCode
	return runners.Result{
		State: runners.StateCompleted,
		Exit:  &runners.Exit{Code: &code},
		Timing: runners.Timing{
			StartedAt:  now,
			FinishedAt: now,
			DurationMs: 5,
		},
		Environment: runners.Environment{
			RunnerRevision: "rev1",
			ImageDigest:    "sha256:" + strings.Repeat("a", 64),
			PolicyDigest:   "sha256:" + strings.Repeat("b", 64),
		},
		Changes: runners.Changes{Complete: true},
		Observations: runners.Observations{
			ExitStatus:    runners.Observation{Measured: true, Complete: true, Method: "wait4"},
			ChangedPaths:  runners.Observation{Measured: true, Complete: true},
			Logs:          runners.Observation{Measured: true, Complete: true},
			ResourceUsage: runners.Observation{Measured: false, Complete: false},
		},
	}
}

// mustHookRunnerActor registers a fresh, globally-unique actors row (actors.id
// is the table's primary key, not namespace-scoped, so every harness needs
// its own) and returns its id. That id becomes both Options.HookRunnerName
// and the ledger.Origin.ActorID a hook's evidence is attributed to (see
// internal/worker/hooks.go's buildHookEvidence): ledger_records
// (migrations/0003_ledger.sql) has a real foreign key from origin_actor_id
// to actors(id) — the ledger attributes evidence to a registered identity,
// not a bare string. A real deployment has the equivalent obligation:
// whatever process runs the hook runner must be registered as an actor
// before its evidence can be appended.
func mustHookRunnerActor(t *testing.T, s *storepg.Store, namespaceID string) string {
	t.Helper()
	actorID := "fake-hook-runner-" + idstore.NewULID()
	if _, err := s.Pool().Exec(context.Background(), `
		INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
		VALUES ($1, $2, $3, 1, 'runner', 'internal')
	`, actorID, namespaceID, actorID); err != nil {
		t.Fatalf("mustHookRunnerActor: insert actor: %v", err)
	}
	return actorID
}

// newHookHarness is worker_test.go's newHarness with a HookRunner wired in.
// It is a separate constructor rather than a parameter added to newHarness
// so none of that file's six existing tests have to change to accommodate a
// feature they do not exercise.
func newHookHarness(t *testing.T, hr runners.Runner, actorHandler func(h *harness, w http.ResponseWriter, req actors.InvocationRequest)) *harness {
	t.Helper()
	s := pgtest.RequireStore(t, testStore)

	ns := pgtest.MustNamespace(t, s, "worker-hooks")
	hookRunnerActorID := mustHookRunnerActor(t, s, ns.ID)
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

	h.callbackServer = httptest.NewServer(actors.NewCallbackHandler(actors.CallbackDeps{
		Store:  callbacks,
		Engine: eng,
		Signer: signer,
	}))
	t.Cleanup(h.callbackServer.Close)

	h.actorServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		"actor://company/hookworker@sha256:cccccc": {URL: h.actorServer.URL},
	}
	wk, err := worker.New(s, eng, worker.Options{
		WorkerID:           "worker-" + t.Name(),
		NamespaceID:        ns.ID,
		ClaimBatch:         4,
		LeaseDuration:      30 * time.Second,
		HeartbeatInterval:  200 * time.Millisecond,
		PollInterval:       20 * time.Millisecond,
		Registry:           registry,
		Signer:             signer,
		CallbackBaseURL:    h.callbackServer.URL,
		HookRunner:         hr,
		HookRunnerName:     hookRunnerActorID,
		HookRunnerRevision: "rev1",
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

func (h *harness) ledgerRecords(t *testing.T, runID string) []ledger.Record {
	t.Helper()
	l, err := storepg.NewLedger(h.store, h.ns.ID)
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	recs, err := l.Records(h.ctx, runID)
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	return recs
}

func filterRecordType(recs []ledger.Record, want ledger.RecordType) []ledger.Record {
	var out []ledger.Record
	for _, r := range recs {
		if r.RecordType == want {
			out = append(out, r)
		}
	}
	return out
}

func filterAuthority(recs []ledger.Record, want ledger.Authority) []ledger.Record {
	var out []ledger.Record
	for _, r := range recs {
		if r.Authority == want {
			out = append(out, r)
		}
	}
	return out
}

func (h *harness) nodeRunOutcome(t *testing.T, runID, nodeKey string) string {
	t.Helper()
	var outcome string
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT outcome FROM node_runs WHERE run_id = $1 AND node_key = $2`, runID, nodeKey,
	).Scan(&outcome); err != nil {
		t.Fatalf("read node run %s: %v", nodeKey, err)
	}
	return outcome
}

func (h *harness) nodeRunStatus(t *testing.T, runID, nodeKey string) string {
	t.Helper()
	var status string
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT status FROM node_runs WHERE run_id = $1 AND node_key = $2`, runID, nodeKey,
	).Scan(&status); err != nil {
		t.Fatalf("read node run %s: %v", nodeKey, err)
	}
	return status
}

func (h *harness) attemptStatuses(t *testing.T, runID, nodeKey string) []string {
	t.Helper()
	rows, err := h.store.Pool().Query(h.ctx, `
		SELECT a.status FROM attempts AS a
		JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = $2
		ORDER BY a.attempt_number`, runID, nodeKey)
	if err != nil {
		t.Fatalf("query attempts: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan attempt status: %v", err)
		}
		out = append(out, s)
	}
	return out
}

func (h *harness) attemptID(t *testing.T, runID, nodeKey string) string {
	t.Helper()
	var id string
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT a.id FROM attempts AS a
		JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = $2
		ORDER BY a.attempt_number DESC LIMIT 1`, runID, nodeKey,
	).Scan(&id); err != nil {
		t.Fatalf("read latest attempt id for %s: %v", nodeKey, err)
	}
	return id
}

// (a) pre-run fails → attempt failed, actor hit-counter zero, evidence
// appended, retry per policy. The fake runner fails pre_run on both attempts
// the node's policy allows (maxAttempts: 2), so the run ultimately fails —
// proving both that the attempt is retried and that the agent is never
// invoked across either attempt.
func TestPreRunHookFailureBlocksTheAgentAndRetries(t *testing.T) {
	fr := &fakeRunner{results: []fakeHookAnswer{
		{result: fakeHookResult(1)}, // attempt 1 pre_run: exits 1
		{result: fakeHookResult(1)}, // attempt 2 pre_run: exits 1
	}}

	var actorHits int32
	h := newHookHarness(t, fr, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		atomic.AddInt32(&actorHits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	})

	run := h.createRun("hooks.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	final := h.run(run.ID)
	if final.State != engine.RunFailed {
		t.Fatalf("run state = %s, want failed once both pre_run attempts fail (worker errors: %v)", final.State, h.workerErrors())
	}
	if got := atomic.LoadInt32(&actorHits); got != 0 {
		t.Fatalf("actor was invoked %d times; a failing pre_run hook must never invoke the agent", got)
	}
	if got := fr.callCount(); got != 2 {
		t.Fatalf("hook runner was called %d times, want 2 (one pre_run attempt per retry)", got)
	}

	statuses := h.attemptStatuses(t, run.ID, "work")
	if len(statuses) != 2 || statuses[0] != string(engine.StatusFailed) || statuses[1] != string(engine.StatusFailed) {
		t.Fatalf("attempt statuses = %v, want [failed failed] — a pre-run failure is a technical failure, never a domain outcome", statuses)
	}

	recs := h.ledgerRecords(t, run.ID)
	evidence := filterRecordType(recs, ledger.RecordEvidence)
	if len(evidence) != 2 {
		t.Fatalf("ledger carries %d evidence records, want 2 (one per retried pre_run attempt): %+v (worker errors: %v)", len(evidence), recs, h.workerErrors())
	}
	for _, rec := range evidence {
		if rec.Authority != ledger.AuthorityObserved || rec.Origin.Kind != ledger.OriginRunner {
			t.Errorf("evidence record %s = %+v, want observed authority from a runner origin", rec.ID, rec)
		}
	}
}

// (b) post-run fails with on_failure.outcome=changes_required → node
// completes with changes_required and the edge routes to a node the agent's
// own `completed` outcome never reaches.
func TestPostRunOnFailureRoutesToDeclaredOutcome(t *testing.T) {
	fr := &fakeRunner{results: []fakeHookAnswer{
		{result: fakeHookResult(0)}, // pre_run passes
		{result: fakeHookResult(1)}, // post_run's check fails
	}}
	h := newHookHarness(t, fr, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		writeSyncResult(w, "completed", `{"summary":"looks fine to the agent"}`)
	})

	run := h.createRun("hooks.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	final := h.run(run.ID)
	if final.State != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed via the changes_required edge (worker errors: %v)", final.State, h.workerErrors())
	}

	if outcome := h.nodeRunOutcome(t, run.ID, "work"); outcome != "changes_required" {
		t.Fatalf("node work outcome = %q, want changes_required — the agent itself reported completed, but a failing post_run check must win", outcome)
	}
	if status := h.nodeRunStatus(t, run.ID, "revise"); status != "completed" {
		t.Errorf("node revise status = %q, want completed: the changes_required edge must actually route there", status)
	}

	recs := h.ledgerRecords(t, run.ID)
	if evidence := filterRecordType(recs, ledger.RecordEvidence); len(evidence) != 2 {
		t.Errorf("ledger carries %d evidence records, want 2 (pre_run + post_run): %+v", len(evidence), recs)
	}
}

// (c) both pass → agent outcome + both hooks' evidence in the ledger.
func TestBothHooksPassAppendsBothEvidenceRecords(t *testing.T) {
	fr := &fakeRunner{results: []fakeHookAnswer{
		{result: fakeHookResult(0)}, // pre_run passes
		{result: fakeHookResult(0)}, // post_run passes
	}}

	var actorHits int32
	h := newHookHarness(t, fr, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		atomic.AddInt32(&actorHits, 1)
		writeSyncResult(w, "completed", `{"summary":"all good"}`)
	})

	run := h.createRun("hooks.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	final := h.run(run.ID)
	if final.State != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", final.State, h.workerErrors())
	}
	if got := atomic.LoadInt32(&actorHits); got != 1 {
		t.Fatalf("actor invocations = %d, want 1", got)
	}
	if outcome := h.nodeRunOutcome(t, run.ID, "work"); outcome != "completed" {
		t.Errorf("node work outcome = %q, want the agent's own completed", outcome)
	}

	recs := h.ledgerRecords(t, run.ID)
	evidence := filterRecordType(recs, ledger.RecordEvidence)
	if len(evidence) != 2 {
		t.Fatalf("ledger carries %d evidence records, want 2 (pre_run + post_run): %+v", len(evidence), recs)
	}

	attemptID := h.attemptID(t, run.ID, "work")
	ops, err := h.store.ListRunnerOperationsByAttempt(h.ctx, attemptID)
	if err != nil {
		t.Fatalf("ListRunnerOperationsByAttempt: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("runner_operations rows = %d, want 2 (pre_run + post_run): %+v", len(ops), ops)
	}
	kinds := map[string]bool{}
	for _, op := range ops {
		kinds[op.OperationKind] = true
		if op.AttemptID != attemptID {
			t.Errorf("runner_operations row %s attempt_id = %q, want %q", op.ID, op.AttemptID, attemptID)
		}
	}
	if !kinds["pre_run"] || !kinds["post_run"] {
		t.Errorf("runner_operations kinds = %v, want pre_run and post_run", kinds)
	}
}

// (d) reject_assurance appends the derived record: the agent's own outcome
// still stands, and a derived, validator-origin rejection is appended
// alongside it — never silent, never a routing change the agent did not
// earn.
func TestPostRunRejectAssuranceAppendsDerivedRecord(t *testing.T) {
	fr := &fakeRunner{results: []fakeHookAnswer{
		{result: fakeHookResult(1)}, // post_run's check fails
	}}
	h := newHookHarness(t, fr, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		writeSyncResult(w, "completed", `{"summary":"agent says it is done"}`)
	})

	run := h.createRun("hooks-reject-assurance.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	final := h.run(run.ID)
	if final.State != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed: the agent's own outcome still stands under reject_assurance (worker errors: %v)", final.State, h.workerErrors())
	}
	if outcome := h.nodeRunOutcome(t, run.ID, "work"); outcome != "completed" {
		t.Errorf("node work outcome = %q, want the agent's own completed — reject_assurance disputes it, it does not replace it", outcome)
	}

	recs := h.ledgerRecords(t, run.ID)
	derived := filterAuthority(recs, ledger.AuthorityDerived)
	if len(derived) != 1 {
		t.Fatalf("ledger carries %d derived records, want exactly 1 assurance rejection: %+v", len(derived), recs)
	}
	rejection := derived[0]
	if rejection.Origin.Kind != ledger.OriginValidator {
		t.Errorf("assurance rejection origin = %+v, want validator", rejection.Origin)
	}
	if rejection.RecordType != ledger.RecordReview {
		t.Errorf("assurance rejection record type = %q, want review", rejection.RecordType)
	}
	var data struct {
		Verdict string `json:"verdict"`
	}
	if err := json.Unmarshal(rejection.Data, &data); err != nil {
		t.Fatalf("decode rejection data: %v", err)
	}
	if data.Verdict != "reject" {
		t.Errorf("rejection verdict = %q, want reject", data.Verdict)
	}
}

// Async (202) agents that declare post_run are refused rather than parked
// (see internal/worker/hooks.go's package doc for why this build cannot run
// a post-run hook against a callback-delivered result): the attempt fails
// as a technical, retryable contract rejection, and the work item is never
// left waiting on a hook that will never run.
func TestAsyncActorWithPostRunIsRefusedNotParked(t *testing.T) {
	fr := &fakeRunner{} // never called: the refusal happens before any hook dispatch
	h := newHookHarness(t, fr, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"invocation_id":"external_async","heartbeat_after_seconds":30,"supports_cancellation":true}`))
	})

	run := h.createRun("hooks-async-postrun.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	final := h.run(run.ID)
	if final.State != engine.RunFailed {
		t.Fatalf("run state = %s, want failed: an async actor cannot satisfy post_run in this build (worker errors: %v)", final.State, h.workerErrors())
	}
	if got := fr.callCount(); got != 0 {
		t.Fatalf("hook runner was called %d times, want 0: the refusal happens before any hook dispatch", got)
	}

	var state string
	var leaseOwner *string
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT wi.state, wi.lease_owner
		FROM work_items AS wi JOIN node_runs AS nr ON nr.id = wi.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = 'work'
	`, run.ID).Scan(&state, &leaseOwner); err != nil {
		t.Fatalf("read work item: %v", err)
	}
	if state == storepg.WaitingWorkState {
		t.Fatalf("work item state = %q; an async acceptance with post_run declared must never be parked", state)
	}

	statuses := h.attemptStatuses(t, run.ID, "work")
	if len(statuses) != 1 || statuses[0] != string(engine.StatusContractRejected) {
		t.Fatalf("attempt statuses = %v, want [contract_rejected]", statuses)
	}
}
