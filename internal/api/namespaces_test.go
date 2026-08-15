package api_test

import (
	"net/http"
	"testing"

	"github.com/agentculture/culture-nodes/internal/api"
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
