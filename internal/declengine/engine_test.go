package declengine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

type memoryBackend struct {
	active    []ActiveDeclaration
	ancestors []Ancestor
	claims    map[string]postgres.DeclarationFiring
	steps     []Evaluation
	nodes     []Landing
}

func (m *memoryBackend) Active(context.Context, string) ([]ActiveDeclaration, error) {
	return m.active, nil
}
func (m *memoryBackend) Lineage(context.Context, string, string) ([]Ancestor, error) {
	return m.ancestors, nil
}
func (m *memoryBackend) Claim(_ context.Context, in postgres.DeclarationFiringInput) (postgres.DeclarationFiring, bool, error) {
	if m.claims == nil {
		m.claims = map[string]postgres.DeclarationFiring{}
	}
	key := in.EventID + in.DeclarationID
	if f, ok := m.claims[key]; ok {
		return f, false, nil
	}
	f := postgres.DeclarationFiring{ID: fmt.Sprint(len(m.claims) + 1), NamespaceID: in.NamespaceID, EventID: in.EventID, DeclarationID: in.DeclarationID, DeclarationVersion: in.DeclarationVersion, TriggerDigest: in.TriggerDigest, ConditionDigest: in.ConditionDigest, ActionDigest: in.ActionDigest}
	f.CanonicalFiringID = f.ID
	f.LineageID = f.ID
	m.claims[key] = f
	return f, true, nil
}
func (m *memoryBackend) Record(_ context.Context, e Evaluation) error {
	m.steps = append(m.steps, e)
	return nil
}
func (m *memoryBackend) Finish(_ context.Context, l Landing, evaluation Evaluation) error {
	m.steps = append(m.steps, evaluation)
	m.nodes = append(m.nodes, l)
	return nil
}

type dispatchFunc func(context.Context, DispatchRequest) (DispatchResult, error)

