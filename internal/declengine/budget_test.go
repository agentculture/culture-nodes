package declengine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
)

// budgetMemoryBackend composes the package's ordinary memoryBackend fake
// (engine_test.go) with an in-memory implementation of BudgetBackend, so
// these tests exercise the exact capability-detection path
// (e.backend.(BudgetBackend)) the postgres backend also goes through,
// without a database.
type budgetMemoryBackend struct {
	memoryBackend
	budgets      []Budget
	spend        map[string]BudgetSpend
	aliasMembers map[string][]string // declarationID -> alias names it directly belongs to
	emitted      []Event
	charged      []chargeEntry
	// applicableErr / spendErr, when set, make the corresponding call fail
	// -- proving a read error blocks neither way (ADR 0011 §2) rather than
	// resolving as "no budget applies".
	applicableErr error
	spendErr      error
}

type chargeEntry struct {
	firingID, node, machine, declarationID string
}

func (b *budgetMemoryBackend) ApplicableBudgets(_ context.Context, _, node, machine, declarationID string) ([]Budget, error) {
	if b.applicableErr != nil {
		return nil, b.applicableErr
	}
	var out []Budget
	aliases := b.aliasMembers[declarationID]
	for _, bud := range b.budgets {
		switch bud.Scope {
		case BudgetScopeNode:
			if bud.Key == node {
				out = append(out, bud)
			}
		case BudgetScopeMachine:
			if machine != "" && bud.Key == machine {
				out = append(out, bud)
			}
		case BudgetScopeDeclaration:
			if bud.Key == declarationID {
				out = append(out, bud)
			}
		case BudgetScopeAlias:
			for _, alias := range aliases {
				if bud.Key == alias {
					out = append(out, bud)
				}
			}
		}
	}
	return out, nil
}

func (b *budgetMemoryBackend) BudgetSpend(_ context.Context, _ string, scope BudgetScope, key string) (BudgetSpend, error) {
	if b.spendErr != nil {
		return BudgetSpend{}, b.spendErr
	}
	if b.spend == nil {
		return BudgetSpend{}, nil
	}
	return b.spend[string(scope)+"/"+key], nil
}

func (b *budgetMemoryBackend) ChargeBudgetSpend(_ context.Context, _, firingID, node, machine, declarationID string) error {
	b.charged = append(b.charged, chargeEntry{firingID, node, machine, declarationID})
	if b.spend == nil {
		b.spend = map[string]BudgetSpend{}
	}
	bump := func(scope BudgetScope, key string) {
		if key == "" {
			return
		}
		s := b.spend[string(scope)+"/"+key]
		s.Sessions++
		b.spend[string(scope)+"/"+key] = s
	}
	bump(BudgetScopeNode, node)
	bump(BudgetScopeMachine, machine)
	bump(BudgetScopeDeclaration, declarationID)
	for _, alias := range b.aliasMembers[declarationID] {
		bump(BudgetScopeAlias, alias)
	}
	return nil
}

func (b *budgetMemoryBackend) EmitBudgetExhausted(_ context.Context, namespaceID, node, reason string) (string, error) {
	id := fmt.Sprintf("budget-event-%d", len(b.emitted)+1)
	b.emitted = append(b.emitted, Event{NamespaceID: namespaceID, ID: id, Kind: ActionTriggerBudgetExhausted, Node: node})
	return id, nil
}

// newBudgetTestEngine mirrors engine_test.go's newTestEngine but accepts any
// Backend (that helper is typed to *memoryBackend specifically), so it can
// take a *budgetMemoryBackend.
func newBudgetTestEngine(t *testing.T, backend Backend, d Dispatcher) *Engine {
	t.Helper()
	t.Setenv("TCA_TEST_BUDGET_MARKER_KEY", strings.Repeat("k", 32))
	e, err := New(Config{MarkerKeyEnv: "TCA_TEST_BUDGET_MARKER_KEY"}, backend, &markerMemory{}, d)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func budgetActive(machine string) ActiveDeclaration {
	a := active("budgeted")
	a.Declaration.Condition = "true"
	a.Declaration.Action.With = []byte(`{"uses":"` + machine + `","input":{}}`)
	return a
}

// Acceptance 1: a session budget on ANY of the four scopes -- node, machine,
// declaration, alias -- blocks dispatch once exhausted, checking every
// applicable budget rather than stopping at the first scope that happens to
// have headroom.
func TestBudgetBlocksDispatchAtEveryScope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget Budget
	}{
		{"node", Budget{Scope: BudgetScopeNode, Key: "waiting", MaxSessions: 1}},
		{"machine", Budget{Scope: BudgetScopeMachine, Key: "actor://thor", MaxSessions: 1}},
		{"declaration", Budget{Scope: BudgetScopeDeclaration, Key: "budgeted", MaxSessions: 1}},
		{"alias", Budget{Scope: BudgetScopeAlias, Key: "jira-chain", MaxSessions: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := budgetActive("actor://thor")
			m := &budgetMemoryBackend{
				budgets:      []Budget{tc.budget},
				aliasMembers: map[string][]string{"budgeted": {"jira-chain"}},
				spend:        map[string]BudgetSpend{string(tc.budget.Scope) + "/" + tc.budget.Key: {Sessions: 1}},
			}
			calls := 0
			e := newBudgetTestEngine(t, m, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
				calls++
				return DispatchResult{}, nil
			}))
			ev := Event{NamespaceID: "ns", ID: "e1", Variables: map[string]any{"priority": "High"}}
			if err := e.evaluate(context.Background(), ev, a, "", nil); err != nil {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Fatalf("%s budget exhausted but dispatch still ran", tc.name)
			}
			last := m.steps[len(m.steps)-1]
			if last.Outcome != OutcomeBudgetBlocked || last.Reason == "" {
				t.Fatalf("evaluation %+v, want a visible budget-exhausted record", last)
			}
			if !strings.Contains(last.Reason, tc.name) {
				t.Errorf("reason %q does not name the exhausted scope %q", last.Reason, tc.name)
			}
		})
	}
}

