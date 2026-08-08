package engine_test

import (
	"context"
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// TestRunSurvivesAProcessRestart is PRD §23's Phase-1 exit criterion "process
// restart preserves the run", and §20.1's "committed orchestration state is
// durable", proved the only way that means anything: by throwing the process
// away mid-run.
//
// The restart here is real, not simulated by re-reading through the same
// handle. A dedicated connection pool is opened, half the run is driven
// through it, the pool is closed — taking with it the engine, its compiled
// workflow cache, and every in-memory trace of the run — and a second pool
// and a second engine finish the same run from nothing but what was
// committed. Nothing is handed across the boundary except the run's id.
func TestRunSurvivesAProcessRestart(t *testing.T) {
	shared := pgtest.RequireStore(t, testStore)
	connString := shared.Pool().Config().ConnString()
	ctx := context.Background()

	// --- first process ---
	before, err := storepg.Connect(ctx, connString)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	f := newFixtureOn(t, before, "loop.workflow.yaml")

	run := f.createRun(`{"subject":"survive a restart"}`)
	intake := f.readyNodeRun(run.ID)
	toWork := f.step("worker-a", intake.ID, succeeded("completed", `{"scope":"s"}`))
	toCheck := f.step("worker-a", toWork.NextNodeRunID, succeeded("completed", `{"revision":1}`))
	loop := f.step("worker-a", toCheck.NextNodeRunID, succeeded("changes_required", `{"give_up":false}`))
	if loop.NextNodeID != "work" {
		t.Fatalf("expected the loop back to work, got %q", loop.NextNodeID)
	}

	runID := run.ID
	pendingNodeRun := loop.NextNodeRunID
	before.Close()

	// --- second process ---
	after, err := storepg.Connect(ctx, connString)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer after.Close()
	restarted := f.rebind(t, after)

	// The run is exactly where it was left: running, at its second visit to
	// work, with the pinned definition still resolvable.
	resumed := restarted.run(runID)
	if resumed.State != engine.RunRunning {
		t.Fatalf("run state after restart = %s, want running", resumed.State)
	}
	if resumed.WorkflowDigest != f.cw.Digest {
		t.Errorf("pinned digest after restart = %q, want %q", resumed.WorkflowDigest, f.cw.Digest)
	}
	waiting := restarted.readyNodeRun(runID)
	if waiting.ID != pendingNodeRun {
		t.Fatalf("ready node run after restart = %s, want %s", waiting.ID, pendingNodeRun)
	}
	if waiting.NodeID != "work" || waiting.VisitCount != 2 {
		t.Errorf("ready node run is %s visit %d, want work visit 2", waiting.NodeID, waiting.VisitCount)
	}

	// And the new process can finish the run, which is the real test: the
	// engine reloaded the definition, the transition and visit counts, and
	// the token position from the database alone.
	toCheckAgain := restarted.step("worker-b", waiting.ID, succeeded("completed", `{"revision":2}`))
	if toCheckAgain.NextNodeID != "check" {
		t.Fatalf("routed to %q, want check", toCheckAgain.NextNodeID)
	}
	done := restarted.step("worker-b", toCheckAgain.NextNodeRunID,
		succeeded("done", `{"summary":"finished after a restart","give_up":false}`))

	if done.RunState != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (%s)", done.RunState, done.Diagnostic)
	}
	equalJSON(t, done.RunOutput, `{"summary":"finished after a restart","give_up":false}`)
	if got, want := done.Transitions, 5; got != want {
		t.Errorf("transitions = %d, want %d — the count survived the restart", got, want)
	}

	// The audit trail spans both processes as one unbroken sequence.
	types := restarted.eventTypes(runID)
	if types[len(types)-1] != engine.TypeRunCompleted {
		t.Errorf("last event = %s, want %s", types[len(types)-1], engine.TypeRunCompleted)
	}
	// One run.created, five node-run.ready, five attempt.completed, five
	// token.transitioned, one run.completed — the same trail the
	// single-process run leaves, minus the ledger record that run appends.
	if len(types) != 17 {
		t.Errorf("recorded %d events across the restart, want 17: %v", len(types), types)
	}
}
