package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// This file adds the runner_operations insert task t14's pre_run/post_run
// code hooks need (internal/worker/hooks.go). Like artifacts.go (t15), it
// intentionally does not use sqlc/queries.sql (see store.go's package doc):
// t14's brief asks for this as a new file only, so the raw SQL here never
// touches the generated sqlcgen code or the hand-maintained queries.sql that
// store.go's existing methods share. It reuses artifacts.go's querier
// interface — the exact subset of pgx's API this file needs too — rather
// than declaring a second, identical one.
//
// The table itself is migrations/0002_runtime_execution.sql, and predates
// this task: nothing wrote to runner_operations before this file. Its
// attempt_id column is a nullable foreign key to attempts(id) on purpose —
// see InsertRunnerOperationInput's doc — so every insert here happens after
// the attempt row it references already exists (attempts are created inside
// engine.CompleteAttempt's own transaction, which a hook's execution
// precedes or follows but never shares).

// RunnerOperationRecord is a row of the runner_operations table.
type RunnerOperationRecord struct {
	ID            string
	NamespaceID   string
	AttemptID     string
	OperationKind string
	PolicyDigest  string
	Request       json.RawMessage
	Result        json.RawMessage
	Status        string
	CreatedAt     time.Time
	CompletedAt   time.Time
}

// InsertRunnerOperationInput is the input to Store.InsertRunnerOperation.
// AttemptID is optional ("" means NULL) because a caller may need to record
// an operation whose attempt does not exist yet or was never created (a
// runner refused to dispatch at all); Result and CompletedAt are optional for
// the same reason a dispatch refusal has no result to report and no
// completion time to record.
type InsertRunnerOperationInput struct {
	ID            string
	NamespaceID   string
	AttemptID     string
	OperationKind string
	PolicyDigest  string
	Request       json.RawMessage
	Result        json.RawMessage
	Status        string
	CompletedAt   time.Time
}

const runnerOperationColumns = `id, namespace_id, attempt_id, operation_kind, policy_digest,
	request, result, status, created_at, completed_at`

// InsertRunnerOperation records one runner_operations row using the Store's
// own pooled connection (no caller-managed transaction). See
// InsertRunnerOperationTx for a caller with its own transaction to compose
// into.
func (s *Store) InsertRunnerOperation(ctx context.Context, in InsertRunnerOperationInput) (RunnerOperationRecord, error) {
	return insertRunnerOperation(ctx, s.pool, in)
}

// InsertRunnerOperationTx is InsertRunnerOperation scoped to a
// caller-managed transaction.
func InsertRunnerOperationTx(ctx context.Context, tx pgx.Tx, in InsertRunnerOperationInput) (RunnerOperationRecord, error) {
	return insertRunnerOperation(ctx, tx, in)
}

