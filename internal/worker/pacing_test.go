package worker_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/pacing"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// Dispatch pacing at the dispatch site (issue #48 item 2, task t10; spec
// claims c5/c43, honesty conditions h4/h36).
//
// The behaviour being changed is deliberate today and documented as such:
// "A tick that found work claims again immediately: a backlog should drain at
// the speed of dispatch, not at the speed of the poll" (worker.go's Run).
// That is exactly right for work that costs nothing and exactly wrong for
// work that starts a billable provider session, which is what the 2026-08
// fan-out paid to discover. So h4's acceptance is stated against a backlog:
// with a rate declared, it must drain no faster than the rate.
//
// These run the real loop against a real PostgreSQL and a real HTTP actor,
// the same harness budget_test.go and breaker_test.go use.

// pacedAt builds a pacing configuration whose window starts (near enough)
// now, so the test runs at the head of a window with the whole budget
// available -- the reset-clock arithmetic itself is unit-tested in
// internal/pacing, and what these tests are about is enforcement.
func pacedAt(limit int, window time.Duration) pacing.Config {
	return pacing.Config{
		Limit:  limit,
		Window: window,
		Anchor: time.Now().UTC().Truncate(time.Microsecond),
	}
}

func withGlobalPacing(cfg pacing.Config) harnessOption {
	return func(o *worker.Options) { o.Pacing = worker.PacingOptions{Global: cfg} }
}

func withActorPacing(actorKey string, cfg pacing.Config) harnessOption {
	return func(o *worker.Options) {
		o.Pacing = worker.PacingOptions{ActorOverrides: map[string]pacing.Config{actorKey: cfg}}
	}
}

// ok answers a synchronous success, the ordinary path -- nothing about
// pacing depends on what the actor says.
func respondCompleted(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
	writeSyncResult(w, "completed", `{"score":0.91,"summary":"fine"}`)
}

