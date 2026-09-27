package declengine

import (
	"context"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// t13 (#328, spec c88, honesty h61): "why did (or didn't) declaration X
// fire for event E" must be answerable from a store/engine query. Each of
// these covers one outcome kind the firing loop's evaluate() pipeline can
// record, plus the two placeholder outcomes (budget-blocked, overlap-
// suppressed) whose producers are other, still-in-flight tasks (t11, t14):
// this task only needs Explain to read them correctly once something else
// writes them, so those two rows are seeded directly.

// mustExplain resolves and asserts Explain found a row, returning it.
func mustExplain(t *testing.T, db *postgres.Store, ns, eventID, name string) ExplainResult {
	t.Helper()
	r, found, err := (PostgresBackend{db}).Explain(context.Background(), ns, eventID, name)
	if err != nil {
		t.Fatalf("Explain(%q): %v", name, err)
	}
	if !found {
		t.Fatalf("Explain(%q) found=false, want a recorded evaluation", name)
	}
	return r
}

func TestExplainFired(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-explain-fired").ID

	d := active("explain-fired").Declaration
	d.Condition = "true"
	version := publishActive(t, db, ns, d)
	eventID := deliver(t, db, ns)

	e := newExplainEngine(t, db, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
		return DispatchResult{Variables: map[string]any{"pr": "1"}}, nil
	}))
	if err := e.Handle(ctx, Event{NamespaceID: ns, ID: eventID, Kind: "timer", Node: "ready"}); err != nil {
		t.Fatal(err)
	}

	got := mustExplain(t, db, ns, eventID, d.Name)
	if got.Outcome != OutcomeFired || got.Reason == "" {
		t.Fatalf("explain=%+v, want outcome %q with a reason", got, OutcomeFired)
	}
	if got.FiringID == "" {
		t.Fatalf("explain=%+v, want a firing id for a fired evaluation", got)
	}
	if got.DeclarationID != version.DeclarationID {
		t.Fatalf("explain declaration_id=%q, want %q", got.DeclarationID, version.DeclarationID)
	}
}

func TestExplainConditionFalse(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-explain-condfalse").ID

	// active()'s default condition requires event.priority == 'High'; an
	// event that carries no such variable evaluates false.
	d := active("explain-condition-false").Declaration
	publishActive(t, db, ns, d)
	eventID := deliver(t, db, ns)

	e := newExplainEngine(t, db, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
		t.Fatal("dispatched despite a false condition")
		return DispatchResult{}, nil
	}))
	if err := e.Handle(ctx, Event{NamespaceID: ns, ID: eventID, Kind: "timer", Node: "ready", Variables: map[string]any{"priority": "Low"}}); err != nil {
		t.Fatal(err)
	}

	got := mustExplain(t, db, ns, eventID, d.Name)
	if got.Outcome != OutcomeConditionFalse || got.Reason == "" {
		t.Fatalf("explain=%+v, want outcome %q with a reason", got, OutcomeConditionFalse)
	}
	if got.FiringID != "" {
		t.Fatalf("explain=%+v, want no firing id: condition false is decided before Claim", got)
	}
}

func TestExplainLineageMissing(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-explain-lineage").ID

	upstream := active("explain-lineage-upstream").Declaration
	upstream.Condition = "true"
	vUpstream := publishActive(t, db, ns, upstream)

	downstream := active("explain-lineage-downstream").Declaration
	downstream.Condition = "true"
	downstream.StartNode.Name = "gate"
	vDownstream := publishActive(t, db, ns, downstream)
	if err := db.LinkDeclarations(ctx, ns, vDownstream.DeclarationID, vUpstream.DeclarationID, "must"); err != nil {
		t.Fatal(err)
	}

	e := newExplainEngine(t, db, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
		return DispatchResult{}, nil
	}))
	// An unmarked event on downstream's start node has no upstream firing
	// in its lineage: 'must' refuses it.
	eventID := deliver(t, db, ns)
	if err := e.Handle(ctx, Event{NamespaceID: ns, ID: eventID, Kind: "timer", Node: "gate"}); err != nil {
		t.Fatal(err)
	}

	got := mustExplain(t, db, ns, eventID, downstream.Name)
	if got.Outcome != OutcomeLineageMissing || got.Reason == "" {
		t.Fatalf("explain=%+v, want outcome %q with a reason", got, OutcomeLineageMissing)
	}
	if got.FiringID != "" {
		t.Fatalf("explain=%+v, want no firing id: lineage missing is decided before Claim", got)
	}
}

