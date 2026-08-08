package scheduler_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/scheduler"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// --- fixtures -----------------------------------------------------------

// mustNodeRun creates the full fixture chain a work_items or timers row
// referencing node_run_id requires: namespace -> workflow_version -> run ->
// node_run. This mirrors internal/store/postgres/claiming_test.go's helper
// of the same name -- duplicated rather than imported because it lives in
// that package's _test.go file, which cannot be imported by this package
// (see pgtest's own doc comment for the same reasoning).
func mustNodeRun(t *testing.T, s *postgres.Store, namespaceID string) string {
	t.Helper()
	ctx := context.Background()

	wv, err := s.CreateWorkflowVersion(ctx, postgres.CreateWorkflowVersionInput{
		NamespaceID:   namespaceID,
		WorkflowKey:   "test-scheduler-workflow-" + store.NewULID(),
		Version:       1,
		SourceFormat:  "yaml",
		Source:        "entrypoint: intake\n",
		ContentDigest: "sha256:" + store.NewULID(),
	})
	if err != nil {
		t.Fatalf("mustNodeRun: CreateWorkflowVersion: %v", err)
	}

	runID := store.NewULID()
	if _, err := s.Pool().Exec(ctx,
		`INSERT INTO runs (id, namespace_id, workflow_version_id) VALUES ($1, $2, $3)`,
		runID, namespaceID, wv.ID,
	); err != nil {
		t.Fatalf("mustNodeRun: insert run: %v", err)
	}

	nodeRunID := store.NewULID()
	if _, err := s.Pool().Exec(ctx,
		`INSERT INTO node_runs (id, namespace_id, run_id, node_key) VALUES ($1, $2, $3, 'intake')`,
		nodeRunID, namespaceID, runID,
	); err != nil {
		t.Fatalf("mustNodeRun: insert node_run: %v", err)
	}
	return nodeRunID
}

// insertWaitingWorkItem inserts a work_items row in a 'waiting' state (not
// 'ready' -- work_items.state carries no CHECK constraint, so this is a
// legitimate app-level state meaning "not yet claimable"), far in the
// future by available_at, so it is unambiguously NOT claimable until
// something -- a fired wait/retry timer -- flips it. EnqueueWork
// (claiming.go) cannot produce this: it always inserts state = 'ready'.
func insertWaitingWorkItem(t *testing.T, s *postgres.Store, namespaceID, nodeRunID string) string {
	t.Helper()
	id := store.NewULID()
	if _, err := s.Pool().Exec(context.Background(), `
		INSERT INTO work_items (id, namespace_id, node_run_id, state, available_at)
		VALUES ($1, $2, $3, 'waiting', now() + interval '1 hour')
	`, id, namespaceID, nodeRunID); err != nil {
		t.Fatalf("insertWaitingWorkItem: %v", err)
	}
	return id
}

type workItemRow struct {
	State       string
	AvailableAt time.Time
}

func mustWorkItem(t *testing.T, s *postgres.Store, id string) workItemRow {
	t.Helper()
	var row workItemRow
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT state, available_at FROM work_items WHERE id = $1`, id,
	).Scan(&row.State, &row.AvailableAt); err != nil {
		t.Fatalf("mustWorkItem: %v", err)
	}
	return row
}

func timerStatus(t *testing.T, s *postgres.Store, id string) string {
	t.Helper()
	var status string
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT status FROM timers WHERE id = $1`, id,
	).Scan(&status); err != nil {
		t.Fatalf("timerStatus: %v", err)
	}
	return status
}

// outboxCountForTimer counts outbox rows whose payload carries timerID as
// "timer_id" (see scheduler.go's timerOutboxPayload) -- the "no loss, no
// double-fire" check every test below that fires a timer relies on: it
// must read exactly 1, never 0 (lost) or more than 1 (double-fired).
func outboxCountForTimer(t *testing.T, s *postgres.Store, timerID string) int {
	t.Helper()
	var n int
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM outbox WHERE payload->>'timer_id' = $1`, timerID,
	).Scan(&n); err != nil {
		t.Fatalf("outboxCountForTimer: %v", err)
	}
	return n
}

func outboxTopicForTimer(t *testing.T, s *postgres.Store, timerID string) string {
	t.Helper()
	var topic string
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT topic FROM outbox WHERE payload->>'timer_id' = $1`, timerID,
	).Scan(&topic); err != nil {
		t.Fatalf("outboxTopicForTimer: %v", err)
	}
	return topic
}

