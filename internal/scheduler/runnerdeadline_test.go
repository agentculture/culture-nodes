package scheduler_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/scheduler"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Task t9's deadline half. A runner operation parked as waiting_external is
// unstuck by exactly the same machinery an asynchronous actor invocation is:
// the §12.7 deadline timer that StartRunnerWait scheduled, fired by this
// package's existing TimerKindDeadline effect.
//
// That reuse is the point, and it is why these tests live beside
// deadline_test.go rather than in a package of their own. There is one
// waiting_external timeout rule in this system, not one per boundary; a
// runner-specific copy of failWaitingExternal would be a second place for
// "when does a parked attempt give up" to drift from the first.
//
// What genuinely differs is only the lookup: a fired deadline timer names a
// timer, and the runtime has to find which durable record — actor invocation
// or runner operation — that timer belongs to.

// startRunnerWait parks the fixture's claimed "build" attempt exactly as a
// runner-protocol 202 does, scheduling a deadline timer for deadline. It is
// startAsyncWait's runner-side twin over the same fixture, so both boundaries
// are proven against the same real run, engine and claim.
func (f *deadlineFixture) startRunnerWait(deadline time.Time) postgres.StartRunnerWaitInput {
	f.t.Helper()

	in := postgres.StartRunnerWaitInput{
		WorkID:       f.claimed.ID,
		WorkerID:     "test-worker",
		FencingToken: f.claimed.FencingToken,
		Attempt:      int(f.claimed.Attempt),

		NamespaceID: f.ns.ID,
		RunID:       f.runID,
		NodeRunID:   f.buildNodeRunID,
		TokenID:     f.buildTokenID,
		NodeID:      "build",

		AttemptID:              "att_" + store.NewULID(),
		RunnerRef:              "runner://company/builder@sha256:333333",
		Endpoint:               "https://runner.thor.internal:8443",
		OperationID:            "op_" + store.NewULID(),
		PollAfterSeconds:       5,
		StatusRetentionSeconds: 86400,

		Deadline: deadline,
	}
	if err := f.store.StartRunnerWait(f.ctx, in); err != nil {
		f.t.Fatalf("StartRunnerWait: %v", err)
	}
	return in
}

func mustRunnerOperationState(t *testing.T, s *postgres.Store, namespaceID, attemptID string) string {
	t.Helper()
	op, err := s.RunnerOperation(context.Background(), namespaceID, attemptID)
	if err != nil {
		t.Fatalf("RunnerOperation %s: %v", attemptID, err)
	}
	return op.State
}

// A runner operation whose deadline passes before any sample read a terminal
// status fails with timed_out and routes its edge — identical to the actor
// path, through the same scheduler effect.
//
// This is what makes a runner service that goes permanently silent safe. The
// protocol forbids reading a missing status as a completion (a 404 is
// runner_unavailable, resampled), so without this timer a runner that vanished
// would leave the attempt parked forever; with it, the wait ends in an honest
// technical timeout that the workflow's own edge routes.
//
// On waiting twice (issue #126). failWaitingExternal commits this one logical
// event in TWO separate transactions: eng.CompleteAttempt flips the node run
// to failed/timed_out and routes its edge, and only afterwards does closeWait
// retire the runner operation to completed. There is no transaction spanning
// both, so observing the first says nothing whatsoever about the second —
// between them the node run is already terminal while the runner operation is
// still waiting_external, and both the state assertion and the due-queue
// assertion below read that second commit.
//
// This test used to wait only for status == "failed" and then read the runner
// operation immediately, which passed solely because the window between the
// two commits is normally microseconds. Under load — CI running every package
// against one shared PostgreSQL widened it enough — that read landed inside
// the window and the test failed on a system that was behaving correctly.
// Each observable therefore gets its own bounded wait; the assertions
// afterwards are unchanged, so a runner operation that never reaches
// completed still fails the test rather than being retried into passing.
func TestSchedulerDeadlineFailsAParkedRunnerOperation(t *testing.T) {
	s := requireStore(t)
	f := newDeadlineFixture(t, s)
	in := f.startRunnerWait(time.Now().Add(-time.Second))

	sch := scheduler.New(s, scheduler.Options{TickInterval: 25 * time.Millisecond})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sch.Run(runCtx) }()

	waitFor(t, 5*time.Second, func() bool {
		status, _ := mustNodeRunStatus(t, s, f.buildNodeRunID)
		return status == "failed"
	})

	status, outcome := mustNodeRunStatus(t, s, f.buildNodeRunID)
	if status != "failed" || outcome != "timed_out" {
		t.Fatalf("build node run = (%q, %q), want (failed, timed_out)", status, outcome)
	}
	if count, latest := attemptCountAndLatestStatus(t, s, f.buildNodeRunID); count != 1 || latest != "timed_out" {
		t.Fatalf("build attempts = %d (latest %q), want 1 with status timed_out", count, latest)
	}
	if !nodeRunExists(t, s, f.runID, "repair") {
		t.Fatal("no repair node run was created; build.timed_out did not route its edge")
	}

	// The second commit. Same helper, same 5s budget as the node-run wait
	// above — not a widened timeout on the first wait, which would still be
	// waiting on the wrong observable, but a wait on the transaction whose
	// effect the next two assertions actually read.
	waitFor(t, 5*time.Second, func() bool {
		return mustRunnerOperationState(t, s, f.ns.ID, in.AttemptID) == postgres.RunnerOperationCompleted
	})

	// The durable runner record is closed, so nothing samples it again: the
	// due queue is the in-flight set.
	if got := mustRunnerOperationState(t, s, f.ns.ID, in.AttemptID); got != postgres.RunnerOperationCompleted {
		t.Errorf("runner operation state = %q, want %q", got, postgres.RunnerOperationCompleted)
	}
	due, err := s.ClaimDueRunnerOperations(context.Background(), f.ns.ID, time.Now().UTC().Add(time.Hour), 10, time.Minute)
	if err != nil {
		t.Fatalf("ClaimDueRunnerOperations: %v", err)
	}
	for _, op := range due {
		if op.AttemptID == in.AttemptID {
			t.Error("a timed-out runner operation is still in the sampling queue")
		}
	}
}

