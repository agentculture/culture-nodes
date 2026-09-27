package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Task t38, acceptance 3: the flip is a human decision. Even with an agent
// principal already in the request context -- past whatever the middleware
// did -- the handler's own allow-list classification (declarationPrincipal)
// refuses it, as it refuses a synthetic transition-secret principal, and
// the switch does not move: the server here has no store at all, so a
// handler that got past the refusal to SwitchStore.Flip could not succeed.
func TestDeclarationSwitchRefusesAgentAndSyntheticPrincipals(t *testing.T) {
	srv := &Server{NamespaceID: "ns_switch", log: slog.Default()}
	for _, tc := range []struct {
		name string
		p    Principal
		want int
	}{
		{"agent bearer", Principal{Subject: "codex", Provider: principalProviderActorToken, ActorID: "company/codex"}, http.StatusForbidden},
		{"unknown provider fails closed to agent", Principal{Subject: "x", Provider: "some-future-provider", ActorID: "company/x"}, http.StatusForbidden},
		{"synthetic transition secret", Principal{Subject: "transition-bearer", Provider: "transition", Synthetic: true}, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1alpha1/declaration-engine/switch", bytes.NewReader([]byte(`{"mode":"before"}`)))
			r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, tc.p))
			rr := httptest.NewRecorder()
			srv.routes().ServeHTTP(rr, r)
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}