// waitFor polls cond every 10ms until it reports true or timeout elapses,
// failing the test in the latter case. Every assertion in this file about
// a running Scheduler goroutine goes through this rather than a fixed
// sleep, since a scheduler's own tick interval is often shorter than any
// sleep budget a test could safely assume without becoming flaky under
// load.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}

// --- tests ----------------------------------------------------------------

func TestSchedulerFiresWaitOrRetryTimerMakesWorkItemReady(t *testing.T) {
	for _, kind := range []postgres.TimerKind{postgres.TimerKindWait, postgres.TimerKindRetry} {
		t.Run(string(kind), func(t *testing.T) {
			s := requireStore(t)
			ctx := context.Background()
			ns := pgtest.MustNamespace(t, s, "test-scheduler-"+string(kind))
			nodeRunID := mustNodeRun(t, s, ns.ID)
			workItemID := insertWaitingWorkItem(t, s, ns.ID, nodeRunID)

			timerID := "tmr_" + store.NewULID()
			if _, err := s.ScheduleTimer(ctx, postgres.Timer{
				ID: timerID, NamespaceID: ns.ID, NodeRunID: nodeRunID, Kind: kind,
				FireAt: time.Now().Add(-time.Second),
			}); err != nil {
				t.Fatalf("ScheduleTimer: %v", err)
			}

			sch := scheduler.New(s, scheduler.Options{TickInterval: 25 * time.Millisecond})
			runCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() { _ = sch.Run(runCtx) }()

			waitFor(t, 5*time.Second, func() bool {
				return timerStatus(t, s, timerID) == postgres.TimerStatusFired
			})

			item := mustWorkItem(t, s, workItemID)
			if item.State != "ready" {
				t.Fatalf("work item state = %q, want %q", item.State, "ready")
			}
			if item.AvailableAt.After(time.Now()) {
				t.Fatalf("work item available_at = %v, want <= now", item.AvailableAt)
			}
			if n := outboxCountForTimer(t, s, timerID); n != 1 {
				t.Fatalf("outbox rows for timer %q = %d, want exactly 1", timerID, n)
			}
		})
	}
}

func TestSchedulerDoesNotFireFutureTimer(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "test-scheduler-future")
	nodeRunID := mustNodeRun(t, s, ns.ID)
	workItemID := insertWaitingWorkItem(t, s, ns.ID, nodeRunID)

	timerID := "tmr_" + store.NewULID()
	if _, err := s.ScheduleTimer(ctx, postgres.Timer{
		ID: timerID, NamespaceID: ns.ID, NodeRunID: nodeRunID, Kind: postgres.TimerKindWait,
		FireAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("ScheduleTimer: %v", err)
	}

	sch := scheduler.New(s, scheduler.Options{TickInterval: 20 * time.Millisecond})
	runCtx, cancel := context.WithCancel(context.Background())
	go func() { _ = sch.Run(runCtx) }()

	// Give it several ticks' worth of time to (incorrectly) fire the
	// not-yet-due timer, then assert it did not.
	waitFor(t, 2*time.Second, func() bool {
		return sch.Health().Status == scheduler.StatusActive
	})
	time.Sleep(200 * time.Millisecond)
	cancel()

	if got := timerStatus(t, s, timerID); got != postgres.TimerStatusPending {
		t.Fatalf("status = %q, want %q (timer is not yet due)", got, postgres.TimerStatusPending)
	}
	item := mustWorkItem(t, s, workItemID)
	if item.State != "waiting" {
		t.Fatalf("work item state = %q, want unchanged %q", item.State, "waiting")
	}
}

func TestSchedulerFiresDeadlineTimerInsertsOutboxEvent(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "test-scheduler-deadline")

	timerID := "tmr_" + store.NewULID()
	if _, err := s.ScheduleTimer(ctx, postgres.Timer{
		ID: timerID, NamespaceID: ns.ID, Kind: postgres.TimerKindDeadline,
		FireAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatalf("ScheduleTimer: %v", err)
	}

	sch := scheduler.New(s, scheduler.Options{TickInterval: 25 * time.Millisecond})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sch.Run(runCtx) }()

	waitFor(t, 5*time.Second, func() bool {
		return timerStatus(t, s, timerID) == postgres.TimerStatusFired
	})

	if n := outboxCountForTimer(t, s, timerID); n != 1 {
		t.Fatalf("outbox rows for timer %q = %d, want exactly 1", timerID, n)
	}
	if topic := outboxTopicForTimer(t, s, timerID); topic != "dev.culture.nodes.timer.deadline-expired" {
		t.Fatalf("topic = %q, want %q", topic, "dev.culture.nodes.timer.deadline-expired")
	}
}

