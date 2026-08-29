package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
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
	PageLink *struct {
		CommentID string `json:"comment_id"`
		Status    string `json:"status"`
	} `json:"page_link"`
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

func TestTicketProjectionExposesPageLinkStatus(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.Pool().Exec(t.Context(), `INSERT INTO jira_ticket_report_outbox
		(id,namespace_id,phase,target_actor_key,issue_key,payload,status)
		VALUES ('page-link-projection',$1,'page-link',$2,'SCRUM-9',$3,'published')`, f.nsID,
		storepg.JiraTicketReporterActorKey, json.RawMessage(`{"verb":"post_comment","issue":"SCRUM-9","comment":"culture-nodes page: /tickets/SCRUM-9 [culture-nodes:ticket-page-link]","phase":"page-link","comment_id":"10191"}`)); err != nil {
		t.Fatal(err)
	}
	var ticket ticketOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/tickets/SCRUM-9"), nil, &ticket)
	requireStatus(t, resp, body, http.StatusOK)
	if ticket.PageLink == nil || ticket.PageLink.Status != "published" || ticket.PageLink.CommentID != "10191" {
		t.Fatalf("page_link = %+v", ticket.PageLink)
	}
}

type replyOut struct {
	ID            string `json:"id"`
	SignalEventID string `json:"signal_event_id"`
	Duplicate     bool   `json:"duplicate"`
}

// postReply is one guarded reply POST for SCRUM-<ticket> under a client key.
func postReply(t *testing.T, f *fixture, ticket, replyID string, out *replyOut) (*http.Response, []byte) {
	t.Helper()
	var decoded any // a typed nil pointer is not a nil interface to the decoder
	if out != nil {
		decoded = out
	}
	return doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/tickets/"+ticket+"/replies"), decisionAuthSecret,
		map[string]any{"reply_id": replyID, "replier": "operator", "text": "Use A", "question_id": "q-9"}, decoded)
}

// replyRowCounts is the three-way invariant every reply write must keep:
// facts, reply rows, and mirror rows for one ticket, plus the durable
// idempotency cursors written for it.
type replyRowCounts struct{ facts, replies, mirrors, cursors int }

