package e2etest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/runners/headspace"
	idstore "github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// The human-review branch of the reference workflow (PRD §11.1's
// `verify.blocked → human-review`), driven end to end exactly like every
// other test in this package: the real API, the real engine, a real worker,
// a real scheduler, a real runner boundary, and a real PostgreSQL. The only
// thing standing in for somebody else's process is the agents' judgement —
// and, here, the person: a decision arriving as an authenticated POST to
// /v1alpha1/human-tasks/{id}/decision is exactly what a human clicking
// "approve" in the web front produces.
//
// What these tests prove that the engine-level (internal/engine) and
// API-level (internal/api) human-task tests cannot:
//
//   - the pause is real in a live deployment — a worker and a scheduler are
//     running the whole time the run sits on the approval node, and neither
//     can claim it, because no work_items row exists for it and no
//     transaction is held open on the run;
//   - the decision travels the real wire — bearer-authenticated HTTP against
//     the deployed server, with the ledger version the decider actually read
//     back from the API;
//   - the *selected* outcome routes the edge: `approved` resumes build and
//     the run finishes with the verifier's later passing verdict,
//     `rejected` ends the run at `finish` — two different runs, two
//     different edges, from the same immutable definition.

// humanTaskOut, decideRequest, and decisionResult are the human-task wire
// shapes as api/openapi/openapi.yaml documents them
// (components.schemas.HumanTask, DecideHumanTaskRequest,
// HumanTaskDecisionResult). They are declared here rather than imported from
// internal/api for the same reason runView is (harness_test.go): this package
// asserts against the documented wire contract, not against whatever struct
// that package happens to serialise.
type humanTaskOut struct {
	ID         string          `json:"id"`
	RunID      string          `json:"run_id"`
	NodeRunID  string          `json:"node_run_id"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	Request    json.RawMessage `json:"request"`
	Response   json.RawMessage `json:"response"`
	CreatedAt  time.Time       `json:"created_at"`
	ResolvedAt *time.Time      `json:"resolved_at"`
}

type humanTaskListOut struct {
	Items []humanTaskOut `json:"items"`
}

type decideRequest struct {
	Outcome               string          `json:"outcome"`
	DeciderActorID        string          `json:"decider_actor_id"`
	Response              json.RawMessage `json:"response,omitempty"`
	ExpectedLedgerVersion int64           `json:"expected_ledger_version"`
}

type decisionResult struct {
	HumanTaskID   string          `json:"human_task_id"`
	RunID         string          `json:"run_id"`
	NodeRunID     string          `json:"node_run_id"`
	Outcome       string          `json:"outcome"`
	LedgerRecords []ledger.Record `json:"ledger_records"`
	NextNodeID    string          `json:"next_node_id"`
	NextNodeRunID string          `json:"next_node_run_id"`
	RunState      string          `json:"run_state"`
	RunOutput     json.RawMessage `json:"run_output"`
}

// humanTaskRequestPayload is human_tasks.request — everything PRD §9.9 asks
// an approval node's task to carry.
type humanTaskRequestPayload struct {
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

// -----------------------------------------------------------------------
// The tests
// -----------------------------------------------------------------------

// TestHumanReviewBranchParksThenResumesBuildOnAnApprovedDecision walks the
// branch the reference workflow omitted while the human-task surface was
// deferred: intake → plan → build → test → verify(blocked) → human-review
// → [pause] → decision(approved) → build → test → verify(passed) → finish.
func TestHumanReviewBranchParksThenResumesBuildOnAnApprovedDecision(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ns := pgtest.MustNamespace(t, s, "e2e-human-review")

	runner := &scriptedRunner{}
	agents, runnerID := setupDeliveryAgentsAndActors(t, s, ns.ID)
	// The verifier reports `blocked` on sight: this test is about the
	// human-review branch, and looping through changes_required first would
	// only blur which edge the assertions are counting.
	agents.verifyRequestsChanges = false
	agents.verifyBlocksFirst = true
	decider := registerHumanDecider(t, s, ns.ID)

	stack := startStack(t, stackConfig{
		namespaceID:   ns.ID,
		agentsURL:     agents.server.URL,
		runner:        runner,
		runnerName:    headspace.RunnerName,
		runnerActorID: runnerID,
	})
	// Registered as a cleanup rather than deferred, and registered BEFORE the
	// diagnostic dump below: cleanups run last-in-first-out, so the dump gets
	// to read the database while the pool is still open.
	t.Cleanup(stack.stop)

	digest := stack.publishWorkflow(t)
	runID := stack.createRun(t, digest, json.RawMessage(`{"request":"add a /healthz endpoint","repository":"example/service"}`))
	t.Cleanup(func() {
		if t.Failed() {
			dumpRunState(t, stack, runID)
			t.Logf("agent script failures: %v", agents.scriptFailures())
		}
	})

	sseDone := make(chan sseResult, 1)
	go func() {
		events, err := streamRunEvents(t, stack.server.URL, runID, 90*time.Second)
		sseDone <- sseResult{events: events, err: err}
	}()

	// ---- the run parks on the approval node ----
	task := stack.awaitPendingHumanTask(t, runID, 60*time.Second)
	assertParkedBeforeDecision(t, stack, runID, task, digest, agents)

	// ---- the decision travels the real wire ----
	body := decideRequest{
		Outcome:               "approved",
		DeciderActorID:        decider,
		Response:              json.RawMessage(`{"note":"the blocker is a known flake; proceed with the fix"}`),
		ExpectedLedgerVersion: stack.ledgerVersion(t, runID),
	}
	result := submitApprovedDecision(t, stack, task, body)
	assertApprovedDecisionResult(t, result, task.NodeRunID)

	// ---- the run resumes and finishes ----
	view := stack.waitForTerminal(t, runID, 90*time.Second)
	assertRunCompletedWithOutput(t, stack, view, agents, []byte(`"passed"`),
		"the verifier's later passing verdict")

	// build really was re-entered by the decision, and the second pass saw
	// what the verifier had said (the agents' own script refuses otherwise).
	assertBuildResumedAfterApproval(t, stack, view, agents, task, decider)
	assertApprovedRunEvents(t, sseDone, view, task.ID)

	assertHumanAuthorityInLedger(t, stack, ns.ID, runID, task.ID, decider, "approved")
}

// assertParkedBeforeDecision checks the run genuinely parked on the approval
// node before any decision arrived: the task's request shape and audit trail
// are correct, the pause holds no lease, and the verifier spoke once while
// build has NOT been re-entered — the run is waiting on the person, not
// racing ahead of them.
func assertParkedBeforeDecision(t *testing.T, s *stack, runID string, task humanTaskOut, digest string, agents *deliveryAgents) {
	t.Helper()
	assertHumanTaskRequest(t, task, digest)
	assertPauseHoldsNoLease(t, s, runID, task.NodeRunID)

	if got := agents.callCount("verify"); got != 1 {
		t.Fatalf("verify was invoked %d times before the decision, want 1", got)
	}
	if got := agents.callCount("build"); got != 1 {
		t.Fatalf("build was invoked %d times before the decision, want 1: the run must wait for the decision", got)
	}
}

// submitApprovedDecision exercises PRD spec decision c45's auth carve-out —
// the endpoint that writes human authority into a ledger is the one place
// this API is not open, so an unauthenticated decision must be refused and
// change nothing — before submitting the same body with the real bearer
// token, and returns the authenticated response for the caller to assert on.
func submitApprovedDecision(t *testing.T, s *stack, task humanTaskOut, body decideRequest) decisionResult {
	t.Helper()
	if status := s.decide(t, task.ID, "", body, nil); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated decision returned %d, want 401", status)
	}
	if refused := s.humanTask(t, task.ID); refused.Status != "pending" {
		t.Fatalf("task status = %q after a refused decision, want pending", refused.Status)
	}

	var result decisionResult
	if status := s.decide(t, task.ID, decisionAuthSecret, body, &result); status != http.StatusOK {
		t.Fatalf("authenticated decision returned %d, want 200", status)
	}
	return result
}

// assertApprovedDecisionResult checks the approved decision's own response:
// the selected outcome routed the edge, in the very same transaction that
// recorded the decision — `human-review.approved` goes back to build.
func assertApprovedDecisionResult(t *testing.T, result decisionResult, wantNodeRunID string) {
	t.Helper()
	if result.Outcome != "approved" {
		t.Errorf("decision outcome = %q, want approved", result.Outcome)
	}
	if result.NodeRunID != wantNodeRunID {
		t.Errorf("decision resolved node run %q, want the parked %q", result.NodeRunID, wantNodeRunID)
	}
	if result.NextNodeID != "build" {
		t.Fatalf("the approved decision routed to %q, want build (edge human-review.approved)", result.NextNodeID)
	}
	if result.RunState != string(engine.RunRunning) {
		t.Errorf("run state after the decision = %q, want running", result.RunState)
	}
}

// assertRunCompletedWithOutput checks the run finished cleanly and its
// output carries wantSubstr, describing what that substring proves via
// wantDesc when it does not.
func assertRunCompletedWithOutput(t *testing.T, s *stack, view runView, agents *deliveryAgents, wantSubstr []byte, wantDesc string) {
	t.Helper()
	if failures := agents.scriptFailures(); len(failures) > 0 {
		t.Fatalf("the scripted agents refused an invocation: %v", failures)
	}
	if view.Run.State != string(engine.RunCompleted) {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", view.Run.State, s.errors())
	}
	if !bytes.Contains(view.Run.Output, wantSubstr) {
		t.Errorf("run output = %s, want %s", view.Run.Output, wantDesc)
	}
}

// assertBuildResumedAfterApproval checks the resumed branch: build and
// verify each ran a second time, the approval node's own node run reads
// completed/approved, the task now reads decided, and — the invariant the
// whole park model rests on — no work_items row was ever created for the
// approval node, not before the decision and not after it.
func assertBuildResumedAfterApproval(t *testing.T, s *stack, view runView, agents *deliveryAgents, task humanTaskOut, decider string) {
	t.Helper()
	if got := agents.callCount("build"); got != 2 {
		t.Errorf("build was invoked %d times, want 2 (the decision resumed it)", got)
	}
	if got := agents.callCount("verify"); got != 2 {
		t.Errorf("verify was invoked %d times, want 2", got)
	}

	assertApprovalNodeRun(t, view, "approved")
	assertDecidedHumanTask(t, s, task.ID, "approved", decider)
	// Not one work item, ever: not before the decision, not after it.
	if got := countWorkItems(t, s, task.NodeRunID); got != 0 {
		t.Errorf("%d work_items rows exist for the approval node run after the decision, want 0", got)
	}
}

// assertApprovedRunEvents drains the run's SSE stream and checks it tells
// the approved branch's story: the edges each walked once, the human-task
// lifecycle events present, and — the whole branch being domain routing —
// nothing that looks like a technical failure.
func assertApprovedRunEvents(t *testing.T, sseDone <-chan sseResult, view runView, taskID string) {
	t.Helper()
	events, err := drainSSE(t, sseDone)
	if err != nil {
		t.Fatalf("SSE stream: %v", err)
	}
	assertHumanReviewEdges(t, events, "human-review.approved")
	assertHumanTaskEvents(t, events, taskID, "approved")
	assertNoTechnicalFailures(t, view)
}

// TestHumanReviewRejectedDecisionRoutesItsOwnEdge is the other half of "the
// selected outcome routes the edge": the identical workflow, the identical
// pause, a different answer — `rejected` ends the run at `finish` rather
// than resuming build.
func TestHumanReviewRejectedDecisionRoutesItsOwnEdge(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ns := pgtest.MustNamespace(t, s, "e2e-human-review-rejected")

	runner := &scriptedRunner{}
	agents, runnerID := setupDeliveryAgentsAndActors(t, s, ns.ID)
	agents.verifyRequestsChanges = false
	agents.verifyBlocksFirst = true
	decider := registerHumanDecider(t, s, ns.ID)

	stack := startStack(t, stackConfig{
		namespaceID:   ns.ID,
		agentsURL:     agents.server.URL,
		runner:        runner,
		runnerName:    headspace.RunnerName,
		runnerActorID: runnerID,
	})
	t.Cleanup(stack.stop)

	digest := stack.publishWorkflow(t)
	runID := stack.createRun(t, digest, json.RawMessage(`{"request":"add a /healthz endpoint"}`))
	t.Cleanup(func() {
		if t.Failed() {
			dumpRunState(t, stack, runID)
			t.Logf("agent script failures: %v", agents.scriptFailures())
		}
	})

	sseDone := make(chan sseResult, 1)
	go func() {
		events, err := streamRunEvents(t, stack.server.URL, runID, 90*time.Second)
		sseDone <- sseResult{events: events, err: err}
	}()

	task := stack.awaitPendingHumanTask(t, runID, 60*time.Second)
	assertPauseHoldsNoLease(t, stack, runID, task.NodeRunID)

	var result decisionResult
	status := stack.decide(t, task.ID, decisionAuthSecret, decideRequest{
		Outcome:               "rejected",
		DeciderActorID:        decider,
		Response:              json.RawMessage(`{"note":"the blocker is real; stop here"}`),
		ExpectedLedgerVersion: stack.ledgerVersion(t, runID),
	}, &result)
	if status != http.StatusOK {
		t.Fatalf("decision returned %d, want 200", status)
	}
	if result.Outcome != "rejected" {
		t.Errorf("decision outcome = %q, want rejected", result.Outcome)
	}
	// The selected outcome routed its own edge: `human-review.rejected` goes
	// to the end node, so the very transaction that recorded the decision also
	// finished the run.
	if result.NextNodeID != "finish" {
		t.Fatalf("the rejected decision routed to %q, want finish (edge human-review.rejected)", result.NextNodeID)
	}
	if result.RunState != string(engine.RunCompleted) {
		t.Fatalf("run state after the rejection = %q, want completed", result.RunState)
	}
	if !bytes.Contains(result.RunOutput, []byte(`"blocked"`)) {
		t.Errorf("the decision's run_output = %s, want the verifier's blocked verdict", result.RunOutput)
	}

	view := stack.waitForTerminal(t, runID, 60*time.Second)
	if failures := agents.scriptFailures(); len(failures) > 0 {
		t.Fatalf("the scripted agents refused an invocation: %v", failures)
	}
	if view.Run.State != string(engine.RunCompleted) {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", view.Run.State, stack.errors())
	}
	// `finish` binds /nodes/verify/output, and the last thing the verifier
	// said on this run was that it was blocked. A rejected review does not
	// invent a passing verdict.
	if !bytes.Contains(view.Run.Output, []byte(`"blocked"`)) {
		t.Errorf("run output = %s, want the verifier's blocked verdict", view.Run.Output)
	}
	// build was never re-entered: the decision routed to the end node.
	if got := agents.callCount("build"); got != 1 {
		t.Errorf("build was invoked %d times, want 1: a rejected review does not resume the loop", got)
	}
	if got := agents.callCount("verify"); got != 1 {
		t.Errorf("verify was invoked %d times, want 1", got)
	}

	assertApprovalNodeRun(t, view, "rejected")
	assertDecidedHumanTask(t, stack, task.ID, "rejected", decider)

	events, err := drainSSE(t, sseDone)
	if err != nil {
		t.Fatalf("SSE stream: %v", err)
	}
	assertHumanReviewEdges(t, events, "human-review.rejected")
	assertHumanTaskEvents(t, events, task.ID, "rejected")
	assertNoTechnicalFailures(t, view)

	assertHumanAuthorityInLedger(t, stack, ns.ID, runID, task.ID, decider, "rejected")
}

// -----------------------------------------------------------------------
// Assertions
// -----------------------------------------------------------------------

// assertHumanTaskRequest checks the parked task carries every field PRD §9.9
// asks an approval node's human task to contain, and that its audit trail
// names the edge that produced it.
func assertHumanTaskRequest(t *testing.T, task humanTaskOut, digest string) {
	t.Helper()
	assertPendingTaskShape(t, task)

	var payload humanTaskRequestPayload
	if err := json.Unmarshal(task.Request, &payload); err != nil {
		t.Fatalf("decode human task request: %v (%s)", err, task.Request)
	}
	assertDecisionPolicy(t, payload)
	assertRequestContextAndAudit(t, payload, digest)
}

// assertPendingTaskShape checks the task record itself, before its request
// payload is even decoded: an approval kind, pending status, a node run to
// join back to the paused run, and no response/resolved_at yet.
func assertPendingTaskShape(t *testing.T, task humanTaskOut) {
	t.Helper()
	if task.Kind != "approval" {
		t.Errorf("human task kind = %q, want approval", task.Kind)
	}
	if task.Status != "pending" {
		t.Errorf("human task status = %q, want pending", task.Status)
	}
	if task.NodeRunID == "" {
		t.Fatal("the human task names no node run; without it nothing joins the task back to the paused run")
	}
	if task.ResolvedAt != nil || len(task.Response) != 0 {
		t.Errorf("a pending task already carries a response/resolved_at: %+v", task)
	}
}

// assertDecisionPolicy checks the decision schema, approver, deadline, and
// allowed outcomes PRD §9.9 asks an approval node's task to declare.
func assertDecisionPolicy(t *testing.T, payload humanTaskRequestPayload) {
	t.Helper()
	if payload.DecisionSchemaRef != "./contracts/review-decision.schema.json" {
		t.Errorf("decision_schema_ref = %q", payload.DecisionSchemaRef)
	}
	if payload.ApproverRef != "group/platform-ai-approvers" {
		t.Errorf("approver_ref = %q", payload.ApproverRef)
	}
	// The reference workflow declares a 24h deadline; the task records the
	// absolute moment it becomes overdue, not the duration string.
	if until := time.Until(payload.Deadline); until < 23*time.Hour || until > 25*time.Hour {
		t.Errorf("deadline = %s (%s from now), want ~24h out", payload.Deadline, until)
	}
	wantOutcomes := map[string]bool{"approved": true, "expired": true, "rejected": true}
	if len(payload.AllowedOutcomes) != len(wantOutcomes) {
		t.Errorf("allowed_outcomes = %v, want approved/expired/rejected", payload.AllowedOutcomes)
	}
	for _, outcome := range payload.AllowedOutcomes {
		if !wantOutcomes[outcome] {
			t.Errorf("allowed_outcomes contains %q, which the approval node's ports do not include", outcome)
		}
	}
}

// assertRequestContextAndAudit checks the task points the approver at the
// verifier's exact output, and that the audit trail names the edge that
// produced the task.
func assertRequestContextAndAudit(t *testing.T, payload humanTaskRequestPayload, digest string) {
	t.Helper()
	// "Exact context and artifact references": the approver is pointed at the
	// verifier's own blocked output, as a reference, not handed a payload the
	// engine assembled.
	if payload.ContextRefs.From != "/nodes/verify/output" {
		t.Errorf("context_refs.from = %q, want /nodes/verify/output", payload.ContextRefs.From)
	}
	if payload.Audit.NodeID != "human-review" {
		t.Errorf("audit.node_id = %q, want human-review", payload.Audit.NodeID)
	}
	if payload.Audit.TokenID == "" {
		t.Error("audit.token_id is empty")
	}
	if payload.Audit.WorkflowDigest != digest {
		t.Errorf("audit.workflow_digest = %q, want the published %q", payload.Audit.WorkflowDigest, digest)
	}
	if payload.Audit.FromNode != "verify" || payload.Audit.FromOutcome != "blocked" {
		t.Errorf("audit.from_node/from_outcome = %s/%s, want verify/blocked",
			payload.Audit.FromNode, payload.Audit.FromOutcome)
	}
}

// assertPauseHoldsNoLease is this test's centrepiece and PRD §9.9's actual
// sentence: "the workflow pauses without holding a worker or database
// transaction". It is the same proof internal/engine's
// TestApprovalPauseHoldsNoLeaseOrOpenTransaction makes, re-made here against
// a LIVE deployment — a worker polling every 25ms and a scheduler ticking
// every 200ms are running throughout, and the assertion is re-checked after a
// settle window so "nothing claimed it" is a statement about a real,
// competing claim loop rather than about a quiet instant.
func assertPauseHoldsNoLease(t *testing.T, s *stack, runID, nodeRunID string) {
	t.Helper()

	// 1. The park model in one line: one human_tasks row, zero work_items
	// rows. Not "a lease was taken and released" — there is nothing to lease,
	// because dispatching an approval node writes a task instead of enqueuing
	// work (internal/engine/humantask.go).
	if got := countRows(t, s, `SELECT COUNT(*)::int FROM human_tasks WHERE node_run_id = $1`, nodeRunID); got != 1 {
		t.Fatalf("%d human_tasks rows exist for the parked approval node run, want exactly 1", got)
	}
	if got := countWorkItems(t, s, nodeRunID); got != 0 {
		t.Fatalf("%d work_items rows exist for the parked approval node run, want 0", got)
	}

	// 2. No transaction is held open on the run: the advisory lock every
	// completion (and every decision) takes for the length of its transaction
	// is free, on a connection that has nothing to do with the run.
	assertRunLockFree(t, s, runID)

	// 3. The pause is stable across a real poll window: several worker poll
	// intervals and a scheduler tick later, nothing has appeared to claim and
	// the node run is untouched.
	time.Sleep(500 * time.Millisecond)
	if got := countWorkItems(t, s, nodeRunID); got != 0 {
		t.Fatalf("%d work_items rows appeared for the parked node run during the settle window, want 0", got)
	}
	if got := nodeRunStatus(t, s, nodeRunID); got != string(engine.NodeRunWaitingHuman) {
		t.Fatalf("parked node run status = %q, want %s — a running worker must not touch it",
			got, engine.NodeRunWaitingHuman)
	}
	if got := runState(t, s.db, runID); got != engine.RunRunning {
		t.Fatalf("run state during the pause = %s, want running: waiting on a person is not a failure", got)
	}
	assertRunLockFree(t, s, runID)
}

// assertRunLockFree acquires and releases the run's advisory transaction lock
// on a fresh connection. Acquiring it proves no engine transaction is open on
// this run (see internal/engine's humantask_test.go for the same check at
// engine level).
func assertRunLockFree(t *testing.T, s *stack, runID string) {
	t.Helper()
	ctx := context.Background()
	var acquired bool
	if err := s.db.Pool().QueryRow(ctx,
		`SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, ledger.RunLockKey(runID),
	).Scan(&acquired); err != nil {
		t.Fatalf("try advisory lock: %v", err)
	}
	if !acquired {
		t.Fatal("the run's advisory lock is held while the run waits on the human task: " +
			"a transaction is open across the pause (PRD §9.9 forbids exactly this)")
	}
	if _, err := s.db.Pool().Exec(ctx,
		`SELECT pg_advisory_unlock(hashtextextended($1, 0))`, ledger.RunLockKey(runID),
	); err != nil {
		t.Fatalf("release advisory lock: %v", err)
	}
}

