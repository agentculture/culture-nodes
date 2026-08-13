package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// The capacity circuit breaker's durable state (migration 0020, task t9 of
// the economy-discord-graphs plan; issue #48 item 1, spec claim c4, honesty
// conditions h3/h38).
//
// One row per (namespace, actor key) says when that actor becomes
// dispatchable again and why it stopped being so. The dispatch site reads it
// one step before invoking (internal/worker/breaker.go); the actors read
// surface renders it; an operator clears it early through
// POST /v1alpha1/actors/{id}/resume.
//
// Three properties are load-bearing and each is enforced here rather than
// left to callers:
//
//   - A PAUSE IS A DEADLINE, NOT A FLAG. Row presence never means "paused";
//     paused_until > now() does. Nothing has to sweep expired rows, and an
//     expired row stays readable as the history of why the actor was
//     stopped.
//   - THE TRIP IS AN IDEMPOTENT UPSERT THAT ONLY EVER EXTENDS. Two workers
//     tripping the same actor concurrently is normal, not exceptional --
//     they are both talking to the same exhausted provider. The ON CONFLICT
//     branch keeps whichever paused_until is LATER, so last-writer-wins
//     resolves in the safe direction: a race may extend a pause, never
//     silently shorten one another worker already committed. (Shortening is
//     the direction that would let dispatch resume into a provider that is
//     still refusing.)
//   - THE ACTOR KEY IS THE IDENTITY. Not actors.id -- see migration 0020's
//     header for why capacity belongs to the identity rather than to one
//     append-only registration revision, and why the dispatch site can
//     always produce a key but only sometimes a row id.

// ActorPause is one actor_availability row: when an actor was stopped, until
// when, why, and (when it happened) who let it back in early.
type ActorPause struct {
	NamespaceID string
	ActorKey    string
	// PausedUntil is when dispatch may resume. Compare it against now()
	// rather than treating the row's existence as a pause -- Paused() and
	// Store.ActivePause both do exactly that.
	PausedUntil time.Time
	// Reason is the §13.5 error class that tripped the breaker
	// ("capacity_exhausted" today).
	Reason string
	// RetryAfterSeconds is the provider's own Retry-After hint. Nil means it
	// named none -- never 0, which would read as "retry immediately".
	RetryAfterSeconds *int32
	// Detail is one human line naming what happened.
	Detail string
	// TrippedAt and the TrippedBy* fields are the provenance that makes a
	// pause explainable from the row alone: which dispatch discovered the
	// exhaustion. Any of them may be "" for a pause tripped outside a run.
	TrippedAt          time.Time
	TrippedByRunID     string
	TrippedByNodeRunID string
	TrippedByAttemptID string
	TrippedByWorkID    string
	// ClearedAt/ClearedBy are set when an operator ended the pause early.
	// They survive the clear so "expired on its own" and "a human let it
	// back in" stay distinguishable afterwards.
	ClearedAt *time.Time
	ClearedBy string
	UpdatedAt time.Time
}

// Paused reports whether this pause is still in force at now.
func (p ActorPause) Paused(now time.Time) bool { return p.PausedUntil.After(now) }

// PauseActorInput is the input to Store.PauseActor. NamespaceID, ActorKey,
// PausedUntil and Reason are required; everything else is provenance the
// caller supplies when it has it.
type PauseActorInput struct {
	NamespaceID string
	ActorKey    string
	PausedUntil time.Time
	Reason      string
	// RetryAfter is the provider's Retry-After hint. Zero means it named
	// none and the column stays NULL.
	RetryAfter time.Duration
	Detail     string
	RunID      string
	NodeRunID  string
	AttemptID  string
	WorkID     string
}

const actorPauseColumns = `namespace_id, actor_key, paused_until, reason, retry_after_seconds, detail,
	tripped_at, tripped_by_run_id, tripped_by_node_run_id, tripped_by_attempt_id, tripped_by_work_id,
	cleared_at, cleared_by, updated_at`

