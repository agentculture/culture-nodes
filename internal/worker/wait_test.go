package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/scheduler"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// Durable wait tests (issue #39, task t9): a `wait` node's until.duration
// parks the run on a §12.7 wait timer with NO lease held, the scheduler's
// timer fire is what wakes it, and the resumed completion goes through the
// engine's ordinary §12.5 transaction — which is what keeps the §9.7 loop
// bounds enforced across the park. until.signal is refused loudly until the
// event surface lands (build plan t10).

// refuseActor is a harness actor handler for workflows that must never
// invoke an actor at all: every wait workflow here is agent-free, so a
// request reaching it is itself a failure.
func refuseActor(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
	w.WriteHeader(http.StatusTeapot)
}

// startScheduler runs a real scheduler loop against the harness store for
// the duration of the test — the same single-active/standby loop a deployed
// scheduler process runs, so a timer fire in these tests goes through
// ClaimDueTimers, the wait effect, MarkFiredTx, and the outbox insert
// exactly as production would.
func startScheduler(t *testing.T, h *harness) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sch := scheduler.New(h.store, scheduler.Options{TickInterval: 25 * time.Millisecond})
	go func() { _ = sch.Run(ctx) }()
}

// pauseNodeRunID finds the run's node run for the `pause` node, newest
// first (the loop test has two).
func pauseNodeRunID(t *testing.T, h *harness, runID string) string {
	t.Helper()
	var id string
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT id FROM node_runs WHERE run_id = $1 AND node_key = 'pause' ORDER BY created_at DESC, id DESC LIMIT 1`,
		runID,
	).Scan(&id); err != nil {
		t.Fatalf("find pause node run: %v", err)
	}
	return id
}

func waitTimerStatuses(t *testing.T, h *harness, runID string) []string {
	t.Helper()
	rows, err := h.store.Pool().Query(h.ctx,
		`SELECT status FROM timers WHERE run_id = $1 AND timer_kind = 'wait' ORDER BY created_at, id`, runID)
	if err != nil {
		t.Fatalf("read wait timers: %v", err)
	}
	defer rows.Close()
	var statuses []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan wait timer: %v", err)
		}
		statuses = append(statuses, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read wait timers: %v", err)
	}
	return statuses
}

// Acceptance criterion 1: a wait node with until.duration parks its run
// with no lease held, and the scheduler resumes it after the timer fires,
// the run completing through its normal edges.
func TestWaitNodeParksDurablyAndSchedulerResumesIt(t *testing.T) {
	h := newHarness(t, refuseActor)
	run := h.createRun("wait.workflow.yaml", `{}`)

	// The dispatch parks: work item leaves 'leased' for 'waiting'.
	h.runUntil(20*time.Second, func() bool { return h.workItemStates(run.ID)["pause"] == "waiting" })

	// No lease is held on the parked item — the §12.6 discipline, applied
	// to a timer wait: nothing reclaims it, nothing re-claims it, and no
	// goroutine anywhere is waiting on it.
	var noLease bool
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT wi.lease_owner IS NULL AND wi.lease_expires_at IS NULL
		FROM work_items AS wi JOIN node_runs AS nr ON nr.id = wi.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = 'pause'
	`, run.ID).Scan(&noLease); err != nil {
		t.Fatalf("read parked work item: %v", err)
	}
	if !noLease {
		t.Fatal("parked wait still holds a lease; a durable wait must release worker capacity")
	}

	// The park is inspectable, not wedged: node run waiting_external, run
	// still live, exactly one pending wait timer bound to the node run.
	var nodeRunStatus string
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT status FROM node_runs WHERE run_id = $1 AND node_key = 'pause'`, run.ID,
	).Scan(&nodeRunStatus); err != nil {
		t.Fatalf("read node run: %v", err)
	}
	if nodeRunStatus != "waiting_external" {
		t.Errorf("parked node run status = %q, want waiting_external", nodeRunStatus)
	}
	if state := h.run(run.ID).State; state.Terminal() {
		t.Fatalf("run reached terminal state %s while parked", state)
	}
	if statuses := waitTimerStatuses(t, h, run.ID); len(statuses) != 1 || statuses[0] != "pending" {
		t.Fatalf("wait timers while parked = %v, want exactly one pending", statuses)
	}

	// Only now does a scheduler exist: the fire is what wakes the run.
	startScheduler(t, h)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", state, h.workerErrors())
	}
	var outcome string
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT outcome FROM node_runs WHERE run_id = $1 AND node_key = 'pause'`, run.ID,
	).Scan(&outcome); err != nil {
		t.Fatalf("read node run outcome: %v", err)
	}
	if outcome != "completed" {
		t.Errorf("wait node outcome = %q, want completed", outcome)
	}
	if statuses := waitTimerStatuses(t, h, run.ID); len(statuses) != 1 || statuses[0] != "fired" {
		t.Errorf("wait timers after resume = %v, want exactly one fired", statuses)
	}

	// The attempt's recorded output is the wait's honest answer: what was
	// waited for, and when it resolved.
	var result []byte
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT a.result FROM attempts AS a JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = 'pause' AND a.status = 'succeeded'
	`, run.ID).Scan(&result); err != nil {
		t.Fatalf("read attempt result: %v", err)
	}
	if !bytes.Contains(result, []byte("completed_at")) || !bytes.Contains(result, []byte("700ms")) {
		t.Errorf("attempt result = %s, want the until block and a completed_at stamp", result)
	}

	if got := h.invocations(); len(got) != 0 {
		t.Errorf("an agent-free wait workflow invoked an actor %d times", len(got))
	}
}

// Acceptance criterion 2: the §9.7 loop bounds still apply across the
// park-and-resume. Every visit to the wait node parks on its own timer and
// resumes through the scheduler; the transition into visit 3 crosses
// maxVisitsPerNode = 2 and the ENGINE — not any executor — stops the run.
func TestWaitInsideCycleStillEnforcesLoopBounds(t *testing.T) {
	h := newHarness(t, refuseActor)
	startScheduler(t, h)
	run := h.createRun("waitloop.workflow.yaml", `{}`)

	h.runUntil(30*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunFailed {
		t.Fatalf("run state = %s, want failed at the loop bound (worker errors: %v)", state, h.workerErrors())
	}

	var bounded int
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT count(*) FROM events WHERE aggregate_id = $1 AND event_type = $2`,
		run.ID, engine.TypeRunBounded,
	).Scan(&bounded); err != nil {
		t.Fatalf("count bounded events: %v", err)
	}
	if bounded != 1 {
		t.Errorf("run.bounded events = %d, want exactly 1", bounded)
	}

	// Both permitted visits genuinely went through a durable park: two wait
	// timers, both fired, and two pause node runs completed.
	statuses := waitTimerStatuses(t, h, run.ID)
	if len(statuses) != 2 {
		t.Fatalf("wait timers = %v, want one per visit (2)", statuses)
	}
	for i, s := range statuses {
		if s != "fired" {
			t.Errorf("wait timer %d status = %q, want fired", i, s)
		}
	}
	var visits int
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT count(*) FROM node_runs WHERE run_id = $1 AND node_key = 'pause' AND status = 'completed'`, run.ID,
	).Scan(&visits); err != nil {
		t.Fatalf("count pause visits: %v", err)
	}
	if visits != 2 {
		t.Errorf("completed pause visits = %d, want 2 (the bound stops the third)", visits)
	}
}

// Acceptance criterion 4: until.signal is refused with a documented error
// naming the event surface as the follow-up — a diagnosed technical
// failure, never a silent stub and never an invented completion.
func TestWaitUntilSignalIsRefusedNotStubbed(t *testing.T) {
	h := newHarness(t, refuseActor)
	run := h.createRun("waitsignal.workflow.yaml", `{}`)

	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunFailed {
		t.Fatalf("run state = %s, want failed", state)
	}
	var result []byte
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT a.result FROM attempts AS a JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = 'pause'
	`, run.ID).Scan(&result); err != nil {
		t.Fatalf("read attempt result: %v", err)
	}
	if !bytes.Contains(result, []byte("until.signal")) || !bytes.Contains(result, []byte("event")) {
		t.Errorf("attempt result = %s, want the refusal to name until.signal and the event surface", result)
	}

	// No timer was armed and nothing is parked: the refusal completed the
	// work item through the normal failure path.
	if statuses := waitTimerStatuses(t, h, run.ID); len(statuses) != 0 {
		t.Errorf("wait timers = %v, want none for a refused signal wait", statuses)
	}
	if got := h.workItemStates(run.ID)["pause"]; got != "completed" {
		t.Errorf("work item state = %q, want completed", got)
	}
}

