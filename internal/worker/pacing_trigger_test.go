package worker_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/pacing"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// Task t6 (plan jira-driven-idea-to-shipped-loop, spec c34/h19): "the
// always-on loop is budget-bounded -- a burst of Jira/ready-event-triggered
// dispatches consults the shipped session-window pacing so the window is
// never overdrawn and the excess queues; the pacing decision is visible per
// dispatch."
//
// MEASURED, NOT BUILT. Dispatch pacing (task t10, issue #48 item 2) sits at
// the worker's single dispatch site -- dispatch.go's dispatchActor, one step
// behind the breaker and one step ahead of the actor invocation -- and every
// CLAIMED work item passes through it, regardless of how its run was
// created. A run a trigger creates reaches the identical
// tx.EnqueueWork(...) call a run engine.CreateRun makes:
// internal/engine/trigger.go's createTriggeredRunTx and engine.go's
// CreateRun both end in e.dispatchNode, which is where EnqueueWork lives
// (internal/engine/humantask.go). Nothing downstream of that -- ClaimWork,
// the breaker, the per-actor concurrency ceiling (t16), pacing, the actor
// invocation -- reads how the run came to exist. So a burst of triggered
// runs was already paced before this task; these tests exist to PIN that
// fact against the real inbound path (Store.DeliverSignalEvent, the same
// call internal/api/signalevents.go's handleDeliverEvent makes), the way
// pacing_test.go already pins the same guarantee for engine.CreateRun.
// Measuring found no gap at this seam, so no production wiring changes
// with this task -- see the task's final report for where that was checked.

// publishTriggerWorkflow makes cw a trigger CANDIDATE without creating a
// run: runEventTriggers (internal/store/postgres/eventtriggers.go) only
// offers a delivered fact to workflow_versions rows that already exist, and
// that row is normally reached by creating a run or by POST
// /v1alpha1/workflows -- neither of which a pacing test wants to go through
// just to make the definition visible to the trigger path. This is
// internal/engine/trigger_test.go's publishFixtureWorkflow, adapted to the
// worker package's harness.
func publishTriggerWorkflow(t *testing.T, h *harness, name string) *compiler.CompiledWorkflow {
	t.Helper()
	cw := h.compile(name)
	es, err := storepg.NewEngineStore(h.store, h.ns.ID)
	if err != nil {
		t.Fatalf("NewEngineStore: %v", err)
	}
	if _, err := es.EnsureWorkflowVersion(h.ctx, engine.WorkflowVersionInput{
		WorkflowKey:   cw.Name,
		SourceFormat:  string(cw.Format),
		Source:        string(cw.Source),
		NormalizedIR:  cw.Normalized,
		ContentDigest: cw.Digest,
	}); err != nil {
		t.Fatalf("publish trigger workflow %s: %v", name, err)
	}
	return cw
}

// triggerRun delivers one inbound event through the real ingest path
// (Store.DeliverSignalEvent with Trigger set to the harness's live
// *engine.Engine) and returns the run it created. subject is a distinct
// correlation key per call -- e.g. a Jira issue key -- so each delivery in a
// burst creates a genuinely NEW run rather than attaching to one already in
// flight (task t15, spec c31/h16): that dedup is a different feature from
// the one under test here, and holding subjects apart keeps the two from
// interacting.
func (h *harness) triggerRun(eventName, subject, payload string) engine.TriggeredRun {
	h.t.Helper()
	delivery, err := h.store.DeliverSignalEvent(h.ctx, storepg.DeliverSignalEventInput{
		NamespaceID: h.ns.ID,
		Name:        eventName,
		Payload:     json.RawMessage(payload),
		Emitter:     "test",
		Subject:     subject,
		SourceKey:   subject,
		Watermark:   json.RawMessage(fmt.Sprintf(`{"seq":%q}`, subject)),
		Trigger:     h.engine,
	})
	if err != nil {
		h.t.Fatalf("DeliverSignalEvent(subject=%s): %v", subject, err)
	}
	if len(delivery.Triggered) != 1 {
		h.t.Fatalf("DeliverSignalEvent(subject=%s) triggered %d runs, want exactly 1: %+v",
			subject, len(delivery.Triggered), delivery.Triggered)
	}
	triggered := delivery.Triggered[0]
	if triggered.RunID == "" {
		h.t.Fatalf("DeliverSignalEvent(subject=%s) produced no run id: %+v", subject, triggered)
	}
	return triggered
}