func TestSchedulerFiresLeaseRecoveryTimerReclaimsExpiredLease(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "test-scheduler-lease-recovery")
	nodeRunID := mustNodeRun(t, s, ns.ID)

	if err := s.EnqueueWork(ctx, postgres.WorkItem{NamespaceID: ns.ID, NodeRunID: nodeRunID}); err != nil {
		t.Fatalf("EnqueueWork: %v", err)
	}
	claimed, err := s.ClaimWork(ctx, "dead-worker", time.Minute, 1)
	if err != nil {
		t.Fatalf("ClaimWork: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("ClaimWork returned %d items, want 1", len(claimed))
	}
	workItemID := claimed[0].ID

	// Backdate the lease so ReclaimExpired treats it as expired right now,
	// mirroring internal/store/postgres/claiming_test.go's
	// backdateLeaseExpiry helper.
	if _, err := s.Pool().Exec(ctx,
		`UPDATE work_items SET lease_expires_at = now() - interval '1 second' WHERE id = $1`,
		workItemID,
	); err != nil {
		t.Fatalf("backdate lease: %v", err)
	}

	timerID := "tmr_" + store.NewULID()
	if _, err := s.ScheduleTimer(ctx, postgres.Timer{
		ID: timerID, NamespaceID: ns.ID, Kind: postgres.TimerKindLeaseRecovery,
		FireAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatalf("ScheduleTimer: %v", err)
	}

	sch := scheduler.New(s, scheduler.Options{TickInterval: 25 * time.Millisecond})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sch.Run(runCtx) }()

	waitFor(t, 5*time.Second, func() bool {
		return timerStatus(t, s, timerID) == postgres.TimerStatusFired
	})

	item := mustWorkItem(t, s, workItemID)
	if item.State != "ready" {
		t.Fatalf("previously-leased work item state = %q, want reclaimed to %q", item.State, "ready")
	}
	if topic := outboxTopicForTimer(t, s, timerID); topic != "dev.culture.nodes.timer.lease-recovery-swept" {
		t.Fatalf("topic = %q, want %q", topic, "dev.culture.nodes.timer.lease-recovery-swept")
	}
}

func TestSchedulerHealthReportsActiveAndAdvancingLastTick(t *testing.T) {
	s := requireStore(t)
	sch := scheduler.New(s, scheduler.Options{TickInterval: 20 * time.Millisecond})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if got := sch.Health().Status; got != scheduler.StatusStandby {
		t.Fatalf("Health().Status before Run = %q, want %q", got, scheduler.StatusStandby)
	}

	go func() { _ = sch.Run(runCtx) }()

	waitFor(t, 5*time.Second, func() bool {
		return sch.Health().Status == scheduler.StatusActive
	})
	waitFor(t, 5*time.Second, func() bool {
		return !sch.Health().LastTick.IsZero()
	})
	firstTick := sch.Health().LastTick
	waitFor(t, 5*time.Second, func() bool {
		return sch.Health().LastTick.After(firstTick)
	})
}

// TestSchedulerCrashBetweenEffectAndMarkFiredRefiresIdempotently exercises
// Hooks.AfterEffect: it fails the first time it is asked about this test's
// timer (simulating a crash between the effect applying and MarkFired
// committing, entirely within fireOne's one transaction -- see fireOne's
// doc comment), then succeeds. The scheduler must retry on its next tick
// and reach the correct final state, with no duplicate outbox event from
// the aborted attempt.
func TestSchedulerCrashBetweenEffectAndMarkFiredRefiresIdempotently(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "test-scheduler-crash-refire")
	nodeRunID := mustNodeRun(t, s, ns.ID)
	workItemID := insertWaitingWorkItem(t, s, ns.ID, nodeRunID)

	timerID := "tmr_" + store.NewULID()
	if _, err := s.ScheduleTimer(ctx, postgres.Timer{
		ID: timerID, NamespaceID: ns.ID, NodeRunID: nodeRunID, Kind: postgres.TimerKindWait,
		FireAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatalf("ScheduleTimer: %v", err)
	}

	var (
		mu        sync.Mutex
		calls     int
		failsLeft = 1
	)
	hooks := scheduler.Hooks{
		AfterEffect: func(pt postgres.Timer) error {
			if pt.ID != timerID {
				return nil
			}
			mu.Lock()
			defer mu.Unlock()
			calls++
			if failsLeft > 0 {
				failsLeft--
				return fmt.Errorf("simulated crash after effect, before MarkFired")
			}
			return nil
		},
	}

	sch := scheduler.New(s, scheduler.Options{TickInterval: 25 * time.Millisecond, Hooks: hooks})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sch.Run(runCtx) }()

	waitFor(t, 5*time.Second, func() bool {
		return timerStatus(t, s, timerID) == postgres.TimerStatusFired
	})

	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls < 2 {
		t.Fatalf("AfterEffect was called %d times, want at least 2 (one simulated crash, one successful retry)", gotCalls)
	}

	item := mustWorkItem(t, s, workItemID)
	if item.State != "ready" {
		t.Fatalf("work item state = %q, want %q", item.State, "ready")
	}
	if n := outboxCountForTimer(t, s, timerID); n != 1 {
		t.Fatalf("outbox rows for timer %q = %d, want exactly 1 (the rolled-back attempt must not have committed one)", timerID, n)
	}
}

