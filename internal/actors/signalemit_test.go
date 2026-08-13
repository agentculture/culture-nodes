package actors_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Mid-execution emission (issue #43 task t21, design D11): an attempt POSTs a
// non-terminal `signal` callback event, the control plane appends and delivers
// the fact, and the attempt KEEPS WORKING. These tests are the design's T20
// and T21 rows.

func signalEvent(id string, sequence int64, payload actors.SignalPayload) actors.CallbackEvent {
	body, _ := json.Marshal(payload)
	return actors.CallbackEvent{EventID: id, Sequence: sequence, Kind: actors.EventSignal, Payload: body}
}

// parkedWaiter creates a second run in the fixture's namespace and parks one
// of its node runs on a named signal — the "human node waiting for the agent
// to ask" half of the design's motivating example.
func (f *asyncFixture) parkedWaiter(eventName string) (runID, nodeRunID, subID, workID string) {
	f.t.Helper()
	run, err := f.engine.CreateRun(f.ctx, f.cw, json.RawMessage(`{"subject":"waiter"}`))
	if err != nil {
		f.t.Fatalf("CreateRun (waiter): %v", err)
	}
	nodeRunID = f.readyNodeRun(run.ID)
	claimed := f.claim("waiter-"+f.t.Name(), nodeRunID)
	subID = "signal-" + nodeRunID
	if err := f.store.StartDurableSignalWait(f.ctx, storepg.StartDurableSignalWaitInput{
		WorkID:         claimed.ID,
		WorkerID:       "waiter-" + f.t.Name(),
		FencingToken:   claimed.FencingToken,
		Attempt:        int(claimed.Attempt),
		NamespaceID:    f.ns.ID,
		RunID:          run.ID,
		NodeRunID:      nodeRunID,
		NodeID:         "work",
		AttemptID:      "att_waiter_" + claimed.ID,
		SubscriptionID: subID,
		EventName:      eventName,
	}); err != nil {
		f.t.Fatalf("StartDurableSignalWait: %v", err)
	}
	return run.ID, nodeRunID, subID, claimed.ID
}

// TestSignalEmissionWakesAnotherRunAndLeavesTheEmitterWorking is the
// acceptance h42's emission half asks for, and design test T20: the emitting
// attempt is still in flight after the emission, the parked waiter is awake,
// and the emitter's own terminal event later commits exactly as it would
// have.
func TestSignalEmissionWakesAnotherRunAndLeavesTheEmitterWorking(t *testing.T) {
	f := newAsyncFixture(t)
	waiterRun, waiterNodeRun, subID, waiterWorkID := f.parkedWaiter("review-requested")

	result := f.handle(signalEvent("ev-emit-1", 1, actors.SignalPayload{
		Name:    "review-requested",
		Payload: json.RawMessage(`{"severity":"high"}`),
		Scope:   actors.SignalScopeNamespace,
	}))
	if result.Disposition != actors.DispositionRecorded {
		t.Fatalf("emission disposition = %q, want recorded", result.Disposition)
	}
	if result.Completion != nil {
		t.Error("an emission committed a completion; the `signal` kind is non-terminal")
	}

	// The emitting attempt is untouched: still parked, still leaseless, its
	// node run still waiting_external. This is what "non-blocking" has to
	// mean operationally.
	if state, owner := f.workItemState(f.claimed.ID); state != storepg.WaitingWorkState || owner != nil {
		t.Errorf("emitter work item = (%q, %v), want (waiting, NULL) — emission must not touch the emitter", state, owner)
	}
	var emitterStatus string
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT status FROM node_runs WHERE id = $1`, f.nodeRunID).Scan(&emitterStatus); err != nil {
		t.Fatalf("read emitter node run: %v", err)
	}
	if emitterStatus != string(engine.NodeRunWaitingExternal) {
		t.Errorf("emitter node run = %q, want waiting_external", emitterStatus)
	}

	// The waiter woke: subscription fired, parked work item claimable again.
	sub, found, err := f.store.SignalSubscriptionByID(f.ctx, subID)
	if err != nil || !found {
		t.Fatalf("SignalSubscriptionByID = (found=%v, err=%v)", found, err)
	}
	if sub.Status != storepg.SignalSubscriptionFired {
		t.Fatalf("waiting subscription status = %q, want fired", sub.Status)
	}
	var waiterState string
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT state FROM work_items WHERE id = $1`, waiterWorkID).Scan(&waiterState); err != nil {
		t.Fatalf("read waiter work item: %v", err)
	}
	if waiterState != "ready" {
		t.Errorf("waiter work item = %q, want ready", waiterState)
	}
	_ = waiterRun
	_ = waiterNodeRun

	// The emission is attributed to the verified attempt, never to anything
	// the caller sent.
	var emitter string
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT emitter FROM signal_events WHERE id = $1`, sub.FiredEventID).Scan(&emitter); err != nil {
		t.Fatalf("read emitted fact: %v", err)
	}
	if !strings.Contains(emitter, "node:work") {
		t.Errorf("emitter = %q, want it to name the emitting node", emitter)
	}

	if !f.hasEvent(actors.TypeSignalEmitted) {
		t.Errorf("no signal-emitted audit event; events were %v", f.eventTypes())
	}

	// And the emitter finishes normally afterwards: emission changed nothing
	// about the attempt it came from.
	done := f.handle(completedEvent("ev-emit-terminal", 2, "kept working"))
	if done.Disposition != actors.DispositionCommitted {
		t.Fatalf("emitter completion disposition = %q, want committed", done.Disposition)
	}
}

// TestSignalEmissionDefaultsToRunScope pins the default: a fact with no
// declared scope is a fact about the emitting run, so another run's waiter
// does not hear it.
func TestSignalEmissionDefaultsToRunScope(t *testing.T) {
	f := newAsyncFixture(t)
	_, _, subID, _ := f.parkedWaiter("run-scoped")

	f.handle(signalEvent("ev-scope-1", 1, actors.SignalPayload{Name: "run-scoped"}))

	sub, found, err := f.store.SignalSubscriptionByID(f.ctx, subID)
	if err != nil || !found {
		t.Fatalf("SignalSubscriptionByID = (found=%v, err=%v)", found, err)
	}
	if sub.Status != storepg.SignalSubscriptionPending {
		t.Errorf("another run's waiter = %q, want still pending: an unscoped emission is a fact about its own run", sub.Status)
	}

	var scoped *string
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT run_id FROM signal_events WHERE name = 'run-scoped' AND namespace_id = $1`, f.ns.ID).Scan(&scoped); err != nil {
		t.Fatalf("read emitted fact: %v", err)
	}
	if scoped == nil || *scoped != f.run.ID {
		t.Errorf("emitted fact run scope = %v, want the emitting run %s", scoped, f.run.ID)
	}
}

