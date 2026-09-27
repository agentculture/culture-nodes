package declengine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
)

func TestMatchingActiveTriggersAndCELErrors(t *testing.T) {
	a := active("A")
	a.Declaration.Trigger.With = json.RawMessage(`{"priority":"High"}`)
	for _, tc := range []struct {
		kind, node, priority string
		want                 bool
	}{{"timer", "ready", "High", true}, {"timer", "other", "High", false}, {"human.decision", "ready", "High", false}, {"timer", "ready", "Low", false}} {
		got, err := matches(a.Declaration, Event{Kind: tc.kind, Node: tc.node, Variables: map[string]any{"priority": tc.priority}})
		if err != nil || got != tc.want {
			t.Fatalf("match %+v =%v err=%v", tc, got, err)
		}
	}
	for _, source := range []string{"1", "invalid(", "event.missing > 1"} {
		m := &memoryBackend{}
		a.Declaration.Condition = source
		e := newTestEngine(t, m, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
			t.Fatal("invalid condition dispatched")
			return DispatchResult{}, nil
		}))
		if err := e.evaluate(context.Background(), Event{ID: "e", NamespaceID: "ns"}, a, "", nil); err == nil {
			t.Fatalf("%q succeeded", source)
		}
		if m.steps[len(m.steps)-1].Outcome != "condition error" {
			t.Fatal("error not recorded")
		}
	}
}
func TestRenderingPreservesJSONAndAbsentVariables(t *testing.T) {
	a := decl.Action{With: json.RawMessage(`{"values":["{name}","{2:missing:fallback}","{absent}",3]}`)}
	value := "quoted \"line\"\n{braces}"
	out, err := renderAction(a, map[string]any{"name": value}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Values []any `json:"values"`
	}
	if err := json.Unmarshal(out.With, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Values[0] != value || decoded.Values[1] != "fallback" || decoded.Values[2] != "{absent}" {
		t.Fatalf("%s", out.With)
	}
}
func TestSecretRequiredAndDispatchFailureRecorded(t *testing.T) {
	for _, key := range []string{"", strings.Repeat("a", 31)} {
		t.Setenv("TCA_BAD_KEY", key)
		if _, err := New(Config{MarkerKeyEnv: "TCA_BAD_KEY"}, &memoryBackend{}, &markerMemory{}, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) { return DispatchResult{}, nil })); err == nil {
			t.Fatal("accepted short key")
		}
	}
	m := &memoryBackend{active: []ActiveDeclaration{active("A")}}
	e := newTestEngine(t, m, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
		return DispatchResult{}, errors.New("worker unavailable")
	}))
	if err := e.Handle(context.Background(), Event{ID: "e", NamespaceID: "ns", Kind: "timer", Node: "ready", Variables: map[string]any{"priority": "High"}}); err == nil {
		t.Fatal("lost dispatch error")
	}
	if len(m.nodes) != 0 || m.steps[len(m.steps)-1].Outcome != "dispatch failed" {
		t.Fatal("failed dispatch was not recorded correctly")
	}
}
func TestThreeDeclarationChainThroughVerifiedMarkers(t *testing.T) {
	m := &memoryBackend{}
	var requests []DispatchRequest
	e := newTestEngine(t, m, dispatchFunc(func(_ context.Context, r DispatchRequest) (DispatchResult, error) {
		requests = append(requests, r)
		return DispatchResult{ArtifactID: r.Firing.ID, Variables: map[string]any{"x": "X", "y": "Y", "z": "Z"}}, nil
	}))
	ev := Event{ID: "1", NamespaceID: "ns", Kind: "timer", Node: "ready", Variables: map[string]any{"priority": "High"}}
	for _, name := range []string{"A", "B", "C"} {
		a := active(name)
		m.active = []ActiveDeclaration{a}
		if err := e.Handle(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
		req := requests[len(requests)-1]
		m.ancestors = append([]Ancestor{{FiringID: req.Firing.ID, CanonicalID: req.Firing.ID, DeclarationID: name, Name: name, Variables: map[string]any{"x": "X", "y": "Y", "z": "Z"}}}, m.ancestors...)
		ev.ID += "n"
		ev.Origin = OriginEvent{Marker: req.Marker, ArtifactKind: "github.pr", ArtifactID: req.Firing.ID}
	}
	if !strings.Contains(string(requests[2].Action.With), "X|Y|Z") {
		t.Fatal(string(requests[2].Action.With))
	}
}
