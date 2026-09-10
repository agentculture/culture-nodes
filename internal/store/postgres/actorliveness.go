package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// The actor liveness row (migration 0058; plan loop-closure t10, spec
// c23/c26/c33, decision c43): the control plane's authority of record for
// whether an actor's SESSION can start — a different fact from whether its
// bridge answers /healthz, which is what the mesh collector's cache already
// says (#308: both lanes answered 200 with a spent refresh token).
//
// Three writers, and the rules between them are what this file enforces
// rather than leaving to callers:
//
//   - RecordActorLiveness (source=collector) upserts the FACT — session_ok,
//     reason, mode, checked_at — and leaves `locked` untouched. A bridge
//     observation can neither impose nor lift the control-plane lock.
//   - LockActorLiveness (source=worker) sets session_ok=false and
//     locked=true after an attempt failed with class credential_spent. This
//     is the control-plane half of c43's OR: whichever side sees the spent
//     credential first closes the lane.
//   - UnlockActorLiveness (source=resume) clears ONLY `locked`. session_ok
//     stays what was last observed, so an operator's resume alone never
//     reopens a lane whose last fact is false — the lane leases again only
//     once a later collector write reports session_ok=true (c43's AND).
//
// Like actor_availability the row is keyed by actor_key: a credential
// belongs to the identity, not to one append-only registration revision.

// LivenessFreshness is how long a session_ok=false fact keeps refusing
// leases (plan t10's "< 5 min" constant). It is declared beside the row so
// the dispatch site (internal/worker/liveness.go) and the read surface
// (internal/api/liveness.go) cannot disagree about the window.
const LivenessFreshness = 5 * time.Minute

// Liveness write sources, the CHECK-constrained `source` column's vocabulary.
const (
	LivenessSourceCollector = "collector"
	LivenessSourceWorker    = "worker"
	LivenessSourceResume    = "resume"
)

// ActorLiveness is one actor_liveness row.
type ActorLiveness struct {
	NamespaceID string
	ActorKey    string
	// SessionOK is three-valued on purpose, mirroring the bridge fact: nil
	// is "nobody measured this", which is neither true nor false.
	SessionOK *bool
	// Reason is the bridge's closed vocabulary (ok, unmeasured,
	// refresh_token_spent, credential_expired, probe_timeout, probe_failed)
	// or, for a worker write, the class-derived refresh_token_spent.
	Reason string
	// Mode is LOCK or CHECK for a bridge fact, "" for a worker write.
	Mode string
	// CheckedAt is when the fact was measured. The router's freshness
	// window (internal/worker/liveness.go) reads this.
	CheckedAt time.Time
	Source    string
	// Locked is the control-plane lock: set only by a worker write, cleared
	// only by resume. A locked row is "not live" regardless of CheckedAt.
	Locked            bool
	LockedByRunID     string
	LockedByAttemptID string
	UpdatedAt         time.Time
}

// Live is the one routing rule: a row is NOT live when it is locked (any
// age — a lock does not expire by time) or when it says session_ok=false and
// was checked inside LivenessFreshness. Everything else — unmeasured, true,
// or a stale false — is live, because refusing on a stale fact would make the
// safety net the new failure mode. A missing row is live by construction:
// there is no row to call this on.
func (l ActorLiveness) Live(now time.Time) bool {
	if l.Locked {
		return false
	}
	if l.SessionOK != nil && !*l.SessionOK && now.Sub(l.CheckedAt) < LivenessFreshness {
		return false
	}
	return true
}

// RecordActorLivenessInput is the collector's write: the bridge fact,
// verbatim. Source must be LivenessSourceCollector (the other two sources
// have their own methods with their own rules).
type RecordActorLivenessInput struct {
	NamespaceID string
	ActorKey    string
	SessionOK   *bool
	Reason      string
	Mode        string
	CheckedAt   time.Time
	Source      string
}

// LockActorLivenessInput is the worker's write after a credential_spent
// attempt. RunID/AttemptID are provenance: which dispatch discovered it.
type LockActorLivenessInput struct {
	NamespaceID string
	ActorKey    string
	Reason      string
	CheckedAt   time.Time
	RunID       string
	AttemptID   string
}

const actorLivenessColumns = `namespace_id, actor_key, session_ok, reason, mode, checked_at, source, locked,
	locked_by_run_id, locked_by_attempt_id, updated_at`

// recordActorLivenessSQL is the collector upsert. The conflict branch
// deliberately omits `locked` and the lock provenance: a bridge fact updates
// what is known, never who closed or opened the lane.
const recordActorLivenessSQL = `
INSERT INTO actor_liveness (namespace_id, actor_key, session_ok, reason, mode, checked_at, source, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, now())
ON CONFLICT (namespace_id, actor_key) DO UPDATE
SET session_ok = EXCLUDED.session_ok,
    reason     = EXCLUDED.reason,
    mode       = EXCLUDED.mode,
    checked_at = EXCLUDED.checked_at,
    source     = EXCLUDED.source,
    updated_at = now()
RETURNING ` + actorLivenessColumns

