package api_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

// The flow-store registry surface (plan task t7, issue #192): a catalog
// entry is a graph (content digest + embedded source, verbatim) PLUS its
// evidence manifest (proving prod run ids, deviation record refs, required
// actor/runner capabilities). These tests pin the #192 acceptance:
//
//	(a) the registry lists entries with graph digest and evidence
//	    manifest, and an entry created from a proven flow carries its run
//	    ids and deviation records verbatim;
//	(b) local additions coexist with pulled entries — pulling never
//	    overwrites or shadows a locally-authored flow;
//
// plus the write-auth gate (every mutating surface ships authenticated —
// the t15 rule, spec c27) and the pull path's integrity checks.

// fixtureStoreSecret is the bearer secret newFixture configures for the
// store's two write routes.
const fixtureStoreSecret = "fixture-store-secret-long-enough"

// Wire mirrors of what api/openapi/openapi.yaml documents (this package's
// convention: encode what the spec says, never reach for internal types).

type storeCapabilityReq struct {
	Kind         string   `json:"kind"`
	Ref          string   `json:"ref"`
	Capabilities []string `json:"capabilities"`
}

type storeDeviationRef struct {
	Ref  string `json:"ref"`
	Note string `json:"note,omitempty"`
}

type storeEvidenceReq struct {
	ProvingRunIDs        []string             `json:"proving_run_ids"`
	DeviationRecords     []storeDeviationRef  `json:"deviation_records"`
	RequiredCapabilities []storeCapabilityReq `json:"required_capabilities"`
}

type storeEntryCreateReq struct {
	Name        string           `json:"name,omitempty"`
	GraphDigest string           `json:"graph_digest"`
	Evidence    storeEvidenceReq `json:"evidence"`
}

type storeEntryGraphResp struct {
	Digest       string `json:"digest"`
	SourceFormat string `json:"source_format"`
	Source       string `json:"source,omitempty"`
}

type storeEntryResp struct {
	ID             string              `json:"id"`
	Name           string              `json:"name"`
	Origin         string              `json:"origin"`
	SourceRegistry string              `json:"source_registry,omitempty"`
	Graph          storeEntryGraphResp `json:"graph"`
	Evidence       storeEvidenceReq    `json:"evidence"`
	EntryDigest    string              `json:"entry_digest"`
	CreatedAt      string              `json:"created_at"`
}

type storeEntryListResp struct {
	Items []storeEntryResp `json:"items"`
}

type storeEntryPullReq struct {
	SourceRegistry string          `json:"source_registry"`
	Entry          json.RawMessage `json:"entry"`
}

type runIDResp struct {
	ID string `json:"id"`
}