// assertApprovalNodeRun checks the approval node's own node run: completed,
// carrying the decided outcome, and — the shape that separates a human task
// from every other node kind — carrying no attempt at all. There was no
// dispatch, so there is nothing to have attempted.
func assertApprovalNodeRun(t *testing.T, view runView, outcome string) {
	t.Helper()
	runs := nodeRunsFor(view, "human-review")
	if len(runs) != 1 {
		t.Fatalf("human-review ran %d times, want exactly 1", len(runs))
	}
	nr := runs[0]
	if nr.State != string(engine.NodeRunCompleted) {
		t.Errorf("the approval node run is %s, want completed", nr.State)
	}
	if nr.Outcome != outcome {
		t.Errorf("the approval node run's outcome = %q, want %q", nr.Outcome, outcome)
	}
	if len(nr.Attempts) != 0 {
		t.Errorf("the approval node run carries %d attempts, want 0: a human decision is not a dispatch", len(nr.Attempts))
	}
}

// assertDecidedHumanTask re-reads the task through the API and checks it now
// reads as decided, with the decider and outcome recorded on it.
func assertDecidedHumanTask(t *testing.T, s *stack, taskID, outcome, decider string) {
	t.Helper()
	task := s.humanTask(t, taskID)
	if task.Status != "decided" {
		t.Errorf("human task status = %q after the decision, want decided", task.Status)
	}
	if task.ResolvedAt == nil {
		t.Error("human task resolved_at is unset after the decision")
	}
	var response struct {
		Outcome        string `json:"outcome"`
		DeciderActorID string `json:"decider_actor_id"`
	}
	if err := json.Unmarshal(task.Response, &response); err != nil {
		t.Fatalf("decode human task response: %v (%s)", err, task.Response)
	}
	if response.Outcome != outcome {
		t.Errorf("response.outcome = %q, want %q", response.Outcome, outcome)
	}
	if response.DeciderActorID != decider {
		t.Errorf("response.decider_actor_id = %q, want the registered decider %q", response.DeciderActorID, decider)
	}

	// The pending list no longer offers it: a decided task cannot be handed to
	// a second approver.
	var pending humanTaskListOut
	if status := s.getJSON("/v1alpha1/human-tasks?status=pending", &pending); status != http.StatusOK {
		t.Fatalf("GET pending human tasks: status %d", status)
	}
	for _, item := range pending.Items {
		if item.ID == taskID {
			t.Errorf("task %s is still listed as pending after being decided", taskID)
		}
	}
}

