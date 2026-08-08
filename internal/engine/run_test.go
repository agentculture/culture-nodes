package engine_test

import (
	"encoding/json"
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// TestRunWalksTheLoopAndCompletes drives the four-node slice end to end with a
// hand-operated worker: intake -> work -> check, once around the
// changes_required loop, then check.done -> finish.
//
// It is the PRD §23 Phase-1 exit criterion "`changes_required` loops to build"
// stated as a test, and it asserts the things that make that claim mean
// something: the loop edge was walked exactly the scripted number of times,
// the run's result came from the end node's output binding, and the audit
// trail is one strictly ordered sequence of the transitions that actually
// committed.
func TestRunWalksTheLoopAndCompletes(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")

	run := f.createRun(`{"subject":"ship the thing"}`)
	if run.State != engine.RunRunning {
		t.Fatalf("new run state = %s, want running", run.State)
	}
	if run.WorkflowDigest != f.cw.Digest {
		t.Errorf("run pinned digest %q, want %q", run.WorkflowDigest, f.cw.Digest)
	}

	intake := f.readyNodeRun(run.ID)
	if intake.NodeID != "intake" {
		t.Fatalf("entry node run is at %q, want intake", intake.NodeID)
	}

	// Step 1: intake succeeds and proposes one ledger claim.
	first := f.step("worker-a", intake.ID, engine.CompletionRequest{
		TechStatus: engine.StatusSucceeded,
		Outcome:    "completed",
		Output:     json.RawMessage(`{"scope":"ship the thing"}`),
		ActorID:    f.actor,
		LedgerDelta: []ledger.Record{{
			RecordType: ledger.RecordClaim,
			Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: f.actor},
			Data:       json.RawMessage(`{"statement":"scope is understood"}`),
		}},
	})
	if first.NextNodeID != "work" {
		t.Fatalf("intake.completed routed to %q, want work", first.NextNodeID)
	}
	if len(first.LedgerRecords) != 1 {
		t.Fatalf("appended %d ledger records, want 1", len(first.LedgerRecords))
	}
	// An agent's record is a proposal, whatever it asked for.
	if got := first.LedgerRecords[0].Authority; got != ledger.AuthorityProposed {
		t.Errorf("ledger authority = %s, want proposed", got)
	}
	if got := first.LedgerRecords[0].AttemptID.String(); got != first.AttemptID {
		t.Errorf("ledger record attempt = %q, want the attempt that produced it (%q)", got, first.AttemptID)
	}

	// Step 2: work succeeds.
	second := f.step("worker-a", first.NextNodeRunID, succeeded("completed", `{"revision":1}`))
	if second.NextNodeID != "check" {
		t.Fatalf("work.completed routed to %q, want check", second.NextNodeID)
	}

	// Step 3: the checker asks for changes — a domain outcome, not a failure —
	// and the token goes back to work.
	loop := f.step("worker-b", second.NextNodeRunID, succeeded("changes_required", `{"give_up":false,"summary":"needs work"}`))
	if loop.TechStatus != engine.StatusSucceeded {
		t.Errorf("changes_required was recorded as %s; it is a domain outcome of a succeeded attempt", loop.TechStatus)
	}
	if loop.NextNodeID != "work" {
		t.Fatalf("check.changes_required routed to %q, want work", loop.NextNodeID)
	}
	if loop.EdgeFrom != "check.changes_required" {
		t.Errorf("followed edge %q, want check.changes_required", loop.EdgeFrom)
	}

	// Step 4: work runs a second time.
	rework := f.step("worker-a", loop.NextNodeRunID, succeeded("completed", `{"revision":2}`))
	if rework.NextNodeID != "check" {
		t.Fatalf("work.completed routed to %q, want check", rework.NextNodeID)
	}

	// Step 5: the checker is satisfied and the run ends at the end node.
	done := f.step("worker-b", rework.NextNodeRunID,
		succeeded("done", `{"summary":"looks good","give_up":false}`))
	if done.RunState != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (%s)", done.RunState, done.Diagnostic)
	}
	equalJSON(t, done.RunOutput, `{"summary":"looks good","give_up":false}`)

	// The loop was walked exactly once: work has two node runs, check has two.
	if got := f.nodeRunCount(run.ID, "work"); got != 2 {
		t.Errorf("work ran %d times, want 2", got)
	}
	if got := f.nodeRunCount(run.ID, "check"); got != 2 {
		t.Errorf("check ran %d times, want 2", got)
	}
	if got := f.nodeRunCount(run.ID, "intake"); got != 1 {
		t.Errorf("intake ran %d times, want 1", got)
	}
	if got, want := done.Transitions, 5; got != want {
		t.Errorf("transitions = %d, want %d", got, want)
	}

	// The run is durable and carries its result.
	stored := f.run(run.ID)
	if stored.State != engine.RunCompleted {
		t.Errorf("stored run state = %s, want completed", stored.State)
	}
	equalJSON(t, stored.Output, `{"summary":"looks good","give_up":false}`)

	// Every committed transition left exactly one audit event, in order.
	equalStrings(t, f.eventTypes(run.ID), []string{
		engine.TypeRunCreated,
		engine.TypeNodeRunReady, // intake
		engine.TypeAttemptCompleted,
		engine.TypeLedgerAppended,
		engine.TypeTokenTransitioned, // intake -> work
		engine.TypeNodeRunReady,
		engine.TypeAttemptCompleted,
		engine.TypeTokenTransitioned, // work -> check
		engine.TypeNodeRunReady,
		engine.TypeAttemptCompleted,
		engine.TypeTokenTransitioned, // check -> work (the loop)
		engine.TypeNodeRunReady,
		engine.TypeAttemptCompleted,
		engine.TypeTokenTransitioned, // work -> check
		engine.TypeNodeRunReady,
		engine.TypeAttemptCompleted,
		engine.TypeTokenTransitioned, // check -> finish
		engine.TypeRunCompleted,
	}, "event types")

	// Every event also produced an outbox row, so the queue signal and the
	// audit trail cannot disagree (§12.5 step 10).
	events := f.countScalar(`SELECT COUNT(*)::int FROM events WHERE aggregate_id = $1`, run.ID)
	outbox := f.countScalar(`SELECT COUNT(*)::int FROM outbox WHERE namespace_id = $1`, f.ns.ID)
	if events != outbox {
		t.Errorf("%d events but %d outbox rows", events, outbox)
	}

	// The ledger record is bound to the run, node run, and attempt that
	// produced it.
	ledgerRows := f.countScalar(`SELECT COUNT(*)::int FROM ledger_records WHERE run_id = $1`, run.ID)
	if ledgerRows != 1 {
		t.Errorf("%d ledger records for the run, want 1", ledgerRows)
	}

	// Exactly one token is created per position, and every one is consumed by
	// the time the run ends.
	if active := f.countScalar(
		`SELECT COUNT(*)::int FROM tokens WHERE run_id = $1 AND state = 'active'`, run.ID); active != 0 {
		t.Errorf("%d tokens still active after the run completed", active)
	}
	if tokens := f.countScalar(`SELECT COUNT(*)::int FROM tokens WHERE run_id = $1`, run.ID); tokens != 6 {
		t.Errorf("%d tokens recorded, want 6 (one per node run)", tokens)
	}
}