// TestSchedulerStandbyTakesOverWhenActiveLosesItsConnection is THE takeover
// test: an active scheduler (A) holding the advisory lock with due timers
// pending has its lock connection killed from the outside -- via
// pg_terminate_backend, not by canceling A's own context -- so the test
// exercises PostgreSQL's own guarantee (a session-level advisory lock
// releases automatically when its session ends) rather than this
// package's graceful-shutdown code path. A standby (B) must then become
// active and every pending timer must fire exactly once: no loss, no
// double-fire.
func TestSchedulerStandbyTakesOverWhenActiveLosesItsConnection(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "test-scheduler-takeover")

	const n = 5
	var (
		timerIDs  []string
		workItems []string
	)
	for i := 0; i < n; i++ {
		nodeRunID := mustNodeRun(t, s, ns.ID)
		workItemID := insertWaitingWorkItem(t, s, ns.ID, nodeRunID)
		timerID := "tmr_" + store.NewULID()
		if _, err := s.ScheduleTimer(ctx, postgres.Timer{
			ID: timerID, NamespaceID: ns.ID, NodeRunID: nodeRunID, Kind: postgres.TimerKindWait,
			FireAt: time.Now().Add(-time.Second),
		}); err != nil {
			t.Fatalf("ScheduleTimer: %v", err)
		}
		timerIDs = append(timerIDs, timerID)
		workItems = append(workItems, workItemID)
	}

	// A: a deliberately long tick interval, so it acquires the lock but
	// (within this test's timeout) never actually ticks before its
	// connection is killed below -- the strongest form of "no loss": A
	// never claims a single timer, never applies an effect, never marks
	// anything fired. A does not need to notice its connection died for
	// this test to pass; that is the whole point of the mechanism (see
	// the package doc comment).
	a := scheduler.New(s, scheduler.Options{TickInterval: time.Hour})
	aCtx, aCancel := context.WithCancel(context.Background())
	defer aCancel()
	go func() { _ = a.Run(aCtx) }()

	waitFor(t, 5*time.Second, func() bool {
		return a.Health().Status == scheduler.StatusActive
	})

	for _, id := range timerIDs {
		if got := timerStatus(t, s, id); got != postgres.TimerStatusPending {
			t.Fatalf("timer %q status = %q before takeover, want %q (A must not have ticked yet)", id, got, postgres.TimerStatusPending)
		}
	}

	var lockPID int32
	if err := s.Pool().QueryRow(ctx,
		`SELECT pid FROM pg_locks WHERE locktype = 'advisory' AND granted LIMIT 1`,
	).Scan(&lockPID); err != nil {
		t.Fatalf("find A's lock-holding backend pid: %v", err)
	}
	if _, err := s.Pool().Exec(ctx, `SELECT pg_terminate_backend($1)`, lockPID); err != nil {
		t.Fatalf("pg_terminate_backend: %v", err)
	}

	// B: fast tick interval, takes over.
	b := scheduler.New(s, scheduler.Options{TickInterval: 25 * time.Millisecond})
	bCtx, bCancel := context.WithCancel(context.Background())
	defer bCancel()
	go func() { _ = b.Run(bCtx) }()

	waitFor(t, 10*time.Second, func() bool {
		return b.Health().Status == scheduler.StatusActive
	})

	for _, id := range timerIDs {
		id := id
		waitFor(t, 10*time.Second, func() bool {
			return timerStatus(t, s, id) == postgres.TimerStatusFired
		})
	}

	for i, id := range timerIDs {
		if got := outboxCountForTimer(t, s, id); got != 1 {
			t.Fatalf("outbox rows for timer %q = %d, want exactly 1 (no loss, no double-fire)", id, got)
		}
		item := mustWorkItem(t, s, workItems[i])
		if item.State != "ready" {
			t.Fatalf("work item for timer %q state = %q, want %q", id, item.State, "ready")
		}
	}
}
