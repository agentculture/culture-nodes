package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
)

type frameOut struct {
	TicketID string          `json:"ticket_id"`
	Version  int             `json:"version"`
	Frame    json.RawMessage `json:"frame"`
}

type ticketOut struct {
	TicketID string          `json:"ticket_id"`
	Frame    *frameOut       `json:"latest_frame"`
	Runs     []apipkg.RunOut `json:"runs"`
	Replies  []struct {
		Text string `json:"text"`
	} `json:"replies"`
}

func TestTicketFrameVersionsAndLatestClaimsAreByteEqual(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	first := json.RawMessage(`{"claims":[{"id":"c1","state":"proposed"}]}`)
	second := json.RawMessage(`{"claims":[{"id":"c1","state":"confirmed"},{"id":"c2","state":"proposed"}]}`)
	for i, frame := range []json.RawMessage{first, second} {
		var posted frameOut
		resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/tickets/SCRUM-9/frame"), decisionAuthSecret,
			map[string]any{"frame": frame, "posted_by": "company/developer"}, &posted)
		requireStatus(t, resp, body, http.StatusCreated)
		if posted.Version != i+1 {
			t.Fatalf("posted version = %d, want %d", posted.Version, i+1)
		}
	}
	if _, err := f.store.Pool().Exec(t.Context(), `INSERT INTO ticket_replies
		(id,namespace_id,ticket_id,replier,text) VALUES ('reply-scrum-9',$1,'SCRUM-9','operator','answer')`, f.nsID); err != nil {
		t.Fatal(err)
	}
	var ticket ticketOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/tickets/SCRUM-9"), nil, &ticket)
	requireStatus(t, resp, body, http.StatusOK)
	if ticket.Frame == nil || ticket.Frame.Version != 2 || !bytes.Equal(ticket.Frame.Frame, second) {
		t.Fatalf("latest frame = %+v, want version 2 bytes %s", ticket.Frame, second)
	}
	if len(ticket.Replies) != 1 || ticket.Replies[0].Text != "answer" {
		t.Fatalf("composed replies = %+v", ticket.Replies)
	}
}

func TestTicketFramePostRequiresDecisionToken(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/tickets/SCRUM-9/frame"),
		map[string]any{"frame": json.RawMessage(`{"claims":[]}`), "posted_by": "test"}, nil)
	requireStatus(t, resp, body, http.StatusUnauthorized)
}

func TestListRunsFiltersBySubject(t *testing.T) {
	f := newFixture(t)
	runA := createRunViaAPI(t, f)
	runB := createRunViaAPI(t, f)
	if _, err := f.store.Pool().Exec(t.Context(), `UPDATE runs SET subject=CASE id WHEN $1 THEN 'SCRUM-9' ELSE 'SCRUM-DECOY' END WHERE id IN ($1,$2)`, runA.ID, runB.ID); err != nil {
		t.Fatal(err)
	}
	var listed apipkg.RunListOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs?subject=SCRUM-9"), nil, &listed)
	requireStatus(t, resp, body, http.StatusOK)
	if len(listed.Items) != 1 || listed.Items[0].ID != runA.ID || listed.Items[0].Subject != "SCRUM-9" {
		t.Fatalf("subject-filtered runs = %+v", listed.Items)
	}
}

func createRunViaAPI(t *testing.T, f *fixture) apipkg.RunOut {
	t.Helper()
	source := readFixtureWorkflow(t, "edge-order-ordered.workflow.yaml")
	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"), workflowSourceReq{Format: "yaml", Source: string(source)}, &published)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		requireStatus(t, resp, body, http.StatusCreated)
	}
	var run apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"), createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{}`)}, &run)
	requireStatus(t, resp, body, http.StatusCreated)
	return run
}
