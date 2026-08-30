package humanfanout_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/humanfanout"
	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// The delivery half of task t11: what a bridge actually receives. The engine
// tests next door prove the right rows are QUEUED; these prove each row
// reaches the right bridge with the payload the engine composed, unchanged.

const fanOutWorkflow = `apiVersion: nodes.culture.dev/v1alpha1
kind: Workflow
metadata: {name: human-fanout-test, version: 1.0.0, ownerRef: team/platform-ai}
spec:
  entry: review
  contract:
    input: {schema: {type: object}}
    output: {schema: {type: object}}
  limits: {maxDuration: 1h, maxTransitions: 2, maxVisitsPerNode: 1, maxParallelTokens: 1}
  nodes:
    review:
      kind: approval
      ownerRef: team/platform-ai
      approverRef: group/platform-maintainers
      input: {from: /run/input}
    done:
      kind: end
      ownerRef: team/platform-ai
      output: {from: /run/input}
  edges:
    - {from: review.approved, to: done}
    - {from: review.rejected, to: done}
    - {from: review.expired, to: done}
`

// invocation is one POST a bridge received, keyed by which bridge it went to.
type invocation struct {
	Host  string
	Input map[string]any
}

type recordingTransport struct {
	status      int
	invocations []invocation
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	var decoded struct {
		Input map[string]any `json:"input"`
	}
	_ = json.Unmarshal(body, &decoded)
	r.invocations = append(r.invocations, invocation{Host: req.URL.Host, Input: decoded.Input})
	status := r.status
	if status == 0 {
		status = http.StatusOK
	}
	response := `{"outcome":"completed","output":{}}`
	if status >= 400 {
		response = `{"message":"bridge unavailable"}`
	}
	return &http.Response{
		StatusCode: status, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(response)), Request: req,
	}, nil
}

type laneFixture struct {
	t         *testing.T
	ctx       context.Context
	store     *storepg.Store
	namespace string
	taskID    string
	transport *recordingTransport
	lane      *humanfanout.Lane
}

func newLaneFixture(t *testing.T, status int, runInput string) *laneFixture {
	t.Helper()
	s := pgtest.RequireStore(t, testStore)
	t.Setenv(engine.UIBaseURLEnv, "http://thor:18080")
	ns := pgtest.MustNamespace(t, s, "humanfanout")

	cw, diags, err := compiler.Compile([]byte(fanOutWorkflow), compiler.FormatYAML)
	if err != nil {
		t.Fatalf("compile fixture: %v (%v)", err, diags)
	}
	eng, err := storepg.NewEngine(s, ns.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run, err := eng.CreateRun(ctx, cw, json.RawMessage(runInput))
	if err != nil {
		t.Fatal(err)
	}

	// Two distinct bridges at two distinct hosts, so "the Jira intents went to
	// the Jira bridge and the notify post went to the notify bridge" is
	// checkable rather than asserted.
	registerBridge(t, s, ns.ID, storepg.JiraTicketReporterActorKey, "http://jira-bridge.test")
	registerBridge(t, s, ns.ID, storepg.HumanTaskNotifierActorKey, "http://notify-bridge.test")

	var taskID string
	if err := s.Pool().QueryRow(ctx,
		`SELECT id FROM human_tasks WHERE run_id=$1 AND status='pending'`, run.ID).Scan(&taskID); err != nil {
		t.Fatalf("read pending human task: %v", err)
	}

	transport := &recordingTransport{status: status}
	client := actors.NewClient(actors.WithHTTPClient(&http.Client{Transport: transport}), actors.WithMaxRequests(1))
	return &laneFixture{
		t: t, ctx: ctx, store: s, namespace: ns.ID, taskID: taskID,
		transport: transport, lane: humanfanout.New(s, client),
	}
}

func registerBridge(t *testing.T, s *storepg.Store, namespaceID, actorKey, endpoint string) {
	t.Helper()
	if _, err := s.Pool().Exec(context.Background(), `INSERT INTO actors
		(id,namespace_id,actor_key,revision,kind,protocol,endpoint_ref)
		VALUES ($1,$2,$3,1,'agent','http',$4)`,
		"bridge_"+store.NewULID(), namespaceID, actorKey, endpoint); err != nil {
		t.Fatalf("register %s: %v", actorKey, err)
	}
}

func (f *laneFixture) statuses() map[string]string {
	f.t.Helper()
	rows, err := f.store.Pool().Query(f.ctx,
		`SELECT channel, status FROM human_task_fanout_outbox WHERE human_task_id=$1`, f.taskID)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var channel, status string
		if err := rows.Scan(&channel, &status); err != nil {
			f.t.Fatal(err)
		}
		out[channel] = status
	}
	return out
}