// publishAndRun publishes the edge-order fixture workflow and creates one
// run of it, returning the published digest+source and the run id — the
// "proven prod flow" every store-entry test starts from.
func publishAndRun(t *testing.T, f *fixture) (digest, source, runID string) {
	t.Helper()
	src := string(readFixtureWorkflow(t, "edge-order-ordered.workflow.yaml"))

	var published struct {
		Digest string `json:"digest"`
		Source string `json:"source"`
	}
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: src}, &published)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("publish workflow: status = %d; body = %s", resp.StatusCode, body)
	}

	var run runIDResp
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{}`)}, &run)
	requireStatus(t, resp, body, http.StatusCreated)
	return published.Digest, published.Source, run.ID
}

// sampleEvidence builds the manifest every test entry proves itself with.
func sampleEvidence(runIDs ...string) storeEvidenceReq {
	return storeEvidenceReq{
		ProvingRunIDs: runIDs,
		DeviationRecords: []storeDeviationRef{
			{Ref: "docs/deviations/2026-08-18-example.md", Note: "recorded against the proving run"},
		},
		RequiredCapabilities: []storeCapabilityReq{
			{Kind: "actor", Ref: "actor://codex@sha256:bbbb", Capabilities: []string{"shell"}},
		},
	}
}

func TestStoreEntryWriteAuth(t *testing.T) {
	f := newFixture(t)

	for _, path := range []string{"/v1alpha1/store/entries", "/v1alpha1/store/entries/pull"} {
		// No token at all.
		resp, body := doJSON(t, f.client, http.MethodPost, f.url(path),
			map[string]string{}, nil)
		requireStatus(t, resp, body, http.StatusUnauthorized)
		decodeAPIError(t, body)

		// A wrong token.
		resp, body = doJSONBearer(t, f.client, http.MethodPost, f.url(path), "not-the-secret",
			map[string]string{}, nil)
		requireStatus(t, resp, body, http.StatusUnauthorized)
		decodeAPIError(t, body)
	}
}

// TestStoreEntryLifecycle is acceptance (a): create an entry from a proven
// flow, see its run ids and deviation records come back verbatim, list it
// with graph digest + evidence manifest, and fetch the full self-contained
// document.
func TestStoreEntryLifecycle(t *testing.T) {
	f := newFixture(t)
	digest, source, runID := publishAndRun(t, f)
	evidence := sampleEvidence(runID)

	var created storeEntryResp
	resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/store/entries"), fixtureStoreSecret,
		storeEntryCreateReq{GraphDigest: digest, Evidence: evidence}, &created)
	requireStatus(t, resp, body, http.StatusCreated)

	if created.Origin != "local" {
		t.Fatalf("origin = %q, want local", created.Origin)
	}
	if created.Name != "edge-order" {
		t.Fatalf("name = %q, want the workflow key %q", created.Name, "edge-order")
	}
	if created.Graph.Digest != digest {
		t.Fatalf("graph digest = %q, want %q", created.Graph.Digest, digest)
	}
	if created.EntryDigest == "" {
		t.Fatal("entry_digest is empty")
	}
	// Verbatim evidence: exactly the run ids and deviation records supplied.
	if !reflect.DeepEqual(created.Evidence, evidence) {
		t.Fatalf("evidence not verbatim:\n got  %+v\n want %+v", created.Evidence, evidence)
	}

	// Re-adding identical content is idempotent (200, same entry).
	var again storeEntryResp
	resp, body = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/store/entries"), fixtureStoreSecret,
		storeEntryCreateReq{GraphDigest: digest, Evidence: evidence}, &again)
	requireStatus(t, resp, body, http.StatusOK)
	if again.ID != created.ID {
		t.Fatalf("re-adding identical content produced a new entry: %s vs %s", again.ID, created.ID)
	}

	// The listing carries graph digest + the evidence manifest.
	var list storeEntryListResp
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/store/entries"), nil, &list)
	requireStatus(t, resp, body, http.StatusOK)
	var listed *storeEntryResp
	for i := range list.Items {
		if list.Items[i].ID == created.ID {
			listed = &list.Items[i]
		}
	}
	if listed == nil {
		t.Fatalf("created entry %s not in listing %+v", created.ID, list.Items)
	}
	if listed.Graph.Digest != digest || !reflect.DeepEqual(listed.Evidence, evidence) {
		t.Fatalf("listing lacks digest or verbatim evidence: %+v", listed)
	}

	// The full document is self-contained: the graph source rides verbatim.
	var fetched storeEntryResp
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/store/entries/"+created.ID), nil, &fetched)
	requireStatus(t, resp, body, http.StatusOK)
	if fetched.Graph.Source != source {
		t.Fatalf("entry does not embed the workflow source verbatim:\n got  %q\n want %q", fetched.Graph.Source, source)
	}

	// An unknown id is a documented 404.
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/store/entries/01JUNKJUNKJUNKJUNKJUNKJUNK"), nil, nil)
	requireStatus(t, resp, body, http.StatusNotFound)
	decodeAPIError(t, body)
}

func TestStoreEntryCreateRejects(t *testing.T) {
	f := newFixture(t)
	digest, _, runID := publishAndRun(t, f)

	post := func(req storeEntryCreateReq) (*http.Response, []byte) {
		return doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/store/entries"), fixtureStoreSecret, req, nil)
	}

	// An unpublished graph digest cannot be entered: there is nothing to
	// embed and nothing it could have proven here.
	resp, body := post(storeEntryCreateReq{
		GraphDigest: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		Evidence:    sampleEvidence(runID)})
	requireStatus(t, resp, body, http.StatusNotFound)
	decodeAPIError(t, body)

	// No proving runs, no entry — the store is for proven flows.
	resp, body = post(storeEntryCreateReq{GraphDigest: digest, Evidence: sampleEvidence()})
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)

	// A proving run id must be a run this control plane actually holds.
	resp, body = post(storeEntryCreateReq{GraphDigest: digest, Evidence: sampleEvidence("01NOTAREALRUNAAAAAAAAAAAAA")})
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)

	// Capability requirements are typed: kind must be actor or runner.
	bad := sampleEvidence(runID)
	bad.RequiredCapabilities = []storeCapabilityReq{{Kind: "person", Ref: "actor://x"}}
	resp, body = post(storeEntryCreateReq{GraphDigest: digest, Evidence: bad})
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)
}

// TestStoreEntryPullCoexistsWithLocal is acceptance (b): ingesting a pulled
// entry — even one carrying the same name and same content as a local
// entry — creates its own row, never overwrites or shadows the local one,
// and both stay listed.
func TestStoreEntryPullCoexistsWithLocal(t *testing.T) {
	f := newFixture(t)
	digest, _, runID := publishAndRun(t, f)
	evidence := sampleEvidence(runID)

	var local storeEntryResp
	resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/store/entries"), fixtureStoreSecret,
		storeEntryCreateReq{GraphDigest: digest, Evidence: evidence}, &local)
	requireStatus(t, resp, body, http.StatusCreated)

	// Fetch the full self-contained document — exactly what a client of a
	// remote registry would GET before pushing it into its own plane.
	var doc json.RawMessage
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/store/entries/"+local.ID), nil, &doc)
	requireStatus(t, resp, body, http.StatusOK)

	var pulled storeEntryResp
	resp, body = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/store/entries/pull"), fixtureStoreSecret,
		storeEntryPullReq{SourceRegistry: "https://nodes.thor.internal:8443", Entry: doc}, &pulled)
	requireStatus(t, resp, body, http.StatusCreated)
	if pulled.Origin != "pulled" {
		t.Fatalf("pulled origin = %q, want pulled", pulled.Origin)
	}
	if pulled.ID == local.ID {
		t.Fatal("pull resolved to the local entry: pulling shadows local authorship")
	}
	if pulled.SourceRegistry != "https://nodes.thor.internal:8443" {
		t.Fatalf("source_registry = %q", pulled.SourceRegistry)
	}
	// Evidence rides verbatim through the pull — the source plane's proof,
	// not re-derived here.
	if !reflect.DeepEqual(pulled.Evidence, evidence) {
		t.Fatalf("pulled evidence not verbatim:\n got  %+v\n want %+v", pulled.Evidence, evidence)
	}

	// Pulling the identical entry again is idempotent.
	var again storeEntryResp
	resp, body = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/store/entries/pull"), fixtureStoreSecret,
		storeEntryPullReq{SourceRegistry: "https://nodes.thor.internal:8443", Entry: doc}, &again)
	requireStatus(t, resp, body, http.StatusOK)
	if again.ID != pulled.ID {
		t.Fatalf("re-pulling duplicated the entry: %s vs %s", again.ID, pulled.ID)
	}

	// The local entry is untouched and both are listed under the name.
	var localAgain storeEntryResp
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/store/entries/"+local.ID), nil, &localAgain)
	requireStatus(t, resp, body, http.StatusOK)
	if localAgain.Origin != "local" || localAgain.EntryDigest != local.EntryDigest {
		t.Fatalf("local entry changed after the pull: %+v", localAgain)
	}
	var list storeEntryListResp
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/store/entries?name=edge-order"), nil, &list)
	requireStatus(t, resp, body, http.StatusOK)
	if len(list.Items) != 2 {
		t.Fatalf("listing shows %d entries under the shared name, want 2 (local + pulled): %+v", len(list.Items), list.Items)
	}
}

func TestStoreEntryPullIntegrity(t *testing.T) {
	f := newFixture(t)
	digest, _, runID := publishAndRun(t, f)

	var local storeEntryResp
	resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/store/entries"), fixtureStoreSecret,
		storeEntryCreateReq{GraphDigest: digest, Evidence: sampleEvidence(runID)}, &local)
	requireStatus(t, resp, body, http.StatusCreated)

	var doc storeEntryResp
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/store/entries/"+local.ID), nil, &doc)
	requireStatus(t, resp, body, http.StatusOK)

	// A missing source registry is refused: a pulled entry must say where
	// it came from.
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal entry doc: %v", err)
	}
	resp, body = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/store/entries/pull"), fixtureStoreSecret,
		storeEntryPullReq{Entry: raw}, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)

	// Tampered evidence — a run id edited without recomputing the entry
	// digest — is refused: the digest is the integrity seal.
	tampered := doc
	tampered.Evidence.ProvingRunIDs = []string{"01TAMPEREDRUNIDAAAAAAAAAAA"}
	raw, err = json.Marshal(tampered)
	if err != nil {
		t.Fatalf("marshal tampered doc: %v", err)
	}
	resp, body = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/store/entries/pull"), fixtureStoreSecret,
		storeEntryPullReq{SourceRegistry: "https://nodes.thor.internal:8443", Entry: raw}, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)

	// A graph whose embedded source does not digest to its declared graph
	// digest is refused too — pull verifies the whole chain, not just the
	// envelope.
	forged := doc
	forged.Graph.Source = forged.Graph.Source + "\n# trailing edit\n"
	raw, err = json.Marshal(forged)
	if err != nil {
		t.Fatalf("marshal forged doc: %v", err)
	}
	resp, body = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/store/entries/pull"), fixtureStoreSecret,
		storeEntryPullReq{SourceRegistry: "https://nodes.thor.internal:8443", Entry: raw}, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)
}
