package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
)

// The dial-in credential issuance lane (issue #111's dial-in half, decided
// for issue #136). A bridge's identity is a token THIS control plane issued:
// it dials out, presents that token, and no address of it is stored anywhere.
//
// Two operations, both authenticated with their own bearer secret
// (WithInboundIssuanceSecret / NODES_INBOUND_ISSUANCE_TOKEN_SECRET):
//
//	POST /v1alpha1/inbound/credentials         mint (or re-mint) one party's
//	                                           credential and reveal it once
//	POST /v1alpha1/inbound/credentials/revoke  end one party's authority
//
// The value is revealed in the issuance response and nowhere else. Nothing
// stores it, nothing logs it (actors.InboundCredentialSecret redacts itself
// in every rendering), and there is no read endpoint that could return it
// again — losing it means issuing a new one, which is the point.
//
// The party key is carried in the BODY, not the path: an actor key contains
// a slash (`company/bridge-name`), and a path-embedded key would be an
// escaping bug waiting to be a security bug.

// WithInboundIssuanceSecret configures the bearer secret the dial-in
// credential issuance and revocation routes require. Omitting it (or passing
// "") leaves both refused with 401 rather than mounted-but-authless — the
// same closed-by-default rule the decision, registration, event and ad-hoc
// secrets follow. Its own secret again: issuing a bridge's identity is a
// distinct standing from registering an actor or deciding a human task.
func WithInboundIssuanceSecret(secret string) Option {
	return func(s *Server) {
		if secret != "" {
			s.inboundIssuanceSecret = []byte(secret)
		}
	}
}

// inboundCredentialRequest is the whole request shape: which party. The
// remaining fields are deliberate honeypots — every name a caller might
// reach for when trying to supply a credential VALUE — so a request that
// tries to register an operator-invented secret gets a named refusal
// instead of the generic "unknown field" that DisallowUnknownFields would
// produce for a typo. Both refuse; only one explains.
type inboundCredentialRequest struct {
	PartyKind string `json:"party_kind"`
	PartyKey  string `json:"party_key"`

	Credential      json.RawMessage `json:"credential"`
	Token           json.RawMessage `json:"token"`
	Secret          json.RawMessage `json:"secret"`
	Verifier        json.RawMessage `json:"verifier"`
	VerifierSHA256  json.RawMessage `json:"verifier_sha256"`
	VerifierEnvName json.RawMessage `json:"verifier_env_name"`
	Digest          json.RawMessage `json:"digest_sha256"`
}

// suppliedMaterial names the field a caller used to try to supply its own
// credential, or "" when it supplied none.
func (r inboundCredentialRequest) suppliedMaterial() string {
	for name, value := range map[string]json.RawMessage{
		"credential":        r.Credential,
		"token":             r.Token,
		"secret":            r.Secret,
		"verifier":          r.Verifier,
		"verifier_sha256":   r.VerifierSHA256,
		"verifier_env_name": r.VerifierEnvName,
		"digest_sha256":     r.Digest,
	} {
		if len(value) > 0 && string(value) != "null" {
			return name
		}
	}
	return ""
}

// issuedInboundCredentialOut is the one and only time the plaintext leaves
// this process. `credential` is not readable anywhere else, at any later
// time, by anyone.
type issuedInboundCredentialOut struct {
	PartyKind    string    `json:"party_kind"`
	PartyKey     string    `json:"party_key"`
	Credential   string    `json:"credential"`
	DigestSHA256 string    `json:"digest_sha256"`
	IssuedAt     time.Time `json:"issued_at"`
	RevealedOnce bool      `json:"revealed_once"`
}

type revokedInboundCredentialOut struct {
	PartyKind string    `json:"party_kind"`
	PartyKey  string    `json:"party_key"`
	RevokedAt time.Time `json:"revoked_at"`
}

