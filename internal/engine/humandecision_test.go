package engine_test

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// decide is a small convenience wrapper so each test states only what
// differs from the common shape of a DecideHumanTask call.
func (f *fixture) decide(req engine.HumanTaskDecisionRequest) (engine.CompletionResult, error) {
	f.t.Helper()
	return f.engine.DecideHumanTask(f.ctx, req)
}

// humanTaskRow reads back status/response for direct assertion, mirroring
// humantask_test.go's own inline queries.
func (f *fixture) humanTaskRow(id string) (status string, responseIsNull bool, response []byte) {
	f.t.Helper()
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT status, response IS NULL, COALESCE(response, '{}') FROM human_tasks WHERE id = $1`, id,
	).Scan(&status, &responseIsNull, &response); err != nil {
		f.t.Fatalf("read human_tasks row %s: %v", id, err)
	}
	return status, responseIsNull, response
}

func (f *fixture) ledgerRecords(runID string) []ledger.Record {
	f.t.Helper()
	l, err := storepg.NewLedger(f.store, f.ns.ID)
	if err != nil {
		f.t.Fatalf("NewLedger: %v", err)
	}
	records, err := l.Records(f.ctx, runID)
	if err != nil {
		f.t.Fatalf("Records: %v", err)
	}
	return records
}

// TestDecideHumanTaskApprovedResumesAndCompletesRun is t7 AC2's centerpiece:
// a decision commits atomically through the review transaction and resumes
// the run — here past `review.approved -> finish`, an end node, so the run
// completes in the same call.
func TestDecideHumanTaskApprovedResumesAndCompletesRun(t *testing.T) {
	f := newFixture(t, "approval.workflow.yaml")
	run, dispatch := advanceToReview(t, f)
	taskID := dispatch.NextHumanTaskID
	reviewNodeRunID := dispatch.NextNodeRunID

	before, err := storepg.NewLedger(f.store, f.ns.ID)
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	beforeVersion, err := before.LedgerVersion(f.ctx, run.ID)
	if err != nil {
		t.Fatalf("LedgerVersion: %v", err)
	}
	if beforeVersion != 0 {
		t.Fatalf("ledger version before any decision = %d, want 0", beforeVersion)
	}

	approver := f.insertActorKind("approver", "human")
	result, err := f.decide(engine.HumanTaskDecisionRequest{
		HumanTaskID:           taskID,
		Outcome:               "approved",
		Response:              json.RawMessage(`{"comment":"ship it"}`),
		DeciderActorID:        approver,
		ExpectedLedgerVersion: beforeVersion,
	})
	if err != nil {
		t.Fatalf("DecideHumanTask: %v", err)
	}

	// The run resumed: review.approved -> finish (an end node), so the run
	// completed in this same call, and the output is intake's output,
	// exactly as finish's output binding says.
	if result.RunState != engine.RunCompleted {
		t.Fatalf("run state = %s, want %s", result.RunState, engine.RunCompleted)
	}
	if result.NodeRunID != reviewNodeRunID {
		t.Fatalf("result.NodeRunID = %s, want %s", result.NodeRunID, reviewNodeRunID)
	}
	if result.Outcome != "approved" {
		t.Fatalf("result.Outcome = %q, want approved", result.Outcome)
	}
	equalJSON(t, result.RunOutput, `{"scope":"s"}`)

	// The review node run itself completed with the decided outcome.
	reviewRun := f.nodeRun(reviewNodeRunID)
	if reviewRun.State != engine.NodeRunCompleted {
		t.Fatalf("review node run state = %s, want %s", reviewRun.State, engine.NodeRunCompleted)
	}
	if reviewRun.Outcome != "approved" {
		t.Fatalf("review node run outcome = %q, want approved", reviewRun.Outcome)
	}

	// human_tasks flipped from pending to decided, with a response recording
	// what was decided.
	status, responseIsNull, responseBytes := f.humanTaskRow(taskID)
	if status != engine.HumanTaskStatusDecided {
		t.Fatalf("human_tasks.status = %q, want %q", status, engine.HumanTaskStatusDecided)
	}
	if responseIsNull {
		t.Fatal("human_tasks.response is still null after a decision")
	}
	var response struct {
		Outcome        string `json:"outcome"`
		DeciderActorID string `json:"decider_actor_id"`
		Response       struct {
			Comment string `json:"comment"`
		} `json:"response"`
		DecidedAt string `json:"decided_at"`
	}
	if err := json.Unmarshal(responseBytes, &response); err != nil {
		t.Fatalf("decode human_tasks.response: %v (%s)", err, responseBytes)
	}
	if response.Outcome != "approved" {
		t.Errorf("response.outcome = %q, want approved", response.Outcome)
	}
	if response.DeciderActorID != approver {
		t.Errorf("response.decider_actor_id = %q, want %q", response.DeciderActorID, approver)
	}
	if response.Response.Comment != "ship it" {
		t.Errorf("response.response.comment = %q, want %q", response.Response.Comment, "ship it")
	}
	if response.DecidedAt == "" {
		t.Error("response.decided_at is empty")
	}

	// The decision landed as a human-authority review in the ledger (PRD
	// §9.9/§10.8, this task's h2): one proposed `decision` record carrying
	// the outcome, and one confirmed `review` record referencing it.
	records := f.ledgerRecords(run.ID)
	if len(records) != 2 {
		t.Fatalf("ledger has %d records after the decision, want 2 (decision + review); got %+v", len(records), records)
	}
	var decisionRecord, reviewRecord *ledger.Record
	for i := range records {
		switch records[i].RecordType {
		case ledger.RecordDecision:
			decisionRecord = &records[i]
		case ledger.RecordReview:
			reviewRecord = &records[i]
		}
	}
	if decisionRecord == nil {
		t.Fatalf("no %s record among %+v", ledger.RecordDecision, records)
	}
	if decisionRecord.Origin.Kind != ledger.OriginHuman || decisionRecord.Origin.ActorID != approver {
		t.Errorf("decision record origin = %+v, want human/%s", decisionRecord.Origin, approver)
	}
	if decisionRecord.Authority != ledger.AuthorityProposed {
		t.Errorf("decision record authority = %s, want %s", decisionRecord.Authority, ledger.AuthorityProposed)
	}
	if decisionRecord.NodeRunID.String() != reviewNodeRunID {
		t.Errorf("decision record node_run_id = %s, want %s", decisionRecord.NodeRunID, reviewNodeRunID)
	}

	if reviewRecord == nil {
		t.Fatalf("no %s record among %+v", ledger.RecordReview, records)
	}
	if reviewRecord.Origin.Kind != ledger.OriginHuman || reviewRecord.Origin.ActorID != approver {
		t.Errorf("review record origin = %+v, want human/%s", reviewRecord.Origin, approver)
	}
	if reviewRecord.Authority != ledger.AuthorityConfirmed {
		t.Errorf("review record authority = %s, want %s (a human decision is the human's own authoritative act, confirmed regardless of the outcome's sentiment)",
			reviewRecord.Authority, ledger.AuthorityConfirmed)
	}
	if reviewRecord.SubjectRef.String() != decisionRecord.ID {
		t.Errorf("review record subject_ref = %s, want the decision record %s", reviewRecord.SubjectRef, decisionRecord.ID)
	}

	// result.LedgerRecords surfaces exactly what was appended.
	if len(result.LedgerRecords) != 2 {
		t.Fatalf("result.LedgerRecords has %d entries, want 2", len(result.LedgerRecords))
	}

	// The audit trail says the task was decided, exactly once.
	var decidedEvents int
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT COUNT(*)::int FROM events WHERE aggregate_id = $1 AND event_type = $2 AND data->>'human_task_id' = $3`,
		run.ID, engine.TypeHumanTaskDecided, taskID,
	).Scan(&decidedEvents); err != nil {
		t.Fatalf("count human-task.decided events: %v", err)
	}
	if decidedEvents != 1 {
		t.Errorf("%d human-task.decided events, want 1", decidedEvents)
	}
}