// pauseActorSQL is the idempotent trip.
//
// The ON CONFLICT branch is a plain overwrite guarded by a WHERE on the
// deadline: a trip whose paused_until is not later than the row's current
// one changes nothing at all (PostgreSQL skips the update and the RETURNING
// clause yields no row -- which the Go wrapper turns into a read of the
// existing row, so every caller still gets the pause that is actually in
// force). cleared_at/cleared_by are reset to NULL on a genuine new trip: the
// new pause was not cleared by anyone, and leaving a stale clearer's name on
// it would misattribute a live pause.
const pauseActorSQL = `
INSERT INTO actor_availability (
	namespace_id, actor_key, paused_until, reason, retry_after_seconds, detail,
	tripped_at, tripped_by_run_id, tripped_by_node_run_id, tripped_by_attempt_id, tripped_by_work_id,
	updated_at
)
VALUES ($1, $2, $3, $4, $5, $6, now(), $7, $8, $9, $10, now())
ON CONFLICT (namespace_id, actor_key) DO UPDATE
SET paused_until           = EXCLUDED.paused_until,
    reason                 = EXCLUDED.reason,
    retry_after_seconds    = EXCLUDED.retry_after_seconds,
    detail                 = EXCLUDED.detail,
    tripped_at             = now(),
    tripped_by_run_id      = EXCLUDED.tripped_by_run_id,
    tripped_by_node_run_id = EXCLUDED.tripped_by_node_run_id,
    tripped_by_attempt_id  = EXCLUDED.tripped_by_attempt_id,
    tripped_by_work_id     = EXCLUDED.tripped_by_work_id,
    cleared_at             = NULL,
    cleared_by             = NULL,
    updated_at             = now()
WHERE EXCLUDED.paused_until > actor_availability.paused_until
RETURNING ` + actorPauseColumns

// PauseActor trips (or extends) the breaker for one actor key.
//
// It is safe to call concurrently from any number of workers: the write is a
// single upsert, and the conflict branch keeps the later deadline (see
// pauseActorSQL). The returned ActorPause is always the pause now in force,
// which may be a LONGER one another worker committed first -- a caller that
// needs to know what the actual deadline is must read the return value
// rather than assume its own input took effect.
func (s *Store) PauseActor(ctx context.Context, in PauseActorInput) (ActorPause, error) {
	switch {
	case in.NamespaceID == "":
		return ActorPause{}, fmt.Errorf("postgres: PauseActor: namespaceID is required")
	case in.ActorKey == "":
		return ActorPause{}, fmt.Errorf("postgres: PauseActor: actorKey is required")
	case in.Reason == "":
		return ActorPause{}, fmt.Errorf("postgres: PauseActor: reason is required")
	case in.PausedUntil.IsZero():
		return ActorPause{}, fmt.Errorf("postgres: PauseActor: pausedUntil is required")
	}

	pause, err := scanActorPause(s.pool.QueryRow(ctx, pauseActorSQL,
		in.NamespaceID, in.ActorKey, in.PausedUntil.UTC(), in.Reason,
		retryAfterSeconds(in.RetryAfter), textOrNull(in.Detail),
		textOrNull(in.RunID), textOrNull(in.NodeRunID), textOrNull(in.AttemptID), textOrNull(in.WorkID),
	))
	if err == nil {
		return pause, nil
	}
	if !isNoRows(err) {
		return ActorPause{}, fmt.Errorf("postgres: PauseActor: %w", err)
	}
	// No row came back: the conflict branch's WHERE refused this trip
	// because a longer pause is already in force. That is a success, not a
	// failure -- read back what IS in force and report it.
	existing, ok, readErr := s.ActorPause(ctx, in.NamespaceID, in.ActorKey)
	if readErr != nil {
		return ActorPause{}, fmt.Errorf("postgres: PauseActor: read the pause already in force: %w", readErr)
	}
	if !ok {
		// Vanishingly unlikely (nothing deletes these rows), but inventing a
		// pause here would be worse than saying so.
		return ActorPause{}, fmt.Errorf(
			"postgres: PauseActor: actor %q in namespace %s has no pause row after an upsert that changed nothing",
			in.ActorKey, in.NamespaceID)
	}
	return existing, nil
}

