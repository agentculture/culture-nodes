package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// POST /v1alpha1/events — the inbound signal delivery route (task t10,
// issue #39, spec decision c35). An external emitter posts a named event;
// the server appends it to signal_events as an immutable fact and resumes
// every node run whose until.signal subscription matches.
//
// This file is the INBOUND half of the events surface; the outbound half —
// GET /v1alpha1/events, the SSE stream of the internal audit log — lives in
// events.go. The two share a path but not a table: an inbound signal event
// is an external fact delivered TO the control plane (signal_events,
// migrations/0016), not a row in the CloudEvents-shaped per-aggregate audit
// log the stream serves (see the migration comment for why they are
// deliberately separate).
//
// Resume path — the design decision this task asked to document: delivery
// does NOT complete the waiting node run the way a human-task decision does
// (internal/engine/humandecision.go completes the waiting_human node run
// and routes its edge inside the decision transaction — it can, because a
// human task carries its own allowed-outcomes contract and the decision IS
// the node's result). A signal wait's result is the wait node's own
// kind-implied `completed` outcome plus the resuming event, and the
// authority to commit node-run completion belongs to the engine's §12.5
// completion transaction, driven by a worker holding a fenced claim.
// Delivery therefore applies the SAME resume effect a scheduler timer fire
// applies: within the one delivery transaction (Store.DeliverSignalEvent),
// mark the subscription fired and flip the parked work item back to
// 'ready'. A worker then re-claims it, finds the fired subscription, and
// completes the node run through planTransition — normal edges, §9.7 loop
// bounds and all. PostgreSQL stays the single writer of waiting state, and
// no completion is ever committed outside a fenced claim. See
// internal/store/postgres/signal.go's doc comment for the full
// single-writer reasoning.
//
// Two things a delivery does besides firing waits, both added by task t21
// (issue #43):
//
//   - Event-route pickup (design D9): the same transaction offers the fact to
//     every active event_routes row for the name, and the engine creates a
//     token at each matching route's target node. A refused pickup — a guard
//     that declined, or a §9.7 bound with no headroom — is reported in the
//     response and recorded in the audit stream, and deliberately does NOT
//     fail the run (design D13): an external event must not kill a healthy
//     run because it arrived at a busy moment.
//   - Catch-up (design D12): an event with nothing waiting is still appended,
//     and a subscription created LATER can now consume it — see
//     internal/store/postgres/signalreplay.go. Delivery itself is unchanged
//     by that; it remains a broadcast over what is waiting now.

// deliverEventRequest is components.schemas.DeliverEventRequest.
type deliverEventRequest struct {
	Name      string          `json:"name"`
	Payload   json.RawMessage `json:"payload"`
	RunID     string          `json:"run_id"`
	Emitter   string          `json:"emitter"`
	SourceKey string          `json:"source_key"`
	Watermark json.RawMessage `json:"watermark"`
}

// SignalEventOut is one appended signal event fact on the wire
// (components.schemas.SignalEvent).
type SignalEventOut struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	RunID     string          `json:"run_id,omitempty"`
	Payload   json.RawMessage `json:"payload"`
	Emitter   string          `json:"emitter"`
	CreatedAt time.Time       `json:"created_at"`
}

// ResumedSubscriptionOut names one waiting node run this delivery resumed.
type ResumedSubscriptionOut struct {
	SubscriptionID string `json:"subscription_id"`
	RunID          string `json:"run_id"`
	NodeRunID      string `json:"node_run_id"`
}

