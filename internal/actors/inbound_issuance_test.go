package actors_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
)

// mustMint mints one credential and fails the test if issuance is refused.
func mustMint(t *testing.T, partyKind, partyKey string) (*actors.InboundCredentialSecret, actors.IssuedInboundCredential) {
	t.Helper()
	secret, issued, err := actors.MintInboundCredential(partyKind, partyKey)
	if err != nil {
		t.Fatalf("MintInboundCredential(%q, %q): %v", partyKind, partyKey, err)
	}
	return secret, issued
}

// TestMintInboundCredentialIssuesDistinctVerifiableSecrets is acceptance
// criterion 1's positive half: the control plane — not the caller — chooses
// the value, every issuance differs, and the digest that will be persisted
// is exactly SHA-256 of the value the bridge will present.
func TestMintInboundCredentialIssuesDistinctVerifiableSecrets(t *testing.T) {
	firstSecret, firstIssued := mustMint(t, "actor", "company/codex-thor")
	secondSecret, secondIssued := mustMint(t, "actor", "company/codex-thor")

	first, err := firstSecret.Reveal()
	if err != nil {
		t.Fatalf("reveal first: %v", err)
	}
	second, err := secondSecret.Reveal()
	if err != nil {
		t.Fatalf("reveal second: %v", err)
	}
	if first == second {
		t.Fatal("two issuances produced the same credential")
	}
	if len(first) < 32 {
		t.Fatalf("issued credential is only %d characters; that is not 256 bits of crypto/rand", len(first))
	}

	want := sha256.Sum256([]byte(first))
	if string(firstIssued.Digest) != string(want[:]) {
		t.Fatal("persisted digest is not SHA-256 of the issued credential")
	}
	if len(secondIssued.Digest) != sha256.Size {
		t.Fatalf("digest width = %d, want %d", len(secondIssued.Digest), sha256.Size)
	}
	if firstIssued.PartyKind != "actor" || firstIssued.PartyKey != "company/codex-thor" {
		t.Fatalf("issued record identity = %q %q", firstIssued.PartyKind, firstIssued.PartyKey)
	}

	verifier, err := actors.NewInboundCredentialVerifier(firstIssued.Digest, "", nil)
	if err != nil {
		t.Fatalf("NewInboundCredentialVerifier: %v", err)
	}
	if !verifier.Verify(first) {
		t.Error("the issued credential does not verify against its own digest")
	}
	if verifier.Verify(second) {
		t.Error("another actor's issued credential verified against this digest")
	}
}

// TestMintedInboundSecretRevealsExactlyOnce is acceptance criterion 2's
// reveal-once half: the plaintext leaves the process once, and the holder
// cannot ask for it a second time (nothing kept it).
func TestMintedInboundSecretRevealsExactlyOnce(t *testing.T) {
	secret, _ := mustMint(t, "actor", "company/claude-code")
	first, err := secret.Reveal()
	if err != nil || first == "" {
		t.Fatalf("first reveal: %q err=%v", first, err)
	}
	again, err := secret.Reveal()
	if err == nil {
		t.Fatal("the credential was revealed a second time")
	}
	if again != "" {
		t.Fatalf("a refused reveal still returned %q", again)
	}
}

// TestMintedInboundSecretRedactsItselfFromLogsAndJSON is acceptance
// criterion 2's never-logged half at the type level: even a caller who logs
// or serialises the secret by accident emits a redaction, before and after
// the single legitimate reveal.
func TestMintedInboundSecretRedactsItselfFromLogsAndJSON(t *testing.T) {
	secret, _ := mustMint(t, "host", "thor")

	var sink bytes.Buffer
	slog.New(slog.NewJSONHandler(&sink, nil)).Info("issued dial-in credential", "credential", secret)

	rendered := []string{
		sink.String(),
		fmt.Sprintf("%v|%s|%#v|%+v", secret, secret, secret, secret),
	}
	if raw, err := json.Marshal(map[string]any{"credential": secret}); err != nil {
		t.Fatalf("json.Marshal: %v", err)
	} else {
		rendered = append(rendered, string(raw))
	}

	plaintext, err := secret.Reveal()
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	rendered = append(rendered, fmt.Sprintf("%v|%s|%#v", secret, secret, secret))

	for _, text := range rendered {
		if strings.Contains(text, plaintext) {
			t.Errorf("rendered form leaked the credential: %s", text)
		}
		if !strings.Contains(text, actors.InboundCredentialRedaction) {
			t.Errorf("rendered form %q does not carry the redaction marker", text)
		}
	}
}

// TestIssuedInboundCredentialCarriesNoPlaintext proves the record handed to
// the store cannot carry the value: acceptance criterion 2's "never reaches
// the DB" is structural here, not a discipline the store has to keep.
func TestIssuedInboundCredentialCarriesNoPlaintext(t *testing.T) {
	secret, issued := mustMint(t, "actor", "company/human-inbox")
	plaintext, err := secret.Reveal()
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	raw, err := json.Marshal(issued)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	for _, text := range []string{string(raw), fmt.Sprintf("%#v %+v", issued, issued)} {
		if strings.Contains(text, plaintext) {
			t.Fatalf("the persistable record carried the plaintext: %s", text)
		}
	}
}

