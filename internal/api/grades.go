package api

import (
	"encoding/json"
	"net/http"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// createGradeRequest is components.schemas.CreateGradeRequest.
type createGradeRequest struct {
	Rating           int    `json:"rating"`
	Rationale        string `json:"rationale"`
	EvaluatedActorID string `json:"evaluated_actor_id"`
	GradingActorID   string `json:"grading_actor_id"`
	NodeRunRef       string `json:"node_run_ref,omitempty"`
	AttemptRef       string `json:"attempt_ref,omitempty"`
	Category         string `json:"category,omitempty"`
}

// handleCreateGrade is POST /v1alpha1/runs/{id}/grades (issue #28 item 1,
// task t16): an opinion — rating plus rationale — evaluating
// evaluated_actor_id's work on this run, appended as a
// schemas/ledger/grade.schema.json record.
//
// This handler decides exactly two things and lets internal/ledger decide
// everything else:
//
//  1. Origin: the grading actor (grading_actor_id) is looked up in the actor
//     registry, and its registered `kind` — "human" or "agent" — becomes the
//     record's origin.Kind. Any other registered kind (runner, engine,
//     validator, service, or an unrecognised string) has no producer rule
//     for a bare opinion (see ledger.CheckAuthority's origin switch), so it
//     is refused here with 400 before an append is even attempted.
//  2. Authority: a human grading directly is stating the human's own
//     opinion, not a claim someone else must ratify, so it is requested as
//     confirmed — internal/ledger's checkHumanAuthority admits exactly this
//     for a grade outside a review transaction. An agent's grade is
//     requested as proposed, exactly like any other agent-origin record; it
//     reaches confirmed only by later going through the ordinary review
//     surface (POST /v1alpha1/runs/{id}/reviews + POST
//     /v1alpha1/reviews/{id}/commit) — task t16's acceptance criterion 2
//     exercises exactly that path end to end.
//
// Everything else — the self-grade refusal (RuleNoSelfGrade), the
// rating/rationale/evaluated_actor_id schema, and the grade-never-observed-
// or-derived rule — is enforced by ledger.Ledger.Append itself (via
// CheckAuthority and schema validation). classify() renders any
// *ledger.AuthorityError or *contracts.ValidationError it returns as 400,
// naming the rule or the violated schema pointer; this handler never
// re-implements those checks.
func (s *Server) handleCreateGrade(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDecisionAuth(r); err != nil {
		return err
	}
	id := r.PathValue("id")
	ctx := r.Context()

	if _, err := s.engineStore.Run(ctx, id); err != nil {
		return classify(err)
	}

	var req createGradeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest(
			"send a JSON body matching CreateGradeRequest: {rating, rationale, evaluated_actor_id, grading_actor_id}",
			"decode request body: %v", err)
	}
	// origin: resolved from principal
	var warning string
	req.GradingActorID, warning = principalActor(r, "grading_actor_id", req.GradingActorID)
	if req.GradingActorID == "" {
		return badRequest("grading_actor_id names the actor recording the grade", "grading_actor_id is required")
	}
	if req.EvaluatedActorID == "" {
		return badRequest("evaluated_actor_id names the actor being graded", "evaluated_actor_id is required")
	}

	grader, err := s.engineStore.GetActor(ctx, req.GradingActorID)
	if err != nil {
		return classify(err)
	}
	if _, err := s.engineStore.GetActor(ctx, req.EvaluatedActorID); err != nil {
		return classify(err)
	}

	var origin ledger.OriginKind
	switch grader.Kind {
	case "human":
		origin = ledger.OriginHuman
	case "agent":
		origin = ledger.OriginAgent
	default:
		return badRequest(
			"grade with a grading_actor_id registered as kind human or agent",
			"grading actor %s is registered as kind %q, which has no supported grading origin", req.GradingActorID, grader.Kind)
	}

	authority := ledger.AuthorityProposed
	if origin == ledger.OriginHuman {
		// A human grading directly is the human's own opinion, not a claim
		// someone else must ratify — checkHumanAuthority in
		// internal/ledger/authority.go admits confirmed authority for a
		// grade outside a review transaction.
		authority = ledger.AuthorityConfirmed
	}

	data := map[string]any{
		"rating":             req.Rating,
		"rationale":          req.Rationale,
		"evaluated_actor_id": req.EvaluatedActorID,
	}
	if req.NodeRunRef != "" {
		data["node_run_ref"] = req.NodeRunRef
	}
	if req.AttemptRef != "" {
		data["attempt_ref"] = req.AttemptRef
	}
	if req.Category != "" {
		data["category"] = req.Category
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return internalError(err)
	}

	appended, err := s.Ledger.Append(ctx, ledger.Record{
		RecordType: ledger.RecordGrade,
		RunID:      id,
		// origin: resolved from principal
		Origin:     ledger.Origin{Kind: origin, ActorID: req.GradingActorID},
		Authority:  authority,
		SubjectRef: ledger.NullableID(req.EvaluatedActorID),
		Data:       payload,
	})
	if err != nil {
		return classify(err)
	}

	writeJSONWithWarning(w, http.StatusCreated, appended, warning)
	return nil
}
