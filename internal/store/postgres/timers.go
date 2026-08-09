package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres/sqlcgen"
)

// Durable timers (docs/initial-design/culture-nodes-prd-spec.md §12.7):
// "Waits, retries, deadlines, and lease recovery are durable rows, not
// in-memory timers. The scheduler claims due timers in bounded batches and
// writes the resulting state change and outbox event transactionally."
// This file is the store-layer half of that: ScheduleTimer inserts a
// durable row, ClaimDueTimers is the scheduler's bounded SKIP LOCKED batch
// claim, MarkFired (and its transactional twin MarkFiredTx) is the guarded
// completion write, and CancelTimer retires a row that is no longer
// needed. internal/scheduler is the other half: it owns the tick loop, the
// single-active advisory lock, and composing MarkFiredTx + an effect + an
// outbox insert into one per-timer transaction.
//
// Deviation from this task's brief, which sketches the type as
// `Timer{ID, NamespaceID, Kind, SubjectID, FireAt}`: the timers table
// (migrations/0002_runtime_execution.sql) carries the subject as two
// separate, independently-optional foreign keys -- run_id and node_run_id
// -- not one collapsed subject_id column. The Go type mirrors the schema
// instead of inventing a field the table does not have. In practice a
// wait/retry/deadline timer's subject is its NodeRunID (the work_items row
// it will make available again), and a lease_recovery timer typically
// carries neither -- ReclaimExpired is a namespace-wide sweep, not scoped
// to one node run (see internal/scheduler's doc comment).

// TimerKind is timers.timer_kind: what a fired timer means the scheduler
// should do (prd-spec §12.7).
type TimerKind string

const (
	// TimerKindWait fires when a node's durable wait (e.g. a workflow
	// "sleep until") elapses. Effect: make the subject work item
	// available now.
	TimerKindWait TimerKind = "wait"
	// TimerKindRetry fires when a failed attempt's backoff elapses.
	// Effect: make the subject work item available now.
	TimerKindRetry TimerKind = "retry"
	// TimerKindDeadline fires when an async actor invocation
	// (prd-spec §12.6) or a human task exceeds its durable deadline.
	// Effect: append a deadline-expired outbox event; this is a domain
	// outcome for the engine to react to, not itself a state mutation.
	TimerKindDeadline TimerKind = "deadline"
	// TimerKindLeaseRecovery is a durable, timer-driven trigger for
	// Store.ReclaimExpired, in addition to the scheduler's own standing
	// periodic sweep (prd-spec §20.4's "Worker dies before dispatch"
	// row) -- see internal/scheduler's doc comment for why lease
	// recovery is not purely timer-driven.
	TimerKindLeaseRecovery TimerKind = "lease_recovery"
)

// Timer status values (timers.status).
const (
	TimerStatusPending  = "pending"
	TimerStatusFired    = "fired"
	TimerStatusCanceled = "canceled"
)

// Timer is a timers row (prd-spec §12.7, migrations/0002_runtime_execution.sql).
// RunID, NodeRunID, ClaimedBy, and Payload read back as their zero value
// when the column is NULL / empty.
type Timer struct {
	ID          string
	NamespaceID string
	RunID       string
	NodeRunID   string
	Kind        TimerKind
	FireAt      time.Time
	Status      string
	ClaimedBy   string
	ClaimedAt   time.Time
	Payload     json.RawMessage
	CreatedAt   time.Time
}

const timerColumns = `id, namespace_id, run_id, node_run_id, timer_kind, fire_at,
	status, claimed_by, claimed_at, payload, created_at`

// timerColumnsT is timerColumns qualified with the "t" alias claimDueTimersSQL
// gives the timers table. It is required there (unlike scheduleTimerSQL's
// unqualified RETURNING): an UPDATE ... FROM statement's RETURNING clause
// can see columns from both the target table and the FROM-list, so an
// unqualified "id" is ambiguous between t.id and the due subquery's id.
const timerColumnsT = `t.id, t.namespace_id, t.run_id, t.node_run_id, t.timer_kind, t.fire_at,
	t.status, t.claimed_by, t.claimed_at, t.payload, t.created_at`