// TestDecideHumanTaskRejectsOutcomeNotAllowed refuses a decision whose
// outcome is not one of the task's own allowed_outcomes, writing nothing.
func TestDecideHumanTaskRejectsOutcomeNotAllowed(t *testing.T) {
	f := newFixture(t, "approval.workflow.yaml")
	_, dispatch := advanceToReview(t, f)
	taskID := dispatch.NextHumanTaskID

	approver := f.insertActorKind("approver", "human")
	_, err := f.decide(engine.HumanTaskDecisionRequest{
		HumanTaskID:           taskID,
		Outcome:               "not-a-real-outcome",
		DeciderActorID:        approver,
		ExpectedLedgerVersion: 0,
	})
	var outcomeErr *engine.OutcomeNotAllowedError
	if !errors.As(err, &outcomeErr) {
		t.Fatalf("DecideHumanTask error = %v, want *OutcomeNotAllowedError", err)
	}
	if !errors.Is(err, engine.ErrOutcomeNotAllowed) {
		t.Errorf("errors.Is(err, ErrOutcomeNotAllowed) = false")
	}

	status, responseIsNull, _ := f.humanTaskRow(taskID)
	if status != engine.HumanTaskStatusPending {
		t.Errorf("human_tasks.status = %q after a refused decision, want %q (nothing written)", status, engine.HumanTaskStatusPending)
	}
	if !responseIsNull {
		t.Error("human_tasks.response is set after a refused decision")
	}
	if records := f.ledgerRecords(dispatch.RunID); len(records) != 0 {
		t.Errorf("%d ledger records after a refused decision, want 0", len(records))
	}
}

