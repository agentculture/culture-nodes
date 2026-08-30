package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/events"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
)

// Task t17 (spec c28, honesty condition h19): freezing a ticket must end
// that ticket's runs. Before this, handleFreezeTicket wrote the freeze row
// and the page banner and touched no run — the SCRUM-5 spec-chain run
// 01M16GMQMWYCA0EW0V7MHHQFWN sat at 'running' on a Done, frozen ticket,
// parked on a question that would never be answered.
//
// Both selectors a ticket's runs can be found by are exercised in every
// case below: one run carries the modern runs.subject column
// (migrations/0038, indexed by 0047), the other carries the ticket only
// inside its own input as the jira work-item `id` — which is the shape the
// prod run this task exists for actually has.

// freezeTicketRuns publishes the minimal workflow and creates two live runs
// bound to ticketID by the two different addresses, plus one run bound to a
// DIFFERENT ticket that no freeze of ticketID may touch. It returns the
// subject-column run, the input-only run, and the other ticket's run.
func freezeTicketRuns(t *testing.T, f *fixture, ticketID, otherTicketID string) (bySubject, byInput, otherTicket apipkg.RunOut) {
	t.Helper()
	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(readFixtureWorkflow(t, "minimal.workflow.yaml"))}, &published)
	requireStatus(t, resp, body, http.StatusCreated)

	newRun := func(input string) apipkg.RunOut {
		t.Helper()
		var run apipkg.RunOut
		resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
			createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(input)}, &run)
		requireStatus(t, resp, body, http.StatusCreated)
		return run
	}
	setSubject := func(runID, subject string) {
		t.Helper()
		if _, err := f.store.Pool().Exec(context.Background(),
			`UPDATE runs SET subject = $2 WHERE id = $1`, runID, subject); err != nil {
			t.Fatalf("set subject on run %s: %v", runID, err)
		}
	}

	// POST /v1alpha1/runs has no subject parameter — a subject is stamped
	// by TriggerEvent, not by an operator — so the column is set directly,
	// the same way this package's other tests stage store state a public
	// route does not expose.
	bySubject = newRun(`{}`)
	setSubject(bySubject.ID, ticketID)
	byInput = newRun(`{"source":"jira","id":"` + ticketID + `","status":"In Progress"}`)
	otherTicket = newRun(`{}`)
	setSubject(otherTicket.ID, otherTicketID)

	for _, run := range []apipkg.RunOut{bySubject, byInput, otherTicket} {
		if got := runStatus(t, f, run.ID); got != "running" {
			t.Fatalf("run %s starts at %q, want running — the fixture must stage LIVE runs", run.ID, got)
		}
	}
	return bySubject, byInput, otherTicket
}

// runReason reads the run-level reason column migration 0052 adds.
func runReason(t *testing.T, f *fixture, runID string) string {
	t.Helper()
	var reason *string
	if err := f.store.Pool().QueryRow(context.Background(),
		`SELECT reason FROM runs WHERE id = $1`, runID).Scan(&reason); err != nil {
		t.Fatalf("read reason of run %s: %v", runID, err)
	}
	if reason == nil {
		return ""
	}
	return *reason
}

// workItemStates counts a run's work items by state.
func workItemStates(t *testing.T, f *fixture, runID string) map[string]int {
	t.Helper()
	rows, err := f.store.Pool().Query(context.Background(), `
		SELECT state, count(*) FROM work_items
		WHERE node_run_id IN (SELECT id FROM node_runs WHERE run_id = $1) GROUP BY state`, runID)
	if err != nil {
		t.Fatalf("read work items of run %s: %v", runID, err)
	}
	defer rows.Close()
	states := map[string]int{}
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			t.Fatalf("scan work item state: %v", err)
		}
		states[state] = count
	}
	return states
}

