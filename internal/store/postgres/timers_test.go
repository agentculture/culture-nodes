package postgres_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// countTimerRows is a raw-SQL escape hatch (the same convention
// claiming_test.go and ledger_test.go use) for asserting how many rows
// exist under an ID -- something the typed API deliberately does not
// expose, since ScheduleTimer's whole idempotency contract is "callers
// never need to ask this question."
func countTimerRows(t *testing.T, s *postgres.Store, id string) int {
	t.Helper()
	var n int
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM timers WHERE id = $1`, id,
	).Scan(&n); err != nil {
		t.Fatalf("countTimerRows: %v", err)
	}
	return n
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

func TestScheduleTimerRoundTrip(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-schedule-timer")

	fireAt := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	id := "tmr_" + store.NewULID()

	got, err := s.ScheduleTimer(ctx, postgres.Timer{
		ID:          id,
		NamespaceID: ns.ID,
		Kind:        postgres.TimerKindWait,
		FireAt:      fireAt,
	})
	if err != nil {
		t.Fatalf("ScheduleTimer: %v", err)
	}

	if got.ID != id {
		t.Fatalf("ID = %q, want %q", got.ID, id)
	}
	if got.NamespaceID != ns.ID {
		t.Fatalf("NamespaceID = %q, want %q", got.NamespaceID, ns.ID)
	}
	if got.Kind != postgres.TimerKindWait {
		t.Fatalf("Kind = %q, want %q", got.Kind, postgres.TimerKindWait)
	}
	if got.Status != postgres.TimerStatusPending {
		t.Fatalf("Status = %q, want %q", got.Status, postgres.TimerStatusPending)
	}
	if !got.FireAt.Equal(fireAt) {
		t.Fatalf("FireAt = %v, want %v", got.FireAt, fireAt)
	}
	if got.ClaimedBy != "" {
		t.Fatalf("ClaimedBy = %q, want empty for a freshly scheduled timer", got.ClaimedBy)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("CreatedAt was not set")
	}
	if string(got.Payload) != "{}" {
		t.Fatalf("Payload = %s, want {} (the zero-value default)", got.Payload)
	}
}

func TestScheduleTimerIdempotentByID(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-schedule-timer-idempotent")

	id := "tmr_" + store.NewULID()
	firstFireAt := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)

	first, err := s.ScheduleTimer(ctx, postgres.Timer{
		ID:          id,
		NamespaceID: ns.ID,
		Kind:        postgres.TimerKindRetry,
		FireAt:      firstFireAt,
	})
	if err != nil {
		t.Fatalf("ScheduleTimer (first): %v", err)
	}

	// A second call with the same ID but a different fire_at must not
	// change the existing row -- ScheduleTimer is idempotent by ID, not an
	// upsert-with-new-values.
	second, err := s.ScheduleTimer(ctx, postgres.Timer{
		ID:          id,
		NamespaceID: ns.ID,
		Kind:        postgres.TimerKindRetry,
		FireAt:      time.Now().Add(48 * time.Hour).UTC(),
	})
	if err != nil {
		t.Fatalf("ScheduleTimer (second): %v", err)
	}

	if !second.FireAt.Equal(first.FireAt) {
		t.Fatalf("second call's FireAt = %v, want unchanged %v", second.FireAt, first.FireAt)
	}
	if n := countTimerRows(t, s, id); n != 1 {
		t.Fatalf("timers rows for id %q = %d, want exactly 1", id, n)
	}
}

func TestClaimDueTimersOnlyClaimsDue(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-claim-due-only")

	dueID := "tmr_" + store.NewULID()
	futureID := "tmr_" + store.NewULID()

	if _, err := s.ScheduleTimer(ctx, postgres.Timer{
		ID: dueID, NamespaceID: ns.ID, Kind: postgres.TimerKindWait,
		FireAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("ScheduleTimer (due): %v", err)
	}
	if _, err := s.ScheduleTimer(ctx, postgres.Timer{
		ID: futureID, NamespaceID: ns.ID, Kind: postgres.TimerKindWait,
		FireAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("ScheduleTimer (future): %v", err)
	}

	claimed, err := s.ClaimDueTimers(ctx, "owner-1", time.Now(), 10)
	if err != nil {
		t.Fatalf("ClaimDueTimers: %v", err)
	}

	var sawDue bool
	for _, c := range claimed {
		if c.ID == futureID {
			t.Fatalf("ClaimDueTimers claimed the not-yet-due timer %q", futureID)
		}
		if c.ID == dueID {
			sawDue = true
			if c.ClaimedBy != "owner-1" {
				t.Fatalf("ClaimedBy = %q, want %q", c.ClaimedBy, "owner-1")
			}
			if c.ClaimedAt.IsZero() {
				t.Fatal("ClaimedAt was not stamped")
			}
			if c.Status != postgres.TimerStatusPending {
				t.Fatalf("Status = %q, want %q (claiming must not itself fire)", c.Status, postgres.TimerStatusPending)
			}
		}
	}
	if !sawDue {
		t.Fatalf("ClaimDueTimers did not claim the due timer %q", dueID)
	}

	// Retire dueID: ClaimDueTimers deliberately never changes status (see
	// claimDueTimersSQL's doc comment), so an un-fired due timer stays
	// globally claimable forever -- this table has no namespace filter,
	// same as ClaimWork -- and would otherwise pollute every later test in
	// this package that calls ClaimDueTimers with a small limit.
	if _, err := s.MarkFired(ctx, dueID, "owner-1"); err != nil {
		t.Fatalf("MarkFired (cleanup): %v", err)
	}
}

// TestClaimDueTimersLastClaimWinsOwnership proves the real invariant
// ClaimDueTimers provides -- see claimDueTimersSQL's doc comment for why it
// is deliberately NOT single-winner claiming: a later ClaimDueTimers call
// can re-claim an already-claimed-but-not-yet-fired timer (that is what
// lets a standby recover one an active instance died holding), and once it
// does, the earlier claimant's ownership is gone -- MarkFired rejects it.
func TestClaimDueTimersLastClaimWinsOwnership(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-claim-due-last-wins")

	id := "tmr_" + store.NewULID()
	if _, err := s.ScheduleTimer(ctx, postgres.Timer{
		ID: id, NamespaceID: ns.ID, Kind: postgres.TimerKindRetry,
		FireAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatalf("ScheduleTimer: %v", err)
	}

	if _, err := s.ClaimDueTimers(ctx, "owner-a", time.Now(), 10); err != nil {
		t.Fatalf("ClaimDueTimers (owner-a): %v", err)
	}
	claimedB, err := s.ClaimDueTimers(ctx, "owner-b", time.Now(), 10)
	if err != nil {
		t.Fatalf("ClaimDueTimers (owner-b): %v", err)
	}
	var sawIt bool
	for _, c := range claimedB {
		if c.ID == id {
			sawIt = true
		}
	}
	if !sawIt {
		t.Fatalf("owner-b's ClaimDueTimers did not re-claim still-pending timer %q", id)
	}

	// owner-a's claim was overwritten; its MarkFired must be rejected.
	fired, err := s.MarkFired(ctx, id, "owner-a")
	if err != nil {
		t.Fatalf("MarkFired (owner-a, stale): %v", err)
	}
	if fired {
		t.Fatal("MarkFired succeeded for owner-a after owner-b re-claimed the timer")
	}

	// owner-b, the current claimant, succeeds.
	fired, err = s.MarkFired(ctx, id, "owner-b")
	if err != nil {
		t.Fatalf("MarkFired (owner-b): %v", err)
	}
	if !fired {
		t.Fatal("MarkFired did not report success for owner-b, the current claimant")
	}
}

// TestClaimDueTimersConcurrentCallsDoNotDeadlockOrLoseRows is the
// SKIP-LOCKED liveness half of claimDueTimersSQL's contract: concurrent
// claim calls racing over an overlapping set of due rows never block each
// other out, deadlock, or drop a row. Note what this test does NOT assert:
// TestClaimDueTimersLastClaimWinsOwnership above already establishes that
// ClaimDueTimers is deliberately not single-winner claiming, so two racing
// calls legitimately CAN both return the same row -- this test's job is
// only to prove that racing is safe and every row this test created is
// still reachable afterward, not that the race partitions cleanly.
//
// It uses a limit well above n so pre-existing due timers left behind by
// other tests in this package (there should be none -- every test that
// claims a due timer retires it with MarkFired or CancelTimer before
// returning, since this table has no namespace filter -- but this test
// does not depend on that being perfectly true) cannot cause it to miss
// one of its own n rows.
func TestClaimDueTimersConcurrentCallsDoNotDeadlockOrLoseRows(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-claim-due-concurrency")

	const n = 20
	ids := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		id := "tmr_" + store.NewULID()
		ids[id] = true
		if _, err := s.ScheduleTimer(ctx, postgres.Timer{
			ID: id, NamespaceID: ns.ID, Kind: postgres.TimerKindRetry,
			FireAt: time.Now().Add(-time.Second),
		}); err != nil {
			t.Fatalf("ScheduleTimer: %v", err)
		}
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		callErr error
	)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			if _, err := s.ClaimDueTimers(ctx, owner, time.Now(), n*10); err != nil {
				mu.Lock()
				callErr = err
				mu.Unlock()
			}
		}(fmt.Sprintf("owner-%d", i))
	}
	wg.Wait()
	if callErr != nil {
		t.Fatalf("ClaimDueTimers: %v", callErr)
	}

	// Cleanup-and-verify in one pass: claim once more (picking up whichever
	// owner ended up as claimed_by for each of this test's rows, per
	// last-claim-wins), then mark every one of this test's own IDs fired.
	// If any of the n rows were lost by the race above, it will not appear
	// among final's due rows and MarkFired below will report false for it.
	final, err := s.ClaimDueTimers(ctx, "closer", time.Now(), n*10)
	if err != nil {
		t.Fatalf("ClaimDueTimers (closer): %v", err)
	}
	found := make(map[string]bool, n)
	for _, c := range final {
		if ids[c.ID] {
			found[c.ID] = true
		}
	}
	if len(found) != n {
		t.Fatalf("closer's ClaimDueTimers reached %d of this test's %d timers, want all %d (a row was lost)", len(found), n, n)
	}
	for id := range ids {
		fired, err := s.MarkFired(ctx, id, "closer")
		if err != nil {
			t.Fatalf("MarkFired (cleanup, %s): %v", id, err)
		}
		if !fired {
			t.Fatalf("MarkFired (cleanup, %s) did not report success for the closer claim", id)
		}
	}
}

func TestMarkFiredGuardsByOwnerAndStatus(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-mark-fired-guard")

	id := "tmr_" + store.NewULID()
	if _, err := s.ScheduleTimer(ctx, postgres.Timer{
		ID: id, NamespaceID: ns.ID, Kind: postgres.TimerKindDeadline,
		FireAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatalf("ScheduleTimer: %v", err)
	}

	claimed, err := s.ClaimDueTimers(ctx, "owner-1", time.Now(), 10)
	if err != nil {
		t.Fatalf("ClaimDueTimers: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("ClaimDueTimers returned %d, want 1", len(claimed))
	}

	// A stale/wrong owner (as if a standby raced in and thinks it also
	// owns this timer) must not be able to mark it fired.
	fired, err := s.MarkFired(ctx, id, "owner-2")
	if err != nil {
		t.Fatalf("MarkFired (wrong owner): %v", err)
	}
	if fired {
		t.Fatal("MarkFired succeeded under the wrong owner")
	}
	if got := timerStatus(t, s, id); got != postgres.TimerStatusPending {
		t.Fatalf("status after wrong-owner MarkFired = %q, want %q", got, postgres.TimerStatusPending)
	}

	// The actual claimant succeeds.
	fired, err = s.MarkFired(ctx, id, "owner-1")
	if err != nil {
		t.Fatalf("MarkFired (correct owner): %v", err)
	}
	if !fired {
		t.Fatal("MarkFired did not report success for the correct owner")
	}
	if got := timerStatus(t, s, id); got != postgres.TimerStatusFired {
		t.Fatalf("status after MarkFired = %q, want %q", got, postgres.TimerStatusFired)
	}

	// A second call, even from the correct owner, is now a guarded no-op:
	// status is no longer 'pending'.
	fired, err = s.MarkFired(ctx, id, "owner-1")
	if err != nil {
		t.Fatalf("MarkFired (already fired): %v", err)
	}
	if fired {
		t.Fatal("MarkFired reported success on an already-fired timer")
	}
}

func TestCancelTimerPreventsFiring(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-cancel-timer")

	id := "tmr_" + store.NewULID()
	if _, err := s.ScheduleTimer(ctx, postgres.Timer{
		ID: id, NamespaceID: ns.ID, Kind: postgres.TimerKindWait,
		FireAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatalf("ScheduleTimer: %v", err)
	}

	if err := s.CancelTimer(ctx, id); err != nil {
		t.Fatalf("CancelTimer: %v", err)
	}
	if got := timerStatus(t, s, id); got != postgres.TimerStatusCanceled {
		t.Fatalf("status after CancelTimer = %q, want %q", got, postgres.TimerStatusCanceled)
	}

	claimed, err := s.ClaimDueTimers(ctx, "owner-1", time.Now(), 10)
	if err != nil {
		t.Fatalf("ClaimDueTimers: %v", err)
	}
	for _, c := range claimed {
		if c.ID == id {
			t.Fatalf("ClaimDueTimers claimed a canceled timer %q", id)
		}
	}

	// Canceling a second time, or a nonexistent ID, is a no-op, not an
	// error -- matching queue.Queue's Ack/Delay idempotency convention.
	if err := s.CancelTimer(ctx, id); err != nil {
		t.Fatalf("CancelTimer (already canceled): %v", err)
	}
	if err := s.CancelTimer(ctx, "tmr_"+store.NewULID()); err != nil {
		t.Fatalf("CancelTimer (nonexistent): %v", err)
	}
}

func TestMarkFiredTxRollsBackWithTransaction(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-mark-fired-tx")

	id := "tmr_" + store.NewULID()
	if _, err := s.ScheduleTimer(ctx, postgres.Timer{
		ID: id, NamespaceID: ns.ID, Kind: postgres.TimerKindDeadline,
		FireAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatalf("ScheduleTimer: %v", err)
	}
	if _, err := s.ClaimDueTimers(ctx, "owner-1", time.Now(), 10); err != nil {
		t.Fatalf("ClaimDueTimers: %v", err)
	}

	// Simulate a crash between the effect and the commit: MarkFiredTx runs
	// inside a transaction that is then rolled back instead of committed.
	tx, err := s.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	fired, err := postgres.MarkFiredTx(ctx, tx, id, "owner-1")
	if err != nil {
		t.Fatalf("MarkFiredTx: %v", err)
	}
	if !fired {
		t.Fatal("MarkFiredTx did not report success before rollback")
	}
	if _, err := postgres.InsertOutboxTx(ctx, s, tx, postgres.InsertOutboxInput{
		NamespaceID: ns.ID,
		Topic:       "dev.culture.nodes.timer.deadline-expired",
	}); err != nil {
		t.Fatalf("InsertOutboxTx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// Nothing committed: the timer is still 'pending' for a later tick to
	// retry, exactly as a real process crash at this point would leave it.
	if got := timerStatus(t, s, id); got != postgres.TimerStatusPending {
		t.Fatalf("status after rolled-back MarkFiredTx = %q, want %q", got, postgres.TimerStatusPending)
	}

	// A second attempt, this time committed, succeeds end to end.
	tx2, err := s.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("Begin (2): %v", err)
	}
	fired, err = postgres.MarkFiredTx(ctx, tx2, id, "owner-1")
	if err != nil {
		t.Fatalf("MarkFiredTx (2): %v", err)
	}
	if !fired {
		t.Fatal("MarkFiredTx (2) did not report success")
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := timerStatus(t, s, id); got != postgres.TimerStatusFired {
		t.Fatalf("status after committed MarkFiredTx = %q, want %q", got, postgres.TimerStatusFired)
	}
}
