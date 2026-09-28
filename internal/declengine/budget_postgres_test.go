package declengine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// insertDeclarationBudget writes one migration-0063 row directly: task t11
// intentionally ships no authoring API in this task (out of scope; only the
// enforcement path is), so tests exercise the schema the way an operator
// tool will once one exists.
func insertDeclarationBudget(t *testing.T, db *postgres.Store, ns, scope, key string, maxSessions *int, maxUncachedInput *int64) {
	t.Helper()
	_, err := db.Pool().Exec(context.Background(),
		`INSERT INTO declaration_budgets(id,namespace_id,scope,scope_key,max_sessions,max_uncached_input) VALUES($1,$2,$3,$4,$5,$6)`,
		store.NewULID(), ns, scope, key, maxSessions, maxUncachedInput)
	if err != nil {
		t.Fatal(err)
	}
}

func intp(v int) *int { return &v }

// declarationIDFor resolves a published declaration's catalog id by name --
// outcomes() and the budget tables key on it, not on the human-readable name.
func declarationIDFor(t *testing.T, db *postgres.Store, ns, name string) string {
	t.Helper()
	var id string
	if err := db.Pool().QueryRow(context.Background(), `SELECT id FROM declarations WHERE namespace_id=$1 AND name=$2`, ns, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Acceptance 1 and 2, end to end against Postgres: a node-scoped session
// budget of 1 lets the first firing dispatch and blocks the second, with a
// visible declaration_evaluations record and an action.budget_exhausted
// signal_events row.
func TestPostgresBudgetBlocksDispatchAndEmitsTrigger(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-budget")

	d := active("budget-node-test").Declaration
	d.Condition = "true"
	d.Action.With = json.RawMessage(`{"uses":"actor://budget-test","input":{}}`)
	publishActive(t, db, ns.ID, d)

	insertDeclarationBudget(t, db, ns.ID, "node", d.LandingNode.Name, intp(1), nil)

	t.Setenv("TCA_PG_BUDGET_KEY", strings.Repeat("k", 32))
	calls := 0
	e, err := New(Config{MarkerKeyEnv: "TCA_PG_BUDGET_KEY"}, PostgresBackend{db}, PostgresMarkerStore{db},
		dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
			calls++
			return DispatchResult{}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}

	declID := declarationIDFor(t, db, ns.ID, "budget-node-test")

	event1 := deliver(t, db, ns.ID)
	ev1 := Event{NamespaceID: ns.ID, ID: event1, Kind: "pr-upkeep.pr", Node: "ready", Variables: map[string]any{"priority": "High"}}
	if err := e.Handle(ctx, ev1); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("first firing: dispatch called %d times, want 1", calls)
	}
	got := outcomes(t, db, ns.ID, event1, declID)
	if got[len(got)-1] != OutcomeFired {
		t.Fatalf("first firing outcomes %v, want it to end fired", got)
	}

	// Second firing, a fresh event: the node budget is now spent (1 of 1
	// sessions), so this one is blocked before the actor would have been
	// invoked.
	event2 := deliver(t, db, ns.ID)
	ev2 := Event{NamespaceID: ns.ID, ID: event2, Kind: "pr-upkeep.pr", Node: "ready", Variables: map[string]any{"priority": "High"}}
	if err := e.Handle(ctx, ev2); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("second firing: dispatch called %d times, want still 1 (blocked)", calls)
	}
	got = outcomes(t, db, ns.ID, event2, declID)
	if got[len(got)-1] != OutcomeBudgetBlocked {
		t.Fatalf("second firing outcomes %v, want it to end %q", got, OutcomeBudgetBlocked)
	}

	// The visible record: an action.budget_exhausted signal_events row,
	// linked to the landing node by name, exists for this namespace.
	var count int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM signal_events WHERE namespace_id=$1 AND name=$2`,
		ns.ID, ActionTriggerBudgetExhausted).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("%d action.budget_exhausted signal events, want exactly 1", count)
	}
	var payload []byte
	if err := db.Pool().QueryRow(ctx, `SELECT payload FROM signal_events WHERE namespace_id=$1 AND name=$2`,
		ns.ID, ActionTriggerBudgetExhausted).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		NodeName string `json:"node_name"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.NodeName != d.LandingNode.Name {
		t.Errorf("emitted event names node %q, want the landing node %q", decoded.NodeName, d.LandingNode.Name)
	}
	if decoded.Reason == "" {
		t.Error("emitted event carries no reason")
	}
}

