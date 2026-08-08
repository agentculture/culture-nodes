package actors_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func signerAt(t *testing.T, now time.Time, opts ...actors.TokenOption) *actors.TokenSigner {
	t.Helper()
	opts = append([]actors.TokenOption{actors.WithTokenClock(func() time.Time { return now })}, opts...)
	signer, err := actors.NewTokenSigner([]byte(testSecret), opts...)
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}
	return signer
}

func TestTokenMintAndVerifyRoundTrip(t *testing.T) {
	now := time.Date(2026, 8, 8, 15, 0, 0, 0, time.UTC)
	signer := signerAt(t, now)

	token, err := signer.Mint("att_01J")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.HasPrefix(token, "cnt1.") {
		t.Errorf("token %q does not carry a format version prefix", token)
	}
	if strings.Contains(token, "att_01J") {
		t.Error("the attempt id appears verbatim in the token; it should be encoded")
	}

	attemptID, err := signer.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if attemptID != "att_01J" {
		t.Errorf("Verify returned attempt %q, want att_01J", attemptID)
	}
	if err := signer.VerifyFor(token, "att_01J"); err != nil {
		t.Errorf("VerifyFor its own attempt: %v", err)
	}
}

// A token is authority over ONE attempt. Presenting a perfectly valid token
// for a different attempt is the confusion attempt scoping exists to prevent.
func TestTokenForAnotherAttemptIsRefused(t *testing.T) {
	signer := signerAt(t, time.Now().UTC())
	token, err := signer.Mint("att_mine")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	err = signer.VerifyFor(token, "att_yours")
	assertTokenKind(t, err, actors.TokenForeignAttempt)
}

func TestTokenExpiry(t *testing.T) {
	minted := time.Date(2026, 8, 8, 15, 0, 0, 0, time.UTC)
	clock := minted
	signer, err := actors.NewTokenSigner([]byte(testSecret),
		actors.WithTokenTTL(5*time.Minute),
		actors.WithTokenClock(func() time.Time { return clock }))
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}

	token, err := signer.Mint("att_01J")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	clock = minted.Add(4 * time.Minute)
	if _, err := signer.Verify(token); err != nil {
		t.Fatalf("a token inside its TTL was refused: %v", err)
	}

	clock = minted.Add(5*time.Minute + time.Second)
	_, err = signer.Verify(token)
	assertTokenKind(t, err, actors.TokenExpired)
}

func TestMintUntilBoundsTheTokenToTheWork(t *testing.T) {
	now := time.Date(2026, 8, 8, 15, 0, 0, 0, time.UTC)
	clock := now
	signer, err := actors.NewTokenSigner([]byte(testSecret),
		// A long default TTL that MintUntil must override: a token for an
		// attempt with a 30-second deadline must not live for 15 minutes.
		actors.WithTokenTTL(time.Hour),
		actors.WithTokenClock(func() time.Time { return clock }))
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}

	token, err := signer.MintUntil("att_01J", now.Add(30*time.Second))
	if err != nil {
		t.Fatalf("MintUntil: %v", err)
	}
	clock = now.Add(45 * time.Second)
	_, err = signer.Verify(token)
	assertTokenKind(t, err, actors.TokenExpired)
}

func TestTokenTamperingIsRefused(t *testing.T) {
	now := time.Now().UTC()
	signer := signerAt(t, now)
	token, err := signer.Mint("att_01J")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	parts := strings.Split(token, ".")

	t.Run("forged subject", func(t *testing.T) {
		// Re-encode a different attempt id under the original signature. If
		// the MAC did not cover the subject this would silently succeed and
		// let one attempt's token report on another.
		forged := strings.Join([]string{parts[0], "YXR0X290aGVy", parts[2], parts[3]}, ".")
		_, err := signer.Verify(forged)
		assertTokenKind(t, err, actors.TokenBadSignature)
	})

	t.Run("extended expiry", func(t *testing.T) {
		extended := strings.Join([]string{parts[0], parts[1], "99999999999", parts[3]}, ".")
		_, err := signer.Verify(extended)
		// The signature covers the expiry, so pushing it out is caught as a
		// bad signature — NOT reported as valid, and not reported as expired.
		assertTokenKind(t, err, actors.TokenBadSignature)
	})

	t.Run("another signer's secret", func(t *testing.T) {
		other, err := actors.NewTokenSigner([]byte("fedcba9876543210fedcba9876543210"),
			actors.WithTokenClock(func() time.Time { return now }))
		if err != nil {
			t.Fatalf("NewTokenSigner: %v", err)
		}
		_, err = other.Verify(token)
		assertTokenKind(t, err, actors.TokenBadSignature)
	})
}

// The signature is checked BEFORE the expiry. Reporting "expired" for a token
// whose signature is wrong would tell a forger that everything except the
// clock was right.
func TestSignatureIsCheckedBeforeExpiry(t *testing.T) {
	minted := time.Date(2026, 8, 8, 15, 0, 0, 0, time.UTC)
	clock := minted
	signer, err := actors.NewTokenSigner([]byte(testSecret),
		actors.WithTokenTTL(time.Minute),
		actors.WithTokenClock(func() time.Time { return clock }))
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}
	token, err := signer.Mint("att_01J")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	parts := strings.Split(token, ".")
	tampered := strings.Join([]string{parts[0], parts[1], parts[2], "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}, ".")

	clock = minted.Add(time.Hour) // also long expired
	_, err = signer.Verify(tampered)
	assertTokenKind(t, err, actors.TokenBadSignature)
}

func TestMalformedTokens(t *testing.T) {
	signer := signerAt(t, time.Now().UTC())
	for _, token := range []string{
		"",
		"not-a-token",
		"cnt1.only.three",
		"cnt1.a.b.c.d",
		"cnt2.YXR0", // right shape, wrong version
		"cnt1.!!!.123.YWJj",
	} {
		attemptID, err := signer.Verify(token)
		if err == nil {
			t.Errorf("Verify(%q) succeeded", token)
			continue
		}
		if attemptID != "" {
			t.Errorf("Verify(%q) returned attempt %q alongside an error", token, attemptID)
		}
		if !errors.Is(err, actors.ErrToken) {
			t.Errorf("Verify(%q) error does not match ErrToken: %v", token, err)
		}
	}
}

func TestTokenSecretFloor(t *testing.T) {
	if _, err := actors.NewTokenSigner([]byte("tooshort")); err == nil {
		t.Fatal("a short secret was accepted")
	}
	if _, err := actors.NewTokenSigner(make([]byte, actors.MinTokenSecretBytes)); err != nil {
		t.Fatalf("a secret at the documented floor was rejected: %v", err)
	}
}

func TestMintRequiresAnAttemptID(t *testing.T) {
	signer := signerAt(t, time.Now().UTC())
	if _, err := signer.Mint(""); err == nil {
		t.Fatal("Mint(\"\") succeeded; a token with no subject authorizes everything or nothing")
	}
	if _, err := signer.MintUntil("att_01J", time.Time{}); err == nil {
		t.Fatal("MintUntil with a zero expiry succeeded")
	}
}

func assertTokenKind(t *testing.T, err error, want actors.TokenErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a %s refusal, got no error", want)
	}
	got, ok := actors.TokenErrorKindOf(err)
	if !ok {
		t.Fatalf("error is not a token refusal: %v", err)
	}
	if got != want {
		t.Fatalf("refusal kind = %s, want %s (%v)", got, want, err)
	}
	if !errors.Is(err, actors.ErrToken) {
		t.Errorf("errors.Is(err, ErrToken) = false for %v", err)
	}
}
