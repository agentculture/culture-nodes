package worker_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// The split/join dispatch seams end to end (issue #43, parallel-tokens
// design D2). Everything below runs through the real worker loop against
// real PostgreSQL: nothing here hand-writes a work item or hand-completes a
// node run, because the point of dispatching parallel and join as ORDINARY
// work is that they inherit fencing, leases, and restart durability from the
// paths every other kind already uses — and a test that bypassed the claim
// loop would prove none of it.

// TestWorkerDrivesASplitAndJoinToCompletion is the whole shape in one run:
// the entry parallel node is claimed and completed with the kind-implied
// outcome `split`, its fan-out commits two branches, a real HTTP actor
// answers both, the second arrival fires the barrier, and the join node run
// — which was NOT claimable until then — is claimed and completed with the
// aggregated arrival array.
func TestWorkerDrivesASplitAndJoinToCompletion(t *testing.T) {
	var invocations int64
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		n := atomic.AddInt64(&invocations, 1)
		writeSyncResult(w, "completed", fmt.Sprintf(`{"score":0.9,"summary":"branch %d"}`, n))
	})

	run := h.createRun("parallel.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	final := h.run(run.ID)
	if final.State != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", final.State, h.workerErrors())
	}

	// Both branches really ran: one actor invocation each. The parallel and
	// join nodes invoke nothing — they are routers.
	if got := atomic.LoadInt64(&invocations); got != 2 {
		t.Errorf("actor invocations = %d, want 2 (one per branch)", got)
	}

	// The run's output is the join's aggregated arrival array (design D5),
	// bound through the end node's /nodes/gather/output.
	var out struct {
		Arrivals []struct {
			FromNode string          `json:"from_node"`
			TokenID  string          `json:"token_id"`
			Outcome  string          `json:"outcome"`
			Output   json.RawMessage `json:"output"`
		} `json:"arrivals"`
		Policy      string `json:"policy"`
		Cardinality int    `json:"cardinality"`
	}
	if err := json.Unmarshal(final.Output, &out); err != nil {
		t.Fatalf("run output is not the join's aggregate (%s): %v", final.Output, err)
	}
	if out.Policy != "all" || out.Cardinality != 2 {
		t.Errorf("join output policy/cardinality = %q/%d, want all/2", out.Policy, out.Cardinality)
	}
	if len(out.Arrivals) != 2 {
		t.Fatalf("join output carried %d arrivals, want 2: %s", len(out.Arrivals), final.Output)
	}
	seen := map[string]bool{}
	for _, a := range out.Arrivals {
		if a.Outcome != "completed" {
			t.Errorf("arrival from %s has outcome %q, want completed", a.FromNode, a.Outcome)
		}
		if a.TokenID == "" {
			t.Errorf("arrival from %s carries no token id", a.FromNode)
		}
		if len(a.Output) == 0 || string(a.Output) == "null" {
			t.Errorf("arrival from %s carries no branch output", a.FromNode)
		}
		seen[a.FromNode] = true
	}
	if !seen["lint"] || !seen["test"] {
		t.Errorf("arrivals came from %v, want both lint and test", seen)
	}

	// Every dispatched work item finished cleanly — including the parallel
	// node's and the join's, which is what "they dispatch as ordinary work"
	// has to mean to be worth the extra queue hop.
	states := h.workItemStates(run.ID)
	for _, node := range []string{"fan", "lint", "test", "gather"} {
		if states[node] != "completed" {
			t.Errorf("work item for node %q is %q, want completed", node, states[node])
		}
	}

	// The barrier left exactly one join node run behind, with its arrivals
	// recorded and its own token consumed.
	var joinNodeRuns, arrivals, activeTokens int
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT COUNT(*)::int FROM node_runs WHERE run_id = $1 AND node_key = 'gather'`, run.ID).Scan(&joinNodeRuns); err != nil {
		t.Fatalf("count join node runs: %v", err)
	}
	if joinNodeRuns != 1 {
		t.Errorf("gather node runs = %d, want 1 — a join instance is one execution fed by K branches", joinNodeRuns)
	}
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT COUNT(*)::int FROM join_arrivals AS ja
		JOIN node_runs AS nr ON nr.id = ja.join_node_run_id
		WHERE nr.run_id = $1`, run.ID).Scan(&arrivals); err != nil {
		t.Fatalf("count arrivals: %v", err)
	}
	if arrivals != 2 {
		t.Errorf("recorded arrivals = %d, want 2", arrivals)
	}
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT COUNT(*)::int FROM tokens WHERE run_id = $1 AND state = 'active'`, run.ID).Scan(&activeTokens); err != nil {
		t.Fatalf("count active tokens: %v", err)
	}
	if activeTokens != 0 {
		t.Errorf("active tokens after completion = %d, want 0", activeTokens)
	}
}

// A parked barrier must be invisible to the claim loop: waiting_join has no
// work item, so a worker ticking while one branch is still running has
// nothing to claim at the join and cannot dispatch it early.
func TestParkedBarrierIsNotClaimable(t *testing.T) {
	release := make(chan struct{})
	var invocations int64
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		// The second branch blocks until the test releases it, so the run
		// spends real time with the barrier parked at one of two arrivals.
		if atomic.AddInt64(&invocations, 1) == 2 {
			<-release
		}
		writeSyncResult(w, "completed", `{"score":0.9}`)
	})

	run := h.createRun("parallel.workflow.yaml", `{"subject":"widget"}`)

	// Tick until a barrier exists, then assert it has no claimable work.
	parked := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && !parked {
		go func() { _, _ = h.worker.Tick(h.ctx) }()
		time.Sleep(50 * time.Millisecond)
		var count int
		if err := h.store.Pool().QueryRow(h.ctx,
			`SELECT COUNT(*)::int FROM node_runs WHERE run_id = $1 AND status = 'waiting_join'`, run.ID).Scan(&count); err != nil {
			t.Fatalf("count barriers: %v", err)
		}
		parked = count == 1
	}
	if !parked {
		close(release)
		t.Fatalf("no barrier ever parked (worker errors: %v)", h.workerErrors())
	}

	var joinWorkItems int
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT COUNT(*)::int FROM work_items AS wi
		JOIN node_runs AS nr ON nr.id = wi.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = 'gather'`, run.ID).Scan(&joinWorkItems); err != nil {
		t.Fatalf("count join work items: %v", err)
	}
	if joinWorkItems != 0 {
		t.Errorf("a parked barrier has %d work items, want 0 — there is no claimable work until it satisfies", joinWorkItems)
	}

	close(release)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })
	if got := h.run(run.ID).State; got != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", got, h.workerErrors())
	}
}