func TestExplainLoopLimited(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-explain-loop").ID

	// A self-loop: the declaration's landing node is its own start node,
	// so a genuine reaction to its own firing is a direct self-retrigger,
	// refused by default (allow_self_retrigger unset).
	d := active("explain-loop").Declaration
	d.Condition = "true"
	d.StartNode.Name, d.LandingNode.Name = "loop", "loop"
	version := publishActive(t, db, ns, d)

	e := newExplainEngine(t, db, dispatchFunc(func(_ context.Context, r DispatchRequest) (DispatchResult, error) {
		return DispatchResult{ArtifactID: "artifact-" + r.Firing.ID}, nil
	}))
	first := deliver(t, db, ns)
	if err := e.Handle(ctx, Event{NamespaceID: ns, ID: first, Kind: "timer", Node: "loop"}); err != nil {
		t.Fatal(err)
	}
	firstFiring := firingByEventDecl(t, db, ns, first, version.DeclarationID)

	second := deliver(t, db, ns)
	if err := e.Handle(ctx, reactEvent(t, db, ns, second, firstFiring, "timer", nil)); err != nil {
		t.Fatal(err)
	}

	got := mustExplain(t, db, ns, second, d.Name)
	if got.Outcome != OutcomeLoopLimited || got.Reason == "" {
		t.Fatalf("explain=%+v, want outcome %q with a reason", got, OutcomeLoopLimited)
	}
}

func TestExplainDeferred(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-explain-deferred").ID

	d := active("explain-deferred").Declaration
	d.Condition = "true"
	d.Trigger.MaxConcurrentSubject = 1
	version := publishActive(t, db, ns, d)

	e := newExplainEngine(t, db, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
		return DispatchResult{}, nil
	}))
	first, second := deliver(t, db, ns), deliver(t, db, ns)
	if err := e.Handle(ctx, Event{NamespaceID: ns, ID: first, Kind: "timer", Node: "ready", Subject: "ISSUE-9"}); err != nil {
		t.Fatal(err)
	}
	if err := e.Handle(ctx, Event{NamespaceID: ns, ID: second, Kind: "timer", Node: "ready", Subject: "ISSUE-9"}); err != nil {
		t.Fatal(err)
	}

	got := mustExplain(t, db, ns, second, version.Name)
	if got.Outcome != OutcomeDeferred || got.Reason == "" {
		t.Fatalf("explain=%+v, want outcome %q with a reason", got, OutcomeDeferred)
	}
	if got.FiringID != "" {
		t.Fatalf("explain=%+v, want no firing id: deferred is decided before Claim", got)
	}
}

func TestExplainShadow(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-explain-shadow").ID

	d := active("explain-shadow").Declaration
	d.Condition = "true"
	publishActive(t, db, ns, d)
	eventID := deliver(t, db, ns)

	sw := PostgresSwitchStore{Store: db}
	if _, err := sw.Flip(ctx, ns, ModeShadow, "human:ops", "explain test"); err != nil {
		t.Fatal(err)
	}
	underlyingCalls := 0
	gate := ShadowGate{Switch: sw, Underlying: dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
		underlyingCalls++
		return DispatchResult{}, nil
	})}
	e := newExplainEngine(t, db, gate)
	if err := e.Handle(ctx, Event{NamespaceID: ns, ID: eventID, Kind: "timer", Node: "ready"}); err != nil {
		t.Fatal(err)
	}
	if underlyingCalls != 0 {
		t.Fatalf("underlying dispatcher called %d times in shadow, want 0", underlyingCalls)
	}

	got := mustExplain(t, db, ns, eventID, d.Name)
	if got.Outcome != OutcomeShadow || got.Reason == "" {
		t.Fatalf("explain=%+v, want outcome %q with a reason", got, OutcomeShadow)
	}
	if got.FiringID == "" {
		t.Fatalf("explain=%+v, want a firing id: a shadow evaluation still claims and opens a landing node", got)
	}
}

