package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/store"
)

// The clarify-then-commit gate's durable state (migration 0026, task t14 of
// the upkeep-actors-jira plan; issue #67).
//
// One row per issued preflight: which dispatch it briefs, which derived
// ledger record states the briefing, when the window closes, whether an
// actor acknowledged it, and whether a dispatch has already ridden it.
//
// The gate's EVIDENCE is two ledger records (a derived `dispatch_preflight`
// and a proposed `dispatch_acknowledgement`) and it stays there. What lives
// here is the part immutable records cannot express: SINGLE USE. The
// protocol this generalizes consumes its confirmation file on use, so a
// second action needs a second confirmation
// (tests/deploy/destructiveconfirm_test.go); expressing that by appending a
// third record would still let two workers read one acknowledgement as
// unconsumed at the same instant. ConsumePreflight is a conditional UPDATE,
// so exactly one dispatch can ever win.
//
// Three properties are enforced here rather than left to callers:
//
//   - AN ACKNOWLEDGEMENT IS NOT A FLAG, IT IS A WINDOWED FACT. Every path
//     compares expires_at against a caller-supplied now, and an expired row
//     authorizes nothing whether or not it was acknowledged. Nothing sweeps
//     expired rows: they stay readable as the record of what was asked, the
//     same way actor_availability keeps expired pauses (migration 0020).
//   - CONSUMPTION REQUIRES AN ACKNOWLEDGEMENT. The UPDATE's WHERE demands
//     acknowledged_at IS NOT NULL, so "the dispatch proceeded without an
//     acknowledgement" is not a code path that exists to be got wrong.
//   - AN EXPIRED OR CONSUMED PREFLIGHT IS NO LONGER THE OPEN ONE, so the
//     next claim of the same node run composes a FRESH briefing against
//     today's host state rather than reviving a stale one.

// Preflight is one dispatch_preflights row.
type Preflight struct {
	ID          string
	NamespaceID string
	RunID       string
	NodeRunID   string
	NodeID      string
	// ActorKey is the identity the dispatch is addressed to; ActorID is the
	// actors-table row id when the registry could resolve one, "" when it
	// could not (never a fabricated id).
	ActorKey string
	ActorID  string
	// RecordID is the derived `dispatch_preflight` ledger record stating
	// what the actor was told, and RecordDigest is that record's content
	// digest — kept here so an acknowledgement can be checked against WHAT
	// the briefing said, not merely against which briefing it was.
	RecordID       string
	RecordDigest   string
	IssuedAt       time.Time
	ExpiresAt      time.Time
	AcknowledgedAt *time.Time
	// AcknowledgedBy is the actor the acknowledgement was recorded for, and
	// AcknowledgementRecordID is the proposed ledger record carrying the
	// claim. Both are empty until an acknowledgement lands.
	AcknowledgedBy          string
	AcknowledgementRecordID string
	ConsumedAt              *time.Time
	ConsumedByAttemptID     string
	UpdatedAt               time.Time
}

// Acknowledged reports whether an actor has answered this preflight. It says
// nothing about whether the answer is still good — see Expired.
func (p Preflight) Acknowledged() bool { return p.AcknowledgedAt != nil }

// Consumed reports whether a dispatch has already ridden this preflight.
func (p Preflight) Consumed() bool { return p.ConsumedAt != nil }

// Expired reports whether the window has closed at now.
func (p Preflight) Expired(now time.Time) bool { return !p.ExpiresAt.After(now) }

// Usable reports whether this preflight authorizes a dispatch right now:
// acknowledged, unexpired, and not yet spent. It is the predicate the
// dispatch site asks, stated once here so no caller assembles it from parts.
func (p Preflight) Usable(now time.Time) bool {
	return p.Acknowledged() && !p.Consumed() && !p.Expired(now)
}

// IssuePreflightInput carries one new row. The id is minted here; every
// other field is the caller's statement about the dispatch being briefed.
type IssuePreflightInput struct {
	NamespaceID  string
	RunID        string
	NodeRunID    string
	NodeID       string
	ActorKey     string
	ActorID      string
	RecordID     string
	RecordDigest string
	IssuedAt     time.Time
	ExpiresAt    time.Time
}

const preflightColumns = `id, namespace_id, run_id, node_run_id, node_id, actor_key, actor_id,
	record_id, record_digest, issued_at, expires_at, acknowledged_at, acknowledged_by,
	acknowledgement_record_id, consumed_at, consumed_by_attempt_id, updated_at`