func (s *Server) decodeInboundCredentialRequest(r *http.Request) (inboundCredentialRequest, error) {
	var req inboundCredentialRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return req, badRequest(
			"send a JSON body naming only the party: {party_kind, party_key}",
			"decode request body: %v", err)
	}
	if field := req.suppliedMaterial(); field != "" {
		return req, badRequest(
			"omit "+field+" — request issuance for a party and read the credential from the response",
			"the control plane issues dial-in credentials; a caller-supplied %s cannot be registered", field)
	}
	if req.PartyKind == "" {
		req.PartyKind = "actor"
	}
	if err := actors.ValidateInboundParty(req.PartyKind, req.PartyKey); err != nil {
		return req, badRequest(
			"party_kind must be actor or host, and party_key must be an actor key (namespace/name) or host name that is not an address",
			"%v", err)
	}
	return req, nil
}

// handleIssueInboundCredential mints one party's dial-in credential and
// reveals it exactly once. Issuing again for a party that already has one
// replaces it: the previous value stops admitting dials immediately, with
// nothing restarted.
func (s *Server) handleIssueInboundCredential(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireInboundIssuanceAuth(r); err != nil {
		return err
	}
	req, err := s.decodeInboundCredentialRequest(r)
	if err != nil {
		return err
	}

	secret, issued, err := actors.MintInboundCredential(req.PartyKind, req.PartyKey)
	if err != nil {
		return internalError(err)
	}
	issuedAt, err := s.Store.IssueInboundCredential(r.Context(), issued)
	if err != nil {
		return classify(err)
	}
	// Revealed only after the verifier is durable: a credential handed out
	// for a row that failed to commit would be a secret nothing can verify.
	plaintext, err := secret.Reveal()
	if err != nil {
		return internalError(err)
	}

	writeJSON(w, http.StatusCreated, issuedInboundCredentialOut{
		PartyKind:    issued.PartyKind,
		PartyKey:     issued.PartyKey,
		Credential:   plaintext,
		DigestSHA256: issued.DigestHex(),
		IssuedAt:     issuedAt,
		RevealedOnce: true,
	})
	return nil
}

// handleRevokeInboundCredential ends one party's dial-in authority. It is
// scoped to exactly one credential, takes effect on that bridge's next dial,
// and touches no other bridge's session or the process serving them.
func (s *Server) handleRevokeInboundCredential(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireInboundIssuanceAuth(r); err != nil {
		return err
	}
	req, err := s.decodeInboundCredentialRequest(r)
	if err != nil {
		return err
	}
	revokedAt, err := s.Store.RevokeInboundCredential(r.Context(), req.PartyKind, req.PartyKey)
	if err != nil {
		return classify(err)
	}
	writeJSON(w, http.StatusOK, revokedInboundCredentialOut{
		PartyKind: req.PartyKind,
		PartyKey:  req.PartyKey,
		RevokedAt: revokedAt,
	})
	return nil
}

// requireInboundIssuanceAuth verifies the issuance bearer secret, in the
// same fixed-cost shape requireActorRegistrationAuth uses: both sides are
// hashed first so the comparison cannot leak the secret's length.
func (s *Server) requireInboundIssuanceAuth(r *http.Request) error {
	if len(s.inboundIssuanceSecret) == 0 {
		return unauthorized(
			"configure the server with a dial-in issuance secret (NODES_INBOUND_ISSUANCE_TOKEN_SECRET) to enable credential issuance",
			"dial-in credential issuance requires a configured bearer secret and none is configured")
	}
	const prefix = "bearer "
	header := r.Header.Get("Authorization")
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return unauthorized("send Authorization: Bearer <token>", "missing or malformed Authorization header")
	}
	presented := sha256.Sum256([]byte(header[len(prefix):]))
	expected := sha256.Sum256(s.inboundIssuanceSecret)
	if subtle.ConstantTimeCompare(presented[:], expected[:]) != 1 {
		return unauthorized("the bearer token is not valid for this deployment", "authorization failed")
	}
	return nil
}