// ageDispatchRateWindow simulates the passage of at least one full window
// (and opens the paced slot) by rewriting the durable rate row directly,
// exactly as internal/store/postgres/dispatchrate_test.go's
// TestDispatchRateWindowRollsWithoutASweep and openTheNextSlot do: pacing's
// "now" comes from the DATABASE clock (ConsumeDispatchSlots reads it inside
// the locking transaction), not from any clock a test can inject, so
// driving it the way its existing tests do means ageing the STATE rather
// than the clock -- window_started_at moves back by a full window (so the
// roll-is-a-comparison logic in pacing.Config.Decide sees a stale window
// and resets consumption to zero) and next_dispatch_at moves into the past
// (so the independent spacing check does not itself refuse).
func ageDispatchRateWindow(t *testing.T, h *harness, scope, scopeKey string) {
	t.Helper()
	if _, err := h.store.Pool().Exec(h.ctx, `
		UPDATE dispatch_rate_state
		SET window_started_at = window_started_at - make_interval(secs => window_seconds),
		    next_dispatch_at  = now() - interval '1 second'
		WHERE namespace_id = $1 AND scope = $2 AND scope_key = $3
	`, h.ns.ID, scope, scopeKey); err != nil {
		t.Fatalf("age dispatch_rate_state %s/%s: %v", scope, scopeKey, err)
	}
}

// TestATriggeredBurstDrainsNoFasterThanTheDeclaredRate is c34/h19's headline
// acceptance, written as the same before/after comparison
// TestABacklogDrainsNoFasterThanTheDeclaredRate uses for a manually created
// backlog: the SAME burst of trigger-created runs, with and without a
// declared rate, so the assertion cannot pass by accident. Without a rate
// every triggered run's entry node dispatches as fast as the loop can claim
// it (documented, not a bug); with one, exactly one session goes out per
// window and the rest -- created by a burst of matching events, not by the
// test calling CreateRun -- queue rather than overdraw it.
func TestATriggeredBurstDrainsNoFasterThanTheDeclaredRate(t *testing.T) {
	const burst = 4

	// Two sessions across a ten-second window: one now, the next in five
	// seconds -- well outside the 1.5s observation span, so the assertion is
	// about the rate and not about how fast the test machine claims work.
	paced := newHarness(t, respondCompleted, withGlobalPacing(pacedAt(2, 10*time.Second)))
	publishTriggerWorkflow(t, paced, "trigger-agent.workflow.yaml")
	for i := 0; i < burst; i++ {
		paced.triggerRun("test.jira-ready", fmt.Sprintf("JIRA-PACED-%d", i), `{"subject":"widget"}`)
	}
	if got := tickFor(t, paced, 1500*time.Millisecond); got != 1 {
		t.Errorf("a burst of %d triggered runs drained %d dispatches in 1.5s under a 1-per-5s rate, want 1",
			burst, got)
	}

	unpaced := newHarness(t, respondCompleted)
	publishTriggerWorkflow(t, unpaced, "trigger-agent.workflow.yaml")
	for i := 0; i < burst; i++ {
		unpaced.triggerRun("test.jira-ready", fmt.Sprintf("JIRA-UNPACED-%d", i), `{"subject":"widget"}`)
	}
	if got := tickFor(t, unpaced, 1500*time.Millisecond); got != burst {
		t.Errorf("the same triggered burst with no rate declared drained %d dispatches, want all %d "+
			"(the comparison is the test: pacing must be what made the difference)", got, burst)
	}
}

