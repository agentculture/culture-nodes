package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/actors"
)

// UpdateInboundAuthentication serializes one complete admission decision.
// The callback receives verifier material only in memory; the presented value
// is never passed into this store and therefore cannot enter SQL arguments.
func (s *Store) UpdateInboundAuthentication(ctx context.Context, partyKind, partyKey string, update func(actors.InboundAuthenticationState) (actors.InboundAuthenticationState, actors.InboundAuthenticationDecision)) (actors.InboundAuthenticationDecision, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return actors.InboundAuthenticationDecision{}, fmt.Errorf("postgres: begin inbound authentication: %w", err)
	}
	defer tx.Rollback(ctx) // no-op after commit

	var digest []byte
	var envName pgtype.Text
	var revokedAt, lockedUntil, windowStart, issuedAt pgtype.Timestamptz
	var failures, attempts int32
	err = tx.QueryRow(ctx, `
		SELECT verifier_sha256, verifier_env_name, revoked_at,
		       failure_count, locked_until, rate_window_started_at, rate_attempt_count,
		       issued_at
		FROM inbound_authentication
		WHERE party_kind = $1 AND party_key = $2
		FOR UPDATE`, partyKind, partyKey).Scan(
		&digest, &envName, &revokedAt, &failures, &lockedUntil, &windowStart, &attempts,
		&issuedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return actors.InboundAuthenticationDecision{}, fmt.Errorf("postgres: inbound authentication record not found for %s %s", partyKind, partyKey)
		}
		return actors.InboundAuthenticationDecision{}, fmt.Errorf("postgres: load inbound authentication: %w", err)
	}
	verifier, err := actors.NewInboundCredentialVerifier(digest, envName.String, nil)
	if err != nil {
		return actors.InboundAuthenticationDecision{}, fmt.Errorf("postgres: invalid inbound authentication verifier: %w", err)
	}
	// issued_at (migration 0037) is the record's provenance: non-null means
	// this control plane minted the credential. It is carried into the
	// decision so an operator-provisioned record can be refused as
	// `not_control_plane_issued` (issue #111) — never as a bad credential,
	// which would be a different and misleading fact.
	state := actors.InboundAuthenticationState{
		Verifier: verifier, Issued: issuedAt.Valid,
		FailureCount: int(failures), RateAttemptCount: int(attempts),
	}
	if revokedAt.Valid {
		at := revokedAt.Time
		state.RevokedAt = &at
	}
	if lockedUntil.Valid {
		at := lockedUntil.Time
		state.LockedUntil = &at
	}
	if windowStart.Valid {
		at := windowStart.Time
		state.RateWindowStart = &at
	}

	next, decision := update(state)
	_, err = tx.Exec(ctx, `
		UPDATE inbound_authentication
		SET failure_count = $3, locked_until = $4,
		    rate_window_started_at = $5, rate_attempt_count = $6,
		    updated_at = now()
		WHERE party_kind = $1 AND party_key = $2`,
		partyKind, partyKey, next.FailureCount, next.LockedUntil,
		next.RateWindowStart, next.RateAttemptCount,
	)
	if err != nil {
		return actors.InboundAuthenticationDecision{}, fmt.Errorf("postgres: update inbound authentication state: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return actors.InboundAuthenticationDecision{}, fmt.Errorf("postgres: commit inbound authentication: %w", err)
	}
	return decision, nil
}

// InboundCredentialRecord is one credential row's identity and its stored
// one-way verifier. It carries no admission state and no presented value:
// it exists so a caller holding a presentation can find out WHICH party a
// digest belongs to, and then ask UpdateInboundAuthentication — the only
// surface that decides admission — about that party.
type InboundCredentialRecord struct {
	PartyKind string
	PartyKey  string
	Digest    []byte
}

// ListInboundCredentialDigests returns every digest-backed credential row of
// one party kind, newest-updated first.
//
// The dial-in transport never needs this: a bridge names its own party in
// the X-Culture-Nodes-Actor-Key header, so the row is looked up by key. The
// break-glass principal (login-from-anywhere task t22) has no such header —
// an operator on the LAN presents a bearer and nothing else — so the party
// is resolved FROM the credential, the same way an agent actor's own bearer
// is resolved by walking the actor rows (internal/api/actorbearer.go).
//
// Rows are returned regardless of revocation, lockout or issuance
// provenance. Filtering them here would turn a revoked credential into an
// unknown one, and "no principal" is a materially different (and, for an
// operator staring at a locked-out site, a misleading) fact from "this
// credential was revoked". The admission decision stays where it already
// lives.
//
// Only the digest leaves this function — the same value migration 0031 says
// is the only credential-shaped thing the database may hold.
func (s *Store) ListInboundCredentialDigests(ctx context.Context, partyKind string) ([]InboundCredentialRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT party_kind, party_key, verifier_sha256
		FROM inbound_authentication
		WHERE party_kind = $1 AND verifier_sha256 IS NOT NULL
		ORDER BY updated_at DESC`, partyKind)
	if err != nil {
		return nil, fmt.Errorf("postgres: list inbound credential digests: %w", err)
	}
	defer rows.Close()

	out := make([]InboundCredentialRecord, 0)
	for rows.Next() {
		var record InboundCredentialRecord
		if err := rows.Scan(&record.PartyKind, &record.PartyKey, &record.Digest); err != nil {
			return nil, fmt.Errorf("postgres: list inbound credential digests: scan: %w", err)
		}
		out = append(out, record)
	}
	return out, rows.Err()
}
