package engine_test

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// The t20 split/join acceptance matrix (issue #43, docs/design/
// 2026-08-13-parallel-tokens-full.md §9). Test numbers below name the
// design's own rows so the two can be read side by side.
//
// Everything runs against real PostgreSQL through the same harness the
// sequential engine tests use: the barrier's race-freedom is a property of
// the run's advisory lock, and a fake store would prove nothing about it.

// splitOf drives a run to its split and returns the committed SplitResult.
// The parallel node is the entry node in every parallel fixture, so the split
// is the first completion: the worker dispatch seam completes a parallel node
// immediately with StatusSucceeded / outcome `split`, and the fan-out happens
// inside that completion's transaction (design D2).
func (f *fixture) splitOf(run engine.Run) engine.CompletionResult {
	f.t.Helper()
	fan := f.readyNodeRun(run.ID)
	return f.step("worker-split", fan.ID, succeeded("split", `{}`))
}

// branch returns the fanned-out branch at nodeID.
func branchAt(t *testing.T, split *engine.SplitResult, nodeID string) engine.SplitBranch {
	t.Helper()
	if split == nil {
		t.Fatalf("completion carried no SplitResult")
	}
	for _, b := range split.Branches {
		if b.NodeID == nodeID {
			return b
		}
	}
	t.Fatalf("split has no branch at node %q (branches: %+v)", nodeID, split.Branches)
	return engine.SplitBranch{}
}

func branchNodes(split *engine.SplitResult) []string {
	var out []string
	for _, b := range split.Branches {
		out = append(out, b.NodeID)
	}
	sort.Strings(out)
	return out
}

func (f *fixture) tokenGroupCount(runID string) int {
	f.t.Helper()
	return f.countScalar(`SELECT COUNT(*)::int FROM token_groups WHERE run_id = $1`, runID)
}

func (f *fixture) activeTokens(runID string) int {
	f.t.Helper()
	return f.countScalar(`SELECT COUNT(*)::int FROM tokens WHERE run_id = $1 AND state = 'active'`, runID)
}

func (f *fixture) tokenGroupOf(tokenID string) string {
	f.t.Helper()
	var group *string
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT group_id FROM tokens WHERE id = $1`, tokenID).Scan(&group); err != nil {
		f.t.Fatalf("read token %s: %v", tokenID, err)
	}
	if group == nil {
		return ""
	}
	return *group
}

func (f *fixture) parentTokenOf(tokenID string) string {
	f.t.Helper()
	var parent *string
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT parent_token_id FROM tokens WHERE id = $1`, tokenID).Scan(&parent); err != nil {
		f.t.Fatalf("read token %s: %v", tokenID, err)
	}
	if parent == nil {
		return ""
	}
	return *parent
}

func (f *fixture) groupCardinality(groupID string) int {
	f.t.Helper()
	return f.countScalar(`SELECT cardinality FROM token_groups WHERE id = $1`, groupID)
}

func (f *fixture) arrivalCount(joinNodeRunID string) int {
	f.t.Helper()
	return f.countScalar(`SELECT COUNT(*)::int FROM join_arrivals WHERE join_node_run_id = $1`, joinNodeRunID)
}

// barrier returns the run's single waiting_join node run, or fails.
func (f *fixture) barrier(runID string) engine.NodeRun {
	f.t.Helper()
	var id string
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT id FROM node_runs WHERE run_id = $1 AND status = 'waiting_join'`, runID).Scan(&id); err != nil {
		f.t.Fatalf("no waiting_join barrier for run %s: %v", runID, err)
	}
	return f.nodeRun(id)
}

func (f *fixture) workItemState(nodeRunID string) string {
	f.t.Helper()
	var state string
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT state FROM work_items WHERE node_run_id = $1 ORDER BY created_at DESC LIMIT 1`, nodeRunID).Scan(&state); err != nil {
		f.t.Fatalf("no work item for node run %s: %v", nodeRunID, err)
	}
	return state
}

func (f *fixture) workItemCount(nodeRunID string) int {
	f.t.Helper()
	return f.countScalar(`SELECT COUNT(*)::int FROM work_items WHERE node_run_id = $1`, nodeRunID)
}

