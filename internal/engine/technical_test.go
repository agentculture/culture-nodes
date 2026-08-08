package engine_test

import (
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
)

// A technical status may originate an edge (PRD §3.4). When it does, the node
// run still *fails* — the attempt produced no domain answer — but the token
// keeps moving, which is how a workflow sends a timeout to a repair node
// instead of ending the run.
func TestTechnicalStatusFollowsItsOwnEdge(t *testing.T) {
	f := newFixture(t, "technical-route.workflow.yaml")
	run := f.createRun(`{"subject":"timeout"}`)

	build := f.readyNodeRun(run.ID)
	result := f.step("worker-a", build.ID, engine.CompletionRequest{TechStatus: engine.StatusTimedOut})

	if result.Retried {
		t.Fatal("build declares maxAttempts 1; there was no retry to schedule")
	}
	if result.NextNodeID != "repair" {
		t.Fatalf("build.timed_out routed to %q, want repair", result.NextNodeID)
	}
	if result.EdgeFrom != "build.timed_out" {
		t.Errorf("followed edge %q, want build.timed_out", result.EdgeFrom)
	}
	if result.NodeRunState != engine.NodeRunFailed {
		t.Errorf("node run state = %s, want failed — a routed timeout is still a failed attempt", result.NodeRunState)
	}
	if result.RunState != engine.RunRunning {
		t.Errorf("run state = %s, want running — the run was not the thing that failed", result.RunState)
	}
	if !contains(result.EventTypes, engine.TypeNodeRunFailed) {
		t.Errorf("events = %v, want one of type %s", result.EventTypes, engine.TypeNodeRunFailed)
	}

	// The repair node finishes the run normally.
	done := f.step("worker-a", result.NextNodeRunID, succeeded("completed", `{"repaired":true}`))
	if done.RunState != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (%s)", done.RunState, done.Diagnostic)
	}
	equalJSON(t, done.RunOutput, `{"repaired":true}`)
}

// Cancellation is an instruction, not a fault: it is never retried, never
// routed, and it ends the run as cancelled rather than failed.
func TestCancellationEndsTheRunWithoutRetryOrRouting(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")
	runID, workNodeRun := advanceToWork(t, f, "cancelled")

	result := f.step("worker-a", workNodeRun, engine.CompletionRequest{TechStatus: engine.StatusCancelled})

	if result.Retried {
		t.Error("work has a retry budget, but a cancellation must not consume it")
	}
	if result.RunState != engine.RunCancelled {
		t.Fatalf("run state = %s, want cancelled", result.RunState)
	}
	if result.NodeRunState != engine.NodeRunCancelled {
		t.Errorf("node run state = %s, want cancelled", result.NodeRunState)
	}
	if got := f.run(runID).State; got != engine.RunCancelled {
		t.Errorf("stored run state = %s, want cancelled", got)
	}

	types := f.eventTypes(runID)
	if types[len(types)-1] != engine.TypeRunCancelled {
		t.Errorf("last event = %s, want %s", types[len(types)-1], engine.TypeRunCancelled)
	}
}
