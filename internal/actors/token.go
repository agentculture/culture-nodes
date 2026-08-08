package actors

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Attempt-scoped callback tokens (PRD §13.1's `callback.token`, described
// there as a "short-lived attempt token").
//
// The whole payload is two facts — which attempt, and until when — so this is
// an HMAC over those two fields and nothing else. There is no JWT here, and
// that is a decision rather than an omission: a JWT would add a parser, an
// algorithm-negotiation field, and a dependency, in exchange for carrying
// claims this token does not have. The runtime's zero-third-party-dependency
// rule (see the repo's root guidance file) is satisfied for free by
// crypto/hmac.
//
// Three properties are load-bearing:
//
//   - The attempt id is inside the signed payload, so a verified token
//     *names* its attempt. Callback ingest never has to trust an attempt id
//     supplied alongside the token; it reads the one the signature covers.
//   - Verification is constant-time (hmac.Equal). A token check that leaked
//     timing would let an attacker walk a signature out one byte at a time.
//   - The signature is checked before the expiry. Reporting "expired" for a
//     token whose signature is wrong would tell a forger that everything
//     except the clock was right.

// DefaultTokenTTL is how long a minted callback token stays valid.
//
// It bounds a leaked token's usefulness, and it is deliberately much shorter
// than a long-running async invocation: an actor that runs for an hour is
// expected to be re-issued a token, not to hold one for the hour. Any
// deployment can shorten it.
const DefaultTokenTTL = 15 * time.Minute

// MinTokenSecretBytes is the shortest secret NewTokenSigner accepts. HMAC-
// SHA256's security argument assumes a key with at least as much entropy as
// the digest is wide; a 16-byte floor is the smallest that is not obviously
// indefensible, and 32 is what a deployment should actually use.
const MinTokenSecretBytes = 16

// tokenPrefix versions the token format. A future format change gets a new
// prefix so an old token fails as malformed rather than as a bad signature.
const tokenPrefix = "cnt1"

// tokenDomain separates this MAC's inputs from any other HMAC the system
// computes with the same secret. Domain separation costs one constant and
// removes a whole class of cross-protocol confusion.
const tokenDomain = "culture-nodes/actor-callback-token/v1"

// TokenErrorKind names why a token was refused.
type TokenErrorKind string

// The refusal kinds.
const (
	// TokenMalformed is a token that is not in this format at all.
	TokenMalformed TokenErrorKind = "malformed"
	// TokenBadSignature is a token whose MAC does not verify: forged,
	// tampered with, or minted with a different secret.
	TokenBadSignature TokenErrorKind = "bad_signature"
	// TokenExpired is a well-formed, correctly signed token past its expiry.
	TokenExpired TokenErrorKind = "expired"
	// TokenForeignAttempt is a valid token presented for a different attempt
	// than the one it was minted for.
	TokenForeignAttempt TokenErrorKind = "foreign_attempt"
)

// TokenError is a typed token refusal.
type TokenError struct {
	Kind   TokenErrorKind
	Detail string
}

func (e *TokenError) Error() string {
	if e.Detail == "" {
		return "actors: callback token " + string(e.Kind)
	}
	return fmt.Sprintf("actors: callback token %s: %s", e.Kind, e.Detail)
}

// Is lets callers match any refusal with errors.Is(err, ErrToken).
func (e *TokenError) Is(target error) bool { return target == ErrToken }

// ErrToken is the sentinel every TokenError matches.
var ErrToken = errors.New("actors: callback token rejected")

// TokenErrorKindOf extracts the refusal kind from an error, reporting false
// when err is not a token refusal.
func TokenErrorKindOf(err error) (TokenErrorKind, bool) {
	var tokErr *TokenError
	if errors.As(err, &tokErr) {
		return tokErr.Kind, true
	}
	return "", false
}

// TokenSigner mints and verifies attempt-scoped callback tokens. It is safe
// for concurrent use.
type TokenSigner struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

// TokenOption configures a TokenSigner.
type TokenOption func(*TokenSigner)

// WithTokenTTL sets how long minted tokens stay valid.
func WithTokenTTL(ttl time.Duration) TokenOption {
	return func(s *TokenSigner) {
		if ttl > 0 {
			s.ttl = ttl
		}
	}
}

// WithTokenClock replaces the signer's clock, so expiry is testable without
// waiting for it.
func WithTokenClock(now func() time.Time) TokenOption {
	return func(s *TokenSigner) {
		if now != nil {
			s.now = now
		}
	}
}

// NewTokenSigner returns a signer over secret.
func NewTokenSigner(secret []byte, opts ...TokenOption) (*TokenSigner, error) {
	if len(secret) < MinTokenSecretBytes {
		return nil, fmt.Errorf(
			"actors: NewTokenSigner: secret is %d bytes, want at least %d",
			len(secret), MinTokenSecretBytes)
	}
	s := &TokenSigner{
		secret: append([]byte(nil), secret...),
		ttl:    DefaultTokenTTL,
		now:    func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s, nil
}

// TTL reports how long minted tokens stay valid.
func (s *TokenSigner) TTL() time.Duration { return s.ttl }

// Mint issues a token for attemptID, valid for the signer's TTL.
func (s *TokenSigner) Mint(attemptID string) (string, error) {
	if attemptID == "" {
		return "", errors.New("actors: Mint: attempt id is required")
	}
	return s.mintAt(attemptID, s.now().Add(s.ttl)), nil
}

// MintUntil issues a token for attemptID that expires at a caller-chosen
// time. It exists for the one case the TTL cannot express: an invocation
// whose deadline is shorter than the TTL should not hand out a token that
// outlives the work.
func (s *TokenSigner) MintUntil(attemptID string, expiry time.Time) (string, error) {
	if attemptID == "" {
		return "", errors.New("actors: MintUntil: attempt id is required")
	}
	if expiry.IsZero() {
		return "", errors.New("actors: MintUntil: expiry is required")
	}
	return s.mintAt(attemptID, expiry), nil
}

func (s *TokenSigner) mintAt(attemptID string, expiry time.Time) string {
	exp := strconv.FormatInt(expiry.UTC().Unix(), 10)
	subject := encode([]byte(attemptID))
	mac := s.sign(subject, exp)
	return strings.Join([]string{tokenPrefix, subject, exp, encode(mac)}, ".")
}

// Verify checks a token and returns the attempt id it was minted for.
//
// The returned attempt id is authenticated: it comes out of the signed
// payload, so a caller may use it to look up durable state without any
// further check. Verify never returns a non-empty attempt id alongside an
// error.
func (s *TokenSigner) Verify(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 4 || parts[0] != tokenPrefix {
		return "", &TokenError{Kind: TokenMalformed, Detail: "expected " + tokenPrefix + ".<subject>.<expiry>.<mac>"}
	}
	subject, exp, macPart := parts[1], parts[2], parts[3]

	presented, err := decode(macPart)
	if err != nil {
		return "", &TokenError{Kind: TokenMalformed, Detail: "signature is not base64url"}
	}

	// Signature first, always. Every field below is attacker-supplied until
	// this check passes, so nothing derived from them may be reported on.
	if !hmac.Equal(presented, s.sign(subject, exp)) {
		return "", &TokenError{Kind: TokenBadSignature}
	}

	attemptIDBytes, err := decode(subject)
	if err != nil {
		return "", &TokenError{Kind: TokenMalformed, Detail: "subject is not base64url"}
	}
	expiryUnix, err := strconv.ParseInt(exp, 10, 64)
	if err != nil {
		return "", &TokenError{Kind: TokenMalformed, Detail: "expiry is not an integer"}
	}
	if !s.now().Before(time.Unix(expiryUnix, 0)) {
		return "", &TokenError{
			Kind:   TokenExpired,
			Detail: "expired at " + time.Unix(expiryUnix, 0).UTC().Format(time.RFC3339),
		}
	}
	return string(attemptIDBytes), nil
}

// VerifyFor checks a token and additionally requires that it was minted for
// wantAttemptID. A valid token for a different attempt is refused as
// TokenForeignAttempt — a token is authority over one attempt, and presenting
// it for another is exactly the confusion attempt scoping exists to prevent.
func (s *TokenSigner) VerifyFor(token, wantAttemptID string) error {
	got, err := s.Verify(token)
	if err != nil {
		return err
	}
	if got != wantAttemptID {
		return &TokenError{
			Kind:   TokenForeignAttempt,
			Detail: fmt.Sprintf("token names attempt %s, presented for %s", got, wantAttemptID),
		}
	}
	return nil
}

func (s *TokenSigner) sign(subject, exp string) []byte {
	mac := hmac.New(sha256.New, s.secret)
	// Length-prefix-free but newline-separated over fields that cannot
	// themselves contain a newline (base64url and digits), so no two distinct
	// field pairs can produce the same signed string.
	mac.Write([]byte(tokenDomain))
	mac.Write([]byte{'\n'})
	mac.Write([]byte(subject))
	mac.Write([]byte{'\n'})
	mac.Write([]byte(exp))
	return mac.Sum(nil)
}

func encode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func decode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