// completeJoin claims the satisfied join node run and completes it the way
// the worker's join dispatch seam does: outcome `joined` with the aggregated
// arrival array (design D5). The engine does not build that payload — a
// fenced worker does — so the test supplies it, the same way every other
// engine test supplies the output its actor would have produced.
func (f *fixture) completeJoin(joinNodeRunID string) engine.CompletionResult {
	f.t.Helper()
	return f.step("worker-join", joinNodeRunID, succeeded("joined", `{"arrivals":[],"policy":"all"}`))
}

// --- T1 / T2: fan-out under one transaction, discovered cardinality --------

func TestSplitFansOutUnderOneTransaction(t *testing.T) {
	f := newFixture(t, "parallel.workflow.yaml")
	run := f.createRun(`{"slow":true}`)

	split := f.splitOf(run)
	if split.Split == nil {
		t.Fatalf("a parallel node's completion carried no SplitResult")
	}
	if split.Split.Cardinality != 3 || len(split.Split.Branches) != 3 {
		t.Fatalf("cardinality = %d over %d branches, want 3 over 3", split.Split.Cardinality, len(split.Split.Branches))
	}
	equalStrings(t, branchNodes(split.Split), []string{"lint", "slow", "test"}, "branch nodes")

	// NextNodeID/NextNodeRunID stay empty: a split has no single "next"
	// (review point S3 — the multi-target CompletionResult change).
	if split.NextNodeID != "" || split.NextNodeRunID != "" {
		t.Errorf("split reported NextNodeID=%q NextNodeRunID=%q, want both empty", split.NextNodeID, split.NextNodeRunID)
	}

	// One group row, cardinality 3, and every branch token stamped with it.
	if got := f.tokenGroupCount(run.ID); got != 1 {
		t.Errorf("token_groups rows = %d, want 1", got)
	}
	if got := f.groupCardinality(split.Split.GroupID); got != 3 {
		t.Errorf("group cardinality = %d, want 3", got)
	}
	fanToken := f.nodeRun(split.NodeRunID).TokenID
	for _, b := range split.Split.Branches {
		if got := f.tokenGroupOf(b.TokenID); got != split.Split.GroupID {
			t.Errorf("branch %s token group = %q, want %q", b.NodeID, got, split.Split.GroupID)
		}
		// Ancestry stays a tree: every fanned token's parent is the split's
		// own token (design §3.3).
		if got := f.parentTokenOf(b.TokenID); got != fanToken {
			t.Errorf("branch %s parent token = %q, want the split token %q", b.NodeID, got, fanToken)
		}
		if b.WorkID == "" {
			t.Errorf("branch %s enqueued no work item", b.NodeID)
		}
		if got := f.workItemState(b.NodeRunID); got != "ready" {
			t.Errorf("branch %s work item state = %q, want ready", b.NodeID, got)
		}
	}
	// Three branch tokens active; the split's own token was consumed.
	if got := f.activeTokens(run.ID); got != 3 {
		t.Errorf("active tokens after the split = %d, want 3", got)
	}
	if !contains(split.EventTypes, engine.TypeTokenSplit) {
		t.Errorf("events = %v, want one of type %s", split.EventTypes, engine.TypeTokenSplit)
	}
}

func TestGuardedSplitFansOutOnlyTheEligibleSet(t *testing.T) {
	f := newFixture(t, "parallel.workflow.yaml")
	// input.slow is absent, so the guarded `slow` edge does not fire: a
	// declared 3-edge split realizes cardinality 2 (design D4 — the sibling
	// set is DISCOVERED, which is exactly why the join cannot declare it).
	run := f.createRun(`{}`)

	split := f.splitOf(run)
	if split.Split.Cardinality != 2 {
		t.Fatalf("cardinality = %d, want 2", split.Split.Cardinality)
	}
	equalStrings(t, branchNodes(split.Split), []string{"lint", "test"}, "branch nodes")
	if got := f.groupCardinality(split.Split.GroupID); got != 2 {
		t.Errorf("recorded group cardinality = %d, want 2", got)
	}
	if got := f.nodeRunCount(run.ID, "slow"); got != 0 {
		t.Errorf("the guarded-out branch created %d node runs, want 0", got)
	}
}

// --- T4: an `all` barrier reconvenes the group ----------------------------

