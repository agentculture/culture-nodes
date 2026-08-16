package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// DialInPresenceRow is one registered actor's current dial-in state, as the
// read-only operator view renders it (task t6, issues #136/#121).
//
// The spine of the query is the ACTORS table, not the presence table, and
// that is the whole point: a view built from presence rows can only show
// bridges that have dialled in at least once, which would silently omit
// exactly the actors an operator is looking for. In practice today one actor
// (`company/developer`) has ever dialled in and ten have not, so the absent
// case is the common case, not a degenerate one.
type DialInPresenceRow struct {
	// ActorKey is the identity. Presence is keyed by actor_key, never by an
	// actors-table row id: registration is append-only, so one identity has
	// many revision rows and exactly one connection.
	ActorKey string
	// ActorID/Revision/Kind describe the CURRENT (highest) registration
	// revision of that identity, so the view can be joined back to
	// GET /v1alpha1/actors/{id} without a second lookup.
	ActorID  string
	Revision int32
	Kind     string

	// LastSeenAt is nil when no inbound_actor_presence row exists — the
	// actor has never dialled in. Non-nil with Connected false is the other
	// case entirely: it dialled in before and stopped.
	LastSeenAt *time.Time
	// Connected is evaluated in SQL with the same `last_seen_at >= $2`
	// predicate InboundActorAvailable uses at dispatch-resolution time, so
	// this view cannot report a different answer than dispatch would.
	Connected bool

	// Credential is the inbound_authentication record for this actor key,
	// or nil when there is none at all (the actor could not dial in even if
	// its process were running). An operator at 03:00 needs to tell "absent"
	// from "revoked" from "locked out": those look identical in presence
	// alone and have completely different remedies.
	Credential *DialInCredentialRow
}

// DialInCredentialRow is the admission-control half of the view: migration
// 0031's verifier record, 0032's revocation/lockout/failure state, and
// 0037's issuance provenance. It carries no verifier material — not the
// digest, not the environment variable name — because this is an operator
// read surface and neither is needed to answer "why is this bridge not
// dialling in".
type DialInCredentialRow struct {
	// Issued is inbound_authentication.issued_at IS NOT NULL: this control
	// plane minted the credential (issue #111). False means the row was
	// provisioned by hand, which the shipped admission default refuses as
	// `not_control_plane_issued` — an actor in that state will never dial in
	// successfully no matter how healthy its process is.
	Issued        bool
	IssuedAt      *time.Time
	IssuanceCount int
	// RevokedAt is positive durable evidence of revocation (migration 0032);
	// a revoked credential's bridge looks exactly as absent as a crashed one.
	RevokedAt *time.Time
	// LockedUntil / FailureCount are the lockout counters. A locked-out
	// bridge is dialling and being refused — the opposite operational
	// situation from one that is not dialling at all.
	LockedUntil  *time.Time
	FailureCount int
}

// DialInPresence returns every registered actor identity in the namespace
// with its current dial-in state, evaluated against `since` (which callers
// obtain from actors.DialInPresenceCutoff so there is exactly one definition
// of "connected" in the system).
//
// It is strictly read-only: no INSERT, no UPDATE, and in particular it never
// touches inbound_actor_presence. Reading presence must not create it —
// a view that refreshed last_seen_at would report every actor connected the
// moment an operator looked, which is the observability equivalent of a
// thermometer that heats the room.
func (s *Store) DialInPresence(ctx context.Context, namespaceID string, since time.Time) ([]DialInPresenceRow, error) {
	rows, err := s.pool.Query(ctx, `
		WITH current_actors AS (
			SELECT DISTINCT ON (actor_key) actor_key, id, revision, kind
			FROM actors
			WHERE namespace_id = $1
			ORDER BY actor_key, revision DESC
		)
		SELECT a.actor_key, a.id, a.revision, a.kind,
		       p.last_seen_at,
		       COALESCE(p.last_seen_at >= $2, false) AS connected,
		       c.party_key IS NOT NULL AS has_credential,
		       c.issued_at, c.issuance_count, c.revoked_at,
		       c.locked_until, c.failure_count
		FROM current_actors a
		LEFT JOIN inbound_actor_presence p
		       ON p.namespace_id = $1 AND p.actor_key = a.actor_key
		LEFT JOIN inbound_authentication c
		       ON c.party_kind = 'actor' AND c.party_key = a.actor_key
		ORDER BY a.actor_key`, namespaceID, since)
	if err != nil {
		return nil, fmt.Errorf("postgres: dial-in presence: %w", err)
	}
	defer rows.Close()

	out := []DialInPresenceRow{}
	for rows.Next() {
		var (
			row                         DialInPresenceRow
			lastSeen                    pgtype.Timestamptz
			issuedAt                    pgtype.Timestamptz
			revokedAt                   pgtype.Timestamptz
			lockedUntil                 pgtype.Timestamptz
			hasCredential               bool
			issuanceCount, failureCount pgtype.Int4
		)
		if err := rows.Scan(&row.ActorKey, &row.ActorID, &row.Revision, &row.Kind,
			&lastSeen, &row.Connected,
			&hasCredential, &issuedAt, &issuanceCount, &revokedAt,
			&lockedUntil, &failureCount); err != nil {
			return nil, fmt.Errorf("postgres: scan dial-in presence: %w", err)
		}
		row.LastSeenAt = utcOrNil(lastSeen)
		if hasCredential {
			row.Credential = &DialInCredentialRow{
				Issued:        issuedAt.Valid,
				IssuedAt:      utcOrNil(issuedAt),
				IssuanceCount: int(issuanceCount.Int32),
				RevokedAt:     utcOrNil(revokedAt),
				LockedUntil:   utcOrNil(lockedUntil),
				FailureCount:  int(failureCount.Int32),
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: read dial-in presence: %w", err)
	}
	return out, nil
}

// utcOrNil renders a nullable timestamp as a *time.Time in UTC, so an absent
// instant stays structurally absent rather than becoming the zero time — the
// same "no data is not a fabricated value" rule ActorDurationPercentiles
// follows.
func utcOrNil(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	at := ts.Time.UTC()
	return &at
}
