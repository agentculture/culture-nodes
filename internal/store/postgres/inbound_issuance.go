package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentculture/culture-nodes/internal/actors"
)

// IssueInboundCredential persists one minted dial-in credential (issue #111,
// migration 0037) and returns the instant and issuance count the database
// recorded.
//
// It takes an actors.IssuedInboundCredential, which has no field a plaintext
// could occupy — there is no signature here through which a presented or
// caller-supplied value could reach SQL, which is what makes "the plaintext
// never reaches the database" structural rather than a rule this function
// has to keep.
//
// Issuing again for the same party REPLACES the verifier and clears the
// admission state: a new secret has no failure history, and a party whose
// credential was revoked is deliberately re-admitted, because minting a new
// secret is exactly the act of granting a new one. The revoked plaintext is
// unrecoverable — only its digest was ever stored, and this overwrites it.
func (s *Store) IssueInboundCredential(ctx context.Context, issued actors.IssuedInboundCredential) (time.Time, error) {
	if err := actors.ValidateInboundParty(issued.PartyKind, issued.PartyKey); err != nil {
		return time.Time{}, err
	}
	if len(issued.Digest) != 32 {
		return time.Time{}, errors.New("postgres: an issued inbound credential must carry a 32-byte SHA-256 verifier")
	}
	var issuedAt time.Time
	err := s.pool.QueryRow(ctx, `
		INSERT INTO inbound_authentication
			(party_kind, party_key, verifier_sha256, issued_at, issuance_count)
		VALUES ($1, $2, $3, now(), 1)
		ON CONFLICT (party_kind, party_key) DO UPDATE SET
			verifier_sha256        = EXCLUDED.verifier_sha256,
			verifier_env_name      = NULL,
			issued_at              = now(),
			issuance_count         = inbound_authentication.issuance_count + 1,
			revoked_at             = NULL,
			failure_count          = 0,
			locked_until           = NULL,
			rate_window_started_at = NULL,
			rate_attempt_count     = 0,
			updated_at             = now()
		RETURNING issued_at`,
		issued.PartyKind, issued.PartyKey, issued.Digest).Scan(&issuedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("postgres: issue inbound credential: %w", err)
	}
	return issuedAt, nil
}

// RevokeInboundCredential records that one party's dial-in credential is no
// longer authority. It is scoped to exactly one (party_kind, party_key) row,
// so every other bridge keeps dialling and nothing is restarted: the
// authenticator reads this state on every dial rather than caching it.
//
// Revocation is durable evidence rather than a deletion — deletion is not
// revocation, because a restore or replay could recreate a usable verifier
// (migration 0032's own reasoning). Re-revoking keeps the ORIGINAL instant:
// the fact being recorded is when authority ended, and that does not move.
func (s *Store) RevokeInboundCredential(ctx context.Context, partyKind, partyKey string) (time.Time, error) {
	var revokedAt time.Time
	err := s.pool.QueryRow(ctx, `
		UPDATE inbound_authentication
		SET revoked_at = COALESCE(revoked_at, now()), updated_at = now()
		WHERE party_kind = $1 AND party_key = $2
		RETURNING revoked_at`, partyKind, partyKey).Scan(&revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, fmt.Errorf("postgres: no inbound credential for %s %s: %w", partyKind, partyKey, ErrNotFound)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("postgres: revoke inbound credential: %w", err)
	}
	return revokedAt, nil
}