func TestAllJoinReconvergesTheGroup(t *testing.T) {
	f := newFixture(t, "parallel.workflow.yaml")
	run := f.createRun(`{"slow":true}`)
	split := f.splitOf(run)

	lint := branchAt(t, split.Split, "lint")
	test := branchAt(t, split.Split, "test")
	slow := branchAt(t, split.Split, "slow")

	// First arrival: the barrier is created parked, with NO work item —
	// there is no claimable work until the barrier satisfies (design D3).
	first := f.step("worker-a", lint.NodeRunID, succeeded("clean", `{}`))
	if first.JoinNodeRunID == "" || first.JoinSatisfied {
		t.Fatalf("first arrival: JoinNodeRunID=%q JoinSatisfied=%v, want a barrier and not satisfied", first.JoinNodeRunID, first.JoinSatisfied)
	}
	barrier := f.barrier(run.ID)
	if barrier.ID != first.JoinNodeRunID {
		t.Errorf("barrier %s is not the one the arrival reported (%s)", barrier.ID, first.JoinNodeRunID)
	}
	if got := f.workItemCount(barrier.ID); got != 0 {
		t.Errorf("a parked barrier enqueued %d work items, want 0", got)
	}
	if !contains(first.EventTypes, engine.TypeJoinArrived) {
		t.Errorf("events = %v, want one of type %s", first.EventTypes, engine.TypeJoinArrived)
	}

	// Second arrival: still parked, still no second node run — a join
	// instance is ONE logical node execution fed by K branches.
	second := f.step("worker-b", test.NodeRunID, succeeded("passed", `{}`))
	if second.JoinSatisfied {
		t.Fatalf("the barrier fired on arrival 2 of 3")
	}
	if got := f.nodeRunCount(run.ID, "gather"); got != 1 {
		t.Errorf("gather node runs = %d, want 1", got)
	}

	// Third arrival satisfies: waiting_join -> ready, work enqueued.
	third := f.step("worker-c", slow.NodeRunID, succeeded("done", `{}`))
	if !third.JoinSatisfied {
		t.Fatalf("the barrier did not fire on the last arrival")
	}
	if got := f.arrivalCount(barrier.ID); got != 3 {
		t.Errorf("recorded arrivals = %d, want 3", got)
	}
	if got := f.nodeRun(barrier.ID).State; got != engine.NodeRunReady {
		t.Errorf("satisfied barrier state = %s, want ready", got)
	}
	if got := f.workItemState(barrier.ID); got != "ready" {
		t.Errorf("satisfied barrier work item = %q, want ready", got)
	}

	// The join completes like any other node and the run ends.
	done := f.completeJoin(barrier.ID)
	if done.RunState != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (diagnostic: %s)", done.RunState, done.Diagnostic)
	}
	if got := f.activeTokens(run.ID); got != 0 {
		t.Errorf("active tokens after completion = %d, want 0", got)
	}
}

// --- D5 (review): arrival uniqueness is the idempotency backstop ----------

func TestJoinArrivalIsUniquePerBranchToken(t *testing.T) {
	f := newFixture(t, "parallel.workflow.yaml")
	run := f.createRun(`{}`)
	split := f.splitOf(run)
	lint := branchAt(t, split.Split, "lint")

	first := f.step("worker-a", lint.NodeRunID, succeeded("clean", `{}`))
	barrier := first.JoinNodeRunID

	// The fenced completion guard already refuses a replayed completion, so
	// the constraint is the backstop against a future caller that bypasses
	// it. Insert the same (barrier, token) arrival directly and prove the
	// database — not the engine's good behaviour — is what forbids a doubled
	// count (migrations/0019, review point D5).
	_, err := f.store.Pool().Exec(f.ctx, `
		INSERT INTO join_arrivals (id, namespace_id, run_id, join_node_run_id, group_id, token_id, from_node, outcome)
		SELECT $1, namespace_id, run_id, join_node_run_id, group_id, token_id, from_node, outcome
		FROM join_arrivals WHERE join_node_run_id = $2
	`, "dup-"+run.ID, barrier)
	if err == nil {
		t.Fatalf("a duplicate arrival for the same branch token was accepted; the barrier count can be inflated")
	}
	if got := f.arrivalCount(barrier); got != 1 {
		t.Errorf("arrivals after the refused duplicate = %d, want 1", got)
	}
}

// --- T7 / T8: early-firing policies reap the losing branches --------------

