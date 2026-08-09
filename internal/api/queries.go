package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// This file holds the read paths internal/store/postgres has no typed
// method for: listing workflow versions, resolving one by digest, listing
// runs, and reading a run's tokens/node runs/attempts for the Run-view
// payload. Every query here reads through (*postgres.Store).Pool(), which
// that package's own doc comment names as the sanctioned escape hatch "for
// callers ... that need raw SQL access beyond this package's typed
// methods" — internal/api is exactly such a caller, and adding these as
// one-off typed methods to internal/store/postgres itself would grow that
// package's surface for a need only this package has.

const workflowVersionColumns = `id, namespace_id, workflow_key, version, draft_id, owner_id,
	source_format, source, normalized_ir, content_digest, published_by_actor_id, created_at`

func scanWorkflowVersion(row pgxRow) (postgres.WorkflowVersion, error) {
	var (
		v                                    postgres.WorkflowVersion
		draftID, ownerID, publishedByActorID pgtype.Text
		createdAt                            pgtype.Timestamptz
	)
	if err := row.Scan(
		&v.ID, &v.NamespaceID, &v.WorkflowKey, &v.Version, &draftID, &ownerID,
		&v.SourceFormat, &v.Source, &v.NormalizedIR, &v.ContentDigest, &publishedByActorID, &createdAt,
	); err != nil {
		return postgres.WorkflowVersion{}, err
	}
	v.DraftID = textOrEmpty(draftID)
	v.OwnerID = textOrEmpty(ownerID)
	v.PublishedByActorID = textOrEmpty(publishedByActorID)
	v.CreatedAt = tsOrZero(createdAt)
	return v, nil
}

// pgxRow is the subset of pgx.Row/pgx.Rows this file scans with.
type pgxRow interface {
	Scan(dest ...any) error
}

func textOrEmpty(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}

func tsOrZero(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time
}

// workflowVersionByDigest resolves one workflow version by content digest,
// returning postgres.ErrNotFound when none matches.
func (s *Server) workflowVersionByDigest(ctx context.Context, digest string) (postgres.WorkflowVersion, error) {
	row := s.Store.Pool().QueryRow(ctx,
		`SELECT `+workflowVersionColumns+` FROM workflow_versions WHERE namespace_id = $1 AND content_digest = $2`,
		s.NamespaceID, digest)
	v, err := scanWorkflowVersion(row)
	if err != nil {
		if isNoRowsErr(err) {
			return postgres.WorkflowVersion{}, postgres.ErrNotFound
		}
		return postgres.WorkflowVersion{}, fmt.Errorf("api: workflow version %s: %w", digest, err)
	}
	return v, nil
}

