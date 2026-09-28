package engine_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Task t45: a notice (a human task that informs rather than asks) can be
// acknowledged — including a legacy row written with no allowed_outcomes —
// and acknowledging it moves nothing.

// failedTriggerRun delivers one subject event and fails the run it mints,
// returning the run and the node run a re-mint notice would name.
func failedTriggerRun(t *testing.T, f *fixture, subject string) (runID, nodeRunID string) {
	t.Helper()
	runID = deliverSubjectEvent(t, f, subject, subject+"-initial").Triggered[0].RunID
	nodeRunID = failRunForRemint(t, f, runID)
	return runID, nodeRunID
}

// insertLegacyNotice writes a trigger_remint_exhausted task exactly as
// remint.go did before t45: its request carries no allowed_outcomes.
func insertLegacyNotice(t *testing.T, f *fixture, runID, nodeRunID string) string {
	t.Helper()
	id := store.NewULID()
	request := `{"reason":"trigger re-mint attempts exhausted","original_event_id":"01EVT","attempts":2,"window_seconds":86400,"subject":"SCRUM-LEGACY"}`
	if _, err := f.store.Pool().Exec(f.ctx, `INSERT INTO human_tasks
		(id,namespace_id,run_id,node_run_id,kind,status,request,created_at)
		VALUES ($1,$2,$3,$4,'trigger_remint_exhausted','pending',$5,now())`,
		id, f.ns.ID, runID, nodeRunID, request); err != nil {
		t.Fatalf("insert legacy notice: %v", err)
	}
	return id
}

// noticeRunSnapshot is everything an acknowledgement must leave exactly as it was.
type noticeRunSnapshot struct {
	RunState   string
	NodeRuns   string
	Tokens     int
	WorkItems  int
	Remints    int
	OpenTokens int
}

func snapshotNoticeRun(t *testing.T, f *fixture, runID string) noticeRunSnapshot {
	t.Helper()
	var s noticeRunSnapshot
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT status FROM runs WHERE id=$1`, runID).Scan(&s.RunState); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT COALESCE(string_agg(id||':'||status||':'||COALESCE(outcome,''), ',' ORDER BY id),'')
		FROM node_runs WHERE run_id=$1`, runID).Scan(&s.NodeRuns); err != nil {
		t.Fatalf("read node runs: %v", err)
	}
	s.Tokens = f.countScalar(`SELECT COUNT(*)::int FROM tokens WHERE run_id=$1`, runID)
	s.OpenTokens = f.countScalar(`SELECT COUNT(*)::int FROM tokens WHERE run_id=$1 AND state='active'`, runID)
	s.WorkItems = f.countScalar(`SELECT COUNT(*)::int FROM work_items wi JOIN node_runs nr ON nr.id=wi.node_run_id WHERE nr.run_id=$1`, runID)
	s.Remints = f.countScalar(`SELECT COUNT(*)::int FROM trigger_remints WHERE namespace_id=$1`, f.ns.ID)
	return s
}

func ledgerVersionOf(t *testing.T, f *fixture, runID string) int64 {
	t.Helper()
	l, err := storepg.NewLedger(f.store, f.ns.ID)
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	v, err := l.LedgerVersion(f.ctx, runID)
	if err != nil {
		t.Fatalf("LedgerVersion: %v", err)
	}
	return v
}