// TestSignalEmissionIsIdempotentPerEventID is design test T21's third clause.
// §13.4 delivery is at-least-once, so a redelivered emission must append one
// fact, not two — the ingest's event-id claim is what guarantees it, and the
// emission path inherits it for free by sitting behind that claim.
func TestSignalEmissionIsIdempotentPerEventID(t *testing.T) {
	f := newAsyncFixture(t)

	ev := signalEvent("ev-dup-1", 1, actors.SignalPayload{Name: "emitted-twice"})
	if got := f.handle(ev).Disposition; got != actors.DispositionRecorded {
		t.Fatalf("first emission disposition = %q, want recorded", got)
	}
	if got := f.handle(ev).Disposition; got != actors.DispositionDuplicate {
		t.Fatalf("redelivered emission disposition = %q, want duplicate", got)
	}

	var facts int
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT count(*) FROM signal_events WHERE namespace_id = $1 AND name = 'emitted-twice'`, f.ns.ID).Scan(&facts); err != nil {
		t.Fatalf("count facts: %v", err)
	}
	if facts != 1 {
		t.Errorf("redelivered emission appended %d facts, want exactly 1", facts)
	}
}

// TestSignalEmissionRefusesAnUnusableRequest: a `signal` event that names no
// event is rejected and changes nothing — and, crucially, does NOT fail the
// session that is still running.
func TestSignalEmissionRefusesAnUnusableRequest(t *testing.T) {
	f := newAsyncFixture(t)

	result := f.handle(signalEvent("ev-bad-1", 1, actors.SignalPayload{}))
	if result.Disposition != actors.DispositionRejected {
		t.Fatalf("nameless emission disposition = %q, want rejected", result.Disposition)
	}
	if state, _ := f.workItemState(f.claimed.ID); state != storepg.WaitingWorkState {
		t.Errorf("a rejected emission moved the emitter's work item to %q", state)
	}

	bad := actors.CallbackEvent{
		EventID: "ev-bad-2", Sequence: 2, Kind: actors.EventSignal,
		Payload: json.RawMessage(`{"name":"x","scope":"everywhere"}`),
	}
	if got := f.handle(bad).Disposition; got != actors.DispositionRejected {
		t.Fatalf("unknown-scope emission disposition = %q, want rejected", got)
	}

	// The session still finishes normally.
	if got := f.handle(completedEvent("ev-bad-terminal", 3, "unharmed")).Disposition; got != actors.DispositionCommitted {
		t.Fatalf("completion after rejected emissions = %q, want committed", got)
	}
}

// TestSignalEmissionAuth is design test T21's first two clauses: an expired
// attempt token cannot emit, and a token minted for attempt A emits as A
// however the request addresses itself. The attempt id comes out of the
// signature; the URL is only ever validated for shape (callback_http.go).
func TestSignalEmissionAuth(t *testing.T) {
	f := newAsyncFixture(t)
	handler := actors.NewCallbackHandler(f.deps)

	post := func(pathAttemptID, token string, ev actors.CallbackEvent) *httptest.ResponseRecorder {
		body, _ := json.Marshal(ev)
		req := httptest.NewRequest(http.MethodPost,
			fmt.Sprintf(actors.CallbackEventsPathFormat, pathAttemptID), strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// An expired token is refused before anything else happens.
	shortLived, err := actors.NewTokenSigner([]byte(testSecret), actors.WithTokenTTL(time.Millisecond))
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}
	expired, err := shortLived.Mint(f.attemptID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if rec := post(f.attemptID, expired, signalEvent("ev-auth-expired", 1,
		actors.SignalPayload{Name: "should-not-exist"})); rec.Code != http.StatusUnauthorized {
		t.Errorf("expired token status = %d, want 401", rec.Code)
	}
	var leaked int
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT count(*) FROM signal_events WHERE namespace_id = $1 AND name = 'should-not-exist'`, f.ns.ID).Scan(&leaked); err != nil {
		t.Fatalf("count facts: %v", err)
	}
	if leaked != 0 {
		t.Errorf("an expired token emitted %d facts", leaked)
	}

	// A token for attempt A addressed to attempt B emits as A: the path is
	// not an identity, the signature is.
	if rec := post("att_somebody_else", f.token, signalEvent("ev-auth-foreign", 1,
		actors.SignalPayload{Name: "attributed"})); rec.Code != http.StatusAccepted {
		t.Fatalf("valid token status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	var emitter string
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT emitter FROM signal_events WHERE namespace_id = $1 AND name = 'attributed'`, f.ns.ID).Scan(&emitter); err != nil {
		t.Fatalf("read emitted fact: %v", err)
	}
	if strings.Contains(emitter, "att_somebody_else") {
		t.Errorf("emitter = %q; the caller's claimed attempt id reached the fact", emitter)
	}
	if !strings.Contains(emitter, f.attemptID) && !strings.Contains(emitter, "actor:") {
		t.Errorf("emitter = %q, want it derived from the verified attempt", emitter)
	}
}
