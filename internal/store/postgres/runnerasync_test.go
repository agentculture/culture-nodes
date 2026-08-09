package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Durable state for parked runner-protocol operations (task t9).
//
// The invariant every test here circles is §12.6's: between two status samples
// nothing in any process holds anything about the operation. That makes this
// table the only thing that knows which work item, under which fencing tuple,
// a later sample is allowed to commit — so the tests below are mostly about
// what happens when two callers reach for the same row at once.

// runnerWaitFixture is one parked runner operation over a real work item
// claimed through the real claiming path. Hand-writing the work_items row
// would prove these queries against a fixture rather than against the store
// they actually run with.
type runnerWaitFixture struct {
	t       *testing.T
	ctx     context.Context
	store   *postgres.Store
	ns      postgres.Namespace
	runID   string
	nodeRun string
	claimed postgres.ClaimedWork
}

func newRunnerWaitFixture(t *testing.T, s *postgres.Store, slug string) *runnerWaitFixture {
	t.Helper()
	ctx := context.Background()
	ns := mustNamespace(t, s, slug)

	wv, err := s.CreateWorkflowVersion(ctx, postgres.CreateWorkflowVersionInput{
		NamespaceID:   ns.ID,
		WorkflowKey:   "runner-wait-" + store.NewULID(),
		Version:       1,
		SourceFormat:  "yaml",
		Source:        "entrypoint: build\n",
		ContentDigest: "sha256:" + store.NewULID(),
	})
	if err != nil {
		t.Fatalf("CreateWorkflowVersion: %v", err)
	}

	runID := store.NewULID()
	if _, err := s.Pool().Exec(ctx,
		`INSERT INTO runs (id, namespace_id, workflow_version_id) VALUES ($1, $2, $3)`,
		runID, ns.ID, wv.ID); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	nodeRunID := store.NewULID()
	if _, err := s.Pool().Exec(ctx,
		`INSERT INTO node_runs (id, namespace_id, run_id, node_key, status) VALUES ($1, $2, $3, 'build', 'running')`,
		nodeRunID, ns.ID, runID); err != nil {
		t.Fatalf("insert node_run: %v", err)
	}
	if err := s.EnqueueWork(ctx, postgres.WorkItem{NamespaceID: ns.ID, NodeRunID: nodeRunID}); err != nil {
		t.Fatalf("EnqueueWork: %v", err)
	}
	claimed, err := s.ClaimWork(ctx, ns.ID, "worker-fixture", time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimWork: %v (claimed %d)", err, len(claimed))
	}

	return &runnerWaitFixture{t: t, ctx: ctx, store: s, ns: ns, runID: runID, nodeRun: nodeRunID, claimed: claimed[0]}
}

func (f *runnerWaitFixture) park(attemptID string, deadline time.Time) postgres.StartRunnerWaitInput {
	f.t.Helper()
	in := postgres.StartRunnerWaitInput{
		WorkID:                 f.claimed.ID,
		WorkerID:               "worker-fixture",
		FencingToken:           f.claimed.FencingToken,
		Attempt:                int(f.claimed.Attempt),
		NamespaceID:            f.ns.ID,
		RunID:                  f.runID,
		NodeRunID:              f.nodeRun,
		NodeID:                 "build",
		AttemptID:              attemptID,
		RunnerRef:              "runner://thor/docker",
		Endpoint:               "https://runner.thor.internal:8443",
		OperationID:            attemptID,
		PollAfterSeconds:       5,
		StatusRetentionSeconds: 86400,
		SupportsCallback:       true,
		NextPollAt:             time.Now().UTC().Add(-time.Second),
		Deadline:               deadline,
	}
	if err := f.store.StartRunnerWait(f.ctx, in); err != nil {
		f.t.Fatalf("StartRunnerWait: %v", err)
	}
	return in
}

func (f *runnerWaitFixture) workItemState() string {
	f.t.Helper()
	var state string
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT state FROM work_items WHERE id = $1`, f.claimed.ID).Scan(&state); err != nil {
		f.t.Fatalf("read work item state: %v", err)
	}
	return state
}

func (f *runnerWaitFixture) nodeRunStatus() string {
	f.t.Helper()
	var status string
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT status FROM node_runs WHERE id = $1`, f.nodeRun).Scan(&status); err != nil {
		f.t.Fatalf("read node run status: %v", err)
	}
	return status
}

