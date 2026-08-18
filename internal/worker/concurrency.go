package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Per-actor concurrent-dispatch ceiling: "one ticket per machine" (task
// t16, issue #166's second half).
//
// WHAT WAS MEASURED BEFORE WRITING ANY OF THIS. #166 asks for two things:
// per-machine limits, and tag-pinned placement. Both need to start from how
// placement resolves today, so that was read first (internal/worker/
// registry.go, affinity.go, and internal/engine/workflow.go's affinity
// resolution):
//
//   - A node names ONE actor identity: `uses: actor://company/verifier@sha256:…`.
//     Registry.Resolve (registry.go) turns that into ONE endpoint, at the
//     actor's current (highest) revision. There is no notion of a POOL of
//     actors sharing an identity, and nothing named "tag" exists anywhere in
//     internal/worker, internal/compiler, internal/engine, or the actors
//     table's columns (migration 0001) or its open `metadata` document
//     (registry.go's authTokenEnvOf/repositoryIdentityOf are the only two
//     keys anything reads out of it today).
//   - The one placement DECISION that exists is `spec.affinity`
//     (internal/compiler's affinityRule, resolved once per run by
//     engine.Workflow.ResolveAffinity and applied by worker/affinity.go's
//     applyAffinity): a CEL condition picks between named actor
//     REFERENCES, evaluated against the triggering event. It routes to a
//     specific declared identity, chosen by a rule the workflow author
//     wrote — it is not a load-balancing or least-loaded selection among
//     interchangeable machines, and nothing in it or its config surface
//     (worker.Options) has ever taken a "tag".
//
// So "tag-pinned placement" is not a small extension of something that
// exists; it needs a NEW concept (actors registering one or more tags,
// something choosing among the tagged set — round-robin, least-loaded, an
// explicit pin recorded per subject so a follow-up run on the same Jira
// issue stays on the machine that already has its working tree) that
// touches the registry schema, ResolveAffinity, and how a run's chosen
// placement is recorded and re-read (the same durability argument
// affinity.go's runAffinity column already makes for its own, narrower,
// job). Building that under this task's seam would be forcing a placement
// redesign into a concurrency-limiting task; it is written up as follow-up
// below instead (task t16's brief explicitly allows this: "whatever needs
// deeper placement work, write up precisely as follow-up... c36's
// acceptance (h21) can then be split honestly").
//
// WHAT DOES fit this seam: a per-actor CEILING on how many dispatches may
// be concurrently in flight, which is expressible today because ONE
// existing durable fact already says "in flight" — see
// internal/store/postgres/actorconcurrency.go's CountWaitingActorInvocations
// doc comment for exactly what it counts and the ASYNC-ONLY gap it
// inherits, stated as bluntly there as breaker.go states its own identical
// gap for the SAME reason. This is deliberately a CONCURRENCY ceiling, not
// a rate (pacing.go already owns that axis, admitting up to Limit dispatches
// per Window regardless of whether earlier ones finished): a rate can only
// approximate "at most N running at once" by setting Limit=N on a window
// sized to the typical task duration, and a task that runs LONGER than the
// window still lets a second one start underneath it. This gate has no such
// approximation — it counts what is ACTUALLY still open right now.
//
// WHERE THE CHECK SITS AND WHY IT DEFERS. Exactly where the breaker's own
// per-actor check sits (dispatch.go, right after it): on CLAIMED work,
// before the registry lookup, before anything outside the control plane is
// touched. Like the breaker and pacing it DEFERS rather than fails — an
// actor already at its concurrency ceiling is a statement about capacity,
// not a verdict on the work — releasing the item to 'ready' with
// available_at pushed forward and the dispatch counter given back
// (postgres.DeferWork), for the identical reason budget.go, breaker.go, and
// pacing.go all give.
//
// WHY A FIXED POLL INTERVAL RATHER THAN pacing's CLOCK ARITHMETIC. Pacing
// can compute exactly when the next slot opens because a rate window is a
// deterministic function of the clock. A concurrency slot opens when some
// OTHER in-flight invocation to the same actor finishes — an instant this
// process cannot predict at all — so there is no RetryAt to compute, only a
// "look again soon" interval, the same shape MinPacingDeferral already
// documents as "the poll interval's own scale".

// TypeDispatchAtCapacity records one work item released without being
// dispatched because its actor's declared concurrency ceiling had no
// headroom. Deliberately its own event type rather than reusing
// TypeDispatchDeferred (the breaker's) or TypeDispatchPaced (pacing's): "the
// actor is unhealthy", "we are pacing ourselves", and "this many are
// already running" are three different facts with three different remedies,
// and an operator reading the event stream should not have to infer which
// happened from a reason string — exactly pacing.go's own argument for its
// event type.
const TypeDispatchAtCapacity = "dev.culture.nodes.dispatch.at-capacity"

// ConcurrencyPollInterval is how soon a deferred-for-capacity item is
// claimable again. There is no computed retry instant (see the package doc
// comment above), so this is a flat poll interval on the same scale as
// MinPacingDeferral: cheap to re-check (one claim, one release, no actor
// call, nothing billable), frequent enough that a slot freeing up is
// noticed promptly, without spinning the claim loop on an item that is
// almost always still blocked between one poll and the next.
const ConcurrencyPollInterval = 10 * time.Second

// ConcurrencyOptions is a deployment's declared per-actor concurrency
// ceiling. The zero value is "no ceiling", which is what every existing
// deployment gets: no actor_invocations query runs, and the dispatch path
// is byte-for-byte the one it was before this existed.
type ConcurrencyOptions struct {
	// ActorDefault caps concurrent in-flight invocations for every actor key
	// that has no entry in ActorOverrides. Zero or negative means uncapped.
	ActorDefault int
	// ActorOverrides replaces ActorDefault for the named actor keys. A key
	// present here with a non-positive value opts that actor out of the
	// default entirely — the only way to say "cap everything except this
	// one" — exactly PacingOptions.ActorOverrides' own precedent.
	ActorOverrides map[string]int
}

// Enabled reports whether any ceiling is declared at all.
func (c ConcurrencyOptions) Enabled() bool {
	if c.ActorDefault > 0 {
		return true
	}
	for _, limit := range c.ActorOverrides {
		if limit > 0 {
			return true
		}
	}
	return false
}

// forActor is the ceiling applied to one actor key: its override when it
// has one (even a non-positive override, which is how an actor opts out),
// the default otherwise.
func (c ConcurrencyOptions) forActor(actorKey string) int {
	if limit, ok := c.ActorOverrides[actorKey]; ok {
		return limit
	}
	return c.ActorDefault
}

// atActorCapacity reports whether node's resolved actor is at or over its
// configured concurrency ceiling, and how many invocations are in flight
// against it.
//
// actorRowID is the resolution the dispatch already computed via
// planSession — this never re-resolves it — and an unattributed actor ("",
// a registry with no ActorRowID capability, or a lookup that missed) is
// never capped: the ceiling needs a durable identity to count invocations
// against (actor_invocations.actor_id), and there is nothing to count for
// an actor that could not be attributed. That is the same "unattributed,
// never a dispatch failure" reading Resolve's own doc comment states.
//
// A store failure is reported and treated as "not at capacity", for
// budget.go's and pacing.go's reason: this ceiling is a cost-avoidance
// optimization over an already-bounded system, and refusing to dispatch
// because the count could not be read would make the optimization the new
// failure mode.
func (w *Worker) atActorCapacity(ctx context.Context, node *nodeSpec, actorRowID string) (limit, inFlight int, atCapacity bool) {
	if !w.opts.Concurrency.Enabled() || actorRowID == "" {
		return 0, 0, false
	}
	limit = w.opts.Concurrency.forActor(actorKeyOf(node.Uses))
	if limit <= 0 {
		return 0, 0, false
	}
	inFlight, err := w.db.CountWaitingActorInvocations(ctx, w.opts.NamespaceID, actorRowID)
	if err != nil {
		w.report(fmt.Errorf("worker: count in-flight invocations for actor %s: %w", actorRowID, err))
		return limit, 0, false
	}
	return limit, inFlight, inFlight >= limit
}

// deferForCapacity releases a claimed work item without dispatching it,
// because its actor's declared concurrency ceiling has no headroom right
// now. It is deferForPacing's and deferForPause's sibling and shares their
// guarantees: 'ready' with available_at pushed forward
// (ConcurrencyPollInterval out), the dispatch counter given back
// (postgres.DeferWork), and a stale claim treated as a no-op rather than a
// failure — somebody else holds the item now and will reach the same
// conclusion.
func (w *Worker) deferForCapacity(
	ctx context.Context, claimed postgres.ClaimedWork, node *nodeSpec, dc DispatchContext, actorRowID string, limit, inFlight int,
) error {
	availableAt := w.opts.Now().Add(ConcurrencyPollInterval)

	if err := w.db.DeferWork(ctx, claimed.ID, w.opts.WorkerID, claimed.FencingToken, int(claimed.Attempt), availableAt); err != nil {
		if errors.Is(err, postgres.ErrStaleClaim) {
			return nil
		}
		return fmt.Errorf("worker: defer work %s at actor %s's concurrency ceiling: %w", claimed.ID, actorRowID, err)
	}

	data := map[string]any{
		"run_id":       dc.RunID,
		"node_run_id":  dc.NodeRunID,
		"node_id":      node.ID,
		"work_id":      claimed.ID,
		"actor_ref":    node.Uses,
		"actor_id":     actorRowID,
		"limit":        limit,
		"in_flight":    inFlight,
		"available_at": availableAt.UTC().Format(time.RFC3339Nano),
	}
	if actorKey := actorKeyOf(node.Uses); actorKey != "" {
		data["actor_key"] = actorKey
	}
	if err := w.callbacks.AppendRunEvent(ctx, w.opts.NamespaceID, dc.RunID, TypeDispatchAtCapacity, data); err != nil {
		w.report(fmt.Errorf("worker: append %s event for work %s: %w", TypeDispatchAtCapacity, claimed.ID, err))
	}
	return nil
}
