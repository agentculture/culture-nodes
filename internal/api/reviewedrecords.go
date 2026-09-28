package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// GET /v1alpha1/reviewed-records (task t46, owner decision d19): the Decided
// half of the one decision page. GET /v1alpha1/pending-decisions answers
// "what is awaiting a decision"; this answers "what was decided, and why",
// newest first — one entry per review record, carrying the record it decided
// in full, the verdict, the reviewer and the stated rationale.
//
// A human task's decision is itself a review (humandecision.go appends a
// proposed `decision` record carrying `human_task_id` and confirms it). Those
// are left out here: the decided human task is already listed by
// GET /v1alpha1/human-tasks, with its note, and listing the review behind it
// too would show one decision twice.

// ReviewedRecordOut is components.schemas.ReviewedRecord.
type ReviewedRecordOut struct {
	// ReviewRecordID is the review record's own id.
	ReviewRecordID  string    `json:"review_record_id"`
	RunID           string    `json:"run_id"`
	ReviewerActorID string    `json:"reviewer_actor_id,omitempty"`
	Verdict         string    `json:"verdict"`
	Rationale       string    `json:"rationale,omitempty"`
	ReviewedAt      time.Time `json:"reviewed_at"`
	// Record is the decided record, rendered exactly as pending-decisions
	// rendered it while it awaited the decision.
	Record PendingDecisionRecordOut `json:"record"`
}

// ReviewedRecordListOut is components.schemas.ReviewedRecordList.
type ReviewedRecordListOut struct {
	Items      []ReviewedRecordOut `json:"items"`
	NextCursor string              `json:"next_cursor,omitempty"`
}

var reviewedRecordFilters = map[string]bool{"run_id": true, "limit": true, "cursor": true}

// handleListReviewedRecords is GET /v1alpha1/reviewed-records.
func (s *Server) handleListReviewedRecords(w http.ResponseWriter, r *http.Request) error {
	query := r.URL.Query()
	for name := range query {
		if !reviewedRecordFilters[name] {
			known := make([]string, 0, len(reviewedRecordFilters))
			for filter := range reviewedRecordFilters {
				known = append(known, filter)
			}
			sort.Strings(known)
			return badRequest(
				fmt.Sprintf("the filters this endpoint understands are: %v", known),
				"unrecognized query parameter %q", name)
		}
	}
	var cursor *nodeRunCursor
	if raw := query.Get("cursor"); raw != "" {
		decoded, err := decodeNodeRunCursor(raw)
		if err != nil {
			return badRequest("pass back a previous next_cursor unchanged", "invalid cursor: %v", err)
		}
		cursor = &decoded
	}
	items, next, err := s.listReviewedRecords(r.Context(), query.Get("run_id"), parseLimit(r, 50, 500), cursor)
	if err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, ReviewedRecordListOut{Items: items, NextCursor: next})
	return nil
}

func (s *Server) listReviewedRecords(ctx context.Context, runID string, limit int, cursor *nodeRunCursor) ([]ReviewedRecordOut, string, error) {
	const query = `
		SELECT rv.id, rv.run_id, rv.origin_actor_id, rv.data, rv.created_at,
		       d.id, d.record_type, d.origin_kind, d.origin_actor_id,
		       d.node_run_id, d.data, d.created_at
		FROM ledger_records rv
		JOIN ledger_records d
		  ON d.namespace_id = rv.namespace_id AND d.id = rv.subject_ref
		WHERE rv.namespace_id = $1
		  AND rv.record_type = $2
		  AND NOT (d.record_type = $3 AND d.data->>'human_task_id' IS NOT NULL)
		  AND ($4 = '' OR rv.run_id = $4)
		  AND ($5::timestamptz IS NULL OR (rv.created_at, rv.id) < ($5, $6))
		ORDER BY rv.created_at DESC, rv.id DESC
		LIMIT $7`
	var cursorAt *time.Time
	var cursorID string
	if cursor != nil {
		cursorAt = &cursor.UpdatedAt
		cursorID = cursor.ID
	}
	rows, err := s.Store.Pool().Query(ctx, query, s.NamespaceID,
		string(ledger.RecordReview), string(ledger.RecordDecision), runID, cursorAt, cursorID, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list reviewed records: %w", err)
	}
	defer rows.Close()

	items := []ReviewedRecordOut{}
	hasMore := false
	for rows.Next() {
		var (
			out                  ReviewedRecordOut
			reviewer, subjectBy  pgtype.Text
			subjectNodeRun       pgtype.Text
			reviewData, subjData []byte
			reviewedAt, subjAt   pgtype.Timestamptz
		)
		if err := rows.Scan(&out.ReviewRecordID, &out.RunID, &reviewer, &reviewData, &reviewedAt,
			&out.Record.ID, &out.Record.RecordType, &out.Record.OriginKind, &subjectBy,
			&subjectNodeRun, &subjData, &subjAt); err != nil {
			return nil, "", fmt.Errorf("scan reviewed record: %w", err)
		}
		if len(items) == limit {
			hasMore = true
			break
		}
		var payload struct {
			Verdict   string `json:"verdict"`
			Rationale string `json:"rationale"`
		}
		_ = json.Unmarshal(reviewData, &payload) // a review record's own payload; a bad one reads as blank
		out.ReviewerActorID = textOrEmpty(reviewer)
		out.Verdict = payload.Verdict
		out.Rationale = payload.Rationale
		out.ReviewedAt = tsOrZero(reviewedAt).UTC()
		out.Record.OriginActorID = textOrEmpty(subjectBy)
		out.Record.NodeRunID = textOrEmpty(subjectNodeRun)
		out.Record.CreatedAt = tsOrZero(subjAt).UTC()
		out.Record.Data = subjData
		items = append(items, out)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("read reviewed records: %w", err)
	}
	next := ""
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		next = encodeNodeRunCursor(nodeRunCursor{UpdatedAt: last.ReviewedAt, ID: last.ReviewRecordID})
	}
	return items, next, nil
}