func scanPreflight(row interface{ Scan(dest ...any) error }) (Preflight, error) {
	var (
		p              Preflight
		actorID        pgtype.Text
		acknowledgedAt pgtype.Timestamptz
		acknowledgedBy pgtype.Text
		ackRecordID    pgtype.Text
		consumedAt     pgtype.Timestamptz
		consumedBy     pgtype.Text
		issuedAt       pgtype.Timestamptz
		expiresAt      pgtype.Timestamptz
		updatedAt      pgtype.Timestamptz
	)
	if err := row.Scan(
		&p.ID, &p.NamespaceID, &p.RunID, &p.NodeRunID, &p.NodeID, &p.ActorKey, &actorID,
		&p.RecordID, &p.RecordDigest, &issuedAt, &expiresAt, &acknowledgedAt, &acknowledgedBy,
		&ackRecordID, &consumedAt, &consumedBy, &updatedAt,
	); err != nil {
		return Preflight{}, err
	}
	p.ActorID = textOrEmpty(actorID)
	p.IssuedAt = tsValue(issuedAt)
	p.ExpiresAt = tsValue(expiresAt)
	p.AcknowledgedAt = tsPointer(acknowledgedAt)
	p.AcknowledgedBy = textOrEmpty(acknowledgedBy)
	p.AcknowledgementRecordID = textOrEmpty(ackRecordID)
	p.ConsumedAt = tsPointer(consumedAt)
	p.ConsumedByAttemptID = textOrEmpty(consumedBy)
	p.UpdatedAt = tsValue(updatedAt)
	return p, nil
}

// tsPointer returns nil for a NULL timestamp rather than a zero time a
// reader would mistake for the epoch.
func tsPointer(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	v := ts.Time.UTC()
	return &v
}

