package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/declengine"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// t13 (#328, spec c88, honesty h61): "why did (or didn't) declaration X
// fire for event E" must be answerable from the API, not just from a store
// query (internal/declengine/explain_postgres_test.go covers the latter).
// This file exercises GET /v1alpha1/declarations/{name}/evaluations
// end-to-end through the real HTTP server: one outcome that fired, one
// that did not, and the two 404 shapes (unmatched, unknown declaration).

// publishActiveDecl publishes d and activates it, mirroring declengine's own
// test helper of the same shape (unexported there, so this package needs
// its own copy against f.store).
func publishActiveDecl(t *testing.T, s *postgres.Store, ns string, d decl.Declaration) postgres.DeclarationVersion {
	t.Helper()
	body, err := d.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.PublishDeclaration(context.Background(), postgres.PublishDeclarationInput{NamespaceID: ns, Name: d.Name, Body: body, Author: "human"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDeclarationActivation(context.Background(), ns, v.ID, "activate", "human", ""); err != nil {
		t.Fatal(err)
	}
	return v
}

func deliverEvent(t *testing.T, s *postgres.Store, ns string) string {
	t.Helper()
	ev, err := s.DeliverSignalEvent(context.Background(), postgres.DeliverSignalEventInput{NamespaceID: ns, Name: "pr-upkeep.pr", Emitter: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return ev.Event.ID
}

func explainDecl(name string) decl.Declaration {
	return decl.Declaration{
		Name:        name,
		Trigger:     decl.Trigger{Kind: "pr-upkeep.pr", ReentryLimit: 3, HopLimit: 20, RateCeiling: "30/h"},
		Condition:   "true",
		Action:      decl.Action{Kind: "agent.work", With: json.RawMessage(`{"uses":"actor://test"}`)},
		StartNode:   decl.Node{Name: "ready", Deadline: "none"},
		LandingNode: decl.Node{Name: "waiting", Deadline: "1h"},
	}
}

// explainDispatchFunc adapts a plain function to declengine.Dispatcher --
// the package itself keeps its own equivalent test-only (unexported, in a
// _test.go file the declengine package cannot export), so this package
// needs its own copy.
type explainDispatchFunc func(context.Context, declengine.DispatchRequest) (declengine.DispatchResult, error)

func (f explainDispatchFunc) Dispatch(ctx context.Context, r declengine.DispatchRequest) (declengine.DispatchResult, error) {
	return f(ctx, r)
}

func explainEngine(t *testing.T, s *postgres.Store, d declengine.Dispatcher) *declengine.Engine {
	t.Helper()
	t.Setenv("TCA_API_EXPLAIN_KEY", strings.Repeat("k", 32))
	e, err := declengine.New(declengine.Config{MarkerKeyEnv: "TCA_API_EXPLAIN_KEY"}, declengine.PostgresBackend{Store: s}, declengine.PostgresMarkerStore{Store: s}, d)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestDeclarationEvaluationRouteReturnsFired(t *testing.T) {
	f := newFixture(t)
	d := explainDecl("api-explain-fired")
	publishActiveDecl(t, f.store, f.nsID, d)
	eventID := deliverEvent(t, f.store, f.nsID)

	e := explainEngine(t, f.store, explainDispatchFunc(func(context.Context, declengine.DispatchRequest) (declengine.DispatchResult, error) {
		return declengine.DispatchResult{}, nil
	}))
	if err := e.Handle(context.Background(), declengine.Event{NamespaceID: f.nsID, ID: eventID, Kind: "pr-upkeep.pr", Node: "ready"}); err != nil {
		t.Fatal(err)
	}

	var out apipkg.DeclarationEvaluationOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/declarations/"+d.Name+"/evaluations?event_id="+eventID), nil, &out)
	requireStatus(t, resp, body, http.StatusOK)
	if out.Outcome != "fired" || out.Reason == "" {
		t.Fatalf("evaluation=%+v, want outcome fired with a reason", out)
	}
	if out.FiringID == "" {
		t.Fatalf("evaluation=%+v, want a firing id", out)
	}
	if out.EventID != eventID || out.DeclarationName != d.Name {
		t.Fatalf("evaluation=%+v, want event_id %q declaration_name %q", out, eventID, d.Name)
	}
}

func TestDeclarationEvaluationRouteReturnsConditionFalse(t *testing.T) {
	f := newFixture(t)
	d := explainDecl("api-explain-condition-false")
	d.Condition = "event.priority == 'High'"
	publishActiveDecl(t, f.store, f.nsID, d)
	eventID := deliverEvent(t, f.store, f.nsID)

	e := explainEngine(t, f.store, explainDispatchFunc(func(context.Context, declengine.DispatchRequest) (declengine.DispatchResult, error) {
		t.Fatal("dispatched despite a false condition")
		return declengine.DispatchResult{}, nil
	}))
	if err := e.Handle(context.Background(), declengine.Event{NamespaceID: f.nsID, ID: eventID, Kind: "pr-upkeep.pr", Node: "ready", Variables: map[string]any{"priority": "Low"}}); err != nil {
		t.Fatal(err)
	}

	var out apipkg.DeclarationEvaluationOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/declarations/"+d.Name+"/evaluations?event_id="+eventID), nil, &out)
	requireStatus(t, resp, body, http.StatusOK)
	if out.Outcome != "condition false" || out.Reason == "" {
		t.Fatalf("evaluation=%+v, want outcome %q with a reason", out, "condition false")
	}
	if out.FiringID != "" {
		t.Fatalf("evaluation=%+v, want no firing id: condition false is decided before a firing is claimed", out)
	}
}

func TestDeclarationEvaluationRouteRefusals(t *testing.T) {
	f := newFixture(t)
	d := explainDecl("api-explain-refusals")
	publishActiveDecl(t, f.store, f.nsID, d)
	eventID := deliverEvent(t, f.store, f.nsID)

	// Missing ?event_id is a caller error.
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/declarations/"+d.Name+"/evaluations"), nil, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)

	// The declaration never matched this event (its trigger fires on
	// "ready", the event landed on a different node): nothing was ever
	// recorded, so the API says not found rather than fabricating a verdict.
	e := explainEngine(t, f.store, explainDispatchFunc(func(context.Context, declengine.DispatchRequest) (declengine.DispatchResult, error) {
		t.Fatal("dispatched a declaration whose trigger never matched")
		return declengine.DispatchResult{}, nil
	}))
	if err := e.Handle(context.Background(), declengine.Event{NamespaceID: f.nsID, ID: eventID, Kind: "pr-upkeep.pr", Node: "some-other-node"}); err != nil {
		t.Fatal(err)
	}
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/declarations/"+d.Name+"/evaluations?event_id="+eventID), nil, nil)
	requireStatus(t, resp, body, http.StatusNotFound)

	// An unknown declaration name is the same 404 shape.
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/declarations/no-such-declaration/evaluations?event_id="+eventID), nil, nil)
	requireStatus(t, resp, body, http.StatusNotFound)
}