// TestDecideHumanTaskRefusesTheEngineOnlyOutcome pins issue #265's other
// half: `expired` is in an approval task's allowed_outcomes (the compiler
// implies it for every approval node) but a PERSON may not select it. It is
// what the control plane records when it reads a fact — a merged PR, a passed
// deadline — so a decider choosing it would hand-produce an engine
// observation, which is the ledger authority model inverted (PRD §10.4).
//
// The engine's own expiry path is not affected and is proved separately by
// TestMergedPRFactExpiresThePendingApprovalAndCompletesTheRun: it reaches the
// same outcome through ExpireHumanTask, which carries an expiry reason.
func TestDecideHumanTaskRefusesTheEngineOnlyOutcome(t *testing.T) {
	f := newFixture(t, "approval.workflow.yaml")
	_, dispatch := advanceToReview(t, f)
	taskID := dispatch.NextHumanTaskID

	// The task really does declare it — otherwise this test would pass for
	// the wrong reason, refusing an outcome that was never on the row.
	var request string
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT request::text FROM human_tasks WHERE id = $1`, taskID).Scan(&request); err != nil {
		t.Fatalf("read human_tasks.request %s: %v", taskID, err)
	}
	if !strings.Contains(request, engine.OutcomeExpired) {
		t.Fatalf("human task request does not declare %q, so this test proves nothing: %s",
			engine.OutcomeExpired, request)
	}

	approver := f.insertActorKind("approver", "human")
	_, err := f.decide(engine.HumanTaskDecisionRequest{
		HumanTaskID:           taskID,
		Outcome:               engine.OutcomeExpired,
		DeciderActorID:        approver,
		ExpectedLedgerVersion: 0,
	})
	if !errors.Is(err, engine.ErrOutcomeNotAllowed) {
		t.Fatalf("DecideHumanTask(outcome=%q) error = %v, want ErrOutcomeNotAllowed",
			engine.OutcomeExpired, err)
	}
	var outcomeErr *engine.OutcomeNotAllowedError
	if errors.As(err, &outcomeErr) {
		for _, allowed := range outcomeErr.Allowed {
			if allowed == engine.OutcomeExpired {
				t.Errorf("the refusal still lists %q as allowed: %v", engine.OutcomeExpired, outcomeErr.Allowed)
			}
		}
	}

	status, responseIsNull, _ := f.humanTaskRow(taskID)
	if status != engine.HumanTaskStatusPending {
		t.Errorf("human_tasks.status = %q after a refused decision, want %q", status, engine.HumanTaskStatusPending)
	}
	if !responseIsNull {
		t.Error("human_tasks.response is set after a refused decision")
	}
	if records := f.ledgerRecords(dispatch.RunID); len(records) != 0 {
		t.Errorf("%d ledger records after a refused decision, want 0", len(records))
	}
}

// TestDecideHumanTaskRejectsStaleLedgerVersion proves AC2's other half: a
// decision naming a ledger version the run has since moved past is refused,
// atomically, with nothing written — the same guarantee CommitReview gives
// an ordinary review commit (PRD §10.8).
func TestDecideHumanTaskRejectsStaleLedgerVersion(t *testing.T) {
	f := newFixture(t, "approval.workflow.yaml")
	run, dispatch := advanceToReview(t, f)
	taskID := dispatch.NextHumanTaskID

	// Move the ledger forward past version 0 before the decision arrives.
	agent := f.insertActor("agent")
	l, err := storepg.NewLedger(f.store, f.ns.ID)
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	if _, err := l.Append(f.ctx, ledger.Record{
		RecordType: ledger.RecordAnnouncement,
		RunID:      run.ID,
		Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: agent},
		Authority:  ledger.AuthorityProposed,
		Data:       json.RawMessage(`{"statement":"an unrelated proposal"}`),
	}); err != nil {
		t.Fatalf("append unrelated record: %v", err)
	}

	approver := f.insertActorKind("approver", "human")
	_, err = f.decide(engine.HumanTaskDecisionRequest{
		HumanTaskID:           taskID,
		Outcome:               "approved",
		DeciderActorID:        approver,
		ExpectedLedgerVersion: 0, // stale: the ledger is now at version 1
	})
	var staleErr *ledger.StaleReviewError
	if !errors.As(err, &staleErr) {
		t.Fatalf("DecideHumanTask error = %v, want *ledger.StaleReviewError", err)
	}

	status, _, _ := f.humanTaskRow(taskID)
	if status != engine.HumanTaskStatusPending {
		t.Errorf("human_tasks.status = %q after a stale decision, want %q (nothing written)", status, engine.HumanTaskStatusPending)
	}
	// Only the one unrelated record from before the decision attempt — the
	// decision's own record was rolled back with everything else.
	if records := f.ledgerRecords(run.ID); len(records) != 1 {
		t.Errorf("%d ledger records after a stale decision, want 1 (only the pre-existing unrelated record)", len(records))
	}
}

// TestDecideHumanTaskRefusesASecondDecision is AC2's "resumes exactly once":
// a decision on an already-decided task is refused with the
// AlreadyDecided error rather than resuming the run again.
func TestDecideHumanTaskRefusesASecondDecision(t *testing.T) {
	f := newFixture(t, "approval.workflow.yaml")
	_, dispatch := advanceToReview(t, f)
	taskID := dispatch.NextHumanTaskID

	approver := f.insertActorKind("approver", "human")
	first, err := f.decide(engine.HumanTaskDecisionRequest{
		HumanTaskID:           taskID,
		Outcome:               "approved",
		DeciderActorID:        approver,
		ExpectedLedgerVersion: 0,
	})
	if err != nil {
		t.Fatalf("first DecideHumanTask: %v", err)
	}
	if first.RunState != engine.RunCompleted {
		t.Fatalf("first decision: run state = %s, want %s", first.RunState, engine.RunCompleted)
	}

	_, err = f.decide(engine.HumanTaskDecisionRequest{
		HumanTaskID:           taskID,
		Outcome:               "rejected",
		DeciderActorID:        approver,
		ExpectedLedgerVersion: 2, // the version after the first decision's two records
	})
	var alreadyErr *engine.HumanTaskAlreadyDecidedError
	if !errors.As(err, &alreadyErr) {
		t.Fatalf("second DecideHumanTask error = %v, want *HumanTaskAlreadyDecidedError", err)
	}
	if !errors.Is(err, engine.ErrHumanTaskAlreadyDecided) {
		t.Errorf("errors.Is(err, ErrHumanTaskAlreadyDecided) = false")
	}

	// The second, refused decision did not touch the ledger again.
	if records := f.ledgerRecords(dispatch.RunID); len(records) != 2 {
		t.Errorf("%d ledger records after a refused second decision, want 2 (only the first decision's)", len(records))
	}
}

// TestDecideHumanTaskConcurrentDecisionsResolveExactlyOnce fires two
// decisions at the same pending task concurrently: exactly one must win and
// resume the run, the other must be refused as already decided — proving
// the "decides exactly once" guarantee holds under a genuine race, not only
// when the caller happens to decide serially.
func TestDecideHumanTaskConcurrentDecisionsResolveExactlyOnce(t *testing.T) {
	f := newFixture(t, "approval.workflow.yaml")
	_, dispatch := advanceToReview(t, f)
	taskID := dispatch.NextHumanTaskID

	approver := f.insertActorKind("approver", "human")
	outcomes := []string{"approved", "rejected"}

	var wg sync.WaitGroup
	results := make([]error, len(outcomes))
	completions := make([]engine.CompletionResult, len(outcomes))
	for i, outcome := range outcomes {
		wg.Add(1)
		go func(i int, outcome string) {
			defer wg.Done()
			result, err := f.engine.DecideHumanTask(f.ctx, engine.HumanTaskDecisionRequest{
				HumanTaskID:           taskID,
				Outcome:               outcome,
				DeciderActorID:        approver,
				ExpectedLedgerVersion: 0,
			})
			results[i] = err
			completions[i] = result
		}(i, outcome)
	}
	wg.Wait()

	var succeeded, refused int
	for i, err := range results {
		switch {
		case err == nil:
			succeeded++
			if completions[i].RunState != engine.RunCompleted {
				t.Errorf("winning decision: run state = %s, want %s", completions[i].RunState, engine.RunCompleted)
			}
		case errors.Is(err, engine.ErrHumanTaskAlreadyDecided):
			refused++
		default:
			t.Fatalf("decision %d: unexpected error %v", i, err)
		}
	}
	if succeeded != 1 || refused != 1 {
		t.Fatalf("succeeded=%d refused=%d, want exactly one of each (results=%v)", succeeded, refused, results)
	}

	if records := f.ledgerRecords(dispatch.RunID); len(records) != 2 {
		t.Errorf("%d ledger records after the race, want 2 (exactly one decision's worth)", len(records))
	}
}

// TestDecideHumanTaskUnknownTaskIsNotFound refuses a decision against a task
// id that does not exist.
func TestDecideHumanTaskUnknownTaskIsNotFound(t *testing.T) {
	f := newFixture(t, "approval.workflow.yaml")
	approver := f.insertActorKind("approver", "human")

	_, err := f.decide(engine.HumanTaskDecisionRequest{
		HumanTaskID:           "human_task_does_not_exist",
		Outcome:               "approved",
		DeciderActorID:        approver,
		ExpectedLedgerVersion: 0,
	})
	if !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("DecideHumanTask error = %v, want errors.Is(_, engine.ErrNotFound)", err)
	}
}