// scheduleTimerSQL is an upsert-returning-existing: ON CONFLICT (id) DO
// UPDATE SET id = timers.id is a deliberate no-op write (a column set to
// its own current value) whose only purpose is to make RETURNING fire on a
// conflict too, so ScheduleTimer always hands back the row that now exists
// under this ID -- freshly inserted, or already there from an earlier call
// with the same ID -- in one round trip, without ever mutating an existing
// row's fields. That is what makes ScheduleTimer idempotent by ID: calling
// it twice with the same ID is exactly as safe as calling it once.
const scheduleTimerSQL = `
INSERT INTO timers (id, namespace_id, run_id, node_run_id, timer_kind, fire_at, payload)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (id) DO UPDATE SET id = timers.id
RETURNING ` + timerColumns

// ScheduleTimer inserts a durable timer row, or -- if in.ID already exists
// -- returns the existing row unchanged. Payload defaults to "{}" when nil.
func (s *Store) ScheduleTimer(ctx context.Context, in Timer) (Timer, error) {
	switch {
	case in.ID == "":
		return Timer{}, fmt.Errorf("postgres: ScheduleTimer: id is required")
	case in.NamespaceID == "":
		return Timer{}, fmt.Errorf("postgres: ScheduleTimer: namespaceID is required")
	case in.Kind == "":
		return Timer{}, fmt.Errorf("postgres: ScheduleTimer: kind is required")
	case in.FireAt.IsZero():
		return Timer{}, fmt.Errorf("postgres: ScheduleTimer: fireAt is required")
	}

	row := s.pool.QueryRow(ctx, scheduleTimerSQL,
		in.ID, in.NamespaceID, textOrNull(in.RunID), textOrNull(in.NodeRunID),
		string(in.Kind), tsOrNow(in.FireAt), jsonOrEmptyObject(in.Payload),
	)

	t, err := scanTimerRow(row)
	if err != nil {
		return Timer{}, fmt.Errorf("postgres: ScheduleTimer: %w", err)
	}
	return t, nil
}

// claimDueTimersSQL borrows ClaimWork's atomic-claim shape (claiming.go):
// the subquery picks up to $3 pending, due rows under FOR UPDATE SKIP
// LOCKED (so two concurrent callers never block on, or deadlock with, each
// other's in-flight claim), and the outer UPDATE stamps exactly those rows
// with this caller's owner id and a claim timestamp, in the same statement.
//
// Unlike ClaimWork, this is deliberately NOT a single-winner claim: it
// leaves status at 'pending' rather than transitioning it, so a timer that
// gets claimed but never reaches a committed MarkFiredTx (its owner
// crashed, or lost the scheduler's advisory lock mid-batch) is still
// 'pending' and therefore still due -- the very next ClaimDueTimers call
// (the same scheduler's next tick, or a standby that has since taken over)
// claims it again, no unstick step required. The tradeoff that follows
// directly from that choice: if two owners' ClaimDueTimers calls really do
// race (e.g. a split-brain window where an old active instance has not yet
// noticed it lost the lock), BOTH can claim the same row -- claimed_by
// simply ends up as whichever caller's UPDATE committed last. That is safe
// only because claiming a timer is not the same as firing it: MarkFired's
// guard (WHERE claimed_by = $2 AND status = 'pending') is the actual
// single-winner boundary, and internal/scheduler always runs a timer's
// effect and its MarkFiredTx in one transaction that it rolls back
// whole-cloth when that guard reports failure -- so a claim a caller has
// since lost never lets it commit an effect. See MarkFired's and
// internal/scheduler's doc comments for the rest of that guarantee.
const claimDueTimersSQL = `
UPDATE timers AS t
SET claimed_by = $1,
    claimed_at = now()
FROM (
    SELECT id
    FROM timers
    WHERE status = 'pending' AND fire_at <= $2
    ORDER BY fire_at, id
    FOR UPDATE SKIP LOCKED
    LIMIT $3
) AS due
WHERE t.id = due.id
RETURNING ` + timerColumnsT