// listWorkflowVersions returns published workflow versions, newest first,
// optionally filtered to one workflow key.
func (s *Server) listWorkflowVersions(ctx context.Context, workflowKey string, limit int) ([]postgres.WorkflowVersion, error) {
	rows, err := s.Store.Pool().Query(ctx,
		`SELECT `+workflowVersionColumns+` FROM workflow_versions
		 WHERE namespace_id = $1 AND ($2 = '' OR workflow_key = $2)
		 ORDER BY created_at DESC, id DESC
		 LIMIT $3`,
		s.NamespaceID, workflowKey, limit)
	if err != nil {
		return nil, fmt.Errorf("api: list workflow versions: %w", err)
	}
	defer rows.Close()

	out := make([]postgres.WorkflowVersion, 0)
	for rows.Next() {
		v, err := scanWorkflowVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("api: list workflow versions: scan: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// runRow is one runs row joined to its pinned workflow digest.
type runRow struct {
	ID             string
	WorkflowDigest string
	Status         string
	Input          json.RawMessage
	Output         json.RawMessage
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    time.Time
}

func (r runRow) out() RunOut {
	out := RunOut{
		ID:             r.ID,
		WorkflowDigest: r.WorkflowDigest,
		State:          r.Status,
		Input:          nonNullJSON(r.Input),
		Output:         nonNullJSON(r.Output),
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
	}
	if !r.CompletedAt.IsZero() {
		completedAt := r.CompletedAt
		out.CompletedAt = &completedAt
	}
	return out
}

// listRuns returns runs newest first, optionally filtered to one state.
func (s *Server) listRuns(ctx context.Context, state string, limit int) ([]RunOut, error) {
	rows, err := s.Store.Pool().Query(ctx, `
		SELECT r.id, wv.content_digest, r.status, r.input, r.output, r.created_at, r.updated_at, r.completed_at
		FROM runs r JOIN workflow_versions wv ON wv.id = r.workflow_version_id
		WHERE r.namespace_id = $1 AND ($2 = '' OR r.status = $2)
		ORDER BY r.created_at DESC, r.id DESC
		LIMIT $3`,
		s.NamespaceID, state, limit)
	if err != nil {
		return nil, fmt.Errorf("api: list runs: %w", err)
	}
	defer rows.Close()

	out := make([]RunOut, 0)
	for rows.Next() {
		var (
			r           runRow
			input       []byte
			output      []byte
			createdAt   pgtype.Timestamptz
			updatedAt   pgtype.Timestamptz
			completedAt pgtype.Timestamptz
		)
		if err := rows.Scan(&r.ID, &r.WorkflowDigest, &r.Status, &input, &output, &createdAt, &updatedAt, &completedAt); err != nil {
			return nil, fmt.Errorf("api: list runs: scan: %w", err)
		}
		r.Input = json.RawMessage(input)
		r.Output = json.RawMessage(output)
		r.CreatedAt = tsOrZero(createdAt)
		r.UpdatedAt = tsOrZero(updatedAt)
		r.CompletedAt = tsOrZero(completedAt)
		out = append(out, r.out())
	}
	return out, rows.Err()
}

// runTokens returns every token of a run, oldest first.
func (s *Server) runTokens(ctx context.Context, runID string) ([]TokenOut, error) {
	rows, err := s.Store.Pool().Query(ctx, `
		SELECT id, node_key, state, parent_token_id, created_at, consumed_at
		FROM tokens WHERE run_id = $1 ORDER BY created_at, id`, runID)
	if err != nil {
		return nil, fmt.Errorf("api: run %s: list tokens: %w", runID, err)
	}
	defer rows.Close()

	out := make([]TokenOut, 0)
	for rows.Next() {
		var (
			t             TokenOut
			parentTokenID pgtype.Text
			createdAt     pgtype.Timestamptz
			consumedAt    pgtype.Timestamptz
		)
		if err := rows.Scan(&t.ID, &t.NodeID, &t.State, &parentTokenID, &createdAt, &consumedAt); err != nil {
			return nil, fmt.Errorf("api: run %s: list tokens: scan: %w", runID, err)
		}
		t.ParentTokenID = textOrEmpty(parentTokenID)
		t.CreatedAt = tsOrZero(createdAt)
		if consumedAt.Valid {
			consumed := consumedAt.Time
			t.ConsumedAt = &consumed
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// runNodeRuns returns every node run of a run, oldest first, each with its
// attempts nested (oldest first).
func (s *Server) runNodeRuns(ctx context.Context, runID string) ([]NodeRunOut, error) {
	rows, err := s.Store.Pool().Query(ctx, `
		SELECT id, token_id, node_key, status, outcome, visit_count, created_at, updated_at, completed_at
		FROM node_runs WHERE run_id = $1 ORDER BY created_at, id`, runID)
	if err != nil {
		return nil, fmt.Errorf("api: run %s: list node runs: %w", runID, err)
	}

	out := make([]NodeRunOut, 0)
	byID := make(map[string]int)
	for rows.Next() {
		var (
			nr          NodeRunOut
			tokenID     pgtype.Text
			outcome     pgtype.Text
			createdAt   pgtype.Timestamptz
			updatedAt   pgtype.Timestamptz
			completedAt pgtype.Timestamptz
		)
		if err := rows.Scan(&nr.ID, &tokenID, &nr.NodeID, &nr.State, &outcome, &nr.VisitCount, &createdAt, &updatedAt, &completedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("api: run %s: list node runs: scan: %w", runID, err)
		}
		nr.TokenID = textOrEmpty(tokenID)
		nr.Outcome = textOrEmpty(outcome)
		nr.CreatedAt = tsOrZero(createdAt)
		nr.UpdatedAt = tsOrZero(updatedAt)
		if completedAt.Valid {
			completed := completedAt.Time
			nr.CompletedAt = &completed
		}
		nr.Attempts = []AttemptOut{}
		byID[nr.ID] = len(out)
		out = append(out, nr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("api: run %s: list node runs: %w", runID, err)
	}

	attemptRows, err := s.Store.Pool().Query(ctx, `
		SELECT a.id, a.node_run_id, a.attempt_number, a.actor_id, a.status, a.fencing_token, a.result, a.started_at, a.completed_at
		FROM attempts a JOIN node_runs nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1
		ORDER BY a.node_run_id, a.attempt_number`, runID)
	if err != nil {
		return nil, fmt.Errorf("api: run %s: list attempts: %w", runID, err)
	}
	defer attemptRows.Close()

	for attemptRows.Next() {
		var (
			a            AttemptOut
			actorID      pgtype.Text
			fencingToken pgtype.Int8
			result       []byte
			startedAt    pgtype.Timestamptz
			completedAt  pgtype.Timestamptz
		)
		if err := attemptRows.Scan(&a.ID, &a.NodeRunID, &a.AttemptNumber, &actorID, &a.Status, &fencingToken, &result, &startedAt, &completedAt); err != nil {
			return nil, fmt.Errorf("api: run %s: list attempts: scan: %w", runID, err)
		}
		a.ActorID = textOrEmpty(actorID)
		if fencingToken.Valid {
			a.FencingToken = fencingToken.Int64
		}
		a.Result = nonNullJSON(result)
		a.StartedAt = tsOrZero(startedAt)
		if completedAt.Valid {
			completed := completedAt.Time
			a.CompletedAt = &completed
		}

		idx, ok := byID[a.NodeRunID]
		if !ok {
			continue // an attempt for a node run outside this page cannot happen (both queries share runID), but stay defensive.
		}
		out[idx].Attempts = append(out[idx].Attempts, a)
	}
	return out, attemptRows.Err()
}

// listHumanTasks returns human tasks newest first, optionally filtered to
// one status ("pending" or "decided"), scoped to this server's namespace —
// the same shape listRuns and listWorkflowVersions use above.
func (s *Server) listHumanTasks(ctx context.Context, status string, limit int) ([]HumanTaskOut, error) {
	rows, err := s.Store.Pool().Query(ctx, `
		SELECT id, run_id, node_run_id, kind, assigned_owner_id, status, request, response, created_at, resolved_at
		FROM human_tasks
		WHERE namespace_id = $1 AND ($2 = '' OR status = $2)
		ORDER BY created_at DESC, id DESC
		LIMIT $3`,
		s.NamespaceID, status, limit)
	if err != nil {
		return nil, fmt.Errorf("api: list human tasks: %w", err)
	}
	defer rows.Close()

	out := make([]HumanTaskOut, 0)
	for rows.Next() {
		var (
			t               HumanTaskOut
			nodeRunID       pgtype.Text
			assignedOwnerID pgtype.Text
			request         []byte
			response        []byte
			createdAt       pgtype.Timestamptz
			resolvedAt      pgtype.Timestamptz
		)
		if err := rows.Scan(
			&t.ID, &t.RunID, &nodeRunID, &t.Kind, &assignedOwnerID, &t.Status,
			&request, &response, &createdAt, &resolvedAt,
		); err != nil {
			return nil, fmt.Errorf("api: list human tasks: scan: %w", err)
		}
		t.NodeRunID = textOrEmpty(nodeRunID)
		t.AssignedOwnerID = textOrEmpty(assignedOwnerID)
		t.Request = json.RawMessage(request)
		t.Response = nonNullJSON(json.RawMessage(response))
		t.CreatedAt = tsOrZero(createdAt)
		if resolvedAt.Valid {
			resolved := resolvedAt.Time
			t.ResolvedAt = &resolved
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// isNoRowsErr reports whether err is pgx's "no rows in result set"
// sentinel, mirroring internal/store/postgres's own isNoRows helper (that
// one is unexported, so this package needs its own copy).
func isNoRowsErr(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
