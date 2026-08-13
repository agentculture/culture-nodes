package worker_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// The capacity circuit breaker (issue #48 item 1, task t9; spec claim c4,
// honesty conditions h3/h38).
//
// The live failure these tests encode: one provider limit turning into a
// cascade of failed billable sessions. Every queued work item addressed to
// an exhausted actor starts its own cold session, hits the same wall, and
// fails — and the run's own retry ladder does it again. h3's acceptance is
// stated as an absence: after a forced provider-limit failure, NO further
// dispatch reaches that actor until the pause expires, verified by observing
// a deferred available_at rather than a failed-attempt cascade.
//
// These run the real loop against a real PostgreSQL and a real HTTP actor,
// the same harness budget_test.go uses. The only thing forced is the
// bridge's own error body, which is exactly what task t8 made
// capacity_exhausted's sole entry point (internal/actors/errors.go's
// bodyDeclarableClasses).

// capacityExhausted answers a §13.5 body-declared capacity_exhausted
// failure — the shape task t5 teaches the bridges to send. The status is 429
// on purpose: task t8's classifyStatus still maps 429 to rate_limited, so a
// test that passed here on the status alone would prove nothing about the
// body-declared class.
func capacityExhausted(retryAfterSeconds int) func(*harness, http.ResponseWriter, actors.InvocationRequest) {
	return func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		w.Header().Set("Content-Type", "application/json")
		if retryAfterSeconds > 0 {
			w.Header().Set("Retry-After", fmt.Sprint(retryAfterSeconds))
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"weekly session limit reached","class":"capacity_exhausted"}`))
	}
}

// actorPause reads the breaker's durable row for an actor key.
func actorPause(t *testing.T, h *harness, actorKey string) (pausedUntil time.Time, reason string, retryAfter *int32) {
	t.Helper()
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT paused_until, reason, retry_after_seconds
		FROM actor_availability WHERE namespace_id = $1 AND actor_key = $2
	`, h.ns.ID, actorKey).Scan(&pausedUntil, &reason, &retryAfter); err != nil {
		t.Fatalf("read actor_availability for %q: %v", actorKey, err)
	}
	return pausedUntil, reason, retryAfter
}

// runEventTypes lists the audit event types recorded against a run, in order.
func runEventTypes(t *testing.T, h *harness, runID string) []string {
	t.Helper()
	rows, err := h.store.Pool().Query(h.ctx,
		`SELECT event_type FROM events WHERE aggregate_id = $1 ORDER BY sequence`, runID)
	if err != nil {
		t.Fatalf("read events for run %s: %v", runID, err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var eventType string
		if err := rows.Scan(&eventType); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		types = append(types, eventType)
	}
	return types
}

// runEventData returns the payload of the first event of a type on a run.
func runEventData(t *testing.T, h *harness, runID, eventType string) map[string]any {
	t.Helper()
	var raw []byte
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT data FROM events WHERE aggregate_id = $1 AND event_type = $2 ORDER BY sequence LIMIT 1`,
		runID, eventType).Scan(&raw); err != nil {
		t.Fatalf("read %s event for run %s: %v", eventType, runID, err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("decode %s payload: %v", eventType, err)
	}
	return data
}

func hasEvent(types []string, want string) bool {
	for _, t := range types {
		if t == want {
			return true
		}
	}
	return false
}

// workItemAvailability reads a run's work item scheduling state.
func workItemAvailability(t *testing.T, h *harness, runID, nodeKey string) (state string, availableAt time.Time, attempt int32) {
	t.Helper()
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT wi.state, wi.available_at, wi.attempt
		FROM work_items AS wi JOIN node_runs AS nr ON nr.id = wi.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = $2
	`, runID, nodeKey).Scan(&state, &availableAt, &attempt); err != nil {
		t.Fatalf("read work item for node %q: %v", nodeKey, err)
	}
	return state, availableAt, attempt
}

