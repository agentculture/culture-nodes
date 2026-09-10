package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/repair"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Lane liveness at the dispatch site (plan loop-closure t10; spec c4, c23,
// c26, c33, honesty h13/h20; decision c43's "both" shape).
//
// WHAT IT IS FOR. Both production lanes answered /healthz 200 with a spent
// refresh token, and a whole wave found out by dispatching into them (#308).
// The bridge's own /v1/capabilities host block now carries a `liveness` fact
// (t9), the mesh collector persists it as an actor_liveness row (migration
// 0058, internal/mesh + internal/api/liveness.go), and THIS file is where
// the worker reads that row one step before resolving an endpoint — so a
// lane whose session cannot start is routed around instead of paid for.
//
// THE RULE, IN ONE PLACE. postgres.ActorLiveness.Live: a row is not live when
// it is LOCKED (any age — a lock does not expire by time), when it says
// session_ok=false from a LOCK-mode bridge (any age — the bridge latched
// with a fixed checked_at, so the fact's age is the latch's, not the
// lane's; code-review fix D), or when it says session_ok=false from a
// CHECK-mode bridge and was checked inside postgres.LivenessFreshness (5
// minutes). A missing row, an unmeasured row, a true, or a STALE CHECK-mode
// false all proceed exactly as today: refusing on a fact a re-measuring
// bridge has not repeated would make the safety net the new failure mode,
// and the row is a cost optimisation over an already-bounded system.
//
// WHAT HAPPENS WHEN THE LANE IS NOT LIVE. If the actor's registration names
// `metadata.fallback_actor`, the dispatch goes to the fallback — same node,
// same contract, same retry policy, different lane — and a derived routing
// record in internal/repair's shape (router=lane_liveness, selected=fallback)
// says so, naming both actors. If it names none (or the fallback is not live
// either), the dispatch PROCEEDS into the lane and a warning-shaped record
// (selected=proceed) says why: a refusal here would turn one lane's login
// state into a dead node run, which is the cascade in a different coat.
//
// WHO CLOSES AND WHO OPENS THE LANE (c43). Locking is an OR: the bridge sets
// session_ok=false the moment it classifies the spent-credential text, and
// the control plane locks the row when an attempt's class says
// credential_spent — from the SYNCHRONOUS path (dispatchActor calls
// lockLaneOnCredentialSpent below) and from the ASYNCHRONOUS one (the
// callback ingest, internal/actors/lanelock.go), both through the one
// shared helper, because production codex is async and a lock only the
// sync path could write never landed there (code-review fix D). Whichever
// side sees it first closes the lane. Unlocking is an AND: POST
// /v1alpha1/actors/{id}/resume clears the control-plane lock, and the lane
// leases again only when a subsequent collector write reports
// session_ok=true. Resume alone never reopens a lane whose last fact is
// false (internal/api/liveness.go).
//
// WHERE IN THE DISPATCH IT RUNS. The lane is DECIDED (decideLane) right
// after the session plan and before every per-actor gate — breaker,
// concurrency ceiling, clarify gate, pacing — so those gates judge the lane
// that will actually be invoked: a paused or rate-limited fallback is
// deferred like any other paused actor, and actor_invocations.actor_ref,
// the session charge and the breaker's own trip all name the fallback. The
// decision is RECORDED (recordLaneDecision) immediately before the endpoint
// is resolved, so a deferral does not leave a routing record for a dispatch
// that never happened.

// laneDecision is what decideLane decided for one dispatch, before any
// record is composed or any endpoint resolved.
type laneDecision struct {
	// target is the reference to resolve and invoke: node.Uses, or the
	// fallback when one was taken.
	target string
	// session is the plan to dispatch under: the caller's, or a cold plan
	// on the fallback's row when the lane was rerouted.
	session sessionPlan
	// primaryRowID is the node's own actor row, kept for attributing a
	// failure to record the reroute (nothing was dispatched anywhere).
	primaryRowID string
	// routed is true when a record must be appended (the lane was not
	// live); selected says which way it went.
	routed   bool
	selected repair.LivenessSelected
	input    repair.LivenessInput
	fallback string
}