// assertHumanReviewEdges checks the branch's edges were each walked exactly
// once, and that the loop edges this branch replaces were not walked at all.
func assertHumanReviewEdges(t *testing.T, events []sseEvent, decisionEdge string) {
	t.Helper()
	if got := countEdgeTransitions(events, "verify.blocked"); got != 1 {
		t.Errorf("the verify.blocked edge was walked %d times, want exactly 1", got)
	}
	if got := countEdgeTransitions(events, decisionEdge); got != 1 {
		t.Errorf("the %s edge was walked %d times, want exactly 1", decisionEdge, got)
	}
	if got := countEdgeTransitions(events, "verify.changes_required"); got != 0 {
		t.Errorf("the verify.changes_required edge was walked %d times, want 0 on this run", got)
	}
}

// assertHumanTaskEvents checks the event log tells the branch's story: the
// task was created, and it was decided by someone, for this node run. A
// node-run.ready event for the approval node would mean claimable work
// existed, and none ever did.
func assertHumanTaskEvents(t *testing.T, events []sseEvent, taskID, outcome string) {
	t.Helper()

	var created, decided int
	for _, ev := range events {
		var envelope humanTaskEventEnvelope
		if err := json.Unmarshal(ev.Data, &envelope); err != nil {
			continue
		}
		c, d := countHumanTaskEvent(t, ev, envelope, taskID, outcome)
		created += c
		decided += d
	}
	if created != 1 {
		t.Errorf("%d human-task.created events for task %s, want 1", created, taskID)
	}
	if decided != 1 {
		t.Errorf("%d human-task.decided events for task %s, want 1", decided, taskID)
	}
}

