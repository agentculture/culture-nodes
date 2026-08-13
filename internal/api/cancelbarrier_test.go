package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/engine"
)

// TestCancelRunReapsAParkedJoinBarrier is row T11 of the parallel-tokens
// design's t20 matrix (issue #43), driven over the real HTTP cancel surface.
//
// The parallel-tokens design predicted that cancelRun would need NO change
// for barriers: its REAP step already cancels every node run whose status is
// not already terminal, and `waiting_join` is deliberately non-terminal, so
// a parked barrier is caught by the same UPDATE that catches a
// `waiting_external` park. This test is what turns that prediction into a
// checked fact — the failure it guards against is a future narrowing of that
// UPDATE to an explicit state list, which would silently strand barriers.
func TestCancelRunReapsAParkedJoinBarrier(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	source := readFixtureWorkflow(t, "parallel-join-ok.workflow.yaml")

	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(source)}, &published)
	requireStatus(t, resp, body, http.StatusCreated)

	var run apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{}`)}, &run)
	requireStatus(t, resp, body, http.StatusCreated)

	// Complete the entry parallel node the way the worker's dispatch seam
	// does, so the fan-out is real committed state rather than hand-written
	// rows.
	fanNodeRun := getRunView(t, f, run.ID).NodeRuns[0].ID
	split := f.completeThroughClaim(ctx, fanNodeRun, "split", `{}`)
	if split.Split == nil || split.Split.Cardinality != 2 {
		t.Fatalf("split = %+v, want cardinality 2", split.Split)
	}

	// One branch arrives, parking the barrier; the other stays live.
	first := f.completeThroughClaim(ctx, split.Split.Branches[0].NodeRunID, "clean", `{}`)
	if first.JoinNodeRunID == "" || first.JoinSatisfied {
		t.Fatalf("first arrival = %+v, want a parked barrier", first)
	}
	liveBranch := split.Split.Branches[1].NodeRunID

	if got := nodeRunStatus(t, f, first.JoinNodeRunID); got != string(engine.NodeRunWaitingJoin) {
		t.Fatalf("barrier status = %q, want waiting_join", got)
	}

	var cancelled apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/cancel"), nil, &cancelled)
	requireStatus(t, resp, body, http.StatusOK)

	if got := runStatus(t, f, run.ID); got != "cancelled" {
		t.Errorf("run status = %q, want cancelled", got)
	}
	if got := nodeRunStatus(t, f, first.JoinNodeRunID); got != "cancelled" {
		t.Errorf("barrier status after cancel = %q, want cancelled — a parked barrier is non-terminal state the REAP must catch", got)
	}
	if got := nodeRunStatus(t, f, liveBranch); got != "cancelled" {
		t.Errorf("live branch status after cancel = %q, want cancelled", got)
	}

	var activeTokens int
	if err := f.store.Pool().QueryRow(ctx,
		`SELECT COUNT(*)::int FROM tokens WHERE run_id = $1 AND state = 'active'`, run.ID).Scan(&activeTokens); err != nil {
		t.Fatalf("count active tokens: %v", err)
	}
	if activeTokens != 0 {
		t.Errorf("active tokens after cancel = %d, want 0 — including the barrier's own", activeTokens)
	}
}

// completeThroughClaim claims a node run's work item through the real
// claiming path and completes it with the given domain outcome, the same
// hand-operated-worker shape the rest of this package uses.
func (f *fixture) completeThroughClaim(ctx context.Context, nodeRunID, outcome, output string) engine.CompletionResult {
	f.t.Helper()
	claimed := f.claim("worker-barrier", nodeRunID)
	result, err := f.api.Engine.CompleteAttempt(ctx, engine.CompletionRequest{
		WorkID:       claimed.ID,
		WorkerID:     "worker-barrier",
		FencingToken: claimed.FencingToken,
		Attempt:      int(claimed.Attempt),
		TechStatus:   engine.StatusSucceeded,
		Outcome:      outcome,
		Output:       json.RawMessage(output),
	})
	if err != nil {
		f.t.Fatalf("CompleteAttempt for node run %s: %v", nodeRunID, err)
	}
	return result
}

func nodeRunStatus(t *testing.T, f *fixture, nodeRunID string) string {
	t.Helper()
	var status string
	if err := f.store.Pool().QueryRow(context.Background(),
		`SELECT status FROM node_runs WHERE id = $1`, nodeRunID).Scan(&status); err != nil {
		t.Fatalf("read node run %s: %v", nodeRunID, err)
	}
	return status
}