func TestLegacyNoticeWithNoOutcomesCanBeAcknowledgedAndLeavesTheRunUntouched(t *testing.T) {
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)
	runID, nodeRunID := failedTriggerRun(t, f, "SCRUM-T45")
	taskID := insertLegacyNotice(t, f, runID, nodeRunID)

	task, err := storepgEngineTask(t, f, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if got := engine.HumanTaskAllowedOutcomes(task); !reflect.DeepEqual(got, []string{engine.OutcomeAcknowledged}) {
		t.Fatalf("legacy notice allowed outcomes = %v, want [acknowledged]", got)
	}

	before := snapshotNoticeRun(t, f, runID)
	decider := f.insertActorKind("owner", "human")
	result, err := f.decide(engine.HumanTaskDecisionRequest{
		HumanTaskID:           taskID,
		Outcome:               engine.OutcomeAcknowledged,
		DeciderActorID:        decider,
		ExpectedLedgerVersion: ledgerVersionOf(t, f, runID),
	})
	if err != nil {
		t.Fatalf("acknowledge legacy notice: %v", err)
	}
	if result.Outcome != engine.OutcomeAcknowledged || result.RunState != engine.RunFailed {
		t.Errorf("result = outcome %q run %q, want acknowledged on a still-failed run", result.Outcome, result.RunState)
	}
	if result.NextNodeRunID != "" || result.NextHumanTaskID != "" || result.NodeRunState != "" {
		t.Errorf("acknowledgement routed something: %+v", result)
	}

	status, _, response := f.humanTaskRow(taskID)
	if status != engine.HumanTaskStatusDecided {
		t.Fatalf("task status = %q, want decided", status)
	}
	var resp struct {
		Outcome        string `json:"outcome"`
		DeciderActorID string `json:"decider_actor_id"`
	}
	if err := json.Unmarshal(response, &resp); err != nil || resp.Outcome != engine.OutcomeAcknowledged || resp.DeciderActorID != decider {
		t.Errorf("response = %s (%v), want outcome acknowledged by %s", response, err, decider)
	}

	// The same audit trail a decision leaves: a human-origin decision record
	// confirmed by a review naming the decider.
	var decisions, reviews int
	for _, rec := range f.ledgerRecords(runID) {
		switch {
		case rec.RecordType == ledger.RecordDecision && rec.Origin.ActorID == decider:
			decisions++
		case rec.RecordType == ledger.RecordReview && rec.Authority == ledger.AuthorityConfirmed:
			reviews++
		}
	}
	if decisions != 1 || reviews != 1 {
		t.Errorf("ledger holds %d decision / %d confirmed review records, want 1/1", decisions, reviews)
	}
	if got := f.countScalar(`SELECT COUNT(*)::int FROM events WHERE aggregate_id=$1 AND event_type=$2`, runID, engine.TypeHumanTaskDecided); got != 1 {
		t.Errorf("human-task.decided events = %d, want 1", got)
	}

	if after := snapshotNoticeRun(t, f, runID); after != before {
		t.Errorf("acknowledgement moved the run:\n before %+v\n after  %+v", before, after)
	}

	// Once is enough: a second acknowledgement is refused like any decision.
	_, err = f.decide(engine.HumanTaskDecisionRequest{
		HumanTaskID: taskID, Outcome: engine.OutcomeAcknowledged, DeciderActorID: decider,
		ExpectedLedgerVersion: ledgerVersionOf(t, f, runID),
	})
	if !errors.Is(err, engine.ErrHumanTaskAlreadyDecided) {
		t.Errorf("second acknowledgement error = %v, want ErrHumanTaskAlreadyDecided", err)
	}
}

func TestNoticeAcknowledgementRefusesANonHumanDecider(t *testing.T) {
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)
	runID, nodeRunID := failedTriggerRun(t, f, "SCRUM-T45-AGENT")
	taskID := insertLegacyNotice(t, f, runID, nodeRunID)

	agent := f.insertActorKind("codex", "agent")
	_, err := f.decide(engine.HumanTaskDecisionRequest{
		HumanTaskID: taskID, Outcome: engine.OutcomeAcknowledged, DeciderActorID: agent,
		ExpectedLedgerVersion: ledgerVersionOf(t, f, runID),
	})
	var authErr *ledger.AuthorityError
	if !errors.As(err, &authErr) || authErr.Rule != ledger.RuleReviewerNotHuman {
		t.Fatalf("agent acknowledgement error = %v, want rule %s", err, ledger.RuleReviewerNotHuman)
	}
	if status, _, _ := f.humanTaskRow(taskID); status != engine.HumanTaskStatusPending {
		t.Errorf("refused acknowledgement left status %q, want pending", status)
	}
	if got := len(f.ledgerRecords(runID)); got != 0 {
		t.Errorf("refused acknowledgement wrote %d ledger records, want 0", got)
	}
}

func TestNoticeRefusesAnyOutcomeButAcknowledged(t *testing.T) {
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)
	runID, nodeRunID := failedTriggerRun(t, f, "SCRUM-T45-OTHER")
	taskID := insertLegacyNotice(t, f, runID, nodeRunID)
	_, err := f.decide(engine.HumanTaskDecisionRequest{
		HumanTaskID: taskID, Outcome: "approved", DeciderActorID: f.insertActorKind("owner", "human"),
		ExpectedLedgerVersion: ledgerVersionOf(t, f, runID),
	})
	if !errors.Is(err, engine.ErrOutcomeNotAllowed) {
		t.Fatalf("notice decided as approved: error = %v, want ErrOutcomeNotAllowed", err)
	}
}