// TestATriggeredBurstsExcessQueuesObservably is c34/h19's second half: the
// dispatch a burst's rate has no headroom for is not merely absent from the
// invocation count (that could be a stall, a crash, or a lost event) -- it
// is a real work item, still 'ready', with its dispatch counter untouched,
// and a real event on ITS run's own stream naming why and when it will be
// looked at again. This is TestAPacedDispatchDefersWithoutSpendingAnything's
// assertion set, driven by a triggered burst instead of two direct
// createRun calls.
func TestATriggeredBurstsExcessQueuesObservably(t *testing.T) {
	h := newHarness(t, respondCompleted, withGlobalPacing(pacedAt(1, time.Hour)))
	publishTriggerWorkflow(t, h, "trigger-agent.workflow.yaml")

	first := h.triggerRun("test.jira-ready", "JIRA-1", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(first.RunID).State.Terminal() })
	if got := len(h.invocations()); got != 1 {
		t.Fatalf("the first triggered run made %d invocations, want 1", got)
	}

	deferred := h.triggerRun("test.jira-ready", "JIRA-2", `{"subject":"widget"}`)
	before := len(h.invocations())
	for i := 0; i < 5; i++ {
		if _, err := h.worker.Tick(h.ctx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// No overdraw: the second triggered run's dispatch never reached the
	// actor.
	if got := len(h.invocations()); got != before {
		t.Errorf("actor was invoked %d more times past the declared rate, want 0", got-before)
	}
	if state := h.run(deferred.RunID).State; state.Terminal() {
		t.Errorf("paced triggered run state = %s, want still live: pacing must never fail the work", state)
	}

	// The excess QUEUES, not vanishes: the work item is back in 'ready' with
	// available_at pushed forward and the dispatch budget given back.
	state, availableAt, attempt := workItemAvailability(t, h, deferred.RunID, "work")
	if state != "ready" {
		t.Errorf("paced triggered work item state = %q, want %q: a deferral releases the item, it does not drop it",
			state, "ready")
	}
	if !availableAt.After(time.Now()) {
		t.Errorf("paced triggered available_at = %s, want pushed into the future", availableAt)
	}
	if attempt != 0 {
		t.Errorf("paced triggered work item attempt = %d, want 0: a paced deferral must not spend the "+
			"dispatch budget", attempt)
	}

	// The pacing decision is visible PER DISPATCH, on the triggered run's own
	// event stream, not merely inferable from an absent invocation.
	types := runEventTypes(t, h, deferred.RunID)
	if !hasEvent(types, worker.TypeDispatchPaced) {
		t.Fatalf("triggered run events = %v, want %s recorded", types, worker.TypeDispatchPaced)
	}
	data := runEventData(t, h, deferred.RunID, worker.TypeDispatchPaced)
	if data["reason"] != pacing.ReasonWindowBudget {
		t.Errorf("%s payload reason = %v, want %q", worker.TypeDispatchPaced, data["reason"], pacing.ReasonWindowBudget)
	}
	if data["scope"] != "global" {
		t.Errorf("%s payload scope = %v, want the scope that refused", worker.TypeDispatchPaced, data["scope"])
	}
	if data["available_at"] == nil || data["retry_at"] == nil {
		t.Errorf("%s payload = %v, want when the item will be looked at again", worker.TypeDispatchPaced, data)
	}
	if data["limit"] == nil || data["dispatched"] == nil || data["window_ends_at"] == nil {
		t.Errorf("%s payload = %v, want the rate and the window it was measured against", worker.TypeDispatchPaced, data)
	}
}

// TestATriggeredBurstsDeferredDispatchProceedsAfterTheWindowResets is c34/
// h19's third half: pacing is a pure function of (config, state, now), so
// what makes the deferred dispatch go is the window resetting, and nothing
// else -- no retry, no operator action, no re-triggering the event. The
// window is rolled by rewriting the durable state the arithmetic reads
// (ageDispatchRateWindow), the same technique dispatchrate_test.go's own
// window-roll test uses, because pacing's "now" is the database's clock and
// no test clock this harness injects reaches it.
func TestATriggeredBurstsDeferredDispatchProceedsAfterTheWindowResets(t *testing.T) {
	h := newHarness(t, respondCompleted, withGlobalPacing(pacedAt(1, time.Hour)))
	publishTriggerWorkflow(t, h, "trigger-agent.workflow.yaml")

	first := h.triggerRun("test.jira-ready", "JIRA-A", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(first.RunID).State.Terminal() })

	deferred := h.triggerRun("test.jira-ready", "JIRA-B", `{"subject":"widget"}`)
	for i := 0; i < 3; i++ {
		if _, err := h.worker.Tick(h.ctx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}
	if state := h.run(deferred.RunID).State; state.Terminal() {
		t.Fatalf("second triggered run reached %s before the window reset; the test's premise (it was paced) "+
			"does not hold", state)
	}
	if got := len(h.invocations()); got != 1 {
		t.Fatalf("invocations before the window reset = %d, want 1 (only the first triggered run)", got)
	}

	// Nothing about the run, the event, or an operator changed -- only the
	// window's own clock moved past the reset the first dispatch's slot was
	// measured against.
	ageDispatchRateWindow(t, h, "global", "")

	// deferForPacing's own deferral floor is MaxPacingDeferral (5 real
	// minutes, breaker.go's MaxDeferralHorizon reasoning): the work item's
	// available_at was set from the WALL clock at defer time, independent of
	// the rate window, precisely so a five-hour window reset stays
	// observable within minutes rather than being invisible for hours (see
	// pacing.go's doc comment). A test proving the window reset is what
	// unblocks the dispatch -- not simply that five wall-clock minutes
	// eventually pass -- opens the claim window the same way
	// breaker_test.go's and clarifygate_test.go's equivalents do, by
	// setting available_at to now directly.
	if _, err := h.store.Pool().Exec(h.ctx,
		`UPDATE work_items SET available_at = now() WHERE id = $1`, workItemOf(t, h, deferred.RunID, "work"),
	); err != nil {
		t.Fatalf("open the claim window for the deferred triggered work item: %v", err)
	}

	h.runUntil(20*time.Second, func() bool { return h.run(deferred.RunID).State.Terminal() })
	if got := len(h.invocations()); got != 2 {
		t.Errorf("invocations after the window reset = %d, want 2 (the deferred triggered run proceeded)", got)
	}
}
