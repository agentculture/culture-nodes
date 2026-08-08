package engine_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The three properties in this file are the engine-level statements of PRD
// §20.1's guarantees: a committed transition is final, a fenced write from a
// stale claim never lands, and every committed transition leaves exactly one
// audit event in one strictly increasing per-run sequence.

// Property: a terminal node run never transitions again.
//
// Two ways to try. The first is the ordinary one — re-report the same claim —
// and fencing catches it, because completing a node run also completes its
// work item. The second is the one fencing *cannot* catch: a brand-new,
// perfectly valid claim against a node run that is already finished. That is
// what the typed terminal error is for.
func TestTerminalNodeRunNeverTransitionsAgain(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")
	run := f.createRun(`{"subject":"terminal"}`)

	intake := f.readyNodeRun(run.ID)
	claimed := f.claim("worker-a", intake.ID)
	first, err := f.engine.CompleteAttempt(f.ctx, completion(claimed, "worker-a", succeeded("completed", `{"scope":"s"}`)))
	if err != nil {
		t.Fatalf("first completion: %v", err)
	}
	if first.NodeRunState != engine.NodeRunCompleted {
		t.Fatalf("node run state = %s, want completed", first.NodeRunState)
	}

	before := snapshot(f, run.ID, intake.ID)

	// 1. The same claim, reported twice.
	_, err = f.engine.CompleteAttempt(f.ctx, completion(claimed, "worker-a", succeeded("completed", `{"scope":"again"}`)))
	if !errors.Is(err, engine.ErrStaleClaim) {
		t.Fatalf("re-reporting a completed claim: err = %v, want ErrStaleClaim", err)
	}

	// 2. A fresh, valid claim against the same — now terminal — node run.
	if err := f.store.EnqueueWork(f.ctx, storepg.WorkItem{NamespaceID: f.ns.ID, NodeRunID: intake.ID}); err != nil {
		t.Fatalf("EnqueueWork: %v", err)
	}
	reclaimed := f.claim("worker-c", intake.ID)
	_, err = f.engine.CompleteAttempt(f.ctx, completion(reclaimed, "worker-c", succeeded("completed", `{"scope":"third"}`)))
	if !errors.Is(err, engine.ErrTerminalNodeRun) {
		t.Fatalf("completing a terminal node run: err = %v, want ErrTerminalNodeRun", err)
	}

	if after := snapshot(f, run.ID, intake.ID); after != before {
		t.Errorf("state changed after refused completions:\n got %+v\nwant %+v", after, before)
	}
}

// Property: an old fencing token never commits.
//
// The claiming layer guarantees it (task t7); this proves the guarantee holds
// through the engine, where a stale completion must also leave no attempt, no
// ledger record, and no event behind — a partially applied stale completion
// would be worse than a rejected one.
func TestStaleFencingTokenCommitsNothing(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")
	run := f.createRun(`{"subject":"fencing"}`)

	intake := f.readyNodeRun(run.ID)
	stale := f.claim("worker-a", intake.ID)

	// worker-a's lease expires and worker-b takes the work: a strictly higher
	// fencing token, per §12.4.
	f.expire(stale.ID)
	fresh := f.claim("worker-b", intake.ID)
	if fresh.FencingToken <= stale.FencingToken {
		t.Fatalf("reclaim produced fencing token %d, want > %d", fresh.FencingToken, stale.FencingToken)
	}

	before := snapshot(f, run.ID, intake.ID)

	_, err := f.engine.CompleteAttempt(f.ctx, completion(stale, "worker-a", succeeded("completed", `{"scope":"late"}`)))
	if !errors.Is(err, engine.ErrStaleClaim) {
		t.Fatalf("late worker: err = %v, want ErrStaleClaim", err)
	}
	// The store's own sentinel is wrapped too, so a caller on either side of
	// the interface can match the one it knows.
	if !errors.Is(err, storepg.ErrStaleClaim) {
		t.Errorf("late worker: err = %v, want it to wrap postgres.ErrStaleClaim as well", err)
	}

	if after := snapshot(f, run.ID, intake.ID); after != before {
		t.Errorf("a stale completion changed state:\n got %+v\nwant %+v", after, before)
	}

	// The current lease holder still commits normally.
	result, err := f.engine.CompleteAttempt(f.ctx, completion(fresh, "worker-b", succeeded("completed", `{"scope":"on time"}`)))
	if err != nil {
		t.Fatalf("current claim holder: %v", err)
	}
	if result.NextNodeID != "work" {
		t.Errorf("routed to %q, want work", result.NextNodeID)
	}
}

// Property: the event sequence per run aggregate is strictly monotonic — 1..N
// with no gaps and no repeats — across interleaved runs and refused
// completions. A consumer that sees sequence n and then n+2 has missed a
// transition, so this is the property that makes the audit trail usable.
func TestEventSequencePerRunIsStrictlyMonotonic(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")

	const runs = 3
	ids := make([]string, 0, runs)
	next := make([]engine.CompletionResult, runs)

	for i := 0; i < runs; i++ {
		run := f.createRun(fmt.Sprintf(`{"subject":"interleaved %d"}`, i))
		ids = append(ids, run.ID)
		next[i] = engine.CompletionResult{NextNodeRunID: f.readyNodeRun(run.ID).ID, NextNodeID: "intake"}
	}

	// Advance every run one step at a time, round robin, so the three runs'
	// events are written interleaved into one events table.
	script := []struct{ outcome, output string }{
		{"completed", `{"scope":"s"}`},               // intake
		{"completed", `{"revision":1}`},              // work
		{"changes_required", `{"give_up":false}`},    // check, loops
		{"completed", `{"revision":2}`},              // work
		{"done", `{"summary":"ok","give_up":false}`}, // check, ends
	}
	for _, step := range script {
		for i := range ids {
			if next[i].NextNodeRunID == "" {
				continue
			}
			next[i] = f.step(fmt.Sprintf("worker-%d", i), next[i].NextNodeRunID, succeeded(step.outcome, step.output))
		}
	}

	for i, id := range ids {
		// eventTypes fails the test if the sequence is not 1..N in order.
		types := f.eventTypes(id)
		if len(types) == 0 {
			t.Fatalf("run %d recorded no events", i)
		}
		if types[len(types)-1] != engine.TypeRunCompleted {
			t.Errorf("run %d ended with %s, want %s", i, types[len(types)-1], engine.TypeRunCompleted)
		}
		if f.run(id).State != engine.RunCompleted {
			t.Errorf("run %d did not complete", i)
		}
	}
}

// runSnapshot is the state a refused completion must not change.
type runSnapshot struct {
	nodeRunState   engine.NodeRunState
	nodeRunOutcome string
	runState       engine.RunState
	attempts       int
	events         int
	ledgerRecords  int
	nodeRuns       int
}

func snapshot(f *fixture, runID, nodeRunID string) runSnapshot {
	f.t.Helper()
	nodeRun := f.nodeRun(nodeRunID)
	return runSnapshot{
		nodeRunState:   nodeRun.State,
		nodeRunOutcome: nodeRun.Outcome,
		runState:       f.run(runID).State,
		attempts:       f.countScalar(`SELECT COUNT(*)::int FROM attempts WHERE node_run_id = $1`, nodeRunID),
		events:         f.countScalar(`SELECT COUNT(*)::int FROM events WHERE aggregate_id = $1`, runID),
		ledgerRecords:  f.countScalar(`SELECT COUNT(*)::int FROM ledger_records WHERE run_id = $1`, runID),
		nodeRuns:       f.countScalar(`SELECT COUNT(*)::int FROM node_runs WHERE run_id = $1`, runID),
	}
}
