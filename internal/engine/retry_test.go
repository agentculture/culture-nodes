package engine_test

import (
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
)

// The retry decision for a timed-out attempt (task t10, spec claims c42/c49).
//
// `timed_out` is retryable in the sense retryable() means — a second attempt
// really could answer differently — and that alone used to be enough to
// re-dispatch. It is not, because the control plane can mint a `timed_out`
// against a session that is still running: internal/scheduler completes the
// attempt when a waiting_external deadline expires and only asks the actor to
// stop AFTER that commits (decision q3/c48). These tests pin the three
// answers the engine now gives, and the boundary between them is who produced
// the timeout, never how it is spelled.
//
// `work` in loop.workflow.yaml declares maxAttempts 3 and no edge from
// `work.timed_out`, so a refusal here is unambiguous: an unspent budget, and
// nowhere for the status to route.

// A deadline expiry does not buy a second session, even with the budget for
// one. Task t10's engine-level half; the scheduler-level half (a real timer,
// a real parked invocation) is
// internal/scheduler's TestSchedulerDeadlineTimeoutIsNotRetriedIntoASecondSession.
func TestDeadlineTimeoutIsNotRetried(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")
	runID, workNodeRun := advanceToWork(t, f, "deadline")

	result := f.step("worker-a", workNodeRun, engine.CompletionRequest{
		TechStatus:    engine.StatusTimedOut,
		TimeoutOrigin: engine.TimeoutOriginDeadline,
	})

	if result.Retried {
		t.Fatal("a deadline expiry must not spend the retry budget: the session it timed out has been " +
			"asked to stop, not observed stopping")
	}
	if result.AttemptNumber != 1 {
		t.Errorf("attempt number = %d, want 1", result.AttemptNumber)
	}
	// Refused is not the same as exhausted, and a caller must be able to tell:
	// two of three attempts were still available.
	if result.RetryRefused == "" {
		t.Error("RetryRefused is empty; a refused retry that looks like an exhausted budget is the " +
			"warning c49 says today's behaviour already effectively is")
	}
	if got := f.nodeRunCount(runID, "work"); got != 1 {
		t.Errorf("work has %d node runs, want 1", got)
	}
	if n := f.countScalar(`SELECT COUNT(*)::int FROM attempts WHERE node_run_id = $1`, workNodeRun); n != 1 {
		t.Errorf("recorded %d attempts, want exactly 1 — a second is exactly what the fence refuses", n)
	}

	types := f.eventTypes(runID)
	if contains(types, engine.TypeAttemptRetryScheduled) {
		t.Errorf("events = %v, want no %s", types, engine.TypeAttemptRetryScheduled)
	}
	if !contains(types, engine.TypeAttemptRetryRefused) {
		t.Errorf("events = %v, want one of type %s", types, engine.TypeAttemptRetryRefused)
	}

	// work declares no edge from timed_out, so with the retry refused the run
	// ends — and it says why, naming the refusal rather than only the status.
	if result.RunState != engine.RunFailed {
		t.Fatalf("run state = %s, want failed", result.RunState)
	}
	if result.Diagnostic == "" {
		t.Error("a run failed by a refused retry should say so")
	}
}

// The fail-closed half of the honesty condition on c49: an ambiguous state
// resolves to refusal. Nothing in the request says where this timeout came
// from, so the engine cannot know whether a session is still holding the
// workspace — and a fence that opens when it is unsure is not a fence.
//
// This is the case that makes the field's zero value load-bearing: a future
// producer of `timed_out` that forgets to name its origin gets the safe
// answer rather than the convenient one.
func TestTimeoutOfUnknownOriginIsNotRetried(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")
	runID, workNodeRun := advanceToWork(t, f, "unknown-origin")

	result := f.step("worker-a", workNodeRun, engine.CompletionRequest{
		TechStatus: engine.StatusTimedOut,
	})

	if result.Retried {
		t.Fatal("a timeout naming no origin must not be retried; the fence fails closed")
	}
	if result.RetryRefused == "" {
		t.Error("RetryRefused is empty; the refusal must be legible, not silent")
	}
	if !contains(f.eventTypes(runID), engine.TypeAttemptRetryRefused) {
		t.Errorf("events = %v, want one of type %s", f.eventTypes(runID), engine.TypeAttemptRetryRefused)
	}
	if n := f.countScalar(`SELECT COUNT(*)::int FROM attempts WHERE node_run_id = $1`, workNodeRun); n != 1 {
		t.Errorf("recorded %d attempts, want exactly 1", n)
	}
}

// The other side of the boundary, and the reason this is a fence on ORIGIN
// rather than a blanket "timeouts are not retryable": when the ACTOR reported
// the timeout, its invocation is over because it said so. There is no session
// left to fence against, and the node's declared retry budget applies exactly
// as it always has.
func TestActorReportedTimeoutIsStillRetried(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")
	runID, workNodeRun := advanceToWork(t, f, "actor-timeout")

	result := f.step("worker-a", workNodeRun, engine.CompletionRequest{
		TechStatus:    engine.StatusTimedOut,
		TimeoutOrigin: engine.TimeoutOriginActor,
	})

	if !result.Retried {
		t.Fatalf("work declares maxAttempts 3; an actor-reported timeout should still buy attempt 2 (%s)",
			result.Diagnostic)
	}
	if result.RetryRefused != "" {
		t.Errorf("RetryRefused = %q, want empty", result.RetryRefused)
	}
	if got := f.nodeRun(workNodeRun).State; got != engine.NodeRunReady {
		t.Errorf("node run state = %s, want ready", got)
	}
	types := f.eventTypes(runID)
	if !contains(types, engine.TypeAttemptRetryScheduled) {
		t.Errorf("events = %v, want one of type %s", types, engine.TypeAttemptRetryScheduled)
	}
	if contains(types, engine.TypeAttemptRetryRefused) {
		t.Errorf("events = %v, want no %s", types, engine.TypeAttemptRetryRefused)
	}
}

// The fence is scoped to timeouts, and this is the check that keeps it there.
// `failed` reaches the engine from a worker or an actor that has finished with
// the invocation and is reporting on it; the control plane never mints one
// against a live session, so nothing about it is ambiguous and its retry
// needs no vouching.
func TestPlainFailureIsRetriedWithoutVouchingForAnOrigin(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")
	_, workNodeRun := advanceToWork(t, f, "plain-failure")

	result := f.step("worker-a", workNodeRun, engine.CompletionRequest{TechStatus: engine.StatusFailed})

	if !result.Retried {
		t.Fatalf("a plain technical failure must still consult the node's retry policy (%s)", result.Diagnostic)
	}
	if result.RetryRefused != "" {
		t.Errorf("RetryRefused = %q, want empty: the fence must not spread beyond timed_out", result.RetryRefused)
	}
}
