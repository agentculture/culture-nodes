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
	// The budget check fires before this dispatch resolved anything, so the
	// exhausted attempt is unattributed ("" → NULL actor_id) — the earlier
	// dispatches that spent the budget each carry their own attribution.
	return w.failAttempt(ctx, claimed, "", engine.StatusFailed, dispatchBudgetClass, detail)
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

// ---------------------------------------------------------------------------
// The DECLARED ECONOMIC budget (task t11; issue #48 item 5, spec claims
// c6/c46, honesty h5). Everything above this line is the dispatch-count cap
// the worker imposes on itself; everything below is a contract the WORKFLOW
// AUTHOR declared and this worker enforces.
//
// WHY IT IS HERE, AND NOT SOMEWHERE CHEAPER. Every word of "WHY THE WORKER,
// NOT THE CLAIM SQL" above applies unchanged: a claim-time skip strands the
// item silently, completing an attempt requires the fencing tuple ClaimWork
// handed out, so the decision belongs on claimed work one step before the
// actor is invoked. The two checks are neighbours because they answer the
// same question at the same instant -- may this dispatch happen at all --
// with nothing external touched yet.
//
// WHAT MAKES IT DIFFERENT FROM ITS NEIGHBOURS. The dispatch cap FAILS the
// attempt (a verdict on the work) and the capacity breaker DEFERS it (a
// statement about the world). This one does neither: it REFUSES the dispatch
// and routes a name the author declared an edge from
// (engine.OutcomeBudgetExhausted). PRD §3.4 is explicit that an expected
// answer follows a graph edge rather than wearing technical failure as a
// costume, and running out of declared budget is the most expected answer
// there is -- the author wrote the number down. So the run continues down
// whatever branch the workflow declared for being unable to pay: a cheaper
// actor, a human, a summarise-and-stop node. Only a workflow that declared a
// budget and no route for exhausting it ends failed, saying so.
//
// The attempt's technical status is `policy_denied`, which is the honest
// §3.4 word: a declared policy denied this dispatch. It is also the status
// the engine never retries, which is what a refusal should be -- the budget
// would refuse the retry for exactly the same reason.
//
// WHAT "SESSION" COUNTS. `maxSessions` bounds COLD STARTS: dispatches that
// open a NEW provider session. A dispatch carrying a prior continuation ref
// (ADR 0010, migration 0018) resumes a conversation this run has already paid
// to open and charges nothing. That is not a discount, it is the only
// coherent reading: with the opposite rule a warm workstream of N node turns
// would count as N sessions and always exhaust the budget it was designed to
// conserve, so session stickiness (spec claim c3) and the economic contract
// (c6) would spend the whole time fighting each other (spec claim c46).
//
// The cold/warm decision and the outbound request's `continuation_ref` come
// from ONE lookup (sessionPlan), so the thing charged for and the thing sent
// on the wire can never disagree.
//
// WHAT IT CANNOT SEE, SAID OUT LOUD. `maxUncachedInput` spends against
// MEASURED usage, and an attempt that reported none -- a cancelled session, a
// crash with no terminal result -- burned real tokens no ceiling here can
// charge for (postgres.UsageRollup's AttemptsNotReported is a permanent
// category, not a transitional one). The measure is therefore a floor, and
// the refusal detail says how many attempts it could not see rather than
// letting the number read as complete. In the other direction the reading is
// deliberately the expensive one: an attempt that reported input tokens with
// no cached figure is charged IN FULL, because a backend reporting no cache
// telemetry is not a backend with a 0% hit rate, and a budget must not hand
// out a discount nobody demonstrated.

// budgetRefusalClass is the diagnostic class recorded in the attempt result
// of a dispatch refused for want of declared budget. It shares its name with
// the routable outcome deliberately: an operator reading the attempt row and
// an author reading the graph are looking at the same event.
const budgetRefusalClass = engine.OutcomeBudgetExhausted

// TypeDispatchRefused records one dispatch declined because the run's
// declared budget could not fund it, and which bound declined it. It is
// emitted on every refusal, for breaker.go's TypeDispatchDeferred reason: a
// refusal that stopped being recorded would be the silent skip this file's
// header refuses to allow.
const TypeDispatchRefused = "dev.culture.nodes.dispatch.refused"

