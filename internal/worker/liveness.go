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
// it is LOCKED (any age — a lock does not expire by time) or when it says
// session_ok=false and was checked inside postgres.LivenessFreshness (5
// minutes). A missing row, an unmeasured row, a true, or a STALE false all
// proceed exactly as today: refusing on a fact nobody has re-measured would
// make the safety net the new failure mode, and the row is a cost
// optimisation over an already-bounded system.
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
// completeFromInvocationError calls lockLaneOnCredentialSpent below when an
// attempt's class says credential_spent — whichever side sees it first
// closes the lane. Unlocking is an AND: POST /v1alpha1/actors/{id}/resume
// clears the control-plane lock, and the lane leases again only when a
// subsequent collector write reports session_ok=true. Resume alone never
// reopens a lane whose last fact is false (internal/api/liveness.go).
//
// WHAT IT DOES NOT COVER, STATED. Like the breaker's trip, only the
// SYNCHRONOUS path locks the row: an asynchronous bridge that answers 202
// and later reports class credential_spent through the callback handler
// commits in the API process, which holds no worker. The enforcement half
// (this gate) still protects every dispatch once a row exists by any route —
// including the collector's, which is the path that does not need the
// worker to have been burned first.

// livenessDecision is what the gate decided for one dispatch, before any
// record is composed or any endpoint resolved.
type livenessDecision struct {
	// target is the reference to resolve and invoke: node.Uses, or the
	// fallback when one was taken.
	target string
	// routed is true when a record must be appended (the lane was not
	// live); selected says which way it went.
	routed   bool
	selected repair.LivenessSelected
	input    repair.LivenessInput
}

