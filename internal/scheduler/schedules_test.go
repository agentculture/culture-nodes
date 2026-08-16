package scheduler_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/scheduler"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// The tick's schedule half (issue #107, task t33).
//
// These tests drive Scheduler.Tick directly with a clock they own. Nothing
// here waits for a ticker, and nothing here sleeps: the subject is what one
// tick does at a given instant, and a test that proved it by waiting would be
// testing time.Ticker.

// errInjectedScheduleCrash is the failure the batch-isolation test injects.
var errInjectedScheduleCrash = errors.New("injected: this schedule cannot commit")

// fakeClock is a clock the test moves by hand.
type fakeClock struct{ at time.Time }

func (c *fakeClock) now() time.Time { return c.at }

func newSchedulerAt(t *testing.T, s *postgres.Store, clock *fakeClock) *scheduler.Scheduler {
	t.Helper()
	return scheduler.New(s, scheduler.Options{Now: clock.now})
}

func mustSchedule(t *testing.T, s *postgres.Store, in postgres.CreateScheduleInput) postgres.Schedule {
	t.Helper()
	sc, err := s.CreateSchedule(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	return sc
}

func scheduleEvents(t *testing.T, s *postgres.Store, namespaceID, name string) []string {
	t.Helper()
	rows, err := s.Pool().Query(context.Background(),
		`SELECT id FROM signal_events WHERE namespace_id = $1 AND name = $2 ORDER BY created_at, id`,
		namespaceID, name)
	if err != nil {
		t.Fatalf("read signal events: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan signal event: %v", err)
		}
		out = append(out, id)
	}
	return out
}

// TestATickStartsADueScheduleWithNoHumanAction is acceptance criterion 1 at
// the tick level: nobody calls FireSchedule, nobody posts an event. The clock
// reaches the declared instant and the loop that was already running does the
// rest.
func TestATickStartsADueScheduleWithNoHumanAction(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "tick-fire")

	base := time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC)
	clock := &fakeClock{at: base.Add(-time.Minute)}
	sch := newSchedulerAt(t, s, clock)

	mustSchedule(t, s, postgres.CreateScheduleInput{
		NamespaceID: ns.ID, Name: "upkeep", EventName: "tick.upkeep",
		Emitter: "schedule", Interval: 30 * time.Minute, FirstFireAt: base,
		Payload: json.RawMessage(`{"source":"schedule"}`),
	})

	// Before the declared instant, a tick does nothing at all.
	if err := sch.Tick(ctx); err != nil {
		t.Fatalf("Tick (before due): %v", err)
	}
	if got := scheduleEvents(t, s, ns.ID, "tick.upkeep"); len(got) != 0 {
		t.Fatalf("a tick before the declared instant appended %d events, want 0", len(got))
	}

	// The clock reaches it. No command is issued between these two lines.
	clock.at = base
	if err := sch.Tick(ctx); err != nil {
		t.Fatalf("Tick (due): %v", err)
	}
	events := scheduleEvents(t, s, ns.ID, "tick.upkeep")
	if len(events) != 1 {
		t.Fatalf("a tick at the declared instant appended %d events, want exactly 1", len(events))
	}

	// A second tick at the same instant must not re-fire it.
	if err := sch.Tick(ctx); err != nil {
		t.Fatalf("Tick (same instant): %v", err)
	}
	if got := scheduleEvents(t, s, ns.ID, "tick.upkeep"); len(got) != 1 {
		t.Fatalf("a second tick at the same instant appended %d events, want still 1", len(got))
	}

	// The next declared boundary fires it again -- the cadence continues.
	clock.at = base.Add(30 * time.Minute)
	if err := sch.Tick(ctx); err != nil {
		t.Fatalf("Tick (next boundary): %v", err)
	}
	if got := scheduleEvents(t, s, ns.ID, "tick.upkeep"); len(got) != 2 {
		t.Fatalf("the next boundary appended %d events in total, want 2", len(got))
	}
}

