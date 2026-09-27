package declengine

import (
	"context"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// mustGraphRun inserts a minimal runs row carrying triggerEventID, the same
// fixture shape internal/store/postgres's own claiming tests use for a run
// that does not need a real compiled workflow.
func mustGraphRun(t *testing.T, db *postgres.Store, namespaceID, triggerEventID string) string {
	t.Helper()
	ctx := context.Background()
	wv, err := db.CreateWorkflowVersion(ctx, postgres.CreateWorkflowVersionInput{
		NamespaceID:   namespaceID,
		WorkflowKey:   "tca-switch-test-" + store.NewULID(),
		Version:       1,
		SourceFormat:  "yaml",
		Source:        "entrypoint: intake\n",
		ContentDigest: "sha256:" + store.NewULID(),
	})
	if err != nil {
		t.Fatalf("mustGraphRun: CreateWorkflowVersion: %v", err)
	}
	runID := store.NewULID()
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO runs (id, namespace_id, workflow_version_id, trigger_event_id) VALUES ($1, $2, $3, $4)`,
		runID, namespaceID, wv.ID, triggerEventID,
	); err != nil {
		t.Fatalf("mustGraphRun: insert run: %v", err)
	}
	return runID
}

// Acceptance 1: switch values before/shadow/after, global per namespace,
// every flip recorded.
func TestPostgresSwitchFlipsAreGlobalPerNamespaceAndRecorded(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-switch")
	other := pgtest.MustNamespace(t, db, "tca-switch-other")
	sw := PostgresSwitchStore{Store: db}

	// A namespace that has never flipped reads as 'before'.
	mode, err := sw.Mode(ctx, ns.ID)
	if err != nil || mode != ModeBefore {
		t.Fatalf("initial mode=%q err=%v, want before", mode, err)
	}

	for _, flip := range []struct{ mode, actor string }{
		{ModeShadow, "human:ops"},
		{ModeAfter, "human:ops"},
		{ModeBefore, "human:ops"}, // rollback path (t18 will freeze; this task only records the flip)
	} {
		previous, err := sw.Flip(ctx, ns.ID, flip.mode, flip.actor, "test flip")
		if err != nil {
			t.Fatalf("Flip(%q): %v", flip.mode, err)
		}
		got, err := sw.Mode(ctx, ns.ID)
		if err != nil || got != flip.mode {
			t.Fatalf("Mode after Flip(%q) = %q err=%v", flip.mode, got, err)
		}
		_ = previous
	}

	// The switch is per namespace, not deployment-wide: a flip on ns must
	// not move other.
	if got, err := sw.Mode(ctx, other.ID); err != nil || got != ModeBefore {
		t.Fatalf("unrelated namespace mode=%q err=%v, want before", got, err)
	}

	// Every flip is recorded: the append-only history has one row per
	// Flip call, oldest first, and it is immutable.
	rows, err := db.Pool().Query(ctx, `SELECT mode,actor FROM engine_switch_history WHERE namespace_id=$1 ORDER BY seq`, ns.ID)
	if err != nil {
		t.Fatal(err)
	}
	var modes []string
	for rows.Next() {
		var m, actor string
		if err := rows.Scan(&m, &actor); err != nil {
			t.Fatal(err)
		}
		if actor != "human:ops" {
			t.Fatalf("recorded actor=%q, want human:ops", actor)
		}
		modes = append(modes, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{ModeShadow, ModeAfter, ModeBefore}
	if len(modes) != len(want) {
		t.Fatalf("recorded flips=%v, want %v", modes, want)
	}
	for i := range want {
		if modes[i] != want[i] {
			t.Fatalf("recorded flips=%v, want %v", modes, want)
		}
	}

	if _, err := db.Pool().Exec(ctx, `UPDATE engine_switch_history SET mode='after' WHERE namespace_id=$1`, ns.ID); err == nil {
		t.Fatal("engine_switch_history row was mutable")
	}

	if _, err := sw.Flip(ctx, ns.ID, "sideways", "human:ops", ""); err == nil {
		t.Fatal("Flip accepted an invalid mode")
	}
	if _, err := sw.Flip(ctx, ns.ID, ModeAfter, "", ""); err == nil {
		t.Fatal("Flip accepted an empty actor")
	}
}

// Acceptance 2, end to end against real Postgres: in shadow the
// declaration engine dispatches zero actions, and the would-fire firing's
// record references the graph run that handled the same event.
func TestPostgresShadowDispatchesZeroActionsAndRecordsGraphRun(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-shadow")

	event, err := db.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{NamespaceID: ns.ID, Name: "test.shadow", Emitter: "test"})
	if err != nil {
		t.Fatal(err)
	}
	// The graph engine's real run for the same event -- shadow derives
	// lineage from this, never from a marker it never stamps.
	graphRun := mustGraphRun(t, db, ns.ID, event.Event.ID)

	d := active("shadow-test").Declaration
	body, err := d.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.PublishDeclaration(ctx, postgres.PublishDeclarationInput{NamespaceID: ns.ID, Name: "shadow-test", Body: body, Author: "test"})
	if err != nil {
		t.Fatal(err)
	}
	firing, created, err := db.RecordDeclarationFiring(ctx, postgres.DeclarationFiringInput{
		NamespaceID: ns.ID, EventID: event.Event.ID, DeclarationID: version.DeclarationID, DeclarationVersion: version.ID,
		TriggerDigest: "trigger", ConditionDigest: "condition", ActionDigest: "action",
	})
	if err != nil || !created {
		t.Fatalf("RecordDeclarationFiring: created=%v err=%v", created, err)
	}

	underlyingCalls := 0
	underlying := dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
		underlyingCalls++
		return DispatchResult{}, nil
	})
	sw := PostgresSwitchStore{Store: db}
	if _, err := sw.Flip(ctx, ns.ID, ModeShadow, "human:ops", "start shadow"); err != nil {
		t.Fatal(err)
	}
	gate := ShadowGate{Switch: sw, Underlying: underlying, GraphRuns: PostgresGraphRunLookup{Store: db}, ShadowLine: PostgresShadowLineageStore{Store: db}}

	result, err := gate.Dispatch(ctx, DispatchRequest{Firing: firing})
	if err != nil {
		t.Fatal(err)
	}
	if underlyingCalls != 0 {
		t.Fatalf("underlying dispatcher called %d times in shadow, want 0", underlyingCalls)
	}
	if result.ArtifactID != "" || result.Async {
		t.Fatalf("shadow dispatch result not empty: %+v", result)
	}
	// No actor invocation, no worker run beyond the graph engine's own: no
	// second runs row was created for this firing (the real dispatcher,
	// which alone would create one keyed by firing.ID, was never called).
	var runsForFiring int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM runs WHERE namespace_id=$1 AND id=$2`, ns.ID, firing.ID).Scan(&runsForFiring); err != nil {
		t.Fatal(err)
	}
	if runsForFiring != 0 {
		t.Fatalf("shadow dispatch created a run for the firing: count=%d", runsForFiring)
	}

	var gotRun string
	if err := db.Pool().QueryRow(ctx, `SELECT graph_run_id FROM declaration_shadow_lineage WHERE namespace_id=$1 AND firing_id=$2`, ns.ID, firing.ID).Scan(&gotRun); err != nil {
		t.Fatal(err)
	}
	if gotRun != graphRun {
		t.Fatalf("shadow lineage graph_run_id=%q, want %q", gotRun, graphRun)
	}

	// A later firing continuing the same chain resolves the graph run back
	// to this would-fire firing -- the lineage derivation c80 asks for.
	backFiring, err := PostgresShadowLineageStore{Store: db}.GraphRunFiring(ctx, ns.ID, graphRun)
	if err != nil {
		t.Fatal(err)
	}
	if backFiring != firing.ID {
		t.Fatalf("GraphRunFiring(%q) = %q, want %q", graphRun, backFiring, firing.ID)
	}

	// Flipping to 'after' makes the same request dispatch exactly once.
	if _, err := sw.Flip(ctx, ns.ID, ModeAfter, "human:ops", "flip after parity"); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Dispatch(ctx, DispatchRequest{Firing: firing}); err != nil {
		t.Fatal(err)
	}
	if underlyingCalls != 1 {
		t.Fatalf("underlying dispatcher called %d times after flip to after, want exactly 1", underlyingCalls)
	}
}
