package postgres_test

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/store"
)

func TestInboundAuthenticationPersistsLockoutAndRevocation(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	key := "test/worker-" + store.NewULID()
	digest := sha256.Sum256([]byte("right"))
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO inbound_authentication (party_kind, party_key, verifier_sha256)
		VALUES ('actor', $1, $2)`, key, digest[:]); err != nil {
		t.Fatal(err)
	}
	defer s.Pool().Exec(ctx, `DELETE FROM inbound_authentication WHERE party_kind='actor' AND party_key=$1`, key)

	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	config := actors.InboundAuthenticationConfig{RateLimit: 100, RateWindow: time.Minute, FailureLimit: 2, LockoutDuration: time.Minute}
	auth, err := actors.NewInboundAuthenticator(s, config, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < config.FailureLimit; i++ {
		if decision, err := auth.Authenticate(ctx, "actor", key, "wrong"); err != nil || decision.Allowed {
			t.Fatalf("wrong credential %d: decision=%v err=%v", i+1, decision, err)
		}
	}
	if decision, err := auth.Authenticate(ctx, "actor", key, "right"); err != nil || decision.Reason != actors.InboundLocked {
		t.Fatalf("persisted lockout: decision=%v err=%v", decision, err)
	}
	now = now.Add(config.LockoutDuration)
	if decision, err := auth.Authenticate(ctx, "actor", key, "right"); err != nil || !decision.Allowed {
		t.Fatalf("after lockout: decision=%v err=%v", decision, err)
	}
	if _, err := s.Pool().Exec(ctx, `UPDATE inbound_authentication SET revoked_at=$2 WHERE party_kind='actor' AND party_key=$1`, key, now); err != nil {
		t.Fatal(err)
	}
	if decision, err := auth.Authenticate(ctx, "actor", key, "right"); err != nil || decision.Reason != actors.InboundRevoked {
		t.Fatalf("revoked next dial: decision=%v err=%v", decision, err)
	}
}
