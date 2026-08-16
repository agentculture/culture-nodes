package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// issuanceConfig is the admission policy every test in this file dials
// under: control-plane issuance required (the #111 replacement's posture),
// with small independent limits so neither control can stand in for the
// other.
var issuanceConfig = actors.InboundAuthenticationConfig{
	RateLimit: 20, RateWindow: time.Minute,
	FailureLimit: 2, LockoutDuration: time.Minute,
	RequireControlPlaneIssued: true,
}

// issueCredential mints through the control plane, persists the verifier and
// returns the revealed plaintext — the only place any test in this file can
// learn a credential's value, which is the point of the design.
func issueCredential(t *testing.T, s *postgres.Store, partyKey string) string {
	t.Helper()
	ctx := context.Background()
	secret, issued, err := actors.MintInboundCredential("actor", partyKey)
	if err != nil {
		t.Fatalf("MintInboundCredential: %v", err)
	}
	if _, err := s.IssueInboundCredential(ctx, issued); err != nil {
		t.Fatalf("IssueInboundCredential: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.Pool().Exec(context.Background(),
			`DELETE FROM inbound_authentication WHERE party_kind='actor' AND party_key=$1`, partyKey)
	})
	plaintext, err := secret.Reveal()
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	return plaintext
}

func inboundRowText(t *testing.T, s *postgres.Store, partyKey string) string {
	t.Helper()
	var text string
	err := s.Pool().QueryRow(context.Background(),
		`SELECT to_jsonb(t)::text FROM inbound_authentication t
		 WHERE party_kind='actor' AND party_key=$1`, partyKey).Scan(&text)
	if err != nil {
		t.Fatalf("read inbound_authentication row: %v", err)
	}
	return text
}

// TestIssueInboundCredentialPersistsOnlyTheDigest covers acceptance criteria
// 1 and 2 at the durable boundary: the row carries the SHA-256 verifier and
// its issuance provenance, the plaintext appears nowhere in the table, and
// the issued value admits a dial while a neighbouring actor's does not.
func TestIssueInboundCredentialPersistsOnlyTheDigest(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	key := "test/issued-" + store.NewULID()
	plaintext := issueCredential(t, s, key)

	var digest []byte
	var envName *string
	var issuedAt *time.Time
	var issuanceCount int
	if err := s.Pool().QueryRow(ctx, `
		SELECT verifier_sha256, verifier_env_name, issued_at, issuance_count
		FROM inbound_authentication WHERE party_kind='actor' AND party_key=$1`, key).
		Scan(&digest, &envName, &issuedAt, &issuanceCount); err != nil {
		t.Fatalf("read issued credential: %v", err)
	}
	want := sha256.Sum256([]byte(plaintext))
	if hex.EncodeToString(digest) != hex.EncodeToString(want[:]) {
		t.Error("stored verifier is not SHA-256 of the issued credential")
	}
	if envName != nil {
		t.Errorf("issuance left an environment reference behind: %q", *envName)
	}
	if issuedAt == nil || issuanceCount != 1 {
		t.Errorf("issuance provenance = %v / %d, want a timestamp and 1", issuedAt, issuanceCount)
	}

	var table string
	if err := s.Pool().QueryRow(ctx,
		`SELECT coalesce(string_agg(to_jsonb(t)::text, ' '), '') FROM inbound_authentication t`).Scan(&table); err != nil {
		t.Fatalf("dump inbound_authentication: %v", err)
	}
	if strings.Contains(table, plaintext) {
		t.Fatal("the issued credential reached the database in presentable form")
	}

	auth, err := actors.NewInboundAuthenticator(s, issuanceConfig, nil)
	if err != nil {
		t.Fatalf("NewInboundAuthenticator: %v", err)
	}
	if decision, err := auth.Authenticate(ctx, "actor", key, plaintext); err != nil || !decision.Allowed {
		t.Fatalf("issued credential was refused: %+v err=%v", decision, err)
	}
	if decision, err := auth.Authenticate(ctx, "actor", key, plaintext+"x"); err != nil || decision.Allowed {
		t.Fatalf("a near-miss presentation was admitted: %+v err=%v", decision, err)
	}
}

