package engine_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// advanceToReview drives a fresh run on approval.workflow.yaml through its
// only work-dispatching node (intake) so its token lands on `review`, the
// approval node — the point every test below is about.
func advanceToReview(t *testing.T, f *fixture) (engine.Run, engine.CompletionResult) {
	t.Helper()
	run := f.createRun(`{"subject":"ship it"}`)
	intake := f.readyNodeRun(run.ID)
	result := f.step("worker-a", intake.ID, succeeded("completed", `{"scope":"s"}`))
	if result.NextNodeID != "review" {
		t.Fatalf("intake routed to %q, want review", result.NextNodeID)
	}
	if result.NextNodeRunID == "" {
		t.Fatalf("completion result named no next node run")
	}
	return run, result
}

// AC1: dispatching an approval node writes a human_tasks row carrying
// decision schema, approver role, deadline, and context refs.
func TestApprovalNodeDispatchWritesHumanTask(t *testing.T) {
	f := newFixture(t, "approval.workflow.yaml")
	dispatchedAt := time.Now().UTC()
	run, result := advanceToReview(t, f)
	reviewNodeRunID := result.NextNodeRunID

	if result.NextHumanTaskID == "" {
		t.Fatalf("CompletionResult.NextHumanTaskID is empty for an approval node's dispatch")
	}

	// The node run is parked waiting on the task, not ready for a worker —
	// and no attempt has begun.
	nodeRun := f.nodeRun(reviewNodeRunID)
	if nodeRun.State != engine.NodeRunWaitingHuman {
		t.Fatalf("review node run state = %s, want %s", nodeRun.State, engine.NodeRunWaitingHuman)
	}

	var (
		id, namespaceID, runID, nodeRunID, kind, status string
		assignedOwnerID                                 pgtype.Text
		request                                         []byte
		responseIsNull, resolvedAtIsNull                bool
	)
	err := f.store.Pool().QueryRow(f.ctx, `
		SELECT id, namespace_id, run_id, node_run_id, kind, assigned_owner_id, status,
		       request, response IS NULL, resolved_at IS NULL
		FROM human_tasks WHERE id = $1
	`, result.NextHumanTaskID).Scan(
		&id, &namespaceID, &runID, &nodeRunID, &kind, &assignedOwnerID, &status,
		&request, &responseIsNull, &resolvedAtIsNull,
	)
	if err != nil {
		t.Fatalf("read human_tasks row %s: %v", result.NextHumanTaskID, err)
	}

	if namespaceID != f.ns.ID {
		t.Errorf("namespace_id = %s, want %s", namespaceID, f.ns.ID)
	}
	if runID != run.ID {
		t.Errorf("run_id = %s, want %s", runID, run.ID)
	}
	if nodeRunID != reviewNodeRunID {
		t.Errorf("node_run_id = %s, want %s", nodeRunID, reviewNodeRunID)
	}
	if kind != "approval" {
		t.Errorf("kind = %q, want approval", kind)
	}
	if status != engine.HumanTaskStatusPending {
		t.Errorf("status = %q, want %q", status, engine.HumanTaskStatusPending)
	}
	if assignedOwnerID.Valid {
		t.Errorf("assigned_owner_id = %q, want unset: resolving approverRef to a concrete owner is not the engine's job", assignedOwnerID.String)
	}
	if !responseIsNull {
		t.Error("response is set on a freshly created task")
	}
	if !resolvedAtIsNull {
		t.Error("resolved_at is set on a freshly created task")
	}

	var payload struct {
		DecisionSchemaRef string    `json:"decision_schema_ref"`
		ApproverRef       string    `json:"approver_ref"`
		Deadline          time.Time `json:"deadline"`
		AllowedOutcomes   []string  `json:"allowed_outcomes"`
		ContextRefs       struct {
			From string `json:"from"`
		} `json:"context_refs"`
		Audit struct {
			NodeID         string `json:"node_id"`
			TokenID        string `json:"token_id"`
			WorkflowDigest string `json:"workflow_digest"`
			FromNode       string `json:"from_node"`
			FromOutcome    string `json:"from_outcome"`
		} `json:"audit"`
	}
	if err := json.Unmarshal(request, &payload); err != nil {
		t.Fatalf("decode human_tasks.request: %v (%s)", err, request)
	}

	// Decision schema: the node's decisionSchemaRef, verbatim.
	if payload.DecisionSchemaRef != "./contracts/review-decision.schema.json" {
		t.Errorf("decision_schema_ref = %q", payload.DecisionSchemaRef)
	}
	// Requested approver role or group: the node's approverRef, verbatim —
	// not resolved against the owners table.
	if payload.ApproverRef != "group/platform-ai-approvers" {
		t.Errorf("approver_ref = %q", payload.ApproverRef)
	}
	// Deadline: the node declares 2h; the recorded deadline is dispatch time
	// plus that duration, not the bare duration string.
	wantDeadline := dispatchedAt.Add(2 * time.Hour)
	if diff := payload.Deadline.Sub(wantDeadline); diff < -time.Minute || diff > time.Minute {
		t.Errorf("deadline = %s, want ~%s (2h after dispatch)", payload.Deadline, wantDeadline)
	}
	// Allowed outcomes: the compiler's implied approval ports (PRD §9.2),
	// since this node declares no contract of its own.
	equalStrings(t, payload.AllowedOutcomes, []string{"approved", "expired", "rejected"}, "allowed_outcomes")
	// Context refs: the node's own input binding, carried as a reference —
	// exactly what PRD §9.9 calls "exact context and artifact references".
	if payload.ContextRefs.From != "/nodes/intake/output" {
		t.Errorf("context_refs.from = %q, want /nodes/intake/output", payload.ContextRefs.From)
	}
	// Audit identity beyond the row's own namespace/run/node-run columns.
	if payload.Audit.NodeID != "review" {
		t.Errorf("audit.node_id = %q, want review", payload.Audit.NodeID)
	}
	if payload.Audit.TokenID == "" {
		t.Error("audit.token_id is empty")
	}
	if payload.Audit.WorkflowDigest != f.cw.Digest {
		t.Errorf("audit.workflow_digest = %q, want %q", payload.Audit.WorkflowDigest, f.cw.Digest)
	}
	if payload.Audit.FromNode != "intake" || payload.Audit.FromOutcome != "completed" {
		t.Errorf("audit.from_node/from_outcome = %s/%s, want intake/completed", payload.Audit.FromNode, payload.Audit.FromOutcome)
	}

	// The audit trail says a human task was created, precisely for this node
	// run — not that claimable work became ready, because none did.
	var humanTaskEvents, readyEvents int
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT COUNT(*)::int FROM events WHERE aggregate_id = $1 AND event_type = $2 AND data->>'node_run_id' = $3`,
		run.ID, engine.TypeHumanTaskCreated, reviewNodeRunID,
	).Scan(&humanTaskEvents); err != nil {
		t.Fatalf("count human-task.created events: %v", err)
	}
	if humanTaskEvents != 1 {
		t.Errorf("%d human-task.created events for review's node run, want 1", humanTaskEvents)
	}
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT COUNT(*)::int FROM events WHERE aggregate_id = $1 AND event_type = $2 AND data->>'node_run_id' = $3`,
		run.ID, engine.TypeNodeRunReady, reviewNodeRunID,
	).Scan(&readyEvents); err != nil {
		t.Fatalf("count node-run.ready events: %v", err)
	}
	if readyEvents != 0 {
		t.Errorf("%d node-run.ready events for review's node run, want 0 — no work item exists to claim", readyEvents)
	}
}

