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
	var revokedAt, lockedUntil, windowStart pgtype.Timestamptz
	var failures, attempts int32
	err = tx.QueryRow(ctx, `
		SELECT verifier_sha256, verifier_env_name, revoked_at,
		       failure_count, locked_until, rate_window_started_at, rate_attempt_count
		FROM inbound_authentication
		WHERE party_kind = $1 AND party_key = $2
		FOR UPDATE`, partyKind, partyKey).Scan(
		&digest, &envName, &revokedAt, &failures, &lockedUntil, &windowStart, &attempts,
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
	state := actors.InboundAuthenticationState{
		Verifier: verifier, FailureCount: int(failures), RateAttemptCount: int(attempts),
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
