package worker_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// The dispatch budget (issue #16, spec claims c4/c19, honesty conditions
// h4/h14/h19).
//
// The live failure these tests encode: a work item whose dispatch never
// commits a completion is re-leased every time its lease expires, and each
// re-lease is a fresh, billable actor session. Production reached
// work_items.attempt = 22 that way. Nothing in the claim path capped it.
//
// The tests below drive the real loop against a real PostgreSQL and a real
// HTTP actor, and force only one thing: the work item's attempt counter,
// standing in for the reclaim cycles that would otherwise take 22 lease
// expiries of wall-clock time to reproduce.

// acceptAsync is an actor handler that answers §13.3 with an acceptance and
// then never reports -- the shape of every dispatch in the live incident.
func acceptAsync(invocationID string) func(*harness, http.ResponseWriter, actors.InvocationRequest) {
	return func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"invocation_id":"` + invocationID + `","heartbeat_after_seconds":30,"supports_cancellation":true}`))
	}
}

// workItemOf returns the id of the (single) work item belonging to a run's
// named node.
func workItemOf(t *testing.T, h *harness, runID, nodeKey string) string {
	t.Helper()
	var workID string
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT wi.id FROM work_items AS wi
		JOIN node_runs AS nr ON nr.id = wi.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = $2
	`, runID, nodeKey).Scan(&workID); err != nil {
		t.Fatalf("read work item for node %q: %v", nodeKey, err)
	}
	return workID
}

// burnBudget puts a parked work item back in the ready queue with `attempt`
// already spent, which is exactly the state the reclaim loop leaves behind
// after that many lease expiries -- see claiming.go (only a claim increments
// the counter) and internal/store/postgres/claiming_test.go's
// TestWaitingWorkAccruesNoAttempts.
func burnBudget(t *testing.T, h *harness, workID string, attempt int) {
	t.Helper()
	if _, err := h.store.Pool().Exec(h.ctx, `
		UPDATE work_items
		SET state            = 'ready',
		    lease_owner      = NULL,
		    lease_expires_at = NULL,
		    available_at     = now(),
		    attempt          = $2,
		    state_version    = state_version + 1,
		    updated_at       = now()
		WHERE id = $1
	`, workID, attempt); err != nil {
		t.Fatalf("burnBudget: %v", err)
	}
}

// attemptRecord reads the recorded attempt for a run's named node.
func attemptRecord(t *testing.T, h *harness, runID, nodeKey string) (status string, result []byte) {
	t.Helper()
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT a.status, a.result
		FROM attempts AS a JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = $2
		ORDER BY a.attempt_number DESC
		LIMIT 1
	`, runID, nodeKey).Scan(&status, &result); err != nil {
		t.Fatalf("read attempt for node %q: %v", nodeKey, err)
	}
	return status, result
}

// parkAsync drives the worker until the run's entry node is parked on an
// asynchronous acceptance, and returns its work item id.
func parkAsync(t *testing.T, h *harness, runID string) string {
	t.Helper()
	h.runUntil(20*time.Second, func() bool { return h.workItemStates(runID)["work"] == "waiting" })
	return workItemOf(t, h, runID, "work")
}

// TestActorDispatchStopsAtTheRetryBudget is issue #16's regression: the
// dispatch after the budget is spent must not happen at all. The work item is
// claimed (a claim is how the worker learns about it) but never dispatched:
// the attempt is completed as technically failed with the exhaustion cause
// recorded, through the ordinary CompleteAttempt path.
func TestActorDispatchStopsAtTheRetryBudget(t *testing.T) {
	h := newHarness(t, acceptAsync("external_zombie"))

	run := h.createRun("async.workflow.yaml", `{"subject":"slow"}`)
	workID := parkAsync(t, h, run.ID)

	if got := len(h.invocations()); got != 1 {
		t.Fatalf("actor was invoked %d times before the budget was spent, want 1", got)
	}
	burnBudget(t, h, workID, worker.MaxDispatchAttempts)

	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if got := len(h.invocations()); got != 1 {
		t.Errorf("actor was invoked %d times in total, want 1: dispatch %d must never happen",
			got, worker.MaxDispatchAttempts+1)
	}

	status, result := attemptRecord(t, h, run.ID, "work")
	if engine.TechStatus(status) != engine.StatusFailed {
		t.Errorf("attempt status = %q, want %q", status, engine.StatusFailed)
	}
	if !bytes.Contains(result, []byte("dispatch_budget_exhausted")) {
		t.Errorf("attempt result = %s, want the exhaustion cause recorded", result)
	}

	// async.workflow.yaml declares no edge from `failed`, so the run ends
	// failed rather than hanging.
	if state := h.run(run.ID).State; state != engine.RunFailed {
		t.Errorf("run state = %s, want failed (worker errors: %v)", state, h.workerErrors())
	}
	if state := h.workItemStates(run.ID)["work"]; state != "completed" {
		t.Errorf("work item state = %q, want %q: an exhausted item must leave the dispatch loop", state, "completed")
	}
}

// TestBudgetExhaustionCancelsThePendingInvocation is spec claim c19: parking
// the node while its session is still running would let that session finish
// into a callback nobody can use -- quota spent on a discarded result. The
// cancel is best-effort and issued alongside the parking completion.
func TestBudgetExhaustionCancelsThePendingInvocation(t *testing.T) {
	h := newHarness(t, acceptAsync("external_zombie"))

	run := h.createRun("async.workflow.yaml", `{"subject":"slow"}`)
	workID := parkAsync(t, h, run.ID)
	burnBudget(t, h, workID, worker.MaxDispatchAttempts)

	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	cancels := h.cancellations()
	if len(cancels) != 1 {
		t.Fatalf("actor received %d cancellations, want 1 (worker errors: %v)", len(cancels), h.workerErrors())
	}
	if cancels[0].InvocationID != "external_zombie" {
		t.Errorf("cancelled invocation = %q, want the pending one (%q)", cancels[0].InvocationID, "external_zombie")
	}
	if cancels[0].Reason == "" {
		t.Error("cancellation carried no reason; an operator reading the bridge log should see why the session was stopped")
	}
}

// TestBudgetExhaustionParksEvenWhenTheCancelFails proves the cancel is
// genuinely best-effort: an actor that refuses (or cannot answer)
// cancellation must not keep the item in the dispatch loop, which would
// reinstate the very loop the budget exists to stop.
func TestBudgetExhaustionParksEvenWhenTheCancelFails(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"invocation_id":"external_stubborn","heartbeat_after_seconds":30}`))
	})

	run := h.createRun("async.workflow.yaml", `{"subject":"slow"}`)
	workID := parkAsync(t, h, run.ID)

	// Make the cancel undeliverable: the invocation names an actor the
	// registry cannot resolve, so the worker has no address to cancel at.
	if _, err := h.store.Pool().Exec(h.ctx,
		`UPDATE actor_invocations SET actor_ref = 'actor://company/vanished@sha256:cccccc' WHERE work_id = $1`, workID,
	); err != nil {
		t.Fatalf("rewrite actor_ref: %v", err)
	}
	burnBudget(t, h, workID, worker.MaxDispatchAttempts)

	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if len(h.cancellations()) != 0 {
		t.Errorf("actor received %d cancellations, want 0 (the reference does not resolve)", len(h.cancellations()))
	}
	status, result := attemptRecord(t, h, run.ID, "work")
	if engine.TechStatus(status) != engine.StatusFailed {
		t.Errorf("attempt status = %q, want %q even though the cancel could not be delivered", status, engine.StatusFailed)
	}
	if !bytes.Contains(result, []byte("dispatch_budget_exhausted")) {
		t.Errorf("attempt result = %s, want the exhaustion cause recorded", result)
	}

	// Best-effort is not silent: the undeliverable cancellation is reported,
	// so an operator can see that a session may still be running somewhere.
	reported := false
	for _, err := range h.workerErrors() {
		if strings.Contains(err.Error(), "cancel") {
			reported = true
		}
	}
	if !reported {
		t.Errorf("worker errors = %v, want the failed cancellation reported", h.workerErrors())
	}
}

