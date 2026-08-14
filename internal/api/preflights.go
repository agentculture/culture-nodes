package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/preflight"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The clarify-then-commit gate's read surface and confirm verb (issue #67,
// task t14).
//
// The gate's other half lives at the dispatch site
// (internal/worker/clarifygate.go): it composes the briefing, appends it as
// a derived ledger record, and defers the work item. This is where the
// second, separate action lands — the one that commits the dispatch.
//
// THREE ROUTES, ONE FOR EACH READER. A bridge polls
// GET /v1alpha1/preflights to discover it is being waited on (the issue
// event goes onto the run's stream, and an actor that was not watching at
// that instant would otherwise never learn). It reads the briefing itself
// from GET /v1alpha1/preflights/{id} — an actor cannot acknowledge what it
// cannot read. And it answers with POST .../acknowledge.
//
// WHY THE ACKNOWLEDGEMENT IS NOT AUTHENTICATED SEPARATELY. Everything on
// this API except human decisions, actor registration, ad-hoc runs and
// event delivery is authless by phase-1 design (PRD spec decision c45), and
// this verb is deliberately in the ordinary group. What it writes is a
// PROPOSED record — the weakest thing the ledger admits — and the ledger's
// authority model already says exactly how much that is worth: an actor's
// claim to have understood is not evidence that it did, whoever posted it.
// Adding a shared secret here would also have to be a secret the four
// bridges hold, which is a credential-distribution problem bought for a
// promotion the record does not make. The row records WHICH actor the
// acknowledgement was made for and the record records WHO produced it, so a
// later reader can tell the two apart.