// IssuePreflight records a newly composed briefing. It always INSERTs: a
// preflight is issued against the host state and task declaration as they
// were at that moment, so re-issuing after an expiry is a new briefing, not
// an update to the old one — and the expired row stays readable as the
// record of what was asked and never answered.
func (s *Store) IssuePreflight(ctx context.Context, in IssuePreflightInput) (Preflight, error) {
	issuedAt := in.IssuedAt
	if issuedAt.IsZero() {
		issuedAt = time.Now().UTC()
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO dispatch_preflights (
			id, namespace_id, run_id, node_run_id, node_id, actor_key, actor_id,
			record_id, record_digest, issued_at, expires_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, now())
		RETURNING `+preflightColumns,
		store.NewULID(), in.NamespaceID, in.RunID, in.NodeRunID, in.NodeID, in.ActorKey,
		textOrNil(in.ActorID), in.RecordID, in.RecordDigest, issuedAt, in.ExpiresAt)

	p, err := scanPreflight(row)
	if err != nil {
		return Preflight{}, fmt.Errorf("postgres: IssuePreflight for node run %s: %w", in.NodeRunID, err)
	}
	return p, nil
}

// OpenPreflight returns the newest UNCONSUMED preflight for a node run.
//
// Consumed rows are excluded rather than filtered by the caller because
// "open" is the question the dispatch site actually asks, and a consumed row
// answers a different one (which briefing did the previous dispatch ride).
// Expiry is deliberately NOT filtered here: an expired-unacknowledged row is
// exactly what tells the dispatch site to refuse rather than to re-issue,
// and hiding it would make those two cases indistinguishable.
func (s *Store) OpenPreflight(ctx context.Context, namespaceID, nodeRunID string) (Preflight, bool, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+preflightColumns+`
		FROM dispatch_preflights
		WHERE namespace_id = $1 AND node_run_id = $2 AND consumed_at IS NULL
		ORDER BY issued_at DESC, id DESC
		LIMIT 1
	`, namespaceID, nodeRunID)

	p, err := scanPreflight(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Preflight{}, false, nil
	}
	if err != nil {
		return Preflight{}, false, fmt.Errorf("postgres: OpenPreflight for node run %s: %w", nodeRunID, err)
	}
	return p, true, nil
}

// Preflight returns one row by id, or ErrNotFound.
func (s *Store) Preflight(ctx context.Context, namespaceID, id string) (Preflight, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+preflightColumns+` FROM dispatch_preflights WHERE namespace_id = $1 AND id = $2`,
		namespaceID, id)
	p, err := scanPreflight(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Preflight{}, fmt.Errorf("preflight %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Preflight{}, fmt.Errorf("postgres: Preflight %s: %w", id, err)
	}
	return p, nil
}

// PendingPreflights lists the unconsumed, unacknowledged, unexpired
// briefings waiting on an actor — what a bridge or an operator polls to
// learn there is something to read. An empty actorKey lists every actor's.
//
// It exists so a pending gate is FINDABLE: the issue event goes onto the run
// stream, and an actor that was not watching at that instant would otherwise
// have no way to discover it was being waited on.
func (s *Store) PendingPreflights(ctx context.Context, namespaceID, actorKey string, now time.Time, limit int) ([]Preflight, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+preflightColumns+`
		FROM dispatch_preflights
		WHERE namespace_id = $1
		  AND ($2 = '' OR actor_key = $2)
		  AND consumed_at IS NULL
		  AND acknowledged_at IS NULL
		  AND expires_at > $3
		ORDER BY issued_at DESC, id DESC
		LIMIT $4
	`, namespaceID, actorKey, now, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: PendingPreflights: %w", err)
	}
	defer rows.Close()

	out := make([]Preflight, 0)
	for rows.Next() {
		p, err := scanPreflight(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: PendingPreflights: scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ErrPreflightNotAcknowledgeable reports that a preflight cannot take an
// acknowledgement: it has expired, it was already consumed, or it was
// already acknowledged. All three are refusals of the same shape — the
// window for answering this briefing is closed — and the caller renders them
// as one 409 rather than leaking which.
var ErrPreflightNotAcknowledgeable = errors.New("postgres: preflight is not acknowledgeable")

// AcknowledgePreflightInput carries one acknowledgement.
type AcknowledgePreflightInput struct {
	NamespaceID string
	ID          string
	// AcknowledgedBy is the actor the acknowledgement is recorded for, and
	// AcknowledgementRecordID is the proposed ledger record that carries the
	// claim. The record is appended first: a row that says it was
	// acknowledged must always point at the evidence that it was.
	AcknowledgedBy          string
	AcknowledgementRecordID string
	Now                     time.Time
}

// AcknowledgePreflight records an actor's answer, or returns
// ErrPreflightNotAcknowledgeable.
//
// The conditional UPDATE is what makes a second acknowledgement of one
// briefing impossible: acknowledged_at IS NULL is in the WHERE, so two
// concurrent answers resolve to one row change and one refusal rather than
// to a last-writer-wins overwrite that would silently replace which record
// the row points at.
func (s *Store) AcknowledgePreflight(ctx context.Context, in AcknowledgePreflightInput) (Preflight, error) {
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	row := s.pool.QueryRow(ctx, `
		UPDATE dispatch_preflights
		SET acknowledged_at           = $3,
		    acknowledged_by           = $4,
		    acknowledgement_record_id = $5,
		    updated_at                = now()
		WHERE namespace_id = $1
		  AND id = $2
		  AND acknowledged_at IS NULL
		  AND consumed_at IS NULL
		  AND expires_at > $3
		RETURNING `+preflightColumns,
		in.NamespaceID, in.ID, now, textOrNil(in.AcknowledgedBy), textOrNil(in.AcknowledgementRecordID))

	p, err := scanPreflight(row)
	if errors.Is(err, pgx.ErrNoRows) {
		// Distinguish "no such preflight" from "not acknowledgeable": the
		// two are different answers to the caller, and only the second is
		// about this row's state.
		if _, readErr := s.Preflight(ctx, in.NamespaceID, in.ID); readErr != nil {
			return Preflight{}, readErr
		}
		return Preflight{}, fmt.Errorf("preflight %s: %w", in.ID, ErrPreflightNotAcknowledgeable)
	}
	if err != nil {
		return Preflight{}, fmt.Errorf("postgres: AcknowledgePreflight %s: %w", in.ID, err)
	}
	return p, nil
}

// ConsumePreflight spends an acknowledgement on one dispatch and reports
// whether this caller is the one that spent it.
//
// false is not an error: it means the acknowledgement was not there to spend
// — never made, already spent by another worker, or expired since. Every one
// of those is a "do not dispatch" the caller handles, and none of them is a
// failure of this call.
func (s *Store) ConsumePreflight(ctx context.Context, namespaceID, id, attemptID string, now time.Time) (bool, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE dispatch_preflights
		SET consumed_at             = $3,
		    consumed_by_attempt_id  = $4,
		    updated_at              = now()
		WHERE namespace_id = $1
		  AND id = $2
		  AND acknowledged_at IS NOT NULL
		  AND consumed_at IS NULL
		  AND expires_at > $3
	`, namespaceID, id, now, textOrNil(attemptID))
	if err != nil {
		return false, fmt.Errorf("postgres: ConsumePreflight %s: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}

// textOrNil maps an empty string to SQL NULL, so an absent value reads back
// as absent rather than as a present empty string.
func textOrNil(v string) any {
	if v == "" {
		return nil
	}
	return v
}