// livenessGate reads the persisted row for the actor a node names and
// decides where the dispatch goes. It reports the resolved target and,
// when the lane was rerouted, the session plan to dispatch under; false
// means the caller must return (the claimed work item has been disposed
// of, or the error is the caller's to propagate).
//
// A row that cannot be READ proceeds: the gate is best-effort in the same
// way activePauseFor is, and for the same reason. A registry that cannot
// answer the metadata question (StaticRegistry) proceeds on a not-live row
// with the warning record — it can refuse nothing because it can name no
// fallback, and saying so is still worth a record.
func (w *Worker) livenessGate(
	ctx context.Context,
	claimed postgres.ClaimedWork,
	node *nodeSpec,
	dc DispatchContext,
	session sessionPlan,
) (string, sessionPlan, bool, error) {
	actorKey := actorKeyOf(node.Uses)
	if actorKey == "" {
		return node.Uses, session, true, nil
	}
	now := w.opts.Now()
	row, found, err := w.db.ActorLiveness(ctx, w.opts.NamespaceID, actorKey)
	if err != nil {
		w.report(fmt.Errorf("worker: read liveness for actor %s: %w", actorKey, err))
		return node.Uses, session, true, nil
	}
	if !found || row.Live(now) {
		return node.Uses, session, true, nil
	}

	decision := livenessDecision{
		target: node.Uses, routed: true, selected: repair.LivenessSelectedProceed,
		input: repair.LivenessInput{
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
		},
	}

	fallback := w.fallbackActorFor(ctx, node.Uses)
	if fallback != "" {
		decision.input.FallbackActorKey = actorKeyOf(fallback)
		decision.input.FallbackActorRef = fallback
		fbRow, fbFound, fbErr := w.db.ActorLiveness(ctx, w.opts.NamespaceID, actorKeyOf(fallback))
		switch {
		case fbErr != nil:
			// Unknown is treated as live for the fallback exactly as for
			// the lane: the alternative is proceeding into a lane KNOWN to
			// be dead on the strength of a read that failed.
			w.report(fmt.Errorf("worker: read liveness for fallback actor %s: %w", fallback, fbErr))
			decision.selected, decision.target = repair.LivenessSelectedFallback, fallback
		case fbFound && !fbRow.Live(now):
			decision.input.FallbackNotLive = fmt.Sprintf("session_ok=%t reason=%s locked=%t",
				fbRow.SessionOK != nil && *fbRow.SessionOK, fbRow.Reason, fbRow.Locked)
		default:
			decision.selected, decision.target = repair.LivenessSelectedFallback, fallback
		}
	}

	record, err := repair.LivenessRouting{Selected: decision.selected}.Record(decision.input)
	if err != nil {
		return "", session, false, fmt.Errorf("worker: compose liveness routing for node run %s: %w", dc.NodeRunID, err)
	}
	if _, err := w.ledger.Append(ctx, record); err != nil {
		if decision.selected == repair.LivenessSelectedFallback {
			// A reroute that cannot be recorded must not happen silently:
			// the record IS the accountability for sending this node's
			// work to an actor the author did not name. Same shape as the
			// preflight gate's own producer-identity failure.
			return "", session, false, w.failAttempt(ctx, claimed, session.ActorRowID, engine.StatusFailed, "configuration",
				fmt.Sprintf("node %q uses %q, whose lane is not live and whose registration names fallback %q, "+
					"but the routing record could not be appended: %v. The router's producer identity %q must be a "+
					"registered actor (ledger_records.origin_actor_id references actors(id)); register it with kind "+
					"`engine` and no endpoint, or set the worker's DispatchGateActorID to an identity that is",
					node.ID, node.Uses, fallback, err, w.dispatchGateActorID()))
		}
		// The warning changes nothing about where the work goes; a missing
		// warning is reported, not fatal.
		w.report(fmt.Errorf("worker: append liveness warning for node run %s: %w", dc.NodeRunID, err))
	}
	w.recordLivenessRouted(ctx, dc, node, decision)

	if decision.selected != repair.LivenessSelectedFallback {
		return node.Uses, session, true, nil
	}
	// The fallback is a different identity: attribution follows it, and a
	// continuation handle from the original lane's conversation is not one
	// the fallback can resume, so the dispatch is a cold start there.
	return fallback, sessionPlan{ActorRowID: w.actorRowID(ctx, fallback)}, true, nil
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

func (w *Worker) recordLivenessRouted(ctx context.Context, dc DispatchContext, node *nodeSpec, d livenessDecision) {
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

// LivenessLockReason is the reason a worker-written lock carries: the
// bridges' own vocabulary word for a spent refresh token, so a row locked
// from either side reads the same.
const LivenessLockReason = "refresh_token_spent"

// lockLaneOnCredentialSpent is the control-plane half of decision c43's OR
// rule: an attempt that failed with class credential_spent locks the actor's
// liveness row. It runs AFTER the failed attempt committed and is
// best-effort exactly like tripCapacityBreaker: a lock that could not be
// written is reported and the completion stands, and the next dispatch to
// the same lane will discover the same wall (and, if the bridge's own fact
// has landed by then, be routed around it).
func (w *Worker) lockLaneOnCredentialSpent(ctx context.Context, claimed postgres.ClaimedWork, node *nodeSpec, dc DispatchContext, actorRef string) {
	actorKey := actorKeyOf(actorRef)
	if actorKey == "" {
		w.report(fmt.Errorf(
			"worker: node %q reported credential_spent but its actor reference %q names no actor key; no lock recorded",
			node.ID, actorRef))
		return
	}
	row, err := w.db.LockActorLiveness(ctx, postgres.LockActorLivenessInput{
		NamespaceID: w.opts.NamespaceID,
		ActorKey:    actorKey,
		Reason:      LivenessLockReason,
		CheckedAt:   w.opts.Now(),
		RunID:       dc.RunID,
		AttemptID:   dc.AttemptID,
	})
	if err != nil {
		w.report(fmt.Errorf("worker: lock liveness for actor %s after %s: %w", actorKey, actors.ClassCredentialSpent, err))
		return
	}
	data := map[string]any{
		"run_id":      dc.RunID,
		"node_run_id": dc.NodeRunID,
		"node_id":     node.ID,
		"attempt_id":  dc.AttemptID,
		"work_id":     claimed.ID,
		"actor_key":   actorKey,
		"actor_ref":   actorRef,
		"reason":      row.Reason,
		"locked":      row.Locked,
		"unlock": "POST /v1alpha1/actors/{id}/resume clears the lock; the lane leases again once the bridge's " +
			"next liveness fact reports session_ok=true",
	}
	if err := w.callbacks.AppendRunEvent(ctx, w.opts.NamespaceID, dc.RunID, TypeLaneLocked, data); err != nil {
		w.report(fmt.Errorf("worker: append %s event for actor %s: %w", TypeLaneLocked, actorKey, err))
	}
}

// TypeLaneLocked records the control plane locking a lane after a
// credential_spent attempt.
const TypeLaneLocked = "dev.culture.nodes.actor.lane_locked"