// decideLane reads the persisted row for the actor a node names and decides
// where the dispatch goes. It reads and never writes: the decision is
// applied by the caller (dc.ActorRef, the session plan) and recorded later
// by recordLaneDecision.
//
// A row that cannot be READ proceeds: the gate is best-effort in the same
// way activePauseFor is, and for the same reason. A registry that cannot
// answer the metadata question (StaticRegistry) proceeds on a not-live row
// with the warning record — it can refuse nothing because it can name no
// fallback, and saying so is still worth a record.
func (w *Worker) decideLane(ctx context.Context, node *nodeSpec, dc DispatchContext, session sessionPlan) laneDecision {
	decision := laneDecision{target: node.Uses, session: session, primaryRowID: session.ActorRowID}
	actorKey := actorKeyOf(node.Uses)
	if actorKey == "" {
		return decision
	}
	now := w.opts.Now()
	row, found, err := w.db.ActorLiveness(ctx, w.opts.NamespaceID, actorKey)
	if err != nil {
		w.report(fmt.Errorf("worker: read liveness for actor %s: %w", actorKey, err))
		return decision
	}
	if !found || row.Live(now) {
		return decision
	}

	decision.routed, decision.selected = true, repair.LivenessSelectedProceed
	decision.input = repair.LivenessInput{
		RunID: dc.RunID, NodeRunID: dc.NodeRunID, AttemptID: dc.AttemptID, NodeID: node.ID,
		Lane: repair.LivenessLane{
			ActorKey: actorKey, ActorRef: node.Uses,
			SessionOK: row.SessionOK != nil && *row.SessionOK,
			Reason:    row.Reason, Mode: row.Mode, Locked: row.Locked,
			CheckedAt: row.CheckedAt, Source: row.Source,
		},
		FreshnessWindow: postgres.LivenessFreshness,
		RouterActorID:   w.dispatchGateActorID(),
		Now:             now,
	}

	fallback := w.fallbackActorFor(ctx, node.Uses)
	if fallback == "" {
		return decision
	}
	decision.input.FallbackActorKey = actorKeyOf(fallback)
	decision.input.FallbackActorRef = fallback
	fbRow, fbFound, fbErr := w.db.ActorLiveness(ctx, w.opts.NamespaceID, actorKeyOf(fallback))
	switch {
	case fbErr != nil:
		// Unknown is treated as live for the fallback exactly as for the
		// lane: the alternative is proceeding into a lane KNOWN to be dead
		// on the strength of a read that failed.
		w.report(fmt.Errorf("worker: read liveness for fallback actor %s: %w", fallback, fbErr))
	case fbFound && !fbRow.Live(now):
		decision.input.FallbackNotLive = fmt.Sprintf("session_ok=%t reason=%s locked=%t",
			fbRow.SessionOK != nil && *fbRow.SessionOK, fbRow.Reason, fbRow.Locked)
		return decision
	}
	// The fallback is a different identity: attribution follows it, and a
	// continuation handle from the original lane's conversation is not one
	// the fallback can resume, so the dispatch is a cold start there.
	decision.selected, decision.target, decision.fallback = repair.LivenessSelectedFallback, fallback, fallback
	decision.session = sessionPlan{ActorRowID: w.actorRowID(ctx, fallback)}
	return decision
}

// recordLaneDecision appends the derived routing record and the run event
// for a decision that rerouted or warned. False means the caller must
// return: the claimed work item has been disposed of, or the error is the
// caller's to propagate. A decision that proceeded on a live lane records
// nothing.
func (w *Worker) recordLaneDecision(
	ctx context.Context,
	claimed postgres.ClaimedWork,
	node *nodeSpec,
	dc DispatchContext,
	decision laneDecision,
) (bool, error) {
	if !decision.routed {
		return true, nil
	}
	record, err := repair.LivenessRouting{Selected: decision.selected}.Record(decision.input)
	if err != nil {
		return false, fmt.Errorf("worker: compose liveness routing for node run %s: %w", dc.NodeRunID, err)
	}
	if _, err := w.ledger.Append(ctx, record); err != nil {
		if decision.selected == repair.LivenessSelectedFallback {
			// A reroute that cannot be recorded must not happen silently:
			// the record IS the accountability for sending this node's
			// work to an actor the author did not name. Same shape as the
			// preflight gate's own producer-identity failure.
			return false, w.failAttempt(ctx, claimed, decision.primaryRowID, engine.StatusFailed, "configuration",
				fmt.Sprintf("node %q uses %q, whose lane is not live and whose registration names fallback %q, "+
					"but the routing record could not be appended: %v. The router's producer identity %q must be a "+
					"registered actor (ledger_records.origin_actor_id references actors(id)); register it with kind "+
					"`engine` and no endpoint, or set the worker's DispatchGateActorID to an identity that is",
					node.ID, node.Uses, decision.fallback, err, w.dispatchGateActorID()))
		}
		// The warning changes nothing about where the work goes; a missing
		// warning is reported, not fatal.
		w.report(fmt.Errorf("worker: append liveness warning for node run %s: %w", dc.NodeRunID, err))
	}
	w.recordLivenessRouted(ctx, dc, node, decision)
	return true, nil
}