func TestApprovalTaskStillRefusesAcknowledged(t *testing.T) {
	f := newFixture(t, "approval.workflow.yaml")
	_, dispatch := advanceToReview(t, f)
	_, err := f.decide(engine.HumanTaskDecisionRequest{
		HumanTaskID:           dispatch.NextHumanTaskID,
		Outcome:               engine.OutcomeAcknowledged,
		DeciderActorID:        f.insertActorKind("approver", "human"),
		ExpectedLedgerVersion: 0,
	})
	if !errors.Is(err, engine.ErrOutcomeNotAllowed) {
		t.Fatalf("approval decided as acknowledged: error = %v, want ErrOutcomeNotAllowed", err)
	}
	if status, _, _ := f.humanTaskRow(dispatch.NextHumanTaskID); status != engine.HumanTaskStatusPending {
		t.Errorf("refused approval left status %q, want pending", status)
	}
}

func TestNewRemintNoticeDeclaresAcknowledged(t *testing.T) {
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)
	producer := registerRemintProducer(t, f)
	runID := deliverSubjectEvent(t, f, "SCRUM-T45-NEW", "initial").Triggered[0].RunID
	for attempt := 1; attempt <= storepg.RemintMaxAttempts; attempt++ {
		nodeRunID := failRunForRemint(t, f, runID)
		if err := f.store.ScheduleRunRemint(f.ctx, f.ns.ID, runID, nodeRunID, engine.StatusFailed, "", time.Now()); err != nil {
			t.Fatal(err)
		}
		makeRemintDue(t, f)
		if _, err := f.store.EnqueueDueRemints(f.ctx, f.ns.ID, f.engine, producer, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := f.store.Pool().QueryRow(f.ctx, `SELECT minted_run_id FROM trigger_remints WHERE namespace_id=$1 AND attempt=$2`, f.ns.ID, attempt).Scan(&runID); err != nil {
			t.Fatal(err)
		}
	}
	lastNode := failRunForRemint(t, f, runID)
	if err := f.store.ScheduleRunRemint(f.ctx, f.ns.ID, runID, lastNode, engine.StatusFailed, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT request->'allowed_outcomes' FROM human_tasks WHERE run_id=$1 AND kind=$2`,
		runID, engine.HumanTaskKindTriggerRemintExhausted).Scan(&raw); err != nil {
		t.Fatalf("read notice: %v", err)
	}
	var got []string
	if err := json.Unmarshal(raw, &got); err != nil || !reflect.DeepEqual(got, []string{engine.OutcomeAcknowledged}) {
		t.Fatalf("new notice allowed_outcomes = %s (%v), want [acknowledged]", raw, err)
	}
}

func storepgEngineTask(t *testing.T, f *fixture, id string) (engine.HumanTask, error) {
	t.Helper()
	es, err := storepg.NewEngineStore(f.store, f.ns.ID)
	if err != nil {
		return engine.HumanTask{}, err
	}
	return es.GetHumanTask(f.ctx, id)
}

// The rule behind every surface: a notice with no stored outcomes reads as
// [acknowledged]; a stored set is kept verbatim; a non-notice with none
// stays empty.
func TestHumanTaskAllowedOutcomesReadsLegacyNoticesAsAcknowledged(t *testing.T) {
	cases := []struct {
		kind, request string
		want          []string
	}{
		{engine.HumanTaskKindScheduleFailing, `{"reason":"x"}`, []string{engine.OutcomeAcknowledged}},
		{engine.HumanTaskKindTriggerRemintExhausted, ``, []string{engine.OutcomeAcknowledged}},
		{engine.HumanTaskKindTriggerRemintExhausted, `{"allowed_outcomes":["acknowledged"]}`, []string{engine.OutcomeAcknowledged}},
		{"approval", `{"allowed_outcomes":["approved","rejected"]}`, []string{"approved", "rejected"}},
		{"approval", `{}`, nil},
		{"ticket_done", `{"allowed_outcomes":["done","not_yet"]}`, []string{"done", "not_yet"}},
	}
	for _, c := range cases {
		got := engine.HumanTaskAllowedOutcomes(engine.HumanTask{Kind: c.kind, Request: json.RawMessage(c.request)})
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %s: allowed = %v, want %v", c.kind, c.request, got, c.want)
		}
	}
}