// TestCapacityExhaustionPausesTheActorAndZeroFurtherDispatchesReachIt is h3's
// acceptance, end to end: the forced provider-limit failure pauses the
// actor, and the NEXT run's work item is deferred — released with its
// available_at pushed forward and its dispatch counter given back — rather
// than dispatched into the same wall.
func TestCapacityExhaustionPausesTheActorAndZeroFurtherDispatchesReachIt(t *testing.T) {
	h := newHarness(t, capacityExhausted(120))

	exhausted := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(exhausted.ID).State.Terminal() })

	if got := len(h.invocations()); got != 1 {
		t.Fatalf("actor was invoked %d times for the tripping run, want 1", got)
	}

	// The attempt itself is an ordinary §13.5 technical failure carrying the
	// class — the breaker is a layer above it, not a replacement for it.
	status, result := attemptRecord(t, h, exhausted.ID, "analyze")
	if engine.TechStatus(status) != engine.StatusFailed {
		t.Errorf("attempt status = %q, want %q", status, engine.StatusFailed)
	}
	if !bytes.Contains(result, []byte(actors.ClassCapacityExhausted)) {
		t.Errorf("attempt result = %s, want the capacity_exhausted class recorded", result)
	}

	// The breaker tripped, honoring the provider's own Retry-After.
	pausedUntil, reason, retryAfter := actorPause(t, h, "company/analyzer")
	if reason != string(actors.ClassCapacityExhausted) {
		t.Errorf("pause reason = %q, want capacity_exhausted", reason)
	}
	if retryAfter == nil || *retryAfter != 120 {
		t.Errorf("retry_after_seconds = %v, want 120 (the actor's own Retry-After)", retryAfter)
	}
	if !pausedUntil.After(time.Now()) {
		t.Fatalf("paused_until = %s, want a future deadline", pausedUntil)
	}
	if types := runEventTypes(t, h, exhausted.ID); !hasEvent(types, worker.TypeActorPaused) {
		t.Errorf("run events = %v, want %s recorded", types, worker.TypeActorPaused)
	}
	paused := runEventData(t, h, exhausted.ID, worker.TypeActorPaused)
	if paused["actor_key"] != "company/analyzer" || paused["paused_until"] == nil {
		t.Errorf("%s payload = %v, want the actor key and the deadline", worker.TypeActorPaused, paused)
	}

	// Now the part h3 actually asks for: the next run must not reach the
	// actor at all.
	before := len(h.invocations())
	deferred := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)

	for i := 0; i < 5; i++ {
		if _, err := h.worker.Tick(h.ctx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := len(h.invocations()); got != before {
		t.Errorf("actor was invoked %d more times while paused, want 0 (h3: zero further dispatches)", got-before)
	}
	if state := h.run(deferred.ID).State; state.Terminal() {
		t.Errorf("deferred run state = %s, want still live: a pause must not fail the work", state)
	}

	state, availableAt, attempt := workItemAvailability(t, h, deferred.ID, "analyze")
	if state != "ready" {
		t.Errorf("deferred work item state = %q, want %q: a deferral releases the item, it does not park or fail it", state, "ready")
	}
	if !availableAt.After(time.Now()) {
		t.Errorf("deferred available_at = %s, want pushed into the future", availableAt)
	}
	if attempt != 0 {
		t.Errorf("deferred work item attempt = %d, want 0: a deferral is not a dispatch and must not spend the budget", attempt)
	}

	// And it is explainable, not a silent skip (budget.go's own warning).
	types := runEventTypes(t, h, deferred.ID)
	if !hasEvent(types, worker.TypeDispatchDeferred) {
		t.Fatalf("deferred run events = %v, want %s recorded", types, worker.TypeDispatchDeferred)
	}
	data := runEventData(t, h, deferred.ID, worker.TypeDispatchDeferred)
	if data["actor_key"] != "company/analyzer" {
		t.Errorf("%s payload actor_key = %v, want company/analyzer", worker.TypeDispatchDeferred, data["actor_key"])
	}
	if data["reason"] != string(actors.ClassCapacityExhausted) || data["available_at"] == nil {
		t.Errorf("%s payload = %v, want the reason and when it will be looked at again", worker.TypeDispatchDeferred, data)
	}
}

// The other half of h3: the pause is a DEADLINE, not a shutdown. Once it
// expires, dispatch resumes on its own with no operator action and no
// state to clean up.
func TestDispatchResumesWhenTheCapacityPauseExpires(t *testing.T) {
	var exhaust atomic.Bool
	exhaust.Store(true)
	h := newHarness(t, func(hh *harness, w http.ResponseWriter, req actors.InvocationRequest) {
		if exhaust.Load() {
			capacityExhausted(0)(hh, w, req)
			return
		}
		writeSyncResult(w, "completed", `{"score":0.91,"summary":"back in business"}`)
	})

	tripping := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(tripping.ID).State.Terminal() })

	// No Retry-After was sent, so the bounded default applies.
	pausedUntil, _, retryAfter := actorPause(t, h, "company/analyzer")
	if retryAfter != nil {
		t.Errorf("retry_after_seconds = %v, want NULL when the provider named none (never 0)", *retryAfter)
	}
	if got := time.Until(pausedUntil); got < worker.DefaultCapacityPause-time.Minute {
		t.Errorf("pause lasts %s, want about the bounded default %s", got, worker.DefaultCapacityPause)
	}

	exhaust.Store(false)
	resumed := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	before := len(h.invocations())

	// Nothing reaches the actor while the pause stands...
	for i := 0; i < 3; i++ {
		if _, err := h.worker.Tick(h.ctx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}
	if got := len(h.invocations()); got != before {
		t.Fatalf("actor was invoked %d times while paused, want 0", got-before)
	}

	// ...and expiry alone is enough to let it through. Expiring the pause is
	// the one thing simulated here: waiting out the real default would mean a
	// fifteen-minute test.
	if _, err := h.store.Pool().Exec(h.ctx,
		`UPDATE actor_availability SET paused_until = now() - interval '1 second'
		 WHERE namespace_id = $1 AND actor_key = $2`, h.ns.ID, "company/analyzer"); err != nil {
		t.Fatalf("expire the pause: %v", err)
	}
	if _, err := h.store.Pool().Exec(h.ctx,
		`UPDATE work_items SET available_at = now() WHERE id = $1`,
		workItemOf(t, h, resumed.ID, "analyze")); err != nil {
		t.Fatalf("bring the deferred item forward: %v", err)
	}

	h.runUntil(20*time.Second, func() bool { return h.run(resumed.ID).State.Terminal() })
	if state := h.run(resumed.ID).State; state != engine.RunCompleted {
		t.Fatalf("resumed run state = %s, want completed (worker errors: %v)", state, h.workerErrors())
	}
	if got := len(h.invocations()); got <= before {
		t.Errorf("actor was invoked %d times after expiry, want at least 1", got-before)
	}
}