// TestReapedAsyncBranchIsCancelledAtTheActor is the best-effort, post-commit
// half of design §4.4: the barrier's `any` policy fires on the decision
// branch, the long-running branch is reaped transactionally, and the worker
// then asks that branch's actor — still holding a live session — to stop.
//
// The assertion that matters is the §13.6 cancel the actor received. Without
// it the run is still correct (the fenced guards refuse the late completion)
// but the actor burns compute on work nobody will read, which is exactly the
// zombie issue #19 fixed for run cancellation.
func TestReapedAsyncBranchIsCancelledAtTheActor(t *testing.T) {
	accepted := make(chan struct{}, 1)
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"invocation_id":"external_reaped","heartbeat_after_seconds":300,"supports_cancellation":true}`))
		select {
		case accepted <- struct{}{}:
		default:
		}
	})

	run := h.createRun("parallel-any-async.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	final := h.run(run.ID)
	if final.State != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", final.State, h.workerErrors())
	}
	select {
	case <-accepted:
	default:
		t.Fatal("the long-running branch was never invoked, so nothing was reaped mid-invocation")
	}

	// The losing branch is retired in committed state...
	var slowState string
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT status FROM node_runs WHERE run_id = $1 AND node_key = 'slow'`, run.ID).Scan(&slowState); err != nil {
		t.Fatalf("read the reaped branch: %v", err)
	}
	if slowState != "cancelled" {
		t.Errorf("reaped branch node run = %q, want cancelled", slowState)
	}

	// ...and its actor was asked to stop.
	cancels := h.cancellations()
	if len(cancels) != 1 {
		t.Fatalf("actor received %d cancel requests, want 1: %+v", len(cancels), cancels)
	}
	if cancels[0].InvocationID != "external_reaped" {
		t.Errorf("cancel names invocation %q, want external_reaped", cancels[0].InvocationID)
	}
	if cancels[0].Reason == "" {
		t.Error("cancel carried no reason; a reaped branch's actor deserves to know why")
	}

	// The attempt is recorded whatever the actor did with it (an
	// attempted-but-failed cancel is still evidence).
	var requested int
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT COUNT(*)::int FROM events WHERE aggregate_id = $1 AND event_type = $2`,
		run.ID, worker.TypeBranchCancelRequested).Scan(&requested); err != nil {
		t.Fatalf("count cancel-requested events: %v", err)
	}
	if requested != 1 {
		t.Errorf("branch cancel-requested events = %d, want 1", requested)
	}
}