func insertRunnerOperation(ctx context.Context, q querier, in InsertRunnerOperationInput) (RunnerOperationRecord, error) {
	switch {
	case in.ID == "":
		return RunnerOperationRecord{}, fmt.Errorf("postgres: InsertRunnerOperation: id is required")
	case in.NamespaceID == "":
		return RunnerOperationRecord{}, fmt.Errorf("postgres: InsertRunnerOperation: namespaceID is required")
	case in.OperationKind == "":
		return RunnerOperationRecord{}, fmt.Errorf("postgres: InsertRunnerOperation: operationKind is required")
	case in.PolicyDigest == "":
		return RunnerOperationRecord{}, fmt.Errorf("postgres: InsertRunnerOperation: policyDigest is required")
	case len(in.Request) == 0:
		return RunnerOperationRecord{}, fmt.Errorf("postgres: InsertRunnerOperation: request is required")
	}

	status := in.Status
	if status == "" {
		status = "pending"
	}

	row := q.QueryRow(ctx, `
		INSERT INTO runner_operations (
			id, namespace_id, attempt_id, operation_kind, policy_digest,
			request, result, status, completed_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+runnerOperationColumns,
		in.ID, in.NamespaceID, textOrNull(in.AttemptID), in.OperationKind, in.PolicyDigest,
		in.Request, jsonBytesOrNull(in.Result), status, tsOrNull(in.CompletedAt),
	)

	rec, err := scanRunnerOperationRow(row)
	if err != nil {
		return RunnerOperationRecord{}, fmt.Errorf("postgres: InsertRunnerOperation: %w", err)
	}
	return rec, nil
}

// GetRunnerOperation fetches one runner_operations row by id. It returns
// ErrNotFound if no such row exists.
func (s *Store) GetRunnerOperation(ctx context.Context, id string) (RunnerOperationRecord, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+runnerOperationColumns+` FROM runner_operations WHERE id = $1`, id)
	rec, err := scanRunnerOperationRow(row)
	if err != nil {
		if isNoRows(err) {
			return RunnerOperationRecord{}, ErrNotFound
		}
		return RunnerOperationRecord{}, fmt.Errorf("postgres: GetRunnerOperation: %w", err)
	}
	return rec, nil
}

// ListRunnerOperationsByAttempt returns every runner_operations row recorded
// against one attempt, ordered by creation time — a pre_run row before its
// sibling post_run row, since pre_run always executes first.
func (s *Store) ListRunnerOperationsByAttempt(ctx context.Context, attemptID string) ([]RunnerOperationRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+runnerOperationColumns+`
		FROM runner_operations
		WHERE attempt_id = $1
		ORDER BY created_at, id`, attemptID)
	if err != nil {
		return nil, fmt.Errorf("postgres: ListRunnerOperationsByAttempt: %w", err)
	}
	defer rows.Close()

	var out []RunnerOperationRecord
	for rows.Next() {
		rec, err := scanRunnerOperationRow(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: ListRunnerOperationsByAttempt: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: ListRunnerOperationsByAttempt: %w", err)
	}
	return out, nil
}

func scanRunnerOperationRow(row artifactRowScanner) (RunnerOperationRecord, error) {
	var (
		id, namespaceID, operationKind, policyDigest, status string
		attemptID                                            pgtype.Text
		request, result                                      []byte
		createdAt, completedAt                               pgtype.Timestamptz
	)
	if err := row.Scan(
		&id, &namespaceID, &attemptID, &operationKind, &policyDigest,
		&request, &result, &status, &createdAt, &completedAt,
	); err != nil {
		return RunnerOperationRecord{}, err
	}
	return RunnerOperationRecord{
		ID:            id,
		NamespaceID:   namespaceID,
		AttemptID:     textOrEmpty(attemptID),
		OperationKind: operationKind,
		PolicyDigest:  policyDigest,
		Request:       json.RawMessage(request),
		Result:        jsonOrNil(result),
		Status:        status,
		CreatedAt:     tsValue(createdAt),
		CompletedAt:   tsValue(completedAt),
	}, nil
}

// jsonBytesOrNull converts an empty payload to a NULL JSONB value and any
// other payload to itself. Unlike jsonOrEmptyObject (used for NOT NULL JSONB
// columns elsewhere in this package), runner_operations.result is nullable:
// a dispatch refusal has no result to report, and NULL says so directly
// instead of standing a fabricated "{}" in for "there was never a result".
func jsonBytesOrNull(data json.RawMessage) any {
	if len(data) == 0 {
		return nil
	}
	return data
}

// jsonOrNil is jsonBytesOrNull's inverse for reading a nullable JSONB column
// back: an absent column reads back as a nil json.RawMessage rather than an
// empty non-nil one, so a caller can tell "no result was recorded" from "an
// empty object was recorded" with a plain nil check.
func jsonOrNil(data []byte) json.RawMessage {
	if len(data) == 0 {
		return nil
	}
	return json.RawMessage(data)
}

// tsOrNull converts a zero time.Time to a NULL timestamptz and any other
// time to a valid one — the inverse convention of tsOrNow (used where a
// timestamp column is NOT NULL and a missing value should default to "now"
// rather than stay absent).
func tsOrNull(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}