// Acceptance 2: a blocked dispatch emits action.budget_exhausted linked to
// the declaration's landing node.
func TestBudgetBlockedEmitsActionTriggerLinkedToNode(t *testing.T) {
	a := budgetActive("actor://thor")
	m := &budgetMemoryBackend{
		budgets: []Budget{{Scope: BudgetScopeNode, Key: "waiting", MaxSessions: 1}},
		spend:   map[string]BudgetSpend{"node/waiting": {Sessions: 1}},
	}
	e := newBudgetTestEngine(t, m, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
		t.Fatal("dispatch must not run once a budget is exhausted")
		return DispatchResult{}, nil
	}))
	ev := Event{NamespaceID: "ns", ID: "e1", Variables: map[string]any{"priority": "High"}}
	if err := e.evaluate(context.Background(), ev, a, "", nil); err != nil {
		t.Fatal(err)
	}
	if len(m.emitted) != 1 {
		t.Fatalf("emitted %d events, want exactly 1", len(m.emitted))
	}
	got := m.emitted[0]
	if got.Kind != ActionTriggerBudgetExhausted {
		t.Errorf("emitted kind %q, want %q", got.Kind, ActionTriggerBudgetExhausted)
	}
	if got.Node != a.Declaration.LandingNode.Name {
		t.Errorf("emitted event linked to node %q, want the landing node %q", got.Node, a.Declaration.LandingNode.Name)
	}
}

// A blocked dispatch's emitted trigger reaches a declaration that reacts to
// it -- the same "runs back through Handle" shape nodes.go's action.*
// triggers already have.
func TestBudgetExhaustedTriggerFiresAReactingDeclaration(t *testing.T) {
	blocked := budgetActive("actor://thor")
	blocked.ID, blocked.VersionID = "blocked", "blocked-v1"
	blocked.Declaration.Name = "blocked"
	reactor := active("reactor")
	reactor.Declaration.Trigger = decl.Trigger{Kind: ActionTriggerBudgetExhausted, ReentryLimit: 3, HopLimit: 20, RateCeiling: "30/h"}
	reactor.Declaration.Condition = "true"
	reactor.Declaration.StartNode = decl.Node{Name: blocked.Declaration.LandingNode.Name, Deadline: "none"}
	// A distinct landing node so the node-scoped budget on blocked's own
	// landing node does not also (correctly, but not what this test is
	// about) apply to the reactor's own dispatch.
	reactor.Declaration.LandingNode = decl.Node{Name: "notified", Deadline: "1h"}

	m := &budgetMemoryBackend{
		budgets: []Budget{{Scope: BudgetScopeNode, Key: blocked.Declaration.LandingNode.Name, MaxSessions: 1}},
		spend:   map[string]BudgetSpend{"node/" + blocked.Declaration.LandingNode.Name: {Sessions: 1}},
	}
	m.active = []ActiveDeclaration{blocked, reactor}
	calls := 0
	e := newBudgetTestEngine(t, m, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
		calls++
		return DispatchResult{}, nil
	}))
	ev := Event{NamespaceID: "ns", ID: "e1", Kind: "pr-upkeep.pr", Node: "ready", Variables: map[string]any{"priority": "High"}}
	if err := e.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	// blocked's own dispatch never ran; reactor's did, once, for the
	// emitted action.budget_exhausted event.
	if calls != 1 {
		t.Fatalf("dispatch called %d times, want exactly 1 (the reactor, not the blocked declaration)", calls)
	}
	var reactorFired bool
	for _, s := range m.steps {
		if s.DeclarationID == "reactor" && s.Outcome == OutcomeFired {
			reactorFired = true
		}
	}
	if !reactorFired {
		t.Fatal("reactor declaration never fired off the emitted action.budget_exhausted trigger")
	}
}