func countReplyRows(t *testing.T, f *fixture, ticket string) replyRowCounts {
	t.Helper()
	var c replyRowCounts
	err := f.store.Pool().QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM signal_events WHERE namespace_id=$1 AND name='pr-upkeep.jira.comment' AND payload->>'id'=$2),
		(SELECT count(*) FROM ticket_replies WHERE namespace_id=$1 AND ticket_id=$2),
		(SELECT count(*) FROM jira_ticket_report_outbox WHERE namespace_id=$1 AND issue_key=$2 AND phase='reply'),
		(SELECT count(*) FROM signal_event_watermarks WHERE namespace_id=$1 AND source_key LIKE 'page-reply:'||$2||':%')`,
		f.nsID, ticket).Scan(&c.facts, &c.replies, &c.mirrors, &c.cursors)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestTicketReplyAppendsHumanFactAndOneMirrorIntent(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	var reply replyOut
	resp, body := postReply(t, f, "SCRUM-9", "reply-key-1", &reply)
	requireStatus(t, resp, body, http.StatusCreated)
	var name, emitter, payload, sourceKey string
	if err := f.store.Pool().QueryRow(t.Context(), `SELECT name,emitter,payload::text,source_key FROM signal_events WHERE id=$1`, reply.SignalEventID).Scan(&name, &emitter, &payload, &sourceKey); err != nil {
		t.Fatal(err)
	}
	if name != "pr-upkeep.jira.comment" || emitter != "ticket-page" || sourceKey != "page-reply:SCRUM-9:reply-key-1" ||
		!bytes.Contains([]byte(payload), []byte(`"kind": "human"`)) ||
		!bytes.Contains([]byte(payload), []byte(`"originating_question_id": "q-9"`)) {
		t.Fatalf("fact name=%q emitter=%q source_key=%q payload=%s", name, emitter, sourceKey, payload)
	}
	var rowEvent string
	if err := f.store.Pool().QueryRow(t.Context(), `SELECT signal_event_id FROM ticket_replies WHERE id=$1`, reply.ID).Scan(&rowEvent); err != nil {
		t.Fatal(err)
	}
	if rowEvent != reply.SignalEventID {
		t.Fatalf("reply row cites fact %q, response cites %q", rowEvent, reply.SignalEventID)
	}
	if c := countReplyRows(t, f, "SCRUM-9"); c != (replyRowCounts{1, 1, 1, 1}) {
		t.Fatalf("rows after one reply = %+v, want one of each", c)
	}
}

func TestTicketReplyRetryWithSameReplyIDIsOneReply(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	var first, second replyOut
	resp, body := postReply(t, f, "SCRUM-9", "reply-key-retry", &first)
	requireStatus(t, resp, body, http.StatusCreated)
	resp, body = postReply(t, f, "SCRUM-9", "reply-key-retry", &second)
	requireStatus(t, resp, body, http.StatusOK)
	if first.Duplicate || !second.Duplicate || second.ID != first.ID || second.SignalEventID != first.SignalEventID {
		t.Fatalf("retry = %+v, first = %+v: want the same reply and fact, flagged duplicate", second, first)
	}
	if c := countReplyRows(t, f, "SCRUM-9"); c != (replyRowCounts{1, 1, 1, 1}) {
		t.Fatalf("rows after a retry = %+v, want one of each", c)
	}
	// A different key on the same ticket is a different reply.
	var third replyOut
	resp, body = postReply(t, f, "SCRUM-9", "reply-key-other", &third)
	requireStatus(t, resp, body, http.StatusCreated)
	if c := countReplyRows(t, f, "SCRUM-9"); c != (replyRowCounts{2, 2, 2, 2}) {
		t.Fatalf("rows after a second reply = %+v, want two of each", c)
	}
}

func TestTicketReplyRequiresReplyID(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/tickets/SCRUM-9/replies"), decisionAuthSecret,
		map[string]any{"replier": "operator", "text": "Use A"}, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	if c := countReplyRows(t, f, "SCRUM-9"); c != (replyRowCounts{}) {
		t.Fatalf("rows after a rejected reply = %+v, want none", c)
	}
}

// TestTicketReplyMirrorFailureLeavesNoFactOrReplyRow is the failure-injection
// half of the atomicity claim: when the LAST write of the transaction (the
// Jira mirror row) fails, neither the fact, the reply row, nor the
// idempotency cursor survives — and the retry then makes the reply for real.
func TestTicketReplyMirrorFailureLeavesNoFactOrReplyRow(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	const constraint = "test_reply_mirror_poison_scrum_poison"
	ctx := t.Context()
	if _, err := f.store.Pool().Exec(ctx, `ALTER TABLE jira_ticket_report_outbox ADD CONSTRAINT `+constraint+
		` CHECK (NOT (phase='reply' AND issue_key='SCRUM-POISON'))`); err != nil {
		t.Fatal(err)
	}
	dropPoison := func() {
		_, _ = f.store.Pool().Exec(ctx, `ALTER TABLE jira_ticket_report_outbox DROP CONSTRAINT IF EXISTS `+constraint)
	}
	t.Cleanup(dropPoison)

	resp, body := postReply(t, f, "SCRUM-POISON", "reply-key-poison", nil)
	requireStatus(t, resp, body, http.StatusInternalServerError)
	if c := countReplyRows(t, f, "SCRUM-POISON"); c != (replyRowCounts{}) {
		t.Fatalf("rows after a failed mirror write = %+v, want none: the fact and the reply row must roll back with it", c)
	}

	dropPoison()
	var reply replyOut
	resp, body = postReply(t, f, "SCRUM-POISON", "reply-key-poison", &reply)
	requireStatus(t, resp, body, http.StatusCreated)
	if reply.Duplicate {
		t.Fatalf("retry after a rolled-back reply reported duplicate: %+v", reply)
	}
	if c := countReplyRows(t, f, "SCRUM-POISON"); c != (replyRowCounts{1, 1, 1, 1}) {
		t.Fatalf("rows after the retry = %+v, want one of each", c)
	}
}

func TestTicketFreezeActionUpdatesProjection(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/tickets/SCRUM-9/freeze"), decisionAuthSecret,
		map[string]any{"frozen_by": "operator", "merged_pr": map[string]any{"number": 230}}, nil)
	requireStatus(t, resp, body, http.StatusOK)
	var ticket struct {
		Frozen   bool            `json:"frozen"`
		MergedPR json.RawMessage `json:"merged_pr"`
	}
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/tickets/SCRUM-9"), nil, &ticket)
	requireStatus(t, resp, body, http.StatusOK)
	var merged map[string]any
	if err := json.Unmarshal(ticket.MergedPR, &merged); err != nil || !ticket.Frozen || merged["number"] != float64(230) {
		t.Fatalf("ticket freeze = %+v (merged_pr %s)", ticket, ticket.MergedPR)
	}
}

func TestMergedPRFactFreezesLinkedTicketProjection(t *testing.T) {
	f := newFixture(t)
	payload := json.RawMessage(`{"issue_key":"SCRUM-9","number":230,"url":"https://example.test/pull/230","merged_at":"2026-08-29T12:00:00Z"}`)
	delivery, err := f.store.DeliverSignalEvent(t.Context(), storepg.DeliverSignalEventInput{
		NamespaceID: f.nsID,
		Name:        "pr.merged",
		Payload:     payload,
		Emitter:     "pr-upkeep/sweep",
		SourceKey:   "github:agentculture/culture-nodes:pr:230:merged",
		Watermark:   json.RawMessage(`{"merged_at":"2026-08-29T12:00:00Z"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var ticket struct {
		Frozen   bool            `json:"frozen"`
		MergedPR json.RawMessage `json:"merged_pr"`
	}
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/tickets/SCRUM-9"), nil, &ticket)
	requireStatus(t, resp, body, http.StatusOK)
	var merged map[string]any
	if err := json.Unmarshal(ticket.MergedPR, &merged); err != nil || !ticket.Frozen ||
		merged["number"] != float64(230) || merged["issue_key"] != "SCRUM-9" {
		t.Fatalf("ticket after event %s = %+v (merged_pr %s)", delivery.Event.ID, ticket, ticket.MergedPR)
	}
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
