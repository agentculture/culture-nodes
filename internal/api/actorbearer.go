package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/agentculture/culture-nodes/internal/auth"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// An agent's own credential on the control plane (login-from-anywhere task
// t11, spec c45 / h31).
//
// Before this, the merge-gate scripts and the developer lane wrote suite
// verdicts, gate reports and ticket frames with the HUMAN decision secret —
// an agent writing with a person's credential, and a record that claimed
// human authority for what was an agent's say-so. Removing that secret from
// every hand (c29 f, task t18) breaks those lanes unless they get a
// principal of their own.
//
// They get the one they already have. Every registered agent actor's row
// names, in metadata.auth_token_env, the environment variable the worker
// reads its OUTBOUND credential from at dispatch time. The control plane
// reads the same variable and accepts the same value INBOUND: a bearer that
// matches a registered agent actor's token resolves to that actor. No new
// table, no new secret class, no new registration verb —
// deploy/prod/register-actor.sh already writes the row, and
// install-secrets.sh already installs the variable.
//
// What that principal may do is narrow on purpose. It opens exactly the
// routes an agent may write (agentMayWrite) and nothing else: a human
// decision under an agent bearer is refused by role, never admitted as the
// synthetic administrator the transition secrets confer. And what it writes
// is stamped as what it is — origin agent, authority proposed — because an
// agent may only propose (PRD §10.4). A gate result posted by the gate's own
// principal is the gate's claim about the suite it ran, not a validator's
// derived finding; the derived shape stays available to a validator posting
// under the decision bearer.

// principalProviderActorToken marks a Principal resolved from an agent
// actor's own bearer, as opposed to an Access assertion ("cloudflare-access")
// or a transition secret ("transition").
const principalProviderActorToken = "actor-token"

// WithActorTokenLookup replaces the environment lookup an agent bearer is
// resolved through. Tests inject a map; production keeps os.LookupEnv, the
// same source worker.DBRegistry reads the outbound credential from.
func WithActorTokenLookup(lookup func(string) (string, bool)) Option {
	return func(s *Server) {
		if lookup != nil {
			s.actorTokenLookup = lookup
		}
	}
}

// isActorBearer reports whether this principal is an agent actor
// authenticated by its own token.
func (p Principal) isActorBearer() bool {
	return p.Provider == principalProviderActorToken
}

// agentMayWrite lists the mutating routes a registered agent actor's own
// bearer opens. Everything else stays with the human/transition gate.
//
//   - suite-verdicts and gate-reports: the merge gate's records (task t11 of
//     issue #101), now the gate's own proposed claim.
//   - tickets/{id}/frame: the developer lane's devague frame snapshot
//     (examples/spec-chain-lane/post_frame.py), the lane's own proposal.
func agentMayWrite(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	if strings.HasSuffix(path, "/suite-verdicts") || strings.HasSuffix(path, "/gate-reports") {
		return true
	}
	return strings.Contains(path, "/tickets/") && strings.HasSuffix(path, "/frame")
}

// actorBearerPrincipal resolves the request's bearer to a registered agent
// actor, or reports that it matches none.
//
// The comparison walks every agent row whose metadata names a variable the
// control plane carries and compares SHA-256 digests in constant time, the
// same way the transition secrets are compared. Rows are append-only
// revisions, so several rows may name the same variable; the newest matching
// revision is the actor the record is attributed to.
func (s *Server) actorBearerPrincipal(ctx context.Context, r *http.Request) (Principal, bool, error) {
	const prefix = "bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) || s.actorTokenLookup == nil {
		return Principal{}, false, nil
	}
	presented := sha256.Sum256([]byte(h[len(prefix):]))

	actors, err := s.engineStore.ListActors(ctx)
	if err != nil {
		return Principal{}, false, err
	}
	var match *postgres.Actor
	for i := range actors {
		a := &actors[i]
		if a.Kind == ledger.ActorKindHuman {
			continue
		}
		envName := worker.AuthTokenEnvOf(a.Metadata)
		if envName == "" {
			continue
		}
		value, ok := s.actorTokenLookup(envName)
		if !ok || value == "" {
			continue
		}
		expected := sha256.Sum256([]byte(value))
		if subtle.ConstantTimeCompare(presented[:], expected[:]) != 1 {
			continue
		}
		if match == nil || a.Revision > match.Revision {
			match = a
		}
	}
	if match == nil {
		return Principal{}, false, nil
	}
	return Principal{
		Subject:  match.ActorKey,
		Provider: principalProviderActorToken,
		ActorID:  match.ID,
		Roles:    []auth.Role{auth.RoleViewer},
	}, true, nil
}

// stampAgentProposed rewrites a composed record's origin and authority when
// the request was authenticated by an agent actor's own bearer: the record
// becomes that agent's proposed claim. Any other principal leaves the
// composed shape untouched.
func stampAgentProposed(r *http.Request, rec ledger.Record) ledger.Record {
	p, ok := PrincipalFromContext(r.Context())
	if !ok || !p.isActorBearer() {
		return rec
	}
	rec.Origin.Kind = ledger.OriginAgent
	rec.Origin.ActorID = p.ActorID
	rec.Authority = ledger.AuthorityProposed
	return rec
}