// retryAfterSeconds renders a Retry-After duration as the nullable column
// value: absent stays NULL, and a sub-second hint rounds up to 1 rather than
// down to 0 -- 0 would read as "retry immediately", which is not what any
// positive hint meant.
func retryAfterSeconds(d time.Duration) pgtype.Int4 {
	if d <= 0 {
		return pgtype.Int4{}
	}
	seconds := int32(d / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return pgtype.Int4{Int32: seconds, Valid: true}
}

// ActorPause returns the availability row for one actor key, whether or not
// its pause is still in force. Absence is (zero, false, nil): an actor that
// has never been paused simply has no row.
func (s *Store) ActorPause(ctx context.Context, namespaceID, actorKey string) (ActorPause, bool, error) {
	switch {
	case namespaceID == "":
		return ActorPause{}, false, fmt.Errorf("postgres: ActorPause: namespaceID is required")
	case actorKey == "":
		return ActorPause{}, false, fmt.Errorf("postgres: ActorPause: actorKey is required")
	}
	pause, err := scanActorPause(s.pool.QueryRow(ctx,
		`SELECT `+actorPauseColumns+` FROM actor_availability WHERE namespace_id = $1 AND actor_key = $2`,
		namespaceID, actorKey))
	if err != nil {
		if isNoRows(err) {
			return ActorPause{}, false, nil
		}
		return ActorPause{}, false, fmt.Errorf("postgres: ActorPause: %w", err)
	}
	return pause, true, nil
}

// ActivePause is the dispatch site's predicate: the pause for this actor
// key IF it is still in force, (zero, false, nil) otherwise.
//
// The comparison is made by PostgreSQL against its own now(), not by the
// caller against its own clock. Two workers on two machines with skewed
// clocks must agree on whether an actor is paused, and the database is the
// only clock they share.
func (s *Store) ActivePause(ctx context.Context, namespaceID, actorKey string) (ActorPause, bool, error) {
	switch {
	case namespaceID == "":
		return ActorPause{}, false, fmt.Errorf("postgres: ActivePause: namespaceID is required")
	case actorKey == "":
		return ActorPause{}, false, fmt.Errorf("postgres: ActivePause: actorKey is required")
	}
	pause, err := scanActorPause(s.pool.QueryRow(ctx,
		`SELECT `+actorPauseColumns+` FROM actor_availability
		 WHERE namespace_id = $1 AND actor_key = $2 AND paused_until > now()`,
		namespaceID, actorKey))
	if err != nil {
		if isNoRows(err) {
			return ActorPause{}, false, nil
		}
		return ActorPause{}, false, fmt.Errorf("postgres: ActivePause: %w", err)
	}
	return pause, true, nil
}

// ListActivePauses returns every actor in this namespace whose pause is
// still in force, ordered by actor key. It is the actors read surface's
// one-query join partner: an empty slice means nothing is paused.
func (s *Store) ListActivePauses(ctx context.Context, namespaceID string) ([]ActorPause, error) {
	if namespaceID == "" {
		return nil, fmt.Errorf("postgres: ListActivePauses: namespaceID is required")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+actorPauseColumns+` FROM actor_availability
		 WHERE namespace_id = $1 AND paused_until > now() ORDER BY actor_key`, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("postgres: ListActivePauses: %w", err)
	}
	defer rows.Close()

	out := make([]ActorPause, 0)
	for rows.Next() {
		pause, err := scanActorPause(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: ListActivePauses: scan: %w", err)
		}
		out = append(out, pause)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: ListActivePauses: %w", err)
	}
	return out, nil
}

// clearActorPauseSQL ends a pause early. It moves paused_until to now()
// rather than deleting the row, so the "is it paused" predicate stays one
// comparison everywhere, and stamps who did it -- an operator clearing a
// breaker is a decision worth being able to point at afterwards.
//
// The WHERE clause makes clearing an actor that is not currently paused a
// no-op rather than an error: an operator may safely repeat the call, and
// two operators racing to clear the same pause both succeed.
const clearActorPauseSQL = `
UPDATE actor_availability
SET paused_until = now(),
    cleared_at   = now(),
    cleared_by   = $3,
    updated_at   = now()
WHERE namespace_id = $1 AND actor_key = $2 AND paused_until > now()
RETURNING ` + actorPauseColumns

// ClearActorPause ends an actor's pause early, attributing the clear to
// clearedBy. It reports (zero, false, nil) when that actor was not paused --
// a no-op, not a failure.
func (s *Store) ClearActorPause(ctx context.Context, namespaceID, actorKey, clearedBy string) (ActorPause, bool, error) {
	switch {
	case namespaceID == "":
		return ActorPause{}, false, fmt.Errorf("postgres: ClearActorPause: namespaceID is required")
	case actorKey == "":
		return ActorPause{}, false, fmt.Errorf("postgres: ClearActorPause: actorKey is required")
	}
	pause, err := scanActorPause(s.pool.QueryRow(ctx, clearActorPauseSQL, namespaceID, actorKey, textOrNull(clearedBy)))
	if err != nil {
		if isNoRows(err) {
			return ActorPause{}, false, nil
		}
		return ActorPause{}, false, fmt.Errorf("postgres: ClearActorPause: %w", err)
	}
	return pause, true, nil
}

// The namespace-scoped mirror of the three methods above, for the read/write
// API surface (internal/api/actors.go). They are the same statements against
// the same table; only the namespace binding differs -- engineQueries carries
// it, *Store takes it as an argument.

// ActorPauses returns every actor in this namespace that carries an
// availability row, whether or not its pause is still in force, keyed by
// actor key.
//
// Unlike ListActivePauses it does NOT filter to live pauses, because the
// actors read surface renders both: a paused actor needs its reason and
// deadline, and an actor whose pause has lapsed or been cleared needs to say
// so rather than look like one that was never paused at all. Each row's
// Paused() answers which it is.
func (eq engineQueries) ActorPauses(ctx context.Context) (map[string]ActorPause, error) {
	rows, err := eq.q.Query(ctx,
		`SELECT `+actorPauseColumns+` FROM actor_availability WHERE namespace_id = $1 ORDER BY actor_key`,
		eq.namespaceID)
	if err != nil {
		return nil, fmt.Errorf("postgres: engine: ActorPauses: %w", err)
	}
	defer rows.Close()

	out := map[string]ActorPause{}
	for rows.Next() {
		pause, err := scanActorPause(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: engine: ActorPauses: scan: %w", err)
		}
		out[pause.ActorKey] = pause
	}
	return out, rows.Err()
}

// ActorPauseFor returns one actor key's availability row, live or historical.
func (eq engineQueries) ActorPauseFor(ctx context.Context, actorKey string) (ActorPause, bool, error) {
	if actorKey == "" {
		return ActorPause{}, false, fmt.Errorf("postgres: engine: ActorPauseFor: actorKey is required")
	}
	pause, err := scanActorPause(eq.q.QueryRow(ctx,
		`SELECT `+actorPauseColumns+` FROM actor_availability WHERE namespace_id = $1 AND actor_key = $2`,
		eq.namespaceID, actorKey))
	if err != nil {
		if isNoRows(err) {
			return ActorPause{}, false, nil
		}
		return ActorPause{}, false, fmt.Errorf("postgres: engine: ActorPauseFor %s: %w", actorKey, err)
	}
	return pause, true, nil
}

// ClearActorPause ends an actor's pause early in this namespace, reporting
// (zero, false, nil) when it was not paused -- a no-op, not a failure, so an
// operator may safely repeat the call and two operators racing to clear the
// same pause both succeed.
func (eq engineQueries) ClearActorPause(ctx context.Context, actorKey, clearedBy string) (ActorPause, bool, error) {
	if actorKey == "" {
		return ActorPause{}, false, fmt.Errorf("postgres: engine: ClearActorPause: actorKey is required")
	}
	pause, err := scanActorPause(eq.q.QueryRow(ctx, clearActorPauseSQL, eq.namespaceID, actorKey, textOrNull(clearedBy)))
	if err != nil {
		if isNoRows(err) {
			return ActorPause{}, false, nil
		}
		return ActorPause{}, false, fmt.Errorf("postgres: engine: ClearActorPause %s: %w", actorKey, err)
	}
	return pause, true, nil
}

func scanActorPause(row interface{ Scan(dest ...any) error }) (ActorPause, error) {
	var (
		p                                 ActorPause
		retryAfter                        pgtype.Int4
		detail, runID, nodeRunID          pgtype.Text
		attemptID, workID, clearedBy      pgtype.Text
		pausedUntil, trippedAt, updatedAt pgtype.Timestamptz
		clearedAt                         pgtype.Timestamptz
	)
	if err := row.Scan(
		&p.NamespaceID, &p.ActorKey, &pausedUntil, &p.Reason, &retryAfter, &detail,
		&trippedAt, &runID, &nodeRunID, &attemptID, &workID,
		&clearedAt, &clearedBy, &updatedAt,
	); err != nil {
		return ActorPause{}, err
	}
	p.PausedUntil = tsValue(pausedUntil)
	if retryAfter.Valid {
		seconds := retryAfter.Int32
		p.RetryAfterSeconds = &seconds
	}
	p.Detail = textOrEmpty(detail)
	p.TrippedAt = tsValue(trippedAt)
	p.TrippedByRunID = textOrEmpty(runID)
	p.TrippedByNodeRunID = textOrEmpty(nodeRunID)
	p.TrippedByAttemptID = textOrEmpty(attemptID)
	p.TrippedByWorkID = textOrEmpty(workID)
	p.ClearedAt = tsPtr(clearedAt)
	p.ClearedBy = textOrEmpty(clearedBy)
	p.UpdatedAt = tsValue(updatedAt)
	return p, nil
}
