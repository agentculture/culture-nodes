package engine_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Task t15 (spec c31/h16): "at most one active run per originating Jira
// issue -- a second state change or comment while a flow is mid-flight must
// resume or queue against the existing run, never spawn a parallel run on
// the same subject". Measuring internal/engine/trigger.go's TriggerEvent at
// HEAD found it created one run per matching event unconditionally -- no
// subject/correlation concept existed anywhere on the inbound event path
// (internal/compiler's trigger struct, internal/engine's PickupEvent, or
// internal/store/postgres's SignalEvent). These tests exercise the fix
// through the exact path a real deliverer uses: Store.DeliverSignalEvent
// with Trigger set to a real *engine.Engine, the same call
// internal/api/signalevents.go's handleDeliverEvent makes.

// publishFixtureWorkflow makes f's compiled workflow a trigger CANDIDATE:
// runEventTriggers (internal/store/postgres/eventtriggers.go) only offers a
// delivered fact to workflow_versions rows that already exist, and
// EnsureWorkflowVersion is normally reached by creating a run
// (engine.Engine.CreateRun) or by POST /v1alpha1/workflows
// (handlePublishWorkflow) — neither of which this test wants to go through
// just to make the definition visible. Calling EnsureWorkflowVersion
// directly, the way handlePublishWorkflow does, publishes the SAME row
// without creating a run or a token.
func publishFixtureWorkflow(t *testing.T, f *fixture) {
	t.Helper()
	es, err := storepg.NewEngineStore(f.store, f.ns.ID)
	if err != nil {
		t.Fatalf("NewEngineStore: %v", err)
	}
	if _, err := es.EnsureWorkflowVersion(f.ctx, engine.WorkflowVersionInput{
		WorkflowKey:   f.cw.Name,
		SourceFormat:  string(f.cw.Format),
		Source:        string(f.cw.Source),
		NormalizedIR:  f.cw.Normalized,
		ContentDigest: f.cw.Digest,
	}); err != nil {
		t.Fatalf("publish fixture workflow: %v", err)
	}
}

func deliverSubjectEvent(t *testing.T, f *fixture, subject, sourceKey string) storepg.SignalDelivery {
	t.Helper()
	delivery, err := f.store.DeliverSignalEvent(f.ctx, storepg.DeliverSignalEventInput{
		NamespaceID: f.ns.ID,
		Name:        "test.subject-event",
		Payload:     json.RawMessage(`{}`),
		Emitter:     "test",
		Subject:     subject,
		// A distinct SourceKey/Watermark per call: that pair's idempotency is
		// for an emitter's exact redelivery of the SAME fact, which is not
		// what this test is about -- these are meant to be recorded as two
		// different, genuinely new facts.
		SourceKey: sourceKey,
		Watermark: json.RawMessage(`{"seq":"` + sourceKey + `"}`),
		Trigger:   f.engine,
	})
	if err != nil {
		t.Fatalf("DeliverSignalEvent(subject=%s, sourceKey=%s): %v", subject, sourceKey, err)
	}
	return delivery
}

// TestTriggerEventAttachesSecondEventToExistingRunForSameSubject is the
// acceptance test for h16: two events on one subject during a single flight
// yield exactly one active run, and the second event's effect is visible on
// that run rather than a sibling.
func TestTriggerEventAttachesSecondEventToExistingRunForSameSubject(t *testing.T) {
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)

	first := deliverSubjectEvent(t, f, "SCRUM-1", "state-change")
	if len(first.Triggered) != 1 {
		t.Fatalf("first delivery: want 1 triggered entry, got %d (%+v)", len(first.Triggered), first.Triggered)
	}
	if first.Triggered[0].Attached {
		t.Fatalf("first delivery for a fresh subject must create a NEW run, not attach: %+v", first.Triggered[0])
	}
	runID := first.Triggered[0].RunID
	if runID == "" {
		t.Fatal("first delivery reported an empty run id")
	}

	second := deliverSubjectEvent(t, f, "SCRUM-1", "comment")
	if len(second.Triggered) != 1 {
		t.Fatalf("second delivery: want 1 triggered entry, got %d (%+v)", len(second.Triggered), second.Triggered)
	}
	if !second.Triggered[0].Attached {
		t.Fatalf("second delivery on the SAME subject during the SAME flight must attach, not create a sibling: %+v", second.Triggered[0])
	}
	if second.Triggered[0].RunID != runID {
		t.Fatalf("second delivery attached to a different run: got %s, want the first run %s", second.Triggered[0].RunID, runID)
	}

	// Acceptance: "Two events on one issue during a single flight yield
	// exactly one active run in the run list."
	runCount := f.countScalar(`SELECT COUNT(*)::int FROM runs WHERE namespace_id = $1 AND subject = $2`, f.ns.ID, "SCRUM-1")
	if runCount != 1 {
		t.Fatalf("want exactly 1 run recorded for subject SCRUM-1, got %d", runCount)
	}

	run := f.run(runID)
	if run.State.Terminal() {
		t.Fatalf("the run the second event should have attached to is unexpectedly terminal: %s", run.State)
	}
	if run.Subject != "SCRUM-1" {
		t.Fatalf("run.Subject = %q, want SCRUM-1", run.Subject)
	}

	// Acceptance: "The second event's effect is visible on the existing run,
	// not a sibling." Confirmed two ways -- the run count above (no sibling
	// exists to be visible on), and here: the SECOND event's own audit trace
	// lands on THIS run's own stream, in order beside everything else that
	// happened to it.
	types := f.eventTypes(runID)
	var attachedCount int
	for _, typ := range types {
		if typ == engine.TypeTriggerEventAttached {
			attachedCount++
		}
	}
	if attachedCount != 1 {
		t.Fatalf("want exactly 1 %s event on run %s, got %d (stream: %v)", engine.TypeTriggerEventAttached, runID, attachedCount, types)
	}
}

