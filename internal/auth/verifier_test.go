package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type testJWKS struct {
	mu       sync.Mutex
	keys     map[string]*rsa.PublicKey
	requests int
}

func (j *testJWKS) serveHTTP(w http.ResponseWriter, _ *http.Request) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.requests++
	keys := make([]map[string]string, 0, len(j.keys))
	for kid, key := range j.keys {
		keys = append(keys, map[string]string{
			"kid": kid,
			"kty": "RSA",
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			"alg": "RS256",
			"use": "sig",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
}

func (j *testJWKS) set(keys map[string]*rsa.PublicKey) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.keys = keys
}

func (j *testJWKS) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.requests
}

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func testToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid})
	payload, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func baseClaims(now time.Time) map[string]any {
	return map[string]any{
		"aud":   []string{"access-audience", "other"},
		"iss":   "https://team.example.com",
		"exp":   now.Add(time.Hour).Unix(),
		"nbf":   now.Add(-time.Minute).Unix(),
		"iat":   now.Add(-time.Minute).Unix(),
		"sub":   "user-123",
		"email": "person@example.com",
	}
}

func newTestVerifier(server *httptest.Server, now time.Time) *Verifier {
	return New("team.example.com", "access-audience",
		WithJWKSURL(server.URL),
		WithHTTPClient(server.Client()),
		WithClock(func() time.Time { return now }),
		WithRefetchWindow(time.Minute),
	)
}

func TestVerifyValidInteractiveToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	key := testKey(t)
	jwks := &testJWKS{keys: map[string]*rsa.PublicKey{"key-1": &key.PublicKey}}
	server := httptest.NewServer(http.HandlerFunc(jwks.serveHTTP))
	defer server.Close()

	principal, err := newTestVerifier(server, now).Verify(context.Background(), testToken(t, key, "key-1", baseClaims(now)))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	want := Principal{Subject: "user-123", Email: "person@example.com", Kind: PrincipalInteractive}
	if principal != want {
		t.Fatalf("Verify() principal = %#v, want %#v", principal, want)
	}
	provider, subject := principal.BindingKey()
	if provider != "cloudflare-access" || subject != "user-123" {
		t.Fatalf("BindingKey() = (%q, %q)", provider, subject)
	}
}

func TestVerifyValidServiceToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	key := testKey(t)
	jwks := &testJWKS{keys: map[string]*rsa.PublicKey{"key-1": &key.PublicKey}}
	server := httptest.NewServer(http.HandlerFunc(jwks.serveHTTP))
	defer server.Close()
	claims := baseClaims(now)
	delete(claims, "email")
	claims["common_name"] = "deploy-bridge"

	principal, err := newTestVerifier(server, now).Verify(context.Background(), testToken(t, key, "key-1", claims))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	want := Principal{Subject: "user-123", CommonName: "deploy-bridge", Kind: PrincipalServiceToken}
	if principal != want {
		t.Fatalf("Verify() principal = %#v, want %#v", principal, want)
	}
	provider, subject := principal.BindingKey()
	if provider != "cloudflare-service-token" || subject != "deploy-bridge" {
		t.Fatalf("BindingKey() = (%q, %q)", provider, subject)
	}
}