func TestAnyJoinFiresOnFirstArrivalAndReapsLosers(t *testing.T) {
	f := newFixture(t, "parallel-any.workflow.yaml")
	run := f.createRun(`{"slow":true}`)
	split := f.splitOf(run)

	lint := branchAt(t, split.Split, "lint")
	test := branchAt(t, split.Split, "test")
	slow := branchAt(t, split.Split, "slow")

	first := f.step("worker-a", lint.NodeRunID, succeeded("clean", `{}`))
	if !first.JoinSatisfied {
		t.Fatalf("an `any` barrier did not fire on its first arrival")
	}
	if len(first.ReapedBranchNodeRuns) != 2 {
		t.Fatalf("reaped %d branch node runs, want 2: %v", len(first.ReapedBranchNodeRuns), first.ReapedBranchNodeRuns)
	}
	if !contains(first.EventTypes, engine.TypeBranchCancelled) {
		t.Errorf("events = %v, want one of type %s", first.EventTypes, engine.TypeBranchCancelled)
	}

	for _, loser := range []engine.SplitBranch{test, slow} {
		if got := f.nodeRun(loser.NodeRunID).State; got != engine.NodeRunCancelled {
			t.Errorf("losing branch %s state = %s, want cancelled", loser.NodeID, got)
		}
		if got := f.workItemState(loser.NodeRunID); got != "cancelled" {
			t.Errorf("losing branch %s work item = %q, want cancelled", loser.NodeID, got)
		}
	}
	// Only the barrier's own token is left active.
	if got := f.activeTokens(run.ID); got != 1 {
		t.Errorf("active tokens after the reap = %d, want 1 (the barrier's)", got)
	}

	// A late completion of a reaped branch leaves no trace: the node run is
	// terminal and the fenced guard refuses it (existing, tested behaviour).
	if _, err := f.engine.CompleteAttempt(f.ctx, engine.CompletionRequest{
		WorkID: test.WorkID, WorkerID: "worker-late", FencingToken: 1, Attempt: 1,
		TechStatus: engine.StatusSucceeded, Outcome: "passed", Output: json.RawMessage(`{}`),
	}); err == nil {
		t.Errorf("a completion for a reaped branch was accepted")
	}
	if got := f.arrivalCount(first.JoinNodeRunID); got != 1 {
		t.Errorf("arrivals after the late completion = %d, want 1", got)
	}
}

func TestQuorumJoinFiresOnTheQuorumthArrival(t *testing.T) {
	f := newFixture(t, "parallel-quorum.workflow.yaml")
	run := f.createRun(`{"slow":true}`)
	split := f.splitOf(run)

	lint := branchAt(t, split.Split, "lint")
	test := branchAt(t, split.Split, "test")
	slow := branchAt(t, split.Split, "slow")

	if first := f.step("worker-a", lint.NodeRunID, succeeded("clean", `{}`)); first.JoinSatisfied {
		t.Fatalf("a quorum-2 barrier fired on arrival 1")
	}
	second := f.step("worker-b", test.NodeRunID, succeeded("passed", `{}`))
	if !second.JoinSatisfied {
		t.Fatalf("a quorum-2 barrier did not fire on arrival 2")
	}
	if len(second.ReapedBranchNodeRuns) != 1 {
		t.Fatalf("reaped %d branch node runs, want 1: %v", len(second.ReapedBranchNodeRuns), second.ReapedBranchNodeRuns)
	}
	if got := f.nodeRun(slow.NodeRunID).State; got != engine.NodeRunCancelled {
		t.Errorf("third branch state = %s, want cancelled", got)
	}
}

// An unsatisfiable quorum resolves loudly rather than parking forever: the
// fixture declares quorum 3 and its guarded split realizes only 2 branches,
// which the compiler cannot catch because cardinality is discovered, not
// declared (design §4.3; the static analysis that would is open item O2).
func TestQuorumAboveRealizedCardinalityFailsLoudly(t *testing.T) {
	f := newFixture(t, "parallel-quorum-unsatisfiable.workflow.yaml")
	run := f.createRun(`{}`)
	split := f.splitOf(run)

	arrived := f.step("worker-a", branchAt(t, split.Split, "lint").NodeRunID, succeeded("clean", `{}`))
	if arrived.RunState != engine.RunFailed {
		t.Fatalf("run state = %s, want failed — a quorum above the realized cardinality can never fire", arrived.RunState)
	}
	if arrived.Diagnostic == "" {
		t.Errorf("the unsatisfiable barrier failed the run with no diagnostic")
	}
	if got := f.activeTokens(run.ID); got != 0 {
		t.Errorf("active tokens after the failure = %d, want 0 (everything reaped)", got)
	}
}

