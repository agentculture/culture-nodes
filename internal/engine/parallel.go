package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Split fan-out and join barriers (issue #43; the parallel-tokens design,
// docs/design/2026-08-13-parallel-tokens-full.md).
//
// Everything in this file runs inside one §12.5 completion transaction under
// the run's advisory lock — that lock is what makes the barrier counting
// race-free (design §4.2): two branches completing "simultaneously" commit
// their arrivals strictly one after the other, so the arrival that brings
// the count to the threshold sees every prior arrival's committed row, and
// exactly one arrival can be the satisfying one (test T13).

// arrival is one branch reaching a join: the consumed branch token, the
// group its barrier gathers, and the routed outcome/output the join's
// aggregated output will carry.
type arrival struct {
	TokenID string
	GroupID string
	Outcome string
	Output  json.RawMessage
	Edge    *Edge
}

// joinTx is the slice of a §12.5 transition transaction the barrier code
// needs. Both transition paths build one — completion (an attempt) and
// humanTaskDecision (a human decision) — so an arrival is recorded and a
// barrier settled by ONE implementation rather than two that can drift.
// humandecision.go's package doc explains why those two paths otherwise
// duplicate their routing methods; the barrier is the piece where duplication
// would be actively unsafe, because a second copy could count a group twice.
//
// result points at the caller's own CompletionResult so the join fields land
// where the caller returns them; emit is the caller's audit-event method, so
// events keep flowing through the same outbox pairing (§12.5 steps 7 and 10).
type joinTx struct {
	engine  *Engine
	tx      Tx
	run     Run
	nodeRun NodeRun
	now     time.Time
	result  *CompletionResult
	emit    func(ctx context.Context, eventType string, data map[string]any) error
}

func (c *completion) joinTx() *joinTx {
	return &joinTx{
		engine: c.engine, tx: c.tx, run: c.run, nodeRun: c.nodeRun,
		now: c.now, result: &c.result, emit: c.emit,
	}
}

func (d *humanTaskDecision) joinTx() *joinTx {
	return &joinTx{
		engine: d.engine, tx: d.tx, run: d.run, nodeRun: d.nodeRun,
		now: d.now, result: &d.result, emit: d.emit,
	}
}

// fanOut is a parallel node's completion (design §3.3): one token group row,
// then one token + node run + dispatch per eligible edge, all in this
// transaction. Restart durability needs no extra machinery — the commit
// leaves K ordinary claimable work items, exactly as a crash after a
// sequential transition leaves one (test T10).
//
// A split edge may point straight at a join node — a zero-node branch. Its
// token is created (the group's cardinality counted it), consumed, and
// recorded as an arrival right here; the barrier is only SETTLED after the
// whole loop, because settling can reap the group's losing branches and a
// reap that ran mid-loop would leave the branches created after it alive.
func (c *completion) fanOut(ctx context.Context, targets []transitionTarget, sourceToken Token, transitions int, visits map[string]int) error {
	group := TokenGroup{
		ID:             c.engine.newID(),
		NamespaceID:    c.run.NamespaceID,
		RunID:          c.run.ID,
		SplitNodeRunID: c.nodeRun.ID,
		ParentGroupID:  sourceToken.GroupID,
		Cardinality:    len(targets),
		CreatedAt:      c.now,
	}
	if err := c.tx.InsertTokenGroup(ctx, group); err != nil {
		return err
	}

	edges := make([]string, 0, len(targets))
	for _, target := range targets {
		edges = append(edges, target.Edge.From+" -> "+target.NextNodeID)
	}
	if err := c.emit(ctx, TypeTokenSplit, map[string]any{
		"run_id":          c.run.ID,
		"node_run_id":     c.nodeRun.ID,
		"node_id":         c.nodeRun.NodeID,
		"group_id":        group.ID,
		"parent_group_id": group.ParentGroupID,
		"cardinality":     group.Cardinality,
		"edges":           edges,
	}); err != nil {
		return err
	}

	j := c.joinTx()
	split := &SplitResult{GroupID: group.ID, Cardinality: group.Cardinality}
	// pending holds the barriers zero-node branches fed, settled once each
	// after every branch exists.
	var pending []pendingBarrier

	// visits is mutated as branches land so two split edges targeting one
	// node get distinct visit counts — the same charge checkBounds made.
	for _, target := range targets {
		next := c.wf.Nodes[target.NextNodeID]
		if next == nil {
			return &WorkflowError{
				Digest: c.wf.Digest,
				Detail: fmt.Sprintf("edge %q targets node %q, which the pinned definition does not declare", target.Edge.From, target.NextNodeID),
			}
		}

		token := Token{
			ID:            c.engine.newID(),
			NamespaceID:   c.run.NamespaceID,
			RunID:         c.run.ID,
			NodeID:        target.NextNodeID,
			State:         TokenActive,
			ParentTokenID: sourceToken.ID,
			GroupID:       group.ID,
			CreatedAt:     c.now,
		}
		if err := c.tx.InsertToken(ctx, token); err != nil {
			return err
		}

		if next.Kind == kindJoin {
			if err := c.tx.ConsumeToken(ctx, token.ID); err != nil {
				return err
			}
			barrier, diagnostic, err := j.recordArrival(ctx, next, arrival{
				TokenID: token.ID,
				GroupID: group.ID,
				Outcome: target.Edge.FromOutcome,
				Output:  c.req.Output,
				Edge:    target.Edge,
			}, visits)
			if err != nil {
				return err
			}
			if diagnostic != "" {
				return c.failRun(ctx, c.result.NodeRunState, diagnostic)
			}
			pending = appendPendingBarrier(pending, pendingBarrier{node: next, barrier: barrier, groupID: group.ID})
			split.Branches = append(split.Branches, SplitBranch{
				NodeID: target.NextNodeID, NodeRunID: barrier.ID, TokenID: token.ID,
			})
			continue
		}

		nodeRun := NodeRun{
			ID:          c.engine.newID(),
			NamespaceID: c.run.NamespaceID,
			RunID:       c.run.ID,
			TokenID:     token.ID,
			NodeID:      target.NextNodeID,
			State:       dispatchState(next.Kind),
			VisitCount:  visits[target.NextNodeID] + 1,
			CreatedAt:   c.now,
			UpdatedAt:   c.now,
		}
		if err := c.tx.InsertNodeRun(ctx, nodeRun); err != nil {
			return err
		}
		visits[target.NextNodeID]++

		if err := c.emit(ctx, TypeTokenTransitioned, map[string]any{
			"run_id":        c.run.ID,
			"node_run_id":   nodeRun.ID,
			"from_node":     c.nodeRun.NodeID,
			"to_node":       nodeRun.NodeID,
			"outcome":       target.Edge.FromOutcome,
			"edge":          target.Edge.From,
			"guard":         target.Edge.When,
			"from_token_id": sourceToken.ID,
			"token_id":      token.ID,
			"group_id":      group.ID,
			"visit":         nodeRun.VisitCount,
		}); err != nil {
			return err
		}

		workID, humanTaskID, err := c.engine.dispatchNode(ctx, c.tx, next, c.run, nodeRun, target.Edge.FromNode, target.Edge.FromOutcome, c.now)
		if err != nil {
			return err
		}
		if humanTaskID != "" {
			if err := c.emit(ctx, TypeHumanTaskCreated, map[string]any{
				"run_id":        c.run.ID,
				"node_run_id":   nodeRun.ID,
				"node_id":       nodeRun.NodeID,
				"token_id":      token.ID,
				"human_task_id": humanTaskID,
				"visit":         nodeRun.VisitCount,
			}); err != nil {
				return err
			}
		} else {
			if err := c.emit(ctx, TypeNodeRunReady, map[string]any{
				"run_id":      c.run.ID,
				"node_run_id": nodeRun.ID,
				"node_id":     nodeRun.NodeID,
				"token_id":    token.ID,
				"work_id":     workID,
				"visit":       nodeRun.VisitCount,
			}); err != nil {
				return err
			}
		}

		split.Branches = append(split.Branches, SplitBranch{
			NodeID: target.NextNodeID, NodeRunID: nodeRun.ID, TokenID: token.ID, WorkID: workID,
		})
	}

	c.result.Split = split
	c.result.RunState = RunRunning

	// Every branch exists now, so a barrier that fires early reaps exactly
	// the branches that lost — no later branch can escape the reap.
	for _, p := range pending {
		diagnostic, err := j.settleBarrier(ctx, p.node, p.barrier, p.groupID)
		if err != nil {
			return err
		}
		if diagnostic != "" {
			return c.failRun(ctx, c.result.NodeRunState, diagnostic)
		}
	}

	// A K-way split is K transitions (design §5.2), but the count is DERIVED
	// from node runs: a zero-node branch contributed one only when it opened
	// the barrier. Re-derive rather than guess.
	c.result.Transitions = 0
	return c.finish(ctx)
}

// pendingBarrier is one barrier a fan-out fed, remembered until every branch
// of the split exists.
type pendingBarrier struct {
	node    *Node
	barrier NodeRun
	groupID string
}

// appendPendingBarrier keeps one entry per barrier node run: two zero-node
// branches of the same split feed one barrier, which must still be settled
// exactly once.
func appendPendingBarrier(pending []pendingBarrier, p pendingBarrier) []pendingBarrier {
	for _, existing := range pending {
		if existing.barrier.ID == p.barrier.ID {
			return pending
		}
	}
	return append(pending, p)
}

// arriveAtJoin is the ordinary (non-fan-out) arrival: record, then settle,
// then the caller finishes. It returns a diagnostic string — not an error —
// when the arrival is a loud runtime refusal the caller must turn into its
// own failRun, because "fail this run" is spelled differently on the two
// transition paths.
func (j *joinTx) arriveAtJoin(ctx context.Context, joinNode *Node, arr arrival, visits map[string]int) (diagnostic string, err error) {
	barrier, diagnostic, err := j.recordArrival(ctx, joinNode, arr, visits)
	if err != nil || diagnostic != "" {
		return diagnostic, err
	}
	return j.settleBarrier(ctx, joinNode, barrier, arr.GroupID)
}

// recordArrival records one branch reaching a barrier (design §4.1/D3): the
// first arrival creates the barrier — a token at the join node and a node run
// parked in waiting_join, with deliberately NO work item — and every arrival
// appends a join_arrivals row. It never fires the barrier; settleBarrier
// does, so a fan-out can record several arrivals before any of them can reap
// a sibling.
func (j *joinTx) recordArrival(ctx context.Context, joinNode *Node, arr arrival, visits map[string]int) (NodeRun, string, error) {
	if arr.GroupID == "" {
		// A token outside any split has no group for the barrier to count
		// against. The compiler refuses the graphs that get here
		// (graph.join_outside_split); this is the loud runtime guard for
		// hand-built IRs.
		return NodeRun{}, fmt.Sprintf(
			"node %q routed into join node %q with a token that belongs to no token group; a join can only reconvene a split's siblings",
			j.nodeRun.NodeID, joinNode.ID), nil
	}
	group, err := j.tx.TokenGroup(ctx, arr.GroupID)
	if err != nil {
		return NodeRun{}, "", err
	}

	barrier, err := j.tx.OpenJoinBarrier(ctx, j.run.ID, joinNode.ID, arr.GroupID)
	switch {
	case errors.Is(err, ErrNotFound):
		barrierToken := Token{
			ID:            j.engine.newID(),
			NamespaceID:   j.run.NamespaceID,
			RunID:         j.run.ID,
			NodeID:        joinNode.ID,
			State:         TokenActive,
			ParentTokenID: arr.TokenID,
			GroupID:       arr.GroupID,
			CreatedAt:     j.now,
		}
		if err := j.tx.InsertToken(ctx, barrierToken); err != nil {
			return NodeRun{}, "", err
		}
		barrier = NodeRun{
			ID:          j.engine.newID(),
			NamespaceID: j.run.NamespaceID,
			RunID:       j.run.ID,
			TokenID:     barrierToken.ID,
			NodeID:      joinNode.ID,
			State:       NodeRunWaitingJoin,
			VisitCount:  visits[joinNode.ID] + 1,
			CreatedAt:   j.now,
			UpdatedAt:   j.now,
		}
		if err := j.tx.InsertNodeRun(ctx, barrier); err != nil {
			return NodeRun{}, "", err
		}
		visits[joinNode.ID]++
	case err != nil:
		return NodeRun{}, "", err
	}

	// The (join_node_run_id, token_id) unique constraint makes a replayed
	// arrival a constraint violation rather than a silently doubled count
	// (migrations/0019; review point D5).
	if err := j.tx.InsertJoinArrival(ctx, JoinArrival{
		ID:            j.engine.newID(),
		NamespaceID:   j.run.NamespaceID,
		RunID:         j.run.ID,
		JoinNodeRunID: barrier.ID,
		GroupID:       arr.GroupID,
		TokenID:       arr.TokenID,
		FromNode:      j.nodeRun.NodeID,
		Outcome:       arr.Outcome,
		Output:        arr.Output,
		ArrivedAt:     j.now,
	}); err != nil {
		return NodeRun{}, "", err
	}
	count, err := j.tx.JoinArrivalCount(ctx, barrier.ID)
	if err != nil {
		return NodeRun{}, "", err
	}

	if err := j.emit(ctx, TypeJoinArrived, map[string]any{
		"run_id":      j.run.ID,
		"node_run_id": barrier.ID,
		"node_id":     joinNode.ID,
		"group_id":    arr.GroupID,
		"token_id":    arr.TokenID,
		"from_node":   j.nodeRun.NodeID,
		"outcome":     arr.Outcome,
		"edge":        arr.Edge.From,
		"arrivals":    count,
		"cardinality": group.Cardinality,
		"policy":      joinNode.JoinPolicy,
	}); err != nil {
		return NodeRun{}, "", err
	}

	j.result.JoinNodeRunID = barrier.ID
	// The run keeps moving whether or not this arrival fires the barrier —
	// either sibling branches are still working, or the join's own work
	// becomes claimable in settleBarrier.
	j.result.RunState = RunRunning
	return barrier, "", nil
}

// settleBarrier fires the barrier when its policy threshold has been reached:
// the join node run flips waiting_join -> ready, its work item is enqueued,
// and a worker completes it with outcome `joined` through the normal fenced
// transaction (design D2 — completion authority stays with fenced workers;
// the barrier never completes anything). An early-firing policy (any/quorum)
// reaps the group's losing branches in this same transaction (design §4.4).
//
// It returns a diagnostic when the barrier can never fire, for the caller to
// turn into a run failure.
func (j *joinTx) settleBarrier(ctx context.Context, joinNode *Node, barrier NodeRun, groupID string) (string, error) {
	group, err := j.tx.TokenGroup(ctx, groupID)
	if err != nil {
		return "", err
	}
	count, err := j.tx.JoinArrivalCount(ctx, barrier.ID)
	if err != nil {
		return "", err
	}

	threshold, satisfiable := joinNode.joinThreshold(group.Cardinality)
	if !satisfiable {
		// A quorum above the realized cardinality can never fire: guarded
		// split edges make this reachable even though the compiler saw
		// enough authored edges. Resolve loudly instead of hanging the
		// barrier open forever (design §4.3; the deferred policy-aware
		// analysis is open item O2).
		return fmt.Sprintf(
			"join node %q requires %d arrival(s) under policy %q but the split realized only %d branch(es); the barrier can never satisfy",
			joinNode.ID, threshold, joinNode.JoinPolicy, group.Cardinality), nil
	}
	if count < threshold {
		return "", nil
	}

	if err := j.tx.UpdateNodeRun(ctx, barrier.ID, NodeRunReady, ""); err != nil {
		return "", err
	}
	workID, err := j.tx.EnqueueWork(ctx, barrier.ID, j.now)
	if err != nil {
		return "", err
	}
	if err := j.emit(ctx, TypeNodeRunReady, map[string]any{
		"run_id":      j.run.ID,
		"node_run_id": barrier.ID,
		"node_id":     joinNode.ID,
		"token_id":    barrier.TokenID,
		"work_id":     workID,
		"visit":       barrier.VisitCount,
	}); err != nil {
		return "", err
	}
	j.result.JoinSatisfied = true
	j.result.NextNodeID = joinNode.ID
	j.result.NextNodeRunID = barrier.ID

	// Losing branches of an early-firing policy are reaped explicitly and
	// transactionally; the caller propagates cancellation to any async actors
	// best-effort after commit (design §4.4).
	if count < group.Cardinality {
		reaped, err := j.tx.ReapGroupBranches(ctx, j.run.ID, groupID, barrier.TokenID)
		if err != nil {
			return "", err
		}
		if err := j.emitBranchesCancelled(ctx, reaped, "join barrier satisfied before this branch arrived"); err != nil {
			return "", err
		}
	}
	return "", nil
}

// emitBranchesCancelled records each reaped branch node run and remembers
// them on the result for post-commit cancellation propagation.
func (j *joinTx) emitBranchesCancelled(ctx context.Context, nodeRunIDs []string, detail string) error {
	for _, id := range nodeRunIDs {
		if err := j.emit(ctx, TypeBranchCancelled, map[string]any{
			"run_id":      j.run.ID,
			"node_run_id": id,
			"detail":      detail,
		}); err != nil {
			return err
		}
	}
	j.result.ReapedBranchNodeRuns = append(j.result.ReapedBranchNodeRuns, nodeRunIDs...)
	return nil
}

// reapSiblings retires every other live branch of the run when this
// transition made the run terminal (failure, bound, cancellation — design
// D6): with parallel tokens, a terminal run with dangling live branches
// would be exactly the re-dispatch zombie issue #19 fixed for cancellation.
// Sequential runs have nothing else live, so this reaps nothing and changes
// nothing for them.
func (j *joinTx) reapSiblings(ctx context.Context, why string) error {
	reaped, err := j.tx.ReapRunState(ctx, j.run.ID, j.nodeRun.ID)
	if err != nil {
		return err
	}
	return j.emitBranchesCancelled(ctx, reaped, why)
}

func (c *completion) reapSiblings(ctx context.Context, why string) error {
	return c.joinTx().reapSiblings(ctx, why)
}

func (d *humanTaskDecision) reapSiblings(ctx context.Context, why string) error {
	return d.joinTx().reapSiblings(ctx, why)
}
