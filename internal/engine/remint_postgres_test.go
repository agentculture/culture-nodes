package engine_test

import (
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

func failRunForRemint(t *testing.T, f *fixture, runID string) string {
	t.Helper()
	var nodeRunID string
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT id FROM node_runs WHERE run_id=$1 ORDER BY created_at LIMIT 1`, runID).Scan(&nodeRunID); err != nil {
		t.Fatalf("find node run: %v", err)
	}
	if _, err := f.store.Pool().Exec(f.ctx, `UPDATE runs SET status='failed',completed_at=now() WHERE id=$1`, runID); err != nil {
		t.Fatalf("fail run: %v", err)
	}
	return nodeRunID
}

func makeRemintDue(t *testing.T, f *fixture) {
	t.Helper()
	if _, err := f.store.Pool().Exec(f.ctx, `UPDATE trigger_remints SET available_at=now()-interval '1 second' WHERE namespace_id=$1 AND status='pending'`, f.ns.ID); err != nil {
		t.Fatalf("make re-mint due: %v", err)
	}
}

// registerRemintProducer registers the identity the derived re-mint decision
// is written under — before the enqueue that writes under it, the order a
// deployment does it in. A generated id rather than the literal
// RemintSchedulerActorID because these tests share one PostgreSQL and
// actors.id is a global primary key (same reasoning as the api package's
// repair-router tests).
func registerRemintProducer(t *testing.T, f *fixture) string {
	t.Helper()
	return f.insertActorKind("engine/remint-scheduler", "validator")
}

func TestTechnicalTriggerFailureRemintsWithinBoundThenParksOnHuman(t *testing.T) {
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)
	producer := registerRemintProducer(t, f)
	first := deliverSubjectEvent(t, f, "SCRUM-194", "initial")
	originalEventID := first.Event.ID
	runID := first.Triggered[0].RunID

	for attempt := 1; attempt <= storepg.RemintMaxAttempts; attempt++ {
		nodeRunID := failRunForRemint(t, f, runID)
		if err := f.store.ScheduleRunRemint(f.ctx, f.ns.ID, runID, nodeRunID, engine.StatusFailed, "", time.Now()); err != nil {
			t.Fatalf("ScheduleRunRemint attempt %d: %v", attempt, err)
		}
		makeRemintDue(t, f)
		if got, err := f.store.EnqueueDueRemints(f.ctx, f.ns.ID, f.engine, producer, time.Now()); err != nil || got != 1 {
			t.Fatalf("EnqueueDueRemints attempt %d = (%d,%v), want (1,nil)", attempt, got, err)
		}
		if err := f.store.Pool().QueryRow(f.ctx, `SELECT minted_run_id FROM trigger_remints WHERE namespace_id=$1 AND original_event_id=$2 AND attempt=$3`, f.ns.ID, originalEventID, attempt).Scan(&runID); err != nil {
			t.Fatalf("read minted run attempt %d: %v", attempt, err)
		}
		var gotEvent string
		var gotAttempt int
		if err := f.store.Pool().QueryRow(f.ctx, `SELECT data->>'original_event_id',(data->>'attempt')::int FROM ledger_records WHERE run_id=$1 AND authority='derived'`, runID).Scan(&gotEvent, &gotAttempt); err != nil {
			t.Fatalf("read derived re-mint record: %v", err)
		}
		if gotEvent != originalEventID || gotAttempt != attempt {
			t.Errorf("derived record = event %q attempt %d, want %q/%d", gotEvent, gotAttempt, originalEventID, attempt)
		}
	}

	lastNode := failRunForRemint(t, f, runID)
	if err := f.store.ScheduleRunRemint(f.ctx, f.ns.ID, runID, lastNode, engine.StatusTimedOut, "", time.Now()); err != nil {
		t.Fatalf("ScheduleRunRemint at ceiling: %v", err)
	}
	if got := f.countScalar(`SELECT COUNT(*)::int FROM trigger_remints WHERE namespace_id=$1 AND original_event_id=$2`, f.ns.ID, originalEventID); got != storepg.RemintMaxAttempts {
		t.Fatalf("re-mint rows = %d, want ceiling %d", got, storepg.RemintMaxAttempts)
	}
	if got := f.countScalar(`SELECT COUNT(*)::int FROM human_tasks WHERE run_id=$1 AND kind='trigger_remint_exhausted' AND status='pending'`, runID); got != 1 {
		t.Fatalf("pending human tasks = %d, want 1", got)
	}
}

func TestDueRemintUsesTriggerEnqueuePathAndWaitsForActiveSubject(t *testing.T) {
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)
	producer := registerRemintProducer(t, f)
	first := deliverSubjectEvent(t, f, "SCRUM-203", "initial")
	failedRun := first.Triggered[0].RunID
	nodeRun := failRunForRemint(t, f, failedRun)
	if err := f.store.ScheduleRunRemint(f.ctx, f.ns.ID, failedRun, nodeRun, engine.StatusFailed, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	makeRemintDue(t, f)

	active := deliverSubjectEvent(t, f, "SCRUM-203", "fresh-while-backed-off").Triggered[0].RunID
	if got, err := f.store.EnqueueDueRemints(f.ctx, f.ns.ID, f.engine, producer, time.Now()); err != nil || got != 0 {
		t.Fatalf("due re-mint with active subject = (%d,%v), want deferred", got, err)
	}
	if got := f.countScalar(`SELECT COUNT(*)::int FROM trigger_remints WHERE source_run_id=$1 AND status='pending'`, failedRun); got != 1 {
		t.Fatalf("pending re-mint = %d, want 1", got)
	}

	failRunForRemint(t, f, active)
	if got, err := f.store.EnqueueDueRemints(f.ctx, f.ns.ID, f.engine, producer, time.Now()); err != nil || got != 1 {
		t.Fatalf("due re-mint after active termination = (%d,%v), want admitted", got, err)
	}
	var minted string
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT minted_run_id FROM trigger_remints WHERE source_run_id=$1`, failedRun).Scan(&minted); err != nil {
		t.Fatal(err)
	}
	// This row can only be produced by TriggerEvent -> dispatchNode ->
	// engine.Tx.EnqueueWork. EnqueueDueRemints has no SQL or helper that can
	// insert a work item itself, so the assertion pins the identical inbound
	// path rather than merely checking that some run row appeared.
	if got := f.countScalar(`SELECT COUNT(*)::int FROM work_items wi JOIN node_runs nr ON nr.id=wi.node_run_id WHERE nr.run_id=$1`, minted); got != 1 {
		t.Fatalf("minted run work items = %d, want 1 from engine Tx.EnqueueWork", got)
	}
}