// TestTriggerEventDoesNotDedupAcrossDifferentSubjects proves the guard is
// scoped to the subject, not a blanket "one run per workflow": two DIFFERENT
// Jira issues must still get two independent runs.
func TestTriggerEventDoesNotDedupAcrossDifferentSubjects(t *testing.T) {
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)

	a := deliverSubjectEvent(t, f, "SCRUM-1", "a")
	b := deliverSubjectEvent(t, f, "SCRUM-2", "b")

	if len(a.Triggered) != 1 || len(b.Triggered) != 1 {
		t.Fatalf("want 1 triggered entry each, got %d and %d", len(a.Triggered), len(b.Triggered))
	}
	if a.Triggered[0].Attached || b.Triggered[0].Attached {
		t.Fatalf("different subjects must each create their own run: %+v %+v", a.Triggered[0], b.Triggered[0])
	}
	if a.Triggered[0].RunID == b.Triggered[0].RunID {
		t.Fatalf("different subjects produced the SAME run id %s", a.Triggered[0].RunID)
	}
}

// TestTriggerEventCreatesNewRunOnceSubjectRunIsTerminal proves the guard is
// a FLIGHT dedup, not a permanent one: once the earlier run for a subject has
// finished, a later matching event is free to open a new one.
func TestTriggerEventCreatesNewRunOnceSubjectRunIsTerminal(t *testing.T) {
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)

	first := deliverSubjectEvent(t, f, "SCRUM-1", "a")
	runID := first.Triggered[0].RunID

	err := f.engine.Store().InTx(f.ctx, func(ctx context.Context, tx engine.Tx) error {
		return tx.UpdateRunState(ctx, runID, engine.RunCancelled, nil)
	})
	if err != nil {
		t.Fatalf("cancel run %s: %v", runID, err)
	}

	second := deliverSubjectEvent(t, f, "SCRUM-1", "b")
	if len(second.Triggered) != 1 {
		t.Fatalf("want 1 triggered entry, got %d (%+v)", len(second.Triggered), second.Triggered)
	}
	if second.Triggered[0].Attached {
		t.Fatalf("a terminal run must not receive an attach: %+v", second.Triggered[0])
	}
	if second.Triggered[0].RunID == runID {
		t.Fatalf("expected a brand-new run once the earlier one for this subject was terminal")
	}
}

// TestTriggerEventWithNoSubjectAlwaysCreatesANewRun is the compatibility
// floor: a caller that never sets subject (every caller that predates task
// t15) gets exactly the pre-existing behavior back -- a run every time.
func TestTriggerEventWithNoSubjectAlwaysCreatesANewRun(t *testing.T) {
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)

	deliver := func(sourceKey string) storepg.SignalDelivery {
		t.Helper()
		d, err := f.store.DeliverSignalEvent(f.ctx, storepg.DeliverSignalEventInput{
			NamespaceID: f.ns.ID,
			Name:        "test.subject-event",
			Payload:     json.RawMessage(`{}`),
			Emitter:     "test",
			SourceKey:   sourceKey,
			Watermark:   json.RawMessage(`{"seq":"` + sourceKey + `"}`),
			Trigger:     f.engine,
		})
		if err != nil {
			t.Fatalf("DeliverSignalEvent: %v", err)
		}
		return d
	}

	first := deliver("a")
	second := deliver("b")
	if first.Triggered[0].Attached || second.Triggered[0].Attached {
		t.Fatalf("no subject means no dedup: %+v %+v", first.Triggered[0], second.Triggered[0])
	}
	if first.Triggered[0].RunID == second.Triggered[0].RunID {
		t.Fatalf("subject-less deliveries must not be deduped against each other")
	}
}
