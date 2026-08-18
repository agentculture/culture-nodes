package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
)

// The store half of task t16's cross-issue concurrency ceiling (spec
// c36/h21), built on 0038/triggersubject.go's per-subject dedup: a
// workflow's `limits.maxConcurrentSubjectRuns` bounds how many DIFFERENT
// subjects it may run at once, and a trigger that would exceed it queues a
// deferred_triggers row (migration 0039) instead of creating a run. Split
// out of engine_store.go for the same reason triggersubject.go was
// (tests/lint/filelength_test.go's 1000-line cap) -- nothing here could not
// have lived there.
//
// See internal/engine/trigger.go's TriggerEvent and DrainSubjectTriggerQueue
// for the callers and the lock-ordering argument that makes the queries
// below race-free: every method here runs under the workflow-scoped
// advisory lock (triggerWorkflowLockKey) TriggerEvent takes before touching
// any of them, so none of these queries need — or take — a lock of their
// own.

// selectActiveSubjectRunCountSQL mirrors selectActiveRunBySubjectSQL
// (triggersubject.go) exactly, except it counts every non-terminal
// subject-bearing run of the workflow together instead of looking up one
// subject.
const selectActiveSubjectRunCountSQL = `
SELECT COUNT(*)::int
FROM runs AS r
JOIN workflow_versions AS wv ON wv.id = r.workflow_version_id
WHERE r.namespace_id = $1 AND wv.workflow_key = $2 AND r.subject IS NOT NULL
  AND r.status NOT IN ('completed', 'failed', 'cancelled')
`

// ActiveSubjectRunCount is engine.Tx's ActiveSubjectRunCount.
func (eq engineQueries) ActiveSubjectRunCount(ctx context.Context, workflowKey string) (int, error) {
	if workflowKey == "" {
		return 0, errors.New("postgres: engine: ActiveSubjectRunCount requires a workflow key")
	}
	var count int
	if err := eq.q.QueryRow(ctx, selectActiveSubjectRunCountSQL, eq.namespaceID, workflowKey).Scan(&count); err != nil {
		return 0, fmt.Errorf("postgres: engine: ActiveSubjectRunCount: %w", err)
	}
	return count, nil
}

const deferredTriggerColumns = `
	id, workflow_key, workflow_digest, source_format, source, normalized_ir,
	subject, trigger_event_id, event_name, event_emitter, event_payload,
	attempts, created_at, updated_at
`

func scanDeferredTrigger(row pgx.Row) (engine.DeferredTrigger, error) {
	var (
		d         engine.DeferredTrigger
		ir        []byte
		payload   []byte
		createdAt pgtype.Timestamptz
		updatedAt pgtype.Timestamptz
	)
	if err := row.Scan(
		&d.ID, &d.WorkflowKey, &d.WorkflowDigest, &d.SourceFormat, &d.Source, &ir,
		&d.Subject, &d.TriggerEventID, &d.EventName, &d.EventEmitter, &payload,
		&d.Attempts, &createdAt, &updatedAt,
	); err != nil {
		return engine.DeferredTrigger{}, err
	}
	d.NormalizedIR = json.RawMessage(ir)
	d.EventPayload = json.RawMessage(payload)
	d.CreatedAt = tsValue(createdAt)
	d.UpdatedAt = tsValue(updatedAt)
	return d, nil
}

const selectDeferredTriggerByWorkflowSubjectSQL = `
SELECT ` + deferredTriggerColumns + `
FROM deferred_triggers
WHERE namespace_id = $1 AND workflow_key = $2 AND subject = $3
`

// FindDeferredTrigger is engine.Tx's FindDeferredTrigger.
func (eq engineQueries) FindDeferredTrigger(ctx context.Context, workflowKey, subject string) (engine.DeferredTrigger, bool, error) {
	if workflowKey == "" || subject == "" {
		return engine.DeferredTrigger{}, false, errors.New("postgres: engine: FindDeferredTrigger requires workflowKey and subject")
	}
	d, err := scanDeferredTrigger(eq.q.QueryRow(ctx, selectDeferredTriggerByWorkflowSubjectSQL, eq.namespaceID, workflowKey, subject))
	if err != nil {
		if isNoRows(err) {
			return engine.DeferredTrigger{}, false, nil
		}
		return engine.DeferredTrigger{}, false, fmt.Errorf("postgres: engine: FindDeferredTrigger: %w", err)
	}
	return d, true, nil
}

const insertDeferredTriggerSQL = `
INSERT INTO deferred_triggers (
	id, namespace_id, workflow_key, workflow_digest, source_format, source, normalized_ir,
	subject, trigger_event_id, event_name, event_emitter, event_payload
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING ` + deferredTriggerColumns

