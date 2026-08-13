package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// fanOut is a parallel node's completion (design §3.3): one token group row,
// then one token + node run + dispatch per eligible edge, all in this
// transaction. Restart durability needs no extra machinery — the commit
// leaves K ordinary claimable work items, exactly as a crash after a
// sequential transition leaves one (test T10).
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
		"run_id":      c.run.ID,
		"node_run_id": c.nodeRun.ID,
		"node_id":     c.nodeRun.NodeID,
		"group_id":    group.ID,
		"cardinality": group.Cardinality,
		"parent_group_id": group.ParentGroupID,
		"edges":       edges,
	}); err != nil {
		return err
	}

	split := &SplitResult{GroupID: group.ID, Cardinality: group.Cardinality}
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

		// A split edge routed straight into a join is a zero-node branch:
		// its token exists (the group's cardinality counted it), is consumed
		// immediately, and arrives at the barrier in this same transaction.
		if next.Kind == kindJoin {
			if err := c.tx.ConsumeToken(ctx, token.ID); err != nil {
				return err
			}
			if err := c.arriveAtJoin(ctx, next, arrival{
				TokenID: token.ID,
				GroupID: group.ID,
				Outcome: target.Edge.FromOutcome,
				Output:  c.req.Output,
				Edge:    target.Edge,
			}, visits); err != nil {
				return err
			}
			split.Branches = append(split.Branches, SplitBranch{
				NodeID: target.NextNodeID, NodeRunID: c.result.JoinNodeRunID, TokenID: token.ID,
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
	// A K-way split is K transitions (design §5.2) — but the DERIVED count
	// only reflects node runs actually created, and a zero-node branch that
	// arrived directly at a barrier created one only if it opened it. Report
	// what a re-derivation would answer.
	c.result.Transitions = transitions + len(split.Branches)
	c.result.RunState = RunRunning
	return c.finish(ctx)
}

// arriveAtJoin records one branch reaching a barrier (design §4.1/D3): the
// first arrival creates the barrier — a token at the join node and a node
// run parked in waiting_join, with deliberately NO work item — and every
// arrival appends a join_arrivals row. The arrival that reaches the policy
// threshold flips the barrier to ready, enqueues its work, and reaps the
// group's losing branches when the policy fired early (design §4.4).
func (c *completion) arriveAtJoin(ctx context.Context, joinNode *Node, arr arrival, visits map[string]int) error {
	if arr.GroupID == "" {
		// A token outside any split has no group for the barrier to count
		// against. The compiler refuses the graphs that get here
		// (graph.join_outside_split); this is the loud runtime guard for
		// hand-built IRs.
		return c.failRun(ctx, c.result.NodeRunState, fmt.Sprintf(
			"node %q routed into join node %q with a token that belongs to no token group; a join can only reconvene a split's siblings",
			c.nodeRun.NodeID, joinNode.ID))
	}
	group, err := c.tx.TokenGroup(ctx, arr.GroupID)
	if err != nil {
		return err
	}

	barrier, err := c.tx.OpenJoinBarrier(ctx, c.run.ID, joinNode.ID, arr.GroupID)
	created := false
	switch {
	case errors.Is(err, ErrNotFound):
		barrierToken := Token{
			ID:            c.engine.newID(),
			NamespaceID:   c.run.NamespaceID,
			RunID:         c.run.ID,
			NodeID:        joinNode.ID,
			State:         TokenActive,
			ParentTokenID: arr.TokenID,
			GroupID:       arr.GroupID,
			CreatedAt:     c.now,
		}
		if err := c.tx.InsertToken(ctx, barrierToken); err != nil {
			return err
		}
		barrier = NodeRun{
			ID:          c.engine.newID(),
			NamespaceID: c.run.NamespaceID,
			RunID:       c.run.ID,
			TokenID:     barrierToken.ID,
			NodeID:      joinNode.ID,
			State:       NodeRunWaitingJoin,
			VisitCount:  visits[joinNode.ID] + 1,
			CreatedAt:   c.now,
			UpdatedAt:   c.now,
		}
		if err := c.tx.InsertNodeRun(ctx, barrier); err != nil {
			return err
		}
		visits[joinNode.ID]++
		created = true
	case err != nil:
		return err
	}

	if err := c.tx.InsertJoinArrival(ctx, JoinArrival{
		ID:            c.engine.newID(),
		NamespaceID:   c.run.NamespaceID,
		RunID:         c.run.ID,
		JoinNodeRunID: barrier.ID,
		GroupID:       arr.GroupID,
		TokenID:       arr.TokenID,
		FromNode:      c.nodeRun.NodeID,
		Outcome:       arr.Outcome,
		Output:        arr.Output,
		ArrivedAt:     c.now,
	}); err != nil {
		return err
	}
	count, err := c.tx.JoinArrivalCount(ctx, barrier.ID)
	if err != nil {
		return err
	}

	if err := c.emit(ctx, TypeJoinArrived, map[string]any{
		"run_id":      c.run.ID,
		"node_run_id": barrier.ID,
		"node_id":     joinNode.ID,
		"group_id":    arr.GroupID,
		"token_id":    arr.TokenID,
		"from_node":   c.nodeRun.NodeID,
		"outcome":     arr.Outcome,
		"edge":        arr.Edge.From,
		"arrivals":    count,
		"cardinality": group.Cardinality,
		"policy":      joinNode.JoinPolicy,
	}); err != nil {
		return err
	}

	c.result.JoinNodeRunID = barrier.ID
	// The run keeps moving whether or not this arrival fired the barrier —
	// either sibling branches are still working, or the join's own work is
	// now claimable.
	c.result.RunState = RunRunning
	// A subsequent arrival creates no node run, so the derived transition
	// count moves only when the barrier was opened (design §5.2's documented
	// undercount: an arrival does no dispatchable work).
	if created {
		c.result.Transitions = 0 // let finish() re-derive
	}

	threshold, satisfiable := joinNode.joinThreshold(group.Cardinality)
	if !satisfiable {
		// A quorum above the realized cardinality can never fire: guarded
		// split edges make this reachable even though the compiler saw
		// enough authored edges. Resolve loudly instead of hanging the
		// barrier open forever (design §4.3; deferred policy-aware analysis
		// is O2).
		return c.failRun(ctx, c.result.NodeRunState, fmt.Sprintf(
			"join node %q requires %d arrival(s) under policy %q but the split realized only %d branch(es); the barrier can never satisfy",
			joinNode.ID, threshold, joinNode.JoinPolicy, group.Cardinality))
	}
	if count < threshold {
		return c.finish(ctx)
	}

	// This arrival satisfied the barrier: the join's work becomes claimable,
	// and a worker completes the node run with outcome `joined` through the
	// normal fenced transaction (design D2 — completion authority stays with
	// fenced workers; the barrier never completes anything).
	if err := c.tx.UpdateNodeRun(ctx, barrier.ID, NodeRunReady, ""); err != nil {
		return err
	}
	workID, err := c.tx.EnqueueWork(ctx, barrier.ID, c.now)
	if err != nil {
		return err
	}
	if err := c.emit(ctx, TypeNodeRunReady, map[string]any{
		"run_id":      c.run.ID,
		"node_run_id": barrier.ID,
		"node_id":     joinNode.ID,
		"token_id":    barrier.TokenID,
		"work_id":     workID,
		"visit":       barrier.VisitCount,
	}); err != nil {
		return err
	}
	c.result.JoinSatisfied = true
	c.result.NextNodeID = joinNode.ID
	c.result.NextNodeRunID = barrier.ID

	// An early-firing policy (any/quorum) leaves losing branches running;
	// they are reaped explicitly and transactionally (design §4.4), and the
	// caller propagates cancellation to any async actors best-effort after
	// commit.
	if count < group.Cardinality {
		reaped, err := c.tx.ReapGroupBranches(ctx, c.run.ID, arr.GroupID, barrier.TokenID)
		if err != nil {
			return err
		}
		if err := c.emitBranchesCancelled(ctx, reaped, "join barrier satisfied before this branch arrived"); err != nil {
			return err
		}
	}
	return c.finish(ctx)
}

// emitBranchesCancelled records each reaped branch node run and remembers
// them on the result for post-commit cancellation propagation.
func (c *completion) emitBranchesCancelled(ctx context.Context, nodeRunIDs []string, detail string) error {
	for _, id := range nodeRunIDs {
		if err := c.emit(ctx, TypeBranchCancelled, map[string]any{
			"run_id":      c.run.ID,
			"node_run_id": id,
			"detail":      detail,
		}); err != nil {
			return err
		}
	}
	c.result.ReapedBranchNodeRuns = append(c.result.ReapedBranchNodeRuns, nodeRunIDs...)
	return nil
}

// reapSiblings retires every other live branch of the run when this
// completion made the run terminal (failure, bound, cancellation — design
// D6): with parallel tokens, a terminal run with dangling live branches
// would be exactly the re-dispatch zombie issue #19 fixed for cancellation.
// Sequential runs have nothing else live, so this reaps nothing and changes
// nothing for them.
func (c *completion) reapSiblings(ctx context.Context, why string) error {
	reaped, err := c.tx.ReapRunState(ctx, c.run.ID, c.nodeRun.ID)
	if err != nil {
		return err
	}
	return c.emitBranchesCancelled(ctx, reaped, why)
}