// PreflightOut is one dispatch_preflights row as the API renders it,
// documented as components.schemas.Preflight.
//
// Document is the briefing itself, read back from its ledger record — the
// same bytes the derived record carries, not a re-rendering. It is omitted
// from list responses (a list is for finding one, not for reading them all)
// and always present on the detail response.
type PreflightOut struct {
	ID        string `json:"id"`
	RunID     string `json:"run_id"`
	NodeRunID string `json:"node_run_id"`
	NodeID    string `json:"node_id"`
	ActorKey  string `json:"actor_key"`
	ActorID   string `json:"actor_id,omitempty"`
	// RecordID and RecordDigest name the derived briefing record and pin its
	// content.
	RecordID     string    `json:"record_id"`
	RecordDigest string    `json:"record_digest"`
	IssuedAt     time.Time `json:"issued_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	// Expired is computed against the server's clock at read time, because
	// row presence is never the predicate — the window is.
	Expired                 bool            `json:"expired"`
	Acknowledged            bool            `json:"acknowledged"`
	AcknowledgedAt          *time.Time      `json:"acknowledged_at,omitempty"`
	AcknowledgedBy          string          `json:"acknowledged_by,omitempty"`
	AcknowledgementRecordID string          `json:"acknowledgement_record_id,omitempty"`
	Consumed                bool            `json:"consumed"`
	ConsumedAt              *time.Time      `json:"consumed_at,omitempty"`
	ConsumedByAttemptID     string          `json:"consumed_by_attempt_id,omitempty"`
	Document                json.RawMessage `json:"document,omitempty"`
}

// PreflightListOut is the list response, documented as
// components.schemas.PreflightList.
type PreflightListOut struct {
	Items []PreflightOut `json:"items"`
}

func preflightOut(row postgres.Preflight, now time.Time) PreflightOut {
	return PreflightOut{
		ID:                      row.ID,
		RunID:                   row.RunID,
		NodeRunID:               row.NodeRunID,
		NodeID:                  row.NodeID,
		ActorKey:                row.ActorKey,
		ActorID:                 row.ActorID,
		RecordID:                row.RecordID,
		RecordDigest:            row.RecordDigest,
		IssuedAt:                row.IssuedAt,
		ExpiresAt:               row.ExpiresAt,
		Expired:                 row.Expired(now),
		Acknowledged:            row.Acknowledged(),
		AcknowledgedAt:          row.AcknowledgedAt,
		AcknowledgedBy:          row.AcknowledgedBy,
		AcknowledgementRecordID: row.AcknowledgementRecordID,
		Consumed:                row.Consumed(),
		ConsumedAt:              row.ConsumedAt,
		ConsumedByAttemptID:     row.ConsumedByAttemptID,
	}
}

// handleListPreflights is GET /v1alpha1/preflights: the briefings waiting to
// be acknowledged, newest first, optionally narrowed to one actor key.
//
// It lists PENDING ones only — unconsumed, unanswered, unexpired. That is
// the question every caller of this route is actually asking ("is anything
// waiting on me"), and answering it with history mixed in would make a
// bridge filter server state it should not have to reason about. One
// preflight's own history stays readable at its detail route.
func (s *Server) handleListPreflights(w http.ResponseWriter, r *http.Request) error {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > 200 {
			return badRequest("limit must be a positive integer no greater than 200",
				"limit %q is not usable", raw)
		}
		limit = parsed
	}

	now := time.Now().UTC()
	rows, err := s.Store.PendingPreflights(r.Context(), s.NamespaceID, r.URL.Query().Get("actor_key"), now, limit)
	if err != nil {
		return internalError(err)
	}
	out := PreflightListOut{Items: make([]PreflightOut, 0, len(rows))}
	for _, row := range rows {
		out.Items = append(out.Items, preflightOut(row, now))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// handleGetPreflight is GET /v1alpha1/preflights/{id}: one row, with the
// briefing document read back from its ledger record.
//
// A row whose record cannot be read still renders, without a document rather
// than as a 500: the row is a fact about a dispatch, and the honest answer to
// "what was the actor told" when the record is unreadable is nothing, not an
// error page in place of the state.
func (s *Server) handleGetPreflight(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	row, err := s.Store.Preflight(ctx, s.NamespaceID, r.PathValue("id"))
	if err != nil {
		return classify(err)
	}

	out := preflightOut(row, time.Now().UTC())
	if record, err := s.Ledger.Record(ctx, row.RecordID); err == nil {
		out.Document = record.Data
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// acknowledgePreflightRequest is components.schemas.AcknowledgePreflightRequest.
type acknowledgePreflightRequest struct {
	// ActorID is the registered actor the acknowledgement is recorded for.
	// It is required: an acknowledgement nobody is attributable for is a
	// record of nobody having understood anything.
	ActorID string `json:"actor_id"`
	// Verdict must be "proceed" when present. It exists so the wire shape
	// says out loud what the action means, matching the document's own
	// vocabulary and the confirmation file this protocol generalizes.
	Verdict string `json:"verdict,omitempty"`
	Note    string `json:"note,omitempty"`
}

// handleAcknowledgePreflight is POST /v1alpha1/preflights/{id}/acknowledge:
// the second, separate action that commits a gated dispatch.
//
// Order is deliberate and is the same order the dispatch site uses when it
// issues: the RECORD is appended first, and only then is the row marked. A
// crash in between leaves a proposed record with no row pointing at it — an
// actor's claim that authorized nothing, which is harmless — rather than a
// row claiming an acknowledgement whose evidence does not exist.
func (s *Server) handleAcknowledgePreflight(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	row, err := s.Store.Preflight(ctx, s.NamespaceID, r.PathValue("id"))
	if err != nil {
		return classify(err)
	}

	var req acknowledgePreflightRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest(
			"send a JSON body matching AcknowledgePreflightRequest: {actor_id, verdict?, note?}",
			"decode request body: %v", err)
	}
	if req.ActorID == "" {
		return badRequest("actor_id names the actor this acknowledgement is recorded for",
			"actor_id is required")
	}
	if req.Verdict != "" && req.Verdict != preflight.VerdictProceed {
		return badRequest(
			"acknowledge with verdict \""+preflight.VerdictProceed+"\", or omit the field",
			"verdict %q is not an acknowledgement; an actor that cannot proceed does not acknowledge, "+
				"and the dispatch is refused when the window closes", req.Verdict)
	}

	actor, err := s.engineStore.GetActor(ctx, req.ActorID)
	if err != nil {
		return classify(err)
	}
	origin := ledger.OriginAgent
	if actor.Kind == "human" {
		// An operator acknowledging on a bridge's behalf. The record still
		// carries proposed authority — a human vouching for an actor's
		// understanding is a claim about somebody else, which is exactly the
		// kind of statement the ledger keeps unpromoted.
		origin = ledger.OriginHuman
	} else if err := requireAddressedActor(row, actor); err != nil {
		return err
	}

	now := time.Now().UTC()
	if row.Consumed() || row.Acknowledged() || row.Expired(now) {
		// One refusal for three states, all of them the same answer: the
		// window for answering this briefing is closed. Which one it is
		// stays visible on the row itself (GET .../preflights/{id}).
		return conflict(
			"read the current state at GET /v1alpha1/preflights/"+row.ID+
				"; an expired or already-answered briefing is replaced by a fresh one on the next dispatch",
			"preflight %s cannot be acknowledged: acknowledged=%t consumed=%t expired=%t (window closed at %s)",
			row.ID, row.Acknowledged(), row.Consumed(), row.Expired(now), row.ExpiresAt.Format(time.RFC3339))
	}

	record, err := preflight.NewAcknowledgementRecord(preflight.AcknowledgementInput{
		RunID:             row.RunID,
		NodeRunID:         row.NodeRunID,
		PreflightRecordID: row.RecordID,
		PreflightDigest:   row.RecordDigest,
		OriginKind:        origin,
		OriginActorID:     actor.ID,
		AcknowledgedBy:    actor.ID,
		Note:              req.Note,
	})
	if err != nil {
		return internalError(err)
	}
	appended, err := s.Ledger.Append(ctx, record)
	if err != nil {
		return classify(err)
	}

	updated, err := s.Store.AcknowledgePreflight(ctx, postgres.AcknowledgePreflightInput{
		NamespaceID:             s.NamespaceID,
		ID:                      row.ID,
		AcknowledgedBy:          actor.ID,
		AcknowledgementRecordID: appended.ID,
		Now:                     now,
	})
	if err != nil {
		if errors.Is(err, postgres.ErrPreflightNotAcknowledgeable) {
			// Somebody answered (or the window closed) between the check
			// above and this write. The proposed record stands — it is a
			// true statement about a party that read the briefing — and it
			// simply authorized nothing.
			return conflict(
				"read the current state at GET /v1alpha1/preflights/"+row.ID,
				"preflight %s stopped being acknowledgeable while this acknowledgement was being recorded "+
					"(the window closed, or another acknowledgement landed first)", row.ID)
		}
		return classify(err)
	}

	out := preflightOut(updated, now)
	if rec, err := s.Ledger.Record(ctx, updated.RecordID); err == nil {
		out.Document = rec.Data
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// requireAddressedActor binds an AGENT's acknowledgement to the actor the
// briefing was addressed to.
//
// This route is unauthenticated, like the other ordinary routes, so this
// check is the only thing standing between "the addressed actor read the
// briefing" and "somebody sent a POST". Without it any registered agent can
// answer any preflight, the worker consumes the acknowledged row, and the
// dispatch proceeds to an actor that never saw the document — which is
// precisely what the gate exists to prevent.
//
// Human actors are deliberately NOT bound: an operator acknowledging on a
// bridge's behalf is a supported path, and its record already says what it
// is by carrying human origin with proposed authority (a claim about
// somebody else's understanding, kept unpromoted). Binding humans here would
// close that door instead of the open one.
//
// The row's actor id is the strong binding and is preferred whenever it is
// set: it is the registry row the dispatch actually resolved to, so a
// re-registered actor reusing a key cannot answer for its predecessor. The
// actor key is the fallback for a row addressed by key alone.
func requireAddressedActor(row postgres.Preflight, actor postgres.Actor) error {
	if row.ActorID != "" {
		if actor.ID == row.ActorID {
			return nil
		}
	} else if actor.ActorKey == row.ActorKey {
		return nil
	}
	return conflict(
		"acknowledge as the actor this briefing was issued for ("+row.ActorKey+
			"), or as a registered human actor vouching on its behalf",
		"preflight %s was issued for actor %s, and cannot be acknowledged by %s: an acknowledgement "+
			"is the addressed actor's own claim to have read the briefing",
		row.ID, row.ActorKey, actor.ActorKey)
}