// --- T5 / T6: bounds refuse the WHOLE split -------------------------------

func TestSplitPastMaxParallelTokensIsRefusedWhole(t *testing.T) {
	f := newFixture(t, "parallel-cap.workflow.yaml")
	run := f.createRun(`{"slow":true}`)

	bounded := f.splitOf(run)
	if bounded.Bound == nil || bounded.Bound.Kind != engine.BoundParallelTokens {
		t.Fatalf("bound = %+v, want %s", bounded.Bound, engine.BoundParallelTokens)
	}
	if bounded.RunState != engine.RunFailed {
		t.Errorf("run state = %s, want failed", bounded.RunState)
	}
	if bounded.Split != nil {
		t.Errorf("a refused split still reported a SplitResult: %+v", bounded.Split)
	}
	// Nothing partial: no group, no branch node runs, no live tokens.
	if got := f.tokenGroupCount(run.ID); got != 0 {
		t.Errorf("token_groups rows after a refused split = %d, want 0", got)
	}
	for _, node := range []string{"lint", "test", "slow"} {
		if got := f.nodeRunCount(run.ID, node); got != 0 {
			t.Errorf("refused split created %d node runs at %s, want 0", got, node)
		}
	}
	if got := f.activeTokens(run.ID); got != 0 {
		t.Errorf("active tokens after a refused split = %d, want 0", got)
	}
	if !contains(bounded.EventTypes, engine.TypeRunBounded) {
		t.Errorf("events = %v, want one of type %s", bounded.EventTypes, engine.TypeRunBounded)
	}
}

func TestSplitIsChargedKTransitions(t *testing.T) {
	f := newFixture(t, "parallel-transitions.workflow.yaml")
	run := f.createRun(`{"slow":true}`)

	bounded := f.splitOf(run)
	if bounded.Bound == nil || bounded.Bound.Kind != engine.BoundTransitions {
		t.Fatalf("bound = %+v, want %s — a 3-way split is charged 3 transitions, not 1", bounded.Bound, engine.BoundTransitions)
	}
	if got := f.tokenGroupCount(run.ID); got != 0 {
		t.Errorf("token_groups rows after a refused split = %d, want 0", got)
	}
}

// A two-way split through the same maxTransitions budget is allowed: the
// charge is +K, so the refusal above is about the third branch, not about
// splits being refused categorically.
func TestSplitUnderTheTransitionBudgetProceeds(t *testing.T) {
	f := newFixture(t, "parallel-transitions.workflow.yaml")
	run := f.createRun(`{}`)

	split := f.splitOf(run)
	if split.Bound != nil {
		t.Fatalf("a 2-way split was refused under maxTransitions 2: %+v", split.Bound)
	}
	if split.Split.Cardinality != 2 {
		t.Errorf("cardinality = %d, want 2", split.Split.Cardinality)
	}
}

// --- T15: maxVisitsPerNode is run-scoped across branches ------------------

func TestSharedNodeCountsAVisitPerBranch(t *testing.T) {
	f := newFixture(t, "parallel-shared.workflow.yaml")
	run := f.createRun(`{}`)

	split := f.splitOf(run)
	if split.Bound != nil {
		t.Fatalf("the split was refused: %+v", split.Bound)
	}
	if got := f.nodeRunCount(run.ID, "shared"); got != 2 {
		t.Fatalf("shared node runs = %d, want 2", got)
	}
	// Two node runs at one node, visit counts 1 and 2 — run-scoped counting,
	// documented as intended in design §5.2 (per-lineage counting is O5).
	var visits []int
	rows, err := f.store.Pool().Query(f.ctx,
		`SELECT visit_count FROM node_runs WHERE run_id = $1 AND node_key = 'shared' ORDER BY visit_count`, run.ID)
	if err != nil {
		t.Fatalf("read visits: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan visit: %v", err)
		}
		visits = append(visits, v)
	}
	if len(visits) != 2 || visits[0] != 1 || visits[1] != 2 {
		t.Errorf("visit counts = %v, want [1 2]", visits)
	}

	// Both branches arrive; the D5 arrival array is what keeps two arrivals
	// from one node distinguishable.
	var first engine.CompletionResult
	for i, b := range split.Split.Branches {
		result := f.step("worker-shared", b.NodeRunID, succeeded("done", `{}`))
		if i == 0 {
			first = result
			if result.JoinSatisfied {
				t.Fatalf("the barrier fired on arrival 1 of 2")
			}
		} else if !result.JoinSatisfied {
			t.Fatalf("the barrier did not fire on arrival 2 of 2")
		}
	}
	if got := f.arrivalCount(first.JoinNodeRunID); got != 2 {
		t.Errorf("arrivals from one shared node = %d, want 2", got)
	}
}

