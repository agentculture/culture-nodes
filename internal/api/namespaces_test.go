package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/agentculture/culture-nodes/internal/api"
	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

func TestListNamespacesAndUnknownAPIRoute(t *testing.T) {
	f := newFixture(t)
	var got []api.NamespaceOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/namespaces"), nil, &got)
	requireStatus(t, resp, body, http.StatusOK)
	if len(got) == 0 || got[0].ID == "" || got[0].Slug == "" {
		t.Fatalf("GET namespaces returned no usable namespace: %#v", got)
	}
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/not-a-real-endpoint"), nil, nil)
	requireStatus(t, resp, body, http.StatusNotFound)
}

// TestSPAFallbackRefusesTheAPINamespace pins the defect issue #8 records, and
// it has to mount the web assets to see it. Every other test in this package
// builds a server WITHOUT WithWebAssets, and in that configuration an unknown
// /v1alpha1 path already 404s — so the assertion above passes whether or not
// the bug is fixed. Only the embedweb configuration, which is the one
// production runs, ever reached the SPA catch-all and answered 200 with
// index.html.
//
// The wrong-method half is the reason the guard lives in spaHandler rather
// than in a `mux.HandleFunc("/v1alpha1/", http.NotFound)` pattern: a
// method-less pattern that broad also matches DELETE on a real operation, and
// the mux then answers 404 where it used to answer 405 — which is exactly how
// TestOpenAPIRoutesAreServed detects that a documented route is served at all.
func TestSPAFallbackRefusesTheAPINamespace(t *testing.T) {
	s := requireStore(t)
	nsID := pgtest.MustNamespace(t, s, "api").ID
	assets := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<!doctype html><title>spa</title>")},
	}
	srv, err := apipkg.NewServer(s, nsID,
		apipkg.WithPollInterval(30*time.Millisecond),
		apipkg.WithWebAssets(assets),
	)
	if err != nil {
		t.Fatalf("api.NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// A client-side route still gets the SPA — that is what the fallback is for.
	resp, err := ts.Client().Get(ts.URL + "/runs/abc")
	if err != nil {
		t.Fatalf("GET /runs/abc: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /runs/abc status = %d, want 200 (the SPA fallback must still work)", resp.StatusCode)
	}

	// An undeclared API path must not.
	resp, err = ts.Client().Get(ts.URL + "/v1alpha1/pending-decisions-that-do-not-exist")
	if err != nil {
		t.Fatalf("GET unknown api path: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET undeclared /v1alpha1 path status = %d, want 404 — "+
			"a 200 here is index.html, and issue #8 is that a client cannot tell "+
			"an absent endpoint from an empty one", resp.StatusCode)
	}

	// And a DECLARED operation asked with the wrong method must still refuse
	// with 405, not be swallowed into the catch-all as a 404.
	req, err := http.NewRequest(http.MethodDelete, ts.URL+"/v1alpha1/namespaces", nil)
	if err != nil {
		t.Fatalf("build DELETE: %v", err)
	}
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatalf("DELETE /v1alpha1/namespaces: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /v1alpha1/namespaces status = %d, want 405 — "+
			"the route sweep uses this signal to prove a documented route is served", resp.StatusCode)
	}
}