// A deadline timer that fires after a sample already committed the operation's
// terminal result changes nothing. Nothing cancels a deadline timer on a
// normal completion, so this is the ordinary case, not an edge case — and the
// guard that makes it safe is the same one that makes two racing samples safe:
// the work item is no longer parked under the recorded fencing tuple, so the
// resume matches nothing and the engine is never reached.
func TestSchedulerDeadlineAfterARunnerOperationCompletedIsANoOp(t *testing.T) {
	s := requireStore(t)
	f := newDeadlineFixture(t, s)
	in := f.startRunnerWait(time.Now().Add(-time.Second))

	// The operation finished and its result committed, exactly as a sample
	// would have committed it.
	op, err := s.RunnerOperation(f.ctx, f.ns.ID, in.AttemptID)
	if err != nil {
		t.Fatalf("RunnerOperation: %v", err)
	}
	cs, err := postgres.NewCallbackStore(s, f.ns.ID)
	if err != nil {
		t.Fatalf("NewCallbackStore: %v", err)
	}
	if err := cs.ResumeWaitingWork(f.ctx, op.PendingInvocation(), time.Minute); err != nil {
		t.Fatalf("ResumeWaitingWork: %v", err)
	}
	if _, err := f.eng.CompleteAttempt(f.ctx, engine.CompletionRequest{
		WorkID:       op.WorkID,
		WorkerID:     op.WorkerID,
		FencingToken: op.FencingToken,
		Attempt:      op.Attempt,
		TechStatus:   engine.StatusSucceeded,
		Outcome:      "completed",
		Output:       json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("CompleteAttempt: %v", err)
	}
	if err := s.CloseRunnerOperation(f.ctx, f.ns.ID, in.AttemptID, postgres.RunnerOperationCompleted); err != nil {
		t.Fatalf("CloseRunnerOperation: %v", err)
	}

	beforeCount, beforeStatus := attemptCountAndLatestStatus(t, s, f.buildNodeRunID)

	sch := scheduler.New(s, scheduler.Options{TickInterval: 25 * time.Millisecond})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sch.Run(runCtx) }()

	waitFor(t, 5*time.Second, func() bool {
		return sch.Health().Status == scheduler.StatusActive && !sch.Health().LastTick.IsZero()
	})
	time.Sleep(300 * time.Millisecond)
	cancel()

	afterCount, afterStatus := attemptCountAndLatestStatus(t, s, f.buildNodeRunID)
	if afterCount != beforeCount || afterStatus != beforeStatus {
		t.Fatalf("attempts moved from (%d, %q) to (%d, %q) after a late deadline timer fired; "+
			"a completed attempt must never receive a deadline failure",
			beforeCount, beforeStatus, afterCount, afterStatus)
	}
	if got := mustRunnerOperationState(t, s, f.ns.ID, in.AttemptID); got != postgres.RunnerOperationCompleted {
		t.Errorf("runner operation state = %q, want it left %q", got, postgres.RunnerOperationCompleted)
	}
}