// TestExplainBudgetBlockedAndOverlapSuppressed seeds one evaluation row per
// placeholder outcome directly -- t11 (budget) and t14 (overlap) are the
// tasks that will make the firing loop actually produce these outcomes;
// this task only defines the constants and proves Explain reads them
// correctly once something does write them.
func TestExplainBudgetBlockedAndOverlapSuppressed(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-explain-placeholders").ID

	for _, tc := range []struct {
		name, declName, outcome, reason string
	}{
		{"budget", "explain-budget-blocked", OutcomeBudgetBlocked, "declaration's namespace spending cap was already reached"},
		{"overlap", "explain-overlap-suppressed", OutcomeOverlapSuppressed, "an overlapping firing for the same scope is already in flight"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := active(tc.declName).Declaration
			version := publishActive(t, db, ns, d)
			eventID := deliver(t, db, ns)
			if _, err := db.Pool().Exec(ctx, `INSERT INTO declaration_evaluations(id,namespace_id,event_id,declaration_id,declaration_version,outcome,reason)
 VALUES($1,$2,$3,$4,$5,$6,$7)`,
				store.NewULID(), ns, eventID, version.DeclarationID, version.ID, tc.outcome, tc.reason); err != nil {
				t.Fatal(err)
			}

			got := mustExplain(t, db, ns, eventID, tc.declName)
			if got.Outcome != tc.outcome || got.Reason != tc.reason {
				t.Fatalf("explain=%+v, want outcome %q reason %q", got, tc.outcome, tc.reason)
			}
			if got.FiringID != "" {
				t.Fatalf("explain=%+v, want no firing id: neither placeholder outcome claims a firing", got)
			}
		})
	}
}

func TestExplainNotFoundForUnmatchedOrUnknown(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-explain-notfound").ID

	d := active("explain-notfound-decl").Declaration
	publishActive(t, db, ns, d)
	eventID := deliver(t, db, ns)

	// The declaration is active but its trigger never matched this event
	// (wrong node), so Handle recorded nothing for it at all.
	e := newExplainEngine(t, db, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
		t.Fatal("dispatched a declaration whose trigger never matched")
		return DispatchResult{}, nil
	}))
	if err := e.Handle(ctx, Event{NamespaceID: ns, ID: eventID, Kind: "timer", Node: "some-other-node"}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := (PostgresBackend{db}).Explain(ctx, ns, eventID, d.Name); err != nil || found {
		t.Fatalf("Explain(unmatched)=found:%v err:%v, want not found", found, err)
	}
	if _, found, err := (PostgresBackend{db}).Explain(ctx, ns, eventID, "no-such-declaration"); err != nil || found {
		t.Fatalf("Explain(unknown name)=found:%v err:%v, want not found", found, err)
	}
	if _, _, err := (PostgresBackend{db}).Explain(ctx, ns, "", d.Name); err == nil {
		t.Fatal("Explain accepted an empty event id")
	}
}

// newExplainEngine builds an Engine over the real PostgresBackend/marker
// store with the given dispatcher, matching this file's tests' shared shape.
func newExplainEngine(t *testing.T, db *postgres.Store, d Dispatcher) *Engine {
	t.Helper()
	t.Setenv("TCA_EXPLAIN_KEY", strings.Repeat("x", 32))
	e, err := New(Config{MarkerKeyEnv: "TCA_EXPLAIN_KEY"}, PostgresBackend{db}, PostgresMarkerStore{db}, d)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