// humanTaskEventEnvelope is the subset of an SSE event's data field
// assertHumanTaskEvents reads: enough to tell which human task an event
// names, and — for a decided event — what outcome it carries.
type humanTaskEventEnvelope struct {
	Data struct {
		HumanTaskID string `json:"human_task_id"`
		NodeID      string `json:"node_id"`
		Outcome     string `json:"outcome"`
	} `json:"data"`
}

// countHumanTaskEvent classifies one already-decoded event against taskID,
// returning (1,0) for a matching created event, (0,1) for a matching decided
// event (after checking its outcome), and (0,0) for anything else. A
// node-run.ready event naming the approval node fails the test outright: no
// claimable work ever existed for it.
func countHumanTaskEvent(t *testing.T, ev sseEvent, envelope humanTaskEventEnvelope, taskID, outcome string) (created, decided int) {
	t.Helper()
	switch ev.Type {
	case engine.TypeHumanTaskCreated:
		if envelope.Data.HumanTaskID == taskID {
			return 1, 0
		}
	case engine.TypeHumanTaskDecided:
		if envelope.Data.HumanTaskID == taskID {
			if envelope.Data.Outcome != outcome {
				t.Errorf("human-task.decided event outcome = %q, want %q", envelope.Data.Outcome, outcome)
			}
			return 0, 1
		}
	case engine.TypeNodeRunReady:
		if envelope.Data.NodeID == "human-review" {
			t.Error("a node-run.ready event names the approval node; no claimable work ever existed for it")
		}
	}
	return 0, 0
}

