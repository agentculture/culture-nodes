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
type InboundAuthenticationConfig struct {
	RateLimit       int
	RateWindow      time.Duration
	FailureLimit    int
	LockoutDuration time.Duration
}

// DefaultInboundAuthenticationConfig is deliberately conservative for a
// dial-in endpoint. Callers may override it, and tests use small independent
// limits to prove neither control is standing in for the other.
var DefaultInboundAuthenticationConfig = InboundAuthenticationConfig{
	RateLimit: 10, RateWindow: time.Minute,
	FailureLimit: 5, LockoutDuration: 15 * time.Minute,
}

// InboundAuthenticationState is the complete non-secret state used by one
// admission decision. It must never grow a field containing a presentation.
type InboundAuthenticationState struct {
	Verifier         *InboundCredentialVerifier
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
