package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/agentculture/culture-nodes/internal/engine"
)

// handleListHumanTasks is GET /v1alpha1/human-tasks. status filters to
// "pending" or "decided"; omitted (or any other value) returns every task in
// this server's namespace, newest first — the same shape GET /v1alpha1/runs
// filters by ?state.
func (s *Server) handleListHumanTasks(w http.ResponseWriter, r *http.Request) error {
	limit := parseLimit(r, 50, 500)
	tasks, err := s.listHumanTasks(r.Context(), r.URL.Query().Get("status"), limit)
	if err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, HumanTaskListOut{Items: tasks})
	return nil
}

// handleGetHumanTask is GET /v1alpha1/human-tasks/{id}.
func (s *Server) handleGetHumanTask(w http.ResponseWriter, r *http.Request) error {
	task, err := s.engineStore.GetHumanTask(r.Context(), r.PathValue("id"))
	if err != nil {
		return classify(err)
	}
	writeJSON(w, http.StatusOK, humanTaskOut(task))
	return nil
}

// decideHumanTaskRequest is components.schemas.DecideHumanTaskRequest.
type decideHumanTaskRequest struct {
	Outcome               string          `json:"outcome"`
	DeciderActorID        string          `json:"decider_actor_id"`
	Response              json.RawMessage `json:"response"`
	ExpectedLedgerVersion int64           `json:"expected_ledger_version"`
	RecordIDs             []string        `json:"record_ids"`
}

// handleDecideHumanTask is POST /v1alpha1/human-tasks/{id}/decision. Unlike
// every other mutating operation in this API (authless by phase-1 design,
// PRD spec decision c45), this one requires a bearer token — see
// requireDecisionAuth — because a decision here writes a human-authority
// review into the run's ledger and resumes the run on whoever's behalf
// presents the token.
//
// The heavy lifting — validating the outcome against the task's
// allowed_outcomes, committing the decision as a human-authority review
// through the atomic stale-guarded review transaction, and routing the
// waiting_human node run's edge — is internal/engine's DecideHumanTask; this
// handler only translates the wire request and the domain error it can
// return (classify maps engine.ErrHumanTaskAlreadyDecided to 409 and
// engine.ErrOutcomeNotAllowed to 400).
func (s *Server) handleDecideHumanTask(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDecisionAuth(r); err != nil {
		return err
	}

	id := r.PathValue("id")
	var req decideHumanTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest(
			"send a JSON body matching DecideHumanTaskRequest: {outcome, decider_actor_id, expected_ledger_version}",
			"decode request body: %v", err)
	}
	if req.Outcome == "" {
		return badRequest("outcome is required", "outcome must not be empty")
	}
	if req.DeciderActorID == "" {
		return badRequest("decider_actor_id is required", "decider_actor_id must not be empty")
	}

	result, err := s.Engine.DecideHumanTask(r.Context(), engine.HumanTaskDecisionRequest{
		HumanTaskID:           id,
		Outcome:               req.Outcome,
		Response:              req.Response,
		DeciderActorID:        req.DeciderActorID,
		ExpectedLedgerVersion: req.ExpectedLedgerVersion,
		RecordIDs:             req.RecordIDs,
	})
	if err != nil {
		return classify(err)
	}
	writeJSON(w, http.StatusOK, humanTaskDecisionResultOut(id, result))
	return nil
}

// requireDecisionAuth enforces the bearer-token gate a human-task decision
// needs (see Server.decisionAuthSecret's doc comment for why this one
// endpoint departs from the rest of the authless API). A missing secret on
// the server (decisionAuthSecret unset) refuses every decision — there is
// nothing to authenticate a decider against — and a present-but-wrong bearer
// token is refused the same way a stale or forged callback token is
// (internal/actors/callback_http.go): 401, constant-time compared.
func (s *Server) requireDecisionAuth(r *http.Request) error {
	if len(s.decisionAuthSecret) == 0 {
		return unauthorized(
			"configure the server with a decision auth secret (NODES_HUMAN_DECISION_TOKEN_SECRET) to enable human-task decisions",
			"human-task decisions require a configured bearer secret and none is configured")
	}

	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return unauthorized("send Authorization: Bearer <token>", "missing or malformed Authorization header")
	}

	presented := strings.TrimPrefix(header, prefix)
	if subtle.ConstantTimeCompare([]byte(presented), s.decisionAuthSecret) != 1 {
		return unauthorized("the bearer token is not valid for this deployment", "authorization failed")
	}
	return nil
}