// sessionPlan is what one lookup answers about the dispatch about to happen:
// which actor identity it belongs to, and whether it continues that
// identity's existing conversation or opens a new one.
type sessionPlan struct {
	// ActorRowID is the resolved actors-table row id, "" when the registry
	// cannot answer one. An unattributed dispatch is always a cold start:
	// a continuation handle belongs to an identity, and there is no identity
	// here whose conversation this would be (ADR 0010 §4).
	ActorRowID string
	// ContinuationRef is the handle this dispatch will carry, nil when there
	// is none to carry.
	ContinuationRef *string
}

// ColdStart reports whether this dispatch would open a NEW provider session
// -- the only kind `budget.maxSessions` counts.
func (p sessionPlan) ColdStart() bool { return p.ContinuationRef == nil }

// planSession resolves the actor identity and the prior conversation once,
// before anything is spent, so the budget check and the outbound §13.1
// request agree by construction.
//
// Both halves are best-effort exactly as they are on their own: an
// unresolvable row id or a failed lookup yields a cold dispatch, which costs
// more and is never wrong.
//
// It runs for every actor dispatch, budgeted or not, because the outbound
// request needs the ref regardless — these are the same two lookups
// dispatchActor always made, moved earlier. The only dispatches that pay for
// them without using them are the ones that end before the wire (a paused
// actor, a failed pre-run hook), and paying two indexed reads there is worth
// having exactly one place where "which session is this" is decided.
func (w *Worker) planSession(ctx context.Context, node *nodeSpec, dc DispatchContext) sessionPlan {
	if w.opts.Registry == nil {
		return sessionPlan{}
	}
	plan := sessionPlan{ActorRowID: w.actorRowID(ctx, node.Uses)}
	probe := dc
	probe.ActorRowID = plan.ActorRowID
	plan.ContinuationRef = w.priorContinuationRef(ctx, probe)
	return plan
}

// unfunded reports the reason this dispatch cannot be paid for, or "" when it
// can.
//
// A returned ERROR is neither of those: it means the budget could not be
// READ. That is deliberately not resolved either way here. Proceeding would
// spend money the author forbade on the strength of a database hiccup;
// refusing would route a workflow permanently down its exhaustion branch on
// the strength of the same hiccup. So the error travels back to the dispatch
// loop, which reports it and lets the lease expire into an ordinary retry --
// nothing external was touched, nothing was billed, and the transient error
// gets to be transient.
func (w *Worker) unfunded(ctx context.Context, spec *workflowSpec, node *nodeSpec, dc DispatchContext, plan sessionPlan) (string, error) {
	budget := spec.Budget
	if !budget.Declared() {
		// Unbudgeted: not a budget of zero, and not a reason to read
		// anything. An unbudgeted run pays nothing for this machinery.
		return "", nil
	}

	if budget.MaxSessions > 0 && plan.ColdStart() {
		started, err := w.db.RunSessionStarts(ctx, dc.RunID)
		if err != nil {
			return "", fmt.Errorf("worker: run %s: read session count for the declared budget: %w", dc.RunID, err)
		}
		if started+1 > budget.MaxSessions {
			return fmt.Sprintf(
				"dispatching node %q to %s would open provider session %d of this run, and the workflow declares "+
					"budget.maxSessions = %d (%d already started); the dispatch was refused before the actor was invoked. "+
					"A dispatch that could have resumed a prior session would not have been counted -- this one carried "+
					"no continuation ref, so it is a cold start",
				node.ID, node.Uses, started+1, budget.MaxSessions, started), nil
		}
	}

	if budget.MaxUncachedInput > 0 {
		spend, err := w.db.RunUncachedInput(ctx, dc.RunID)
		if err != nil {
			return "", fmt.Errorf("worker: run %s: read uncached input for the declared budget: %w", dc.RunID, err)
		}
		// The test is against spend ALREADY accrued, because what this
		// dispatch will itself consume is unknowable before it runs. The
		// ceiling therefore bounds the run at the last funded dispatch
		// rather than mid-turn.
		if spend.Tokens >= budget.MaxUncachedInput {
			return fmt.Sprintf(
				"node %q was not dispatched to %s: this run has already sent %d input tokens the provider did not "+
					"serve from cache, and the workflow declares budget.maxUncachedInput = %d. "+
					"%d of the %d attempts that reported usage reported no cache telemetry and were charged in full "+
					"(absent telemetry is not a demonstrated cache hit); %d attempts reported no usage at all and "+
					"could not be charged, so the measured figure is a floor",
				node.ID, node.Uses, spend.Tokens, budget.MaxUncachedInput,
				spend.AttemptsWithoutCacheTelemetry, spend.AttemptsReported, spend.AttemptsNotReported), nil
		}
	}

	return "", nil
}

