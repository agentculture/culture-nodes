package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// The actor callback route (POST /v1/attempts/{id}/events) is mounted by
// (*api.Server).Handler only when the server was built WithCallbackSigner
// (internal/api/server.go). These tests prove the route is wired to a real
// mux with a real token signer and a real store, end to end over HTTP --
// the deep protocol semantics (dedup, ordering, late completion, ...) are
// already covered by internal/actors/callback_test.go and
// internal/worker's own suite; this package only needs to prove the wiring
// itself, not re-litigate the protocol.
const callbackTestSecret = "0123456789abcdef0123456789abcdef"

// newCallbackFixture is a variant of newFixture (fixture_test.go) that also
// wires a callback signer, so it is a separate helper rather than an
// optional argument on the widely-used newFixture.
func newCallbackFixture(t *testing.T, signer *actors.TokenSigner) *fixture {
	t.Helper()
	s := requireStore(t)

	nsID := pgtest.MustNamespace(t, s, "api-callback").ID
	opts := []api.Option{api.WithPollInterval(30 * time.Millisecond)}
	if signer != nil {
		opts = append(opts, api.WithCallbackSigner(signer))
	}
	srv, err := api.NewServer(s, nsID, opts...)
	if err != nil {
		t.Fatalf("api.NewServer: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &fixture{t: t, server: ts, api: srv, store: s, nsID: nsID, client: ts.Client()}
}

func TestCallbackRoute_NotMountedWithoutSigner(t *testing.T) {
	f := newCallbackFixture(t, nil)

	req, err := http.NewRequest(http.MethodPost, f.url("/v1/attempts/att_whatever/events"), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer not-even-checked")
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatalf("POST callback route: %v", err)
	}
	defer resp.Body.Close()

	// No signer means Handler never mounted the pattern at all, so the mux
	// itself reports 404 -- proof this is "the route does not exist", not
	// "the route exists and rejected the request" (that path is exercised
	// by the tests below).
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d (404: route must be absent without a callback signer)", resp.StatusCode, http.StatusNotFound)
	}
}

func TestCallbackRoute_RejectsMissingOrBadToken(t *testing.T) {
	signer, err := actors.NewTokenSigner([]byte(callbackTestSecret))
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}
	f := newCallbackFixture(t, signer)

	body := `{"event_id":"ev1","sequence":1,"kind":"heartbeat"}`

	t.Run("missing_authorization_header", func(t *testing.T) {
		resp, respBody := doJSON(t, f.client, http.MethodPost, f.url("/v1/attempts/att_missing/events"), nil, nil)
		_ = respBody
		requireStatus(t, resp, respBody, http.StatusUnauthorized)
	})

	t.Run("garbage_bearer_token", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, f.url("/v1/attempts/att_bad/events"), strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer this-is-not-a-valid-token")
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatalf("POST callback route: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d (401 for a malformed token)", resp.StatusCode, http.StatusUnauthorized)
		}
	})

	t.Run("token_signed_by_a_different_secret", func(t *testing.T) {
		other, err := actors.NewTokenSigner([]byte("ffffffffffffffffffffffffffffffff"))
		if err != nil {
			t.Fatalf("NewTokenSigner: %v", err)
		}
		token, err := other.Mint("att_foreign_secret")
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		req, err := http.NewRequest(http.MethodPost, f.url("/v1/attempts/att_foreign_secret/events"), strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatalf("POST callback route: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d (401: this server's signer must reject a foreign secret's token)", resp.StatusCode, http.StatusUnauthorized)
		}
	})
}

// TestCallbackRoute_UnknownAttempt404s mints a genuinely valid token (the
// same signer the mounted route verifies against) for an attempt id that
// has no durable invocation, and checks the ingest reaches
// actors.ErrUnknownAttempt -- proof that Store, Engine, and Signer are all
// threaded through Handler into a real actors.CallbackHandler, not just
// that the route exists.
func TestCallbackRoute_UnknownAttempt404s(t *testing.T) {
	signer, err := actors.NewTokenSigner([]byte(callbackTestSecret))
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}
	f := newCallbackFixture(t, signer)

	attemptID := "att_" + store.NewULID()
	token, err := signer.Mint(attemptID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, f.url("/v1/attempts/"+attemptID+"/events"),
		strings.NewReader(`{"event_id":"ev1","sequence":1,"kind":"heartbeat"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatalf("POST callback route: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d (404: no invocation exists for this attempt)", resp.StatusCode, http.StatusNotFound)
	}
}
