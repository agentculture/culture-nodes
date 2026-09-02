package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/auth"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The LAN break-glass (login-from-anywhere task t22, deviation d2; spec
// c20 / c48, honesty conditions h11 and h34).
//
// c48 asks for one thing beyond classified refusals: "an operator on the LAN
// keeps a break-glass path (an issued service credential bound to the
// operator's actor) so a misconfigured Access policy cannot lock every human
// out". Task t13 wrote the recipe for it and found it could not be closed
// from a document (docs/operations/people.md, "The c48-shaped credential"):
// deploy/prod/issue-dialin-credential.sh could already mint a credential for
// a human actor, but an issued `cnd_` value was verified in exactly ONE
// place — authenticateInbound, and only for /v1alpha1/inbound/poll and
// /v1alpha1/inbound/{id}/complete. Minting one would have left a live secret
// on a host that opened nothing, while the only real LAN break-glass stayed
// the shared NODES_HUMAN_DECISION_TOKEN_SECRET that h25 takes out of every
// hand.
//
// This file is the missing verification, and deliberately adds no
// credential, table, role source or issuance path:
//
//   - the CREDENTIAL is the existing issued dial-in credential (c20: "what
//     is missing is binding, not credentials"). Nothing here mints, and
//     internal/actors/inbound_issuance.go is untouched.
//   - the ADMISSION POLICY is the existing one, entire and unweakened:
//     internal/actors.InboundAuthenticator.Authenticate, under the same
//     per-credential lock, applying revocation, control-plane issuance,
//     lockout, the rate window and the constant-time verifier check in that
//     order. A break-glass presentation is not a second, laxer door into the
//     same credential — it is the same door.
//   - the ROLE comes from the actor row's registered KIND, not from a new
//     `actor_identities` provider. t13 laid out both options; a human actor
//     gets `approver` — the role c48's break-glass needs to decide, and no
//     more. It is deliberately NOT namespace_administrator: an operator
//     locked out of Access can decide the human task that is blocking the
//     lane, not register actors or publish workflows.
//   - an AGENT actor's credential keeps today's agent semantics exactly
//     (actorbearer.go): machine principal, viewer role, writes only where
//     agentMayWrite says agents may, and a human decision under it is
//     refused by role rather than admitted.
//
// Where it is honoured is as narrow as what it grants. Only the LAN listener
// (Handler) resolves it, never AccessHandler: the loopback listener's whole
// point is that a JWT is honoured only where the tunnel can reach (c43), and
// a second credential class there would widen the surface the split exists
// to keep narrow. And only on the routes principalPolicy protects, plus
// whoami — so the two machine surfaces that already authenticate this
// credential themselves keep doing exactly one Authenticate call per dial,
// with their own rate budget untouched.

// principalProviderInboundCredential marks a Principal resolved from an
// issued dial-in credential presented by a PERSON — as opposed to an Access
// assertion ("cloudflare-access"), a transition secret ("transition"), or an
// agent actor's own bearer ("actor-token").
const principalProviderInboundCredential = "nodes-inbound-credential"

// The refusal reason classes a dial-in credential can produce on the
// principal gate. They are their own classes rather than the JWT ones
// because they are their own facts: a revoked credential is not a bad
// signature, and an operator reading a log line during an incident needs to
// know which of the two happened. Each is logged and counted through
// Server.refuse like every other class (spec c48).
const (
	refusalCredentialRevoked     = "credential_revoked"
	refusalCredentialLocked      = "credential_locked"
	refusalCredentialRateLimited = "credential_rate_limited"
	refusalCredentialNotIssued   = "credential_not_issued"
	refusalCredentialInvalid     = "credential_invalid"
)

// inboundRefusalReason maps an admission decision to its reason class. The
// mapping is total: an unrecognised reason becomes credential_invalid rather
// than being reported as something more specific than it is.
func inboundRefusalReason(reason actors.InboundAuthenticationReason) string {
	switch reason {
	case actors.InboundRevoked:
		return refusalCredentialRevoked
	case actors.InboundLocked:
		return refusalCredentialLocked
	case actors.InboundRateLimited:
		return refusalCredentialRateLimited
	case actors.InboundNotIssued:
		return refusalCredentialNotIssued
	default:
		return refusalCredentialInvalid
	}
}

