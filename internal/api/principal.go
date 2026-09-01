package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/agentculture/culture-nodes/internal/auth"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

const accessAssertionHeader = "Cf-Access-Jwt-Assertion"

type principalContextKey struct{}

// Principal is the verified caller together with its live actor binding.
// ActorID and Roles are empty when Access verified the identity but no live
// binding exists. Synthetic is true only for a transition bearer secret.
type Principal struct {
	Subject    string
	Provider   string
	Email      string
	CommonName string
	ActorID    string
	Roles      []auth.Role
	Synthetic  bool
}

// PrincipalFromContext exposes the resolved principal to later tasks such as
// ledger-origin stamping without making handlers parse credentials again.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(Principal)
	return p, ok
}

type principalVerifier interface {
	Verify(context.Context, string) (auth.Principal, error)
}

// WithPrincipalVerifier enables the principal gate. Omitting it preserves the
// pre-Access listener behaviour, including existing transition bearer gates.
func WithPrincipalVerifier(verifier principalVerifier) Option {
	return func(s *Server) { s.principalVerifier = verifier }
}

// AccessHandler marks requests as arriving on the loopback Access listener.
// Only this handler considers Cf-Access-Jwt-Assertion; Handler deliberately
// ignores that header so a JWT observed on the LAN is useless there.
func (s *Server) AccessHandler() http.Handler {
	return s.principalMiddleware(true, s.accessRoutes())
}

func (s *Server) principalMiddleware(accessListener bool, next http.Handler) http.Handler {
	if s.principalVerifier == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p Principal
		var present bool
		if accessListener {
			assertion := r.Header.Get(accessAssertionHeader)
			if assertion != "" {
				verified, err := s.principalVerifier.Verify(r.Context(), assertion)
				if err != nil {
					reason := "malformed"
					var refusal *auth.VerificationError
					if errors.As(err, &refusal) {
						reason = refusal.Reason
					}
					s.refuse(w, r, http.StatusUnauthorized, reason, "")
					return
				}
				provider, subject := verified.BindingKey()
				p = Principal{Subject: subject, Provider: provider, Email: verified.Email, CommonName: verified.CommonName}
				identity, err := s.Store.LookupIdentity(r.Context(), s.NamespaceID, provider, subject)
				if err == nil {
					p.ActorID = identity.ActorID
					for _, raw := range identity.Roles {
						if role, e := auth.ParseRole(raw); e == nil {
							p.Roles = append(p.Roles, role)
						}
					}
				} else if !errors.Is(err, postgres.ErrIdentityNotFound) {
					s.writeAPIError(w, r, internalError(err))
					return
				}
				present = true
			}
		}

		policy, protected := principalPolicy(r.Method, r.URL.Path)
		if !present && protected {
			if secret := s.transitionSecret(policy.secret); secret != nil && bearerMatches(r, secret) {
				p = Principal{Subject: "transition-bearer", Provider: "transition", Roles: []auth.Role{auth.RoleNamespaceAdministrator}, Synthetic: true}
				present = true
			}
		}
		if present {
			r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, p))
		}
		if r.URL.Path == "/v1alpha1/whoami" && !present {
			s.refuse(w, r, http.StatusUnauthorized, "no_principal", "")
			return
		}
		if protected {
			if !present {
				s.refuse(w, r, http.StatusUnauthorized, "no_principal", "")
				return
			}
			if !p.Synthetic && p.ActorID == "" {
				s.refuse(w, r, http.StatusForbidden, "unbound", p.Subject)
				return
			}
			if !hasRole(p.Roles, policy.role) {
				s.refuse(w, r, http.StatusForbidden, "forbidden_role", p.Subject)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type routePolicy struct {
	role   auth.Role
	secret string
}

func principalPolicy(method, path string) (routePolicy, bool) {
	if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
		return routePolicy{}, false
	}
	// Machine-authenticated surfaces retain their dedicated credentials.
	if path == "/v1alpha1/events" || path == "/v1alpha1/webhooks/jira" || path == "/v1alpha1/inbound/poll" || (strings.HasPrefix(path, "/v1alpha1/inbound/") && strings.HasSuffix(path, "/complete")) || strings.Contains(path, "/attempts/") || strings.HasPrefix(path, "/callbacks/") {
		return routePolicy{}, false
	}
	if strings.HasSuffix(path, "/suite-verdicts") || strings.HasSuffix(path, "/gate-reports") {
		return routePolicy{}, false
	}
	p := routePolicy{role: auth.RoleNamespaceAdministrator}
	switch {
	case strings.Contains(path, "/human-tasks/") && strings.HasSuffix(path, "/decision"),
		strings.Contains(path, "/reviews"), strings.Contains(path, "/tickets/"), strings.HasSuffix(path, "/grades"):
		p.role, p.secret = auth.RoleApprover, "decision"
	case path == "/v1alpha1/actors" || strings.HasSuffix(path, "/resume"):
		p.secret = "actor"
	case path == "/v1alpha1/adhoc-runs":
		p.secret = "adhoc"
	case strings.HasPrefix(path, "/v1alpha1/store/"):
		p.secret = "store"
	case strings.HasPrefix(path, "/v1alpha1/inbound/credentials"):
		p.secret = "inbound"
	}
	return p, true
}

func hasRole(roles []auth.Role, required auth.Role) bool {
	for _, role := range roles {
		if role == auth.RoleNamespaceAdministrator || role == required || (required == auth.RoleViewer && role != "") {
			return true
		}
	}
	return false
}

func bearerMatches(r *http.Request, secret []byte) bool {
	const prefix = "bearer "
	h := r.Header.Get("Authorization")
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	presented, expected := sha256.Sum256([]byte(h[len(prefix):])), sha256.Sum256(secret)
	return subtle.ConstantTimeCompare(presented[:], expected[:]) == 1
}

func (s *Server) transitionSecret(kind string) []byte {
	switch kind {
	case "decision":
		return s.decisionAuthSecret
	case "actor":
		return s.actorRegistrationSecret
	case "adhoc":
		return s.adhocRunSecret
	case "store":
		return s.storeWriteSecret
	case "inbound":
		return s.inboundIssuanceSecret
	}
	return nil
}

func (s *Server) refuse(w http.ResponseWriter, r *http.Request, status int, reason, subject string) {
	s.log.Warn("principal refusal", "reason", reason, "subject", subject)
	if s.telemetry != nil {
		s.telemetry.RecordAuthRefusal(r.Context(), reason)
	}
	writeJSON(w, status, map[string]any{"code": 1, "message": "request refused", "remediation": "authenticate with a bound principal holding the required role", "reason": reason})
}

func (s *Server) requirePrincipalOrLegacy(r *http.Request, secret []byte) error {
	if _, ok := PrincipalFromContext(r.Context()); ok {
		return nil
	}
	if bearerMatches(r, secret) {
		return nil
	}
	return unauthorized("send valid credentials", "authorization failed")
}