// TestOperatorProvisionedCredentialIsRefusedAsUnissued is acceptance
// criterion 1 against the durable record: a verifier inserted by hand — the
// operator-invented PREFIX_DIAL_TOKEN shape migration 0031 called temporary
// — no longer admits a dial.
func TestOperatorProvisionedCredentialIsRefusedAsUnissued(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	key := "test/handmade-" + store.NewULID()
	digest := sha256.Sum256([]byte("operator-invented-token"))
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO inbound_authentication (party_kind, party_key, verifier_sha256)
		VALUES ('actor', $1, $2)`, key, digest[:]); err != nil {
		t.Fatalf("insert hand-provisioned credential: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.Pool().Exec(context.Background(),
			`DELETE FROM inbound_authentication WHERE party_kind='actor' AND party_key=$1`, key)
	})

	auth, err := actors.NewInboundAuthenticator(s, issuanceConfig, nil)
	if err != nil {
		t.Fatalf("NewInboundAuthenticator: %v", err)
	}
	decision, err := auth.Authenticate(ctx, "actor", key, "operator-invented-token")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if decision.Allowed || decision.Reason != actors.InboundNotIssued {
		t.Fatalf("hand-provisioned credential decision = %+v", decision)
	}
}

// TestRevokeInboundCredentialUnDialsOnlyThatParty is acceptance criterion 3:
// one bridge's credential is revoked through the same live authenticator
// every other bridge is dialling through — no restart, no reconstruction,
// and no effect on the others.
func TestRevokeInboundCredentialUnDialsOnlyThatParty(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	revokedKey := "test/revoked-" + store.NewULID()
	survivingKey := "test/surviving-" + store.NewULID()
	revokedSecret := issueCredential(t, s, revokedKey)
	survivingSecret := issueCredential(t, s, survivingKey)

	auth, err := actors.NewInboundAuthenticator(s, issuanceConfig, nil)
	if err != nil {
		t.Fatalf("NewInboundAuthenticator: %v", err)
	}
	for _, party := range []struct {
		key    string
		secret string
	}{{revokedKey, revokedSecret}, {survivingKey, survivingSecret}} {
		if decision, err := auth.Authenticate(ctx, "actor", party.key, party.secret); err != nil || !decision.Allowed {
			t.Fatalf("pre-revocation dial for %s: %+v err=%v", party.key, decision, err)
		}
	}

	revokedAt, err := s.RevokeInboundCredential(ctx, "actor", revokedKey)
	if err != nil {
		t.Fatalf("RevokeInboundCredential: %v", err)
	}
	if revokedAt.IsZero() {
		t.Fatal("revocation returned no instant")
	}

	if decision, err := auth.Authenticate(ctx, "actor", revokedKey, revokedSecret); err != nil || decision.Reason != actors.InboundRevoked {
		t.Fatalf("revoked party still dialled: %+v err=%v", decision, err)
	}
	if decision, err := auth.Authenticate(ctx, "actor", survivingKey, survivingSecret); err != nil || !decision.Allowed {
		t.Fatalf("revoking one credential un-dialled another: %+v err=%v", decision, err)
	}

	again, err := s.RevokeInboundCredential(ctx, "actor", revokedKey)
	if err != nil {
		t.Fatalf("second revocation: %v", err)
	}
	if !again.Equal(revokedAt) {
		t.Errorf("re-revoking moved the recorded instant from %s to %s", revokedAt, again)
	}
	if _, err := s.RevokeInboundCredential(ctx, "actor", "test/never-issued-"+store.NewULID()); !errors.Is(err, postgres.ErrNotFound) {
		t.Fatalf("revoking an unknown party: err = %v, want ErrNotFound", err)
	}
}

// TestReissueInboundCredentialReplacesTheSecretAndClearsLockout proves a
// reissue is a genuine replacement: the previous plaintext stops working,
// the new one works, and the lockout the previous failures accumulated does
// not outlive the credential it belonged to.
func TestReissueInboundCredentialReplacesTheSecretAndClearsLockout(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	key := "test/reissued-" + store.NewULID()
	first := issueCredential(t, s, key)

	auth, err := actors.NewInboundAuthenticator(s, issuanceConfig, nil)
	if err != nil {
		t.Fatalf("NewInboundAuthenticator: %v", err)
	}
	for i := 0; i < issuanceConfig.FailureLimit; i++ {
		if _, err := auth.Authenticate(ctx, "actor", key, "wrong"); err != nil {
			t.Fatalf("failing dial %d: %v", i+1, err)
		}
	}
	if decision, err := auth.Authenticate(ctx, "actor", key, first); err != nil || decision.Reason != actors.InboundLocked {
		t.Fatalf("expected a lockout before reissue: %+v err=%v", decision, err)
	}

	second := issueCredential(t, s, key)
	if second == first {
		t.Fatal("reissue returned the previous credential")
	}
	if decision, err := auth.Authenticate(ctx, "actor", key, second); err != nil || !decision.Allowed {
		t.Fatalf("reissued credential was refused: %+v err=%v", decision, err)
	}
	if decision, err := auth.Authenticate(ctx, "actor", key, first); err != nil || decision.Allowed {
		t.Fatalf("the superseded credential still dialled: %+v err=%v", decision, err)
	}
	if text := inboundRowText(t, s, key); strings.Contains(text, first) || strings.Contains(text, second) {
		t.Fatal("a reissued row retained a presentable value")
	}

	var issuanceCount int
	if err := s.Pool().QueryRow(ctx,
		`SELECT issuance_count FROM inbound_authentication WHERE party_kind='actor' AND party_key=$1`, key).
		Scan(&issuanceCount); err != nil {
		t.Fatalf("read issuance_count: %v", err)
	}
	if issuanceCount != 2 {
		t.Errorf("issuance_count = %d after two issuances, want 2", issuanceCount)
	}
}

// TestIssuedCredentialRemainsGatedByRateLimitAndLockout is acceptance
// criterion 4 against the durable record: minting changed nothing about the
// controls migrations 0032 added.
func TestIssuedCredentialRemainsGatedByRateLimitAndLockout(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	key := "test/gated-" + store.NewULID()
	secret := issueCredential(t, s, key)

	config := issuanceConfig
	config.RateLimit = 4
	auth, err := actors.NewInboundAuthenticator(s, config, nil)
	if err != nil {
		t.Fatalf("NewInboundAuthenticator: %v", err)
	}
	for i := 0; i < config.RateLimit; i++ {
		if decision, err := auth.Authenticate(ctx, "actor", key, secret); err != nil || !decision.Allowed {
			t.Fatalf("dial %d: %+v err=%v", i+1, decision, err)
		}
	}
	if decision, err := auth.Authenticate(ctx, "actor", key, secret); err != nil || decision.Reason != actors.InboundRateLimited {
		t.Fatalf("rate limit on an issued credential: %+v err=%v", decision, err)
	}

	lockKey := "test/lockable-" + store.NewULID()
	lockSecret := issueCredential(t, s, lockKey)
	for i := 0; i < config.FailureLimit; i++ {
		if _, err := auth.Authenticate(ctx, "actor", lockKey, "wrong"); err != nil {
			t.Fatalf("failing dial %d: %v", i+1, err)
		}
	}
	if decision, err := auth.Authenticate(ctx, "actor", lockKey, lockSecret); err != nil || decision.Reason != actors.InboundLocked {
		t.Fatalf("lockout on an issued credential: %+v err=%v", decision, err)
	}
}
