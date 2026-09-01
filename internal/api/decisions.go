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

// This file is the discoverability half of PRD §10.4's affirmative side
// (task t30, issue #99): "what is awaiting my decision", answerable as a
// query across every run.
//
// The question matters because of a property that surprises everyone the
// first time: committing a confirmation does NOT mutate the claim. Records
// are immutable, so a confirmed claim reads `authority: proposed` forever and
// the decision is a separate `review` record naming it. "Which claims are
// still undecided" is therefore not `WHERE authority = 'proposed'` — it is
// "proposed, and no review record points at it", which is a join no operator
// should have to write by hand. Before this endpoint the operator kept
// docs/triage/cycle-runs.txt by hand so scripts/ledger-gate.py had a list of
// runs to walk.

// PendingDecisionRecordOut is one proposed record awaiting a decision. It
// carries the whole ledger record rather than a summary: a decider needs to
// read the claim itself, and re-fetching each record individually to do that
// would make the queue useless as the thing you decide from.
type PendingDecisionRecordOut struct {
	ID            string          `json:"id"`
	RecordType    string          `json:"record_type"`
	OriginKind    string          `json:"origin_kind"`
	OriginActorID string          `json:"origin_actor_id,omitempty"`
	NodeRunID     string          `json:"node_run_id,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	Data          json.RawMessage `json:"data"`
}

// PendingDecisionRunOut groups one run's undecided records with the ledger
// version they were read at.
//
// The grouping is not cosmetic. A review is opened per run at a stated ledger
// version (PRD §10.8), so a caller that wants to decide these records needs
// exactly this pairing to open one; a flat list would make every caller
// re-derive it, and a caller that guessed wrong would be refused by the
// staleness guard rather than told what to send.
type PendingDecisionRunOut struct {
	RunID string `json:"run_id"`
	// LedgerVersion is the run's current version, read in the same request.
	// It is what POST /v1alpha1/runs/{id}/reviews wants as `ledger_version`.
	LedgerVersion int64                      `json:"ledger_version"`
	Records       []PendingDecisionRecordOut `json:"records"`
}

// PendingDecisionListOut is components.schemas.PendingDecisionList.
type PendingDecisionListOut struct {
	Items []PendingDecisionRunOut `json:"items"`
	// RecordCount is the number of undecided records across every listed
	// run — the number a stage gate compares against zero.
	RecordCount int    `json:"record_count"`
	NextCursor  string `json:"next_cursor,omitempty"`
}

// pendingDecisionFilters are the only query parameters this endpoint
// understands. Anything else is refused: a filter silently ignored answers a
// different question than the one asked, while still looking authoritative.
var pendingDecisionFilters = map[string]bool{
	"run_id":      true,
	"record_type": true,
	"actor_id":    true,
	"limit":       true,
	"cursor":      true,
}

// handleListPendingDecisions is GET /v1alpha1/pending-decisions.
func (s *Server) handleListPendingDecisions(w http.ResponseWriter, r *http.Request) error {
	query := r.URL.Query()
	for name := range query {
		if !pendingDecisionFilters[name] {
			known := make([]string, 0, len(pendingDecisionFilters))
			for filter := range pendingDecisionFilters {
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
	groups, count, nextCursor, err := s.listPendingDecisions(r.Context(), pendingDecisionParams{
		RunID:      query.Get("run_id"),
		RecordType: query.Get("record_type"),
		ActorID:    query.Get("actor_id"),
		Limit:      parseLimit(r, 200, 1000),
		Cursor:     cursor,
	})
	if err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, PendingDecisionListOut{Items: groups, RecordCount: count, NextCursor: nextCursor})
	return nil
}

type pendingDecisionParams struct {
	RunID      string
	RecordType string
	ActorID    string
	// RunIDs narrows to a KNOWN SET of runs, which is what a projection
	// scoped to one subject needs — the ticket page (task t14) asks for the
	// undecided records of the runs it already listed, and a per-run query
	// would be one round trip per run of a ticket that may have hundreds.
	// Empty means "every run", as before; a non-nil but empty set would be
	// a query for nothing and is treated the same way by callers, which
	// never build one.
	RunIDs []string
	Limit  int
	Cursor *nodeRunCursor
}

// listPendingDecisions returns every proposed record no review record points
// at, grouped by run.
//
// "Decided" means a review record names it, whatever the verdict — a
// rejection answers the question as completely as a confirmation does. A
// superseded record is excluded too: a correction that replaced it is what
// should be decided now, not the record it replaced.
func (s *Server) listPendingDecisions(ctx context.Context, p pendingDecisionParams) ([]PendingDecisionRunOut, int, string, error) {
	const query = `
		SELECT r.id, r.run_id, r.record_type, r.origin_kind, r.origin_actor_id,
		       r.node_run_id, r.data, r.created_at
		FROM ledger_records r
		WHERE r.namespace_id = $1
		  AND r.authority = $2
		  AND ($3 = '' OR r.run_id = $3)
		  AND ($4 = '' OR r.record_type = $4)
		  AND ($5 = '' OR r.origin_actor_id = $5)
		  AND NOT EXISTS (
			  SELECT 1 FROM ledger_records d
			  WHERE d.namespace_id = r.namespace_id
			    AND d.record_type = $6
			    AND d.subject_ref = r.id
		  )
		  AND NOT EXISTS (
			  SELECT 1 FROM ledger_records s
			  WHERE s.namespace_id = r.namespace_id AND s.supersedes = r.id
		  )
		  AND ($7::timestamptz IS NULL OR (r.created_at, r.id) < ($7, $8))
		  AND ($9::text[] IS NULL OR r.run_id = ANY($9))
		ORDER BY r.created_at DESC, r.id DESC
		LIMIT $10`
	var cursorCreatedAt *time.Time
	var cursorID string
	if p.Cursor != nil {
		cursorCreatedAt = &p.Cursor.UpdatedAt
		cursorID = p.Cursor.ID
	}

	var runIDs any
	if len(p.RunIDs) > 0 {
		runIDs = p.RunIDs
	}
	rows, err := s.Store.Pool().Query(ctx, query,
		s.NamespaceID, string(ledger.AuthorityProposed),
		p.RunID, p.RecordType, p.ActorID, string(ledger.RecordReview), cursorCreatedAt, cursorID, runIDs, p.Limit+1)
	if err != nil {
		return nil, 0, "", fmt.Errorf("list pending decisions: %w", err)
	}
	defer rows.Close()

	byRun := map[string][]PendingDecisionRecordOut{}
	order := []string{}
	total := 0
	hasMore := false
	var last PendingDecisionRecordOut
	for rows.Next() {
		var (
			rec       PendingDecisionRecordOut
			runID     string
			actorID   pgtype.Text
			nodeRunID pgtype.Text
			createdAt pgtype.Timestamptz
			data      []byte
		)
		if err := rows.Scan(&rec.ID, &runID, &rec.RecordType, &rec.OriginKind,
			&actorID, &nodeRunID, &data, &createdAt); err != nil {
			return nil, 0, "", fmt.Errorf("scan pending decision: %w", err)
		}
		rec.OriginActorID = textOrEmpty(actorID)
		rec.NodeRunID = textOrEmpty(nodeRunID)
		rec.CreatedAt = tsOrZero(createdAt).UTC()
		rec.Data = data
		if total == p.Limit {
			hasMore = true
			break
		}
		if _, seen := byRun[runID]; !seen {
			order = append(order, runID)
		}
		byRun[runID] = append(byRun[runID], rec)
		last = rec
		total++
	}
	if err := rows.Err(); err != nil {
		return nil, 0, "", fmt.Errorf("read pending decisions: %w", err)
	}

	items := make([]PendingDecisionRunOut, 0, len(order))
	for _, runID := range order {
		// The version is read per run rather than counted from the rows
		// above: a review is opened against the run's CURRENT version,
		// which includes every record — decided, superseded, and of every
		// authority — not just the undecided ones listed here.
		version, err := s.Ledger.LedgerVersion(ctx, runID)
		if err != nil {
			return nil, 0, "", fmt.Errorf("ledger version of run %s: %w", runID, err)
		}
		items = append(items, PendingDecisionRunOut{
			RunID:         runID,
			LedgerVersion: version,
			Records:       byRun[runID],
		})
	}
	nextCursor := ""
	if hasMore {
		nextCursor = encodeNodeRunCursor(nodeRunCursor{UpdatedAt: last.CreatedAt, ID: last.ID})
	}
	return items, total, nextCursor, nil
}
