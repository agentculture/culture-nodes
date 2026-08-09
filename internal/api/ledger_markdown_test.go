package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// TestGetLedgerProjectionMarkdownFormat proves the projection endpoint can
// render the same projection two ways: the default (and only previously
// existing) JSON body, and — with ?format=markdown — the deterministic
// Markdown reflection internal/ledger's Projection.Markdown renders (PRD
// §10.9, docs/acceptance.md criterion 9). The Markdown is never a second
// source of truth: it is requested through the identical read path as the
// JSON, over the identical committed ledger state.
func TestGetLedgerProjectionMarkdownFormat(t *testing.T) {
	f := newFixture(t)
	source := readFixtureWorkflow(t, "edge-order-ordered.workflow.yaml")

	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(source)}, &published)
	requireStatus(t, resp, body, http.StatusCreated)

	var run apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{"subject":"t17"}`)}, &run)
	requireStatus(t, resp, body, http.StatusCreated)

	actorID := f.insertActor("worker")
	if _, err := f.api.Ledger.Append(t.Context(), ledger.Record{
		RecordType: ledger.RecordAnnouncement,
		RunID:      run.ID,
		Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: actorID},
		Authority:  ledger.AuthorityProposed,
		Data:       json.RawMessage(`{"statement":"deliver the markdown format"}`),
	}); err != nil {
		t.Fatalf("append announcement: %v", err)
	}

	// The default, still-JSON response is unchanged.
	var jsonProjection ledger.Projection
	resp, body = doJSON(t, f.client, http.MethodGet,
		f.url("/v1alpha1/runs/"+run.ID+"/ledger/projections/current_scope"), nil, &jsonProjection)
	requireStatus(t, resp, body, http.StatusOK)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("default Content-Type = %q, want application/json", ct)
	}
	if len(jsonProjection.Items) != 1 {
		t.Fatalf("current_scope projection items = %+v, want exactly one", jsonProjection.Items)
	}

	// ?format=markdown renders the identical projection as Markdown.
	resp, mdBody := doJSON(t, f.client, http.MethodGet,
		f.url("/v1alpha1/runs/"+run.ID+"/ledger/projections/current_scope?format=markdown"), nil, nil)
	requireStatus(t, resp, mdBody, http.StatusOK)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("markdown Content-Type = %q, want text/markdown", ct)
	}
	md := string(mdBody)
	if !strings.HasPrefix(md, "# ") {
		t.Errorf("markdown body does not start with a heading:\n%s", md)
	}
	if !strings.Contains(md, jsonProjection.Digest) {
		t.Errorf("markdown body does not carry the same digest %q as the JSON projection:\n%s", jsonProjection.Digest, md)
	}
	if !strings.Contains(md, "not authoritative") {
		t.Errorf("markdown body does not state the PRD §10.9 non-authoritative rule:\n%s", md)
	}

	// An unrecognized format value is a documented 400, in the same shape
	// every other bad-request refusal in this API uses.
	resp, body = doJSON(t, f.client, http.MethodGet,
		f.url("/v1alpha1/runs/"+run.ID+"/ledger/projections/current_scope?format=yaml"), nil, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)
}
