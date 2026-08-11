package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/agentculture/culture-nodes/internal/actors"
	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Task t4 (issue #19): cancelRun's REAP step must reap every leasable
// work_items row for a cancelled run -- not only 'ready' -- and its
// PROPAGATE step must best-effort ask any actor a node run was waiting on
// to stop.

// leasableWorkItemCount counts a run's work_items rows still in any state
// ClaimWork, ReclaimExpired, or the scheduler's wait/retry timer effect
// could act on: exactly the states cancelRun's REAP step must have emptied.
func leasableWorkItemCount(t *testing.T, f *fixture, runID string) int {
	t.Helper()
	var n int
	err := f.store.Pool().QueryRow(context.Background(), `
		SELECT count(*)
		FROM work_items wi
		JOIN node_runs nr ON nr.id = wi.node_run_id
		WHERE nr.run_id = $1 AND wi.state IN ('ready', 'waiting', 'leased')`, runID).Scan(&n)
	if err != nil {
		t.Fatalf("count leasable work items for run %s: %v", runID, err)
	}
	return n
}

// workItemState reads one work_items row's state directly.
func workItemState(t *testing.T, f *fixture, workID string) string {
	t.Helper()
	var state string
	if err := f.store.Pool().QueryRow(context.Background(),
		`SELECT state FROM work_items WHERE id = $1`, workID).Scan(&state); err != nil {
		t.Fatalf("read work item %s state: %v", workID, err)
	}
	return state
}

// cancelRequestEvents returns the data payload of every
// dev.culture.nodes.actor.cancel-requested event appended against a run, in
// sequence order.
func cancelRequestEvents(t *testing.T, f *fixture, runID string) []json.RawMessage {
	t.Helper()
	rows, err := f.store.Pool().Query(context.Background(), `
		SELECT data FROM events WHERE aggregate_id = $1 AND event_type = $2 ORDER BY sequence`,
		runID, apipkg.TypeActorCancelRequested)
	if err != nil {
		t.Fatalf("query cancel-request events for run %s: %v", runID, err)
	}
	defer rows.Close()

	var out []json.RawMessage
	for rows.Next() {
		var data json.RawMessage
		if err := rows.Scan(&data); err != nil {
			t.Fatalf("scan cancel-request event: %v", err)
		}
		out = append(out, data)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate cancel-request events: %v", err)
	}
	return out
}

// registerActor inserts a minimal actors row an actor reference resolves
// against, matching worker.DBRegistry's lookup ("current endpoint" = the
// newest revision for (namespace_id, actor_key)).
func registerActor(t *testing.T, f *fixture, actorKey, endpointURL string) {
	t.Helper()
	if _, err := f.store.Pool().Exec(context.Background(), `
		INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref)
		VALUES ($1, $2, $3, 1, 'agent', 'http', $4)`,
		store.NewULID(), f.nsID, actorKey, endpointURL,
	); err != nil {
		t.Fatalf("register actor %s: %v", actorKey, err)
	}
}

// TestCancelRunReapsLeasedWorkItem proves the REAP step's extension: a work
// item a worker currently holds a lease on (not merely 'ready') is cancelled
// by POST /v1alpha1/runs/{id}/cancel, closing the re-dispatch gap issue #19
// describes -- before this task, a 'leased' row was left alone entirely.
func TestCancelRunReapsLeasedWorkItem(t *testing.T) {
	f := newFixture(t)
	source := readFixtureWorkflow(t, "minimal.workflow.yaml")

	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(source)}, &published)
	requireStatus(t, resp, body, http.StatusCreated)

	var run apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{}`)}, &run)
	requireStatus(t, resp, body, http.StatusCreated)

	view := getRunView(t, f, run.ID)
	if len(view.NodeRuns) != 1 {
		t.Fatalf("got %d node runs, want 1", len(view.NodeRuns))
	}
	claimed := f.claim("worker-1", view.NodeRuns[0].ID)
	if got := workItemState(t, f, claimed.ID); got != "leased" {
		t.Fatalf("work item state before cancel = %q, want leased", got)
	}

	var cancelled apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/cancel"), nil, &cancelled)
	requireStatus(t, resp, body, http.StatusOK)

	if got := workItemState(t, f, claimed.ID); got != "cancelled" {
		t.Fatalf("previously leased work item %s state = %q, want cancelled", claimed.ID, got)
	}
	if n := leasableWorkItemCount(t, f, run.ID); n != 0 {
		t.Fatalf("run %s: %d work items still in a leasable state after cancel, want 0", run.ID, n)
	}
}

// TestCancelRunReapsWaitingWorkItemAndPropagatesCancel proves both the REAP
// step's 'waiting' coverage and the PROPAGATE step: a work item parked on an
// asynchronous actor invocation is cancelled, and the actor endpoint the
// invocation named receives a §13.6 cancel request, whose outcome is
// recorded as a dev.culture.nodes.actor.cancel-requested event.
func TestCancelRunReapsWaitingWorkItemAndPropagatesCancel(t *testing.T) {
	f := newFixture(t)

	var (
		mu          sync.Mutex
		cancelPaths []string
		cancelReqs  []actors.CancelRequest
	)
	actorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		cancelPaths = append(cancelPaths, r.URL.Path)
		var req actors.CancelRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		cancelReqs = append(cancelReqs, req)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer actorServer.Close()

	const actorKey = "test/waiting-actor"
	registerActor(t, f, actorKey, actorServer.URL)

	source := readFixtureWorkflow(t, "minimal.workflow.yaml")
	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(source)}, &published)
	requireStatus(t, resp, body, http.StatusCreated)

	var run apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{}`)}, &run)
	requireStatus(t, resp, body, http.StatusCreated)

	view := getRunView(t, f, run.ID)
	if len(view.NodeRuns) != 1 {
		t.Fatalf("got %d node runs, want 1", len(view.NodeRuns))
	}
	nodeRun := view.NodeRuns[0]
	claimed := f.claim("worker-1", nodeRun.ID)

	invocationID := "ext-inv-" + store.NewULID()
	ctx := context.Background()
	if err := f.store.StartAsyncWait(ctx, storepg.StartAsyncWaitInput{
		WorkID:               claimed.ID,
		WorkerID:             "worker-1",
		FencingToken:         claimed.FencingToken,
		Attempt:              int(claimed.Attempt),
		NamespaceID:          f.nsID,
		RunID:                run.ID,
		NodeRunID:            nodeRun.ID,
		NodeID:               nodeRun.NodeID,
		AttemptID:            store.NewULID(),
		ActorRef:             actorKey,
		InvocationID:         invocationID,
		SupportsCancellation: true,
	}); err != nil {
		t.Fatalf("StartAsyncWait: %v", err)
	}
	if got := workItemState(t, f, claimed.ID); got != "waiting" {
		t.Fatalf("work item state after StartAsyncWait = %q, want waiting", got)
	}

	var cancelled apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/cancel"), nil, &cancelled)
	requireStatus(t, resp, body, http.StatusOK)

	// REAP: the parked item is cancelled, not left 'waiting' for a fired
	// deadline timer to return to 'ready' and a worker to re-dispatch.
	if got := workItemState(t, f, claimed.ID); got != "cancelled" {
		t.Fatalf("previously waiting work item %s state = %q, want cancelled", claimed.ID, got)
	}
	if n := leasableWorkItemCount(t, f, run.ID); n != 0 {
		t.Fatalf("run %s: %d work items still in a leasable state after cancel, want 0", run.ID, n)
	}

	// PROPAGATE: the fake actor endpoint received exactly one §13.6 cancel
	// request for this invocation.
	mu.Lock()
	gotPaths := append([]string(nil), cancelPaths...)
	gotReqs := append([]actors.CancelRequest(nil), cancelReqs...)
	mu.Unlock()

	if len(gotPaths) != 1 {
		t.Fatalf("actor endpoint received %d cancel requests, want 1: %v", len(gotPaths), gotPaths)
	}
	if want := actors.InvocationPath + "/" + invocationID + "/cancel"; gotPaths[0] != want {
		t.Fatalf("cancel request path = %q, want %q", gotPaths[0], want)
	}
	if gotReqs[0].InvocationID != invocationID {
		t.Fatalf("cancel request invocation_id = %q, want %q", gotReqs[0].InvocationID, invocationID)
	}

	// One cancel-request event was appended, recording a "sent" outcome.
	events := cancelRequestEvents(t, f, run.ID)
	if len(events) != 1 {
		t.Fatalf("got %d cancel-request events, want 1", len(events))
	}
	var payload struct {
		InvocationID string `json:"invocation_id"`
		ActorRef     string `json:"actor_ref"`
		Outcome      string `json:"outcome"`
	}
	if err := json.Unmarshal(events[0], &payload); err != nil {
		t.Fatalf("unmarshal cancel-request event data: %v", err)
	}
	if payload.InvocationID != invocationID {
		t.Fatalf("event invocation_id = %q, want %q", payload.InvocationID, invocationID)
	}
	if payload.ActorRef != actorKey {
		t.Fatalf("event actor_ref = %q, want %q", payload.ActorRef, actorKey)
	}
	if payload.Outcome != "sent" {
		t.Fatalf("event outcome = %q, want sent", payload.Outcome)
	}
}