// TestMintInboundCredentialRefusesPartiesTheSchemaWouldRefuse keeps the Go
// issuance path and migration 0031's CHECK constraints in agreement: an
// address-shaped or malformed party is a named refusal at mint time rather
// than a constraint violation from PostgreSQL.
func TestMintInboundCredentialRefusesPartiesTheSchemaWouldRefuse(t *testing.T) {
	cases := []struct {
		name string
		kind string
		key  string
	}{
		{"unknown kind", "bridge", "company/x"},
		{"empty key", "actor", ""},
		{"actor key without a namespace segment", "actor", "codex"},
		{"dotted quad host", "host", "192.168.1.157"},
		{"ipv6 host", "host", "fe80::1"},
		{"actor key that is an address", "actor", "192.168.1.118"},
		{"host with a slash", "host", "thor/codex"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := actors.MintInboundCredential(tc.kind, tc.key); err == nil {
				t.Fatalf("MintInboundCredential(%q, %q) was accepted", tc.kind, tc.key)
			}
		})
	}
}

// TestUnissuedCredentialIsRefusedWhenIssuanceIsRequired is acceptance
// criterion 1's negative half: a verifier the control plane did not mint —
// an operator-invented PREFIX_DIAL_TOKEN registered by hand — does not admit
// a dial once the deployment requires control-plane issuance.
func TestUnissuedCredentialIsRefusedWhenIssuanceIsRequired(t *testing.T) {
	config := actors.InboundAuthenticationConfig{
		RateLimit: 10, RateWindow: time.Minute,
		FailureLimit: 5, LockoutDuration: time.Minute,
		RequireControlPlaneIssued: true,
	}
	auth, state, _ := inboundFixture(t, config)

	decision, err := auth.Authenticate(context.Background(), "actor", "company/x", "right")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if decision.Allowed || decision.Reason != actors.InboundNotIssued {
		t.Fatalf("operator-provisioned credential decision = %+v", decision)
	}
	if state.state.RateAttemptCount != 0 || state.state.FailureCount != 0 {
		t.Fatalf("a record-level refusal consumed admission budget: %+v", state.state)
	}

	state.state.Issued = true
	if decision, err := auth.Authenticate(context.Background(), "actor", "company/x", "right"); err != nil || !decision.Allowed {
		t.Fatalf("issued credential decision = %+v err=%v", decision, err)
	}
}

// TestDefaultInboundAuthenticationRequiresControlPlaneIssuance pins the
// posture: the shipped default is the #111 replacement, not the operator-
// provisioned record migration 0031 called temporary.
func TestDefaultInboundAuthenticationRequiresControlPlaneIssuance(t *testing.T) {
	if !actors.DefaultInboundAuthenticationConfig.RequireControlPlaneIssued {
		t.Fatal("the default configuration still admits credentials the control plane did not issue")
	}
}

// TestIssuedCredentialStillPassesThroughRateLimitAndLockout is acceptance
// criterion 4 at the decision level: issuance does not create a bypass — an
// issued credential is rate limited and locked out exactly as before.
func TestIssuedCredentialStillPassesThroughRateLimitAndLockout(t *testing.T) {
	config := actors.InboundAuthenticationConfig{
		RateLimit: 3, RateWindow: time.Minute,
		FailureLimit: 2, LockoutDuration: time.Minute,
		RequireControlPlaneIssued: true,
	}
	auth, state, _ := inboundFixture(t, config)
	state.state.Issued = true
	ctx := context.Background()

	for i := 0; i < config.FailureLimit; i++ {
		if decision, err := auth.Authenticate(ctx, "actor", "company/x", "wrong"); err != nil || decision.Reason != actors.InboundInvalid {
			t.Fatalf("wrong presentation %d: %+v err=%v", i+1, decision, err)
		}
	}
	if decision, err := auth.Authenticate(ctx, "actor", "company/x", "right"); err != nil || decision.Reason != actors.InboundLocked {
		t.Fatalf("lockout after failures: %+v err=%v", decision, err)
	}

	fresh, freshState, _ := inboundFixture(t, config)
	freshState.state.Issued = true
	for i := 0; i < config.RateLimit; i++ {
		if _, err := fresh.Authenticate(ctx, "actor", "company/x", "right"); err != nil {
			t.Fatalf("dial %d: %v", i+1, err)
		}
	}
	if decision, err := fresh.Authenticate(ctx, "actor", "company/x", "right"); err != nil || decision.Reason != actors.InboundRateLimited {
		t.Fatalf("rate limit on an issued credential: %+v err=%v", decision, err)
	}
}
