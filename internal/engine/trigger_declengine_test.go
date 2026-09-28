package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/declengine"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// Task t38 (#328), acceptance 1, through the path a real deliverer uses:
// Store.DeliverSignalEvent with the graph engine as Trigger (drain gate
// wired, as NewServer wires it) AND the declaration engine's Router as
// Declarations, over the production engine (declengine.NewPostgres:
// WorkerDispatcher behind ShadowGate). One event is offered to both.
//
//   - 'before': the graph engine starts its run; the declaration engine
//     records nothing at all.
//   - 'shadow': the graph engine starts its run exactly as in 'before'; the
//     declaration engine records a would-fire firing (outcome shadow) and
//     dispatches nothing -- no run keyed by the firing exists.
//   - 'after': the graph engine starts nothing (drain); the declaration
//     engine fires through WorkerDispatcher -- the firing's run exists and
//     its derived decision record is written under the registered producer.
//
// A second offer of the same delivery (the redelivery path) fires nothing
// twice.
func TestDeliveryReachesDeclarationEngineUnderTheSwitch(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ctx := context.Background()
	sw := declengine.PostgresSwitchStore{Store: s}
	f := newFixtureOn(t, s, "trigger-declengine.workflow.yaml", engine.WithNewRunGate(declengine.DrainGate{Switch: sw}))
	publishFixtureWorkflow(t, f)

	producer := store.NewULID()
	if _, err := s.Pool().Exec(ctx, `INSERT INTO actors(id,namespace_id,actor_key,revision,kind,protocol) VALUES($1,$2,'engine/declarations',1,'engine','internal')`, producer, f.ns.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool().Exec(ctx, `INSERT INTO actors(id,namespace_id,actor_key,revision,kind,protocol,capabilities) VALUES($1,$2,'test/worker',1,'agent','http','{"stamping":{"marker":"cn1","version":1}}')`, store.NewULID(), f.ns.ID); err != nil {
		t.Fatal(err)
	}
	d := decl.Declaration{Name: "on-timer", Condition: "true",
		Trigger:     decl.Trigger{Kind: "pr-upkeep.pr", ReentryLimit: 3, HopLimit: 20, RateCeiling: "30/h"},
		Action:      decl.Action{Kind: "agent.work", With: json.RawMessage(`{"uses":"actor://test/worker@sha256:aaaaaa","input":{"text":"hi"}}`)},
		StartNode:   decl.Node{Name: declengine.RootNode, Deadline: "none"},
		LandingNode: decl.Node{Name: "waiting", Deadline: "1h"}}
	body, err := d.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.PublishDeclaration(ctx, storepg.PublishDeclarationInput{NamespaceID: f.ns.ID, Name: d.Name, Body: body, Author: "human"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDeclarationActivation(ctx, f.ns.ID, v.ID, "activate", "human", ""); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TCA_T38_ENGINE_KEY", strings.Repeat("r", 32))
	eng, err := declengine.NewPostgres(declengine.Config{MarkerKeyEnv: "TCA_T38_ENGINE_KEY"}, s, producer)
	if err != nil {
		t.Fatal(err)
	}
	router := declengine.Router{Engine: eng, Switch: sw}

	deliver := func(withDeclarations bool) storepg.SignalDelivery {
		t.Helper()
		in := storepg.DeliverSignalEventInput{NamespaceID: f.ns.ID, Name: "pr-upkeep.pr", Payload: json.RawMessage(`{}`), Emitter: "test",
			Pickup: f.engine, Trigger: f.engine}
		if withDeclarations {
			in.Declarations = router
		}
		delivery, err := s.DeliverSignalEvent(ctx, in)
		if err != nil {
			t.Fatalf("DeliverSignalEvent: %v", err)
		}
		if delivery.DeclarationErr != nil {
			t.Fatalf("declaration engine: %v", delivery.DeclarationErr)
		}
		return delivery
	}
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := s.Pool().QueryRow(ctx, query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	evaluations := func(eventID string) int {
		return count(`SELECT count(*) FROM declaration_evaluations WHERE namespace_id=$1 AND event_id=$2`, f.ns.ID, eventID)
	}
	firing := func(eventID string) (id, outcome string) {
		t.Helper()
		if err := s.Pool().QueryRow(ctx, `SELECT f.id,(SELECT e.outcome FROM declaration_evaluations e WHERE e.namespace_id=f.namespace_id AND e.event_id=f.event_id AND e.declaration_id=f.declaration_id ORDER BY e.created_at DESC,e.id DESC LIMIT 1)
			FROM declaration_firings f WHERE f.namespace_id=$1 AND f.event_id=$2`, f.ns.ID, eventID).Scan(&id, &outcome); err != nil {
			t.Fatalf("firing for %s: %v", eventID, err)
		}
		return id, outcome
	}
	// graphRun reports what the graph engine did with one delivery: how many
	// runs it started and the status of the one it started.
	graphRun := func(d storepg.SignalDelivery) (int, string) {
		t.Helper()
		if len(d.Triggered) == 0 {
			return 0, ""
		}
		var status string
		if err := s.Pool().QueryRow(ctx, `SELECT status FROM runs WHERE namespace_id=$1 AND id=$2`, f.ns.ID, d.Triggered[0].RunID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		return len(d.Triggered), status
	}

	// Reference: the graph engine alone, before t38 existed.
	refRuns, refStatus := graphRun(deliver(false))
	if refRuns != 1 {
		t.Fatalf("reference delivery started %d graph runs, want 1", refRuns)
	}

	// --- before: records nothing; graph engine unchanged.
	before := deliver(true)
	if n, status := graphRun(before); n != refRuns || status != refStatus {
		t.Fatalf("'before' graph behaviour = (%d, %q), want the reference (%d, %q)", n, status, refRuns, refStatus)
	}
	if n := evaluations(before.Event.ID); n != 0 {
		t.Fatalf("'before' recorded %d declaration evaluations, want 0", n)
	}
	if n := count(`SELECT count(*) FROM declaration_firings WHERE namespace_id=$1`, f.ns.ID); n != 0 {
		t.Fatalf("'before' claimed %d firings, want 0", n)
	}

	// --- shadow: would-fire record, nothing dispatched; graph engine unchanged.
	if _, err := sw.Flip(ctx, f.ns.ID, declengine.ModeShadow, "human:ops", "t38 shadow"); err != nil {
		t.Fatal(err)
	}
	shadow := deliver(true)
	if n, status := graphRun(shadow); n != refRuns || status != refStatus {
		t.Fatalf("'shadow' graph behaviour = (%d, %q), want the reference (%d, %q)", n, status, refRuns, refStatus)
	}
	shadowFiring, outcome := firing(shadow.Event.ID)
	if outcome != declengine.OutcomeShadow {
		t.Fatalf("'shadow' terminal outcome = %q, want %q", outcome, declengine.OutcomeShadow)
	}
	if n := count(`SELECT count(*) FROM runs WHERE namespace_id=$1 AND id=$2`, f.ns.ID, shadowFiring); n != 0 {
		t.Fatalf("'shadow' dispatched: %d runs keyed by the firing", n)
	}

	// --- after: graph engine drained; declaration engine dispatches through
	// WorkerDispatcher.
	if _, err := sw.Flip(ctx, f.ns.ID, declengine.ModeAfter, "human:ops", "t38 after"); err != nil {
		t.Fatal(err)
	}
	after := deliver(true)
	if n, _ := graphRun(after); n != 0 {
		t.Fatalf("'after' started %d graph runs, want 0 (drained)", n)
	}
	afterFiring, outcome := firing(after.Event.ID)
	if outcome != declengine.OutcomeFired {
		t.Fatalf("'after' terminal outcome = %q, want %q", outcome, declengine.OutcomeFired)
	}
	if n := count(`SELECT count(*) FROM runs WHERE namespace_id=$1 AND id=$2 AND trigger_event_id=$3`, f.ns.ID, afterFiring, after.Event.ID); n != 1 {
		t.Fatalf("'after' did not dispatch through WorkerDispatcher: %d runs keyed by the firing", n)
	}
	if n := count(`SELECT count(*) FROM ledger_records WHERE run_id=$1 AND origin_actor_id=$2 AND authority='derived'`, afterFiring, producer); n != 1 {
		t.Fatalf("firing's derived decision records under the producer = %d, want 1", n)
	}

	// Idempotent on the event id: offering the same delivery again (a
	// redelivery, or a retry after a crash between commit and handler)
	// claims nothing new and dispatches nothing new.
	if err := router.HandleDeliveredEvent(ctx, after); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM declaration_firings WHERE namespace_id=$1 AND event_id=$2`, f.ns.ID, after.Event.ID); n != 1 {
		t.Fatalf("redelivery claimed %d firings, want still 1", n)
	}
	if n := count(`SELECT count(*) FROM runs WHERE namespace_id=$1 AND trigger_event_id=$2`, f.ns.ID, after.Event.ID); n != 1 {
		t.Fatalf("redelivery dispatched again: %d runs for the event", n)
	}
}
