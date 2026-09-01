package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/auth"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	pgtest "github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

type verifierFunc func(context.Context, string) (auth.Principal, error)

func (f verifierFunc) Verify(ctx context.Context, token string) (auth.Principal, error) {
	return f(ctx, token)
}

func TestPrincipalGateRefusesNoPrincipalAndViewer(t *testing.T) {
	s := requireStore(t)
	nsID := pgtest.MustNamespace(t, s, "principal-gate").ID
	actorID := insertPrincipalTestActor(t, s, nsID, "principal-viewer")
	if _, err := s.BindIdentity(context.Background(), nsID, "cloudflare-access", "viewer-sub", actorID, []string{"viewer"}); err != nil {
		t.Fatal(err)
	}
	srv, err := api.NewServer(s, nsID, api.WithPrincipalVerifier(verifierFunc(func(_ context.Context, token string) (auth.Principal, error) {
		return auth.Principal{Subject: token, Email: "viewer@example.test", Kind: auth.PrincipalInteractive}, nil
	})))
	if err != nil {
		t.Fatal(err)
	}

	mutating := []struct{ method, path string }{
		{http.MethodPost, "/v1alpha1/workflows/validate"}, {http.MethodPost, "/v1alpha1/workflows"}, {http.MethodPost, "/v1alpha1/workflow-generations"},
		{http.MethodPost, "/v1alpha1/adhoc-runs"}, {http.MethodPost, "/v1alpha1/runs"}, {http.MethodPatch, "/v1alpha1/runs/run_x"}, {http.MethodPost, "/v1alpha1/runs/run_x/cancel"},
		{http.MethodPost, "/v1alpha1/tickets/SCRUM-1/frame"}, {http.MethodPost, "/v1alpha1/tickets/SCRUM-1/replies"}, {http.MethodPost, "/v1alpha1/tickets/SCRUM-1/freeze"},
		{http.MethodPost, "/v1alpha1/actors"}, {http.MethodPost, "/v1alpha1/actors/act_x/resume"}, {http.MethodPost, "/v1alpha1/inbound/credentials"}, {http.MethodPost, "/v1alpha1/inbound/credentials/revoke"},
		{http.MethodPost, "/v1alpha1/schedules"}, {http.MethodPatch, "/v1alpha1/schedules/sch_x"}, {http.MethodDelete, "/v1alpha1/schedules/sch_x"}, {http.MethodPost, "/v1alpha1/namespaces"},
		{http.MethodPost, "/v1alpha1/preflights/pre_x/acknowledge"}, {http.MethodPost, "/v1alpha1/store/entries"}, {http.MethodPost, "/v1alpha1/store/entries/pull"},
		{http.MethodPost, "/v1alpha1/store/entries/entry_x/bindings"}, {http.MethodPost, "/v1alpha1/store/entries/entry_x/publish"}, {http.MethodPost, "/v1alpha1/plan-imports"},
		{http.MethodPost, "/v1alpha1/runs/run_x/reviews"}, {http.MethodPost, "/v1alpha1/reviews/rev_x/commit"}, {http.MethodPost, "/v1alpha1/runs/run_x/grades"}, {http.MethodPost, "/v1alpha1/human-tasks/ht_x/decision"},
	}
	for _, route := range mutating {
		for _, tc := range []struct {
			name, token, reason string
			status              int
		}{
			{"no principal", "", "no_principal", http.StatusUnauthorized},
			{"viewer", "viewer-sub", "forbidden_role", http.StatusForbidden},
		} {
			t.Run(route.method+" "+route.path+"/"+tc.name, func(t *testing.T) {
				req := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
				if tc.token != "" {
					req.Header.Set("Cf-Access-Jwt-Assertion", tc.token)
				}
				rr := httptest.NewRecorder()
				srv.AccessHandler().ServeHTTP(rr, req)
				if rr.Code != tc.status {
					t.Fatalf("status %d, want %d: %s", rr.Code, tc.status, rr.Body.String())
				}
				if !strings.Contains(rr.Body.String(), tc.reason) {
					t.Fatalf("body %q lacks reason %q", rr.Body.String(), tc.reason)
				}
			})
		}
	}
}