// breakGlassRoute reports whether an issued dial-in credential is even
// considered on this request. The protected routes are the ones a principal
// is required for; whoami is added so an operator can prove the credential
// works BEFORE the incident that needs it, without deciding anything.
//
// Everything else — and in particular /v1alpha1/inbound/poll and
// /v1alpha1/inbound/{id}/complete, which principalPolicy leaves to their own
// machine credential — is excluded, so a dialling bridge still makes exactly
// one admission call per request against its own rate window.
func breakGlassRoute(method, path string) bool {
	if path == "/v1alpha1/whoami" {
		return true
	}
	_, protected := principalPolicy(method, path)
	return protected
}

// breakGlassAdmit resolves an issued dial-in credential on the LAN listener.
//
// It returns handled=true when it has already answered the request: that is
// the case where the credential IS a real record and admission refused it,
// which is a classified refusal rather than an anonymous request. A bearer
// no record matches is not this function's business at all — it returns
// nothing and the caller's existing no_principal path answers, unchanged.
func (s *Server) breakGlassAdmit(w http.ResponseWriter, r *http.Request) (Principal, bool, bool) {
	presented := bearerValue(r)
	if !strings.HasPrefix(presented, actors.InboundCredentialPrefix) || s.inboundAuthenticator == nil || s.Store == nil {
		return Principal{}, false, false
	}
	partyKey, err := s.inboundCredentialParty(r.Context(), presented)
	if err != nil {
		s.writeAPIError(w, r, internalError(err))
		return Principal{}, false, true
	}
	if partyKey == "" {
		return Principal{}, false, false
	}
	// The presentation is verified where every other dial is: same store,
	// same lock, same order of checks, same counters.
	decision, err := s.inboundAuthenticator.Authenticate(r.Context(), "actor", partyKey, presented)
	if err != nil {
		s.writeAPIError(w, r, internalError(err))
		return Principal{}, false, true
	}
	if !decision.Allowed {
		s.refuse(w, r, http.StatusUnauthorized, inboundRefusalReason(decision.Reason), partyKey)
		return Principal{}, false, true
	}
	principal, err := s.inboundCredentialPrincipal(r.Context(), partyKey)
	if err != nil {
		s.writeAPIError(w, r, internalError(err))
		return Principal{}, false, true
	}
	return principal, true, false
}

// inboundCredentialParty answers which party a presentation belongs to, or
// "" for one no record matches.
//
// The digest comparison is the constant-time one actorbearer.go makes over
// actor rows, for the same reason: the set of candidates is small and known,
// and comparing in Go keeps the presented value out of a SQL argument (the
// posture migration 0031 and UpdateInboundAuthentication both hold).
func (s *Server) inboundCredentialParty(ctx context.Context, presented string) (string, error) {
	records, err := s.Store.ListInboundCredentialDigests(ctx, "actor")
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(presented))
	for _, record := range records {
		if len(record.Digest) != len(digest) {
			continue
		}
		if subtle.ConstantTimeCompare(record.Digest, digest[:]) == 1 {
			return record.PartyKey, nil
		}
	}
	return "", nil
}

// inboundCredentialPrincipal binds an admitted credential to the party's
// newest registered actor revision.
//
// A party with no registered actor produces a principal with no ActorID,
// which the middleware refuses as `unbound` — the same visible refusal a
// person who passed Access with no binding gets (spec c46), rather than a
// silent pass.
func (s *Server) inboundCredentialPrincipal(ctx context.Context, partyKey string) (Principal, error) {
	principal := Principal{Subject: partyKey, Provider: principalProviderInboundCredential}
	actorRows, err := s.engineStore.ListActors(ctx)
	if err != nil {
		return Principal{}, err
	}
	var newest *postgres.Actor
	for i := range actorRows {
		a := &actorRows[i]
		if a.ActorKey != partyKey {
			continue
		}
		if newest == nil || a.Revision > newest.Revision {
			newest = a
		}
	}
	if newest == nil {
		return principal, nil
	}
	principal.ActorID = newest.ID
	if newest.Kind != ledger.ActorKindHuman {
		// Not a person: this is the agent principal actorbearer.go already
		// defines, reached by a different credential. Same provider, same
		// viewer role, same narrow write surface, same agent/proposed stamp.
		principal.Provider = principalProviderActorToken
		principal.Roles = []auth.Role{auth.RoleViewer}
		return principal, nil
	}
	principal.Roles = []auth.Role{auth.RoleApprover}
	return principal, nil
}
