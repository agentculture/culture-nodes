package ticketreport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
	"github.com/agentculture/culture-nodes/internal/ticketreport"
)

const reportWorkflow = `apiVersion: nodes.culture.dev/v1alpha1
kind: Workflow
metadata: {name: ticket-report-test, version: 1.0.0, ownerRef: team/platform-ai}
spec:
  entry: work
  contract:
    input: {schema: {type: object}}
    output: {schema: {type: object}}
  limits: {maxDuration: 1h, maxTransitions: 2, maxVisitsPerNode: 1, maxParallelTokens: 1}
  nodes:
    work:
      kind: agent
      ownerRef: team/platform-ai
      uses: actor://company/developer@sha256:aaaaaa
      contract: {outcomes: {completed: {schema: {type: object}}}}
    done:
      kind: end
      ownerRef: team/platform-ai
      output: {from: /nodes/work/output}
  edges:
    - {from: work.completed, to: done}
`

type recordingTransport struct {
	status int
	bodies [][]byte
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	r.bodies = append(r.bodies, body)
	status := r.status
	if status == 0 {
		status = http.StatusOK
	}
	response := `{"outcome":"completed","output":{}}`
	if status >= 400 {
		response = `{"message":"bridge unavailable"}`
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
}

type reportFixture struct {
	t         *testing.T
	ctx       context.Context
	store     *storepg.Store
	namespace string
	runID     string
	eventID   string
	transport *recordingTransport
	dispatch  *ticketreport.Dispatcher
}

func newReportFixture(t *testing.T, status int) *reportFixture {
	t.Helper()
	s := pgtest.RequireStore(t, testStore)
	ns := pgtest.MustNamespace(t, s, "ticketreport")
	cw, diags, err := compiler.Compile([]byte(reportWorkflow), compiler.FormatYAML)
	if err != nil {
		t.Fatalf("compile fixture: %v (%v)", err, diags)
	}
	eng, err := storepg.NewEngine(s, ns.ID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := eng.CreateRun(context.Background(), cw, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := s.DeliverSignalEvent(context.Background(), storepg.DeliverSignalEventInput{
		NamespaceID: ns.ID, Name: "jira.comment", Payload: json.RawMessage(`{}`), Emitter: "test",
		Subject: "SCRUM-9", SourceKey: "jira:cloud:SCRUM-9:comment:1", Watermark: json.RawMessage(`{"seq":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	actorID := "jira_report_" + store.NewULID()
	if _, err := s.Pool().Exec(context.Background(), `INSERT INTO actors
		(id,namespace_id,actor_key,revision,kind,protocol,endpoint_ref)
		VALUES ($1,$2,$3,1,'agent','http','http://jira-bridge.test')`, actorID, ns.ID, storepg.JiraTicketReporterActorKey); err != nil {
		t.Fatal(err)
	}
	transport := &recordingTransport{status: status}
	client := actors.NewClient(actors.WithHTTPClient(&http.Client{Transport: transport}), actors.WithMaxRequests(1))
	return &reportFixture{t: t, ctx: context.Background(), store: s, namespace: ns.ID, runID: run.ID,
		eventID: delivery.Event.ID, transport: transport, dispatch: ticketreport.New(s, client)}
}

func (f *reportFixture) insert(id, phase string, available time.Time) {
	f.t.Helper()
	payload, _ := json.Marshal(map[string]string{"issue": "SCRUM-9", "comment": phase + " report"})
	if _, err := f.store.Pool().Exec(f.ctx, `INSERT INTO jira_ticket_report_outbox
		(id,namespace_id,run_id,trigger_event_id,phase,target_actor_key,issue_key,payload,available_at)
		VALUES ($1,$2,$3,$4,$5,$6,'SCRUM-9',$7,$8)`, id, f.namespace, f.runID, f.eventID, phase,
		storepg.JiraTicketReporterActorKey, payload, available); err != nil {
		f.t.Fatal(err)
	}
}

func TestStartAndFinishReportsForOneRun(t *testing.T) {
	f := newReportFixture(t, http.StatusOK)
	f.insert("report-01-start", "start", time.Now().Add(-time.Second))
	f.insert("report-02-finish", "finish", time.Now().Add(-time.Second))
	if err := f.dispatch.Run(f.ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.transport.bodies) != 2 {
		t.Fatalf("bridge invocations = %d, want start and finish", len(f.transport.bodies))
	}
	if !bytes.Contains(f.transport.bodies[0], []byte("start report")) || !bytes.Contains(f.transport.bodies[1], []byte("finish report")) {
		t.Fatalf("invocation order/bodies = %q", f.transport.bodies)
	}
}

func TestReportsDeliverThroughJiraBridgeOnly(t *testing.T) {
	f := newReportFixture(t, http.StatusOK)
	f.insert("report-bridge-only", "start", time.Now().Add(-time.Second))
	if err := f.dispatch.Run(f.ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.transport.bodies) != 1 || !bytes.Contains(f.transport.bodies[0], []byte(`"verb":"post_comment"`)) {
		t.Fatalf("jira bridge calls = %q, want one post_comment", f.transport.bodies)
	}
}

func TestRetryBackoffEndsInTerminalFailure(t *testing.T) {
	f := newReportFixture(t, http.StatusInternalServerError)
	f.insert("report-poison", "start", time.Now().Add(-time.Second))
	for attempt := 1; attempt <= 5; attempt++ {
		if err := f.dispatch.Run(f.ctx); err == nil {
			t.Fatalf("attempt %d returned nil error", attempt)
		}
		var status string
		var attempts int
		var available time.Time
		if err := f.store.Pool().QueryRow(f.ctx, `SELECT status,attempts,available_at FROM jira_ticket_report_outbox WHERE id='report-poison'`).Scan(&status, &attempts, &available); err != nil {
			t.Fatal(err)
		}
		if attempts != attempt {
			t.Fatalf("attempts = %d, want %d", attempts, attempt)
		}
		if attempt < 5 {
			if status != "pending" || !available.After(time.Now()) {
				t.Fatalf("attempt %d status/available = %s/%s, want pending future backoff", attempt, status, available)
			}
			if _, err := f.store.Pool().Exec(f.ctx, `UPDATE jira_ticket_report_outbox SET available_at=now()-interval '1 second' WHERE id='report-poison'`); err != nil {
				t.Fatal(err)
			}
		} else if status != "failed" {
			t.Fatalf("terminal status = %q, want failed", status)
		}
	}
}

func TestFinishWaitsWhenStartHasNotBeenDelivered(t *testing.T) {
	f := newReportFixture(t, http.StatusOK)
	f.insert("report-start-blocked", "start", time.Now().Add(time.Hour))
	f.insert("report-finish-ready", "finish", time.Now().Add(-time.Second))
	if err := f.dispatch.Run(f.ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.transport.bodies) != 0 {
		t.Fatalf("finish delivered before start: %q", f.transport.bodies)
	}
	if _, err := f.store.Pool().Exec(f.ctx, `UPDATE jira_ticket_report_outbox SET available_at=now()-interval '1 second' WHERE id='report-start-blocked'`); err != nil {
		t.Fatal(err)
	}
	if err := f.dispatch.Run(f.ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.transport.bodies) != 2 || !bytes.Contains(f.transport.bodies[0], []byte("start report")) || !bytes.Contains(f.transport.bodies[1], []byte("finish report")) {
		t.Fatalf("delivery after start became due = %q", f.transport.bodies)
	}
}