// refuseUnfunded ends a dispatch the run's declared budget cannot pay for.
//
// It mirrors parkExhausted's shape -- cancel anything an earlier dispatch of
// this item left running (spec claim c19: a session that can no longer commit
// anything is pure spend), then commit the outcome through the ordinary
// completion path -- and differs in the one way that matters: the completion
// carries a REFUSAL OUTCOME, so a workflow that declared an edge from
// `budget_exhausted` keeps its token moving.
//
// The attempt is recorded unattributed ("" -> NULL actor_id) for
// parkExhausted's reason: no actor did anything here. Which actor the
// dispatch was addressed to is in the detail, where it is a fact about the
// refusal rather than a mark against the actor's record.
func (w *Worker) refuseUnfunded(
	ctx context.Context, claimed postgres.ClaimedWork, node *nodeSpec, dc DispatchContext, detail string,
) error {
	w.cancelPendingInvocation(ctx, claimed, node, detail)

	_, err := w.complete(ctx, claimed, engine.CompletionRequest{
		TechStatus:     engine.StatusPolicyDenied,
		RefusalOutcome: engine.OutcomeBudgetExhausted,
		Output:         diagnosticOutput(budgetRefusalClass, detail, nil),
	})
	if err != nil {
		if isStale(err) {
			// Somebody else holds the item and will make the same decision
			// against the same durable measures.
			return nil
		}
		return err
	}

	data := map[string]any{
		"run_id":      dc.RunID,
		"node_run_id": dc.NodeRunID,
		"node_id":     node.ID,
		"attempt_id":  dc.AttemptID,
		"work_id":     claimed.ID,
		"actor_ref":   node.Uses,
		"outcome":     engine.OutcomeBudgetExhausted,
		"detail":      detail,
	}
	if err := w.callbacks.AppendRunEvent(ctx, w.opts.NamespaceID, dc.RunID, TypeDispatchRefused, data); err != nil {
		w.report(fmt.Errorf("worker: append %s event for work %s: %w", TypeDispatchRefused, claimed.ID, err))
	}
	return nil
}

// chargeSession records a cold start against the run, immediately before the
// invocation that opens it.
//
// It is written only for a run whose workflow declares `budget.maxSessions`:
// the ledger exists to be spent against, and putting a row on the hot path of
// every dispatch in every unbudgeted run would be a write nothing reads.
// (Migration 0023 says the same, so a later analytics consumer does not read
// the table as a complete history of every session ever opened.)
//
// A failure here fails the DISPATCH, which is the one place in this file that
// is strict rather than best-effort. Invoking anyway would spend a session
// the budget could never see, and a budget that silently under-counts is
// worse than one that occasionally makes a worker retry a claim.
func (w *Worker) chargeSession(ctx context.Context, spec *workflowSpec, node *nodeSpec, dc DispatchContext) error {
	if spec.Budget.MaxSessions <= 0 {
		return nil
	}
	err := w.db.RecordSessionStart(ctx, postgres.SessionStart{
		AttemptID:   dc.AttemptID,
		NamespaceID: w.opts.NamespaceID,
		RunID:       dc.RunID,
		NodeRunID:   dc.NodeRunID,
		NodeKey:     node.ID,
		ActorRef:    node.Uses,
		ActorID:     dc.ActorRowID,
	})
	if err != nil {
		return fmt.Errorf("worker: charge a session for node %q of run %s: %w", node.ID, dc.RunID, err)
	}
	return nil
}
