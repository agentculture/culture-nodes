package worker_test

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Task t8, deviation d1: the original plan assumed approval work reached the
// worker through a registered HumanDispatcher seam. Task t6 shipped
// something different — the engine parks an approval node's token by
// inserting a human_tasks row directly, inside the very same transaction
// that creates the node run, and never calls EnqueueWork for it (see
// internal/engine/humantask.go and its doc.go). That means no work_items row
// is *ever* created for an approval node run in the real system, which in
// turn means the worker's claim loop cannot observe approval work at all —
// not "declines to act on it", but structurally has nothing to claim.
//
// The two tests below are this task's contract:
//
//  1. TestApprovalNodeNeverEnqueuesWorkAndClaimLoopNeverObservesIt proves the
//     invariant itself, end to end through the real worker loop exactly like
//     this package's other integration tests (newHarness: real PostgreSQL,
//     real engine, real HTTP actor, real Worker.Tick).
//  2. TestApprovalWorkItemThatSomehowExistsIsRefusedNotProcessed proves the
//     defensive side: if that invariant were ever violated — a bug, a bad
//     migration, a future engine change that regresses it — an approval-kind
//     work item reaching the worker is refused with a recorded, diagnosable
//     technical failure, never silently dropped and never treated as an
//     implicit approval. Nothing in this file, seams.go, or doc.go registers
//     a HumanDispatcher; the refusal is the worker's default behaviour for
//     any kind whose seam is unregistered (see dispatchSeam's errNoSeam path
//     in dispatch.go), and this test exists to pin that behaviour
//     specifically for the human-kind case the re-scoped task cares about.

// pollReviewNodeRun looks up review's node run without failing the test when
// it does not exist yet — unlike harness.nodeRunStatus (hooks_test.go),
// which is meant to be called only after the row is known to exist. Before
// intake completes there is no node_runs row for review at all, so a runUntil
// predicate needs a form that tolerates "not there yet".
func pollReviewNodeRun(h *harness, runID string) (id, status string, ok bool) {
	err := h.store.Pool().QueryRow(h.ctx,
		`SELECT id, status FROM node_runs WHERE run_id = $1 AND node_key = 'review'`, runID,
	).Scan(&id, &status)
	if err != nil {
		return "", "", false
	}
	return id, status, true
}

func TestApprovalNodeNeverEnqueuesWorkAndClaimLoopNeverObservesIt(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		writeSyncResult(w, "completed", `{"scope":"s"}`)
	})

	run := h.createRun("approval.workflow.yaml", `{"subject":"ship it"}`)

	// Drive intake (an ordinary agent node) to completion through the real
	// worker loop — h.runUntil ticks h.worker, the same claim path this test
	// is about — until the token has landed on review and the engine has
	// parked it.
	var reviewID string
	h.runUntil(20*time.Second, func() bool {
		id, status, ok := pollReviewNodeRun(h, run.ID)
		if ok && status == string(engine.NodeRunWaitingHuman) {
			reviewID = id
			return true
		}
		return false
	})
	if reviewID == "" {
		t.Fatalf("review node run never reached waiting_human (worker errors: %v)", h.workerErrors())
	}

	// The invariant itself: dispatching review wrote a human_tasks row, and
	// nothing ever wrote a work_items row for it.
	var workItems int
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT COUNT(*)::int FROM work_items WHERE node_run_id = $1`, reviewID,
	).Scan(&workItems); err != nil {
		t.Fatalf("count work_items for review: %v", err)
	}
	if workItems != 0 {
		t.Fatalf("%d work_items rows exist for the approval node run, want 0 — "+
			"engine-side parking (task t6) must never enqueue work for an approval node", workItems)
	}

	var humanTasks int
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT COUNT(*)::int FROM human_tasks WHERE node_run_id = $1`, reviewID,
	).Scan(&humanTasks); err != nil {
		t.Fatalf("count human_tasks for review: %v", err)
	}
	if humanTasks != 1 {
		t.Fatalf("%d human_tasks rows exist for review, want exactly 1", humanTasks)
	}

	// The claim loop does not merely decline to act — it has nothing to
	// claim. Several ticks across a real poll window claim zero items, and
	// review's node run is left completely untouched.
	for i := 0; i < 5; i++ {
		dispatched, err := h.worker.Tick(h.ctx)
		if err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
		if dispatched != 0 {
			t.Fatalf("tick %d claimed %d work item(s); the claim loop must never observe approval work", i, dispatched)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, status, ok := pollReviewNodeRun(h, run.ID); !ok || status != string(engine.NodeRunWaitingHuman) {
		t.Fatalf("review node run status = %q (found=%v), want still waiting_human — untouched by the worker", status, ok)
	}
	if state := h.run(run.ID).State; state != engine.RunRunning {
		t.Fatalf("run state = %s, want running: an approval pause is not a failure", state)
	}
}

// TestApprovalWorkItemThatSomehowExistsIsRefusedNotProcessed manufactures the
// one state the test above proves the engine never produces — a work_items
// row for an approval node run — by calling Store.EnqueueWork directly,
// bypassing the engine exactly the way a hypothetical future bug would. It
// then asserts the worker refuses the item loudly rather than silently
// dropping it or completing it as if a human had answered.
func TestApprovalWorkItemThatSomehowExistsIsRefusedNotProcessed(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		writeSyncResult(w, "completed", `{"scope":"s"}`)
	})

	run := h.createRun("approval.workflow.yaml", `{"subject":"ship it"}`)

	var reviewID string
	h.runUntil(20*time.Second, func() bool {
		id, status, ok := pollReviewNodeRun(h, run.ID)
		if ok && status == string(engine.NodeRunWaitingHuman) {
			reviewID = id
			return true
		}
		return false
	})
	if reviewID == "" {
		t.Fatalf("review node run never reached waiting_human (worker errors: %v)", h.workerErrors())
	}

	// dispatchNode (internal/engine/humantask.go) never does this for an
	// approval node — it calls InsertHumanTask, not EnqueueWork. This call is
	// the test manufacturing the violation on purpose, standing in for
	// whatever bug or migration mistake might one day produce it, to prove
	// the worker's defensive path actually fires. No HumanDispatcher is
	// registered on this harness's worker (its default), which is the
	// production-honest state per seams.go's updated documentation.
	if err := h.store.EnqueueWork(h.ctx, storepg.WorkItem{
		NamespaceID: h.ns.ID,
		NodeRunID:   reviewID,
	}); err != nil {
		t.Fatalf("manufacture an out-of-band work item for the approval node run: %v", err)
	}

	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunFailed {
		t.Fatalf("run state = %s, want failed: an unexpected human-kind work item must be refused, not silently processed", state)
	}

	var result []byte
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT a.result FROM attempts AS a
		WHERE a.node_run_id = $1
		ORDER BY a.attempt_number DESC LIMIT 1
	`, reviewID).Scan(&result); err != nil {
		t.Fatalf("read review's attempt: %v", err)
	}
	if !bytes.Contains(result, []byte("not_implemented")) {
		t.Errorf("attempt result = %s, want a not_implemented diagnostic", result)
	}
	if !bytes.Contains(result, []byte("human-task")) {
		t.Errorf("attempt result = %s, want the missing capability named", result)
	}
}