// The park transition: the lease is released without completing anything, the
// node run says waiting_external, the durable row carries the fencing tuple,
// and a deadline timer is the only thing that will ever wake it if the runner
// goes silent forever.
func TestStartRunnerWaitParksTheWorkItemAndRecordsTheFencingTuple(t *testing.T) {
	s := requireStore(t)
	f := newRunnerWaitFixture(t, s, "test-runner-park")

	attemptID := "att_" + store.NewULID()
	in := f.park(attemptID, time.Now().UTC().Add(10*time.Minute))

	if got := f.workItemState(); got != postgres.WaitingWorkState {
		t.Fatalf("work item state = %q, want %q (the lease must be released without completing the item)",
			got, postgres.WaitingWorkState)
	}
	if got := f.nodeRunStatus(); got != "waiting_external" {
		t.Fatalf("node run status = %q, want waiting_external", got)
	}

	op, err := s.RunnerOperation(f.ctx, f.ns.ID, attemptID)
	if err != nil {
		t.Fatalf("RunnerOperation: %v", err)
	}
	if op.WorkID != in.WorkID || op.FencingToken != in.FencingToken || op.Attempt != in.Attempt {
		t.Errorf("recorded fencing tuple = (%s, %d, %d), want (%s, %d, %d)",
			op.WorkID, op.FencingToken, op.Attempt, in.WorkID, in.FencingToken, in.Attempt)
	}
	if op.OperationID != attemptID || op.Endpoint != in.Endpoint || op.RunnerRef != in.RunnerRef {
		t.Errorf("recorded dispatch identity = %+v, want the operation, endpoint and runner ref that were dispatched", op)
	}
	if op.State != postgres.RunnerOperationWaiting {
		t.Errorf("state = %q, want %q", op.State, postgres.RunnerOperationWaiting)
	}
	if op.DeadlineTimerID == "" {
		t.Error("no deadline timer was scheduled; a parked operation with no deadline can never be unstuck")
	}

	// The deadline timer is a real §12.7 timer the scheduler will claim.
	var kind string
	if err := s.Pool().QueryRow(f.ctx, `SELECT timer_kind FROM timers WHERE id = $1`, op.DeadlineTimerID).Scan(&kind); err != nil {
		t.Fatalf("read deadline timer: %v", err)
	}
	if kind != string(postgres.TimerKindDeadline) {
		t.Errorf("timer kind = %q, want %q", kind, postgres.TimerKindDeadline)
	}
}

// A park whose fencing tuple has moved on writes nothing at all — the same
// guarantee CompleteWork and StartAsyncWait give.
func TestStartRunnerWaitRefusesAStaleClaim(t *testing.T) {
	s := requireStore(t)
	f := newRunnerWaitFixture(t, s, "test-runner-park-stale")

	in := postgres.StartRunnerWaitInput{
		WorkID:       f.claimed.ID,
		WorkerID:     "worker-fixture",
		FencingToken: f.claimed.FencingToken + 99, // somebody else re-claimed it
		Attempt:      int(f.claimed.Attempt),
		NamespaceID:  f.ns.ID,
		RunID:        f.runID,
		NodeRunID:    f.nodeRun,
		NodeID:       "build",
		AttemptID:    "att_" + store.NewULID(),
		RunnerRef:    "runner://thor/docker",
		Endpoint:     "https://runner.thor.internal:8443",
		OperationID:  "op_stale",
	}
	err := s.StartRunnerWait(f.ctx, in)
	if !errors.Is(err, engine.ErrStaleClaim) {
		t.Fatalf("StartRunnerWait with a stale tuple = %v, want engine.ErrStaleClaim", err)
	}
	if got := f.workItemState(); got != "leased" {
		t.Errorf("work item state = %q, want leased (a refused park must write nothing)", got)
	}
}