// AC2: the paused run holds no work_items lease and no open transaction
// while it waits on the human task.
func TestApprovalPauseHoldsNoLeaseOrOpenTransaction(t *testing.T) {
	f := newFixture(t, "approval.workflow.yaml")
	run, result := advanceToReview(t, f)
	reviewNodeRunID := result.NextNodeRunID

	// No work_items lease: not "a leased item was released", but that no
	// work_items row was ever created for this node run at all — the park
	// model, not the code-node lease-holding model. dispatchNode calls
	// InsertHumanTask instead of EnqueueWork, so there is nothing here to
	// hold a lease on in the first place.
	workItems := f.countScalar(`SELECT COUNT(*)::int FROM work_items WHERE node_run_id = $1`, reviewNodeRunID)
	if workItems != 0 {
		t.Errorf("%d work_items rows exist for the paused node run, want 0", workItems)
	}

	// No open transaction: CompleteAttempt takes the run's advisory
	// transaction lock (ledger.RunLockKey) for the whole §12.5 completion,
	// released only when that transaction commits or rolls back. If the
	// engine were still holding a transaction open for this run while the
	// approval pause "waits", this lock would still be held and the
	// try-lock below — issued on a fresh connection, well after
	// CompleteAttempt returned — would fail to acquire it.
	var acquired bool
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, ledger.RunLockKey(run.ID),
	).Scan(&acquired); err != nil {
		t.Fatalf("try advisory lock: %v", err)
	}
	if !acquired {
		t.Fatal("the run's advisory lock is still held after CompleteAttempt returned: a transaction is still open while the run waits on the human task")
	}
	if _, err := f.store.Pool().Exec(f.ctx,
		`SELECT pg_advisory_unlock(hashtextextended($1, 0))`, ledger.RunLockKey(run.ID),
	); err != nil {
		t.Fatalf("release advisory lock: %v", err)
	}

	// The pool holds no connection open on the paused run's behalf either —
	// every connection CompleteAttempt used was returned when its
	// transaction committed. Polled briefly: pgxpool's own housekeeping can
	// transiently acquire a connection for a health check.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if acquired := f.store.Pool().Stat().AcquiredConns(); acquired == 0 {
			break
		} else if time.Now().After(deadline) {
			t.Errorf("pool has %d acquired connections after CompleteAttempt returned, want 0", acquired)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
}
