package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/engine"
)

// Task t18 (spec c6/c10, plan decision q1): the ticket page has to be able
// to DECIDE, not just narrate.
//
// Before this, a human task fanned out to Jira named its options in a
// comment and linked to /tickets/SCRUM-N — a page that listed the task as an
// opaque row and offered nothing to click. GET /v1alpha1/tickets/{id} now
// serves the decidable subset with the outcomes the engine will accept, the
// ledger version a decision must be submitted against, and the ticket's own
// board URL.

// advanceTicketToReview is advanceToReview's ticket-bound sibling: the run's
// input carries the sweep's Jira work-item fact, so the run is reachable
// from the ticket by `input->>'id'` (the SubjectFromInput address the ticket
// projection opts into) and carries the `details_url` the back-link is
// composed from.
func advanceTicketToReview(t *testing.T, f *fixture, input string) apipkg.RunOut {
	t.Helper()

	// Publishing is content-addressed and idempotent: the second call in a
	// test that stages two tickets re-publishes the same digest and answers
	// 200 rather than 201, so both are accepted here.
	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(readApprovalWorkflow(t))}, &published)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("publish approval workflow: status = %d; body = %s", resp.StatusCode, body)
	}

	var run apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(input)}, &run)
	requireStatus(t, resp, body, http.StatusCreated)

	claimed := f.claim("worker-a", onlyReadyNodeRun(t, getRunView(t, f, run.ID)).ID)
	if _, err := f.api.Engine.CompleteAttempt(context.Background(), engine.CompletionRequest{
		WorkID:       claimed.ID,
		WorkerID:     "worker-a",
		FencingToken: claimed.FencingToken,
		Attempt:      int(claimed.Attempt),
		TechStatus:   engine.StatusSucceeded,
		Outcome:      "completed",
		Output:       json.RawMessage(`{"scope":"s"}`),
	}); err != nil {
		t.Fatalf("complete intake: %v", err)
	}
	return run
}

func getTicketProjection(t *testing.T, f *fixture, ticketID string) apipkg.TicketOut {
	t.Helper()
	var ticket apipkg.TicketOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/tickets/"+ticketID), nil, &ticket)
	requireStatus(t, resp, body, http.StatusOK)
	return ticket
}

// TestTicketProjectionServesItsPendingTaskAndBackLink is t18's acceptance:
// a ticket with one pending task returns it with allowed_outcomes, and a
// ticket_url composed from the Jira fact the run carries.
func TestTicketProjectionServesItsPendingTaskAndBackLink(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	const ticketID = "SCRUM-18"
	const detailsURL = "https://jira.example.test/browse/SCRUM-18"
	run := advanceTicketToReview(t, f, `{"subject":"decide it","source":"jira","id":"`+ticketID+
		`","status":"In Progress","details_url":"`+detailsURL+`"}`)

	ticket := getTicketProjection(t, f, ticketID)

	if ticket.TicketURL != detailsURL {
		t.Fatalf("ticket_url = %q, want %q — the page cannot link back to the board", ticket.TicketURL, detailsURL)
	}
	if len(ticket.PendingTasks) != 1 {
		t.Fatalf("pending_tasks = %+v, want exactly the one waiting approval", ticket.PendingTasks)
	}
	task := ticket.PendingTasks[0]
	if task.RunID != run.ID {
		t.Errorf("pending task run_id = %q, want %q", task.RunID, run.ID)
	}
	if task.Kind != "approval" {
		t.Errorf("pending task kind = %q, want approval", task.Kind)
	}
	// The outcomes served are the ones DecideHumanTask will actually
	// accept, so a button rendered from this list cannot 400.
	if !contains(task.AllowedOutcomes, "approved") || !contains(task.AllowedOutcomes, "rejected") {
		t.Errorf("allowed_outcomes = %v, want the approval node's approved/rejected", task.AllowedOutcomes)
	}
	if task.DecisionSchemaRef != "./contracts/review-decision.schema.json" {
		t.Errorf("decision_schema_ref = %q, want the node's authored ref", task.DecisionSchemaRef)
	}
	if task.Deadline == nil || task.Deadline.Before(task.CreatedAt) {
		t.Errorf("deadline = %v, want the node's 2h deadline after created_at %v", task.Deadline, task.CreatedAt)
	}

	// The version served is the one a decision must be submitted against:
	// posting it back is accepted, and the decided task leaves the list.
	resp, body := authedDecide(t, f, task.ID, decisionAuthSecret, decideHumanTaskReq{
		Outcome:               "approved",
		DeciderActorID:        f.insertActorKind("approver", "human"),
		ExpectedLedgerVersion: task.LedgerVersion,
	})
	requireStatus(t, resp, body, http.StatusOK)

	after := getTicketProjection(t, f, ticketID)
	if len(after.PendingTasks) != 0 {
		t.Fatalf("pending_tasks after the decision = %+v, want empty", after.PendingTasks)
	}
	if len(after.HumanTasks) != 1 {
		t.Fatalf("human_tasks after the decision = %d, want the decided task still in the history", len(after.HumanTasks))
	}
}

