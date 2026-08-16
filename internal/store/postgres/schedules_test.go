package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Store-level tests for the declared cadence (issue #107, task t33).
//
// Everything here drives the clock as a PARAMETER. FireSchedule takes the
// instant it is firing AT, and every assertion below picks that instant
// explicitly, so not one of these tests sleeps or waits for wall time. That
// is not only a speed decision: a test that proved a schedule by sleeping
// would prove the tick interval and nothing about the durability rules,
// which are the actual subject.

func mustSchedule(t *testing.T, s *postgres.Store, in postgres.CreateScheduleInput) postgres.Schedule {
	t.Helper()
	sc, err := s.CreateSchedule(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	return sc
}

// countEvents is the observable effect of a fire: one appended signal_events
// fact. Counting the facts is deliberately how these tests decide whether a
// schedule fired, rather than trusting FireSchedule's own return value --
// "did it double-start" is a question about what is durably in the database.
func countEvents(t *testing.T, s *postgres.Store, namespaceID, name string) int {
	t.Helper()
	var n int
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM signal_events WHERE namespace_id = $1 AND name = $2`,
		namespaceID, name).Scan(&n); err != nil {
		t.Fatalf("count signal events: %v", err)
	}
	return n
}

func TestScheduleFiresOnceWhenDueAndAdvancesItsOwnCadence(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "sched-fire")

	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	sc := mustSchedule(t, s, postgres.CreateScheduleInput{
		NamespaceID: ns.ID,
		Name:        "upkeep",
		EventName:   "upkeep.tick",
		Emitter:     "schedule",
		Payload:     json.RawMessage(`{"source":"schedule"}`),
		Interval:    time.Hour,
		FirstFireAt: base,
	})
	if !sc.NextFireAt.Equal(base) {
		t.Fatalf("NextFireAt = %s, want the declared first fire %s", sc.NextFireAt, base)
	}

	// One second before it is due: nothing happens, and nothing is recorded.
	res, err := s.FireSchedule(ctx, postgres.FireScheduleInput{ScheduleID: sc.ID, Now: base.Add(-time.Second)})
	if err != nil {
		t.Fatalf("FireSchedule (not yet due): %v", err)
	}
	if res.Fired {
		t.Fatalf("schedule fired %s before its declared next_fire_at %s", base.Add(-time.Second), base)
	}
	if got := countEvents(t, s, ns.ID, "upkeep.tick"); got != 0 {
		t.Fatalf("appended %d events before the schedule was due, want 0", got)
	}

	// Exactly due: it fires, appends its declared payload, and advances.
	res, err = s.FireSchedule(ctx, postgres.FireScheduleInput{ScheduleID: sc.ID, Now: base})
	if err != nil {
		t.Fatalf("FireSchedule (due): %v", err)
	}
	if !res.Fired {
		t.Fatalf("schedule did not fire at its declared next_fire_at %s", base)
	}
	if res.Delivery.Event.ID == "" {
		t.Fatal("a fired schedule reported no appended event; the event id is what links the run back to the cadence")
	}
	if string(res.Delivery.Event.Payload) != `{"source": "schedule"}` &&
		string(res.Delivery.Event.Payload) != `{"source":"schedule"}` {
		t.Fatalf("appended payload = %s, want the schedule's declared payload", res.Delivery.Event.Payload)
	}
	if want := base.Add(time.Hour); !res.Schedule.NextFireAt.Equal(want) {
		t.Fatalf("NextFireAt = %s after firing, want %s (one declared interval on)", res.Schedule.NextFireAt, want)
	}
	if res.Schedule.LastEventID != res.Delivery.Event.ID {
		t.Fatalf("LastEventID = %q, want the appended event %q", res.Schedule.LastEventID, res.Delivery.Event.ID)
	}

	// Firing again at the same instant must do nothing: the row says it is
	// no longer due, which is the whole double-start guard.
	res, err = s.FireSchedule(ctx, postgres.FireScheduleInput{ScheduleID: sc.ID, Now: base})
	if err != nil {
		t.Fatalf("FireSchedule (already fired): %v", err)
	}
	if res.Fired {
		t.Fatal("schedule fired twice for the same occurrence")
	}
	if got := countEvents(t, s, ns.ID, "upkeep.tick"); got != 1 {
		t.Fatalf("appended %d events for one occurrence, want exactly 1", got)
	}
}

func TestDisablingAScheduleStopsItStarting(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "sched-disable")

	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	sc := mustSchedule(t, s, postgres.CreateScheduleInput{
		NamespaceID: ns.ID, Name: "upkeep", EventName: "disable.tick",
		Emitter: "schedule", Interval: time.Minute, FirstFireAt: base,
	})

	if _, err := s.SetScheduleEnabled(ctx, ns.ID, sc.ID, false); err != nil {
		t.Fatalf("SetScheduleEnabled(false): %v", err)
	}

	// A disabled schedule is invisible to the due scan...
	due, err := s.DueSchedules(ctx, base.Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("DueSchedules: %v", err)
	}
	for _, d := range due {
		if d.ID == sc.ID {
			t.Fatal("a disabled schedule was returned as due; disabling must stop it starting")
		}
	}
	// ...and refuses to fire even when asked directly, so a stale id already
	// in a tick's batch cannot slip past the disable either.
	res, err := s.FireSchedule(ctx, postgres.FireScheduleInput{ScheduleID: sc.ID, Now: base.Add(time.Hour)})
	if err != nil {
		t.Fatalf("FireSchedule (disabled): %v", err)
	}
	if res.Fired {
		t.Fatal("a disabled schedule fired")
	}
	if got := countEvents(t, s, ns.ID, "disable.tick"); got != 0 {
		t.Fatalf("a disabled schedule appended %d events, want 0", got)
	}

	// Re-enabling resumes it, so "disabled" is a pause and not a deletion.
	if _, err := s.SetScheduleEnabled(ctx, ns.ID, sc.ID, true); err != nil {
		t.Fatalf("SetScheduleEnabled(true): %v", err)
	}
	res, err = s.FireSchedule(ctx, postgres.FireScheduleInput{ScheduleID: sc.ID, Now: base.Add(time.Hour)})
	if err != nil {
		t.Fatalf("FireSchedule (re-enabled): %v", err)
	}
	if !res.Fired {
		t.Fatal("a re-enabled, overdue schedule did not fire")
	}
}

// TestConcurrentFiresOfOneOccurrenceStartExactlyOneRun is the rolling-deploy
// window: two control planes tick at the same instant and both read the
// schedule as due. Exactly one appended event is the pass condition.
func TestConcurrentFiresOfOneOccurrenceStartExactlyOneRun(t *testing.T) {
	s := requireStore(t)
	ns := mustNamespace(t, s, "sched-race")

	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	sc := mustSchedule(t, s, postgres.CreateScheduleInput{
		NamespaceID: ns.ID, Name: "upkeep", EventName: "race.tick",
		Emitter: "schedule", Interval: time.Hour, FirstFireAt: base,
	})

	const racers = 8
	var wg sync.WaitGroup
	fired := make([]bool, racers)
	errs := make([]error, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := s.FireSchedule(context.Background(),
				postgres.FireScheduleInput{ScheduleID: sc.ID, Now: base})
			fired[i], errs[i] = res.Fired, err
		}(i)
	}
	close(start)
	wg.Wait()

	wins := 0
	for i := range fired {
		if errs[i] != nil {
			t.Fatalf("racer %d: FireSchedule: %v", i, errs[i])
		}
		if fired[i] {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("%d of %d concurrent fires reported firing, want exactly 1", wins, racers)
	}
	if got := countEvents(t, s, ns.ID, "race.tick"); got != 1 {
		t.Fatalf("%d concurrent fires appended %d events, want exactly 1", racers, got)
	}
}

// TestACrashBetweenDecidingDueAndCommittingLosesNothing is criterion 2's
// second failure mode, made deterministic. The hook fails inside the still
// open transaction, at the exact point where the row has been advanced and
// the event appended but nothing has committed -- which is where a killed
// process would leave things. Nothing may survive, and the occurrence must
// still be due afterwards.
func TestACrashBetweenDecidingDueAndCommittingLosesNothing(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "sched-crash")

	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	sc := mustSchedule(t, s, postgres.CreateScheduleInput{
		NamespaceID: ns.ID, Name: "upkeep", EventName: "crash.tick",
		Emitter: "schedule", Interval: time.Hour, FirstFireAt: base,
	})

	boom := errors.New("simulated crash after advancing, before commit")
	_, err := s.FireSchedule(ctx, postgres.FireScheduleInput{
		ScheduleID:   sc.ID,
		Now:          base,
		BeforeCommit: func(postgres.Schedule) error { return boom },
	})
	if !errors.Is(err, boom) {
		t.Fatalf("FireSchedule error = %v, want the injected crash", err)
	}

	if got := countEvents(t, s, ns.ID, "crash.tick"); got != 0 {
		t.Fatalf("a fire that never committed left %d events behind, want 0", got)
	}
	after, err := s.Schedule(ctx, ns.ID, sc.ID)
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if !after.NextFireAt.Equal(base) {
		t.Fatalf("NextFireAt = %s after an uncommitted fire, want it unchanged at %s", after.NextFireAt, base)
	}
	if after.FireCount != 0 {
		t.Fatalf("FireCount = %d after an uncommitted fire, want 0", after.FireCount)
	}

	// The retry -- which is what a restarted process does -- fires it once.
	res, err := s.FireSchedule(ctx, postgres.FireScheduleInput{ScheduleID: sc.ID, Now: base})
	if err != nil {
		t.Fatalf("FireSchedule (retry after crash): %v", err)
	}
	if !res.Fired {
		t.Fatal("the occurrence was lost: a retry after an uncommitted fire did not fire it")
	}
	if got := countEvents(t, s, ns.ID, "crash.tick"); got != 1 {
		t.Fatalf("appended %d events after crash+retry, want exactly 1", got)
	}
}

// TestCatchUpFireOnceFiresLateExactlyOnceAndRealigns covers the schedule that
// came due while nothing was running. The declared answer is "fire late, once,
// and do not backfill" -- the missed occurrences are counted, not replayed.
func TestCatchUpFireOnceFiresLateExactlyOnceAndRealigns(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "sched-catchup")

	base := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	sc := mustSchedule(t, s, postgres.CreateScheduleInput{
		NamespaceID: ns.ID, Name: "upkeep", EventName: "catchup.tick",
		Emitter: "schedule", Interval: time.Hour, FirstFireAt: base,
		CatchUp: postgres.CatchUpFireOnce,
	})

	// The process was down for five and a half hours.
	now := base.Add(5*time.Hour + 30*time.Minute)
	res, err := s.FireSchedule(ctx, postgres.FireScheduleInput{ScheduleID: sc.ID, Now: now})
	if err != nil {
		t.Fatalf("FireSchedule: %v", err)
	}
	if !res.Fired {
		t.Fatal("an overdue fire-once schedule did not fire; a missed occurrence must not be silently lost")
	}
	if res.Missed != 5 {
		t.Fatalf("Missed = %d, want 5 whole intervals passed while nothing was running", res.Missed)
	}
	if got := countEvents(t, s, ns.ID, "catchup.tick"); got != 1 {
		t.Fatalf("appended %d events for a 5-interval outage, want exactly 1 (no backfill storm)", got)
	}
	// Realigned to the phase the operator declared, not to now+interval.
	if want := base.Add(6 * time.Hour); !res.Schedule.NextFireAt.Equal(want) {
		t.Fatalf("NextFireAt = %s, want %s (the next declared boundary after %s)", res.Schedule.NextFireAt, want, now)
	}
}

// TestCatchUpSkipDeclinesAMissedOccurrenceAndRecordsIt is the other declared
// answer: a nightly sweep that was missed overnight should not run at noon.
// It must still leave a durable trace -- a schedule that quietly did nothing
// is exactly the "silently loses the schedule" failure.
func TestCatchUpSkipDeclinesAMissedOccurrenceAndRecordsIt(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "sched-skip")

	base := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	sc := mustSchedule(t, s, postgres.CreateScheduleInput{
		NamespaceID: ns.ID, Name: "upkeep", EventName: "skip.tick",
		Emitter: "schedule", Interval: time.Hour, FirstFireAt: base,
		CatchUp: postgres.CatchUpSkip,
	})

	res, err := s.FireSchedule(ctx, postgres.FireScheduleInput{
		ScheduleID: sc.ID, Now: base.Add(3*time.Hour + 10*time.Minute)})
	if err != nil {
		t.Fatalf("FireSchedule: %v", err)
	}
	if res.Fired {
		t.Fatal("a catch-up=skip schedule fired for an occurrence missed 3 intervals ago")
	}
	if !res.Skipped {
		t.Fatal("the declined occurrence was not reported as skipped")
	}
	if got := countEvents(t, s, ns.ID, "skip.tick"); got != 0 {
		t.Fatalf("a skipped occurrence appended %d events, want 0", got)
	}
	if res.Schedule.SkipCount != 1 {
		t.Fatalf("SkipCount = %d, want 1: a declined occurrence must leave a trace", res.Schedule.SkipCount)
	}
	if want := base.Add(4 * time.Hour); !res.Schedule.NextFireAt.Equal(want) {
		t.Fatalf("NextFireAt = %s after a skip, want %s", res.Schedule.NextFireAt, want)
	}

	// Merely LATE (within one interval) is not missed: it still fires.
	res, err = s.FireSchedule(ctx, postgres.FireScheduleInput{
		ScheduleID: sc.ID, Now: base.Add(4*time.Hour + 30*time.Minute)})
	if err != nil {
		t.Fatalf("FireSchedule (late but not missed): %v", err)
	}
	if !res.Fired {
		t.Fatal("catch-up=skip declined an occurrence that was late by less than one interval")
	}
}

func TestDueSchedulesReturnsOnlyEnabledSchedulesThatAreDue(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "sched-due")

	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	dueNow := mustSchedule(t, s, postgres.CreateScheduleInput{
		NamespaceID: ns.ID, Name: "due", EventName: "due.tick",
		Emitter: "schedule", Interval: time.Hour, FirstFireAt: base,
	})
	later := mustSchedule(t, s, postgres.CreateScheduleInput{
		NamespaceID: ns.ID, Name: "later", EventName: "later.tick",
		Emitter: "schedule", Interval: time.Hour, FirstFireAt: base.Add(time.Hour),
	})

	got, err := s.DueSchedules(ctx, base, 100)
	if err != nil {
		t.Fatalf("DueSchedules: %v", err)
	}
	seen := map[string]bool{}
	for _, d := range got {
		seen[d.ID] = true
		if d.NamespaceID == "" {
			t.Fatal("a due schedule carried no namespace; the tick needs it to pick the right engine")
		}
	}
	if !seen[dueNow.ID] {
		t.Fatal("a due, enabled schedule was not returned")
	}
	if seen[later.ID] {
		t.Fatal("a schedule whose next_fire_at is in the future was returned as due")
	}
}

func TestCreateScheduleRefusesADeclarationItCannotHonour(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "sched-validate")

	for _, tc := range []struct {
		name string
		in   postgres.CreateScheduleInput
	}{
		{"no interval", postgres.CreateScheduleInput{
			NamespaceID: ns.ID, Name: "a", EventName: "x", Emitter: "s"}},
		{"negative interval", postgres.CreateScheduleInput{
			NamespaceID: ns.ID, Name: "b", EventName: "x", Emitter: "s", Interval: -time.Hour}},
		{"no event name", postgres.CreateScheduleInput{
			NamespaceID: ns.ID, Name: "c", Emitter: "s", Interval: time.Hour}},
		{"no name", postgres.CreateScheduleInput{
			NamespaceID: ns.ID, EventName: "x", Emitter: "s", Interval: time.Hour}},
		{"unknown catch-up policy", postgres.CreateScheduleInput{
			NamespaceID: ns.ID, Name: "d", EventName: "x", Emitter: "s",
			Interval: time.Hour, CatchUp: postgres.ScheduleCatchUp("whenever")}},
	} {
		if _, err := s.CreateSchedule(ctx, tc.in); err == nil {
			t.Errorf("CreateSchedule accepted %s", tc.name)
		}
	}

	// A duplicate name in one namespace is refused: the name is the handle an
	// operator disables by, and two schedules answering to it would make
	// "disable the upkeep sweep" ambiguous.
	in := postgres.CreateScheduleInput{
		NamespaceID: ns.ID, Name: "unique", EventName: "x", Emitter: "s", Interval: time.Hour}
	if _, err := s.CreateSchedule(ctx, in); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if _, err := s.CreateSchedule(ctx, in); err == nil {
		t.Error("CreateSchedule accepted a duplicate name in the same namespace")
	}
}
