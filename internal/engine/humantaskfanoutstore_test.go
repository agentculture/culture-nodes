package engine_test

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Postgres-backed acceptance for task t11 (spec c6). These drive the real
// creation path — CreateRun -> dispatchNode -> InsertHumanTask — so what they
// assert is what a deployment actually queues, not what a planner would
// return if someone called it.

type fanOutRow struct {
	Channel  string
	ActorKey string
	TaskID   string
	RunID    string
	Payload  string
}

func (f *fixture) fanOutRows(taskID string) []fanOutRow {
	f.t.Helper()
	rows, err := f.store.Pool().Query(f.ctx,
		`SELECT channel, target_actor_key, human_task_id, run_id, payload::text
		 FROM human_task_fanout_outbox WHERE human_task_id = $1 ORDER BY channel`, taskID)
	if err != nil {
		f.t.Fatalf("read fan-out outbox: %v", err)
	}
	defer rows.Close()
	var out []fanOutRow
	for rows.Next() {
		var row fanOutRow
		if err := rows.Scan(&row.Channel, &row.ActorKey, &row.TaskID, &row.RunID, &row.Payload); err != nil {
			f.t.Fatalf("scan fan-out row: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("read fan-out outbox: %v", err)
	}
	return out
}

func channelNames(rows []fanOutRow) []string {
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Channel)
	}
	sort.Strings(names)
	return names
}

// AC: one task on a Jira-keyed run -> exactly one Jira comment record, one
// transition, one notify record.
func TestJiraKeyedRunQueuesOneCommentOneTransitionAndOneNotify(t *testing.T) {
	t.Setenv(engine.UIBaseURLEnv, "http://thor:18080")
	f := newFixture(t, "approval-subject.workflow.yaml")

	run := f.createRun(`{"source":"jira","id":"SCRUM-6"}`)
	taskID := f.pendingHumanTaskID(run.ID)

	rows := f.fanOutRows(taskID)
	want := []string{
		engine.FanOutChannelJiraComment,
		engine.FanOutChannelJiraTransition,
		engine.FanOutChannelNotify,
	}
	if got := channelNames(rows); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("queued channels = %v, want %v", got, want)
	}
	for _, row := range rows {
		if row.RunID != run.ID {
			t.Errorf("%s row names run %s, want %s", row.Channel, row.RunID, run.ID)
		}
		wantActor := storepg.JiraTicketReporterActorKey
		if row.Channel == engine.FanOutChannelNotify {
			wantActor = storepg.HumanTaskNotifierActorKey
		}
		if row.ActorKey != wantActor {
			t.Errorf("%s row targets %s, want %s", row.Channel, row.ActorKey, wantActor)
		}
		// The comment and the notify post carry the page link; the
		// transition carries a status name and nothing else, because that is
		// the entire input the bridge's transition verb accepts.
		if row.Channel != engine.FanOutChannelJiraTransition &&
			!strings.Contains(row.Payload, "http://thor:18080/tickets/SCRUM-6") {
			t.Errorf("%s payload carries no absolute page link: %s", row.Channel, row.Payload)
		}
	}
}

// AC: the same task twice emits nothing more. The second enqueue is run
// through the same Tx method InsertHumanTask itself calls, so this asserts
// the property at the write, not at a caller's guard.
func TestTheSameHumanTaskFansOutOnlyOnce(t *testing.T) {
	f := newFixture(t, "approval-subject.workflow.yaml")
	run := f.createRun(`{"source":"jira","id":"SCRUM-6"}`)
	taskID := f.pendingHumanTaskID(run.ID)

	before := len(f.fanOutRows(taskID))
	if before != 3 {
		t.Fatalf("first fan-out queued %d rows, want 3", before)
	}

	es, err := storepg.NewEngineStore(f.store, f.ns.ID)
	if err != nil {
		t.Fatalf("NewEngineStore: %v", err)
	}
	var queued int
	if err := es.InTx(f.ctx, func(ctx context.Context, tx engine.Tx) error {
		queued, err = tx.EnqueueHumanTaskFanOut(ctx, taskID)
		return err
	}); err != nil {
		t.Fatalf("second EnqueueHumanTaskFanOut: %v", err)
	}
	if queued != 0 {
		t.Errorf("second fan-out inserted %d rows, want 0", queued)
	}
	if after := len(f.fanOutRows(taskID)); after != before {
		t.Errorf("fan-out rows after re-enqueue = %d, want %d", after, before)
	}
}