// Machine, declaration and alias scoped budgets each independently apply,
// and ApplicableBudgets/BudgetSpend/ChargeBudgetSpend read and write exactly
// what task t11's acceptance criterion 1 requires -- exercised directly
// against Postgres rather than through the in-memory fake.
func TestPostgresApplicableBudgetsCoverAllFourScopes(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-budget-scopes")

	d := active("budget-scopes-test").Declaration
	publishActive(t, db, ns.ID, d)
	declID := declarationIDFor(t, db, ns.ID, "budget-scopes-test")

	alias, err := db.CreateDeclarationAlias(ctx, ns.ID, "budget-chain")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddDeclarationToAlias(ctx, ns.ID, alias.Name, declID); err != nil {
		t.Fatal(err)
	}

	insertDeclarationBudget(t, db, ns.ID, "node", "waiting", intp(3), nil)
	insertDeclarationBudget(t, db, ns.ID, "machine", "actor://budget-test", intp(5), nil)
	insertDeclarationBudget(t, db, ns.ID, "declaration", declID, intp(7), nil)
	insertDeclarationBudget(t, db, ns.ID, "alias", "budget-chain", intp(9), nil)
	// A budget for an unrelated node must not come back as applicable.
	insertDeclarationBudget(t, db, ns.ID, "node", "unrelated-node", intp(1), nil)

	backend := PostgresBackend{db}
	budgets, err := backend.ApplicableBudgets(ctx, ns.ID, "waiting", "actor://budget-test", declID)
	if err != nil {
		t.Fatal(err)
	}
	if len(budgets) != 4 {
		t.Fatalf("got %d applicable budgets, want exactly 4 (one per scope): %+v", len(budgets), budgets)
	}
	byScope := map[BudgetScope]Budget{}
	for _, b := range budgets {
		byScope[b.Scope] = b
	}
	cases := []struct {
		scope BudgetScope
		key   string
		max   int
	}{
		{BudgetScopeNode, "waiting", 3},
		{BudgetScopeMachine, "actor://budget-test", 5},
		{BudgetScopeDeclaration, declID, 7},
		{BudgetScopeAlias, "budget-chain", 9},
	}
	for _, c := range cases {
		got, ok := byScope[c.scope]
		if !ok {
			t.Fatalf("missing %s budget in %+v", c.scope, budgets)
		}
		if got.Key != c.key || got.MaxSessions != c.max {
			t.Errorf("%s budget = %+v, want key=%q max_sessions=%d", c.scope, got, c.key, c.max)
		}
	}

	// Before any charge, spend is zero on every scope.
	for _, c := range cases {
		spend, err := backend.BudgetSpend(ctx, ns.ID, c.scope, c.key)
		if err != nil {
			t.Fatal(err)
		}
		if spend.Sessions != 0 {
			t.Errorf("%s spend before any charge = %+v, want 0 sessions", c.scope, spend)
		}
	}

	event, err := db.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{NamespaceID: ns.ID, Name: "test.charge", Emitter: "test"})
	if err != nil {
		t.Fatal(err)
	}
	var versionID string
	if err := db.Pool().QueryRow(ctx, `SELECT id FROM declaration_versions WHERE declaration_id=$1 ORDER BY version DESC LIMIT 1`, declID).Scan(&versionID); err != nil {
		t.Fatal(err)
	}
	firingID := store.NewULID()
	if _, err := db.Pool().Exec(ctx, `INSERT INTO declaration_firings(id,namespace_id,event_id,declaration_id,declaration_version,trigger_digest,condition_digest,action_digest,lineage_id,canonical_firing_id)
		VALUES($1,$2,$3,$4,$5,'t','c','a',$1,$1)`,
		firingID, ns.ID, event.Event.ID, declID, versionID); err != nil {
		t.Fatal(err)
	}

	if err := backend.ChargeBudgetSpend(ctx, ns.ID, firingID, "waiting", "actor://budget-test", declID); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		spend, err := backend.BudgetSpend(ctx, ns.ID, c.scope, c.key)
		if err != nil {
			t.Fatal(err)
		}
		if spend.Sessions != 1 {
			t.Errorf("%s spend after one charge = %+v, want 1 session", c.scope, spend)
		}
	}
	// Charging the same firing again is idempotent (ON CONFLICT DO NOTHING).
	if err := backend.ChargeBudgetSpend(ctx, ns.ID, firingID, "waiting", "actor://budget-test", declID); err != nil {
		t.Fatal(err)
	}
	spend, err := backend.BudgetSpend(ctx, ns.ID, BudgetScopeNode, "waiting")
	if err != nil {
		t.Fatal(err)
	}
	if spend.Sessions != 1 {
		t.Errorf("re-charging the same firing spent %d sessions, want still 1", spend.Sessions)
	}

	// A budget for an unrelated declaration is never applicable to this one
	// (declaration- and alias-scoped budgets do not leak across declarations
	// that never joined that alias).
	other := active("budget-scopes-other").Declaration
	publishActive(t, db, ns.ID, other)
	otherID := declarationIDFor(t, db, ns.ID, "budget-scopes-other")
	otherBudgets, err := backend.ApplicableBudgets(ctx, ns.ID, "waiting", "actor://budget-test", otherID)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range otherBudgets {
		if b.Scope == BudgetScopeDeclaration || b.Scope == BudgetScopeAlias {
			t.Errorf("declaration- or alias-scoped budget %+v wrongly applies to an unrelated declaration", b)
		}
	}
}