// The guard on check.changes_required -> finish wins over the unguarded
// fallback beside it, which is what first-match-wins in normalized edge order
// buys the author: a way to leave the loop without a new outcome.
func TestGuardedEdgeLeavesTheLoop(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")
	run := f.createRun(`{"subject":"give up early"}`)

	intake := f.readyNodeRun(run.ID)
	toWork := f.step("worker-a", intake.ID, succeeded("completed", `{"scope":"s"}`))
	toCheck := f.step("worker-a", toWork.NextNodeRunID, succeeded("completed", `{"revision":1}`))

	result := f.step("worker-a", toCheck.NextNodeRunID,
		succeeded("changes_required", `{"give_up":true,"summary":"out of budget"}`))

	if result.NextNodeID != "finish" {
		t.Fatalf("a checker that gave up routed to %q, want finish", result.NextNodeID)
	}
	if result.RunState != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (%s)", result.RunState, result.Diagnostic)
	}
	if got := f.nodeRunCount(run.ID, "work"); got != 1 {
		t.Errorf("work ran %d times, want 1 — the loop should not have been walked", got)
	}
}

// A deliberately unbounded run — the checker never says done — is stopped by
// the engine, not by the agent (PRD §9.7). The fixture lowers maxTransitions
// to 4 so the bound is reached in a few steps.
func TestLoopBoundStopsAnUnboundedRun(t *testing.T) {
	f := newFixture(t, "unbounded.workflow.yaml")
	run := f.createRun(`{"subject":"never satisfied"}`)

	nodeRunID := f.readyNodeRun(run.ID).ID
	result := f.step("worker-a", nodeRunID, succeeded("completed", `{"scope":"s"}`))

	// Keep answering: work completes, the checker always asks for changes.
	// The loop below cannot run away — if the bound did not fire, the step
	// budget fails the test rather than hanging.
	const budget = 20
	var stopped engine.CompletionResult
	for step := 0; step < budget; step++ {
		if result.RunState != engine.RunRunning {
			stopped = result
			break
		}
		req := succeeded("completed", `{"revision":1}`)
		if result.NextNodeID == "check" {
			req = succeeded("changes_required", `{"give_up":false}`)
		}
		result = f.step("worker-a", result.NextNodeRunID, req)
	}

	if stopped.Bound == nil {
		t.Fatalf("the run was not stopped by a bound: state=%s diagnostic=%q", result.RunState, result.Diagnostic)
	}
	if stopped.Bound.Kind != engine.BoundTransitions {
		t.Errorf("bound = %s, want %s", stopped.Bound.Kind, engine.BoundTransitions)
	}
	if stopped.RunState != engine.RunFailed {
		t.Errorf("run state = %s, want failed", stopped.RunState)
	}
	if got := f.run(run.ID).State; got != engine.RunFailed {
		t.Errorf("stored run state = %s, want failed", got)
	}

	// The bound is its own event type, so "this workflow is looping" is
	// readable without parsing a failure message.
	types := f.eventTypes(run.ID)
	if types[len(types)-1] != engine.TypeRunBounded {
		t.Errorf("last event = %s, want %s", types[len(types)-1], engine.TypeRunBounded)
	}

	// And the bound held: the run never took more transitions than declared.
	transitions := f.countScalar(
		`SELECT GREATEST(COUNT(*) - 1, 0)::int FROM node_runs WHERE run_id = $1`, run.ID)
	if transitions > 4 {
		t.Errorf("run took %d transitions with maxTransitions 4", transitions)
	}
}
