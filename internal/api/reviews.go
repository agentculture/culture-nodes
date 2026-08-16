package api

import (
	"encoding/json"
	"net/http"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// The review surface is the affirmative half of PRD §10.4: an agent's
// proposal is placed in front of a named human, and the human's answer is
// recorded as its own immutable record. Three properties are enforced here
// rather than left to the caller:
//
//   - Both routes are gated by the decision bearer token, the same secret
//     POST /v1alpha1/human-tasks/{id}/decision requires. They write
//     human-authority records into the ledger on whoever presents the token,
//     which is exactly the reason that endpoint is gated; leaving these two
//     open would have made them the unauthenticated way to do the same thing.
//   - A review must name its reviewer when it is CREATED, not two calls later
//     when ledger.CommitReview refuses it.
//   - A commit must state a rationale. A confirmation with no stated reason
//     cannot be told apart from an unread one, and "who decided" without "on
//     what grounds" is an attribution, not an account.
//
// Who may be a reviewer is not decided here: ledger.CommitReview resolves the
// named actor against the registry and refuses anything that is not a human,
// so the rule holds for every caller of the ledger, not only for HTTP ones.

// createReviewRequest is components.schemas.CreateReviewRequest.
type createReviewRequest struct {
	RecordIDs       []string `json:"record_ids"`
	LedgerVersion   int64    `json:"ledger_version"`
	ReviewerActorID string   `json:"reviewer_actor_id"`
}

// handleCreateReview is POST /v1alpha1/runs/{id}/reviews.
func (s *Server) handleCreateReview(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDecisionAuth(r); err != nil {
		return err
	}

	id := r.PathValue("id")
	ctx := r.Context()

	if _, err := s.engineStore.Run(ctx, id); err != nil {
		return classify(err)
	}

	var req createReviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest("send a JSON body matching CreateReviewRequest: {record_ids, ledger_version, reviewer_actor_id}", "decode request body: %v", err)
	}
	if req.ReviewerActorID == "" {
		return badRequest(
			"name the human actor who will decide this review in reviewer_actor_id; GET /v1alpha1/actors lists the registered ones",
			"reviewer_actor_id must not be empty: a confirmation nobody is accountable for is not a confirmation")
	}

	created, err := s.Ledger.CreateReviewRequest(ctx, id, req.RecordIDs, req.LedgerVersion,
		ledger.WithReviewer(req.ReviewerActorID))
	if err != nil {
		return classify(err)
	}
	writeJSON(w, http.StatusCreated, reviewRequestOut(created))
	return nil
}

// commitReviewRequest is components.schemas.CommitReviewRequest.
type commitReviewRequest struct {
	Decisions             map[string]ledger.Verdict `json:"decisions"`
	ExpectedLedgerVersion int64                     `json:"expected_ledger_version"`
	// Rationale is why the reviewer decided this way. Required — see the
	// file comment above.
	Rationale string `json:"rationale"`
}

// handleCommitReview is POST /v1alpha1/reviews/{id}/commit. A stale or
// already-committed review is refused with 409 and nothing written (PRD
// §10.8); a decision set that does not exactly cover the reviewed records
// is refused with 400, as is a reviewer the registry does not record as a
// human (PRD §10.4).
func (s *Server) handleCommitReview(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDecisionAuth(r); err != nil {
		return err
	}

	id := r.PathValue("id")

	var req commitReviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest("send a JSON body matching CommitReviewRequest: {decisions, expected_ledger_version, rationale}", "decode request body: %v", err)
	}
	if req.Rationale == "" {
		return badRequest(
			"state why in rationale — what you read, ran, or checked to reach this verdict",
			"rationale must not be empty: a confirmation with no stated reason cannot be told apart from an unread one")
	}

	result, err := s.Ledger.CommitReview(r.Context(), id, req.Decisions, req.ExpectedLedgerVersion,
		ledger.WithRationale(req.Rationale))
	if err != nil {
		return classify(err)
	}

	writeJSON(w, http.StatusOK, ReviewCommitResultOut{
		ReviewID:      result.ReviewID,
		Records:       result.Records,
		LedgerVersion: result.LedgerVersion,
	})
	return nil
}