func TestVerifyRefusalReasons(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	key := testKey(t)
	wrongKey := testKey(t)
	tests := []struct {
		name   string
		reason string
		token  func(*testing.T) string
	}{
		{"wrong audience", "bad_audience", func(t *testing.T) string {
			c := baseClaims(now)
			c["aud"] = "wrong"
			return testToken(t, key, "key-1", c)
		}},
		{"wrong issuer", "bad_issuer", func(t *testing.T) string {
			c := baseClaims(now)
			c["iss"] = "https://wrong.example.com"
			return testToken(t, key, "key-1", c)
		}},
		{"expired", "expired", func(t *testing.T) string {
			c := baseClaims(now)
			c["exp"] = now.Add(-time.Second).Unix()
			return testToken(t, key, "key-1", c)
		}},
		{"not yet valid", "not_yet_valid", func(t *testing.T) string {
			c := baseClaims(now)
			c["nbf"] = now.Add(time.Second).Unix()
			return testToken(t, key, "key-1", c)
		}},
		{"unknown kid", "unknown_kid", func(t *testing.T) string { return testToken(t, key, "missing", baseClaims(now)) }},
		{"bad signature", "bad_signature", func(t *testing.T) string { return testToken(t, wrongKey, "key-1", baseClaims(now)) }},
		{"malformed", "malformed", func(*testing.T) string { return "not.a.jwt.with-three-parts" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jwks := &testJWKS{keys: map[string]*rsa.PublicKey{"key-1": &key.PublicKey}}
			server := httptest.NewServer(http.HandlerFunc(jwks.serveHTTP))
			defer server.Close()
			_, err := newTestVerifier(server, now).Verify(context.Background(), tt.token(t))
			var verificationErr *VerificationError
			if !errors.As(err, &verificationErr) {
				t.Fatalf("Verify() error = %T %v, want *VerificationError", err, err)
			}
			if verificationErr.Reason != tt.reason {
				t.Fatalf("Verify() reason = %q, want %q", verificationErr.Reason, tt.reason)
			}
			if tt.reason == "unknown_kid" && jwks.count() != 2 {
				t.Fatalf("unknown kid requests = %d, want initial fetch plus exactly one refetch", jwks.count())
			}
		})
	}
}

func TestJWKSCacheAntiFloodsKidMisses(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	key := testKey(t)
	jwks := &testJWKS{keys: map[string]*rsa.PublicKey{"key-1": &key.PublicKey}}
	server := httptest.NewServer(http.HandlerFunc(jwks.serveHTTP))
	defer server.Close()
	verifier := newTestVerifier(server, now)

	for _, kid := range []string{"missing-1", "missing-2"} {
		_, err := verifier.Verify(context.Background(), testToken(t, key, kid, baseClaims(now)))
		var verificationErr *VerificationError
		if !errors.As(err, &verificationErr) || verificationErr.Reason != "unknown_kid" {
			t.Fatalf("Verify(%q) error = %v", kid, err)
		}
	}
	if jwks.count() != 2 {
		t.Fatalf("JWKS requests = %d, want initial fetch and one miss refetch", jwks.count())
	}
}

func TestJWKSCacheSurvivesKeyRotation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	oldKey := testKey(t)
	newKey := testKey(t)
	jwks := &testJWKS{keys: map[string]*rsa.PublicKey{"old": &oldKey.PublicKey}}
	server := httptest.NewServer(http.HandlerFunc(jwks.serveHTTP))
	defer server.Close()
	verifier := newTestVerifier(server, now)

	if _, err := verifier.Verify(context.Background(), testToken(t, oldKey, "old", baseClaims(now))); err != nil {
		t.Fatalf("old token: %v", err)
	}
	jwks.set(map[string]*rsa.PublicKey{"new": &newKey.PublicKey})
	if _, err := verifier.Verify(context.Background(), testToken(t, newKey, "new", baseClaims(now))); err != nil {
		t.Fatalf("new token after rotation: %v", err)
	}
	if jwks.count() != 2 {
		t.Fatalf("JWKS requests = %d, want initial fetch and one rotation refetch", jwks.count())
	}
}

func TestVerifyMalformedClaims(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	key := testKey(t)
	jwks := &testJWKS{keys: map[string]*rsa.PublicKey{"key-1": &key.PublicKey}}
	server := httptest.NewServer(http.HandlerFunc(jwks.serveHTTP))
	defer server.Close()
	claims := baseClaims(now)
	delete(claims, "sub")

	_, err := newTestVerifier(server, now).Verify(context.Background(), testToken(t, key, "key-1", claims))
	var verificationErr *VerificationError
	if !errors.As(err, &verificationErr) || verificationErr.Reason != "malformed" {
		t.Fatalf("Verify() error = %T %v", err, err)
	}
}

func ExampleVerificationError() {
	err := &VerificationError{Reason: "expired"}
	fmt.Println(err)
	// Output: access token verification failed: expired
}
