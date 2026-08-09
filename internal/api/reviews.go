package api

import (
	"encoding/json"
	"net/http"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// createReviewRequest is components.schemas.CreateReviewRequest.
type createReviewRequest struct {
	RecordIDs       []string `json:"record_ids"`
	LedgerVersion   int64    `json:"ledger_version"`
	ReviewerActorID string   `json:"reviewer_actor_id"`
}

// handleCreateReview is POST /v1alpha1/runs/{id}/reviews.
func (s *Server) handleCreateReview(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	ctx := r.Context()

	if _, err := s.engineStore.Run(ctx, id); err != nil {
		return classify(err)
	}

	var req createReviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest("send a JSON body matching CreateReviewRequest: {record_ids, ledger_version}", "decode request body: %v", err)
	}

	var opts []ledger.ReviewOption
	if req.ReviewerActorID != "" {
		opts = append(opts, ledger.WithReviewer(req.ReviewerActorID))
	}

	created, err := s.Ledger.CreateReviewRequest(ctx, id, req.RecordIDs, req.LedgerVersion, opts...)
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
}

// handleCommitReview is POST /v1alpha1/reviews/{id}/commit. A stale or
// already-committed review is refused with 409 and nothing written (PRD
// §10.8); a decision set that does not exactly cover the reviewed records
// is refused with 400.
func (s *Server) handleCommitReview(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")

	var req commitReviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest("send a JSON body matching CommitReviewRequest: {decisions, expected_ledger_version}", "decode request body: %v", err)
	}

	result, err := s.Ledger.CommitReview(r.Context(), id, req.Decisions, req.ExpectedLedgerVersion)
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