// tickFor drives the loop for a fixed span of wall time, the way Run would,
// and reports how many invocations reached the actor.
func tickFor(t *testing.T, h *harness, d time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := h.worker.Tick(h.ctx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return len(h.invocations())
}

// TestABacklogDrainsNoFasterThanTheDeclaredRate is h4's second half, and it
// is written as a comparison so that it cannot pass for the wrong reason: the
// SAME backlog, on the same harness code, with and without a declared rate.
// Without one the loop drains it as fast as it can dispatch (which is the
// documented behaviour, not a bug); with one, one session goes out and the
// rest wait for their slots.
func TestABacklogDrainsNoFasterThanTheDeclaredRate(t *testing.T) {
	const backlog = 4

	// Two sessions across a ten-second window: one now, the next in five
	// seconds. Well outside the observation span below, so the assertion is
	// about the rate and not about how fast the test machine is.
	paced := newHarness(t, respondCompleted, withGlobalPacing(pacedAt(2, 10*time.Second)))
	for i := 0; i < backlog; i++ {
		paced.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	}
	if got := tickFor(t, paced, 1500*time.Millisecond); got != 1 {
		t.Errorf("a backlog of %d drained %d sessions in 1.5s under a 1-per-5s rate, want 1", backlog, got)
	}

	unpaced := newHarness(t, respondCompleted)
	for i := 0; i < backlog; i++ {
		unpaced.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	}
	if got := tickFor(t, unpaced, 1500*time.Millisecond); got != backlog {
		t.Errorf("the same backlog with no rate declared drained %d sessions, want all %d "+
			"(the comparison is the test: pacing must be what made the difference)", got, backlog)
	}
}

// The deferral is explainable and costs the work item nothing: t9's posture,
// applied to a second reason. A paced dispatch is not a failed attempt, not a
// parked node, and not a spent dispatch budget -- it is the same work, later.
func TestAPacedDispatchDefersWithoutSpendingAnything(t *testing.T) {
	h := newHarness(t, respondCompleted, withGlobalPacing(pacedAt(1, time.Hour)))

	first := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(first.ID).State.Terminal() })
	if got := len(h.invocations()); got != 1 {
		t.Fatalf("the first run made %d invocations, want 1", got)
	}

	deferred := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	before := len(h.invocations())
	for i := 0; i < 5; i++ {
		if _, err := h.worker.Tick(h.ctx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := len(h.invocations()); got != before {
		t.Errorf("actor was invoked %d more times past the declared rate, want 0", got-before)
	}
	if state := h.run(deferred.ID).State; state.Terminal() {
		t.Errorf("paced run state = %s, want still live: pacing must never fail the work", state)
	}

	state, availableAt, attempt := workItemAvailability(t, h, deferred.ID, "analyze")
	if state != "ready" {
		t.Errorf("paced work item state = %q, want %q: a deferral releases the item", state, "ready")
	}
	if !availableAt.After(time.Now()) {
		t.Errorf("paced available_at = %s, want pushed into the future", availableAt)
	}
	if attempt != 0 {
		t.Errorf("paced work item attempt = %d, want 0: a paced deferral is not a dispatch and must not "+
			"spend the dispatch budget", attempt)
	}

	// And it says so, in the run's own event stream, with everything an
	// operator needs to answer "why is this sitting still".
	types := runEventTypes(t, h, deferred.ID)
	if !hasEvent(types, worker.TypeDispatchPaced) {
		t.Fatalf("paced run events = %v, want %s recorded", types, worker.TypeDispatchPaced)
	}
	data := runEventData(t, h, deferred.ID, worker.TypeDispatchPaced)
	if data["reason"] != pacing.ReasonWindowBudget {
		t.Errorf("%s payload reason = %v, want %q", worker.TypeDispatchPaced, data["reason"], pacing.ReasonWindowBudget)
	}
	if data["scope"] != "global" {
		t.Errorf("%s payload scope = %v, want the scope that refused", worker.TypeDispatchPaced, data["scope"])
	}
	if data["available_at"] == nil || data["retry_at"] == nil {
		t.Errorf("%s payload = %v, want when the item will be looked at again", worker.TypeDispatchPaced, data)
	}
	if data["actor_key"] != "company/analyzer" {
		t.Errorf("%s payload actor_key = %v, want the actor the dispatch was addressed to",
			worker.TypeDispatchPaced, data["actor_key"])
	}
	if data["limit"] == nil || data["dispatched"] == nil || data["window_ends_at"] == nil {
		t.Errorf("%s payload = %v, want the rate and the window it was measured against",
			worker.TypeDispatchPaced, data)
	}
}

// A per-actor rate paces that actor and nothing else. The scope key is the
// actor KEY, mirroring how t9 keyed availability -- so it applies to every
// registration revision of the same identity.
func TestAPerActorRateIsHonoredAndRecordedUnderTheActorKey(t *testing.T) {
	h := newHarness(t, respondCompleted, withActorPacing("company/analyzer", pacedAt(1, time.Hour)))

	first := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(first.ID).State.Terminal() })

	h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	before := len(h.invocations())
	for i := 0; i < 3; i++ {
		if _, err := h.worker.Tick(h.ctx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}
	if got := len(h.invocations()); got != before {
		t.Errorf("the actor was invoked %d more times past its own declared rate, want 0", got-before)
	}

	rates, err := h.store.ListDispatchRates(h.ctx, h.ns.ID)
	if err != nil {
		t.Fatalf("ListDispatchRates: %v", err)
	}
	if len(rates) != 1 {
		t.Fatalf("dispatch rate rows = %+v, want exactly the actor scope", rates)
	}
	if rates[0].Scope != "actor" || rates[0].ScopeKey != "company/analyzer" {
		t.Errorf("rate row scope = %q/%q, want actor/company/analyzer", rates[0].Scope, rates[0].ScopeKey)
	}
	if rates[0].Dispatched != 1 {
		t.Errorf("rate row dispatched = %d, want 1", rates[0].Dispatched)
	}
}

// With no pacing configured, nothing changes at all: no rows, no deferrals,
// no events. This is the path every existing deployment is on.
func TestUnconfiguredPacingChangesNothing(t *testing.T) {
	h := newHarness(t, respondCompleted)

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	rates, err := h.store.ListDispatchRates(h.ctx, h.ns.ID)
	if err != nil {
		t.Fatalf("ListDispatchRates: %v", err)
	}
	if len(rates) != 0 {
		t.Errorf("unconfigured pacing wrote %d rate rows, want none: %+v", len(rates), rates)
	}
	if types := runEventTypes(t, h, run.ID); hasEvent(types, worker.TypeDispatchPaced) {
		t.Errorf("run events = %v, want no pacing event when no rate is declared", types)
	}
}