// assertHumanAuthorityInLedger is the ledger half of the acceptance criterion:
// the decision landed as a HUMAN-authority review, not as an agent's claim.
//
// PRD §10.4 makes `confirmed` reachable only through a review transaction, and
// internal/ledger makes a review record the only thing that may carry it — so
// what a correct decision leaves behind is a pair: the decider's own
// `decision` record, appended `proposed` like anything a producer asserts, and
// a `review` record confirming it, carrying human origin and confirmed
// authority. Both name the same decider. Nothing else in the run gained
// authority: every agent-origin record is still `proposed`.
func assertHumanAuthorityInLedger(t *testing.T, s *stack, namespaceID, runID, taskID, decider, outcome string) {
	t.Helper()

	led := ledgerFor(t, s.db, namespaceID)
	records, err := led.Records(context.Background(), runID)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}

	decision := findDecisionRecord(t, records, taskID, decider, outcome)
	assertConfirmingReview(t, records, decision, decider)
	// The human confirmed their own decision — and nothing else. An agent
	// saying "done" is still only a claim after a person approved the branch.
	assertNothingAgentSaidGainedAuthority(t, records)

	// The decision is visible where a reader looks for decisions.
	history, err := led.ProjectRun(context.Background(), runID, ledger.KindDecisionHistory, "")
	if err != nil {
		t.Fatalf("project decision_history: %v", err)
	}
	found := false
	for _, rec := range history.Items {
		if rec.ID == decision.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("the decision_history projection (%d items) omits the human decision %s", len(history.Items), decision.ID)
	}
}

