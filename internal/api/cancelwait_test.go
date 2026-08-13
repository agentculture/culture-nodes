package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Task t9 (issue #39): cancelRun's REAP step must retire a durably-waiting
// run's pending wait timer along with its work item. A parked wait holds no
// lease and no invocation — the pending timer is the ONLY thing that will
// ever wake it — so a cancel that left the timer pending would have the
// scheduler flip the cancelled run's work item back to 'ready' when it
// fires (the wait effect's UPDATE targets `state <> 'completed'`), exactly
// the re-dispatch-after-cancel gap issue #19 closed for actor invocations.

func timerRow(t *testing.T, f *fixture, timerID string) (status string, kind string) {
	t.Helper()
	if err := f.store.Pool().QueryRow(context.Background(),
		`SELECT status, timer_kind FROM timers WHERE id = $1`, timerID,
	).Scan(&status, &kind); err != nil {
		t.Fatalf("read timer %s: %v", timerID, err)
	}
	return status, kind
}

func runStatus(t *testing.T, f *fixture, runID string) string {
	t.Helper()
	var status string
	if err := f.store.Pool().QueryRow(context.Background(),
		`SELECT status FROM runs WHERE id = $1`, runID).Scan(&status); err != nil {
		t.Fatalf("read run %s: %v", runID, err)
	}
	return status
}

// TestCancelRunReapsPendingWaitTimer parks a run on a durable wait exactly
// as the worker's wait dispatch does (Store.StartDurableWait under the real
// claim's fencing tuple), then cancels the run over the HTTP surface and
// proves the whole park is retired together: run cancelled, work item
// cancelled, wait timer canceled — terminated cleanly, with nothing left
// pending to fire.
func TestCancelRunReapsPendingWaitTimer(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	source := readFixtureWorkflow(t, "wait.workflow.yaml")

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
	nodeRunID := view.NodeRuns[0].ID

	claimed := f.claim("worker-1", nodeRunID)
	timerID := "wait-" + nodeRunID
	if err := f.store.StartDurableWait(ctx, storepg.StartDurableWaitInput{
		WorkID:       claimed.ID,
		WorkerID:     "worker-1",
		FencingToken: claimed.FencingToken,
		Attempt:      int(claimed.Attempt),
		NamespaceID:  f.nsID,
		RunID:        run.ID,
		NodeRunID:    nodeRunID,
		NodeID:       "pause",
		AttemptID:    "att_" + store.NewULID(),
		TimerID:      timerID,
		FireAt:       time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("StartDurableWait: %v", err)
	}

	// Parked: leaseless waiting work item, pending wait timer.
	if got := workItemState(t, f, claimed.ID); got != "waiting" {
		t.Fatalf("work item state after park = %q, want waiting", got)
	}
	if status, kind := timerRow(t, f, timerID); status != "pending" || kind != "wait" {
		t.Fatalf("timer after park = (%s, %s), want (pending, wait)", status, kind)
	}

	var cancelled apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/cancel"), nil, &cancelled)
	requireStatus(t, resp, body, http.StatusOK)

	if got := runStatus(t, f, run.ID); got != "cancelled" {
		t.Errorf("run status after cancel = %q, want cancelled", got)
	}
	if got := workItemState(t, f, claimed.ID); got != "cancelled" {
		t.Errorf("work item state after cancel = %q, want cancelled", got)
	}
	if status, _ := timerRow(t, f, timerID); status != "canceled" {
		t.Errorf("wait timer status after cancel = %q, want canceled (nothing may ever fire for a dead run)", status)
	}
	if n := leasableWorkItemCount(t, f, run.ID); n != 0 {
		t.Errorf("leasable work items after cancel = %d, want 0", n)
	}
}
