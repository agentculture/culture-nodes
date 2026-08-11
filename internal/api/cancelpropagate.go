package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// Propagating a run cancellation to in-flight actor sessions (issue #19).
//
// cancelRun's REAP step (runs.go) reaps every leasable work_items row for a
// cancelled run, including one an asynchronous actor invocation has parked
// in 'waiting' -- the engine's own fenced completion path already refuses a
// late completion against a work item that is no longer 'leased'/'waiting'
// under the fencing tuple the invocation was parked with, so the run's
// authoritative state is already durably safe by the time this file runs.
//
// What is NOT safe by itself is the actor: without this step, a cancelled
// run's actor session keeps running on the far side -- burning whatever
// compute or tokens it was burning -- because nothing ever told it to stop.
// PRD §13.6 gives exactly the tool for that: POST .../cancel, and it is
// explicitly best-effort ("workflow state does not depend on an external
// process acknowledging cancellation" -- internal/actors/client.go's own
// Cancel doc). So this file's only job is to ask, once, per invocation still
// waiting on an actor when the run was cancelled, and record what happened
// -- never to gate the cancel response on the answer.

// TypeActorCancelRequested records that cancelRun asked an actor to stop an
// in-flight invocation. It is its own event type, distinct from
// internal/actors' TypeCallbackLate/TypeCallbackRejected, because those
// describe what an ACTOR reported; this describes what the control plane
// SENT, and the two can genuinely disagree -- a Cancel that never reached
// the actor at all still needs a durable trace that it was attempted.
const TypeActorCancelRequested = "dev.culture.nodes.actor.cancel-requested"

// cancelPropagateTimeout bounds one actor's Cancel round trip. §13.6 makes
// cancellation best-effort by design; a hung or slow actor endpoint must not
// hold POST /v1alpha1/runs/{id}/cancel's response hostage waiting on it, so
// every invocation gets its own short, independent budget.
const cancelPropagateTimeout = 10 * time.Second

// cancelActorClient is the actors.Client every propagateCancelToActors call
// uses. A package-level default rather than a Server field: server.go (task
// t2 owns it this wave) is not where a new dependency for a same-wave t4
// feature belongs, and actors.Client is documented safe for concurrent use,
// so one shared instance costs nothing.
var cancelActorClient = actors.NewClient()

// pendingActorInvocation is the slice of an actor_invocations row
// propagateCancelToActors needs: enough to resolve an endpoint and identify
// the invocation to an actor's §13.6 cancel endpoint.
type pendingActorInvocation struct {
	AttemptID    string
	NodeRunID    string
	NodeKey      string
	ActorRef     string
	InvocationID string
}

// pendingActorInvocations loads every actor_invocations row still waiting on
// an actor (actors.InvocationWaiting) for one run -- the same "what is this
// run currently blocked on" question actor_invocations_waiting_idx
// (migrations/0009) exists to answer efficiently, narrowed to one run_id.
func (s *Server) pendingActorInvocations(ctx context.Context, runID string) ([]pendingActorInvocation, error) {
	rows, err := s.Store.Pool().Query(ctx, `
		SELECT attempt_id, node_run_id, node_key, COALESCE(actor_ref, ''), COALESCE(invocation_id, '')
		FROM actor_invocations
		WHERE run_id = $1 AND namespace_id = $2 AND state = $3`,
		runID, s.NamespaceID, actors.InvocationWaiting,
	)
	if err != nil {
		return nil, fmt.Errorf("api: pending actor invocations for run %s: %w", runID, err)
	}
	defer rows.Close()

	var out []pendingActorInvocation
	for rows.Next() {
		var inv pendingActorInvocation
		if err := rows.Scan(&inv.AttemptID, &inv.NodeRunID, &inv.NodeKey, &inv.ActorRef, &inv.InvocationID); err != nil {
			return nil, fmt.Errorf("api: pending actor invocations for run %s: scan: %w", runID, err)
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// propagateCancelToActors is cancelRun's PROPAGATE step, called after the
// cancellation transaction has committed. It is entirely best-effort: every
// failure -- loading the invocation list, resolving an endpoint, the Cancel
// call itself -- is either recorded (when there is enough context to record
// against) or silently dropped, and none of it is ever returned to the
// caller, matching §13.6's "workflow state does not depend on an external
// process acknowledging cancellation". The run is already durably cancelled
// by the time this runs; nothing here can change that.
func (s *Server) propagateCancelToActors(ctx context.Context, runID string) {
	invocations, err := s.pendingActorInvocations(ctx, runID)
	if err != nil {
		// Nothing to iterate and nowhere to record against: the run-level
		// event log has no invocation identity to attach this failure to.
		return
	}
	if len(invocations) == 0 {
		return
	}

	registry, err := worker.NewDBRegistry(s.Store, s.NamespaceID)
	if err != nil {
		// Same reasoning: a registry construction failure here means a
		// misconfigured server, not a per-invocation condition worth its own
		// event.
		return
	}

	for _, inv := range invocations {
		s.cancelOneInvocation(ctx, runID, registry, inv)
	}
}

// cancelOneInvocation resolves and cancels one actor invocation, then
// records exactly one dev.culture.nodes.actor.cancel-requested event
// regardless of outcome -- an attempted-but-failed Cancel is still evidence
// worth keeping, the same evidence-not-status-assumption discipline the PRD
// applies everywhere else.
func (s *Server) cancelOneInvocation(ctx context.Context, runID string, registry *worker.DBRegistry, inv pendingActorInvocation) {
	var outcome, detail string

	switch {
	case inv.ActorRef == "":
		outcome, detail = "skipped", "invocation names no actor_ref to resolve"
	case inv.InvocationID == "":
		outcome, detail = "skipped", "invocation has no actor-assigned invocation_id to cancel"
	default:
		endpoint, err := registry.Resolve(ctx, inv.ActorRef)
		if err != nil {
			outcome, detail = "failed", fmt.Sprintf("resolve actor %q: %v", inv.ActorRef, err)
			break
		}
		cancelCtx, cancel := context.WithTimeout(ctx, cancelPropagateTimeout)
		cancelErr := cancelActorClient.Cancel(cancelCtx, endpoint, inv.InvocationID, fmt.Sprintf("run %s cancelled", runID))
		cancel()
		if cancelErr != nil {
			outcome, detail = "failed", cancelErr.Error()
		} else {
			outcome = "sent"
		}
	}

	s.recordCancelPropagation(ctx, runID, inv, outcome, detail)
}

// recordCancelPropagation appends the one audit event
// propagateCancelToActors promises per invocation. It uses
// (*postgres.Store).InsertEvent directly -- the run's cancellation
// transaction already committed, so this is a new, independent append
// rather than something that could roll back with it, the same pattern
// internal/store/postgres/async.go's CallbackStore.AppendRunEvent uses for
// diagnostic events that are not themselves part of a state transition.
func (s *Server) recordCancelPropagation(ctx context.Context, runID string, inv pendingActorInvocation, outcome, detail string) {
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
	// Best-effort in both directions: a failure to append this diagnostic
	// event must not surface anywhere the cancel response could see it.
	_, _ = s.Store.InsertEvent(ctx, postgres.InsertEventInput{
		NamespaceID:   s.NamespaceID,
		AggregateType: "run",
		AggregateID:   runID,
		EventType:     TypeActorCancelRequested,
		Data:          data,
	})
}