// A budget with headroom lets dispatch proceed and charges the spend that
// later firings under the same scope will see.
func TestBudgetWithHeadroomDispatchesAndCharges(t *testing.T) {
	a := budgetActive("actor://thor")
	m := &budgetMemoryBackend{
		budgets:      []Budget{{Scope: BudgetScopeNode, Key: "waiting", MaxSessions: 2}},
		aliasMembers: map[string][]string{"budgeted": {"jira-chain"}},
	}
	calls := 0
	e := newBudgetTestEngine(t, m, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
		calls++
		return DispatchResult{}, nil
	}))
	ev := Event{NamespaceID: "ns", ID: "e1", Variables: map[string]any{"priority": "High"}}
	if err := e.evaluate(context.Background(), ev, a, "", nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("dispatch called %d times, want 1", calls)
	}
	if len(m.charged) != 1 {
		t.Fatalf("charged %d times, want 1", len(m.charged))
	}
	c := m.charged[0]
	if c.node != "waiting" || c.machine != "actor://thor" || c.declarationID != "budgeted" {
		t.Errorf("charge %+v does not name the firing's own scopes", c)
	}
	if len(m.emitted) != 0 {
		t.Error("no budget was exhausted, so no action.budget_exhausted should have been emitted")
	}
	last := m.steps[len(m.steps)-1]
	if last.Outcome != OutcomeFired {
		t.Fatalf("outcome %q, want %q", last.Outcome, OutcomeFired)
	}
}

// The uncached-input axis blocks on the same footing as sessions, reusing
// ADR 0011's second unit unchanged.
func TestBudgetBlocksOnUncachedInput(t *testing.T) {
	a := budgetActive("actor://thor")
	m := &budgetMemoryBackend{
		budgets: []Budget{{Scope: BudgetScopeDeclaration, Key: "budgeted", MaxUncachedInput: 1000}},
		spend:   map[string]BudgetSpend{"declaration/budgeted": {UncachedInputTokens: 1000}},
	}
	calls := 0
	e := newBudgetTestEngine(t, m, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
		calls++
		return DispatchResult{}, nil
	}))
	ev := Event{NamespaceID: "ns", ID: "e1", Variables: map[string]any{"priority": "High"}}
	if err := e.evaluate(context.Background(), ev, a, "", nil); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("dispatched past an exhausted max_uncached_input ceiling")
	}
	last := m.steps[len(m.steps)-1]
	if last.Outcome != OutcomeBudgetBlocked {
		t.Fatalf("outcome %q, want %q", last.Outcome, OutcomeBudgetBlocked)
	}
}

// ADR 0011 §2: a budget that cannot be READ resolves neither way -- the
// error travels back rather than either silently dispatching (spending money
// the author forbade) or silently blocking (routing the workflow down its
// exhaustion branch on the strength of a database hiccup).
func TestBudgetReadErrorNeitherDispatchesNorBlocksSilently(t *testing.T) {
	a := budgetActive("actor://thor")
	wantErr := errors.New("boom")
	m := &budgetMemoryBackend{applicableErr: wantErr}
	calls := 0
	e := newBudgetTestEngine(t, m, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
		calls++
		return DispatchResult{}, nil
	}))
	ev := Event{NamespaceID: "ns", ID: "e1", Variables: map[string]any{"priority": "High"}}
	err := e.evaluate(context.Background(), ev, a, "", nil)
	if err == nil {
		t.Fatal("want an error when applicable budgets cannot be read")
	}
	if calls != 0 {
		t.Fatal("dispatched despite a budget read error")
	}
	last := m.steps[len(m.steps)-1]
	if last.Outcome == OutcomeBudgetBlocked {
		t.Fatal("a read error must not be recorded as a routable block")
	}
}

// A Backend that does not implement BudgetBackend (every fixture in this
// package before task t11) keeps dispatching every firing, unaffected.
func TestBudgetNoBudgetBackendIsANoOp(t *testing.T) {
	a := active("A")
	m := &memoryBackend{}
	calls := 0
	e := newTestEngine(t, m, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
		calls++
		return DispatchResult{}, nil
	}))
	ev := Event{NamespaceID: "ns", ID: "e1", Variables: map[string]any{"priority": "High"}}
	if err := e.evaluate(context.Background(), ev, a, "", nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("a backend with no budget support must dispatch normally")
	}
}
