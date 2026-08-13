package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// actorRegistrationSecret is a fixed, sufficiently long test secret — like
// humantasks_test.go's decisionAuthSecret, length is all
// api.WithActorRegistrationSecret cares about; it is not a production value.
const actorRegistrationSecret = "test-only-actor-registration-secret-not-for-production"

// newFixtureWithActorRegistrationAuth mirrors humantasks_test.go's
// newFixtureWithDecisionAuth: one server carrying an actor-registration
// bearer secret, so the tests in this file can exercise both "wrong/missing
// credentials refused" and "correct credentials accepted" against it.
// newFixture's own default (no secret configured at all) is what
// TestRegisterActorRefusedWhenNoSecretConfigured below exercises.
func newFixtureWithActorRegistrationAuth(t *testing.T, secret string) *fixture {
	t.Helper()
	s := requireStore(t)

	nsID := pgtest.MustNamespace(t, s, "api").ID
	srv, err := apipkg.NewServer(s, nsID,
		apipkg.WithPollInterval(30*time.Millisecond),
		apipkg.WithActorRegistrationSecret(secret),
	)
	if err != nil {
		t.Fatalf("api.NewServer: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &fixture{t: t, server: ts, api: srv, store: s, nsID: nsID, client: ts.Client()}
}

// registerActorReq is the wire shape POST /v1alpha1/actors accepts
// (components.schemas.RegisterActorRequest).
type registerActorReq struct {
	Namespace    string          `json:"namespace,omitempty"`
	ActorKey     string          `json:"actor_key,omitempty"`
	Kind         string          `json:"kind,omitempty"`
	Protocol     string          `json:"protocol,omitempty"`
	EndpointRef  string          `json:"endpoint_ref,omitempty"`
	Capabilities json.RawMessage `json:"capabilities,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
}

// authedRegisterActor sends the registration request with the given bearer
// token (empty means no Authorization header at all) — the same shape
// humantasks_test.go's authedDecide uses.
func authedRegisterActor(t *testing.T, f *fixture, bearer string, req registerActorReq) (*http.Response, []byte) {
	t.Helper()

	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal registration request: %v", err)
	}
	httpReq, err := http.NewRequest(http.MethodPost, f.url("/v1alpha1/actors"), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		httpReq.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := f.client.Do(httpReq)
	if err != nil {
		t.Fatalf("POST /v1alpha1/actors: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp, data
}

// listActorRowsByKey fetches GET /v1alpha1/actors and returns every row
// whose actor_key matches, in list order (actor_key then revision).
func listActorRowsByKey(t *testing.T, f *fixture, actorKey string) []apipkg.ActorOut {
	t.Helper()
	var list apipkg.ActorListOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/actors"), nil, &list)
	requireStatus(t, resp, body, http.StatusOK)
	var out []apipkg.ActorOut
	for _, a := range list.Items {
		if a.ActorKey == actorKey {
			out = append(out, a)
		}
	}
	return out
}

// TestRegisterActorCreatesRevisionReadableViaGet is t13 acceptance criterion
// 1: an authorized registration request creates a new actor revision that
// the read surface (GET /v1alpha1/actors, GET /v1alpha1/actors/{id}) renders.
func TestRegisterActorCreatesRevisionReadableViaGet(t *testing.T) {
	f := newFixtureWithActorRegistrationAuth(t, actorRegistrationSecret)

	req := registerActorReq{
		ActorKey:     "codex-newbox",
		Kind:         "agent",
		Protocol:     "http",
		EndpointRef:  "https://newbox.example/actor",
		Capabilities: json.RawMessage(`{"review": true}`),
		Metadata:     json.RawMessage(`{"auth_token_env": "NEWBOX_TOKEN"}`),
	}
	resp, body := authedRegisterActor(t, f, actorRegistrationSecret, req)
	requireStatus(t, resp, body, http.StatusCreated)

	var created apipkg.ActorOut
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created actor: %v (%s)", err, body)
	}
	if created.ID == "" {
		t.Fatal("created actor has no id")
	}
	if created.ActorKey != req.ActorKey {
		t.Errorf("actor_key = %q, want %q", created.ActorKey, req.ActorKey)
	}
	if created.Revision != 1 {
		t.Errorf("revision = %d, want 1 (first registration of this key)", created.Revision)
	}
	if created.Kind != "agent" || created.Protocol != "http" {
		t.Errorf("kind/protocol = %s/%s, want agent/http", created.Kind, created.Protocol)
	}
	if created.EndpointRef != req.EndpointRef {
		t.Errorf("endpoint_ref = %q, want %q", created.EndpointRef, req.EndpointRef)
	}
	if created.CreatedAt.IsZero() {
		t.Error("created_at is zero")
	}

	// The new revision is readable via the list surface...
	rows := listActorRowsByKey(t, f, req.ActorKey)
	if len(rows) != 1 || rows[0].ID != created.ID {
		t.Fatalf("list rows for %q = %+v, want exactly the created row %s", req.ActorKey, rows, created.ID)
	}

	// ...and via GET /v1alpha1/actors/{id}, carrying the registered
	// capabilities/metadata verbatim (semantic JSON equality — key order is
	// not part of the contract).
	var fetched apipkg.ActorOut
	resp2, body2 := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/actors/"+created.ID), nil, &fetched)
	requireStatus(t, resp2, body2, http.StatusOK)
	requireJSONEqual(t, "capabilities", fetched.Capabilities, req.Capabilities)
	requireJSONEqual(t, "metadata", fetched.Metadata, req.Metadata)
}

func requireJSONEqual(t *testing.T, what string, got, want json.RawMessage) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s: decode got %s: %v", what, got, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("%s: decode want %s: %v", what, want, err)
	}
	gj, _ := json.Marshal(g)
	wj, _ := json.Marshal(w)
	if !bytes.Equal(gj, wj) {
		t.Fatalf("%s = %s, want %s", what, gj, wj)
	}
}

// TestRegisterActorAppendsRevisionNeverUpdates is t13 acceptance criterion
// 3: re-registering an existing actor_key INSERTs the next revision row —
// deploy/prod/register-actor.sh:147's append-only semantics — verified by
// listing both revisions side by side.
func TestRegisterActorAppendsRevisionNeverUpdates(t *testing.T) {
	f := newFixtureWithActorRegistrationAuth(t, actorRegistrationSecret)

	first := registerActorReq{
		ActorKey:    "codex-rotating",
		Kind:        "agent",
		Protocol:    "http",
		EndpointRef: "https://old.example/actor",
	}
	resp, body := authedRegisterActor(t, f, actorRegistrationSecret, first)
	requireStatus(t, resp, body, http.StatusCreated)
	var rev1 apipkg.ActorOut
	if err := json.Unmarshal(body, &rev1); err != nil {
		t.Fatalf("decode revision 1: %v (%s)", err, body)
	}

	second := first
	second.EndpointRef = "https://new.example/actor"
	resp, body = authedRegisterActor(t, f, actorRegistrationSecret, second)
	requireStatus(t, resp, body, http.StatusCreated)
	var rev2 apipkg.ActorOut
	if err := json.Unmarshal(body, &rev2); err != nil {
		t.Fatalf("decode revision 2: %v (%s)", err, body)
	}

	if rev2.ID == rev1.ID {
		t.Fatalf("second registration returned the same row id %s — updated in place instead of appending", rev1.ID)
	}
	if rev2.Revision != rev1.Revision+1 {
		t.Fatalf("second registration revision = %d, want %d", rev2.Revision, rev1.Revision+1)
	}

	// Both revisions remain listed: the first row was appended past, never
	// mutated — its endpoint_ref still reads what revision 1 registered.
	rows := listActorRowsByKey(t, f, first.ActorKey)
	if len(rows) != 2 {
		t.Fatalf("list rows for %q = %+v, want both revisions", first.ActorKey, rows)
	}
	if rows[0].Revision != 1 || rows[0].EndpointRef != first.EndpointRef {
		t.Errorf("revision 1 row = %+v, want revision=1 endpoint_ref=%q untouched", rows[0], first.EndpointRef)
	}
	if rows[1].Revision != 2 || rows[1].EndpointRef != second.EndpointRef {
		t.Errorf("revision 2 row = %+v, want revision=2 endpoint_ref=%q", rows[1], second.EndpointRef)
	}
}

// TestRegisterActorRefusedWithoutValidToken is t13 acceptance criterion 2:
// an unauthenticated request and a wrong-token request are both refused with
// 401 in the documented Error shape, and neither writes a row.
func TestRegisterActorRefusedWithoutValidToken(t *testing.T) {
	f := newFixtureWithActorRegistrationAuth(t, actorRegistrationSecret)
	req := registerActorReq{ActorKey: "codex-intruder", Kind: "agent", Protocol: "http"}

	// No Authorization header at all.
	resp, body := authedRegisterActor(t, f, "", req)
	requireStatus(t, resp, body, http.StatusUnauthorized)
	decodeAPIError(t, body)

	// Wrong bearer token.
	resp, body = authedRegisterActor(t, f, "not-the-configured-secret", req)
	requireStatus(t, resp, body, http.StatusUnauthorized)
	decodeAPIError(t, body)

	// Neither refused attempt registered anything.
	if rows := listActorRowsByKey(t, f, req.ActorKey); len(rows) != 0 {
		t.Fatalf("refused registrations still wrote rows: %+v", rows)
	}
}

// TestRegisterActorRefusedWhenNoSecretConfigured proves a server with no
// registration secret configured at all refuses every registration — closed
// by default, never mounted-but-authless (the same posture the human-task
// decision endpoint takes).
func TestRegisterActorRefusedWhenNoSecretConfigured(t *testing.T) {
	f := newFixture(t)
	resp, body := authedRegisterActor(t, f, actorRegistrationSecret,
		registerActorReq{ActorKey: "codex-nosecret", Kind: "agent", Protocol: "http"})
	requireStatus(t, resp, body, http.StatusUnauthorized)
	decodeAPIError(t, body)
}

// TestRegisterActorValidation exercises the 400 lane: required fields
// (actor_key, kind, protocol) must be present, and a namespace naming
// anything other than this server's own namespace is refused rather than
// silently rerouted (the API is bound to one namespace at construction).
func TestRegisterActorValidation(t *testing.T) {
	f := newFixtureWithActorRegistrationAuth(t, actorRegistrationSecret)

	cases := []struct {
		name string
		req  registerActorReq
	}{
		{"missing_actor_key", registerActorReq{Kind: "agent", Protocol: "http"}},
		{"missing_kind", registerActorReq{ActorKey: "k", Protocol: "http"}},
		{"missing_protocol", registerActorReq{ActorKey: "k", Kind: "agent"}},
		{"wrong_namespace", registerActorReq{Namespace: "not-this-namespace", ActorKey: "k", Kind: "agent", Protocol: "http"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := authedRegisterActor(t, f, actorRegistrationSecret, tc.req)
			requireStatus(t, resp, body, http.StatusBadRequest)
			decodeAPIError(t, body)
		})
	}

	// The server's own namespace id, spelled explicitly, is accepted.
	resp, body := authedRegisterActor(t, f, actorRegistrationSecret,
		registerActorReq{Namespace: f.nsID, ActorKey: "codex-explicit-ns", Kind: "agent", Protocol: "http"})
	requireStatus(t, resp, body, http.StatusCreated)
}