// The until block's validation is pure and needs no store: signal refusals
// and shape errors are diagnosed before any timer lookup.
func TestDispatchWaitValidatesUntilShapes(t *testing.T) {
	d := worker.NewTimerWaitDispatcher(nil, nil)
	ctx := context.Background()
	dc := worker.DispatchContext{NodeRunID: "nr-unit"}

	cases := map[string]struct {
		until string
		want  []string
	}{
		"signal names the follow-up":  {`{"signal":"green-light"}`, []string{"until.signal", "event"}},
		"empty object":                {`{}`, []string{"exactly one"}},
		"duration and timestamp both": {`{"duration":"5m","timestamp":"2030-01-01T00:00:00Z"}`, []string{"exactly one"}},
		"malformed duration":          {`{"duration":"soon"}`, []string{"not a duration"}},
		"malformed timestamp":         {`{"timestamp":"tomorrow"}`, []string{"RFC 3339"}},
		"missing block":               {``, []string{"no until block"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var until json.RawMessage
			if tc.until != "" {
				until = json.RawMessage(tc.until)
			}
			_, err := d.DispatchWait(ctx, dc, until)
			if err == nil {
				t.Fatalf("DispatchWait(%s) succeeded, want a refusal", tc.until)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// A wait whose fire time is already past never parks: an until.timestamp in
// the past completes on first dispatch with the kind-implied outcome. The
// dispatcher needs a store (the persisted timer is consulted before the
// elapsed check), so this runs against the shared test instance.
func TestDispatchWaitElapsedTimestampCompletesImmediately(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	d := worker.NewTimerWaitDispatcher(s, nil)

	res, err := d.DispatchWait(context.Background(),
		worker.DispatchContext{NodeRunID: "nr-elapsed-" + t.Name()},
		json.RawMessage(`{"timestamp":"2020-01-01T00:00:00Z"}`))
	if err != nil {
		t.Fatalf("DispatchWait: %v", err)
	}
	if res.Async {
		t.Fatal("an already-elapsed timestamp parked instead of completing")
	}
	if res.TechStatus != engine.StatusSucceeded || res.Outcome != "completed" {
		t.Errorf("result = (%s, %q), want (succeeded, completed)", res.TechStatus, res.Outcome)
	}
	if !bytes.Contains(res.Output, []byte("completed_at")) {
		t.Errorf("output = %s, want a completed_at stamp", res.Output)
	}
}
