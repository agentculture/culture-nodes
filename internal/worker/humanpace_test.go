package worker_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// Human-timescale lifecycle (issue #38, spec claim c28, honesty condition
// h23, task t11): a human-actor node authored with a week-scale
// policy.timeout must park for days without timing out, retrying, or
// exhausting the dispatch budget — and a late callback must complete it
// through the standard path.
//
// The pacing is config-driven end to end: the node under test is an ordinary
// agent-kind node (testdata/humanpace.workflow.yaml), and nothing in the
// engine or worker reads actor kind to grant it time — that absence is
// grep-enforced by internal/invariants' TestActorKindReadsStayOutOfDispatch,
// which stays green alongside this test.
//
// Time is simulated, never slept: the injected clock (newClockedHarness)
// drives both the worker's deadline arithmetic and the engine's run-duration
// bound, and the scheduler's own due predicate (claimDueTimersSQL: status =
// 'pending' AND fire_at <= now) is evaluated directly against the advanced
// clock to prove the deadline timer is not claimable early.

// testClock is a mutable wall clock shared between the engine and the worker.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	// Truncate to Postgres's microsecond precision so a timestamp written
	// through the store reads back exactly equal.
	return &testClock{now: time.Now().UTC().Truncate(time.Microsecond)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Set(to time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = to
}

// TestWeekTimeoutFlowsUnclampedIntoAsyncDeadline is t11's first proof: an
// authored policy.timeout of 168h reaches the durable §12.7 deadline timer
// verbatim — no clamp, no default, and no heartbeat-derived shortening (the
// actor here declares a 1h heartbeat, whose ×3 tolerance would otherwise
// bound the wait at 3h; the node's own deadline wins per asyncDeadline).
func TestWeekTimeoutFlowsUnclampedIntoAsyncDeadline(t *testing.T) {
	clock := newTestClock()
	start := clock.Now()

	var (
		mu       sync.Mutex
		captured actors.InvocationRequest
		accepted = make(chan struct{})
	)
	h := newClockedHarness(t, clock.Now, func(_ *harness, w http.ResponseWriter, req actors.InvocationRequest) {
		mu.Lock()
		captured = req
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"invocation_id":"external_human_review","heartbeat_after_seconds":3600,"supports_cancellation":false}`))
		close(accepted)
	})

	run := h.createRun("humanpace.workflow.yaml", `{"subject":"release 1.2 sign-off"}`)
	if _, err := h.worker.Tick(h.ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	select {
	case <-accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("the actor was never invoked")
	}

	want := start.Add(168 * time.Hour)

	// The §13.1 invocation carried the authored week, not a machine-scale
	// default.
	mu.Lock()
	invDeadline := captured.Deadline
	mu.Unlock()
	if invDeadline == nil {
		t.Fatal("invocation carried no deadline; the node declares policy.timeout")
	}
	if !invDeadline.Equal(want) {
		t.Errorf("invocation deadline = %s, want the authored %s (start %s + 168h)", invDeadline, want, start)
	}

	// And the durable deadline timer fires a week out — the row the
	// scheduler would claim, holding exactly the authored bound.
	var fireAt time.Time
	var status string
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT fire_at, status FROM timers
		WHERE run_id = $1 AND timer_kind = $2
	`, run.ID, string(storepg.TimerKindDeadline)).Scan(&fireAt, &status); err != nil {
		t.Fatalf("read deadline timer: %v", err)
	}
	if status != "pending" {
		t.Errorf("deadline timer status = %q, want pending", status)
	}
	if !fireAt.Equal(want) {
		t.Errorf("deadline timer fire_at = %s, want the unclamped %s", fireAt, want)
	}
}

// TestHumanPacedParkSurvivesDaysAndCompletesFromLateCallback is t11's core
// acceptance: park an agent-kind attempt with a multi-day deadline, advance
// the simulated clock across five days, and prove at each step that nothing
// wakes, retries, or bills — then deliver the human's late callback and watch
// the run complete normally.
//
// The budget half (spec claim c20 territory, re-proven here across a resume):
// work_items.attempt counts DISPATCHES — it is incremented only by
// claimWorkSQL — so a park-and-resume consumes exactly the one dispatch that
// started it, however many simulated days pass in between.
func TestHumanPacedParkSurvivesDaysAndCompletesFromLateCallback(t *testing.T) {
	clock := newTestClock()
	start := clock.Now()

	var (
		mu       sync.Mutex
		captured actors.InvocationRequest
		accepted = make(chan struct{})
	)
	h := newClockedHarness(t, clock.Now, func(_ *harness, w http.ResponseWriter, req actors.InvocationRequest) {
		mu.Lock()
		captured = req
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"invocation_id":"external_human_review","heartbeat_after_seconds":0,"supports_cancellation":false}`))
		close(accepted)
	})

	run := h.createRun("humanpace.workflow.yaml", `{"subject":"release 1.2 sign-off"}`)
	if _, err := h.worker.Tick(h.ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	select {
	case <-accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("the actor was never invoked")
	}

	var nodeRunID string
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT id FROM node_runs WHERE run_id = $1 AND node_key = 'review'`, run.ID,
	).Scan(&nodeRunID); err != nil {
		t.Fatalf("find review node run: %v", err)
	}

	readState := func() (workState string, dispatches int32, attempts int, nodeRunStatus string) {
		t.Helper()
		if err := h.store.Pool().QueryRow(h.ctx, `
			SELECT wi.state, wi.attempt, nr.status,
			       (SELECT count(*) FROM attempts WHERE node_run_id = nr.id)
			FROM work_items AS wi JOIN node_runs AS nr ON nr.id = wi.node_run_id
			WHERE nr.id = $1
		`, nodeRunID).Scan(&workState, &dispatches, &nodeRunStatus, &attempts); err != nil {
			t.Fatalf("read park state: %v", err)
		}
		return workState, dispatches, attempts, nodeRunStatus
	}

	// Attempt rows are written at completion (CompleteAttempt), so a parked
	// node has zero — and must KEEP zero across the days: any row appearing
	// before the callback would be a timeout or a retry being recorded.
	if state, dispatches, attempts, status := readState(); state != storepg.WaitingWorkState ||
		dispatches != 1 || attempts != 0 || status != "waiting_external" {
		t.Fatalf("after park: work=%q dispatches=%d attempts=%d nodeRun=%q, want waiting/1/0/waiting_external",
			state, dispatches, attempts, status)
	}

	// dueDeadlineTimers is the scheduler's own claimability predicate
	// (claimDueTimersSQL: status = 'pending' AND fire_at <= now), asked at a
	// simulated instant. Zero means the scheduler, ticking at that instant,
	// could not have fired this run's deadline.
	dueDeadlineTimers := func(at time.Time) int {
		t.Helper()
		var n int
		if err := h.store.Pool().QueryRow(h.ctx, `
			SELECT count(*) FROM timers
			WHERE run_id = $1 AND timer_kind = $2 AND status = 'pending' AND fire_at <= $3
		`, run.ID, string(storepg.TimerKindDeadline), at).Scan(&n); err != nil {
			t.Fatalf("evaluate due predicate: %v", err)
		}
		return n
	}

	// Days pass. At each step: the deadline timer is not yet claimable, the
	// lease sweep has nothing to reclaim (a parked item holds no lease), the
	// worker finds nothing to dispatch, and the budget counter has not moved.
	for _, day := range []time.Duration{2 * 24 * time.Hour, 5 * 24 * time.Hour} {
		clock.Set(start.Add(day))

		if due := dueDeadlineTimers(clock.Now()); due != 0 {
			t.Fatalf("day %v: %d deadline timer(s) due before the authored 168h — an early timeout", day, due)
		}
		if _, err := h.store.ReclaimExpired(h.ctx); err != nil {
			t.Fatalf("day %v: ReclaimExpired: %v", day, err)
		}
		dispatched, err := h.worker.Tick(h.ctx)
		if err != nil {
			t.Fatalf("day %v: Tick: %v", day, err)
		}
		if dispatched != 0 {
			t.Fatalf("day %v: the worker dispatched %d item(s) while the node was parked — a retry", day, dispatched)
		}
		if state, dispatches, attempts, status := readState(); state != storepg.WaitingWorkState ||
			dispatches != 1 || attempts != 0 || status != "waiting_external" {
			t.Fatalf("day %v: work=%q dispatches=%d attempts=%d nodeRun=%q, want the untouched park (waiting/1/0/waiting_external)",
				day, state, dispatches, attempts, status)
		}
	}

	// Day five: the human answers. The callback rides the standard §13.1
	// ingest; the engine's clock now reads start+5d, so this also proves the
	// run-duration bound honors the authored 336h (the 1h default would have
	// ended the run right here, at the resume transition).
	mu.Lock()
	callback := captured.Callback
	mu.Unlock()
	completedPayload, _ := json.Marshal(actors.CompletedPayload{
		Outcome: "completed",
		Output:  json.RawMessage(`{"verdict":"approved after five days of consideration"}`),
	})
	body, _ := json.Marshal(actors.CallbackEvent{
		EventID: "ev-human-1", Sequence: 1, Kind: actors.EventCompleted, Payload: completedPayload,
	})
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
	var decoded struct {
		Disposition string `json:"disposition"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	if resp.StatusCode != http.StatusAccepted || decoded.Disposition != string(actors.DispositionCommitted) {
		t.Fatalf("late callback answered %d/%q, want 202/%q: a five-day-late submission within the authored deadline must commit",
			resp.StatusCode, decoded.Disposition, actors.DispositionCommitted)
	}

	final := h.run(run.ID)
	if final.State != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", final.State, h.workerErrors())
	}
	if !bytes.Contains(final.Output, []byte("approved after five days")) {
		t.Errorf("run output = %s, want the human's verdict", final.Output)
	}

	// The whole five-day park spent exactly one dispatch of the budget of
	// worker.MaxDispatchAttempts, and the node's single attempt completed —
	// no retry attempt, no budget-exhaustion failure, ever.
	_, dispatches, attempts, _ := readState()
	if dispatches != 1 || attempts != 1 {
		t.Errorf("after completion: dispatches=%d attempts=%d, want 1/1 — waiting must consume no budget", dispatches, attempts)
	}
	if int(dispatches) >= worker.MaxDispatchAttempts {
		t.Errorf("dispatches=%d has reached the budget of %d", dispatches, worker.MaxDispatchAttempts)
	}
	var latestStatus string
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT status FROM attempts WHERE node_run_id = $1 ORDER BY attempt_number DESC LIMIT 1`, nodeRunID,
	).Scan(&latestStatus); err != nil {
		t.Fatalf("read attempt status: %v", err)
	}
	if latestStatus != "succeeded" {
		t.Errorf("latest attempt status = %q, want succeeded", latestStatus)
	}
}