func TestWhoamiBoundAndUnbound(t *testing.T) {
	s := requireStore(t)
	nsID := pgtest.MustNamespace(t, s, "principal-whoami").ID
	actorID := insertPrincipalTestActor(t, s, nsID, "principal-actor")
	if _, err := s.BindIdentity(context.Background(), nsID, "cloudflare-access", "bound", actorID, []string{"approver"}); err != nil {
		t.Fatal(err)
	}
	srv, err := api.NewServer(s, nsID, api.WithPrincipalVerifier(verifierFunc(func(_ context.Context, token string) (auth.Principal, error) {
		return auth.Principal{Subject: token, Email: token + "@example.test", Kind: auth.PrincipalInteractive}, nil
	})))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		token   string
		unbound bool
	}{{"bound", false}, {"new-person", true}} {
		req := httptest.NewRequest(http.MethodGet, "/v1alpha1/whoami", nil)
		req.Header.Set("Cf-Access-Jwt-Assertion", tc.token)
		rr := httptest.NewRecorder()
		srv.AccessHandler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.token, rr.Code, rr.Body.String())
		}
		var got map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if (got["unbound"] == true) != tc.unbound {
			t.Fatalf("%s: %#v", tc.token, got)
		}
	}
}

func TestBadAssertionLoggedOnceWithoutToken(t *testing.T) {
	s := requireStore(t)
	nsID := pgtest.MustNamespace(t, s, "principal-log").ID
	var logs bytes.Buffer
	srv, err := api.NewServer(s, nsID,
		api.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))),
		api.WithPrincipalVerifier(verifierFunc(func(context.Context, string) (auth.Principal, error) {
			return auth.Principal{}, &auth.VerificationError{Reason: "bad_signature"}
		})))
	if err != nil {
		t.Fatal(err)
	}
	token := "secret.assertion.must-not-appear"
	req := httptest.NewRequest(http.MethodGet, "/v1alpha1/actors", nil)
	req.Header.Set("Cf-Access-Jwt-Assertion", token)
	rr := httptest.NewRecorder()
	srv.AccessHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(logs.String(), token) {
		t.Fatalf("token leaked: %s", logs.String())
	}
	if strings.Count(strings.TrimSpace(logs.String()), "\n") != 0 || !strings.Contains(logs.String(), "bad_signature") {
		t.Fatalf("want one classified line: %q", logs.String())
	}
}

func TestPrincipalOverridesTicketFrameOriginAndWarns(t *testing.T) {
	s := requireStore(t)
	nsID := pgtest.MustNamespace(t, s, "principal-frame-origin").ID
	bound := insertPrincipalTestActor(t, s, nsID, "bound-frame-author")
	other := insertPrincipalTestActor(t, s, nsID, "body-frame-author")
	if _, err := s.BindIdentity(context.Background(), nsID, "cloudflare-access", "frame-sub", bound, []string{"approver"}); err != nil {
		t.Fatal(err)
	}
	srv, err := api.NewServer(s, nsID, api.WithPrincipalVerifier(verifierFunc(func(context.Context, string) (auth.Principal, error) {
		return auth.Principal{Subject: "frame-sub", Kind: auth.PrincipalInteractive}, nil
	})))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1alpha1/tickets/SCRUM-10/frame",
		strings.NewReader(`{"frame":{"state":"ready"},"posted_by":"`+other+`"}`))
	req.Header.Set("Cf-Access-Jwt-Assertion", "assertion")
	rr := httptest.NewRecorder()
	srv.AccessHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 override response: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"warning"`) || !strings.Contains(rr.Body.String(), other) {
		t.Fatalf("override response lacks warning naming supplied actor: %s", rr.Body.String())
	}
	var stored string
	if err := s.Pool().QueryRow(context.Background(), `SELECT posted_by FROM ticket_frames WHERE namespace_id=$1 AND ticket_id='SCRUM-10'`, nsID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != bound {
		t.Fatalf("stored posted_by = %q, want principal actor %q", stored, bound)
	}
}

func insertPrincipalTestActor(t *testing.T, s *postgres.Store, nsID, key string) string {
	t.Helper()
	id := store.NewULID()
	if _, err := s.Pool().Exec(context.Background(), `INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol) VALUES ($1,$2,$3,1,'human','http')`, id, nsID, key+"-"+id); err != nil {
		t.Fatal(err)
	}
	return id
}