// TestBudgetExhaustionRoutesADeclaredFailureEdge is honesty condition h4's
// other half: exhaustion is a technical failure, and a workflow that declares
// an edge from `failed` keeps routing -- exhaustion is not a special state
// bolted onto the engine.
func TestBudgetExhaustionRoutesADeclaredFailureEdge(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, req actors.InvocationRequest) {
		if req.Node.ID == "repair" {
			writeSyncResult(w, "completed", `{"summary":"repaired"}`)
			return
		}
		acceptAsync("external_zombie")(nil, w, req)
	})

	run := h.createRun("async-failure-edge.workflow.yaml", `{"subject":"slow"}`)
	workID := parkAsync(t, h, run.ID)
	burnBudget(t, h, workID, worker.MaxDispatchAttempts)

	h.runUntil(30*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed via the repair path (worker errors: %v)", state, h.workerErrors())
	}
	var workState string
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT status FROM node_runs WHERE run_id = $1 AND node_key = 'work'`, run.ID,
	).Scan(&workState); err != nil {
		t.Fatalf("read node run: %v", err)
	}
	if engine.NodeRunState(workState) != engine.NodeRunFailed {
		t.Errorf("node run `work` state = %q, want failed", workState)
	}
	if _, result := attemptRecord(t, h, run.ID, "work"); !bytes.Contains(result, []byte("dispatch_budget_exhausted")) {
		t.Errorf("attempt result = %s, want the exhaustion cause recorded", result)
	}
}

// TestDispatchProceedsOnTheLastAttemptWithinBudget fixes the boundary: the
// budget is three dispatches, not two. A test asserting only the refusal
// would pass against a worker that refused everything.
func TestDispatchProceedsOnTheLastAttemptWithinBudget(t *testing.T) {
	h := newHarness(t, acceptAsync("external_zombie"))

	run := h.createRun("async.workflow.yaml", `{"subject":"slow"}`)
	workID := parkAsync(t, h, run.ID)
	burnBudget(t, h, workID, worker.MaxDispatchAttempts-1)

	h.runUntil(20*time.Second, func() bool { return len(h.invocations()) > 1 })

	invocations := h.invocations()
	if len(invocations) != 2 {
		t.Fatalf("actor was invoked %d times, want 2 (the last dispatch within budget)", len(invocations))
	}
	if invocations[1].Attempt != worker.MaxDispatchAttempts {
		t.Errorf("second invocation carried attempt %d, want %d", invocations[1].Attempt, worker.MaxDispatchAttempts)
	}
	if len(h.cancellations()) != 0 {
		t.Errorf("actor received %d cancellations, want 0 while the budget still had room", len(h.cancellations()))
	}
}

// TestMaxDispatchAttemptsIsTheRecordedDecision pins the budget itself. It is
// a recorded product decision (three dispatch attempts, then park), not a
// tuning knob a refactor may drift.
func TestMaxDispatchAttemptsIsTheRecordedDecision(t *testing.T) {
	if worker.MaxDispatchAttempts != 3 {
		t.Errorf("worker.MaxDispatchAttempts = %d, want 3 (the recorded decision behind spec claim c4)",
			worker.MaxDispatchAttempts)
	}
}
