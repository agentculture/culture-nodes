package worker_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// Code-review fix D on the liveness gate (plan loop-closure, decision c43).
//
// What was wrong, in one sentence each, and what each test pins:
//
//  1. A LOCK-mode bridge latches session_ok=false with a FIXED checked_at,
//     so at T+5m01s the row went "stale, therefore live" and the dead lane
//     was leased again forever — a LOCK false row of any age now refuses.
//  2. Production codex is async: the credential_spent class arrives through
//     the callback handler, which never locked — it does now, through the
//     harness's real ingest route.
//  3. The gate ran AFTER the breaker, capacity, clarify and pacing gates,
//     which had all keyed on the PRIMARY: a paused fallback was dispatched
//     anyway. The lane is decided first; the per-actor gates see the
//     fallback.
//  4. actor_invocations.actor_ref named the primary for a dispatch that
//     went to the fallback, so a later async credential_spent on the
//     fallback would have locked the wrong lane.

// postCallbackEvent sends one §13.4 event through the harness's real
// callback ingest and returns the disposition.
func postCallbackEvent(t *testing.T, callback actors.Callback, ev actors.CallbackEvent) string {
	t.Helper()
	body, _ := json.Marshal(ev)
	req, err := http.NewRequest(http.MethodPost, callback.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build callback request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+callback.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST callback: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("callback %s answered %d, want 202", ev.Kind, resp.StatusCode)
	}
	var decoded struct {
		Disposition string `json:"disposition"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return decoded.Disposition
}

func credentialSpentEvent(id string) actors.CallbackEvent {
	payload, _ := json.Marshal(actors.FailedPayload{
		Class:   actors.ClassCredentialSpent,
		Message: "Your access token could not be refreshed because your refresh token was revoked",
	})
	return actors.CallbackEvent{EventID: id, Sequence: 1, Kind: actors.EventFailed, Payload: payload}
}

// asyncFallbackActor is newFallbackActor's async sibling: it answers 202
// and hands the test the callback block it was given.
type asyncFallbackActor struct {
	server   *httptest.Server
	mu       sync.Mutex
	callback actors.Callback
	hits     int
}

func newAsyncFallbackActor(t *testing.T) *asyncFallbackActor {
	t.Helper()
	fb := &asyncFallbackActor{}
	fb.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req actors.InvocationRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		fb.mu.Lock()
		fb.callback = req.Callback
		fb.hits++
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"invocation_id":"inv-fallback","heartbeat_after_seconds":30,"supports_cancellation":true}`))
	}))
	t.Cleanup(fb.server.Close)
	return fb
}

func (fb *asyncFallbackActor) captured() (actors.Callback, int) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.callback, fb.hits
}

