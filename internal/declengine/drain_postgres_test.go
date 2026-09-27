package declengine

import (
	"context"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// Acceptance 1 (drain.go): DrainGate allows new-run creation in 'before'
// and 'shadow' -- exactly today's behavior -- and refuses only in 'after'.
func TestDrainGateAllowsBeforeAndShadowRefusesOnlyAfter(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-drain-gate")
	sw := PostgresSwitchStore{Store: db}
	gate := DrainGate{Switch: sw}

	// Never flipped: reads as 'before', new runs allowed.
	if allow, err := gate.AllowNewRun(ctx, ns.ID); err != nil || !allow {
		t.Fatalf("AllowNewRun(never flipped) = (%v, %v), want (true, nil)", allow, err)
	}

	if _, err := sw.Flip(ctx, ns.ID, ModeShadow, "human:ops", "drain test"); err != nil {
		t.Fatalf("Flip(shadow): %v", err)
	}
	if allow, err := gate.AllowNewRun(ctx, ns.ID); err != nil || !allow {
		t.Fatalf("AllowNewRun(shadow) = (%v, %v), want (true, nil)", allow, err)
	}

	if _, err := sw.Flip(ctx, ns.ID, ModeAfter, "human:ops", "drain test"); err != nil {
		t.Fatalf("Flip(after): %v", err)
	}
	if allow, err := gate.AllowNewRun(ctx, ns.ID); err != nil || allow {
		t.Fatalf("AllowNewRun(after) = (%v, %v), want (false, nil)", allow, err)
	}

	// Flipping back to 'before' (t18's rollback path) restores it.
	if _, err := sw.Flip(ctx, ns.ID, ModeBefore, "human:ops", "drain test"); err != nil {
		t.Fatalf("Flip(before): %v", err)
	}
	if allow, err := gate.AllowNewRun(ctx, ns.ID); err != nil || !allow {
		t.Fatalf("AllowNewRun(before again) = (%v, %v), want (true, nil)", allow, err)
	}

	// The gate is namespace-scoped, not deployment-wide: a namespace that
	// never flipped stays allowed regardless of another namespace's mode.
	other := pgtest.MustNamespace(t, db, "tca-drain-gate-other")
	if allow, err := gate.AllowNewRun(ctx, other.ID); err != nil || !allow {
		t.Fatalf("AllowNewRun(unrelated namespace) = (%v, %v), want (true, nil)", allow, err)
	}
}

// Acceptance 2 (drain.go + api/drain.go): PostgresOpenRunCounter counts
// non-terminal runs only, namespace-scoped, and the count reaches zero
// once every open run has ended.
func TestPostgresOpenRunCounterCountsNonTerminalRunsOnly(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-open-run-count")
	counter := PostgresOpenRunCounter{Store: db}

	if n, err := counter.OpenGraphRunCount(ctx, ns.ID); err != nil || n != 0 {
		t.Fatalf("OpenGraphRunCount(empty namespace) = (%d, %v), want (0, nil)", n, err)
	}

	triggerEvent := func(name string) string {
		t.Helper()
		ev, err := db.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{NamespaceID: ns.ID, Name: name, Emitter: "test"})
		if err != nil {
			t.Fatalf("DeliverSignalEvent(%s): %v", name, err)
		}
		return ev.Event.ID
	}
	running := mustGraphRun(t, db, ns.ID, triggerEvent("test.open-run-count.running"))
	waiting := mustGraphRun(t, db, ns.ID, triggerEvent("test.open-run-count.waiting"))
	completed := mustGraphRun(t, db, ns.ID, triggerEvent("test.open-run-count.completed"))
	if _, err := db.Pool().Exec(ctx, `UPDATE runs SET status='waiting' WHERE id=$1`, waiting); err != nil {
		t.Fatalf("set waiting: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE runs SET status='completed' WHERE id=$1`, completed); err != nil {
		t.Fatalf("set completed: %v", err)
	}
	_ = running // created state, already non-terminal by default

	if n, err := counter.OpenGraphRunCount(ctx, ns.ID); err != nil || n != 2 {
		t.Fatalf("OpenGraphRunCount = (%d, %v), want (2, nil) -- running+waiting open, completed excluded", n, err)
	}

	// Failing and cancelling the two open runs brings the count to zero,
	// the fact c94 requires be readable once the graph engine's open runs
	// have all ended.
	if _, err := db.Pool().Exec(ctx, `UPDATE runs SET status='failed' WHERE id=$1`, running); err != nil {
		t.Fatalf("set failed: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE runs SET status='cancelled' WHERE id=$1`, waiting); err != nil {
		t.Fatalf("set cancelled: %v", err)
	}
	if n, err := counter.OpenGraphRunCount(ctx, ns.ID); err != nil || n != 0 {
		t.Fatalf("OpenGraphRunCount(all terminal) = (%d, %v), want (0, nil)", n, err)
	}
}