// An operator clearing the pause early has the same effect as expiry, and it
// takes effect within one deferral horizon rather than at the original
// deadline — which is the whole reason a deferral is bounded by
// MaxDeferralHorizon instead of jumping straight to the pause deadline
// (h38's "clearable without touching the database", proven here at the store
// seam the API and CLI both call).
func TestClearingThePauseLetsDeferredWorkThroughBeforeTheOriginalDeadline(t *testing.T) {
	var exhaust atomic.Bool
	exhaust.Store(true)
	h := newHarness(t, func(hh *harness, w http.ResponseWriter, req actors.InvocationRequest) {
		if exhaust.Load() {
			capacityExhausted(3600)(hh, w, req)
			return
		}
		writeSyncResult(w, "completed", `{"score":0.91,"summary":"cleared"}`)
	})

	tripping := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(tripping.ID).State.Terminal() })

	exhaust.Store(false)
	waiting := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	if _, err := h.worker.Tick(h.ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// An hour-long pause must not defer the item for an hour: the horizon
	// bounds how long an early clear can go unnoticed.
	_, availableAt, _ := workItemAvailability(t, h, waiting.ID, "analyze")
	if wait := time.Until(availableAt); wait > worker.MaxDeferralHorizon+time.Minute {
		t.Errorf("deferred for %s, want at most the %s horizon even under an hour-long pause",
			wait, worker.MaxDeferralHorizon)
	}

	cleared, ok, err := h.store.ClearActorPause(h.ctx, h.ns.ID, "company/analyzer", "operator-test")
	if err != nil || !ok {
		t.Fatalf("ClearActorPause: (%v, %v)", ok, err)
	}
	if cleared.ClearedBy != "operator-test" {
		t.Errorf("cleared_by = %q, want the operator recorded", cleared.ClearedBy)
	}

	// Bring the already-deferred item forward the way the horizon's expiry
	// would within five minutes, then let the loop run.
	if _, err := h.store.Pool().Exec(h.ctx,
		`UPDATE work_items SET available_at = now() WHERE id = $1`,
		workItemOf(t, h, waiting.ID, "analyze")); err != nil {
		t.Fatalf("bring the deferred item forward: %v", err)
	}
	h.runUntil(20*time.Second, func() bool { return h.run(waiting.ID).State.Terminal() })
	if state := h.run(waiting.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state after the clear = %s, want completed (worker errors: %v)", state, h.workerErrors())
	}
}