func tick(t *testing.T, h *harness, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := h.worker.Tick(h.ctx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestALockModeFalseRowOlderThanTheWindowStillRefusesTheLane(t *testing.T) {
	fb := newFallbackActor(t)
	var lanes livenessLane
	h := newHarness(t, completesSynchronously, withLivenessLanes(t, withFallbackMetadata, fb.server.URL, &lanes))
	// What the collector re-persists, verbatim, on every probe of a
	// LOCK-mode bridge that latched an hour ago: the same checked_at.
	observeNotLiveInMode(t, h, "company/analyzer", time.Now().UTC().Add(-time.Hour), storepg.LivenessModeLock)

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if got := len(h.invocations()); got != 0 {
		t.Fatalf("primary invoked %d times on an hour-old LOCK-mode false fact, want 0: a latch does not expire by age", got)
	}
	if got := fb.hits.Load(); got != 1 {
		t.Fatalf("fallback invoked %d times, want 1", got)
	}
	if routings := livenessRoutings(t, h, run.ID); len(routings) != 1 || routings[0]["selected"] != "fallback" {
		t.Fatalf("routing records = %v, want one fallback routing", routings)
	}
}

func TestAnAsyncCredentialSpentCallbackLocksTheLane(t *testing.T) {
	var (
		mu       sync.Mutex
		callback actors.Callback
	)
	h := newHarness(t, asyncActor(&callback, &mu))
	run := h.createRun("async.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return callback.URL != ""
	})
	mu.Lock()
	cb := callback
	mu.Unlock()

	if got := postCallbackEvent(t, cb, credentialSpentEvent("ev-spent")); got != string(actors.DispositionCommitted) {
		t.Fatalf("terminal callback disposition = %q, want committed", got)
	}
	status, _ := attemptRecord(t, h, run.ID, "work")
	if engine.TechStatus(status) != engine.StatusFailed {
		t.Errorf("attempt status = %q, want failed", status)
	}

	row, ok, err := h.store.ActorLiveness(h.ctx, h.ns.ID, "company/long-runner")
	if err != nil || !ok {
		t.Fatalf("ActorLiveness = (%v, %v), want the locked row: the async path must lock exactly as the sync one does", ok, err)
	}
	if !row.Locked || row.SessionOK == nil || *row.SessionOK || row.Reason != worker.LivenessLockReason || row.Source != storepg.LivenessSourceWorker {
		t.Fatalf("row = %+v, want locked session_ok=false reason=refresh_token_spent source=worker", row)
	}
	if row.LockedByRunID != run.ID {
		t.Errorf("locked_by_run_id = %q, want %s", row.LockedByRunID, run.ID)
	}
	if !hasEvent(runEventTypes(t, h, run.ID), worker.TypeLaneLocked) {
		t.Errorf("run events carry no %s", worker.TypeLaneLocked)
	}
}

func TestAPausedFallbackLaneIsDeferredNotDispatched(t *testing.T) {
	fb := newFallbackActor(t)
	var lanes livenessLane
	h := newHarness(t, completesSynchronously, withLivenessLanes(t, withFallbackMetadata, fb.server.URL, &lanes))
	observeNotLive(t, h, "company/analyzer", time.Now().UTC())
	if _, err := h.store.PauseActor(h.ctx, storepg.PauseActorInput{
		NamespaceID: h.ns.ID, ActorKey: "company/fallback", PausedUntil: time.Now().UTC().Add(time.Hour),
		Reason: string(actors.ClassCapacityExhausted),
	}); err != nil {
		t.Fatalf("PauseActor: %v", err)
	}

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	tick(t, h, 5)

	if got := len(h.invocations()); got != 0 {
		t.Fatalf("primary invoked %d times, want 0: its lane is not live", got)
	}
	if got := fb.hits.Load(); got != 0 {
		t.Fatalf("fallback invoked %d times while paused, want 0: the breaker must see the lane actually dispatched", got)
	}
	if state := h.run(run.ID).State; state.Terminal() {
		t.Fatalf("run state = %s, want still live: a pause defers, it does not fail", state)
	}
	if state, _, _ := workItemAvailability(t, h, run.ID, "analyze"); state != "ready" {
		t.Errorf("work item state = %q, want ready (deferred)", state)
	}
	types := runEventTypes(t, h, run.ID)
	if !hasEvent(types, worker.TypeDispatchDeferred) {
		t.Fatalf("run events = %v, want %s", types, worker.TypeDispatchDeferred)
	}
	data := runEventData(t, h, run.ID, worker.TypeDispatchDeferred)
	if data["actor_key"] != "company/fallback" || data["actor_ref"] != "company/fallback" {
		t.Errorf("deferral names %v/%v, want the fallback lane company/fallback", data["actor_key"], data["actor_ref"])
	}
	if from, _ := data["routed_from"].(string); !strings.HasPrefix(from, "actor://company/analyzer") {
		t.Errorf("deferral routed_from = %v, want the node's own reference", data["routed_from"])
	}
}

func TestAFallbackDispatchRecordsTheFallbackOnTheInvocationAndLocksIt(t *testing.T) {
	fb := newAsyncFallbackActor(t)
	var lanes livenessLane
	h := newHarness(t, completesSynchronously, withLivenessLanes(t, withFallbackMetadata, fb.server.URL, &lanes))
	observeNotLive(t, h, "company/analyzer", time.Now().UTC())

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool {
		cb, _ := fb.captured()
		return cb.URL != ""
	})
	cb, hits := fb.captured()
	if hits != 1 || len(h.invocations()) != 0 {
		t.Fatalf("fallback/primary invoked %d/%d times, want 1/0", hits, len(h.invocations()))
	}

	var actorRef string
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT COALESCE(actor_ref, '') FROM actor_invocations WHERE run_id = $1`, run.ID).Scan(&actorRef); err != nil {
		t.Fatalf("read actor_invocations: %v", err)
	}
	if actorRef != "company/fallback" {
		t.Fatalf("actor_invocations.actor_ref = %q, want the fallback that was actually invoked", actorRef)
	}

	if got := postCallbackEvent(t, cb, credentialSpentEvent("ev-fb-spent")); got != string(actors.DispositionCommitted) {
		t.Fatalf("terminal callback disposition = %q, want committed", got)
	}
	row, ok, err := h.store.ActorLiveness(h.ctx, h.ns.ID, "company/fallback")
	if err != nil || !ok || !row.Locked {
		t.Fatalf("fallback liveness = (%+v, %v, %v), want locked: the lock follows the lane invoked", row, ok, err)
	}
	if primary, _, _ := h.store.ActorLiveness(h.ctx, h.ns.ID, "company/analyzer"); primary.Locked {
		t.Error("the primary was locked for a failure on the fallback lane")
	}
}
