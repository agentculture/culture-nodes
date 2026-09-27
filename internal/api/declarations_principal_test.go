package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/declengine"
)

// The classifier is an allow-list: a synthetic transition principal is
// refused, an unknown provider is an agent, and only Access or a human
// break-glass credential is human.
func TestDeclarationPrincipalClassificationIsAnAllowList(t *testing.T) {
	cases := []struct {
		name      string
		p         Principal
		wantKind  declengine.PrincipalKind
		wantError bool
	}{
		{"access human", Principal{Subject: "s", Provider: "cloudflare-access", ActorID: "company/ori"}, declengine.PrincipalHuman, false},
		{"break-glass human", Principal{Subject: "ori", Provider: principalProviderInboundCredential, ActorID: "company/ori"}, declengine.PrincipalHuman, false},
		{"agent bearer", Principal{Subject: "codex", Provider: principalProviderActorToken, ActorID: "company/codex"}, declengine.PrincipalAgent, false},
		{"unknown provider fails closed to agent", Principal{Subject: "x", Provider: "some-future-provider", ActorID: "company/x"}, declengine.PrincipalAgent, false},
		{"synthetic transition secret refused", Principal{Subject: "transition-bearer", Provider: "transition", Synthetic: true}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1alpha1/declarations", nil)
			r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, tc.p))
			got, apiErr := declarationPrincipal(r)
			if tc.wantError {
				if apiErr == nil {
					t.Fatalf("got %+v, want a refusal", got)
				}
				return
			}
			if apiErr != nil || got.Kind != tc.wantKind {
				t.Fatalf("got %+v err=%v, want kind %s", got, apiErr, tc.wantKind)
			}
		})
	}
}

// TestWriteJSONWithWarningAnswers200 pins the shared convention the cortex
// review flagged as finding (d2): a response carrying a warning is 200 with
// the warning merged in, whatever status was passed. Declaration publish
// passes 200 on that path so the call site says what is sent.
func TestWriteJSONWithWarningAnswers200(t *testing.T) {
	rr := httptest.NewRecorder()
	writeJSONWithWarning(rr, http.StatusCreated, map[string]string{"id": "v1"}, "overlap report failed")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"warning":"overlap report failed"`) {
		t.Fatalf("body lost the warning: %s", rr.Body.String())
	}
}