// assertNothingAgentSaidGainedAuthority checks the human decision confirmed
// the decision and nothing else: every agent-origin record in the run is
// still `proposed`. It is slice_test.go's assertAgentRecordsStayProposed
// without that test's claim-count expectation, which is a statement about a
// two-build-pass run rather than about authority.
func assertNothingAgentSaidGainedAuthority(t *testing.T, records []ledger.Record) {
	t.Helper()
	agentRecords := 0
	for _, rec := range records {
		if rec.Origin.Kind != ledger.OriginAgent {
			continue
		}
		agentRecords++
		if rec.Authority != ledger.AuthorityProposed {
			t.Errorf("agent-origin record %s (%s) has authority %q after the human decision; "+
				"approving a branch is not confirming what an agent claimed (PRD §10.4)",
				rec.ID, rec.RecordType, rec.Authority)
		}
	}
	if agentRecords == 0 {
		t.Fatal("no agent-origin records were appended; the ledger contract was never exercised")
	}
}

func findDecisionRecord(t *testing.T, records []ledger.Record, taskID, decider, outcome string) ledger.Record {
	t.Helper()
	var found []ledger.Record
	for _, rec := range filterRecords(records, ledger.RecordDecision) {
		data, err := rec.DataMap()
		if err != nil {
			t.Fatalf("decode decision payload: %v", err)
		}
		if data["human_task_id"] != taskID {
			continue
		}
		found = append(found, rec)
		if got, _ := data["outcome"].(string); got != outcome {
			t.Errorf("decision record outcome = %q, want %q", got, outcome)
		}
		if rec.Origin.Kind != ledger.OriginHuman {
			t.Errorf("decision record origin kind = %q, want human", rec.Origin.Kind)
		}
		if rec.Origin.ActorID != decider {
			t.Errorf("decision record actor = %q, want the decider %q", rec.Origin.ActorID, decider)
		}
		if rec.Authority != ledger.AuthorityProposed {
			t.Errorf("decision record authority = %q, want proposed: the confirming review is a separate record", rec.Authority)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d decision records name human task %s, want exactly 1", len(found), taskID)
	}
	return found[0]
}

func assertConfirmingReview(t *testing.T, records []ledger.Record, decision ledger.Record, decider string) {
	t.Helper()

	// A run holds two quite different kinds of review record, and telling them
	// apart is the authority model working: the code node's acceptance
	// verdict is a VALIDATOR-origin review carrying `derived` authority (a
	// deterministic computation over what was measured), while a human
	// decision's confirmation is a HUMAN-origin review carrying `confirmed`.
	// Only the latter can confirm anything, and there must be exactly one of
	// it per decision.
	var human []ledger.Record
	for _, rec := range filterRecords(records, ledger.RecordReview) {
		if rec.Origin.Kind == ledger.OriginHuman {
			human = append(human, rec)
			continue
		}
		if rec.Authority == ledger.AuthorityConfirmed {
			t.Errorf("review %s has %s origin and confirmed authority; confirmation is a human review transaction (PRD §10.4)",
				rec.ID, rec.Origin.Kind)
		}
	}
	if len(human) != 1 {
		t.Fatalf("the run holds %d human-origin review records, want exactly 1 (the decision's own confirmation)", len(human))
	}
	review := human[0]
	if review.Authority != ledger.AuthorityConfirmed {
		t.Errorf("review authority = %q, want confirmed", review.Authority)
	}
	if review.Origin.Kind != ledger.OriginHuman || review.Origin.ActorID != decider {
		t.Errorf("review origin = %+v, want human/%s", review.Origin, decider)
	}
	if review.SubjectRef.String() != decision.ID {
		t.Errorf("review subject_ref = %q, want the decision record %q", review.SubjectRef, decision.ID)
	}
	if len(review.ProvenanceRefs) != 1 || review.ProvenanceRefs[0] != decision.ID {
		t.Errorf("review provenance_refs = %v, want [%s]", review.ProvenanceRefs, decision.ID)
	}
}

// -----------------------------------------------------------------------
// Fixtures and API helpers
// -----------------------------------------------------------------------

// registerHumanDecider inserts the actors row a human decision's ledger
// records point at. ledger_records.origin_actor_id is a foreign key, so a
// person who is not a registered actor cannot be recorded as having decided
// anything — which is the point: "a confirmation nobody is accountable for is
// not a confirmation" (PRD §10.8).
func registerHumanDecider(t *testing.T, db *postgres.Store, namespaceID string) string {
	t.Helper()
	id := "actor_" + idstore.NewULID()
	if _, err := db.Pool().Exec(context.Background(), `
		INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
		VALUES ($1, $2, $3, 1, 'human', 'internal')
	`, id, namespaceID, "people/platform-ai-approver"); err != nil {
		t.Fatalf("register the human decider: %v", err)
	}
	return id
}

// awaitPendingHumanTask polls GET /v1alpha1/human-tasks?status=pending — the
// list a review surface renders — until this run's task appears.
func (s *stack) awaitPendingHumanTask(t *testing.T, runID string, timeout time.Duration) humanTaskOut {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var list humanTaskListOut
		if status := s.getJSON("/v1alpha1/human-tasks?status=pending", &list); status != http.StatusOK {
			t.Fatalf("GET pending human tasks: status %d", status)
		}
		for _, task := range list.Items {
			if task.RunID == runID {
				return task
			}
		}
		if engine.RunState(s.runView(t, runID).Run.State).Terminal() {
			dumpRunState(t, s, runID)
			t.Fatalf("run %s reached a terminal state without ever parking on the approval node (worker errors: %v)",
				runID, s.errors())
		}
		time.Sleep(50 * time.Millisecond)
	}
	dumpRunState(t, s, runID)
	t.Fatalf("run %s never produced a pending human task within %s (worker errors: %v)", runID, timeout, s.errors())
	return humanTaskOut{}
}

