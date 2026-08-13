package api_test

// The t15 auth-hardening gate (spec c27, honesty h22): every mutating
// surface the attempts-evidence-humans-loops batch ADDED must refuse
// unauthenticated requests, and the phase-1 authless posture must survive
// unchanged on the read-only surfaces. This is a table-driven negative
// gate: it enumerates the batch's new mutating routes against a server
// configured with all four bearer secrets and asserts 401 for (a) no
// Authorization header and (b) a wrong bearer token — and asserts a
// representative read-only listing stays reachable with no token at all.
//
// The human-task decision route predates the batch but shares the same
// bearer pattern, so it rides in the same table as the reference row.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/agentculture/culture-nodes/internal/api"
	pgtest "github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

func TestBatchMutatingRoutesRefuseUnauthenticated(t *testing.T) {
	s := requireStore(t)
	nsID := pgtest.MustNamespace(t, s, "authgate").ID

	srv, err := api.NewServer(s, nsID,
		api.WithDecisionAuthSecret("decision-secret-long-enough-000"),
		api.WithActorRegistrationSecret("registration-secret-long-enough"),
		api.WithEventTokenSecret("event-secret-long-enough-000000"),
		api.WithAdhocRunSecret("adhoc-secret-long-enough-000000"),
	)
	if err != nil {
		t.Fatalf("api.NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	mutating := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"actor registration (t13)", http.MethodPost, "/v1alpha1/actors",
			`{"actor_key":"company/x","kind":"agent","protocol":"http"}`},
		{"event delivery (t10)", http.MethodPost, "/v1alpha1/events",
			`{"name":"x","payload":{}}`},
		{"ad-hoc run (t19)", http.MethodPost, "/v1alpha1/adhoc-runs",
			`{"instruction":"x","actor_ref":"actor://company/x@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repo":"/tmp/x"}`},
		{"human decision (pre-batch reference)", http.MethodPost, "/v1alpha1/human-tasks/ht_x/decision",
			`{"outcome":"approved"}`},
		// Later addition, same posture: clearing a capacity pause (t9)
		// restores exactly the standing registration granted, so it rides
		// the registration secret and belongs in this gate. The 401 must
		// come BEFORE the actor lookup — an unauthenticated caller learns
		// nothing about which actor ids exist.
		{"resume actor / clear capacity pause (t9)", http.MethodPost, "/v1alpha1/actors/act_x/resume", `{}`},
	}

	for _, route := range mutating {
		t.Run(route.name+"/no token", func(t *testing.T) {
			req, _ := http.NewRequest(route.method, ts.URL+route.path, strings.NewReader(route.body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s %s without a token: got %d, want 401",
					route.method, route.path, resp.StatusCode)
			}
		})
		t.Run(route.name+"/wrong token", func(t *testing.T) {
			req, _ := http.NewRequest(route.method, ts.URL+route.path, strings.NewReader(route.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer not-the-configured-secret")
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s %s with a wrong token: got %d, want 401",
					route.method, route.path, resp.StatusCode)
			}
		})
	}

	// The phase-1 posture survives: a representative read-only listing
	// answers without any Authorization header (200, not 401).
	t.Run("read-only listing stays authless", func(t *testing.T) {
		resp, err := ts.Client().Get(ts.URL + "/v1alpha1/actors")
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /v1alpha1/actors without a token: got %d, want 200", resp.StatusCode)
		}
	})
}

// TestAdhocRefusedWhenNoSecretConfigured pins the closed-by-default rule for
// the one route this gate had to harden: a server with NO ad-hoc secret
// refuses the lane entirely rather than mounting it authless.
func TestAdhocRefusedWhenNoSecretConfigured(t *testing.T) {
	s := requireStore(t)
	nsID := pgtest.MustNamespace(t, s, "authgate-noscrt").ID

	srv, err := api.NewServer(s, nsID)
	if err != nil {
		t.Fatalf("api.NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1alpha1/adhoc-runs",
		strings.NewReader(`{"instruction":"x"}`))
	req.Header.Set("Authorization", "Bearer anything")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("adhoc with no configured secret: got %d, want 401", resp.StatusCode)
	}
}