func TestDomainOutcomeMatrixNeverSchedulesARemintRecord(t *testing.T) {
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)
	delivery := deliverSubjectEvent(t, f, "SCRUM-DOMAIN", "domain-answer")
	runID := delivery.Triggered[0].RunID
	var nodeRunID string
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT id FROM node_runs WHERE run_id=$1 LIMIT 1`, runID).Scan(&nodeRunID); err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"completed", "changes_required", "rejected", "needs_human"} {
		t.Run(outcome, func(t *testing.T) {
			if err := f.store.ScheduleRunRemint(f.ctx, f.ns.ID, runID, nodeRunID, engine.StatusSucceeded, outcome, time.Now()); err != nil {
				t.Fatalf("ScheduleRunRemint: %v", err)
			}
		})
	}
	if got := f.countScalar(`SELECT COUNT(*)::int FROM trigger_remints WHERE source_run_id=$1`, runID); got != 0 {
		t.Fatalf("domain outcomes produced %d re-mint rows, want 0", got)
	}
	if got := f.countScalar(`SELECT COUNT(*)::int FROM ledger_records WHERE run_id=$1 AND data->>'kind'='trigger_remint'`, runID); got != 0 {
		t.Fatalf("domain outcomes produced %d derived re-mint records, want 0", got)
	}
}
