package engine_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Schedule backoff and the singleton failing-schedule alert (task t9, spec
// c40/c4/c44, issue #253).
//
// The defect these tests pin is not hypothetical arithmetic: on 2026-08-29/30
// the `pr-upkeep-sweep-5m` schedule minted 184 consecutive
// `contract_rejected` runs whose attempts all carried the SAME
// `result.error.detail`. Every one of those runs cost a dispatch, a ledger
// trail and an operator's attention, and not one of them carried information
// the first had not already carried. A cadence is a declaration that the work
// is worth doing again; it is not a declaration that it is worth doing again
// with the environment in a state that has already been measured as unable to
// do it.
//
// So the contract is a rate, not a stop: a schedule whose last two runs
// failed identically stops minting on its declared cadence and mints a PROBE
// at most every NODES_SCHEDULE_PROBE_INTERVAL. The probe is the half that
// makes suppression safe to apply automatically — a repaired environment is
// discovered by the system rather than by an operator remembering to resume
// something.
//
// Everything here drives the real mint path (Store.FireSchedule with a real
// *engine.Engine as its Trigger), because a test that hand-wrote schedule
// rows would prove the arithmetic and not the behaviour. Time is passed in
// rather than slept: three hours of a five-minute cadence is 36 ticks, and no
// test may take three hours to say so.

const (
	// scheduleTestCadence is the five-minute cadence the real
	// pr-upkeep-sweep-5m schedule declares.
	scheduleTestCadence = 5 * time.Minute
	// scheduleTestProbe is NODES_SCHEDULE_PROBE_INTERVAL's default.
	scheduleTestProbe = 30 * time.Minute
	// scheduleTestAlertAfter is NODES_SWEEP_FAILURE_ALERT_AFTER's default.
	scheduleTestAlertAfter = 3
	// scheduleTestDetail is the one repeated reason every failing run
	// reports — the shape internal/worker's diagnosticOutput writes.
	scheduleTestDetail = `node "work" reported outcome "changes_required", which the pinned definition does not declare`
)

// newFailingSweepSchedule declares a five-minute schedule on the fixture's
// trigger-bearing workflow, first due at `start`.
func newFailingSweepSchedule(t *testing.T, f *fixture, name string, start time.Time) storepg.Schedule {
	t.Helper()
	sc, err := f.store.CreateSchedule(f.ctx, storepg.CreateScheduleInput{
		NamespaceID: f.ns.ID,
		Name:        name,
		EventName:   "test.subject-event",
		Emitter:     "schedule:" + name,
		Payload:     json.RawMessage(`{}`),
		Interval:    scheduleTestCadence,
		FirstFireAt: start,
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	return sc
}

// tickSchedule is one scheduler tick's schedule half, at an instant the test
// chooses. It is the same call internal/scheduler's fireDueSchedules makes.
func (f *fixture) tickSchedule(scheduleID string, at time.Time) storepg.ScheduleFireResult {
	f.t.Helper()
	res, err := f.store.FireSchedule(f.ctx, storepg.FireScheduleInput{
		ScheduleID:    scheduleID,
		Now:           at,
		Pickup:        f.engine,
		Trigger:       f.engine,
		ProbeInterval: scheduleTestProbe,
		AlertAfter:    scheduleTestAlertAfter,
	})
	if err != nil {
		f.t.Fatalf("FireSchedule at %s: %v", at, err)
	}
	return res
}

// failMintedRun makes the run the sweep just minted fail technically with
// `detail` — a broken environment, reported the way a real worker reports one
// (internal/worker.diagnosticOutput writes {"error":{"class","detail"}}).
func (f *fixture) failMintedRun(runID, detail string) {
	f.t.Helper()
	nodeRun := f.readyNodeRun(runID)
	output, err := json.Marshal(map[string]any{
		"error": map[string]any{"class": "execution", "detail": detail},
	})
	if err != nil {
		f.t.Fatalf("marshal failure output: %v", err)
	}
	f.step("sweep-worker", nodeRun.ID, engine.CompletionRequest{
		TechStatus: engine.StatusFailed,
		Output:     json.RawMessage(output),
	})
	if got := f.run(runID).State; got != engine.RunFailed {
		f.t.Fatalf("minted run %s state = %q, want %q", runID, got, engine.RunFailed)
	}
}

// completeMintedRun is the repaired environment: the same node succeeds and
// the run reaches its end node.
func (f *fixture) completeMintedRun(runID string) {
	f.t.Helper()
	nodeRun := f.readyNodeRun(runID)
	f.step("sweep-worker", nodeRun.ID, succeeded("completed", `{}`))
	if got := f.run(runID).State; got != engine.RunCompleted {
		f.t.Fatalf("repaired run %s state = %q, want %q", runID, got, engine.RunCompleted)
	}
}

// pendingScheduleFailingTasks counts the schedule_failing tasks still awaiting
// a human for one schedule.
func (f *fixture) pendingScheduleFailingTasks(scheduleID string) int {
	f.t.Helper()
	return f.countScalar(
		`SELECT COUNT(*)::int FROM human_tasks
		 WHERE kind='schedule_failing' AND status='pending' AND request->>'schedule_id'=$1`, scheduleID)
}

func (f *fixture) scheduleRow(scheduleID string) storepg.Schedule {
	f.t.Helper()
	sc, err := f.store.Schedule(f.ctx, f.ns.ID, scheduleID)
	if err != nil {
		f.t.Fatalf("read schedule %s: %v", scheduleID, err)
	}
	return sc
}

// TestFixedFailingRunnerSuppressesTicksToOneProbePerIntervalAndRaisesOneTask
// is the acceptance test for task t9's first two clauses, run against three
// simulated hours of the real five-minute cadence.
//
// Two things are asserted that a "does it back off" test would miss:
//
//   - the mint rate AFTER suppression engages is at most one per probe
//     interval, and the ticks that did not mint are COUNTED
//     (schedules.suppressed_count) rather than silently skipped. A schedule
//     that quietly stops is the failure mode 0033's skip_count already exists
//     to prevent; suppression must not reintroduce it.
//   - exactly one pending schedule_failing task exists at EVERY tick, not
//     merely at the end. 184 identical runs would otherwise become 182
//     identical human tasks, which is the same defect wearing a different hat.
func TestFixedFailingRunnerSuppressesTicksToOneProbePerIntervalAndRaisesOneTask(t *testing.T) {
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)

	start := time.Now().UTC().Truncate(time.Second)
	sc := newFailingSweepSchedule(t, f, "pr-upkeep-sweep-5m", start)

	const ticks = 36 // three hours at a five-minute cadence
	var minted []time.Time
	for tick := 0; tick < ticks; tick++ {
		at := start.Add(time.Duration(tick) * scheduleTestCadence)
		res := f.tickSchedule(sc.ID, at)
		switch {
		case res.Fired:
			if len(res.Delivery.Triggered) != 1 {
				t.Fatalf("tick %d fired but triggered %d runs, want 1", tick, len(res.Delivery.Triggered))
			}
			minted = append(minted, at)
			f.failMintedRun(res.Delivery.Triggered[0].RunID, scheduleTestDetail)
		case res.Suppressed:
			// nothing minted, and the tick is on the record
		default:
			t.Fatalf("tick %d neither fired nor suppressed: %+v", tick, res)
		}
		if got := f.pendingScheduleFailingTasks(sc.ID); got > 1 {
			t.Fatalf("tick %d: %d pending schedule_failing tasks, want at most 1", tick, got)
		}
	}

	// Without suppression this is 36 mints — the #253 shape. With it, the
	// cadence mints until the second identical failure proves the environment
	// is fixed-broken, and only probes after that.
	if len(minted) != 7 {
		t.Fatalf("minted %d runs over %d ticks (%v), want 7", len(minted), ticks, minted)
	}
	// minted[0] and minted[1] are the two runs it takes to LEARN the failure
	// repeats; every mint after that is a probe and must be a whole probe
	// interval apart.
	for i := 2; i < len(minted); i++ {
		if gap := minted[i].Sub(minted[i-1]); gap < scheduleTestProbe {
			t.Errorf("mint %d came %s after the previous one, want at least %s", i, gap, scheduleTestProbe)
		}
	}

	row := f.scheduleRow(sc.ID)
	if want := int64(ticks - len(minted)); row.SuppressedCount != want {
		t.Errorf("suppressed_count = %d, want %d (every tick that did not mint)", row.SuppressedCount, want)
	}
	if row.LastFailureDetail != scheduleTestDetail {
		t.Errorf("last_failure_detail = %q, want the repeated reason %q", row.LastFailureDetail, scheduleTestDetail)
	}
	if row.FireCount != int64(len(minted)) {
		t.Errorf("fire_count = %d, want %d", row.FireCount, len(minted))
	}

	if got := f.pendingScheduleFailingTasks(sc.ID); got != 1 {
		t.Fatalf("pending schedule_failing tasks = %d, want exactly 1", got)
	}
	var (
		requestName   string
		requestReason string
		requestCount  int
	)
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT request->>'schedule_name', request->>'reason', (request->>'consecutive_failures')::int
		 FROM human_tasks WHERE kind='schedule_failing' AND request->>'schedule_id'=$1`,
		sc.ID).Scan(&requestName, &requestReason, &requestCount); err != nil {
		t.Fatalf("read schedule_failing request: %v", err)
	}
	if requestName != "pr-upkeep-sweep-5m" || requestReason != scheduleTestDetail {
		t.Errorf("schedule_failing task carries name=%q reason=%q, want %q/%q",
			requestName, requestReason, "pr-upkeep-sweep-5m", scheduleTestDetail)
	}
	if requestCount != scheduleTestAlertAfter {
		t.Errorf("schedule_failing raised at %d consecutive failures, want %d", requestCount, scheduleTestAlertAfter)
	}
}

// TestRepairedRunnerResetsSuppressionOnTheNextProbe is task t9's third clause:
// the probe is what discovers the repair, and a completed run puts the
// schedule back on its declared cadence with no operator resume.
func TestRepairedRunnerResetsSuppressionOnTheNextProbe(t *testing.T) {
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)

	start := time.Now().UTC().Truncate(time.Second)
	sc := newFailingSweepSchedule(t, f, "sweep-repaired", start)

	at := start
	tick := func() storepg.ScheduleFireResult {
		res := f.tickSchedule(sc.ID, at)
		at = at.Add(scheduleTestCadence)
		return res
	}

	// Two identical failures are what it takes to engage suppression.
	for i := 0; i < 2; i++ {
		res := tick()
		if !res.Fired {
			t.Fatalf("learning fire %d did not fire: %+v", i, res)
		}
		f.failMintedRun(res.Delivery.Triggered[0].RunID, scheduleTestDetail)
	}
	if res := tick(); !res.Suppressed {
		t.Fatalf("third tick was not suppressed: %+v", res)
	}

	// Walk forward to the probe and repair the environment on it.
	var probe storepg.ScheduleFireResult
	for i := 0; i < 12 && !probe.Fired; i++ {
		probe = tick()
	}
	if !probe.Fired {
		t.Fatalf("no probe minted a run within a probe interval of suppressed ticks")
	}
	f.completeMintedRun(probe.Delivery.Triggered[0].RunID)

	// The tick after the probe reads a COMPLETED run: suppression is off, and
	// the declared cadence resumes immediately rather than waiting out another
	// probe interval.
	if res := tick(); !res.Fired {
		t.Fatalf("tick after a completed probe did not fire: %+v", res)
	} else {
		f.failMintedRun(res.Delivery.Triggered[0].RunID, scheduleTestDetail)
	}

	if row := f.scheduleRow(sc.ID); row.ConsecutiveFailures != 0 || row.LastFailureDetail != "" {
		t.Errorf("after a completed probe: consecutive_failures=%d last_failure_detail=%q, want 0/\"\" "+
			"(a stale reason on a schedule that is working again reads as a current fault)",
			row.ConsecutiveFailures, row.LastFailureDetail)
	}

	// And the reset was a real reset, not a decrement: the failure recorded
	// just above is the FIRST of a new streak, so the very next tick still
	// fires on the declared cadence rather than being suppressed.
	if res := tick(); !res.Fired {
		t.Fatalf("first failure of a new streak suppressed the next tick: %+v", res)
	} else {
		f.failMintedRun(res.Delivery.Triggered[0].RunID, scheduleTestDetail)
	}
	row := f.scheduleRow(sc.ID)
	if row.ConsecutiveFailures != 1 {
		t.Errorf("consecutive_failures = %d after one post-repair failure, want 1", row.ConsecutiveFailures)
	}
	if row.LastFailureDetail != scheduleTestDetail {
		t.Errorf("last_failure_detail = %q, want the fresh failure %q", row.LastFailureDetail, scheduleTestDetail)
	}

	// The second identical failure of the new streak suppresses again: the
	// mechanism re-arms rather than being spent once.
	if res := tick(); !res.Suppressed {
		t.Fatalf("a second identical post-repair failure did not re-arm suppression: %+v", res)
	}
}

// TestScheduleFailingTaskIsReRaisedOnlyAfterItIsDecided pins the singleton
// half of clause 2 from both sides: a pending task suppresses re-raising, and
// a DECIDED one does not — because a decided task is a question that was
// answered, and a schedule that is still failing after the answer is a new
// question.
func TestScheduleFailingTaskIsReRaisedOnlyAfterItIsDecided(t *testing.T) {
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)

	start := time.Now().UTC().Truncate(time.Second)
	sc := newFailingSweepSchedule(t, f, "sweep-realert", start)

	at := start
	driveToAlert := func() {
		for i := 0; i < 40; i++ {
			res := f.tickSchedule(sc.ID, at)
			at = at.Add(scheduleTestCadence)
			if res.Fired {
				f.failMintedRun(res.Delivery.Triggered[0].RunID, scheduleTestDetail)
			}
			if f.pendingScheduleFailingTasks(sc.ID) == 1 {
				return
			}
		}
		t.Fatalf("no schedule_failing task was raised within 40 ticks")
	}

	driveToAlert()
	total := f.countScalar(
		`SELECT COUNT(*)::int FROM human_tasks WHERE kind='schedule_failing' AND request->>'schedule_id'=$1`, sc.ID)
	if total != 1 {
		t.Fatalf("schedule_failing tasks after the first alert = %d, want 1", total)
	}

	// Keep failing across another two probe intervals: still exactly one.
	for i := 0; i < 24; i++ {
		res := f.tickSchedule(sc.ID, at)
		at = at.Add(scheduleTestCadence)
		if res.Fired {
			f.failMintedRun(res.Delivery.Triggered[0].RunID, scheduleTestDetail)
		}
		if got := f.pendingScheduleFailingTasks(sc.ID); got != 1 {
			t.Fatalf("pending schedule_failing tasks = %d while one is unanswered, want 1", got)
		}
	}

	// A human answers it. Failures continue, so the question is asked again.
	if _, err := f.store.Pool().Exec(f.ctx,
		`UPDATE human_tasks SET status=$2, resolved_at=now()
		 WHERE kind='schedule_failing' AND status='pending' AND request->>'schedule_id'=$1`,
		sc.ID, engine.HumanTaskStatusDecided); err != nil {
		t.Fatalf("decide the schedule_failing task: %v", err)
	}

	driveToAlert()
	total = f.countScalar(
		`SELECT COUNT(*)::int FROM human_tasks WHERE kind='schedule_failing' AND request->>'schedule_id'=$1`, sc.ID)
	if total != 2 {
		t.Fatalf("schedule_failing tasks after the first was decided = %d, want 2", total)
	}
}
