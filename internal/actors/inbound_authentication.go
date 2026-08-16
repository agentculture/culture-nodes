package actors

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// InboundAuthenticationConfig keeps admission pacing and failure lockout
// separate. RateLimit bounds every presentation in RateWindow; FailureLimit
// locks a credential after that many consecutive bad presentations.
//
// RequireControlPlaneIssued is the #111 posture switch: with it set, only a
// credential this control plane MINTED (see MintInboundCredential) admits a
// dial, and the operator-provisioned record migration 0031 called temporary
// — a hand-inserted digest, or a verifier_env_name pointing at an
// operator-invented PREFIX_DIAL_TOKEN — is refused as InboundNotIssued.
type InboundAuthenticationConfig struct {
	RateLimit                 int
	RateWindow                time.Duration
	FailureLimit              int
	LockoutDuration           time.Duration
	RequireControlPlaneIssued bool
}

// DefaultInboundAuthenticationConfig is deliberately conservative for a
// dial-in endpoint. Callers may override it, and tests use small independent
// limits to prove neither control is standing in for the other.
//
// RequireControlPlaneIssued is true here because the decision recorded for
// issue #136 is that a bridge's identity IS a control-plane-issued token: a
// deployment that wants the older operator-provisioned record must ask for
// it explicitly, rather than getting it by having configured nothing.
var DefaultInboundAuthenticationConfig = InboundAuthenticationConfig{
	RateLimit: 10, RateWindow: time.Minute,
	FailureLimit: 5, LockoutDuration: 15 * time.Minute,
	RequireControlPlaneIssued: true,
}

// InboundAuthenticationState is the complete non-secret state used by one
// admission decision. It must never grow a field containing a presentation.
type InboundAuthenticationState struct {
	Verifier *InboundCredentialVerifier
	// Issued records whether this verifier was minted by the control plane
	// (inbound_authentication.issued_at is not null, migration 0037) rather
	// than provisioned by hand. It is provenance, not material.
	Issued           bool
	RevokedAt        *time.Time
	FailureCount     int
	LockedUntil      *time.Time
	RateWindowStart  *time.Time
	RateAttemptCount int
}

// InboundAuthenticationReason names why a dial was refused.
type InboundAuthenticationReason string

const (
	InboundAuthenticated InboundAuthenticationReason = "authenticated"
	InboundInvalid       InboundAuthenticationReason = "invalid_credential"
	InboundLocked        InboundAuthenticationReason = "locked_out"
	InboundRateLimited   InboundAuthenticationReason = "rate_limited"
	InboundRevoked       InboundAuthenticationReason = "revoked"
	// InboundNotIssued refuses a credential this control plane did not mint
	// (issue #111): the record exists, but its authority came from an
	// operator rather than from issuance.
	InboundNotIssued InboundAuthenticationReason = "not_control_plane_issued"
)

// InboundAuthenticationDecision is safe to return to the dial-in path. It
// contains no credential material and Allowed is true only for an admitted
// connection.
type InboundAuthenticationDecision struct {
	Allowed bool
	Reason  InboundAuthenticationReason
	RetryAt time.Time
}

// InboundAuthenticationStateStore serializes one credential's read, decision
// and state write. PostgreSQL implements this with SELECT ... FOR UPDATE;
// in-memory tests implement it without a database or socket.
type InboundAuthenticationStateStore interface {
	UpdateInboundAuthentication(context.Context, string, string, func(InboundAuthenticationState) (InboundAuthenticationState, InboundAuthenticationDecision)) (InboundAuthenticationDecision, error)
}

// InboundAuthenticator is the gate a dial-in path must call before accepting
// a connection. Time is injected so windows, lockout expiry and scheduled
// revocation are deterministic inputs rather than sleeps.
type InboundAuthenticator struct {
	store  InboundAuthenticationStateStore
	config InboundAuthenticationConfig
	now    func() time.Time
}

func NewInboundAuthenticator(store InboundAuthenticationStateStore, config InboundAuthenticationConfig, now func() time.Time) (*InboundAuthenticator, error) {
	if store == nil {
		return nil, errors.New("actors: inbound authentication store is required")
	}
	if config.RateLimit <= 0 || config.RateWindow <= 0 || config.FailureLimit <= 0 || config.LockoutDuration <= 0 {
		return nil, errors.New("actors: inbound authentication limits and durations must be positive")
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &InboundAuthenticator{store: store, config: config, now: now}, nil
}

// Authenticate performs revocation, lockout, rate and constant-time
// credential checks, in that order, under the store's per-credential lock.
func (a *InboundAuthenticator) Authenticate(ctx context.Context, partyKind, partyKey, presented string) (InboundAuthenticationDecision, error) {
	if partyKind == "" || partyKey == "" {
		return InboundAuthenticationDecision{}, errors.New("actors: inbound party kind and key are required")
	}
	now := a.now().UTC()
	return a.store.UpdateInboundAuthentication(ctx, partyKind, partyKey, func(state InboundAuthenticationState) (InboundAuthenticationState, InboundAuthenticationDecision) {
		return decideInboundAuthentication(state, presented, now, a.config)
	})
}

func decideInboundAuthentication(state InboundAuthenticationState, presented string, now time.Time, config InboundAuthenticationConfig) (InboundAuthenticationState, InboundAuthenticationDecision) {
	if state.RevokedAt != nil && !now.Before(*state.RevokedAt) {
		return state, InboundAuthenticationDecision{Reason: InboundRevoked}
	}
	// Provenance is a property of the RECORD, not of the presentation, so it
	// is answered here — before lockout, before the rate window and without
	// consuming either. The answer cannot vary with what was presented, so
	// refusing early discloses nothing an attacker could probe with.
	if config.RequireControlPlaneIssued && !state.Issued {
		return state, InboundAuthenticationDecision{Reason: InboundNotIssued}
	}
	if state.LockedUntil != nil && now.Before(*state.LockedUntil) {
		return state, InboundAuthenticationDecision{Reason: InboundLocked, RetryAt: *state.LockedUntil}
	}
	if state.LockedUntil != nil {
		state.LockedUntil = nil
		state.FailureCount = 0
	}
	if state.RateWindowStart == nil || !now.Before(state.RateWindowStart.Add(config.RateWindow)) {
		windowStart := now
		state.RateWindowStart = &windowStart
		state.RateAttemptCount = 0
	}
	if state.RateAttemptCount >= config.RateLimit {
		return state, InboundAuthenticationDecision{Reason: InboundRateLimited, RetryAt: state.RateWindowStart.Add(config.RateWindow)}
	}
	state.RateAttemptCount++
	if state.Verifier == nil || !state.Verifier.Verify(presented) {
		state.FailureCount++
		if state.FailureCount >= config.FailureLimit {
			until := now.Add(config.LockoutDuration)
			state.LockedUntil = &until
		}
		return state, InboundAuthenticationDecision{Reason: InboundInvalid}
	}
	state.FailureCount = 0
	state.LockedUntil = nil
	return state, InboundAuthenticationDecision{Allowed: true, Reason: InboundAuthenticated}
}

func (d InboundAuthenticationDecision) String() string {
	return fmt.Sprintf("allowed=%t reason=%s", d.Allowed, d.Reason)
}
