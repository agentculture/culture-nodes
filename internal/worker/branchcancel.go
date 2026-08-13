package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Propagating a sibling-branch reap to in-flight actor sessions (issue #43,
// parallel-tokens design §4.4).
//
// When a completion reaps sibling branches — the losers of an `any`/`quorum`
// barrier, or every remaining branch when a terminal failure, a bound, or a
// cancellation ended the run — the reap itself is explicit and transactional:
// the branches' tokens are consumed, their node runs cancelled, their work
// items cancelled, all inside the completing transaction. The run's
// authoritative state is durably safe the moment that commits, and a reaped
// branch's actor that reports anyway hits the engine's existing fenced and
// terminal guards and leaves no trace.
//
// What is NOT safe by itself is the actor: nothing has told it to stop, so a
// reaped branch's agent keeps burning compute on work whose result no one
// will ever read. §13.6 gives exactly the tool, and makes it best-effort
// ("workflow state does not depend on an external process acknowledging
// cancellation"). This file is the worker-side mirror of the API's
// cancelRun PROPAGATE step (internal/api/cancelpropagate.go), narrowed from
// "every invocation of a run" to "the invocations of these node runs",
// because a reap is per-branch: an `any` barrier reaps its losers while the
// run itself stays perfectly healthy.
//
// No HTTP happens inside the §12.5 transaction — the engine never calls an
// actor — so this runs after CompleteAttempt returns, on whatever result it
// committed. Every failure here is recorded or logged and none is ever
// returned: a propagation that could fail a completion would make the
// barrier's correctness depend on a remote endpoint.

// TypeBranchCancelRequested records that a worker asked an actor to stop an
// invocation belonging to a reaped sibling branch. It is distinct from the
// engine's dev.culture.nodes.branch.cancelled (which records that the
// control plane retired the branch — committed state) exactly the way the
// API's actor.cancel-requested is distinct from run.cancelled: one is what
// the control plane DID, the other is what it SENT, and the two can honestly
// disagree.
const TypeBranchCancelRequested = "dev.culture.nodes.branch.cancel-requested"

// branchCancelTimeout bounds one actor's Cancel round trip, matching the
// API's cancelPropagateTimeout for the same measured reason: a production
// bridge answers an idle cancel in seconds and can exceed ten mid-session.
const branchCancelTimeout = 30 * time.Second

// reapedInvocation is the slice of an actor_invocations row this step needs.
type reapedInvocation struct {
	AttemptID    string
	NodeRunID    string
	NodeKey      string
	ActorRef     string
	InvocationID string
}

// propagateBranchCancellations asks the actors of every reaped branch node
// run to stop. It returns nothing: the caller has already committed, and
// there is no answer that could change what it committed.
func (w *Worker) propagateBranchCancellations(ctx context.Context, result engine.CompletionResult) {
	if len(result.ReapedBranchNodeRuns) == 0 {
		return
	}
	invocations, err := w.waitingInvocations(ctx, result.ReapedBranchNodeRuns)
	if err != nil {
		w.report(fmt.Errorf("worker: branch cancel propagation: loading waiting invocations: %w", err))
		return
	}
	for _, inv := range invocations {
		w.cancelReapedInvocation(ctx, result.RunID, inv)
	}
}

// waitingInvocations loads the actor invocations still waiting on an actor
// for the given node runs — the same actors.InvocationWaiting question
// actor_invocations_waiting_idx (migrations/0009) serves, narrowed to a set
// of node runs rather than to a run.
func (w *Worker) waitingInvocations(ctx context.Context, nodeRunIDs []string) ([]reapedInvocation, error) {
	rows, err := w.db.Pool().Query(ctx, `
		SELECT attempt_id, node_run_id, node_key, COALESCE(actor_ref, ''), COALESCE(invocation_id, '')
		FROM actor_invocations
		WHERE namespace_id = $1 AND node_run_id = ANY($2) AND state = $3`,
		w.opts.NamespaceID, nodeRunIDs, actors.InvocationWaiting,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []reapedInvocation
	for rows.Next() {
		var inv reapedInvocation
		if err := rows.Scan(&inv.AttemptID, &inv.NodeRunID, &inv.NodeKey, &inv.ActorRef, &inv.InvocationID); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// cancelReapedInvocation resolves and cancels one invocation, then records
// exactly one event whatever happened — an attempted-but-failed Cancel is
// still evidence worth keeping.
func (w *Worker) cancelReapedInvocation(ctx context.Context, runID string, inv reapedInvocation) {
	var outcome, detail string
	switch {
	case inv.ActorRef == "":
		outcome, detail = "skipped", "invocation names no actor_ref to resolve"
	case inv.InvocationID == "":
		outcome, detail = "skipped", "invocation has no actor-assigned invocation_id to cancel"
	default:
		endpoint, err := w.opts.Registry.Resolve(ctx, inv.ActorRef)
		if err != nil {
			outcome, detail = "failed", fmt.Sprintf("resolve actor %q: %v", inv.ActorRef, err)
			break
		}
		cancelCtx, done := context.WithTimeout(ctx, branchCancelTimeout)
		cancelErr := w.opts.Client.Cancel(cancelCtx, endpoint, inv.InvocationID,
			fmt.Sprintf("branch %s of run %s was reaped", inv.NodeKey, runID))
		done()
		if cancelErr != nil {
			outcome, detail = "failed", cancelErr.Error()
		} else {
			outcome = "sent"
		}
	}

	if outcome == "failed" {
		w.report(fmt.Errorf("worker: branch cancel propagation: node run %s (%s): %s", inv.NodeRunID, inv.ActorRef, detail))
	}

	data, err := json.Marshal(map[string]any{
		"run_id":        runID,
		"node_run_id":   inv.NodeRunID,
		"node_id":       inv.NodeKey,
		"attempt_id":    inv.AttemptID,
		"actor_ref":     inv.ActorRef,
		"invocation_id": inv.InvocationID,
		"outcome":       outcome,
		"detail":        detail,
	})
	if err != nil {
		data = []byte(`{}`)
	}
	// Best-effort in both directions: failing to append this diagnostic must
	// not surface anywhere the completion could see it.
	_, _ = w.db.InsertEvent(ctx, postgres.InsertEventInput{
		NamespaceID:   w.opts.NamespaceID,
		AggregateType: "run",
		AggregateID:   runID,
		EventType:     TypeBranchCancelRequested,
		Data:          data,
	})
}
