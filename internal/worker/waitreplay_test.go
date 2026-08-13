package worker_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/engine"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The replay acceptance, end to end (issue #43 task t21, design D12): an
// event delivered BEFORE anything subscribed to it still resumes the late
// subscriber. Migration 0016 documented this as a limitation —
// event-then-subscription stayed parked forever — and this test is the fact
// that it no longer is.
//
// The ordering the test needs is real, not contrived: the harness's worker
// only ticks inside runUntil, so the run exists (which is what puts the fact
// inside the run's own lifetime — the D12 floor) while nothing has yet
// dispatched the wait node. That is exactly the production race the design
// is about: a run is created, the world speaks, and only then does the run
// reach the node that was listening.
func TestSignalDeliveredBeforeTheWaitStillResumesIt(t *testing.T) {
	const eventSecret = "test-only-event-secret-not-for-production"

	h := newHarness(t, refuseActor)
	apiSrv, err := api.NewServer(h.store, h.ns.ID, api.WithEventTokenSecret(eventSecret))
	if err != nil {
		t.Fatalf("api.NewServer: %v", err)
	}
	ts := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(ts.Close)

	run := h.createRun("waitsignal.workflow.yaml", `{}`)

	// The event fires while the wait node has not been dispatched yet: no
	// subscription exists, so live delivery resumes nothing.
	if code := postSignalEvent(t, ts.URL, eventSecret,
		`{"name":"green-light","payload":{"go":true},"emitter":"ops"}`); code != http.StatusCreated {
		t.Fatalf("delivery = %d, want 201", code)
	}
	var armed int
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT count(*) FROM signal_subscriptions WHERE run_id = $1`, run.ID).Scan(&armed); err != nil {
		t.Fatalf("count subscriptions: %v", err)
	}
	if armed != 0 {
		t.Fatalf("%d subscriptions existed before the wait was dispatched", armed)
	}

	// Now the worker reaches the wait node. It must NOT park on an event
	// that has been and gone; it must catch up and complete.
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })
	if state := h.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", state, h.workerErrors())
	}

	// The subscription is recorded as fired by the backlogged fact — the
	// catch-up leaves the same durable trail a live resume does, so an
	// operator can still answer "what woke this node".
	if status, _ := signalSubscriptionRow(t, h, run.ID); status != "fired" {
		t.Errorf("subscription after catch-up = %q, want fired", status)
	}

	// The audit trail distinguishes catch-up from live delivery.
	var replayed, resumed int
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT count(*) FILTER (WHERE event_type = $2), count(*) FILTER (WHERE event_type = $3)
		 FROM events WHERE aggregate_id = $1`,
		run.ID, storepg.TypeSignalReplayed, storepg.TypeSignalResumed,
	).Scan(&replayed, &resumed); err != nil {
		t.Fatalf("count audit events: %v", err)
	}
	if replayed != 1 {
		t.Errorf("signal.replayed events = %d, want 1", replayed)
	}
	if resumed != 0 {
		t.Errorf("signal.resumed events = %d, want 0 — nothing was parked to resume", resumed)
	}

	// The wait node's output carries the resuming event exactly as a live
	// resume would: a downstream binding reads what woke the run, not how the
	// control plane happened to notice.
	var result []byte
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT a.result FROM attempts AS a JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = 'pause' AND a.status = 'succeeded'
	`, run.ID).Scan(&result); err != nil {
		t.Fatalf("read attempt result: %v", err)
	}
	var out struct {
		Event struct {
			Name    string `json:"name"`
			Emitter string `json:"emitter"`
			Payload struct {
				Go bool `json:"go"`
			} `json:"payload"`
		} `json:"event"`
	}
	if err := json.Unmarshal(result, &out); err != nil {
		t.Fatalf("decode attempt result %s: %v", result, err)
	}
	if out.Event.Name != "green-light" || out.Event.Emitter != "ops" || !out.Event.Payload.Go {
		t.Errorf("attempt result = %s, want the replayed event folded into the output", result)
	}

	// And the wait never actually parked: no work item was ever left waiting
	// for an event that had already happened.
	if states := h.workItemStates(run.ID); states["pause"] == "waiting" {
		t.Errorf("the wait parked despite a backlogged fact; work item states = %v", states)
	}
}
