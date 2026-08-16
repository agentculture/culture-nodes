package actors_test

import (
	"context"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
)

type memoryInboundState struct {
	mu    sync.Mutex
	state actors.InboundAuthenticationState
}

func (s *memoryInboundState) UpdateInboundAuthentication(_ context.Context, _, _ string, update func(actors.InboundAuthenticationState) (actors.InboundAuthenticationState, actors.InboundAuthenticationDecision)) (actors.InboundAuthenticationDecision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, decision := update(s.state)
	s.state = next
	return decision, nil
}

func inboundFixture(t *testing.T, config actors.InboundAuthenticationConfig) (*actors.InboundAuthenticator, *memoryInboundState, *time.Time) {
	t.Helper()
	digest := sha256.Sum256([]byte("right"))
	verifier, err := actors.NewInboundCredentialVerifier(digest[:], "", nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	state := &memoryInboundState{state: actors.InboundAuthenticationState{Verifier: verifier}}
	auth, err := actors.NewInboundAuthenticator(state, config, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return auth, state, &now
}

func TestRepeatedWrongCredentialLocksOutWorker(t *testing.T) {
	config := actors.InboundAuthenticationConfig{RateLimit: 100, RateWindow: time.Minute, FailureLimit: 3, LockoutDuration: 10 * time.Minute}
	auth, _, now := inboundFixture(t, config)
	for i := 0; i < config.FailureLimit; i++ {
		decision, err := auth.Authenticate(context.Background(), "actor", "company/worker", "wrong")
		if err != nil || decision.Allowed || decision.Reason != actors.InboundInvalid {
			t.Fatalf("failure %d: decision=%v err=%v", i+1, decision, err)
		}
	}
	decision, err := auth.Authenticate(context.Background(), "actor", "company/worker", "right")
	if err != nil || decision.Allowed || decision.Reason != actors.InboundLocked {
		t.Fatalf("correct credential during lockout: decision=%v err=%v", decision, err)
	}
	*now = now.Add(config.LockoutDuration)
	decision, err = auth.Authenticate(context.Background(), "actor", "company/worker", "right")
	if err != nil || !decision.Allowed {
		t.Fatalf("after lockout expiry: decision=%v err=%v", decision, err)
	}
}

func TestRevokedActorCredentialStopsOnNextDial(t *testing.T) {
	config := actors.InboundAuthenticationConfig{RateLimit: 10, RateWindow: time.Minute, FailureLimit: 3, LockoutDuration: time.Minute}
	auth, state, now := inboundFixture(t, config)
	decision, err := auth.Authenticate(context.Background(), "actor", "company/worker", "right")
	if err != nil || !decision.Allowed {
		t.Fatalf("before revocation: decision=%v err=%v", decision, err)
	}
	revokedAt := *now
	state.mu.Lock()
	state.state.RevokedAt = &revokedAt
	state.mu.Unlock()
	decision, err = auth.Authenticate(context.Background(), "actor", "company/worker", "right")
	if err != nil || decision.Allowed || decision.Reason != actors.InboundRevoked {
		t.Fatalf("next dial after revocation: decision=%v err=%v", decision, err)
	}
}

func TestRateLimitIsIndependentOfLockout(t *testing.T) {
	config := actors.InboundAuthenticationConfig{RateLimit: 2, RateWindow: time.Minute, FailureLimit: 100, LockoutDuration: time.Hour}
	auth, _, now := inboundFixture(t, config)
	for i := 0; i < 2; i++ {
		decision, err := auth.Authenticate(context.Background(), "actor", "company/worker", "right")
		if err != nil || !decision.Allowed {
			t.Fatalf("attempt %d: decision=%v err=%v", i+1, decision, err)
		}
	}
	decision, _ := auth.Authenticate(context.Background(), "actor", "company/worker", "right")
	if decision.Reason != actors.InboundRateLimited {
		t.Fatalf("decision=%v", decision)
	}
	*now = now.Add(config.RateWindow)
	decision, _ = auth.Authenticate(context.Background(), "actor", "company/worker", "right")
	if !decision.Allowed {
		t.Fatalf("new rate window: decision=%v", decision)
	}
}
