package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/pacing"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Dispatch pacing (issue #48 item 2, task t10; spec claims c5/c43, honesty
// conditions h4/h36).
//
// WHAT IT IS FOR. The worker loop is deliberately anti-pacing, and says so:
// "A tick that found work claims again immediately: a backlog should drain at
// the speed of dispatch, not at the speed of the poll" (worker.go's Run).
// That is the right rule for work whose cost is a database write. It is the
// wrong rule for work that starts a billable provider session against a
// subscription that holds a fixed number of them per window: a fan-out of
// twenty nodes drains its backlog in seconds and the window is gone before
// the wave is half done, which is what the 2026-08 cycle actually paid for
// (issues #47/#48). t9's breaker is the reaction to hitting that wall; pacing
// is the attempt not to hit it.
//
// WHY THE STATE IS IN THE DATABASE. Because "a process may run several, and
// several processes may run one each" (worker.go's Worker doc comment). An
// in-memory limiter in N workers enforces N times the declared rate, and does
// it invisibly. So the rate lives in one table every worker shares, and the
// decision is made under that row's lock -- see migration 0022 and
// internal/store/postgres/dispatchrate.go. The arithmetic is
// internal/pacing's, which is where the reset-clock reasoning lives.
//
// WHERE THE CHECK SITS, AND WHY IT DEFERS. Exactly where t9's breaker check
// sits, one step behind it, for exactly budget.go's reason: on CLAIMED work,
// holding the fencing tuple, before anything outside the control plane has
// been touched. And like the breaker -- not like the budget -- it DEFERS
// rather than fails. A paced dispatch is a statement about the clock, not a
// verdict on the work: the work is fine, it is simply not this work's turn.
// So the item goes back to 'ready' with available_at pushed forward through
// postgres.DeferWork, and the dispatch counter the claim burned is given back,
// because nothing was dispatched. A pacing control that spent a node's
// dispatch budget while refusing to dispatch it would fail the node for being
// paced, which would be a worse bug than the one it fixes.
//
// WHAT IS PACED AND WHAT IS NOT. Only actor dispatch -- `agent` and
// `action.http`, the two kinds whose dispatch starts something billable
// outside this process (see dispatch.go, which handles both in one path
// precisely so neither gets a special case). Decision nodes, code nodes,
// waits and hook runs are not paced: they cost what a database write costs,
// and pacing them would be latency in exchange for nothing.
//
// THE SLOT IS SPENT ON THE DECISION TO DISPATCH, NOT ON THE INVOCATION. The
// check runs before the registry lookup and before any pre_run hook, which
// means a dispatch that then fails to resolve an endpoint has still consumed
// a slot. That is deliberate in both directions. Placing it later -- after a
// pre_run hook had already executed -- would mean deferring work whose hook
// has run, and re-running that hook when the item came back; a hook is
// allowed to have side effects, so replaying one to save a rate slot is not a
// trade worth making. The cost is that a misconfigured actor can burn slots
// without ever reaching a provider, which shows up on the operator surface as
// consumption with no completed attempts behind it.

// Event type the pacing control emits.
const (
	// TypeDispatchPaced records one work item released without being
	// dispatched because a declared dispatch rate had no headroom, and when
	// it will be looked at again.
	//
	// It is deliberately NOT TypeDispatchDeferred (t9's breaker event) even
	// though the mechanism is identical: "the provider refused us" and "we
	// are holding ourselves back" are different facts with different
	// remedies, and an operator reading the event stream should not have to
	// infer which one happened from a reason string. Like the breaker's, it
	// is emitted on EVERY deferral rather than only the first -- a deferral
	// that stopped being recorded would be the silent skip budget.go warns
	// about, just with a slower onset.
	TypeDispatchPaced = "dev.culture.nodes.dispatch.paced"
)

// Pacing deferral bounds.
const (
	// MinPacingDeferral floors how long a paced item waits before it is
	// claimable again. A retry instant already in the past (a slot that
	// opened between the decision and the write, a clock that moved) would
	// otherwise put the item straight back in the next claim batch, and the
	// loop would spin on it -- claiming, deferring, claiming -- burning
	// database round trips to enforce a rate. A second is the poll interval's
	// own scale, so a floored deferral costs no more than an idle tick.
	MinPacingDeferral = time.Second
	// MaxPacingDeferral bounds the other end, for MaxDeferralHorizon's reason
	// (see breaker.go): an item deferred straight to a window reset five
	// hours out is invisible for five hours, and every operator action that
	// could change the answer in the meantime -- raising the limit,
	// restarting a worker with a new configuration -- would appear to do
	// nothing. Re-examining at most this often costs one claim and one
	// release: no actor call, no session, nothing billable.
	MaxPacingDeferral = 5 * time.Minute
)

// PacingOptions is a deployment's declared dispatch rates.
//
// Every field is optional and the zero value is "no pacing", which is what
// every existing deployment gets: no rate rows are written, no transaction is
// opened, and the dispatch path is byte-for-byte the one it was before this
// existed.
type PacingOptions struct {
	// Global is the whole installation's session rate -- the meter that
	// matters when several actors draw on ONE subscription pool, which is
	// exactly the situation issue #48 describes ("the operator's interactive
	// session, local subagents, and all bridge sessions on the same account
	// share ONE subscription session window").
	Global pacing.Config
	// Actor is the default rate applied to each actor key separately. It is
	// per key, not shared: two actors each get their own budget.
	Actor pacing.Config
	// ActorOverrides replaces Actor for the named actor keys. A key present
	// here with a disabled config (no limit) opts that actor out of the
	// default rate entirely, which is the only way to say "pace everything
	// except this one".
	ActorOverrides map[string]pacing.Config
}