// RecordActorLiveness upserts a bridge-observed fact for one actor key.
func (s *Store) RecordActorLiveness(ctx context.Context, in RecordActorLivenessInput) (ActorLiveness, error) {
	switch {
	case in.NamespaceID == "":
		return ActorLiveness{}, fmt.Errorf("postgres: RecordActorLiveness: namespaceID is required")
	case in.ActorKey == "":
		return ActorLiveness{}, fmt.Errorf("postgres: RecordActorLiveness: actorKey is required")
	case in.Reason == "":
		return ActorLiveness{}, fmt.Errorf("postgres: RecordActorLiveness: reason is required")
	case in.CheckedAt.IsZero():
		return ActorLiveness{}, fmt.Errorf("postgres: RecordActorLiveness: checkedAt is required")
	case in.Source != LivenessSourceCollector:
		// The other sources carry rules this statement does not apply
		// (lock, unlock); refusing here keeps them from being bypassed.
		return ActorLiveness{}, fmt.Errorf("postgres: RecordActorLiveness: source %q is not %q", in.Source, LivenessSourceCollector)
	}
	row, err := scanActorLiveness(s.pool.QueryRow(ctx, recordActorLivenessSQL,
		in.NamespaceID, in.ActorKey, boolOrNull(in.SessionOK), in.Reason, in.Mode, in.CheckedAt.UTC(), in.Source))
	if err != nil {
		return ActorLiveness{}, fmt.Errorf("postgres: RecordActorLiveness: %w", err)
	}
	return row, nil
}

// lockActorLivenessSQL is the worker's write. It sets the fact AND the lock
// in one statement, and records which attempt discovered the spent
// credential. Mode is left as the bridge last reported it (or empty when no
// bridge ever did): the worker knows the outcome class, not the lane's
// configured detection mode.
const lockActorLivenessSQL = `
INSERT INTO actor_liveness (namespace_id, actor_key, session_ok, reason, mode, checked_at, source, locked,
	locked_by_run_id, locked_by_attempt_id, updated_at)
VALUES ($1, $2, FALSE, $3, '', $4, $5, TRUE, $6, $7, now())
ON CONFLICT (namespace_id, actor_key) DO UPDATE
SET session_ok           = FALSE,
    reason               = EXCLUDED.reason,
    checked_at           = EXCLUDED.checked_at,
    source               = EXCLUDED.source,
    locked               = TRUE,
    locked_by_run_id     = EXCLUDED.locked_by_run_id,
    locked_by_attempt_id = EXCLUDED.locked_by_attempt_id,
    updated_at           = now()
RETURNING ` + actorLivenessColumns

// LockActorLiveness closes a lane from the control-plane side.
func (s *Store) LockActorLiveness(ctx context.Context, in LockActorLivenessInput) (ActorLiveness, error) {
	switch {
	case in.NamespaceID == "":
		return ActorLiveness{}, fmt.Errorf("postgres: LockActorLiveness: namespaceID is required")
	case in.ActorKey == "":
		return ActorLiveness{}, fmt.Errorf("postgres: LockActorLiveness: actorKey is required")
	case in.Reason == "":
		return ActorLiveness{}, fmt.Errorf("postgres: LockActorLiveness: reason is required")
	case in.CheckedAt.IsZero():
		return ActorLiveness{}, fmt.Errorf("postgres: LockActorLiveness: checkedAt is required")
	}
	row, err := scanActorLiveness(s.pool.QueryRow(ctx, lockActorLivenessSQL,
		in.NamespaceID, in.ActorKey, in.Reason, in.CheckedAt.UTC(), LivenessSourceWorker,
		textOrNull(in.RunID), textOrNull(in.AttemptID)))
	if err != nil {
		return ActorLiveness{}, fmt.Errorf("postgres: LockActorLiveness: %w", err)
	}
	return row, nil
}

// ActorLiveness returns the row for one actor key. Absence is (zero, false,
// nil): an actor nothing has ever observed has no row.
func (s *Store) ActorLiveness(ctx context.Context, namespaceID, actorKey string) (ActorLiveness, bool, error) {
	switch {
	case namespaceID == "":
		return ActorLiveness{}, false, fmt.Errorf("postgres: ActorLiveness: namespaceID is required")
	case actorKey == "":
		return ActorLiveness{}, false, fmt.Errorf("postgres: ActorLiveness: actorKey is required")
	}
	row, err := scanActorLiveness(s.pool.QueryRow(ctx,
		`SELECT `+actorLivenessColumns+` FROM actor_liveness WHERE namespace_id = $1 AND actor_key = $2`,
		namespaceID, actorKey))
	if err != nil {
		if isNoRows(err) {
			return ActorLiveness{}, false, nil
		}
		return ActorLiveness{}, false, fmt.Errorf("postgres: ActorLiveness: %w", err)
	}
	return row, true, nil
}