// routedFrom marks a per-actor gate's event with the node's own reference
// when the dispatch it judged had been rerouted to a fallback, so a
// deferral that names company/fallback also says which node lane it stood
// in for.
func routedFrom(data map[string]any, node *nodeSpec, dc DispatchContext) {
	if dc.ActorRef != "" && dc.ActorRef != node.Uses {
		data["routed_from"] = node.Uses
	}
}

// fallbackActorFor reads `metadata.fallback_actor` from the actor's current
// registration, "" when it names none or the registry cannot answer.
func (w *Worker) fallbackActorFor(ctx context.Context, ref string) string {
	resolver, ok := w.opts.Registry.(preflightConfigResolver)
	if !ok {
		return ""
	}
	_, metadata, err := resolver.PreflightConfig(ctx, ref)
	if err != nil {
		w.report(fmt.Errorf("worker: read fallback_actor for %q: %w", ref, err))
		return ""
	}
	return fallbackActorOf(metadata)
}

// TypeLaneRouted records the liveness gate's decision as a run event
// beside the ledger record, so the events stream sees a reroute the same
// way it sees a deferral.
const TypeLaneRouted = "dev.culture.nodes.dispatch.lane_routed"

func (w *Worker) recordLivenessRouted(ctx context.Context, dc DispatchContext, node *nodeSpec, d laneDecision) {
	data := map[string]any{
		"run_id":      dc.RunID,
		"node_run_id": dc.NodeRunID,
		"node_id":     node.ID,
		"attempt_id":  dc.AttemptID,
		"actor_ref":   node.Uses,
		"actor_key":   d.input.Lane.ActorKey,
		"selected":    string(d.selected),
		"reason":      string(repair.ReasonLaneNotLive),
		"target":      d.target,
		"liveness": map[string]any{
			"session_ok": d.input.Lane.SessionOK,
			"reason":     d.input.Lane.Reason,
			"locked":     d.input.Lane.Locked,
			"checked_at": d.input.Lane.CheckedAt.UTC().Format(time.RFC3339Nano),
		},
	}
	if d.input.FallbackActorKey != "" {
		data["fallback_actor_key"] = d.input.FallbackActorKey
	}
	if err := w.callbacks.AppendRunEvent(ctx, w.opts.NamespaceID, dc.RunID, TypeLaneRouted, data); err != nil {
		w.report(fmt.Errorf("worker: append %s event for node run %s: %w", TypeLaneRouted, dc.NodeRunID, err))
	}
}

// LivenessLockReason is the reason a control-plane lock carries, shared with
// the asynchronous path (internal/actors/lanelock.go).
const LivenessLockReason = actors.LivenessLockReason

// TypeLaneLocked records the control plane locking a lane after a
// credential_spent attempt, from either path.
const TypeLaneLocked = actors.TypeLaneLocked

// lockLaneOnCredentialSpent is the synchronous path's call into decision
// c43's OR rule: an attempt that failed with class credential_spent locks
// the liveness row of the lane that was actually invoked (dc.ActorRef —
// the fallback, when one was taken). It runs AFTER the failed attempt
// committed and is best-effort exactly like tripCapacityBreaker: a lock
// that could not be written is reported and the completion stands. The
// write itself is actors.LockLaneOnCredentialSpent, the same function the
// callback ingest calls for an asynchronous credential_spent, so the two
// paths cannot lock different rows or record different events.
func (w *Worker) lockLaneOnCredentialSpent(ctx context.Context, claimed postgres.ClaimedWork, node *nodeSpec, dc DispatchContext, actorRef string) {
	err := actors.LockLaneOnCredentialSpent(ctx, w.callbacks, w.callbacks, actors.LaneLock{
		NamespaceID: w.opts.NamespaceID,
		ActorRef:    actorRef,
		RunID:       dc.RunID,
		NodeRunID:   dc.NodeRunID,
		NodeID:      node.ID,
		AttemptID:   dc.AttemptID,
		WorkID:      claimed.ID,
		CheckedAt:   w.opts.Now(),
	})
	if err != nil {
		w.report(fmt.Errorf("worker: %w", err))
	}
}
