package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store"
)

// Store pull with actor mapping (plan task t8, issue #192's portability
// half). These tests pin the t8 acceptance end to end:
//
//	(a) a flow exported from one plane imports into a second whose
//	    actor ids differ; after an explicit mapping step it publishes and
//	    a run can be created, and the graph document hash-compares
//	    byte-identical before and after — no digest hand-edited;
//	(b) the pulled entry's evidence (run ids, deviations, required
//	    actors) stays readable in the importing plane's catalog;
//	(c) unbound or capability-mismatched requirements are refused BY NAME.
//
// TWO-PLANE SIMULATION: two newFixture calls — two namespaces in the one
// PG test harness, each behind its own api.Server. Actor rows on the
// importing plane are minted fresh (distinct ids and keys), which is
// exactly the "ids and digests differ" condition.

type storeBindingCreateReq struct {
	Ref      string `json:"ref"`
	ActorKey string `json:"actor_key"`
	BoundBy  string `json:"bound_by"`
}

type storeBindingResp struct {
	ID        string `json:"id"`
	EntryID   string `json:"entry_id"`
	Ref       string `json:"ref"`
	Kind      string `json:"kind"`
	ActorID   string `json:"actor_id"`
	ActorKey  string `json:"actor_key"`
	BoundBy   string `json:"bound_by"`
	CreatedAt string `json:"created_at"`
}

type storeBindingListResp struct {
	Items []storeBindingResp `json:"items"`
}

// The fixture graph's pinned refs (edge-order-ordered.workflow.yaml) — the
// identifiers a source plane's evidence manifest carries verbatim.
const (
	startRef  = "actor://company/start@sha256:aaaaaa"
	middleRef = "actor://company/middle@sha256:bbbbbb"
)

// insertActorWithCaps registers a local actor with an exact key and a
// capabilities document — the importing plane's own registrations, whose
// ids and keys share nothing with the exporting plane's pinned refs.
func insertActorWithCaps(t *testing.T, f *fixture, key, kind, capabilities string) string {
	t.Helper()
	id := store.NewULID()
	_, err := f.store.Pool().Exec(context.Background(),
		`INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, capabilities)
		 VALUES ($1, $2, $3, 1, $4, 'http', $5)`,
		id, f.nsID, key, kind, []byte(capabilities))
	if err != nil {
		t.Fatalf("insert actor %s: %v", key, err)
	}
	return id
}

// portableEvidence declares requirements against the fixture graph's own
// pinned refs, so the pulled flow's requirements are the graph's, verbatim.
func portableEvidence(runID string) storeEvidenceReq {
	return storeEvidenceReq{
		ProvingRunIDs: []string{runID},
		DeviationRecords: []storeDeviationRef{
			{Ref: "docs/deviations/2026-08-18-example.md", Note: "recorded against the proving run"},
		},
		RequiredCapabilities: []storeCapabilityReq{
			{Kind: "actor", Ref: startRef, Capabilities: []string{"shell"}},
			{Kind: "actor", Ref: middleRef, Capabilities: []string{"shell", "workspace-write"}},
		},
	}
}

// pullOnto exports an entry from the source fixture and ingests it into
// the importing one, returning the imported row.
func pullOnto(t *testing.T, source, importing *fixture, entryID string) storeEntryResp {
	t.Helper()
	var doc json.RawMessage
	resp, body := doJSON(t, source.client, http.MethodGet, source.url("/v1alpha1/store/entries/"+entryID), nil, &doc)
	requireStatus(t, resp, body, http.StatusOK)

	var pulled storeEntryResp
	resp, body = doJSONBearer(t, importing.client, http.MethodPost, importing.url("/v1alpha1/store/entries/pull"), fixtureStoreSecret,
		storeEntryPullReq{SourceRegistry: "https://nodes.source.internal:8443", Entry: doc}, &pulled)
	requireStatus(t, resp, body, http.StatusCreated)
	return pulled
}