// ClaimDueTimers atomically claims up to limit pending timers whose fire_at
// is at or before now (time.Now().UTC() when now is zero) for ownerID. It
// returns the rows it won -- possibly fewer than limit, possibly zero when
// nothing is due right now -- never an error merely because nothing was
// claimable.
func (s *Store) ClaimDueTimers(ctx context.Context, ownerID string, now time.Time, limit int) ([]Timer, error) {
	switch {
	case ownerID == "":
		return nil, fmt.Errorf("postgres: ClaimDueTimers: ownerID is required")
	case limit <= 0:
		return nil, fmt.Errorf("postgres: ClaimDueTimers: limit must be positive")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	rows, err := s.pool.Query(ctx, claimDueTimersSQL, ownerID, now, int64(limit))
	if err != nil {
		return nil, fmt.Errorf("postgres: ClaimDueTimers: %w", err)
	}
	defer rows.Close()

	var claimed []Timer
	for rows.Next() {
		t, err := scanTimerRow(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: ClaimDueTimers: scan: %w", err)
		}
		claimed = append(claimed, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: ClaimDueTimers: %w", err)
	}

	return claimed, nil
}

// markFiredSQL guards the completion write with the exact claim a caller
// must currently hold: the timer must still be 'pending' (not already
// fired or canceled by someone else) and claimed_by must still equal the
// caller's ownerID (not overwritten by a later ClaimDueTimers call from a
// different owner, e.g. a standby that took over mid-processing). Zero
// rows affected means that guard failed -- the caller no longer holds the
// claim it thinks it holds -- which internal/scheduler treats as "someone
// else is already handling this timer" and skips (see its fireOne doc
// comment), never as a hard error.
const markFiredSQL = `
UPDATE timers
SET status = 'fired'
WHERE id = $1 AND claimed_by = $2 AND status = 'pending'
`

// querier is defined in artifacts.go (this package) and is satisfied by
// both *pgxpool.Pool and pgx.Tx; markFired reuses it so MarkFired and
// MarkFiredTx share one implementation.
func markFired(ctx context.Context, q querier, timerID, ownerID string) (bool, error) {
	if timerID == "" {
		return false, fmt.Errorf("postgres: MarkFired: timerID is required")
	}
	if ownerID == "" {
		return false, fmt.Errorf("postgres: MarkFired: ownerID is required")
	}
	tag, err := q.Exec(ctx, markFiredSQL, timerID, ownerID)
	if err != nil {
		return false, fmt.Errorf("postgres: MarkFired: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// MarkFired marks timerID fired using the Store's own pooled connection
// (no caller-managed transaction). It returns (false, nil) -- not an error
// -- when the guarded update affects no rows; see markFiredSQL's doc
// comment for why that is an expected outcome, not a malfunction.
func (s *Store) MarkFired(ctx context.Context, timerID, ownerID string) (bool, error) {
	return markFired(ctx, s.pool, timerID, ownerID)
}

// MarkFiredTx is MarkFired scoped to a caller-managed transaction.
// internal/scheduler composes this into the same transaction as a timer's
// effect and its outbox insert (see InsertOutboxTx), so "the effect
// applied", "the timer is marked fired", and "the outbox event exists" are
// all-or-nothing together -- a crash or error between them rolls back the
// whole transaction, leaving the timer 'pending' (and its effect, if any,
// undone) for the next ClaimDueTimers call to retry from scratch.
func MarkFiredTx(ctx context.Context, tx pgx.Tx, timerID, ownerID string) (bool, error) {
	return markFired(ctx, tx, timerID, ownerID)
}

// cancelTimerSQL only ever moves a row out of 'pending': a timer that has
// already fired or was already canceled is left untouched, matching this
// package's convention (see queue.Queue's Ack/Delay doc comments) that
// retiring something already-retired is a no-op, not an error.
const cancelTimerSQL = `
UPDATE timers
SET status = 'canceled'
WHERE id = $1 AND status = 'pending'
`

// CancelTimer retires timerID so it never fires. It is idempotent: calling
// it on a timer that has already fired, was already canceled, or does not
// exist at all is a silent no-op, never an error -- a caller with a stale
// or already-superseded timer ID has nothing useful to distinguish those
// cases by anyway.
func (s *Store) CancelTimer(ctx context.Context, timerID string) error {
	if timerID == "" {
		return fmt.Errorf("postgres: CancelTimer: timerID is required")
	}
	if _, err := s.pool.Exec(ctx, cancelTimerSQL, timerID); err != nil {
		return fmt.Errorf("postgres: CancelTimer: %w", err)
	}
	return nil
}

// InsertOutboxTx is Store.InsertOutbox (store.go) scoped to a
// caller-managed transaction, added here rather than there because this
// task's brief (t11) asks for new files only in this package.
// internal/scheduler uses it to fold a timer's outbox event into the same
// transaction as MarkFiredTx and the timer's effect -- see MarkFiredTx's
// doc comment for why that atomicity matters.
func InsertOutboxTx(ctx context.Context, s *Store, tx pgx.Tx, in InsertOutboxInput) (OutboxRecord, error) {
	switch {
	case in.NamespaceID == "":
		return OutboxRecord{}, fmt.Errorf("postgres: InsertOutboxTx: namespaceID is required")
	case in.Topic == "":
		return OutboxRecord{}, fmt.Errorf("postgres: InsertOutboxTx: topic is required")
	}

	qtx := s.q.WithTx(tx)
	row, err := qtx.InsertOutbox(ctx, sqlcgen.InsertOutboxParams{
		ID:          store.NewULID(),
		NamespaceID: in.NamespaceID,
		Topic:       in.Topic,
		Payload:     jsonOrEmptyObject(in.Payload),
		Status:      "pending",
		AvailableAt: tsOrNow(in.AvailableAt),
	})
	if err != nil {
		return OutboxRecord{}, fmt.Errorf("postgres: InsertOutboxTx: %w", err)
	}

	return OutboxRecord{
		ID:          row.ID,
		NamespaceID: row.NamespaceID,
		Topic:       row.Topic,
		Payload:     row.Payload,
		Status:      row.Status,
		AvailableAt: tsValue(row.AvailableAt),
		PublishedAt: tsPtr(row.PublishedAt),
		Attempts:    row.Attempts,
		CreatedAt:   tsValue(row.CreatedAt),
	}, nil
}

// timerRowScanner is satisfied by pgx.Row (QueryRow) and pgx.Rows (Query,
// per-row Scan during iteration) -- the same shape as artifacts.go's
// artifactRowScanner, defined again here (rather than shared) so this file
// reads standalone.
type timerRowScanner interface {
	Scan(dest ...any) error
}

func scanTimerRow(row timerRowScanner) (Timer, error) {
	var (
		id, namespaceID, kind, status string
		runID, nodeRunID, claimedBy   pgtype.Text
		fireAt, claimedAt, createdAt  pgtype.Timestamptz
		payload                       []byte
	)

	if err := row.Scan(
		&id, &namespaceID, &runID, &nodeRunID, &kind, &fireAt,
		&status, &claimedBy, &claimedAt, &payload, &createdAt,
	); err != nil {
		return Timer{}, err
	}

	return Timer{
		ID:          id,
		NamespaceID: namespaceID,
		RunID:       textOrEmpty(runID),
		NodeRunID:   textOrEmpty(nodeRunID),
		Kind:        TimerKind(kind),
		FireAt:      tsValue(fireAt),
		Status:      status,
		ClaimedBy:   textOrEmpty(claimedBy),
		ClaimedAt:   tsValue(claimedAt),
		Payload:     jsonOrEmptyObject(payload),
		CreatedAt:   tsValue(createdAt),
	}, nil
}
