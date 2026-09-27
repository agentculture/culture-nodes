package api

import (
	"net/http"

	"github.com/agentculture/culture-nodes/internal/declengine"
)

// The declaration engine's explain surface (task t13, #328; spec c88,
// honesty h61): "why did (or didn't) declaration X fire for event E" must
// be answerable from the API, not just from a store query. This route is
// a thin read over declengine.PostgresBackend.Explain, which already does
// the actual lookup (declengine/explain.go).
//
// It is READ-ONLY by design, matching dispatchrates.go's and schedules.go's
// posture: the firing loop (internal/declengine/engine.go) is the only
// writer of declaration_evaluations rows, and this route never touches them.

// DeclarationEvaluationOut is components.schemas.DeclarationEvaluation: the
// terminal outcome the firing loop recorded for one declaration's
// evaluation of one event.
type DeclarationEvaluationOut struct {
	EventID string `json:"event_id"`
	// DeclarationID is the declaration's stable id, shared by every
	// version; DeclarationVersion is the specific version this evaluation
	// pinned.
	DeclarationID      string `json:"declaration_id"`
	DeclarationName    string `json:"declaration_name"`
	DeclarationVersion string `json:"declaration_version"`
	// Outcome is one of: matched, lineage missing, lineage checked,
	// loop-limited, condition error, condition false, condition true,
	// evaluation failed, duplicate, dispatching, dispatch failed, fired,
	// deferred, shadow, budget-blocked, overlap-suppressed. The last two
	// are defined but not yet produced by any task (t11, t14).
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	// FiringID is present once a firing was claimed for this evaluation
	// (fired, shadow, and any outcome that reused an already-claimed
	// firing); absent for an outcome decided before a firing was claimed.
	FiringID string `json:"firing_id,omitempty"`
}

func declarationEvaluationOut(r declengine.ExplainResult) DeclarationEvaluationOut {
	return DeclarationEvaluationOut{
		EventID:            r.EventID,
		DeclarationID:      r.DeclarationID,
		DeclarationName:    r.DeclarationName,
		DeclarationVersion: r.DeclarationVersion,
		Outcome:            r.Outcome,
		Reason:             r.Reason,
		FiringID:           r.FiringID,
	}
}

// handleGetDeclarationEvaluation is GET
// /v1alpha1/declarations/{name}/evaluations?event_id=... : the newest
// (terminal) declaration_evaluations row this declaration's evaluation of
// event_id recorded. 404 when the declaration never matched that event's
// trigger at all — engine.go's Handle records nothing for a non-match, so
// there is no evaluation to explain — or when the declaration name is
// unknown in this namespace.
func (s *Server) handleGetDeclarationEvaluation(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("name")
	eventID := r.URL.Query().Get("event_id")
	if eventID == "" {
		return badRequest("pass ?event_id=<id> naming the event to explain", "event_id is required")
	}

	result, found, err := (declengine.PostgresBackend{Store: s.Store}).Explain(r.Context(), s.NamespaceID, eventID, name)
	if err != nil {
		return internalError(err)
	}
	if !found {
		return notFound("check the declaration name and event id: this declaration never matched that event, or one of them does not exist",
			"no recorded evaluation of event %q by declaration %q", eventID, name)
	}

	writeJSON(w, http.StatusOK, declarationEvaluationOut(result))
	return nil
}