// The namespace-bound mirror for the API surface (internal/api/liveness.go).

// ActorLivenessFor returns one actor key's liveness row in this namespace.
func (eq engineQueries) ActorLivenessFor(ctx context.Context, actorKey string) (ActorLiveness, bool, error) {
	if actorKey == "" {
		return ActorLiveness{}, false, fmt.Errorf("postgres: engine: ActorLivenessFor: actorKey is required")
	}
	row, err := scanActorLiveness(eq.q.QueryRow(ctx,
		`SELECT `+actorLivenessColumns+` FROM actor_liveness WHERE namespace_id = $1 AND actor_key = $2`,
		eq.namespaceID, actorKey))
	if err != nil {
		if isNoRows(err) {
			return ActorLiveness{}, false, nil
		}
		return ActorLiveness{}, false, fmt.Errorf("postgres: engine: ActorLivenessFor %s: %w", actorKey, err)
	}
	return row, true, nil
}

// ActorLivenessAll returns every liveness row in this namespace keyed by
// actor key — the one-query join partner for the actors list and the mesh
// read model.
func (eq engineQueries) ActorLivenessAll(ctx context.Context) (map[string]ActorLiveness, error) {
	rows, err := eq.q.Query(ctx,
		`SELECT `+actorLivenessColumns+` FROM actor_liveness WHERE namespace_id = $1 ORDER BY actor_key`,
		eq.namespaceID)
	if err != nil {
		return nil, fmt.Errorf("postgres: engine: ActorLivenessAll: %w", err)
	}
	defer rows.Close()
	out := map[string]ActorLiveness{}
	for rows.Next() {
		row, err := scanActorLiveness(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: engine: ActorLivenessAll: scan: %w", err)
		}
		out[row.ActorKey] = row
	}
	return out, rows.Err()
}

// unlockActorLivenessSQL is resume's write: the lock and only the lock.
// session_ok, reason, mode and checked_at are untouched — they are the last
// observed fact, and an operator clearing a lock is not a measurement.
const unlockActorLivenessSQL = `
UPDATE actor_liveness
SET locked     = FALSE,
    source     = $3,
    updated_at = now()
WHERE namespace_id = $1 AND actor_key = $2
RETURNING ` + actorLivenessColumns

// UnlockActorLiveness clears the control-plane lock for one actor key in
// this namespace, reporting (zero, false, nil) when the actor has no row —
// a no-op, not a failure, so a resume of a never-observed actor succeeds.
func (eq engineQueries) UnlockActorLiveness(ctx context.Context, actorKey string) (ActorLiveness, bool, error) {
	if actorKey == "" {
		return ActorLiveness{}, false, fmt.Errorf("postgres: engine: UnlockActorLiveness: actorKey is required")
	}
	row, err := scanActorLiveness(eq.q.QueryRow(ctx, unlockActorLivenessSQL, eq.namespaceID, actorKey, LivenessSourceResume))
	if err != nil {
		if isNoRows(err) {
			return ActorLiveness{}, false, nil
		}
		return ActorLiveness{}, false, fmt.Errorf("postgres: engine: UnlockActorLiveness %s: %w", actorKey, err)
	}
	return row, true, nil
}

func boolOrNull(b *bool) pgtype.Bool {
	if b == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *b, Valid: true}
}

func scanActorLiveness(row interface{ Scan(dest ...any) error }) (ActorLiveness, error) {
	var (
		l                    ActorLiveness
		sessionOK            pgtype.Bool
		runID, attemptID     pgtype.Text
		checkedAt, updatedAt pgtype.Timestamptz
	)
	if err := row.Scan(
		&l.NamespaceID, &l.ActorKey, &sessionOK, &l.Reason, &l.Mode, &checkedAt, &l.Source, &l.Locked,
		&runID, &attemptID, &updatedAt,
	); err != nil {
		return ActorLiveness{}, err
	}
	if sessionOK.Valid {
		value := sessionOK.Bool
		l.SessionOK = &value
	}
	l.CheckedAt = tsValue(checkedAt)
	l.LockedByRunID = textOrEmpty(runID)
	l.LockedByAttemptID = textOrEmpty(attemptID)
	l.UpdatedAt = tsValue(updatedAt)
	return l, nil
}
