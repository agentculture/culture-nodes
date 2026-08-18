package engine_test

import (
	"encoding/json"
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Task t16 (spec c36/h21), built on t15's per-subject dedup
// (trigger_test.go): "a configurable max concurrent subject-runs per
// workflow -- at or above the configured max, the event does not create a
// run -- it lands in a visible queued/deferred state that a later event or
// run completion naturally drains". These tests exercise the ceiling and
// the drain through the exact path a real deliverer uses, the same way
// trigger_test.go does: Store.DeliverSignalEvent with Trigger set to a real
// *engine.Engine, and (for drain) Engine.CompleteAttempt through the real
// claim/complete path, because DrainSubjectTriggerQueue runs INSIDE that
// completion transaction (complete.go/humandecision.go), not in a raw
// UpdateRunState call the way trigger_test.go's own terminal-run fixture
// takes a shortcut with.

func deliverConcurrencyEvent(t *testing.T, f *fixture, subject, sourceKey string) storepg.SignalDelivery {
	t.Helper()
	delivery, err := f.store.DeliverSignalEvent(f.ctx, storepg.DeliverSignalEventInput{
		NamespaceID: f.ns.ID,
		Name:        "test.subject-concurrency-event",
		Payload:     json.RawMessage(`{}`),
		Emitter:     "test",
		Subject:     subject,
		SourceKey:   sourceKey,
		Watermark:   json.RawMessage(`{"seq":"` + sourceKey + `"}`),
		Trigger:     f.engine,
	})
	if err != nil {
		t.Fatalf("DeliverSignalEvent(subject=%s, sourceKey=%s): %v", subject, sourceKey, err)
	}
	return delivery
}

// runIDForSubject reads the run id a (namespace, subject) pair resolved to,
// once the queue has drained into one -- the fact DeliverSignalEvent's own
// response cannot report, because the run did not exist yet when the
// deferring delivery returned.
func (f *fixture) runIDForSubject(subject string) (string, bool) {
	f.t.Helper()
	var id string
	err := f.store.Pool().QueryRow(f.ctx,
		`SELECT id FROM runs WHERE namespace_id = $1 AND subject = $2`, f.ns.ID, subject,
	).Scan(&id)
	if err != nil {
		return "", false
	}
	return id, true
}

// completeReadyWork drives the run's currently-ready node run to a
// successful `completed` outcome through the real claim/complete path --
// which for this fixture's single-node graph (work -> finish, an end node)
// completes the run itself, inside the same §12.5 transaction
// DrainSubjectTriggerQueue runs in.
func (f *fixture) completeReadyWork(runID string) {
	f.t.Helper()
	nr := f.readyNodeRun(runID)
	f.step(f.actor, nr.ID, succeeded("completed", `{}`))
}

// TestTriggerEventQueuesAtConcurrencyCeilingAndDrainsOnRunCompletion is the
// acceptance test for h21's headline claim: with
// limits.maxConcurrentSubjectRuns: 2, two subjects proceed to their own
// runs, a third queues instead of creating a sibling run, and the queued
// one drains into a run once one of the first two terminates.
func TestTriggerEventQueuesAtConcurrencyCeilingAndDrainsOnRunCompletion(t *testing.T) {
	f := newFixture(t, "trigger-subject-concurrency.workflow.yaml")
	publishFixtureWorkflow(t, f)

	a := deliverConcurrencyEvent(t, f, "SCRUM-1", "a")
	b := deliverConcurrencyEvent(t, f, "SCRUM-2", "b")
	if len(a.Triggered) != 1 || len(b.Triggered) != 1 {
		t.Fatalf("want 1 triggered entry each, got %d and %d", len(a.Triggered), len(b.Triggered))
	}
	if a.Triggered[0].Deferred || b.Triggered[0].Deferred {
		t.Fatalf("the first two subjects must proceed under a max of 2: %+v %+v", a.Triggered[0], b.Triggered[0])
	}
	runA, runB := a.Triggered[0].RunID, b.Triggered[0].RunID
	if runA == "" || runB == "" || runA == runB {
		t.Fatalf("want two distinct new runs, got %q and %q", runA, runB)
	}

	// A third, DIFFERENT subject must queue rather than create a sibling
	// run: the workflow is already holding two active subject-runs, its
	// configured ceiling.
	c := deliverConcurrencyEvent(t, f, "SCRUM-3", "c")
	if len(c.Triggered) != 1 {
		t.Fatalf("want 1 triggered entry, got %d (%+v)", len(c.Triggered), c.Triggered)
	}
	if !c.Triggered[0].Deferred {
		t.Fatalf("a third subject at the ceiling must be deferred, not create a run: %+v", c.Triggered[0])
	}
	if c.Triggered[0].RunID != "" {
		t.Fatalf("a deferred trigger must report no run id, got %q", c.Triggered[0].RunID)
	}
	if c.Triggered[0].DeferredTriggerID == "" {
		t.Fatal("a deferred trigger must report the queued row's id")
	}
	if runCount := f.countScalar(`SELECT COUNT(*)::int FROM runs WHERE namespace_id = $1 AND subject = $2`, f.ns.ID, "SCRUM-3"); runCount != 0 {
		t.Fatalf("SCRUM-3 must not have a run yet, found %d", runCount)
	}
	if queued := f.countScalar(`SELECT COUNT(*)::int FROM deferred_triggers WHERE namespace_id = $1 AND subject = $2`, f.ns.ID, "SCRUM-3"); queued != 1 {
		t.Fatalf("want exactly 1 queued deferred_triggers row for SCRUM-3, got %d", queued)
	}

	// Complete run A -- one of the two active subject-runs goes terminal,
	// freeing exactly the slot SCRUM-3 is waiting on.
	f.completeReadyWork(runA)

	if run := f.run(runA); run.State != engine.RunCompleted {
		t.Fatalf("run A state = %s, want completed", run.State)
	}
	runC, ok := f.runIDForSubject("SCRUM-3")
	if !ok {
		t.Fatal("SCRUM-3 did not drain into a run once a slot freed")
	}
	if queued := f.countScalar(`SELECT COUNT(*)::int FROM deferred_triggers WHERE namespace_id = $1 AND subject = $2`, f.ns.ID, "SCRUM-3"); queued != 0 {
		t.Fatalf("SCRUM-3's queue row must be consumed once drained, still see %d", queued)
	}
	run := f.run(runC)
	if run.State.Terminal() {
		t.Fatalf("the drained run is unexpectedly terminal: %s", run.State)
	}
	if run.Subject != "SCRUM-3" {
		t.Fatalf("run.Subject = %q, want SCRUM-3", run.Subject)
	}

	// The drained run's own stream carries the evidence that it started
	// from the queue rather than at first match (task t16): there was no
	// run to record that fact against while it waited.
	var sawDrained int
	for _, typ := range f.eventTypes(runC) {
		if typ == engine.TypeTriggerQueueDrained {
			sawDrained++
		}
	}
	if sawDrained != 1 {
		t.Fatalf("want exactly 1 %s event on the drained run, got %d", engine.TypeTriggerQueueDrained, sawDrained)
	}

	// The ceiling is still respected: exactly two active subject-runs (B and
	// the drained C), never three.
	if active := f.countScalar(
		`SELECT COUNT(*)::int FROM runs WHERE namespace_id = $1 AND subject IN ('SCRUM-1','SCRUM-2','SCRUM-3') AND status NOT IN ('completed','failed','cancelled')`,
		f.ns.ID,
	); active != 2 {
		t.Fatalf("want exactly 2 active subject-runs after the drain, got %d", active)
	}
}

// TestTriggerEventAttachDedupHoldsUnderConcurrencyCeiling proves t15's
// per-issue dedup keeps working once a workflow ALSO declares
// maxConcurrentSubjectRuns: a second event on a subject that already has an
// active run attaches to it -- it must not be treated as "a new subject"
// and refused for want of ceiling headroom, and it must not queue.
func TestTriggerEventAttachDedupHoldsUnderConcurrencyCeiling(t *testing.T) {
	f := newFixture(t, "trigger-subject-concurrency.workflow.yaml")
	publishFixtureWorkflow(t, f)

	first := deliverConcurrencyEvent(t, f, "SCRUM-1", "a")
	deliverConcurrencyEvent(t, f, "SCRUM-2", "b") // saturate the ceiling (max 2)
	runID := first.Triggered[0].RunID

	second := deliverConcurrencyEvent(t, f, "SCRUM-1", "comment")
	if len(second.Triggered) != 1 {
		t.Fatalf("want 1 triggered entry, got %d (%+v)", len(second.Triggered), second.Triggered)
	}
	if second.Triggered[0].Deferred {
		t.Fatalf("a second event on an ALREADY-ACTIVE subject must attach, never queue: %+v", second.Triggered[0])
	}
	if !second.Triggered[0].Attached || second.Triggered[0].RunID != runID {
		t.Fatalf("want an attach to run %s, got %+v", runID, second.Triggered[0])
	}
	if queued := f.countScalar(`SELECT COUNT(*)::int FROM deferred_triggers WHERE namespace_id = $1`, f.ns.ID); queued != 0 {
		t.Fatalf("want no queued rows at all, got %d", queued)
	}
}

// TestTriggerEventSecondEventForAlreadyQueuedSubjectRefreshesEntry proves
// the dedup invariant extends to the QUEUE itself: two events on a subject
// that is already deferred (no run yet) must produce ONE queue row, not two
// -- refreshed with the newer event -- so draining it can never create two
// runs for the same subject.
func TestTriggerEventSecondEventForAlreadyQueuedSubjectRefreshesEntry(t *testing.T) {
	f := newFixture(t, "trigger-subject-concurrency.workflow.yaml")
	publishFixtureWorkflow(t, f)

	deliverConcurrencyEvent(t, f, "SCRUM-1", "a")
	deliverConcurrencyEvent(t, f, "SCRUM-2", "b") // saturate the ceiling (max 2)

	first := deliverConcurrencyEvent(t, f, "SCRUM-3", "c")
	if !first.Triggered[0].Deferred {
		t.Fatalf("SCRUM-3 must queue at the ceiling: %+v", first.Triggered[0])
	}
	firstID := first.Triggered[0].DeferredTriggerID

	second := deliverConcurrencyEvent(t, f, "SCRUM-3", "d")
	if !second.Triggered[0].Deferred {
		t.Fatalf("a second event for an already-queued subject must still be deferred, not create a run: %+v", second.Triggered[0])
	}
	if second.Triggered[0].DeferredTriggerID != firstID {
		t.Fatalf("want the SAME queue row refreshed (%s), got a different one (%s)", firstID, second.Triggered[0].DeferredTriggerID)
	}
	if queued := f.countScalar(`SELECT COUNT(*)::int FROM deferred_triggers WHERE namespace_id = $1 AND subject = $2`, f.ns.ID, "SCRUM-3"); queued != 1 {
		t.Fatalf("want exactly 1 queued row for SCRUM-3, got %d", queued)
	}

	var attempts int
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT attempts FROM deferred_triggers WHERE id = $1`, firstID).Scan(&attempts); err != nil {
		f.t.Fatalf("read attempts: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("want attempts = 2 after the refresh, got %d", attempts)
	}

	// Draining it must produce exactly one run for SCRUM-3, from the
	// FRESHEST event (sourceKey "d"), not two runs.
	runA, _ := f.runIDForSubject("SCRUM-1")
	f.completeReadyWork(runA)

	if runCount := f.countScalar(`SELECT COUNT(*)::int FROM runs WHERE namespace_id = $1 AND subject = $2`, f.ns.ID, "SCRUM-3"); runCount != 1 {
		t.Fatalf("want exactly 1 run for SCRUM-3 after the drain, got %d", runCount)
	}
	runC, ok := f.runIDForSubject("SCRUM-3")
	if !ok {
		t.Fatal("SCRUM-3 did not drain into a run once a slot freed")
	}
	run := f.run(runC)
	var triggerEventID string
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT (data->>'trigger_event_id') FROM events WHERE aggregate_id = $1 AND event_type = $2`,
		run.ID, engine.TypeRunCreated,
	).Scan(&triggerEventID); err != nil {
		f.t.Fatalf("read the drained run's trigger_event_id: %v", err)
	}
	if triggerEventID != second.Event.ID {
		t.Fatalf("the drained run must pin the FRESHEST queued event %s, got %s", second.Event.ID, triggerEventID)
	}
}