func TestJiraIntentsGoToTheJiraBridgeAndTheNotifyPostToTheNotifyBridge(t *testing.T) {
	f := newLaneFixture(t, http.StatusOK, `{"source":"jira","id":"SCRUM-6"}`)

	if err := f.lane.Run(f.ctx); err != nil {
		t.Fatalf("lane run: %v", err)
	}
	if len(f.transport.invocations) != 3 {
		t.Fatalf("bridge invocations = %d, want 3: %+v", len(f.transport.invocations), f.transport.invocations)
	}

	byVerb := map[string]invocation{}
	for _, inv := range f.transport.invocations {
		verb, _ := inv.Input["verb"].(string)
		if verb == "" {
			verb = "notify"
		}
		byVerb[verb] = inv
	}
	for verb, wantHost := range map[string]string{
		"post_comment":     "jira-bridge.test",
		"transition_issue": "jira-bridge.test",
		"notify":           "notify-bridge.test",
	} {
		inv, ok := byVerb[verb]
		if !ok {
			t.Fatalf("no %s invocation among %+v", verb, f.transport.invocations)
		}
		if inv.Host != wantHost {
			t.Errorf("%s went to %s, want %s", verb, inv.Host, wantHost)
		}
	}

	// The transition names the status a pending decision parks a ticket at,
	// and the bridge's own allowlist is what decides whether it is permitted.
	if got := byVerb["transition_issue"].Input["target"]; got != engine.JiraDecisionStatus {
		t.Errorf("transition target = %v, want %q", got, engine.JiraDecisionStatus)
	}
	// The notify bridge reads title/description/fields, not a verb — the
	// payload is passed through exactly as the engine composed it.
	if title, _ := byVerb["notify"].Input["title"].(string); !strings.Contains(title, "approval") {
		t.Errorf("notify title = %q, want the task kind in it", title)
	}

	for channel, status := range f.statuses() {
		if status != "published" {
			t.Errorf("%s row status = %s, want published", channel, status)
		}
	}
}

// A second tick sends nothing: published rows leave the eligible set, so the
// same task cannot be announced twice by re-running the lane.
func TestASecondTickSendsNothingMore(t *testing.T) {
	f := newLaneFixture(t, http.StatusOK, `{"source":"jira","id":"SCRUM-6"}`)
	if err := f.lane.Run(f.ctx); err != nil {
		t.Fatalf("first run: %v", err)
	}
	sent := len(f.transport.invocations)
	if err := f.lane.Run(f.ctx); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(f.transport.invocations) != sent {
		t.Errorf("second tick sent %d more invocations, want none",
			len(f.transport.invocations)-sent)
	}
}

// A refusing bridge is backed off, not retried in a hot loop, and the failure
// is reported rather than swallowed.
func TestAFailingBridgeIsBackedOffAndReported(t *testing.T) {
	f := newLaneFixture(t, http.StatusInternalServerError, `{"source":"jira","id":"SCRUM-6"}`)
	err := f.lane.Run(f.ctx)
	if err == nil {
		t.Fatal("a lane whose bridge refused every message returned no error")
	}
	for channel, status := range f.statuses() {
		if status != "pending" {
			t.Errorf("%s row status = %s, want it left pending for a later tick", channel, status)
		}
	}
	var eligible int
	if e := f.store.Pool().QueryRow(f.ctx,
		`SELECT count(*) FROM human_task_fanout_outbox
		 WHERE human_task_id=$1 AND status='pending' AND available_at<=now()`, f.taskID).Scan(&eligible); e != nil {
		t.Fatal(e)
	}
	if eligible != 0 {
		t.Errorf("%d row(s) are still immediately eligible after a failure; the backoff did not apply", eligible)
	}
}