// TestRestartingTheControlPlaneNeitherDoubleStartsNorLosesTheSchedule is
// acceptance criterion 2 at the tick level. "Restart" here is what a restart
// actually is to this package: the old Scheduler value is dropped on the
// floor, unreferenced, and a brand new one is constructed over the same
// database with a different owner id. Everything the schedule knows about
// itself is in PostgreSQL, which is the whole claim being tested.
func TestRestartingTheControlPlaneNeitherDoubleStartsNorLosesTheSchedule(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "tick-restart")

	base := time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC)
	clock := &fakeClock{at: base}
	mustSchedule(t, s, postgres.CreateScheduleInput{
		NamespaceID: ns.ID, Name: "upkeep", EventName: "restart.upkeep",
		Emitter: "schedule", Interval: time.Hour, FirstFireAt: base,
	})

	before := newSchedulerAt(t, s, clock)
	if err := before.Tick(ctx); err != nil {
		t.Fatalf("Tick (before restart): %v", err)
	}
	fired := scheduleEvents(t, s, ns.ID, "restart.upkeep")
	if len(fired) != 1 {
		t.Fatalf("appended %d events before the restart, want 1", len(fired))
	}

	// --- the process dies here; a new one comes up ---
	after := newSchedulerAt(t, s, clock)
	if after.OwnerID() == before.OwnerID() {
		t.Fatal("the restarted scheduler reused the old owner id; this test is not testing a restart")
	}
	if err := after.Tick(ctx); err != nil {
		t.Fatalf("Tick (after restart): %v", err)
	}
	if got := scheduleEvents(t, s, ns.ID, "restart.upkeep"); len(got) != 1 {
		t.Fatalf("the restarted control plane double-started the occurrence: %d events, want 1", len(got))
	}

	// ...and the schedule is not lost either: the next boundary still fires.
	clock.at = base.Add(time.Hour)
	if err := after.Tick(ctx); err != nil {
		t.Fatalf("Tick (next boundary after restart): %v", err)
	}
	got := scheduleEvents(t, s, ns.ID, "restart.upkeep")
	if len(got) != 2 {
		t.Fatalf("the restarted control plane lost the schedule: %d events after the next boundary, want 2", len(got))
	}
	if got[0] == got[1] {
		t.Fatal("the two occurrences share an event id")
	}
}

// TestAScheduleThatCameDueWhileTheProcessWasDownFiresLateExactlyOnce is the
// third restart case, and the one a naive last_run_at check gets wrong in the
// other direction: the process was down across four whole occurrences.
func TestAScheduleThatCameDueWhileTheProcessWasDownFiresLateExactlyOnce(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "tick-down")

	base := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	clock := &fakeClock{at: base.Add(-time.Minute)}
	mustSchedule(t, s, postgres.CreateScheduleInput{
		NamespaceID: ns.ID, Name: "upkeep", EventName: "down.upkeep",
		Emitter: "schedule", Interval: time.Hour, FirstFireAt: base,
		CatchUp: postgres.CatchUpFireOnce,
	})

	// Nothing ran between base and base+4h30m. The control plane comes back.
	clock.at = base.Add(4*time.Hour + 30*time.Minute)
	sch := newSchedulerAt(t, s, clock)
	if err := sch.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := scheduleEvents(t, s, ns.ID, "down.upkeep"); len(got) != 1 {
		t.Fatalf("a four-interval outage produced %d events on recovery, want exactly 1", len(got))
	}
	// And it is realigned, so the very next tick does not fire again.
	if err := sch.Tick(ctx); err != nil {
		t.Fatalf("Tick (immediately after recovery): %v", err)
	}
	if got := scheduleEvents(t, s, ns.ID, "down.upkeep"); len(got) != 1 {
		t.Fatalf("the recovered schedule fired again on the next tick: %d events, want 1", len(got))
	}
}

// TestATickCarriesOnWhenOneScheduleFails is the batch-isolation rule the
// timer half already follows: one schedule that cannot fire must not stop the
// others in the same tick.
func TestATickCarriesOnWhenOneScheduleFails(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "tick-isolate")

	base := time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC)
	clock := &fakeClock{at: base}
	mustSchedule(t, s, postgres.CreateScheduleInput{
		NamespaceID: ns.ID, Name: "aaa-doomed", EventName: "isolate.doomed",
		Emitter: "schedule", Interval: time.Hour, FirstFireAt: base,
	})
	mustSchedule(t, s, postgres.CreateScheduleInput{
		NamespaceID: ns.ID, Name: "zzz-healthy", EventName: "isolate.healthy",
		Emitter: "schedule", Interval: time.Hour, FirstFireAt: base,
	})

	sch := scheduler.New(s, scheduler.Options{
		Now: clock.now,
		Hooks: scheduler.Hooks{
			BeforeScheduleCommit: func(sc postgres.Schedule) error {
				if sc.Name == "aaa-doomed" {
					return errInjectedScheduleCrash
				}
				return nil
			},
		},
	})
	if err := sch.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := scheduleEvents(t, s, ns.ID, "isolate.doomed"); len(got) != 0 {
		t.Fatalf("the failing schedule committed %d events, want 0", len(got))
	}
	if got := scheduleEvents(t, s, ns.ID, "isolate.healthy"); len(got) != 1 {
		t.Fatalf("a healthy schedule in the same batch appended %d events, want 1", len(got))
	}
}