// TestTicketBackLinkFallsBackToJiraSite covers the other half of c10's
// composition: a fact that names the site but no details_url still yields a
// link, built the same way sweep.py builds one.
func TestTicketBackLinkFallsBackToJiraSite(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	const ticketID = "SCRUM-19"
	advanceTicketToReview(t, f, `{"subject":"decide it","source":"jira","id":"`+ticketID+
		`","jira_site":"jira.example.test"}`)

	ticket := getTicketProjection(t, f, ticketID)
	if want := "https://jira.example.test/browse/SCRUM-19"; ticket.TicketURL != want {
		t.Fatalf("ticket_url = %q, want %q", ticket.TicketURL, want)
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// The ticket's tasks are read by RUN, not "the newest N in the namespace,
// then filtered" (task t18). Two tickets each holding a pending task must
// each see exactly their own — the property that stops a busy namespace from
// emptying the very page a Jira comment sent a decider to.
func TestTicketPendingTasksAreScopedToTheTicketsOwnRuns(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	mine := advanceTicketToReview(t, f, `{"subject":"a","source":"jira","id":"SCRUM-20"}`)
	theirs := advanceTicketToReview(t, f, `{"subject":"b","source":"jira","id":"SCRUM-21"}`)

	for _, tc := range []struct{ ticketID, runID string }{
		{"SCRUM-20", mine.ID},
		{"SCRUM-21", theirs.ID},
	} {
		ticket := getTicketProjection(t, f, tc.ticketID)
		if len(ticket.PendingTasks) != 1 || ticket.PendingTasks[0].RunID != tc.runID {
			t.Fatalf("%s pending_tasks = %+v, want exactly the task on run %s", tc.ticketID, ticket.PendingTasks, tc.runID)
		}
	}
}

func TestTicketProjectionFindsPendingTaskBeyondFirstRunPage(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	const ticketID = "SCRUM-22"
	older := advanceTicketToReview(t, f, `{"subject":"older decision","source":"jira","id":"`+ticketID+`"}`)

	// Put 500 newer runs for this ticket ahead of the run that owns the
	// pending task. These rows need no execution graph: the ticket projection
	// derives tasks and ledger records by run id, and empty ledger histories
	// are a valid result.
	if _, err := f.store.Pool().Exec(t.Context(), `
		INSERT INTO runs (id, namespace_id, workflow_version_id, status, input, subject, created_at, updated_at)
		SELECT 'ticket-page-filler-' || lpad(n::text, 3, '0'), namespace_id,
		       workflow_version_id, 'completed', '{}'::jsonb, $2,
		       now() + n * interval '1 millisecond', now() + n * interval '1 millisecond'
		FROM runs CROSS JOIN generate_series(1, 500) AS n
		WHERE id = $1`, older.ID, ticketID); err != nil {
		t.Fatalf("stage newer ticket runs: %v", err)
	}

	ticket := getTicketProjection(t, f, ticketID)
	if len(ticket.PendingTasks) != 1 || ticket.PendingTasks[0].RunID != older.ID {
		t.Fatalf("pending_tasks = %+v, want the task on older run %s beyond the first 500 runs", ticket.PendingTasks, older.ID)
	}
}