// InsertDeferredTrigger is engine.Tx's InsertDeferredTrigger. It is only
// ever called after the caller's own FindDeferredTrigger confirmed this
// (workflow, subject) held nothing already -- see TriggerEvent -- so a
// unique-constraint violation here (deferred_triggers_workflow_subject_key)
// means the caller's invariant broke, and this returns the underlying error
// rather than silently reinterpreting it as a touch.
func (eq engineQueries) InsertDeferredTrigger(ctx context.Context, in engine.DeferredTriggerInput) (engine.DeferredTrigger, error) {
	if in.WorkflowKey == "" || in.Subject == "" || in.WorkflowDigest == "" {
		return engine.DeferredTrigger{}, errors.New("postgres: engine: InsertDeferredTrigger requires workflowKey, workflowDigest, and subject")
	}
	d, err := scanDeferredTrigger(eq.q.QueryRow(ctx, insertDeferredTriggerSQL,
		store.NewULID(), eq.namespaceID, in.WorkflowKey, in.WorkflowDigest, in.SourceFormat, in.Source, []byte(jsonOrEmptyObject(in.NormalizedIR)),
		in.Subject, in.TriggerEventID, in.EventName, in.EventEmitter, []byte(jsonOrEmptyObject(in.EventPayload)),
	))
	if err != nil {
		return engine.DeferredTrigger{}, fmt.Errorf("postgres: engine: InsertDeferredTrigger: %w", err)
	}
	return d, nil
}

const touchDeferredTriggerSQL = `
UPDATE deferred_triggers
SET workflow_digest = $2, source_format = $3, source = $4, normalized_ir = $5,
    trigger_event_id = $6, event_name = $7, event_emitter = $8, event_payload = $9,
    attempts = attempts + 1, updated_at = now()
WHERE id = $1
`

// TouchDeferredTrigger is engine.Tx's TouchDeferredTrigger: it replaces the
// queued row's triggering event with a fresher one and bumps its attempt
// count, but never its id or created_at -- the subject's place in FIFO drain
// order is exactly where it first queued.
func (eq engineQueries) TouchDeferredTrigger(ctx context.Context, id string, in engine.DeferredTriggerInput) error {
	if id == "" {
		return errors.New("postgres: engine: TouchDeferredTrigger requires an id")
	}
	tag, err := eq.q.Exec(ctx, touchDeferredTriggerSQL,
		id, in.WorkflowDigest, in.SourceFormat, in.Source, []byte(jsonOrEmptyObject(in.NormalizedIR)),
		in.TriggerEventID, in.EventName, in.EventEmitter, []byte(jsonOrEmptyObject(in.EventPayload)),
	)
	if err != nil {
		return fmt.Errorf("postgres: engine: TouchDeferredTrigger: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres: engine: TouchDeferredTrigger: no deferred trigger %s: %w", id, engine.ErrNotFound)
	}
	return nil
}

const selectOldestDeferredTriggerSQL = `
SELECT ` + deferredTriggerColumns + `
FROM deferred_triggers
WHERE namespace_id = $1 AND workflow_key = $2
ORDER BY created_at ASC
LIMIT 1
`

// OldestDeferredTrigger is engine.Tx's OldestDeferredTrigger: FIFO across
// every subject queued for workflowKey, which is what DrainSubjectTriggerQueue
// pops after a subject-bearing run of this workflow goes terminal.
func (eq engineQueries) OldestDeferredTrigger(ctx context.Context, workflowKey string) (engine.DeferredTrigger, bool, error) {
	if workflowKey == "" {
		return engine.DeferredTrigger{}, false, errors.New("postgres: engine: OldestDeferredTrigger requires a workflow key")
	}
	d, err := scanDeferredTrigger(eq.q.QueryRow(ctx, selectOldestDeferredTriggerSQL, eq.namespaceID, workflowKey))
	if err != nil {
		if isNoRows(err) {
			return engine.DeferredTrigger{}, false, nil
		}
		return engine.DeferredTrigger{}, false, fmt.Errorf("postgres: engine: OldestDeferredTrigger: %w", err)
	}
	return d, true, nil
}

// DeleteDeferredTrigger is engine.Tx's DeleteDeferredTrigger.
func (eq engineQueries) DeleteDeferredTrigger(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("postgres: engine: DeleteDeferredTrigger requires an id")
	}
	if _, err := eq.q.Exec(ctx, `DELETE FROM deferred_triggers WHERE id = $1`, id); err != nil {
		return fmt.Errorf("postgres: engine: DeleteDeferredTrigger: %w", err)
	}
	return nil
}
