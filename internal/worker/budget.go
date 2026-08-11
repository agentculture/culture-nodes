package worker

import (
	"context"
	"fmt"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The dispatch budget: how many times one work item may be handed to an actor
// before the worker stops trying (issue #16, spec claims c4/c19).
//
// WHAT IT BOUNDS. work_items.attempt counts dispatches of ONE work item, and
// it is incremented by exactly one statement -- claimWorkSQL -- so it moves
// only when a worker genuinely takes the item (proved by
// internal/store/postgres/claiming_test.go's TestWaitingWorkAccruesNoAttempts:
// an item parked on a healthy long-running actor accrues nothing, however
// long it waits). What makes that counter climb without end is the re-lease
// cycle: a dispatch whose completion never commits leaves the item leased,
// the lease expires, ReclaimExpired returns it to 'ready', and the next claim
// dispatches it again. Live on 2026-08-11 that reached attempt 22, each
// attempt a fresh billable agent session, for a node nobody was waiting on.
//
// MaxDispatchAttempts caps that cycle. It does NOT replace, shorten, or
// override a workflow's own declared retry policy (PRD §9.6): the engine's
// retry ladder enqueues a NEW work item per retry, each starting its own
// budget, so an author who declared maxAttempts: 3 still gets three attempts.
// The two bounds compose, and both are finite -- which is the property that
// was missing.
//
// WHY THE WORKER, NOT THE CLAIM SQL. The obvious-looking implementation --
// have claimWorkSQL skip rows whose attempt is spent -- silently strands the
// item: it stays 'ready' forever, its node run stays running, its run never
// ends, and nothing anywhere records why. PRD §3.4's vocabulary already has
// the right word for "this dispatch is over and it did not produce a domain
// answer", and the ledger-authority boundary (spec claim c8) says an
// exhausted node must reach that word through the SAME completion path every
// other technical failure takes, not through a new state or a direct write.
// Completing an attempt requires holding the claim -- CompleteAttempt's guard
// is the fencing tuple ClaimWork handed out -- so the budget check has to
// happen where that tuple exists, which is here, on claimed work, one step
// before the actor is invoked.
//
// So the budget-exhausting claim IS taken (the counter reaches
// MaxDispatchAttempts+1) and is immediately spent on ending the work rather
// than on another dispatch. Nothing is invoked, nothing is billed, and the
// run either routes its declared `failed` edge or ends failed.
//
// WHY ACTOR DISPATCH. The budget guards the kinds whose dispatch costs
// something external and unbounded -- an agent session, an HTTP action -- and
// it names no provider: `agent` and `action.http` share one dispatcher here
// (see dispatch.go) precisely so none of them gets a special case, and the
// same cap therefore applies to every registered actor identity, whatever
// executes behind it. The runner-protocol path is deliberately not
// covered this cycle (recorded as open vagueness v2): a runner operation is
// polled compute, not a per-invocation billed session, and it has its own
// deadline machinery.

// budgetExhausted reports whether this claim is one dispatch too far.
//
// The comparison is against the claim's own attempt counter, which the claim
// has already incremented: attempt N means "this is dispatch N", so
// exhaustion begins at MaxDispatchAttempts + 1.
func budgetExhausted(claimed postgres.ClaimedWork) bool {
	return int(claimed.Attempt) > MaxDispatchAttempts
}

// dispatchBudgetClass is the diagnostic class recorded in the attempt result
// of a parked, budget-exhausted dispatch. It is a class, not a status: the
// status is the ordinary technical `failed` (PRD §3.4), which is what makes
// the outcome routable by a workflow that declares an edge from it.
const dispatchBudgetClass = "dispatch_budget_exhausted"

// parkExhausted ends a work item that has spent its dispatch budget.
//
// The pending invocation is cancelled first, best-effort: the session started
// by an earlier dispatch of this item may still be running, and letting it
// finish would spend quota on a result that can no longer commit anything
// (spec claim c19). Whether that cancellation succeeds, fails, or cannot even
// be addressed changes nothing about the parking that follows -- a worker
// that let an unreachable actor block the parking would have reinstated
// exactly the loop this budget exists to stop.
func (w *Worker) parkExhausted(ctx context.Context, claimed postgres.ClaimedWork, node *nodeSpec, dc DispatchContext) error {
	detail := fmt.Sprintf(
		"node %q has been dispatched %d times for this work item without a committed completion; "+
			"the dispatch budget of %d is spent, so this attempt was not dispatched (work %s, node run %s)",
		node.ID, MaxDispatchAttempts, MaxDispatchAttempts, claimed.ID, dc.NodeRunID)

	w.cancelPendingInvocation(ctx, claimed, node, detail)
	return w.failAttempt(ctx, claimed, engine.StatusFailed, dispatchBudgetClass, detail)
}

// cancelPendingInvocation asks the actor to stop whatever an earlier dispatch
// of this work item started (PRD §13.6).
//
// Every way this can fail -- no invocation recorded, an actor reference that
// no longer resolves, an actor that refuses or is unreachable -- is reported
// and swallowed. §13.6 is explicit that workflow state does not depend on an
// external process acknowledging cancellation, and here the control plane is
// about to record the node's end regardless.
func (w *Worker) cancelPendingInvocation(ctx context.Context, claimed postgres.ClaimedWork, node *nodeSpec, reason string) {
	inv, ok, err := w.db.PendingInvocationForWork(ctx, claimed.ID)
	if err != nil {
		w.report(fmt.Errorf("worker: budget exhausted for work %s: look up pending invocation: %w", claimed.ID, err))
		return
	}
	if !ok || inv.InvocationID == "" {
		// Nothing in flight: either the dispatch never reached an actor, or
		// the actor accepted without naming an invocation there is no way to
		// address a cancellation to.
		return
	}

	// The invocation records the reference that was actually dispatched,
	// which is the honest thing to cancel against; the node's own `uses` is
	// the fallback for a row written before that was recorded.
	ref := inv.ActorRef
	if ref == "" {
		ref = node.Uses
	}
	if w.opts.Registry == nil {
		w.report(fmt.Errorf("worker: budget exhausted for work %s: cannot cancel invocation %s: no actor registry configured",
			claimed.ID, inv.InvocationID))
		return
	}
	endpoint, err := w.opts.Registry.Resolve(ctx, ref)
	if err != nil {
		w.report(fmt.Errorf("worker: budget exhausted for work %s: cannot address a cancellation for invocation %s (%s): %w",
			claimed.ID, inv.InvocationID, ref, err))
		return
	}
	if err := w.opts.Client.Cancel(ctx, endpoint, inv.InvocationID, reason); err != nil {
		w.report(fmt.Errorf("worker: budget exhausted for work %s: cancelling invocation %s was refused: %w",
			claimed.ID, inv.InvocationID, err))
	}
}