// TestCancelRunPropagateSkipsInvocationWithNoActorRef proves a Cancel
// failure (here, an actor reference the registry cannot resolve) never
// fails the cancel response itself -- §13.6's "workflow state does not
// depend on an external process acknowledging cancellation" applies to
// resolution failures too, not only to a reachable-but-refusing actor.
func TestCancelRunPropagateSkipsUnresolvableActor(t *testing.T) {
	f := newFixture(t)
	source := readFixtureWorkflow(t, "minimal.workflow.yaml")

	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(source)}, &published)
	requireStatus(t, resp, body, http.StatusCreated)

	var run apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{}`)}, &run)
	requireStatus(t, resp, body, http.StatusCreated)

	view := getRunView(t, f, run.ID)
	nodeRun := view.NodeRuns[0]
	claimed := f.claim("worker-1", nodeRun.ID)

	invocationID := "ext-inv-" + store.NewULID()
	ctx := context.Background()
	if err := f.store.StartAsyncWait(ctx, storepg.StartAsyncWaitInput{
		WorkID:               claimed.ID,
		WorkerID:             "worker-1",
		FencingToken:         claimed.FencingToken,
		Attempt:              int(claimed.Attempt),
		NamespaceID:          f.nsID,
		RunID:                run.ID,
		NodeRunID:            nodeRun.ID,
		NodeID:               nodeRun.NodeID,
		AttemptID:            store.NewULID(),
		ActorRef:             "test/never-registered",
		InvocationID:         invocationID,
		SupportsCancellation: true,
	}); err != nil {
		t.Fatalf("StartAsyncWait: %v", err)
	}

	var cancelled apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/cancel"), nil, &cancelled)
	requireStatus(t, resp, body, http.StatusOK)
	if cancelled.State != "cancelled" {
		t.Fatalf("state = %q, want cancelled", cancelled.State)
	}

	if got := workItemState(t, f, claimed.ID); got != "cancelled" {
		t.Fatalf("work item state = %q, want cancelled", got)
	}

	events := cancelRequestEvents(t, f, run.ID)
	if len(events) != 1 {
		t.Fatalf("got %d cancel-request events, want 1", len(events))
	}
	var payload struct {
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal(events[0], &payload); err != nil {
		t.Fatalf("unmarshal cancel-request event data: %v", err)
	}
	if payload.Outcome != "failed" {
		t.Fatalf("event outcome = %q, want failed", payload.Outcome)
	}
}