func activeTokens(t *testing.T, f *fixture, runID string) int {
	t.Helper()
	var count int
	if err := f.store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM tokens WHERE run_id = $1 AND state = 'active'`, runID).Scan(&count); err != nil {
		t.Fatalf("count active tokens of run %s: %v", runID, err)
	}
	return count
}

func getTicketFreeze(t *testing.T, f *fixture, ticketID string) (apipkg.TicketOut, *apipkg.TicketFreezeOut) {
	t.Helper()
	var ticket apipkg.TicketOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/tickets/"+ticketID), nil, &ticket)
	requireStatus(t, resp, body, http.StatusOK)
	if !ticket.Frozen {
		t.Fatalf("ticket %s is not frozen", ticketID)
	}
	if ticket.Freeze == nil {
		t.Fatalf("frozen ticket %s carries no freeze summary", ticketID)
	}
	return ticket, ticket.Freeze
}

// TestFreezingADoneTicketCancelsItsSubjectRuns is h19's first half: a Done
// ticket's freeze is terminal, and every run it ended says why.
func TestFreezingADoneTicketCancelsItsSubjectRuns(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	bySubject, byInput, otherTicket := freezeTicketRuns(t, f, "SCRUM-9", "SCRUM-10")

	var freeze struct {
		Cancelled []string `json:"cancelled_runs"`
		Parked    []string `json:"parked_runs"`
	}
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/tickets/SCRUM-9/freeze"), decisionAuthSecret,
		map[string]any{"frozen_by": "operator", "ticket_status": "Done", "merged_pr": map[string]any{"number": 230}}, &freeze)
	requireStatus(t, resp, body, http.StatusOK)
	if len(freeze.Cancelled) != 2 || len(freeze.Parked) != 0 {
		t.Fatalf("freeze reported cancelled=%v parked=%v, want both subject runs cancelled", freeze.Cancelled, freeze.Parked)
	}

	for _, run := range []apipkg.RunOut{bySubject, byInput} {
		if got := runStatus(t, f, run.ID); got != "cancelled" {
			t.Errorf("run %s = %q after freezing a Done ticket, want cancelled", run.ID, got)
		}
		if got := runReason(t, f, run.ID); got != apipkg.TicketFrozenReason {
			t.Errorf("run %s reason = %q, want %q", run.ID, got, apipkg.TicketFrozenReason)
		}
		if got := activeTokens(t, f, run.ID); got != 0 {
			t.Errorf("run %s keeps %d active tokens after a cancel", run.ID, got)
		}
	}
	// The freeze reaches exactly its own ticket. A run correlated to a
	// different ticket is not collateral, and neither is its reason.
	if got := runStatus(t, f, otherTicket.ID); got != "running" {
		t.Errorf("SCRUM-10's run = %q, want running — a SCRUM-9 freeze must not reach it", got)
	}
	if got := runReason(t, f, otherTicket.ID); got != "" {
		t.Errorf("SCRUM-10's run carries reason %q from another ticket's freeze", got)
	}

	ticket, summary := getTicketFreeze(t, f, "SCRUM-9")
	if summary.Cancelled != 2 || summary.Parked != 0 || summary.Reason != apipkg.TicketFrozenReason {
		t.Fatalf("freeze summary = %+v, want 2 cancelled / 0 parked / reason %s", summary, apipkg.TicketFrozenReason)
	}
	if summary.TicketStatus != "Done" {
		t.Errorf("freeze summary status = %q, want Done", summary.TicketStatus)
	}
	// The banner a human reads names the count and the reason.
	for _, want := range []string{"2 runs cancelled", "0 parked", apipkg.TicketFrozenReason, "Done"} {
		if !strings.Contains(summary.Banner, want) {
			t.Errorf("banner %q does not name %q", summary.Banner, want)
		}
	}
	// The same reason is on each affected run in the projection, not only
	// in the aggregate — a reader of one run must not have to read the
	// banner to learn why it stopped.
	reasons := 0
	for _, run := range ticket.Runs {
		if run.Reason == apipkg.TicketFrozenReason && run.State == "cancelled" {
			reasons++
		}
	}
	if reasons != 2 {
		t.Fatalf("projection carries %d cancelled runs with reason %s, want 2", reasons, apipkg.TicketFrozenReason)
	}
}

// TestFreezingANonDoneTicketParksItsSubjectRuns is h19's second half: a
// ticket frozen in any other status parks its runs instead, and a park
// leaves every durable row a resume would need.
func TestFreezingANonDoneTicketParksItsSubjectRuns(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	bySubject, byInput, otherTicket := freezeTicketRuns(t, f, "SCRUM-9", "SCRUM-10")

	var freeze struct {
		Cancelled []string `json:"cancelled_runs"`
		Parked    []string `json:"parked_runs"`
	}
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/tickets/SCRUM-9/freeze"), decisionAuthSecret,
		map[string]any{"frozen_by": "operator", "ticket_status": "In Progress"}, &freeze)
	requireStatus(t, resp, body, http.StatusOK)
	if len(freeze.Parked) != 2 || len(freeze.Cancelled) != 0 {
		t.Fatalf("freeze reported cancelled=%v parked=%v, want both subject runs parked", freeze.Cancelled, freeze.Parked)
	}

	for _, run := range []apipkg.RunOut{bySubject, byInput} {
		if got := runStatus(t, f, run.ID); got != "waiting" {
			t.Errorf("run %s = %q after freezing a non-Done ticket, want waiting", run.ID, got)
		}
		if got := runReason(t, f, run.ID); got != apipkg.TicketFrozenReason {
			t.Errorf("run %s reason = %q, want %q", run.ID, got, apipkg.TicketFrozenReason)
		}
		// Parked, not ended: the work item is in the same 'waiting' state
		// the signal wait leaves its item in — invisible to ClaimWork, and
		// still there to be resumed — and the token is still active. A
		// cancel would have made both of these 'cancelled' and 'consumed'.
		states := workItemStates(t, f, run.ID)
		if states["waiting"] != 1 || states["ready"] != 0 || states["cancelled"] != 0 {
			t.Errorf("run %s work items = %v, want exactly one parked 'waiting' item", run.ID, states)
		}
		if got := activeTokens(t, f, run.ID); got == 0 {
			t.Errorf("run %s lost its active tokens to a PARK — a parked run must stay resumable", run.ID)
		}
	}
	if got := runStatus(t, f, otherTicket.ID); got != "running" {
		t.Errorf("SCRUM-10's run = %q, want running", got)
	}

	_, summary := getTicketFreeze(t, f, "SCRUM-9")
	if summary.Parked != 2 || summary.Cancelled != 0 {
		t.Fatalf("freeze summary = %+v, want 0 cancelled / 2 parked", summary)
	}
	for _, want := range []string{"2 parked", "0 runs cancelled", apipkg.TicketFrozenReason, "In Progress"} {
		if !strings.Contains(summary.Banner, want) {
			t.Errorf("banner %q does not name %q", summary.Banner, want)
		}
	}
}

func TestFreezingANonDoneTicketTwiceParksEachRunOnce(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	bySubject, byInput, _ := freezeTicketRuns(t, f, "SCRUM-9", "SCRUM-10")

	for attempt := 1; attempt <= 2; attempt++ {
		var freeze struct {
			Parked []string `json:"parked_runs"`
		}
		resp, body := doJSONBearer(t, f.client, http.MethodPost,
			f.url("/v1alpha1/tickets/SCRUM-9/freeze"), decisionAuthSecret,
			map[string]any{"frozen_by": "operator", "ticket_status": "In Progress"}, &freeze)
		requireStatus(t, resp, body, http.StatusOK)
		want := 2
		if attempt == 2 {
			want = 0
		}
		if len(freeze.Parked) != want {
			t.Fatalf("freeze attempt %d parked %v, want %d runs", attempt, freeze.Parked, want)
		}
	}

	for _, run := range []apipkg.RunOut{bySubject, byInput} {
		var eventCount, outboxCount int
		if err := f.store.Pool().QueryRow(t.Context(), `
			SELECT count(*) FROM events
			WHERE aggregate_id = $1 AND event_type = $2`, run.ID, events.TypeRunWaiting).Scan(&eventCount); err != nil {
			t.Fatalf("count waiting events for run %s: %v", run.ID, err)
		}
		if err := f.store.Pool().QueryRow(t.Context(), `
			SELECT count(*) FROM outbox
			WHERE topic = $2 AND payload->>'run_id' = $1`, run.ID, events.TypeRunWaiting).Scan(&outboxCount); err != nil {
			t.Fatalf("count waiting outbox rows for run %s: %v", run.ID, err)
		}
		if eventCount != 1 || outboxCount != 1 {
			t.Errorf("run %s has %d waiting events and %d waiting outbox rows, want one of each", run.ID, eventCount, outboxCount)
		}
	}
}

// TestFreezingATicketWithNoStatusParksRatherThanCancels pins the
// conservative default. A caller that does not say what the board status is
// leaves this control plane unable to find out — the Jira bridge has no read
// verb — and the reversible outcome is the honest one.
func TestFreezingATicketWithNoStatusParksRatherThanCancels(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	bySubject, _, _ := freezeTicketRuns(t, f, "SCRUM-9", "SCRUM-10")

	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/tickets/SCRUM-9/freeze"), decisionAuthSecret,
		map[string]any{"frozen_by": "operator"}, nil)
	requireStatus(t, resp, body, http.StatusOK)

	if got := runStatus(t, f, bySubject.ID); got != "waiting" {
		t.Fatalf("run %s = %q after a freeze that named no status, want waiting (parked)", bySubject.ID, got)
	}
	_, summary := getTicketFreeze(t, f, "SCRUM-9")
	if summary.Cancelled != 0 || summary.Parked != 2 {
		t.Fatalf("freeze summary = %+v, want 0 cancelled / 2 parked", summary)
	}
	if !strings.Contains(summary.Banner, "unknown") {
		t.Errorf("banner %q does not say the status was unknown", summary.Banner)
	}
}

// TestMergedPRDeliveryParksTheTicketsRuns covers the path the sweep
// actually takes: POST /v1alpha1/events with a pr.merged fact freezes the
// ticket inside the delivery transaction (internal/store/postgres/signal.go)
// and must end its runs too. The fact carries no board status, so it parks.
func TestMergedPRDeliveryParksTheTicketsRuns(t *testing.T) {
	f := newFixtureWithEventAuth(t, eventTokenSecret)
	bySubject, byInput, otherTicket := freezeTicketRuns(t, f, "SCRUM-9", "SCRUM-10")

	resp, body := postEvent(t, f, eventTokenSecret, map[string]any{
		"name":    "pr.merged",
		"emitter": "pr-upkeep/sweep",
		"payload": map[string]any{
			"issue_key": "SCRUM-9", "number": 230,
			"url": "https://example.test/pull/230", "merged_at": "2026-08-29T12:00:00Z",
		},
	}, nil)
	requireStatus(t, resp, body, http.StatusCreated)

	for _, run := range []apipkg.RunOut{bySubject, byInput} {
		if got := runStatus(t, f, run.ID); got != "waiting" {
			t.Errorf("run %s = %q after a pr.merged freeze, want waiting", run.ID, got)
		}
		if got := runReason(t, f, run.ID); got != apipkg.TicketFrozenReason {
			t.Errorf("run %s reason = %q, want %q", run.ID, got, apipkg.TicketFrozenReason)
		}
	}
	if got := runStatus(t, f, otherTicket.ID); got != "running" {
		t.Errorf("SCRUM-10's run = %q, want running", got)
	}
	_, summary := getTicketFreeze(t, f, "SCRUM-9")
	if summary.Parked != 2 {
		t.Fatalf("freeze summary = %+v, want 2 parked", summary)
	}
}