// The sampler's claim: due operations come back exactly once per claim, and a
// second sampler racing the first gets none of them. This is what makes
// "no goroutine per in-flight operation" affordable — the queue is the table.
func TestClaimDueRunnerOperationsIsARaceFreeQueue(t *testing.T) {
	s := requireStore(t)
	f := newRunnerWaitFixture(t, s, "test-runner-due")

	attemptID := "att_" + store.NewULID()
	f.park(attemptID, time.Now().UTC().Add(10*time.Minute))

	now := time.Now().UTC()
	first, err := s.ClaimDueRunnerOperations(f.ctx, f.ns.ID, now, 10, 30*time.Second)
	if err != nil {
		t.Fatalf("ClaimDueRunnerOperations: %v", err)
	}
	if len(first) != 1 || first[0].AttemptID != attemptID {
		t.Fatalf("first claim returned %d operations, want the one parked operation", len(first))
	}
	if first[0].PollCount != 1 {
		t.Errorf("poll_count = %d after one claim, want 1", first[0].PollCount)
	}

	// The claim advanced next_poll_at, so a second sampler at the same instant
	// finds nothing: two samplers never sample the same operation at once.
	second, err := s.ClaimDueRunnerOperations(f.ctx, f.ns.ID, now, 10, 30*time.Second)
	if err != nil {
		t.Fatalf("second ClaimDueRunnerOperations: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second claim returned %d operations, want none (the first claim owns this sampling round)", len(second))
	}

	// Once the backoff has elapsed the operation is due again — a sampler that
	// died mid-sample strands nothing.
	later, err := s.ClaimDueRunnerOperations(f.ctx, f.ns.ID, now.Add(31*time.Second), 10, 30*time.Second)
	if err != nil {
		t.Fatalf("third ClaimDueRunnerOperations: %v", err)
	}
	if len(later) != 1 {
		t.Fatalf("after the sampling interval elapsed the operation was claimed %d times, want 1", len(later))
	}
}