// EventPickupOut is one event route this delivery offered the fact to
// (issue #43). A refused entry carries `refusal` (a guard decline or the
// §9.7 bound that had no headroom) and no token — design D13 makes that
// refusal the only trace, so reporting it here is how a caller learns its
// event was heard but not acted on.
type EventPickupOut struct {
	RouteID   string `json:"route_id"`
	RunID     string `json:"run_id"`
	NodeID    string `json:"node_id"`
	Admitted  bool   `json:"admitted"`
	TokenID   string `json:"token_id,omitempty"`
	NodeRunID string `json:"node_run_id,omitempty"`
	Refusal   string `json:"refusal,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// EventDeliveryOut is components.schemas.EventDeliveryResult: the appended
// fact, every subscription it fired, and every event route it reached. Empty
// `resumed` and `picked_up` lists are a normal answer, not an error — the
// event was recorded and nothing was listening.
type EventDeliveryOut struct {
	Event     SignalEventOut           `json:"event"`
	Resumed   []ResumedSubscriptionOut `json:"resumed"`
	PickedUp  []EventPickupOut         `json:"picked_up"`
	Triggered []engine.TriggeredRun    `json:"triggered"`
	Duplicate bool                     `json:"duplicate,omitempty"`
}

// handleDeliverEvent is POST /v1alpha1/events. Authenticated with its own
// bearer secret (NODES_EVENT_TOKEN_SECRET, see requireEventAuth): an
// unauthenticated caller who could emit arbitrary named events could resume
// any signal-parked run in the namespace at will.
func (s *Server) handleDeliverEvent(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireEventAuth(r); err != nil {
		return err
	}

	var req deliverEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest(
			"send a JSON body matching DeliverEventRequest: {name, payload?, run_id?, emitter?}",
			"decode request body: %v", err)
	}
	if req.Name == "" {
		return badRequest("name is required — the signal name subscriptions match on", "name must not be empty")
	}
	if req.RunID != "" {
		// A run-scoped event must name a run this server actually owns:
		// scoping to a nonexistent run would otherwise surface as an
		// unhelpful FK violation from the insert.
		if _, err := s.engineStore.Run(r.Context(), req.RunID); err != nil {
			return classify(err)
		}
	}
	emitter := req.Emitter
	if emitter == "" {
		// Attribution for operators, never an authority claim (PRD §10.4);
		// an anonymous authenticated delivery is recorded as such.
		emitter = "external"
	}

	delivery, err := s.Store.DeliverSignalEvent(r.Context(), postgres.DeliverSignalEventInput{
		NamespaceID: s.NamespaceID,
		Name:        req.Name,
		Payload:     req.Payload,
		Emitter:     emitter,
		RunID:       req.RunID,
		Pickup:      s.Engine,
		Trigger:     s.Engine,
		SourceKey:   req.SourceKey,
		Watermark:   req.Watermark,
	})
	if err != nil {
		return internalError(err)
	}

	ev := delivery.Event
	out := EventDeliveryOut{
		Event: SignalEventOut{
			ID:        ev.ID,
			Name:      ev.Name,
			RunID:     ev.RunID,
			Payload:   ev.Payload,
			Emitter:   ev.Emitter,
			CreatedAt: ev.CreatedAt,
		},
		Resumed:   make([]ResumedSubscriptionOut, 0, len(delivery.Fired)),
		PickedUp:  make([]EventPickupOut, 0, len(delivery.Pickups)),
		Triggered: make([]engine.TriggeredRun, 0, len(delivery.Triggered)),
		Duplicate: delivery.Duplicate,
	}
	out.Triggered = append(out.Triggered, delivery.Triggered...)
	for _, sub := range delivery.Fired {
		out.Resumed = append(out.Resumed, ResumedSubscriptionOut{
			SubscriptionID: sub.ID,
			RunID:          sub.RunID,
			NodeRunID:      sub.NodeRunID,
		})
	}
	for _, p := range delivery.Pickups {
		out.PickedUp = append(out.PickedUp, EventPickupOut{
			RouteID:   p.RouteID,
			RunID:     p.RunID,
			NodeID:    p.NodeID,
			Admitted:  p.Admitted,
			TokenID:   p.TokenID,
			NodeRunID: p.NodeRunID,
			Refusal:   p.Refusal,
			Detail:    p.Detail,
		})
	}
	writeJSON(w, http.StatusCreated, out)
	return nil
}

// requireEventAuth is requireDecisionAuth's pattern (humantasks.go) applied
// to inbound event delivery, against its OWN secret (Server.eventTokenSecret,
// NODES_EVENT_TOKEN_SECRET) — deliberately not the human-decision or
// actor-registration secret, so an operator can hand an external system the
// standing to emit signals without also handing it the power to decide
// human tasks or register actors. A missing secret refuses every delivery
// (closed by default), and a present-but-wrong bearer token is refused 401
// after a fixed-cost digest comparison.
func (s *Server) requireEventAuth(r *http.Request) error {
	if len(s.eventTokenSecret) == 0 {
		return unauthorized(
			"configure the server with an event token secret (NODES_EVENT_TOKEN_SECRET) to enable inbound event delivery",
			"event delivery requires a configured bearer secret and none is configured")
	}

	const prefix = "bearer "
	header := r.Header.Get("Authorization")
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return unauthorized("send Authorization: Bearer <token>", "missing or malformed Authorization header")
	}

	// Compare digests, not the raw values — the same fixed-cost shape
	// requireDecisionAuth uses: ConstantTimeCompare returns early on length
	// mismatch, which would leak the secret's length; hashing both sides
	// first makes the comparison genuinely constant-time.
	presented := sha256.Sum256([]byte(header[len(prefix):]))
	expected := sha256.Sum256(s.eventTokenSecret)
	if subtle.ConstantTimeCompare(presented[:], expected[:]) != 1 {
		return unauthorized("the bearer token is not valid for this deployment", "authorization failed")
	}
	return nil
}