// TestPostgresBudgetSpendUncachedInputSumsAcrossFirings exercises
// BudgetSpend's second unit against real attempts rows, joined the same way
// postgres.RunUncachedInput joins them for a single run -- here summed
// across every firing charged against one scope, and charging an attempt
// with no cached figure the ADR 0011 §5 way: in full.
func TestPostgresBudgetSpendUncachedInputSumsAcrossFirings(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-budget-uncached")

	d := active("budget-uncached-test").Declaration
	publishActive(t, db, ns.ID, d)
	declID := declarationIDFor(t, db, ns.ID, "budget-uncached-test")

	var versionID string
	if err := db.Pool().QueryRow(ctx, `SELECT id FROM declaration_versions WHERE declaration_id=$1 ORDER BY version DESC LIMIT 1`, declID).Scan(&versionID); err != nil {
		t.Fatal(err)
	}
	wfVersionID := store.NewULID()
	if _, err := db.Pool().Exec(ctx, `INSERT INTO workflow_versions(id,namespace_id,workflow_key,version,source_format,source,normalized_ir,content_digest)
		VALUES($1,$2,'budget-uncached-wf',1,'json','{}','{}'::jsonb,$1)`, wfVersionID, ns.ID); err != nil {
		t.Fatal(err)
	}

	// Two firings charged against the same node budget, each with one
	// attempt: the first reports a cached figure (charged for the
	// difference), the second reports usage with no cached figure at all
	// (charged in full -- ADR 0011 §5's honesty rule) and a third attempt
	// on a THIRD firing reports no usage at all (excluded from the sum, but
	// counted as not-reported).
	type fixture struct {
		inputTokens, cachedTokens *int64
	}
	cached := int64(5000)
	fixtures := []fixture{
		{inputTokens: ptr64(10000), cachedTokens: &cached}, // charged 5000
		{inputTokens: ptr64(3000), cachedTokens: nil},      // charged 3000 in full
		{inputTokens: nil, cachedTokens: nil},              // not reported at all
	}
	for i, f := range fixtures {
		event, err := db.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{NamespaceID: ns.ID, Name: "test.uncached", Emitter: "test"})
		if err != nil {
			t.Fatal(err)
		}
		firingID := store.NewULID()
		if _, err := db.Pool().Exec(ctx, `INSERT INTO declaration_firings(id,namespace_id,event_id,declaration_id,declaration_version,trigger_digest,condition_digest,action_digest,lineage_id,canonical_firing_id)
			VALUES($1,$2,$3,$4,$5,'t','c','a',$1,$1)`, firingID, ns.ID, event.Event.ID, declID, versionID); err != nil {
			t.Fatal(err)
		}
		// runs.id doubles as the firing id (dispatch.go's convention).
		if _, err := db.Pool().Exec(ctx, `INSERT INTO runs(id,namespace_id,workflow_version_id,status) VALUES($1,$2,$3,'completed')`,
			firingID, ns.ID, wfVersionID); err != nil {
			t.Fatal(err)
		}
		nodeRunID := store.NewULID()
		if _, err := db.Pool().Exec(ctx, `INSERT INTO node_runs(id,namespace_id,run_id,node_key,status) VALUES($1,$2,$3,'action','completed')`,
			nodeRunID, ns.ID, firingID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool().Exec(ctx, `INSERT INTO attempts(id,namespace_id,node_run_id,attempt_number,status,usage_input_tokens,usage_cached_input_tokens)
			VALUES($1,$2,$3,1,'succeeded',$4,$5)`, store.NewULID(), ns.ID, nodeRunID, f.inputTokens, f.cachedTokens); err != nil {
			t.Fatal(err)
		}
		backend := PostgresBackend{db}
		if err := backend.ChargeBudgetSpend(ctx, ns.ID, firingID, "uncached-node", "", declID); err != nil {
			t.Fatal(err)
		}
		_ = i
	}

	backend := PostgresBackend{db}
	spend, err := backend.BudgetSpend(ctx, ns.ID, BudgetScopeNode, "uncached-node")
	if err != nil {
		t.Fatal(err)
	}
	if spend.Sessions != 3 {
		t.Errorf("sessions = %d, want 3 (one per charged firing)", spend.Sessions)
	}
	if spend.UncachedInputTokens != 5000+3000 {
		t.Errorf("uncached input tokens = %d, want %d", spend.UncachedInputTokens, 5000+3000)
	}
	if spend.AttemptsWithoutCacheTelemetry != 1 {
		t.Errorf("attempts without cache telemetry = %d, want 1", spend.AttemptsWithoutCacheTelemetry)
	}
	if spend.AttemptsNotReported != 1 {
		t.Errorf("attempts not reported = %d, want 1", spend.AttemptsNotReported)
	}
}

func ptr64(v int64) *int64 { return &v }
