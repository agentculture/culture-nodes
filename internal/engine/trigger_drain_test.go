package engine_test

import (
	"encoding/json"
	"testing"

	"github.com/agentculture/culture-nodes/internal/declengine"
	"github.com/agentculture/culture-nodes/internal/engine"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// Task t17 (#328, spec c94, ADR 0014 "Consequences", honesty h63): the
// acceptance test for the drain guarantee -- "flipping to 'after' drains,
// it does not strand". This exercises the whole path a real deliverer uses
// (Store.DeliverSignalEvent with Trigger set to a real *engine.Engine
// built with the drain gate wired in, exactly as internal/api/server.go's
// NewServer wires it in production) rather than calling
// engine.NewRunGate/TriggerEvent directly, because the acceptance
// criterion is about what a namespace observes across a real flip, not
// about the gate's own unit behavior (see
// internal/declengine/drain_postgres_test.go for that).
func TestTriggerEventDrainsOnFlipToAfterAndOpenGraphRunCountReachesZero(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	sw := declengine.PostgresSwitchStore{Store: s}
	gate := declengine.DrainGate{Switch: sw}
	counter := declengine.PostgresOpenRunCounter{Store: s}

	f := newFixtureOn(t, s, "trigger-subject.workflow.yaml", engine.WithNewRunGate(gate))
	publishFixtureWorkflow(t, f)

	deliver := func() storepg.SignalDelivery {
		delivery, err := f.store.DeliverSignalEvent(f.ctx, storepg.DeliverSignalEventInput{
			NamespaceID: f.ns.ID,
			Name:        "test.subject-event",
			Payload:     json.RawMessage(`{}`),
			Emitter:     "test",
			Trigger:     f.engine,
		})
		if err != nil {
			t.Fatalf("DeliverSignalEvent: %v", err)
		}
		return delivery
	}

	// Before the flip: a matching event starts a brand-new graph run --
	// the entry node dispatches a work item and the run stays open,
	// exactly like today (before this task, and in 'before'/'shadow').
	before := deliver()
	if len(before.Triggered) != 1 || before.Triggered[0].RunID == "" {
		t.Fatalf("pre-flip delivery = %+v, want exactly one new run", before.Triggered)
	}
	openRunID := before.Triggered[0].RunID

	if n, err := counter.OpenGraphRunCount(f.ctx, f.ns.ID); err != nil || n != 1 {
		t.Fatalf("open run count before flip = (%d, %v), want (1, nil)", n, err)
	}

	// The flip (h53: takes effect atomically, no separate "propagation"
	// step to race).
	if _, err := sw.Flip(f.ctx, f.ns.ID, declengine.ModeAfter, "human:ops", "t17 drain acceptance test"); err != nil {
		t.Fatalf("Flip(after): %v", err)
	}

	// After the flip: a new matching event creates NO new graph run --
	// c94/h63, the declaration engine is what takes it now (out of this
	// test's scope; internal/declengine's own dispatch/marker suites cover
	// that half).
	after := deliver()
	if len(after.Triggered) != 0 {
		t.Fatalf("post-flip delivery = %+v, want zero new graph runs", after.Triggered)
	}

	// The pre-flip run is untouched by the flip: it is still open, and its
	// dispatched work item is still exactly what it was waiting for --
	// h63, "no graph run open at the flip is left without an engine".
	if n, err := counter.OpenGraphRunCount(f.ctx, f.ns.ID); err != nil || n != 1 {
		t.Fatalf("open run count after flip (before the callback) = (%d, %v), want (1, nil) -- the pre-flip run must still be open", n, err)
	}

	// Deliver the callback the still-open run was waiting for: the graph
	// engine, not the declaration engine, completes it -- the flip did not
	// strand it.
	nr := f.readyNodeRun(openRunID)
	f.step(f.actor, nr.ID, succeeded("completed", `{}`))

	if got := f.run(openRunID).State; got != engine.RunCompleted {
		t.Fatalf("run state after its callback = %q, want %q", got, engine.RunCompleted)
	}

	// The graph engine's open-run count reaches zero once every run open
	// at the flip has ended -- c94's headline claim, made checkable.
	if n, err := counter.OpenGraphRunCount(f.ctx, f.ns.ID); err != nil || n != 0 {
		t.Fatalf("open run count after the callback = (%d, %v), want (0, nil)", n, err)
	}
}

// TestDrainGateLeavesBeforeAndShadowUnchanged pins the other half of c94:
// a namespace that has never flipped, or is only in 'shadow', keeps
// creating new graph runs from triggers exactly as it did before this
// task -- the drain gate wired unconditionally in production
// (internal/api/server.go's NewServer) must never change 'before'/'shadow'
// behavior.
func TestDrainGateLeavesBeforeAndShadowUnchanged(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	sw := declengine.PostgresSwitchStore{Store: s}
	gate := declengine.DrainGate{Switch: sw}

	f := newFixtureOn(t, s, "trigger-subject.workflow.yaml", engine.WithNewRunGate(gate))
	publishFixtureWorkflow(t, f)

	deliver := func(sourceKey string) storepg.SignalDelivery {
		delivery, err := f.store.DeliverSignalEvent(f.ctx, storepg.DeliverSignalEventInput{
			NamespaceID: f.ns.ID,
			Name:        "test.subject-event",
			Payload:     json.RawMessage(`{}`),
			Emitter:     "test",
			Trigger:     f.engine,
			SourceKey:   sourceKey,
			Watermark:   json.RawMessage(`{"seq":"` + sourceKey + `"}`),
		})
		if err != nil {
			t.Fatalf("DeliverSignalEvent: %v", err)
		}
		return delivery
	}

	before := deliver("before")
	if len(before.Triggered) != 1 || before.Triggered[0].RunID == "" {
		t.Fatalf("'before' mode delivery = %+v, want exactly one new run", before.Triggered)
	}

	if _, err := sw.Flip(f.ctx, f.ns.ID, declengine.ModeShadow, "human:ops", "t17 unchanged-mode test"); err != nil {
		t.Fatalf("Flip(shadow): %v", err)
	}
	shadow := deliver("shadow")
	if len(shadow.Triggered) != 1 || shadow.Triggered[0].RunID == "" {
		t.Fatalf("'shadow' mode delivery = %+v, want exactly one new run", shadow.Triggered)
	}
}

// #328 t32 (found live in production): after the flip, a published graph
// workflow whose input contract predates a new payload key (pr-upkeep@12 and
// work_item, #310) still matched the event and failed contract validation
// BEFORE the drain gate was consulted, so the whole delivery rolled back with
// a 500 and the declaration engine never saw the event. In 'after' the graph
// engine creates no new run, so its stale contract must not refuse the fact;
// in 'before' the refusal stays exactly as it was.
func TestDrainedGraphContractDoesNotRefuseTheDelivery(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	sw := declengine.PostgresSwitchStore{Store: s}
	f := newFixtureOn(t, s, "trigger-strict.workflow.yaml", engine.WithNewRunGate(declengine.DrainGate{Switch: sw}))
	publishFixtureWorkflow(t, f)
	deliver := func() (storepg.SignalDelivery, error) {
		return f.store.DeliverSignalEvent(f.ctx, storepg.DeliverSignalEventInput{
			NamespaceID: f.ns.ID,
			Name:        "test.subject-event",
			Payload:     json.RawMessage(`{"work_item":"gh:o/r#1"}`),
			Emitter:     "test",
			Trigger:     f.engine,
		})
	}
	if _, err := deliver(); err == nil {
		t.Fatal("before the flip: a payload the graph contract refuses was delivered; want the contract error, unchanged")
	}
	if _, err := sw.Flip(f.ctx, f.ns.ID, declengine.ModeAfter, "human:ops", "t32 stale graph contract"); err != nil {
		t.Fatalf("Flip(after): %v", err)
	}
	d, err := deliver()
	if err != nil {
		t.Fatalf("after the flip: delivery failed on the drained graph engine's contract: %v", err)
	}
	if len(d.Triggered) != 0 || d.Event.ID == "" {
		t.Fatalf("after the flip: delivery = %+v, want the fact appended and zero graph runs", d)
	}
}