// --- T9: a terminal branch failure fails the run and reaps siblings -------

func TestTerminalBranchFailureFailsTheRunAndReapsSiblings(t *testing.T) {
	f := newFixture(t, "parallel.workflow.yaml")
	run := f.createRun(`{"slow":true}`)
	split := f.splitOf(run)

	lint := branchAt(t, split.Split, "lint")
	test := branchAt(t, split.Split, "test")
	slow := branchAt(t, split.Split, "slow")

	// One branch arrives first, so the run also carries a parked barrier —
	// review point R1/S5: the barrier's own token must be consumed by the
	// reap, or it would sit active forever.
	if arrived := f.step("worker-a", lint.NodeRunID, succeeded("clean", `{}`)); arrived.JoinSatisfied {
		t.Fatalf("the barrier fired on arrival 1 of 3")
	}
	barrier := f.barrier(run.ID)

	// The `test` branch fails terminally: retries are exhausted (maxAttempts
	// defaults to 1) and the workflow declares no edge from `failed`.
	failed := f.step("worker-b", test.NodeRunID, engine.CompletionRequest{TechStatus: engine.StatusFailed})
	if failed.RunState != engine.RunFailed {
		t.Fatalf("run state = %s, want failed", failed.RunState)
	}
	if got := f.run(run.ID).State; got != engine.RunFailed {
		t.Fatalf("committed run state = %s, want failed", got)
	}

	// Every live sibling is retired in the SAME transaction (design D6): the
	// still-running branch, its work item, and the parked barrier.
	if got := f.nodeRun(slow.NodeRunID).State; got != engine.NodeRunCancelled {
		t.Errorf("sibling branch state = %s, want cancelled", got)
	}
	if got := f.workItemState(slow.NodeRunID); got != "cancelled" {
		t.Errorf("sibling work item = %q, want cancelled", got)
	}
	if got := f.nodeRun(barrier.ID).State; got != engine.NodeRunCancelled {
		t.Errorf("parked barrier state = %s, want cancelled (review S2/R1)", got)
	}
	if got := f.activeTokens(run.ID); got != 0 {
		t.Errorf("active tokens after the reap = %d, want 0 — including the barrier's own", got)
	}
	if !contains(failed.EventTypes, engine.TypeBranchCancelled) {
		t.Errorf("events = %v, want one of type %s", failed.EventTypes, engine.TypeBranchCancelled)
	}
}

// --- T11: cancellation across tokens --------------------------------------

func TestCancellationReapsEveryBranch(t *testing.T) {
	f := newFixture(t, "parallel.workflow.yaml")
	run := f.createRun(`{"slow":true}`)
	split := f.splitOf(run)

	lint := branchAt(t, split.Split, "lint")
	test := branchAt(t, split.Split, "test")
	slow := branchAt(t, split.Split, "slow")

	f.step("worker-a", lint.NodeRunID, succeeded("clean", `{}`))
	barrier := f.barrier(run.ID)

	cancelled := f.step("worker-b", test.NodeRunID, engine.CompletionRequest{TechStatus: engine.StatusCancelled})
	if cancelled.RunState != engine.RunCancelled {
		t.Fatalf("run state = %s, want cancelled", cancelled.RunState)
	}
	if got := f.nodeRun(slow.NodeRunID).State; got != engine.NodeRunCancelled {
		t.Errorf("sibling branch state = %s, want cancelled", got)
	}
	if got := f.nodeRun(barrier.ID).State; got != engine.NodeRunCancelled {
		t.Errorf("barrier state = %s, want cancelled", got)
	}
	if got := f.activeTokens(run.ID); got != 0 {
		t.Errorf("active tokens after cancellation = %d, want 0", got)
	}
}

// --- T14: nested split/join ------------------------------------------------

