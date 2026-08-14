package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The capacity circuit breaker (issue #48 item 1, task t9; spec claim c4,
// honesty conditions h3/h38).
//
// WHAT IT IS FOR. Task t8 gave §13.5 a capacity_exhausted class and made it
// non-retryable inside one attempt (internal/actors/errors.go): retrying a
// hard provider quota does not wait out backpressure, it burns another
// billable session against a wall that has not moved. That fixes one
// attempt. It does nothing about the next twelve work items queued for the
// same actor, each of which will start its own cold session, hit the same
// wall, and fail -- the cascade the 2026-08 fan-out actually paid for. The
// breaker is the layer above: when an actor's failure classifies
// capacity_exhausted, that ACTOR is marked unavailable until a deadline, and
// no work is dispatched to it in the meantime.
//
// WHERE THE CHECK LIVES, AND WHY IT IS THE SAME PLACE budget.go ARGUES FOR.
// budget.go's "WHY THE WORKER, NOT THE CLAIM SQL" applies here almost
// verbatim, and its warning applies to the obvious shortcut just as sharply:
// having claimWorkSQL skip rows whose actor is paused would strand the item
// -- "it stays 'ready' forever, its node run stays running, its run never
// ends, and nothing anywhere records why". The claim SQL also cannot make
// the judgement: which actor a work item is addressed to lives in the
// pinned workflow IR, not in the work_items row, so the decision needs a
// loaded dispatch. So the check happens exactly where the budget check
// happens -- on CLAIMED work, holding the fencing tuple, one step before the
// actor is invoked and before anything external is touched.
//
// WHY IT DEFERS INSTEAD OF FAILING. This is the one place the breaker
// deliberately does NOT follow budget.go. An exhausted dispatch budget is a
// verdict about the work ("this has been tried enough"); a paused actor is a
// statement about the WORLD ("the provider is out of capacity right now"),
// and the work is untouched by it. Failing the attempt would convert a
// provider's temporary refusal into a permanent node failure -- turning one
// provider limit into a cascade of failed nodes rather than a cascade of
// failed billable sessions, which is the same bug wearing a different coat.
// So the item is DEFERRED: released back to 'ready' with available_at pushed
// forward (postgres.DeferWork), and the dispatch counter it burned given
// back, because no dispatch happened.
//
// WHAT IT DOES NOT COVER YET, STATED RATHER THAN LEFT TO BE DISCOVERED. Only
// the SYNCHRONOUS dispatch path trips the breaker: capacity_exhausted
// reaches this package as a classified *actors.InvocationError, which is
// what a synchronous invocation produces (internal/actors/client.go). An
// ASYNCHRONOUS bridge that answers 202 and later reports a §13.4 `failed`
// event carrying "class":"capacity_exhausted" commits through
// internal/actors' callback handler, in the API process, which holds no
// availability store and no worker — so that path fails its attempt with
// the class recorded and does not pause the actor.
//
// This is a real gap, not a decision that async exhaustion does not matter,
// and it is worth being blunt about which dispatches fall in it: at least
// one bridge in adapters/ routes LONG work asynchronously by policy, and
// long work is exactly what the split-plan routing default sends to the
// agent fleet — so the dispatches most likely to exhaust a provider are the
// ones on the uncovered path. Closing it means giving CallbackDeps a pause
// seam and relocating the pause-policy constants below into internal/actors
// (which cannot import this package — the dependency runs the other way),
// i.e. a change to a shared protocol surface this task's brief scoped out.
// Until it is closed, the ENFORCEMENT half still protects every queued
// dispatch, sync or async, once a pause exists by any route: what is missing
// is only the async TRIP.
//
// AND IT IS NOT SILENT. budget.go's objection to a claim-time skip is that
// nothing records why. Every trip emits TypeActorPaused and every deferral
// emits TypeDispatchDeferred against the run, the pause is a readable row
// with its own provenance (actor_availability, migration 0020), and it
// renders on the actors API with a reason and an until-when. An operator who
// wonders why a run is sitting still has three places to look, all of which
// say the same thing.

// Event types the breaker emits. Both carry the "dev.culture.nodes." prefix
// §15.1 requires and, like internal/worker/runnerasync.go's own two, are
// declared here rather than in internal/events: §15.1's list is explicitly
// illustrative, and a package that does not dispatch work has no use for
// "dispatch.deferred".
const (
	// TypeActorPaused records the breaker tripping: an actor marked
	// unavailable until a deadline because a dispatch to it classified
	// capacity_exhausted.
	TypeActorPaused = "dev.culture.nodes.actor.paused"
	// TypeDispatchDeferred records one work item released without being
	// dispatched because its actor is paused, and when it will be looked at
	// again. It is emitted on EVERY deferral rather than only the first:
	// a deferral that stopped being recorded would be the silent skip
	// budget.go warns about, just with a slower onset.
	TypeDispatchDeferred = "dev.culture.nodes.dispatch.deferred"
)

// Pause durations.
const (
	// DefaultCapacityPause is how long an actor is paused when the provider
	// named no Retry-After. It is a guess and is meant to look like one: long
	// enough that a rate window or a per-session limit has plausibly moved,
	// short enough that a wrong guess costs minutes rather than an afternoon.
	// An operator who knows better clears the pause early (POST
	// /v1alpha1/actors/{id}/resume).
	DefaultCapacityPause = 15 * time.Minute
	// MaxCapacityPause caps whatever the provider asks for. A Retry-After is
	// a hint from a system with no idea what else this control plane has
	// queued, and an actor pausing itself for a day on one header would be a
	// denial of service a misconfigured bridge could trigger by accident.
	// Past the cap the work is simply re-examined sooner; if the provider is
	// still exhausted, the next dispatch trips the breaker again, which costs
	// one session rather than a day of stalled runs.
	MaxCapacityPause = 2 * time.Hour
	// MaxDeferralHorizon bounds how far one deferral pushes a work item's
	// available_at, independently of how long the pause runs.
	//
	// Deferring straight to a two-hour pause deadline would be simpler and
	// slightly cheaper, and it would make the operator's early clear useless:
	// an item already deferred to 14:30 stays invisible until 14:30 however
	// early the pause is lifted. Re-examining at most this often costs one
	// claim and one release -- no actor call, no session, nothing billable --
	// and it is what makes "clear the pause and work resumes" true rather
	// than aspirational.
	MaxDeferralHorizon = 5 * time.Minute
)

// capacityPauseUntil is when an actor that just reported capacity_exhausted
// becomes dispatchable again: the provider's own Retry-After when it named
// one (capped), the bounded default otherwise.
func capacityPauseUntil(now time.Time, retryAfter time.Duration) time.Duration {
	switch {
	case retryAfter <= 0:
		return DefaultCapacityPause
	case retryAfter > MaxCapacityPause:
		return MaxCapacityPause
	default:
		return retryAfter
	}
}

// tripCapacityBreaker marks an actor unavailable after one of its dispatches
// classified capacity_exhausted.
//
// It runs AFTER the failed attempt has been committed, and it is best-effort
// by construction: a breaker that could not be recorded is reported and the
// completion stands. The alternative -- returning the error and letting the
// dispatch loop retry -- would re-run a completion that already committed,
// which is strictly worse than a missing pause. The next dispatch to the
// same exhausted actor will trip it again.
func (w *Worker) tripCapacityBreaker(
	ctx context.Context, claimed postgres.ClaimedWork, node *nodeSpec, dc DispatchContext, invokeErr error,
) {
	actorKey := actorKeyOf(node.Uses)
	if actorKey == "" {
		// Nothing to key a pause on. A node whose `uses` names no actor key
		// never resolved an endpoint either, so there is no provider capacity
		// this failure could be about.
		w.report(fmt.Errorf(
			"worker: node %q reported capacity_exhausted but its uses %q names no actor key; no pause recorded",
			node.ID, node.Uses))
		return
	}

	retryAfter := retryAfterOf(invokeErr)
	pauseFor := capacityPauseUntil(w.opts.Now(), retryAfter)
	detail := fmt.Sprintf(
		"actor %s reported capacity_exhausted while dispatching node %q (run %s, attempt %s); "+
			"dispatch to it is paused for %s",
		actorKey, node.ID, dc.RunID, dc.AttemptID, pauseFor)

	pause, err := w.db.PauseActor(ctx, postgres.PauseActorInput{
		NamespaceID: w.opts.NamespaceID,
		ActorKey:    actorKey,
		PausedUntil: w.opts.Now().Add(pauseFor),
		Reason:      string(actors.ClassCapacityExhausted),
		RetryAfter:  retryAfter,
		Detail:      detail,
		RunID:       dc.RunID,
		NodeRunID:   dc.NodeRunID,
		AttemptID:   dc.AttemptID,
		WorkID:      claimed.ID,
	})
	if err != nil {
		w.report(fmt.Errorf("worker: pause actor %s after capacity exhaustion: %w", actorKey, err))
		return
	}

	data := map[string]any{
		"run_id":       dc.RunID,
		"node_run_id":  dc.NodeRunID,
		"node_id":      node.ID,
		"attempt_id":   dc.AttemptID,
		"actor_key":    actorKey,
		"actor_ref":    node.Uses,
		"reason":       pause.Reason,
		"paused_until": pause.PausedUntil.UTC().Format(time.RFC3339Nano),
	}
	if dc.ActorRowID != "" {
		data["actor_id"] = dc.ActorRowID
	}
	if pause.RetryAfterSeconds != nil {
		data["retry_after_seconds"] = *pause.RetryAfterSeconds
	}
	if err := w.callbacks.AppendRunEvent(ctx, w.opts.NamespaceID, dc.RunID, TypeActorPaused, data); err != nil {
		w.report(fmt.Errorf("worker: append %s event for actor %s: %w", TypeActorPaused, actorKey, err))
	}
}

// retryAfterOf extracts the delay a classified invocation failure's actor
// asked for, zero when it asked for none or when err is not one.
func retryAfterOf(err error) time.Duration {
	var invErr *actors.InvocationError
	if errors.As(err, &invErr) {
		return invErr.RetryAfter
	}
	return 0
}

// activePauseFor reports the pause in force for the actor a node names, if
// any.
//
// A lookup that fails is reported and treated as "not paused": the breaker
// is a cost optimisation over an already-bounded system (the dispatch budget
// and the node's own retry policy both still apply), and failing a dispatch
// because the breaker could not be consulted would make the safety net the
// new failure mode.
func (w *Worker) activePauseFor(ctx context.Context, node *nodeSpec) (postgres.ActorPause, bool) {
	actorKey := actorKeyOf(node.Uses)
	if actorKey == "" {
		return postgres.ActorPause{}, false
	}
	pause, ok, err := w.db.ActivePause(ctx, w.opts.NamespaceID, actorKey)
	if err != nil {
		w.report(fmt.Errorf("worker: read availability for actor %s: %w", actorKey, err))
		return postgres.ActorPause{}, false
	}
	return pause, ok
}

// deferForPause releases a claimed work item without dispatching it, because
// the actor it is addressed to is paused.
//
// The item goes back to 'ready' with available_at at min(pause deadline, now
// + MaxDeferralHorizon) -- see MaxDeferralHorizon for why the pause deadline
// alone is the wrong answer -- and the dispatch counter the claim burned is
// given back, because nothing was dispatched (postgres.DeferWork).
//
// A stale claim here is not a failure: somebody else holds the item now and
// will make the same decision. Anything else is returned, so the dispatch
// loop reports it and the lease expires into an ordinary retry.
func (w *Worker) deferForPause(
	ctx context.Context, claimed postgres.ClaimedWork, node *nodeSpec, dc DispatchContext, pause postgres.ActorPause,
) error {
	now := w.opts.Now()
	availableAt := pause.PausedUntil
	if horizon := now.Add(MaxDeferralHorizon); availableAt.After(horizon) {
		availableAt = horizon
	}

	if err := w.db.DeferWork(ctx, claimed.ID, w.opts.WorkerID, claimed.FencingToken, int(claimed.Attempt), availableAt); err != nil {
		if errors.Is(err, postgres.ErrStaleClaim) {
			return nil
		}
		return fmt.Errorf("worker: defer work %s while actor %s is paused: %w", claimed.ID, pause.ActorKey, err)
	}

	data := map[string]any{
		"run_id":       dc.RunID,
		"node_run_id":  dc.NodeRunID,
		"node_id":      node.ID,
		"work_id":      claimed.ID,
		"actor_key":    pause.ActorKey,
		"actor_ref":    node.Uses,
		"reason":       pause.Reason,
		"paused_until": pause.PausedUntil.UTC().Format(time.RFC3339Nano),
		"available_at": availableAt.UTC().Format(time.RFC3339Nano),
	}
	if pause.Detail != "" {
		data["detail"] = pause.Detail
	}
	if err := w.callbacks.AppendRunEvent(ctx, w.opts.NamespaceID, dc.RunID, TypeDispatchDeferred, data); err != nil {
		w.report(fmt.Errorf("worker: append %s event for work %s: %w", TypeDispatchDeferred, claimed.ID, err))
	}
	return nil
}