// A capacity failure is the ONLY class that trips the breaker. Every other
// §13.5 failure is about this request, not about the provider's capacity,
// and pausing a whole actor over one of them would be the breaker becoming
// the outage.
func TestOtherFailureClassesDoNotPauseTheActor(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		// A plain 429 with no body-declared class: ordinary backpressure,
		// which task t8 deliberately keeps mapping to rate_limited.
		_, _ = w.Write([]byte(`{"error":"slow down"}`))
	})

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	var rows int
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT count(*) FROM actor_availability WHERE namespace_id = $1`, h.ns.ID).Scan(&rows); err != nil {
		t.Fatalf("count pauses: %v", err)
	}
	if rows != 0 {
		t.Errorf("actor_availability rows = %d, want 0: only capacity_exhausted trips the breaker", rows)
	}
	if types := runEventTypes(t, h, run.ID); hasEvent(types, worker.TypeActorPaused) {
		t.Errorf("run events = %v, want no %s for a plain 429", types, worker.TypeActorPaused)
	}
}

// The pause bounds themselves are recorded product decisions, not tuning
// knobs a refactor may drift — the same pin budget_test.go puts on
// MaxDispatchAttempts.
func TestCapacityPauseBoundsAreTheRecordedDecision(t *testing.T) {
	if worker.DefaultCapacityPause != 15*time.Minute {
		t.Errorf("DefaultCapacityPause = %s, want 15m", worker.DefaultCapacityPause)
	}
	if worker.MaxCapacityPause != 2*time.Hour {
		t.Errorf("MaxCapacityPause = %s, want 2h", worker.MaxCapacityPause)
	}
	if worker.MaxDeferralHorizon != 5*time.Minute {
		t.Errorf("MaxDeferralHorizon = %s, want 5m", worker.MaxDeferralHorizon)
	}
	if worker.MaxDeferralHorizon >= worker.MaxCapacityPause {
		t.Error("MaxDeferralHorizon must be shorter than MaxCapacityPause, or an early clear can go unnoticed for the whole pause")
	}
}

// A provider asking for an absurd delay does not get one: MaxCapacityPause
// caps whatever Retry-After says, so a misconfigured bridge cannot take an
// actor out for a day with one header.
func TestRetryAfterIsCappedAtTheMaximumPause(t *testing.T) {
	h := newHarness(t, capacityExhausted(int((48 * time.Hour).Seconds())))

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	pausedUntil, _, retryAfter := actorPause(t, h, "company/analyzer")
	if retryAfter == nil || *retryAfter != int32((48*time.Hour).Seconds()) {
		t.Errorf("retry_after_seconds = %v, want the provider's own unclamped hint recorded", retryAfter)
	}
	if got := time.Until(pausedUntil); got > worker.MaxCapacityPause+time.Minute {
		t.Errorf("pause lasts %s, want capped at %s however long the provider asked for", got, worker.MaxCapacityPause)
	}
}
