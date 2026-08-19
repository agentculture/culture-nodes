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

func TestTechnicalTriggerFailureRemintsWithinBoundThenParksOnHuman(t *testing.T) {
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)
	first := deliverSubjectEvent(t, f, "SCRUM-194", "initial")
	originalEventID := first.Event.ID
	runID := first.Triggered[0].RunID

	for attempt := 1; attempt <= storepg.RemintMaxAttempts; attempt++ {
		nodeRunID := failRunForRemint(t, f, runID)
		if err := f.store.ScheduleRunRemint(f.ctx, f.ns.ID, runID, nodeRunID, engine.StatusFailed, "", time.Now()); err != nil {
			t.Fatalf("ScheduleRunRemint attempt %d: %v", attempt, err)
		}
		makeRemintDue(t, f)
		if got, err := f.store.EnqueueDueRemints(f.ctx, f.ns.ID, f.engine, time.Now()); err != nil || got != 1 {
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
