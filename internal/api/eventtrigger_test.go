package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
)

const triggeredWorkflow = `apiVersion: nodes.culture.dev/v1alpha1
kind: Workflow
metadata: {name: event-start, version: 1.0.0, ownerRef: team/platform-ai}
spec:
  entry: start
  triggers:
    - onEvent: pull-request
      when: event.payload.action == "opened"
  contract:
    input: {schema: {type: object, required: [action]}}
    output: {schema: {type: object}}
  nodes:
    start:
      kind: agent
      ownerRef: team/platform-ai
      uses: actor://company/start@sha256:aaaaaa
      contract: {outcomes: {completed: {schema: {type: object}}}}
    finish: {kind: end, ownerRef: team/platform-ai, output: {from: /nodes/start/output}}
  edges: [{from: start.completed, to: finish}]
`

func publishTriggerWorkflow(t *testing.T, f *fixture, source string) {
	t.Helper()
	var out apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: source}, &out)
	requireStatus(t, resp, body, http.StatusCreated)
}

func countRuns(t *testing.T, f *fixture) int {
	t.Helper()
	var n int
	if err := f.store.Pool().QueryRow(context.Background(), `SELECT count(*) FROM runs WHERE namespace_id = $1`, f.nsID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestConditionedTriggerRecordsNonMatchWithoutCreatingOrResuming(t *testing.T) {
	f := newFixtureWithEventAuth(t, eventTokenSecret)
	publishTriggerWorkflow(t, f, triggeredWorkflow)
	out := deliver(t, f, "pull-request", json.RawMessage(`{"action":"closed"}`))
	if len(out.Triggered) != 0 || len(out.Resumed) != 0 {
		t.Fatalf("delivery acted on declined handler: %+v", out)
	}
	if countRuns(t, f) != 0 {
		t.Fatal("declined handler created a run")
	}
	if _, ok, err := f.store.SignalEventByID(context.Background(), out.Event.ID); err != nil || !ok {
		t.Fatalf("event was not recorded: ok=%v err=%v", ok, err)
	}
}

func TestTriggerCreatesRunAndNewerVersionCanRemoveIt(t *testing.T) {
	f := newFixtureWithEventAuth(t, eventTokenSecret)
	publishTriggerWorkflow(t, f, triggeredWorkflow)
	out := deliver(t, f, "pull-request", json.RawMessage(`{"action":"opened"}`))
	if len(out.Triggered) != 1 || countRuns(t, f) != 1 {
		t.Fatalf("delivery=%+v runs=%d", out, countRuns(t, f))
	}
	without := strings.Replace(triggeredWorkflow, "  triggers:\n    - onEvent: pull-request\n      when: event.payload.action == \"opened\"\n", "", 1)
	publishTriggerWorkflow(t, f, without)
	out = deliver(t, f, "pull-request", json.RawMessage(`{"action":"opened"}`))
	if len(out.Triggered) != 0 || countRuns(t, f) != 1 {
		t.Fatalf("removed trigger still created: delivery=%+v runs=%d", out, countRuns(t, f))
	}
}