func (s *stack) humanTask(t *testing.T, id string) humanTaskOut {
	t.Helper()
	var task humanTaskOut
	if status := s.getJSON("/v1alpha1/human-tasks/"+id, &task); status != http.StatusOK {
		t.Fatalf("GET human task %s: status %d", id, status)
	}
	return task
}

// ledgerVersion reads the run's current ledger version the way a decider's
// surface does: from the ledger endpoint it just rendered. The decision
// carries it back as expected_ledger_version, so a decision taken against a
// stale view of the ledger is refused rather than silently applied.
func (s *stack) ledgerVersion(t *testing.T, runID string) int64 {
	t.Helper()
	var out struct {
		LedgerVersion int64 `json:"ledger_version"`
	}
	if status := s.getJSON("/v1alpha1/runs/"+runID+"/ledger", &out); status != http.StatusOK {
		t.Fatalf("GET ledger: status %d", status)
	}
	return out.LedgerVersion
}

// decide POSTs a human-task decision with the given bearer token (empty means
// no Authorization header at all) and returns the status code, decoding the
// body into out when one is supplied and the call succeeded.
func (s *stack) decide(t *testing.T, taskID, bearer string, body decideRequest, out any) int {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode decision: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost,
		s.server.URL+"/v1alpha1/human-tasks/"+taskID+"/decision", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("build decision request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST decision: %v", err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode decision response: %v", err)
		}
	}
	return resp.StatusCode
}

func countWorkItems(t *testing.T, s *stack, nodeRunID string) int {
	t.Helper()
	return countRows(t, s, `SELECT COUNT(*)::int FROM work_items WHERE node_run_id = $1`, nodeRunID)
}

func countRows(t *testing.T, s *stack, query string, args ...any) int {
	t.Helper()
	var count int
	if err := s.db.Pool().QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatalf("count rows (%s): %v", query, err)
	}
	return count
}

func nodeRunStatus(t *testing.T, s *stack, nodeRunID string) string {
	t.Helper()
	var status string
	if err := s.db.Pool().QueryRow(context.Background(),
		`SELECT status FROM node_runs WHERE id = $1`, nodeRunID).Scan(&status); err != nil {
		t.Fatalf("read node run status: %v", err)
	}
	return status
}
