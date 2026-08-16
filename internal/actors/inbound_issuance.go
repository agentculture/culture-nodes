package actors

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// Dial-in credential ISSUANCE (issue #111's dial-in half, decided for issue
// #136). Migration 0031 gave the control plane somewhere to keep a per-party
// verifier and 0032 gave it revocation, lockout and rate state; what neither
// gave it was a way to MINT. Until this file the value a bridge presented
// was operator-invented (PREFIX_DIAL_TOKEN) and the control plane only
// learned a digest of it after the fact — so "who issued this identity" had
// no answer, which is exactly what docs/decisions/transport-inversion.md
// names as the precondition for enabling the first production bridge.
//
// The shape of the answer is deliberate:
//
//   - crypto/rand chooses the value. There is no exported way to construct
//     an IssuedInboundCredential around a value a caller supplies, so an
//     operator-invented secret cannot become an issued one by mistake.
//   - only the SHA-256 digest is persistable: the plaintext lives in an
//     InboundCredentialSecret that reveals itself once and redacts itself in
//     every string, JSON and slog rendering, before and after that reveal.
//   - issuance is per party — one bridge, one credential — so revoking one
//     is not an event any other bridge can observe.

// InboundCredentialBytes is how many crypto/rand bytes back one issued
// credential. 256 bits: the same width as the SHA-256 verifier it is stored
// as, so the digest is not the weaker half of the pair.
const InboundCredentialBytes = 32

// InboundCredentialPrefix marks an issued dial-in credential. It makes a
// leaked value recognisable in a log or a config file (and greppable by a
// secret scanner) without disclosing anything: the entropy is all in the
// part after it.
const InboundCredentialPrefix = "cnd_"

// InboundCredentialRedaction is what every rendering of a secret prints
// instead of the secret.
const InboundCredentialRedaction = "[redacted inbound credential]"

// ErrInboundCredentialAlreadyRevealed is returned by a second Reveal. The
// control plane keeps no copy, so there is nothing a second caller could be
// given even if it were willing to give it.
var ErrInboundCredentialAlreadyRevealed = errors.New("actors: the issued inbound credential has already been revealed")

// InboundCredentialSecret holds a freshly minted credential until its single
// reveal. It is deliberately not a string: a string is trivially logged,
// formatted and marshalled, and this type answers all three with a
// redaction. Reveal empties it, so the process holds the plaintext only
// between minting and the response that carries it out.
type InboundCredentialSecret struct {
	value    []byte
	revealed bool
}

// Reveal returns the plaintext exactly once and forgets it. The caller is
// expected to write it straight into the response it was minted for.
func (s *InboundCredentialSecret) Reveal() (string, error) {
	if s == nil || s.revealed || len(s.value) == 0 {
		return "", ErrInboundCredentialAlreadyRevealed
	}
	plaintext := string(s.value)
	for i := range s.value {
		s.value[i] = 0
	}
	s.value = nil
	s.revealed = true
	return plaintext, nil
}

// String, GoString, MarshalJSON and LogValue all render the redaction, so no
// %v, %#v, json.Marshal or slog attribute can leak the value — acceptance
// criterion 2's "never logged" is a property of the type rather than a rule
// every call site has to remember.
func (s *InboundCredentialSecret) String() string { return InboundCredentialRedaction }

func (s *InboundCredentialSecret) GoString() string { return InboundCredentialRedaction }

func (s *InboundCredentialSecret) MarshalJSON() ([]byte, error) {
	return []byte(`"` + InboundCredentialRedaction + `"`), nil
}

func (s *InboundCredentialSecret) LogValue() slog.Value {
	return slog.StringValue(InboundCredentialRedaction)
}

// IssuedInboundCredential is everything about an issuance that may be
// written down: which party it belongs to and the one-way verifier. It has
// no field the plaintext could occupy, which is what makes "the plaintext
// never reaches the database" structural rather than careful.
type IssuedInboundCredential struct {
	PartyKind string `json:"party_kind"`
	PartyKey  string `json:"party_key"`
	Digest    []byte `json:"digest_sha256"`
}

// DigestHex renders the verifier for an operator-facing response. It
// discloses nothing: a SHA-256 digest is what the database already holds.
func (c IssuedInboundCredential) DigestHex() string {
	return fmt.Sprintf("%x", c.Digest)
}

var (
	inboundActorKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)+$`)
	inboundHostKeyPattern  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?$`)
	inboundDottedQuad      = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,3}){3}$`)
)

// ValidateInboundParty mirrors migration 0031's CHECK constraints in Go, so
// a malformed or address-shaped party is a named refusal at issuance time
// rather than a constraint violation surfacing from PostgreSQL. The
// address rejection is the same shape-based one the migration makes: an
// address-like typo must not become durable identity merely because one
// octet was out of range.
func ValidateInboundParty(partyKind, partyKey string) error {
	if partyKey == "" {
		return errors.New("actors: inbound party key is required")
	}
	if inboundDottedQuad.MatchString(partyKey) || strings.Contains(partyKey, ":") {
		return fmt.Errorf("actors: %q is address-shaped and cannot be a dial-in identity", partyKey)
	}
	switch partyKind {
	case "actor":
		if !inboundActorKeyPattern.MatchString(partyKey) {
			return fmt.Errorf("actors: %q is not an actor key (expected namespace/name)", partyKey)
		}
	case "host":
		if !inboundHostKeyPattern.MatchString(partyKey) {
			return fmt.Errorf("actors: %q is not a host name", partyKey)
		}
	default:
		return fmt.Errorf("actors: inbound party kind %q must be actor or host", partyKind)
	}
	return nil
}

// MintInboundCredential issues one dial-in credential for one party: the
// control plane chooses the value with crypto/rand, keeps only its SHA-256
// digest in the returned record, and hands the plaintext back in a secret
// that reveals itself once.
//
// There is no variant of this function that accepts a value. That is
// acceptance criterion 1 — an operator cannot register a credential they
// invented — expressed as an API rather than as a validation rule.
func MintInboundCredential(partyKind, partyKey string) (*InboundCredentialSecret, IssuedInboundCredential, error) {
	if err := ValidateInboundParty(partyKind, partyKey); err != nil {
		return nil, IssuedInboundCredential{}, err
	}
	material := make([]byte, InboundCredentialBytes)
	if _, err := rand.Read(material); err != nil {
		return nil, IssuedInboundCredential{}, fmt.Errorf("actors: generate inbound credential: %w", err)
	}
	plaintext := []byte(InboundCredentialPrefix + base64.RawURLEncoding.EncodeToString(material))
	for i := range material {
		material[i] = 0
	}
	digest := sha256.Sum256(plaintext)
	return &InboundCredentialSecret{value: plaintext}, IssuedInboundCredential{
		PartyKind: partyKind,
		PartyKey:  partyKey,
		Digest:    digest[:],
	}, nil
}