// AC: a PR-sourced run queues one notify record and no Jira ones. The absence
// of a GitHub PR comment is the documented one — see
// engine.NoGitHubPRCommentReason and migration 0051's header.
func TestPRSourcedRunQueuesOnlyTheNotifyPost(t *testing.T) {
	f := newFixture(t, "approval-subject.workflow.yaml")
	run := f.createRun(`{"source":"github_pr","repository":"agentculture/culture-nodes","number":236}`)
	taskID := f.pendingHumanTaskID(run.ID)

	rows := f.fanOutRows(taskID)
	if got := channelNames(rows); strings.Join(got, ",") != engine.FanOutChannelNotify {
		t.Fatalf("queued channels = %v, want only %s", got, engine.FanOutChannelNotify)
	}
	if !strings.Contains(rows[0].Payload, "agentculture/culture-nodes#236") {
		t.Errorf("notify payload does not name the pull request: %s", rows[0].Payload)
	}
}

// AC: a pr.merged fact expires the matching pending task with reason
// pr_merged, and its run routes the approval node's `expired` edge to finish.
func TestMergedPRFactExpiresThePendingApprovalAndCompletesTheRun(t *testing.T) {
	f := newFixture(t, "approval-subject.workflow.yaml")
	run := f.createRun(`{"source":"github_pr","repository":"agentculture/culture-nodes","number":236}`)
	taskID := f.pendingHumanTaskID(run.ID)

	fact, _ := json.Marshal(map[string]any{
		"source": "github_pr", "repository": "agentculture/culture-nodes", "number": 236,
		"url":       "https://github.com/agentculture/culture-nodes/pull/236",
		"merged_at": "2026-08-29T10:00:00Z", "issue_key": "SCRUM-6",
	})
	if _, err := f.store.DeliverSignalEvent(f.ctx, storepg.DeliverSignalEventInput{
		NamespaceID: f.ns.ID, Name: "pr.merged", Payload: fact, Emitter: "pr-upkeep-sweep",
		SourceKey: "github:agentculture/culture-nodes:pr:236:merged",
		Watermark: json.RawMessage(`{"merged_at":"2026-08-29T10:00:00Z"}`),
		Subject:   "SCRUM-6",
	}); err != nil {
		t.Fatalf("deliver pr.merged: %v", err)
	}

	selected, err := f.engine.PendingHumanTasksWithMergedPR(f.ctx, 10)
	if err != nil {
		t.Fatalf("PendingHumanTasksWithMergedPR: %v", err)
	}
	if len(selected) != 1 || selected[0] != taskID {
		t.Fatalf("selected %v, want exactly [%s]", selected, taskID)
	}

	expired, err := f.engine.ExpirePendingTasksForMergedPR(f.ctx, 10, registerExpiryProducer(t, f))
	if err != nil {
		t.Fatalf("ExpirePendingTasksForMergedPR: %v", err)
	}
	if len(expired) != 1 {
		t.Fatalf("expired %d tasks, want 1", len(expired))
	}
	if expired[0].Err != nil {
		t.Fatalf("expiring %s: %v", expired[0].HumanTaskID, expired[0].Err)
	}
	if expired[0].Outcome != engine.OutcomeExpired {
		t.Errorf("outcome = %q, want %q", expired[0].Outcome, engine.OutcomeExpired)
	}

	var status, reason, response string
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT status, COALESCE(expiry_reason,''), response::text FROM human_tasks WHERE id = $1`, taskID).
		Scan(&status, &reason, &response); err != nil {
		t.Fatalf("read human task: %v", err)
	}
	if status != engine.HumanTaskStatusExpired {
		t.Errorf("status = %q, want %q", status, engine.HumanTaskStatusExpired)
	}
	if reason != engine.HumanTaskExpiryReasonPRMerged {
		t.Errorf("expiry_reason = %q, want %q", reason, engine.HumanTaskExpiryReasonPRMerged)
	}
	if !strings.Contains(response, engine.HumanTaskExpiryReasonPRMerged) {
		t.Errorf("response %s does not carry the reason", response)
	}

	// The run went down review.expired to the end node, so it is completed —
	// an expiry is a domain outcome, never an engine failure.
	after := f.run(run.ID)
	if after.State != engine.RunCompleted {
		t.Fatalf("run state = %s, want %s", after.State, engine.RunCompleted)
	}

	// A second sweep finds nothing: the task is no longer pending.
	again, err := f.engine.PendingHumanTasksWithMergedPR(f.ctx, 10)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second scan selected %v, want none", again)
	}
}

// t15: merge freezes immediately but Done remains a human authority boundary.
func TestMergedPRRaisesOneTicketDoneTaskAndDoneDecisionQueuesTransition(t *testing.T) {
	f := newFixture(t, "approval-subject.workflow.yaml")
	f.createRun(`{"source":"jira","id":"SCRUM-15"}`)
	if _, err := f.store.Pool().Exec(f.ctx, `DELETE FROM human_task_fanout_outbox WHERE namespace_id=$1`, f.ns.ID); err != nil {
		t.Fatalf("clear fixture fan-out: %v", err)
	}
	fact := json.RawMessage(`{"source":"github_pr","repository":"agentculture/culture-nodes","number":415,"url":"https://github.com/agentculture/culture-nodes/pull/415","merged_at":"2026-09-02T10:00:00Z","issue_key":"SCRUM-15"}`)
	in := storepg.DeliverSignalEventInput{
		NamespaceID: f.ns.ID, Name: "pr.merged", Payload: fact, Emitter: "pr-upkeep-sweep",
		SourceKey: "github:agentculture/culture-nodes:pr:415:merged",
		Watermark: json.RawMessage(`{"merged_at":"2026-09-02T10:00:00Z"}`), Subject: "SCRUM-15",
	}
	if _, err := f.store.DeliverSignalEvent(f.ctx, in); err != nil {
		t.Fatalf("deliver pr.merged: %v", err)
	}
	if _, err := f.store.DeliverSignalEvent(f.ctx, in); err != nil {
		t.Fatalf("redeliver pr.merged: %v", err)
	}

	var taskID, request string
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT id, request::text FROM human_tasks
		WHERE namespace_id=$1 AND kind='ticket_done'`, f.ns.ID).Scan(&taskID, &request); err != nil {
		t.Fatalf("read Ticket done task: %v", err)
	}
	for _, want := range []string{"Ticket done?", "done", "not_yet", "approver", "validate-delivery", "SCRUM-15"} {
		if !strings.Contains(request, want) {
			t.Errorf("task request %s does not contain %q", request, want)
		}
	}
	var taskCount, transitionCount int
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT count(*) FROM human_tasks
		WHERE namespace_id=$1 AND kind='ticket_done'`, f.ns.ID).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT count(*) FROM human_task_fanout_outbox
		WHERE namespace_id=$1 AND payload->>'verb'='transition_issue'`, f.ns.ID).Scan(&transitionCount); err != nil {
		t.Fatal(err)
	}
	if taskCount != 1 || transitionCount != 0 {
		t.Fatalf("after merge (tasks=%d, transitions=%d), want (1, 0)", taskCount, transitionCount)
	}

	if _, err := f.engine.DecideHumanTask(f.ctx, engine.HumanTaskDecisionRequest{
		HumanTaskID: taskID, Outcome: "done", DeciderActorID: f.insertActorKind("done-approver", "human"),
	}); err != nil {
		t.Fatalf("DecideHumanTask(done): %v", err)
	}
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT count(*) FROM human_task_fanout_outbox
		WHERE namespace_id=$1 AND payload->>'verb'='transition_issue' AND payload->>'target'='Done'`,
		f.ns.ID).Scan(&transitionCount); err != nil {
		t.Fatal(err)
	}
	if transitionCount != 1 {
		t.Fatalf("Done transition intents = %d, want 1", transitionCount)
	}
}

// An expiry is a derived record with an engine origin, never a confirmed
// human decision: nobody chose this, and the ledger must not read as if
// someone did.
func TestExpiryRecordsDerivedAuthorityAndNoDecider(t *testing.T) {
	f := newFixture(t, "approval-subject.workflow.yaml")
	run := f.createRun(`{"source":"github_pr","repository":"o/r","number":7}`)
	taskID := f.pendingHumanTaskID(run.ID)

	producer := registerExpiryProducer(t, f)
	if _, err := f.engine.ExpireHumanTask(f.ctx, engine.ExpireHumanTaskRequest{
		HumanTaskID: taskID, Reason: engine.HumanTaskExpiryReasonPRMerged,
		Detail: "o/r#7 is merged", ProducerActorID: producer,
	}); err != nil {
		t.Fatalf("ExpireHumanTask: %v", err)
	}

	var kind, authority, actorID, data string
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT origin_kind, authority, COALESCE(origin_actor_id,''), data::text
		 FROM ledger_records WHERE run_id = $1 AND record_type = 'decision' ORDER BY created_at DESC LIMIT 1`,
		run.ID).Scan(&kind, &authority, &actorID, &data); err != nil {
		t.Fatalf("read ledger record: %v", err)
	}
	if kind != "engine" || authority != "derived" {
		t.Errorf("expiry record origin/authority = %s/%s, want engine/derived", kind, authority)
	}
	if actorID != producer {
		t.Errorf("expiry record origin actor = %q, want the registered producer %q", actorID, producer)
	}
	// No review was created: an expiry is derived, never confirmed. A
	// confirmed record here would mean the engine had promoted its own
	// inference to a human's authority.
	var reviews int
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT count(*) FROM ledger_records WHERE run_id = $1 AND authority = 'confirmed'`, run.ID).
		Scan(&reviews); err != nil {
		t.Fatalf("count confirmed records: %v", err)
	}
	if reviews != 0 {
		t.Errorf("expiry left %d confirmed ledger record(s); an expiry confirms nothing", reviews)
	}
	if !strings.Contains(data, engine.HumanTaskExpiryReasonPRMerged) {
		t.Errorf("expiry record data %s does not carry the reason", data)
	}
}

// pendingHumanTaskID reads back the single pending human task of a run.
func (f *fixture) pendingHumanTaskID(runID string) string {
	f.t.Helper()
	var id string
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT id FROM human_tasks WHERE run_id = $1 AND status = $2`,
		runID, engine.HumanTaskStatusPending).Scan(&id); err != nil {
		f.t.Fatalf("read pending human task of run %s: %v", runID, err)
	}
	return id
}

// registerExpiryProducer registers the identity the derived expiry record is
// written under, before the expiry that writes under it — the order a
// deployment does it in (see engine.HumanTaskExpiryActorID). A generated id
// rather than that literal because these tests share one PostgreSQL and
// actors.id is a global primary key, the same reasoning
// registerRemintProducer gives next door.
func registerExpiryProducer(t *testing.T, f *fixture) string {
	t.Helper()
	return f.insertActorKind("engine/human-task-expiry", "validator")
}