// TestStorePullBindPublishRun is acceptance (a) + (b): export, import,
// map, publish, run — graph bytes and digest identical throughout.
func TestStorePullBindPublishRun(t *testing.T) {
	sourcePlane := newFixture(t)
	importingPlane := newFixture(t)

	digest, graphSource, runID := publishAndRun(t, sourcePlane)
	evidence := portableEvidence(runID)

	var exported storeEntryResp
	resp, body := doJSONBearer(t, sourcePlane.client, http.MethodPost, sourcePlane.url("/v1alpha1/store/entries"), fixtureStoreSecret,
		storeEntryCreateReq{GraphDigest: digest, Evidence: evidence}, &exported)
	requireStatus(t, resp, body, http.StatusCreated)

	pulled := pullOnto(t, sourcePlane, importingPlane, exported.ID)

	// Before any mapping: publishing is refused, and the refusal names
	// every unbound ref (acceptance (c)'s "by name").
	resp, body = doJSONBearer(t, importingPlane.client, http.MethodPost,
		importingPlane.url("/v1alpha1/store/entries/"+pulled.ID+"/publish"), fixtureStoreSecret, nil, nil)
	requireStatus(t, resp, body, http.StatusConflict)
	refusal := decodeAPIError(t, body)
	for _, ref := range []string{startRef, middleRef} {
		if !strings.Contains(refusal.Message, ref) {
			t.Fatalf("unbound-requirements refusal does not name %s: %s", ref, refusal.Message)
		}
	}

	// The importing plane registers its OWN actors — fresh ids, keys the
	// exporting plane never saw.
	insertActorWithCaps(t, importingPlane, "local/start-lane", "agent", `{"shell": {"posture": "workspace-write"}}`)
	middleActorID := insertActorWithCaps(t, importingPlane, "local/middle-lane", "agent",
		`{"shell": true, "workspace-write": {"paths": ["/work"]}}`)

	bind := func(ref, actorKey string) (storeBindingResp, *http.Response, []byte) {
		var out storeBindingResp
		resp, body := doJSONBearer(t, importingPlane.client, http.MethodPost,
			importingPlane.url("/v1alpha1/store/entries/"+pulled.ID+"/bindings"), fixtureStoreSecret,
			storeBindingCreateReq{Ref: ref, ActorKey: actorKey, BoundBy: "operator@spark"}, &out)
		return out, resp, body
	}

	first, resp, body := bind(startRef, "local/start-lane")
	requireStatus(t, resp, body, http.StatusCreated)
	if first.Ref != startRef || first.Kind != "actor" || first.ActorKey != "local/start-lane" ||
		first.BoundBy != "operator@spark" || first.CreatedAt == "" {
		t.Fatalf("binding record incomplete: %+v", first)
	}
	second, resp, body := bind(middleRef, "local/middle-lane")
	requireStatus(t, resp, body, http.StatusCreated)
	if second.ActorID != middleActorID {
		t.Fatalf("binding recorded actor row %s, want the registration current at bind time %s", second.ActorID, middleActorID)
	}

	// Bindings are records, readable back: who bound what to what, when.
	var trail storeBindingListResp
	resp, body = doJSON(t, importingPlane.client, http.MethodGet,
		importingPlane.url("/v1alpha1/store/entries/"+pulled.ID+"/bindings"), nil, &trail)
	requireStatus(t, resp, body, http.StatusOK)
	if len(trail.Items) != 2 {
		t.Fatalf("binding trail has %d records, want 2: %+v", len(trail.Items), trail.Items)
	}
	if trail.Items[0].ID != second.ID || trail.Items[1].ID != first.ID {
		t.Fatalf("trail not newest-first: %+v", trail.Items)
	}

	// The declared mapping done, the entry publishes — and the workflow
	// version's digest hash-compares identical to the exported original:
	// same bytes in, same content address out, nothing hand-edited.
	var published struct {
		Digest string `json:"digest"`
		Source string `json:"source"`
	}
	resp, body = doJSONBearer(t, importingPlane.client, http.MethodPost,
		importingPlane.url("/v1alpha1/store/entries/"+pulled.ID+"/publish"), fixtureStoreSecret, nil, &published)
	requireStatus(t, resp, body, http.StatusCreated)
	if published.Digest != digest {
		t.Fatalf("imported publish digests to %s, exported original was %s — the graph document changed", published.Digest, digest)
	}

	// Byte-identical, not merely digest-equal: read the published source
	// back off the importing plane and compare the bytes themselves.
	var fetched struct {
		Source string `json:"source"`
	}
	resp, body = doJSON(t, importingPlane.client, http.MethodGet,
		importingPlane.url("/v1alpha1/workflows/"+published.Digest), nil, &fetched)
	requireStatus(t, resp, body, http.StatusOK)
	if fetched.Source != graphSource {
		t.Fatal("published workflow source is not byte-identical to the exported original")
	}

	// A run can be created from it.
	var run runIDResp
	resp, body = doJSON(t, importingPlane.client, http.MethodPost, importingPlane.url("/v1alpha1/runs"),
		createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{}`)}, &run)
	requireStatus(t, resp, body, http.StatusCreated)
	if run.ID == "" {
		t.Fatal("run created with no id")
	}

	// Re-publishing is the generic lane's idempotent 200.
	resp, body = doJSONBearer(t, importingPlane.client, http.MethodPost,
		importingPlane.url("/v1alpha1/store/entries/"+pulled.ID+"/publish"), fixtureStoreSecret, nil, nil)
	requireStatus(t, resp, body, http.StatusOK)

	// Acceptance (b): the pulled entry's evidence — run ids, deviations,
	// required actors — reads back verbatim from the importing catalog.
	var catalogued storeEntryResp
	resp, body = doJSON(t, importingPlane.client, http.MethodGet,
		importingPlane.url("/v1alpha1/store/entries/"+pulled.ID), nil, &catalogued)
	requireStatus(t, resp, body, http.StatusOK)
	if !reflect.DeepEqual(catalogued.Evidence, evidence) {
		t.Fatalf("imported evidence not readable verbatim:\n got  %+v\n want %+v", catalogued.Evidence, evidence)
	}
}

// TestStoreBindingRefusals is acceptance (c): every refusal names what is
// wrong — the undeclared ref, the missing capabilities, the wrong-kind
// registration — and local entries take no bindings at all.
func TestStoreBindingRefusals(t *testing.T) {
	sourcePlane := newFixture(t)
	importingPlane := newFixture(t)

	digest, _, runID := publishAndRun(t, sourcePlane)
	var exported storeEntryResp
	resp, body := doJSONBearer(t, sourcePlane.client, http.MethodPost, sourcePlane.url("/v1alpha1/store/entries"), fixtureStoreSecret,
		storeEntryCreateReq{GraphDigest: digest, Evidence: portableEvidence(runID)}, &exported)
	requireStatus(t, resp, body, http.StatusCreated)
	pulled := pullOnto(t, sourcePlane, importingPlane, exported.ID)

	bind := func(req storeBindingCreateReq) (*http.Response, []byte) {
		return doJSONBearer(t, importingPlane.client, http.MethodPost,
			importingPlane.url("/v1alpha1/store/entries/"+pulled.ID+"/bindings"), fixtureStoreSecret, req, nil)
	}

	// The mutating mapping routes are store-write-gated like every other
	// store write (the t15 rule).
	for _, path := range []string{"/bindings", "/publish"} {
		resp, body := doJSON(t, importingPlane.client, http.MethodPost,
			importingPlane.url("/v1alpha1/store/entries/"+pulled.ID+path), map[string]string{}, nil)
		requireStatus(t, resp, body, http.StatusUnauthorized)
		decodeAPIError(t, body)
	}

	// A ref the entry never declared is refused, named.
	resp, body = bind(storeBindingCreateReq{Ref: "actor://company/nobody@sha256:ffff", ActorKey: "local/x", BoundBy: "op"})
	requireStatus(t, resp, body, http.StatusBadRequest)
	if e := decodeAPIError(t, body); !strings.Contains(e.Message, "actor://company/nobody@sha256:ffff") {
		t.Fatalf("undeclared-ref refusal does not name the ref: %s", e.Message)
	}

	// An unregistered actor key is refused, named.
	resp, body = bind(storeBindingCreateReq{Ref: startRef, ActorKey: "local/ghost", BoundBy: "op"})
	requireStatus(t, resp, body, http.StatusBadRequest)
	if e := decodeAPIError(t, body); !strings.Contains(e.Message, "local/ghost") {
		t.Fatalf("unknown-actor refusal does not name the key: %s", e.Message)
	}

	// A capability-mismatched registration is refused with the missing
	// capabilities listed by name.
	insertActorWithCaps(t, importingPlane, "local/read-only", "agent", `{"shell": true}`)
	resp, body = bind(storeBindingCreateReq{Ref: middleRef, ActorKey: "local/read-only", BoundBy: "op"})
	requireStatus(t, resp, body, http.StatusBadRequest)
	if e := decodeAPIError(t, body); !strings.Contains(e.Message, "workspace-write") {
		t.Fatalf("capability refusal does not name the missing capability: %s", e.Message)
	}

	// A capability advertised as false or null is not advertised.
	insertActorWithCaps(t, importingPlane, "local/disabled", "agent", `{"shell": true, "workspace-write": false}`)
	resp, body = bind(storeBindingCreateReq{Ref: middleRef, ActorKey: "local/disabled", BoundBy: "op"})
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)

	// A runner registration cannot stand in for an actor requirement.
	insertActorWithCaps(t, importingPlane, "local/runner-lane", "runner", `{"shell": true, "workspace-write": true}`)
	resp, body = bind(storeBindingCreateReq{Ref: startRef, ActorKey: "local/runner-lane", BoundBy: "op"})
	requireStatus(t, resp, body, http.StatusBadRequest)
	if e := decodeAPIError(t, body); !strings.Contains(e.Message, "runner") {
		t.Fatalf("kind refusal does not say why: %s", e.Message)
	}

	// bound_by is required: a binding with no author is refused.
	insertActorWithCaps(t, importingPlane, "local/ok", "agent", `{"shell": true}`)
	resp, body = bind(storeBindingCreateReq{Ref: startRef, ActorKey: "local/ok"})
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)

	// A LOCAL entry takes no bindings — its refs already resolve here.
	resp, body = doJSONBearer(t, sourcePlane.client, http.MethodPost,
		sourcePlane.url("/v1alpha1/store/entries/"+exported.ID+"/bindings"), fixtureStoreSecret,
		storeBindingCreateReq{Ref: startRef, ActorKey: "local/ok", BoundBy: "op"}, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)

	// Unknown entry ids are documented 404s on all three routes.
	for _, probe := range []struct{ method, path string }{
		{http.MethodPost, "/v1alpha1/store/entries/01JUNKJUNKJUNKJUNKJUNKJUNK/bindings"},
		{http.MethodGet, "/v1alpha1/store/entries/01JUNKJUNKJUNKJUNKJUNKJUNK/bindings"},
		{http.MethodPost, "/v1alpha1/store/entries/01JUNKJUNKJUNKJUNKJUNKJUNK/publish"},
	} {
		resp, body := doJSONBearer(t, importingPlane.client, probe.method, importingPlane.url(probe.path), fixtureStoreSecret,
			storeBindingCreateReq{Ref: startRef, ActorKey: "local/ok", BoundBy: "op"}, nil)
		requireStatus(t, resp, body, http.StatusNotFound)
		decodeAPIError(t, body)
	}
}

// TestStorePublishSatisfiedByDirectRegistration pins unboundRequirements'
// other satisfaction arm: a plane that registers the ref's own key (the
// same-fleet case) publishes without any binding.
func TestStorePublishSatisfiedByDirectRegistration(t *testing.T) {
	sourcePlane := newFixture(t)
	importingPlane := newFixture(t)

	digest, _, runID := publishAndRun(t, sourcePlane)
	var exported storeEntryResp
	resp, body := doJSONBearer(t, sourcePlane.client, http.MethodPost, sourcePlane.url("/v1alpha1/store/entries"), fixtureStoreSecret,
		storeEntryCreateReq{GraphDigest: digest, Evidence: portableEvidence(runID)}, &exported)
	requireStatus(t, resp, body, http.StatusCreated)
	pulled := pullOnto(t, sourcePlane, importingPlane, exported.ID)

	// Register the pinned refs' own keys locally — no mapping needed.
	insertActorWithCaps(t, importingPlane, "company/start", "agent", `{}`)
	insertActorWithCaps(t, importingPlane, "company/middle", "agent", `{}`)

	resp, body = doJSONBearer(t, importingPlane.client, http.MethodPost,
		importingPlane.url("/v1alpha1/store/entries/"+pulled.ID+"/publish"), fixtureStoreSecret, nil, nil)
	requireStatus(t, resp, body, http.StatusCreated)
}