// A closed operation is never sampled again: the due queue is the in-flight
// set, not the history.
func TestClosedRunnerOperationsLeaveTheDueQueue(t *testing.T) {
	s := requireStore(t)
	f := newRunnerWaitFixture(t, s, "test-runner-closed")

	attemptID := "att_" + store.NewULID()
	f.park(attemptID, time.Now().UTC().Add(10*time.Minute))

	if err := s.CloseRunnerOperation(f.ctx, f.ns.ID, attemptID, postgres.RunnerOperationCompleted); err != nil {
		t.Fatalf("CloseRunnerOperation: %v", err)
	}
	due, err := s.ClaimDueRunnerOperations(f.ctx, f.ns.ID, time.Now().UTC().Add(time.Hour), 10, 30*time.Second)
	if err != nil {
		t.Fatalf("ClaimDueRunnerOperations: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("a completed operation was claimed for sampling %d times, want 0", len(due))
	}

	// Closing something already closed is a no-op, not an error: two racing
	// samples that both reach a terminal status must both be able to finish.
	if err := s.CloseRunnerOperation(f.ctx, f.ns.ID, attemptID, postgres.RunnerOperationSuperseded); err != nil {
		t.Fatalf("second CloseRunnerOperation: %v", err)
	}
	op, err := s.RunnerOperation(f.ctx, f.ns.ID, attemptID)
	if err != nil {
		t.Fatalf("RunnerOperation: %v", err)
	}
	if op.State != postgres.RunnerOperationCompleted {
		t.Errorf("state = %q after a second close, want the first close to stand (%q)",
			op.State, postgres.RunnerOperationCompleted)
	}
}

// The callback's entire power: bring the next sample forward. It commits
// nothing, so the worst a forged or replayed notification can do is cost one
// extra authenticated status read.
func TestTightenRunnerPollOnlyMovesTheNextSampleEarlier(t *testing.T) {
	s := requireStore(t)
	f := newRunnerWaitFixture(t, s, "test-runner-tighten")

	attemptID := "att_" + store.NewULID()
	f.park(attemptID, time.Now().UTC().Add(10*time.Minute))

	now := time.Now().UTC()
	if _, err := s.ClaimDueRunnerOperations(f.ctx, f.ns.ID, now, 10, time.Hour); err != nil {
		t.Fatalf("ClaimDueRunnerOperations: %v", err)
	}
	// Nothing is due for an hour now.
	due, err := s.ClaimDueRunnerOperations(f.ctx, f.ns.ID, now, 10, time.Hour)
	if err != nil || len(due) != 0 {
		t.Fatalf("expected nothing due, got %d (%v)", len(due), err)
	}

	tightened, err := s.TightenRunnerPoll(f.ctx, f.ns.ID, attemptID, now)
	if err != nil {
		t.Fatalf("TightenRunnerPoll: %v", err)
	}
	if !tightened {
		t.Fatal("TightenRunnerPoll reported no in-flight operation to tighten")
	}
	due, err = s.ClaimDueRunnerOperations(f.ctx, f.ns.ID, now, 10, time.Hour)
	if err != nil {
		t.Fatalf("ClaimDueRunnerOperations after tightening: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("after a callback tightened the schedule, %d operations were due, want 1", len(due))
	}

	// A callback for an operation that already finished changes nothing at
	// all — there is nothing left to sample.
	if err := s.CloseRunnerOperation(f.ctx, f.ns.ID, attemptID, postgres.RunnerOperationCompleted); err != nil {
		t.Fatalf("CloseRunnerOperation: %v", err)
	}
	tightened, err = s.TightenRunnerPoll(f.ctx, f.ns.ID, attemptID, now)
	if err != nil {
		t.Fatalf("TightenRunnerPoll after close: %v", err)
	}
	if tightened {
		t.Error("a callback for a finished operation reopened it; a callback must never commit anything")
	}
}

// A fired deadline timer names a timer, not an attempt. The reverse lookup is
// what lets the scheduler's existing waiting_external deadline path fail a
// parked runner operation exactly as it fails a parked actor invocation.
func TestRunnerOperationByDeadlineTimerIsTheSchedulersReverseLookup(t *testing.T) {
	s := requireStore(t)
	f := newRunnerWaitFixture(t, s, "test-runner-deadline-lookup")

	attemptID := "att_" + store.NewULID()
	f.park(attemptID, time.Now().UTC().Add(10*time.Minute))

	op, err := s.RunnerOperation(f.ctx, f.ns.ID, attemptID)
	if err != nil {
		t.Fatalf("RunnerOperation: %v", err)
	}

	inv, ok, err := s.RunnerOperationByDeadlineTimer(f.ctx, op.DeadlineTimerID)
	if err != nil {
		t.Fatalf("RunnerOperationByDeadlineTimer: %v", err)
	}
	if !ok {
		t.Fatal("the deadline timer did not resolve to its runner operation")
	}
	if inv.AttemptID != attemptID || inv.WorkID != f.claimed.ID || inv.FencingToken != f.claimed.FencingToken {
		t.Errorf("resolved invocation = %+v, want the parked operation's fencing tuple", inv)
	}
	if inv.State != actors.InvocationWaiting {
		t.Errorf("state = %q, want %q so the scheduler's own guard reads it", inv.State, actors.InvocationWaiting)
	}

	// A timer nothing points at is "nothing to fail", not a fault.
	if _, ok, err := s.RunnerOperationByDeadlineTimer(f.ctx, store.NewULID()); err != nil || ok {
		t.Errorf("unknown timer resolved to (ok=%v, err=%v), want (false, nil)", ok, err)
	}
}

// migrations/0011 is expand-only: it adds one table and its indexes, and a
// binary that predates it is unaffected because it never looks them up.
func TestMigration0011CreatesRunnerInvocations(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	var tableExists bool
	if err := s.Pool().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'runner_invocations')`,
	).Scan(&tableExists); err != nil {
		t.Fatalf("check runner_invocations: %v", err)
	}
	if !tableExists {
		t.Fatal("table runner_invocations not found -- migrations/0011 should have created it")
	}

	for _, index := range []string{
		"runner_invocations_due_idx",
		"runner_invocations_deadline_timer_idx",
		"runner_invocations_namespace_id_idx",
	} {
		var exists bool
		if err := s.Pool().QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE tablename = 'runner_invocations' AND indexname = $1)`,
			index).Scan(&exists); err != nil {
			t.Fatalf("check index %s: %v", index, err)
		}
		if !exists {
			t.Errorf("index %s not found on runner_invocations", index)
		}
	}

	// The existing evidence table is untouched by this migration: the two
	// runner tables are different halves of one life cycle, not a rename.
	var evidenceTable bool
	if err := s.Pool().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'runner_operations')`,
	).Scan(&evidenceTable); err != nil {
		t.Fatalf("check runner_operations: %v", err)
	}
	if !evidenceTable {
		t.Fatal("table runner_operations disappeared; 0011 must be expand-only")
	}
}