func TestNestedSplitJoinPropagatesGroups(t *testing.T) {
	f := newFixture(t, "parallel-nested.workflow.yaml")
	run := f.createRun(`{}`)

	outer := f.splitOf(run)
	outerGroup := outer.Split.GroupID
	plain := branchAt(t, outer.Split, "plain")
	inner := branchAt(t, outer.Split, "inner")

	// The inner parallel node is itself a branch: completing it fans out
	// again, under a group whose parent is the outer one.
	innerSplit := f.step("worker-inner", inner.NodeRunID, succeeded("split", `{}`))
	if innerSplit.Split == nil || innerSplit.Split.Cardinality != 2 {
		t.Fatalf("inner split = %+v, want cardinality 2", innerSplit.Split)
	}
	innerGroup := innerSplit.Split.GroupID
	var parent *string
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT parent_group_id FROM token_groups WHERE id = $1`, innerGroup).Scan(&parent); err != nil {
		t.Fatalf("read inner group: %v", err)
	}
	if parent == nil || *parent != outerGroup {
		t.Fatalf("inner group parent = %v, want the outer group %s", parent, outerGroup)
	}

	// The inner barrier gathers the inner group only.
	left := branchAt(t, innerSplit.Split, "left")
	right := branchAt(t, innerSplit.Split, "right")
	f.step("worker-l", left.NodeRunID, succeeded("done", `{}`))
	innerDone := f.step("worker-r", right.NodeRunID, succeeded("done", `{}`))
	if !innerDone.JoinSatisfied {
		t.Fatalf("the inner barrier did not fire on its second arrival")
	}

	// Completing the inner join hands its post-join token back to the OUTER
	// group, and routes it straight into the outer barrier (design §3.3).
	innerJoined := f.step("worker-ij", innerDone.JoinNodeRunID, succeeded("joined", `{"arrivals":[]}`))
	if innerJoined.JoinNodeRunID == "" {
		t.Fatalf("the inner join's post-join token did not reach the outer barrier")
	}
	if innerJoined.JoinSatisfied {
		t.Fatalf("the outer barrier fired on arrival 1 of 2")
	}

	outerDone := f.step("worker-p", plain.NodeRunID, succeeded("done", `{}`))
	if !outerDone.JoinSatisfied {
		t.Fatalf("the outer barrier did not fire on its second arrival")
	}
	// The post-join token of the OUTER join carries no group: the outer
	// group's parent is empty, so the run re-enters unsplit control flow.
	finished := f.step("worker-oj", outerDone.JoinNodeRunID, succeeded("joined", `{"arrivals":[]}`))
	if finished.RunState != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (diagnostic: %s)", finished.RunState, finished.Diagnostic)
	}
}

// --- T13: concurrent arrivals count exactly once --------------------------

// TestConcurrentArrivalsRaceIntoOneBarrier is the race the whole barrier
// design rests on (§4.2): every completion for a run serializes on
// ledger.RunLockKey, so two branches completing at the same wall-clock
// instant commit their arrivals strictly one after the other and exactly one
// of them is the satisfying arrival.
func TestConcurrentArrivalsRaceIntoOneBarrier(t *testing.T) {
	shared := pgtest.RequireStore(t, testStore)
	connString := shared.Pool().Config().ConnString()
	ctx := context.Background()

	f := newFixture(t, "parallel.workflow.yaml")
	run := f.createRun(`{"slow":true}`)
	split := f.splitOf(run)

	// Two of the three branches arrive concurrently, each through its own
	// pool and engine — real concurrency, not two calls on one connection.
	type outcome struct {
		branch    string
		satisfied bool
	}
	branches := []struct {
		node, outcome string
	}{{"lint", "clean"}, {"test", "passed"}}

	results := make([]outcome, len(branches))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, b := range branches {
		branch := branchAt(t, split.Split, b.node)
		wg.Add(1)
		go func(i int, node, domainOutcome, nodeRunID string) {
			defer wg.Done()
			pool, err := storepg.Connect(ctx, connString)
			if err != nil {
				t.Errorf("connect: %v", err)
				return
			}
			defer pool.Close()
			racer := f.rebind(t, pool)
			<-start
			result := racer.step("worker-"+node, nodeRunID, succeeded(domainOutcome, `{}`))
			results[i] = outcome{branch: node, satisfied: result.JoinSatisfied}
		}(i, b.node, b.outcome, branch.NodeRunID)
	}
	close(start)
	wg.Wait()

	// Two of three arrivals: neither may fire an `all` barrier of three, and
	// the count must be exactly 2 — no lost update, no double count.
	for _, r := range results {
		if r.satisfied {
			t.Errorf("branch %s fired an all-barrier at arrival 2 of 3", r.branch)
		}
	}
	barrier := f.barrier(run.ID)
	if got := f.arrivalCount(barrier.ID); got != 2 {
		t.Fatalf("recorded arrivals after two racing branches = %d, want exactly 2", got)
	}
	if got := f.nodeRunCount(run.ID, "gather"); got != 1 {
		t.Fatalf("gather node runs = %d, want exactly 1 — both racers created a barrier", got)
	}

	// The last branch fires it, once.
	last := f.step("worker-slow", branchAt(t, split.Split, "slow").NodeRunID, succeeded("done", `{}`))
	if !last.JoinSatisfied {
		t.Fatalf("the third arrival did not fire the barrier")
	}
	if got := f.arrivalCount(barrier.ID); got != 3 {
		t.Errorf("recorded arrivals = %d, want 3", got)
	}
}

// --- T10: a split survives a process restart ------------------------------

func TestSplitSurvivesAProcessRestart(t *testing.T) {
	shared := pgtest.RequireStore(t, testStore)
	connString := shared.Pool().Config().ConnString()
	ctx := context.Background()

	before, err := storepg.Connect(ctx, connString)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	f := newFixtureOn(t, before, "parallel.workflow.yaml")
	run := f.createRun(`{}`)
	split := f.splitOf(run)
	lint := branchAt(t, split.Split, "lint")
	test := branchAt(t, split.Split, "test")
	runID := run.ID
	before.Close()

	// Nothing crosses the restart but ids: the fan-out committed, so both
	// branches are ordinary claimable work items a fresh process picks up.
	after, err := storepg.Connect(ctx, connString)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer after.Close()
	restarted := f.rebind(t, after)

	if got := restarted.run(runID).State; got != engine.RunRunning {
		t.Fatalf("run state after restart = %s, want running", got)
	}
	if got := restarted.activeTokens(runID); got != 2 {
		t.Fatalf("active tokens after restart = %d, want 2", got)
	}
	restarted.step("worker-after", lint.NodeRunID, succeeded("clean", `{}`))
	satisfied := restarted.step("worker-after", test.NodeRunID, succeeded("passed", `{}`))
	if !satisfied.JoinSatisfied {
		t.Fatalf("the barrier did not satisfy after the restart")
	}
	if got := restarted.completeJoin(satisfied.JoinNodeRunID).RunState; got != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed", got)
	}
}

// --- T12 (runtime half): an end node with live siblings fails loudly ------

// TestEndNodeWithActiveSiblingsFailsTheRun drives the D7 defense-in-depth
// guard the compiler is supposed to make unreachable, by publishing an IR the
// compiler would have refused: a split branch routed straight at the end
// node. The compiler half of D7 is pinned in internal/compiler's fixtures.
func TestEndNodeWithActiveSiblingsFailsTheRun(t *testing.T) {
	f := newFixture(t, "parallel.workflow.yaml")
	// Rewrite the pinned IR the way a buggy or bypassed compiler might: the
	// `lint` branch now ends the run instead of arriving at the barrier.
	f.cw = rewriteIR(t, f.cw, func(ir map[string]any) {
		spec := ir["spec"].(map[string]any)
		for _, raw := range spec["edges"].([]any) {
			edge := raw.(map[string]any)
			if edge["from"] == "lint.clean" {
				edge["to"] = "finish"
			}
		}
	})

	run := f.createRun(`{}`)
	split := f.splitOf(run)
	stranding := f.step("worker-a", branchAt(t, split.Split, "lint").NodeRunID, succeeded("clean", `{}`))

	if stranding.RunState != engine.RunFailed {
		t.Fatalf("run state = %s, want failed — completing here would strand the sibling branch", stranding.RunState)
	}
	if stranding.Diagnostic == "" {
		t.Errorf("the stranded-sibling refusal carried no diagnostic")
	}
	if got := f.run(run.ID).State; got != engine.RunFailed {
		t.Errorf("committed run state = %s, want failed", got)
	}
}