func (f dispatchFunc) Dispatch(c context.Context, r DispatchRequest) (DispatchResult, error) {
	return f(c, r)
}
func active(name string) ActiveDeclaration {
	return ActiveDeclaration{ID: name, VersionID: name + "-v1", Declaration: decl.Declaration{Name: name, Trigger: decl.Trigger{Kind: "timer", ReentryLimit: 3, HopLimit: 20, RateCeiling: "30/h"}, Condition: "event.priority == 'High'", Action: decl.Action{Kind: "agent.work", With: json.RawMessage(`{"uses":"actor://test","input":{"text":"{1:x}|{2:y}|{A:z:absent}"}}`)}, StartNode: decl.Node{Name: "ready", Deadline: "none"}, LandingNode: decl.Node{Name: "waiting", Deadline: "1h"}}}
}
func newTestEngine(t *testing.T, m *memoryBackend, d Dispatcher) *Engine {
	t.Helper()
	t.Setenv("TCA_TEST_MARKER_KEY", strings.Repeat("k", 32))
	e, err := New(Config{MarkerKeyEnv: "TCA_TEST_MARKER_KEY"}, m, &markerMemory{}, d)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// Acceptance 1 and 5: event -> match -> lineage -> condition -> dispatch ->
// landing node, each step recorded; the same event delivered twice fires once.
func TestFiringPipelineAndDuplicate(t *testing.T) {
	m := &memoryBackend{active: []ActiveDeclaration{active("C")}}
	calls := 0
	e := newTestEngine(t, m, dispatchFunc(func(_ context.Context, r DispatchRequest) (DispatchResult, error) {
		calls++
		if r.Marker == "" {
			t.Fatal("missing marker")
		}
		return DispatchResult{Variables: map[string]any{"pr": "42"}}, nil
	}))
	event := Event{NamespaceID: "ns", ID: "event", Kind: "timer", Node: "ready", Variables: map[string]any{"priority": "High"}}
	for i := 0; i < 2; i++ {
		if err := e.Handle(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 || len(m.nodes) != 1 || m.nodes[0].Node.Name != "waiting" || m.nodes[0].Deadline != time.Hour {
		t.Fatalf("calls=%d nodes=%+v", calls, m.nodes)
	}
	var outcomes []string
	for _, v := range m.steps {
		outcomes = append(outcomes, v.Outcome)
	}
	want := []string{OutcomeMatched, OutcomeLineageChecked, OutcomeConditionTrue, OutcomeDispatching, OutcomeFired,
		OutcomeMatched, OutcomeLineageChecked, OutcomeConditionTrue, OutcomeDuplicate}
	if strings.Join(outcomes, ",") != strings.Join(want, ",") {
		t.Fatalf("steps %q, want %q", outcomes, want)
	}
	fired := m.steps[4]
	if fired.FiringID == "" || fired.Variables["priority"] != "High" || fired.Variables["pr"] != "42" {
		t.Fatalf("fired evaluation %+v", fired)
	}
	for _, v := range m.steps {
		if v.Outcome != OutcomeFired && v.Variables != nil {
			t.Fatalf("%s evaluation carries variables", v.Outcome)
		}
	}
}

// A landing node that cannot be opened is refused before the firing is
// claimed, so no action is ever dispatched without somewhere to land.
func TestInvalidLandingDeadlineRefusedBeforeDispatch(t *testing.T) {
	a := active("A")
	a.Declaration.LandingNode.Deadline = "soon"
	m := &memoryBackend{active: []ActiveDeclaration{a}}
	e := newTestEngine(t, m, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
		t.Fatal("dispatched without a valid landing node")
		return DispatchResult{}, nil
	}))
	err := e.Handle(context.Background(), Event{NamespaceID: "ns", ID: "e", Kind: "timer", Node: "ready", Variables: map[string]any{"priority": "High"}})
	if err == nil || len(m.claims) != 0 || m.steps[len(m.steps)-1].Outcome != OutcomeEvaluationError {
		t.Fatalf("err=%v claims=%d steps=%+v", err, len(m.claims), m.steps)
	}
}

func TestMustCanAndLineageTemplates(t *testing.T) {
	for _, kind := range []string{"must", "can"} {
		for _, present := range []bool{false, true} {
			t.Run(fmt.Sprint(kind, present), func(t *testing.T) {
				a := active("C")
				a.Links = []postgres.DeclarationLink{{FromDeclarationID: "C", ToDeclarationID: "A", Kind: kind}}
				m := &memoryBackend{active: []ActiveDeclaration{a}}
				if present {
					m.ancestors = []Ancestor{{FiringID: "b", CanonicalID: "b", DeclarationID: "B", Name: "B", Variables: map[string]any{"x": "X"}}, {FiringID: "a", CanonicalID: "a", DeclarationID: "A", Name: "A", Variables: map[string]any{"y": "Y", "z": "Z"}}}
				}
				calls := 0
				e := newTestEngine(t, m, dispatchFunc(func(_ context.Context, r DispatchRequest) (DispatchResult, error) {
					calls++
					want := "{1:x}|{2:y}|absent"
					if present {
						want = "X|Y|Z"
					}
					if !strings.Contains(string(r.Action.With), want) {
						t.Fatalf("rendered %s want %s", r.Action.With, want)
					}
					return DispatchResult{}, nil
				}))
				// The marker verifier is independently covered; exercise matching on the
				// resolved causal ancestry, never on all rows sharing a lineage ID.
				err := e.evaluate(context.Background(), Event{NamespaceID: "ns", ID: "e", Kind: "timer", Node: "ready", Variables: map[string]any{"priority": "High"}}, a, "parent", m.ancestors)
				if err != nil {
					t.Fatal(err)
				}
				want := 1
				if kind == "must" && !present {
					want = 0
				}
				if calls != want {
					t.Fatalf("calls %d want %d", calls, want)
				}
			})
		}
	}
}
func TestLiveUpgradePinsComponents(t *testing.T) {
	a := active("A")
	m := &memoryBackend{active: []ActiveDeclaration{a}}
	e := newTestEngine(t, m, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) { return DispatchResult{}, nil }))
	ev := Event{NamespaceID: "ns", ID: "one", Kind: "timer", Node: "ready", Variables: map[string]any{"priority": "High"}}
	if err := e.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	a.VersionID = "A-v2"
	a.Declaration.Trigger.With = json.RawMessage(`{"priority":"High"}`)
	a.Declaration.Condition = "true"
	a.Declaration.Action.With = json.RawMessage(`{"input":"new"}`)
	m.active = []ActiveDeclaration{a}
	ev.ID = "two"
	if err := e.evaluate(context.Background(), ev, a, "1", []Ancestor{{CanonicalID: "1", DeclarationID: "A"}}); err != nil {
		t.Fatal(err)
	}
	one, two := m.claims["oneA"], m.claims["twoA"]
	if one.DeclarationVersion == two.DeclarationVersion || one.TriggerDigest == two.TriggerDigest || one.ConditionDigest == two.ConditionDigest || one.ActionDigest == two.ActionDigest {
		t.Fatalf("versions not pinned: %+v %+v", one, two)
	}
}
func TestConditionAndCanonicalReentry(t *testing.T) {
	a := active("A")
	a.Declaration.Trigger.ReentryLimit = 1
	m := &memoryBackend{}
	calls := 0
	e := newTestEngine(t, m, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) { calls++; return DispatchResult{}, nil }))
	ancestors := []Ancestor{{FiringID: "retry", CanonicalID: "first", DeclarationID: "A"}, {FiringID: "first", CanonicalID: "first", DeclarationID: "A"}}
	ev := Event{NamespaceID: "ns", ID: "one", Variables: map[string]any{"priority": "Low"}}
	if err := e.evaluate(context.Background(), ev, a, "retry", ancestors); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("false condition fired")
	}
	ev.Variables["priority"] = "High"
	if err := e.evaluate(context.Background(), ev, a, "retry", ancestors); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("remint counted twice")
	}
	ev.ID = "two"
	ancestors = append(ancestors, Ancestor{CanonicalID: "second", DeclarationID: "A"})
	if err := e.evaluate(context.Background(), ev, a, "second", ancestors); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("reentry not bounded")
	}
}
func BenchmarkResolveLineage10000(b *testing.B) {
	table := make([]Ancestor, 10000)
	for i := range table {
		table[i] = Ancestor{CanonicalID: fmt.Sprint(i), DeclarationID: fmt.Sprint(i % 50), Name: fmt.Sprint(i % 50), Variables: map[string]any{"x": "value"}}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lineage := ResolveLineage(table)
		if len(lineage) != 10000 {
			b.Fatal(len(lineage))
		}
	}
}