// Enabled reports whether any rate is declared at all. A worker whose pacing
// is not enabled never touches the rate table.
func (p PacingOptions) Enabled() bool {
	if p.Global.Enabled() || p.Actor.Enabled() {
		return true
	}
	for _, cfg := range p.ActorOverrides {
		if cfg.Enabled() {
			return true
		}
	}
	return false
}

// forActor is the rate applied to one actor key: its override when it has
// one (even a disabled override, which is how an actor opts out), the
// per-actor default otherwise.
func (p PacingOptions) forActor(actorKey string) pacing.Config {
	if cfg, ok := p.ActorOverrides[actorKey]; ok {
		return cfg
	}
	return p.Actor
}

// requests is the set of rate scopes one dispatch to actorKey must find
// headroom in. Disabled configurations are included and filtered by the store,
// which is where "disabled" is already defined.
func (p PacingOptions) requests(actorKey string) []postgres.RateRequest {
	reqs := []postgres.RateRequest{{Scope: postgres.RateScopeGlobal, Config: p.Global}}
	if actorKey != "" {
		reqs = append(reqs, postgres.RateRequest{
			Scope: postgres.RateScopeActor, ScopeKey: actorKey, Config: p.forActor(actorKey),
		})
	}
	return reqs
}

// consumeDispatchSlot asks the declared rates for headroom for one dispatch
// and consumes a slot in every applicable scope when they all have it.
//
// A store failure is reported and treated as "allowed", for the reason
// activePauseFor treats an unreadable pause as "not paused": pacing is a cost
// optimisation over an already-bounded system, and refusing to dispatch
// because the rate limiter could not be consulted would make the safety net
// the new failure mode. An installation that would rather stall than overspend
// can say so by declaring a rate the provider itself enforces -- t9's breaker
// is the backstop either way.
func (w *Worker) consumeDispatchSlot(ctx context.Context, node *nodeSpec) (postgres.DispatchRateDecision, bool) {
	if !w.opts.Pacing.Enabled() {
		return postgres.DispatchRateDecision{Allowed: true}, true
	}
	decision, err := w.db.ConsumeDispatchSlots(ctx, w.opts.NamespaceID, w.opts.Pacing.requests(actorKeyOf(node.Uses)))
	if err != nil {
		w.report(fmt.Errorf("worker: consult the dispatch rate for node %q: %w", node.ID, err))
		return postgres.DispatchRateDecision{Allowed: true}, true
	}
	return decision, decision.Allowed
}

// deferForPacing releases a claimed work item without dispatching it, because
// a declared dispatch rate has no headroom right now.
//
// It is deferForPause's sibling and shares its guarantees: the item goes back
// to 'ready' with available_at pushed forward, the dispatch counter is given
// back (postgres.DeferWork), and a stale claim is not a failure -- somebody
// else holds the item now and will reach the same conclusion.
func (w *Worker) deferForPacing(
	ctx context.Context, claimed postgres.ClaimedWork, node *nodeSpec, dc DispatchContext,
	decision postgres.DispatchRateDecision,
) error {
	now := w.opts.Now()
	availableAt := decision.RetryAt
	if floor := now.Add(MinPacingDeferral); availableAt.Before(floor) {
		availableAt = floor
	}
	if horizon := now.Add(MaxPacingDeferral); availableAt.After(horizon) {
		availableAt = horizon
	}

	if err := w.db.DeferWork(ctx, claimed.ID, w.opts.WorkerID, claimed.FencingToken, int(claimed.Attempt), availableAt); err != nil {
		if errors.Is(err, postgres.ErrStaleClaim) {
			return nil
		}
		return fmt.Errorf("worker: defer work %s under dispatch rate %s/%s: %w",
			claimed.ID, decision.Scope, decision.ScopeKey, err)
	}

	data := map[string]any{
		"run_id":         dc.RunID,
		"node_run_id":    dc.NodeRunID,
		"node_id":        node.ID,
		"work_id":        claimed.ID,
		"actor_ref":      node.Uses,
		"scope":          decision.Scope,
		"reason":         decision.Reason,
		"retry_at":       decision.RetryAt.UTC().Format(time.RFC3339Nano),
		"available_at":   availableAt.UTC().Format(time.RFC3339Nano),
		"limit":          decision.Limit,
		"dispatched":     decision.Dispatched,
		"allowance":      decision.Allowance,
		"window_ends_at": decision.Window.End.UTC().Format(time.RFC3339Nano),
	}
	if decision.ScopeKey != "" {
		data["scope_key"] = decision.ScopeKey
	}
	if actorKey := actorKeyOf(node.Uses); actorKey != "" {
		data["actor_key"] = actorKey
	}
	if err := w.callbacks.AppendRunEvent(ctx, w.opts.NamespaceID, dc.RunID, TypeDispatchPaced, data); err != nil {
		w.report(fmt.Errorf("worker: append %s event for work %s: %w", TypeDispatchPaced, claimed.ID, err))
	}
	return nil
}
