package faulttest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/store"
)

// TestFaultTwoWorkersNoDoubleCommit proves §20.4's "SQS signal is
// duplicated | PostgreSQL claim permits one current owner" under real
// process-level concurrency: two separate OS processes race
// internal/store/postgres's ClaimWork over 50 ready work items against one
// Postgres. Every item must be completed exactly once -- a double-commit
// would show up as either a violated PRIMARY KEY on test_work_results.work_id
// (which would make one of the two worker processes exit non-zero, failing
// this test via wait()) or a completed-count mismatch.
func TestFaultTwoWorkersNoDoubleCommit(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "fault-two-workers")
	runID := mustRun(t, s, ns.ID)

	const total = 50
	for i := 0; i < total; i++ {
		mustEnqueue(t, s, ns.ID, mustNodeRun(t, s, ns.ID, runID))
	}

	workerA := startWorker(t, workerConfig{namespaceID: ns.ID, workerID: "fault-worker-a-" + store.NewULID(), leaseSeconds: 5.0, limit: 5, workMS: 20, idleTimeoutMS: 3000})
	workerB := startWorker(t, workerConfig{namespaceID: ns.ID, workerID: "fault-worker-b-" + store.NewULID(), leaseSeconds: 5.0, limit: 5, workMS: 20, idleTimeoutMS: 3000})

	if err := workerA.wait(t, 30*time.Second); err != nil {
		t.Fatalf("worker A: %v\n--- worker A output ---\n%s", err, workerA.out.String())
	}
	if err := workerB.wait(t, 30*time.Second); err != nil {
		t.Fatalf("worker B: %v\n--- worker B output ---\n%s", err, workerB.out.String())
	}

	var completedCount int
	if err := s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM work_items WHERE namespace_id = $1 AND state = 'completed'`, ns.ID,
	).Scan(&completedCount); err != nil {
		t.Fatalf("count completed work_items: %v", err)
	}
	if completedCount != total {
		t.Fatalf("completed work_items = %d, want %d\n--- worker A ---\n%s\n--- worker B ---\n%s",
			completedCount, total, workerA.out.String(), workerB.out.String())
	}

	var resultsCount int
	if err := s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM test_work_results r JOIN work_items w ON w.id = r.work_id WHERE w.namespace_id = $1`, ns.ID,
	).Scan(&resultsCount); err != nil {
		t.Fatalf("count test_work_results: %v", err)
	}
	if resultsCount != total {
		t.Fatalf("test_work_results rows = %d, want %d (no item may be double-committed or lost)", resultsCount, total)
	}
}

// TestFaultKilledWorkerReclaimedBySurvivor proves §20.4's "Worker dies
// before dispatch | Lease expires; another worker claims" end-to-end:
// worker "fault-victim" claims 5 items and is SIGKILLed (coordinated via a
// flag file it writes right after claiming, before it can do any of the
// simulated work) before it completes any of them. Worker "fault-survivor"
// runs the whole time; its own poll loop reclaims the victim's expired
// leases and completes them, with no special-casing from this test. The
// recovery must land within lease-expiry+5s, per the task's acceptance
// bound.
func TestFaultKilledWorkerReclaimedBySurvivor(t *testing.T) {
	s := requireStore(t)

	ns := mustNamespace(t, s, "fault-kill-worker")
	runID := mustRun(t, s, ns.ID)

	const total = 5
	for i := 0; i < total; i++ {
		mustEnqueue(t, s, ns.ID, mustNodeRun(t, s, ns.ID, runID))
	}

	flagFile := filepath.Join(t.TempDir(), "claimed.flag")
	const leaseSeconds = 2.0

	// The victim claims everything at once (limit=total) and "works" each
	// claimed item for 4s before it would complete -- far longer than the
	// time it takes the test to notice the flag file and kill it, so the
	// kill is guaranteed to land before any completion.
	victim := startWorker(t, workerConfig{namespaceID: ns.ID, workerID: "fault-victim", leaseSeconds: leaseSeconds, limit: total, workMS: 4000, idleTimeoutMS: 8000, claimedFlagFile: flagFile})

	waitForFlagFile(t, flagFile, 5*time.Second)

	if err := victim.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill victim: %v", err)
	}
	_ = victim.wait(t, 5*time.Second) // a "signal: killed" exit error is expected here

	// The survivor starts only after the kill. Starting it alongside the
	// victim looks like a stronger test but is a start-order RACE: nothing
	// stops the survivor's first poll from winning some or all of the ready
	// items, in which case the victim claims nothing, never writes its flag
	// file, and the test fails without exercising recovery at all (observed
	// under parallel-suite load). §20.4's row is "worker dies before
	// dispatch -> lease expires; another worker claims" — a post-kill
	// survivor exercises exactly that path with no special-casing: its own
	// poll loop (ReclaimExpired, then ClaimWork) does all the recovering.
	survivor := startWorker(t, workerConfig{namespaceID: ns.ID, workerID: "fault-survivor", leaseSeconds: leaseSeconds, limit: total, workMS: 20, idleTimeoutMS: 6000})

	// The h19/§20.4 bound is about RECLAIM latency: "a killed worker's lease
	// is reclaimed within expiry plus five seconds". Measure exactly that —
	// the moment any victim-held item is claimed again (its fencing token
	// rises past the victim's claim) — against expiry+5s. Completing all the
	// reclaimed work is a separate LIVENESS assertion with its own generous
	// bound: on a loaded host (CI runners, parallel Docker suites) the
	// survivor's work loop can legitimately take longer than the reclaim
	// bound without violating the spec's recovery promise.
	killedAt := time.Now()
	reclaimDeadline := time.Duration(leaseSeconds*float64(time.Second)) + 5*time.Second
	waitForReclaim(t, s, ns.ID, killedAt.Add(reclaimDeadline))
	waitForCompletedCount(t, s, ns.ID, total, 30*time.Second)

	// Not part of the timing assertion above (a worker's own idle timeout
	// is independent of how fast recovery happened) -- just confirms the
	// survivor shuts down cleanly and captures its log for the failure
	// message below, if needed.
	if err := survivor.wait(t, 10*time.Second); err != nil {
		t.Fatalf("survivor worker exited abnormally: %v\n--- survivor output ---\n%s", err, survivor.out.String())
	}

	var survivorCompletions int
	// Scoped to this test's namespace: under `go test -count=N` every
	// iteration shares the one ephemeral database, so an unscoped count
	// accumulates prior iterations' completions.
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM test_work_results r
		  WHERE r.completed_by = 'fault-survivor'
		    AND r.node_run_id IN (SELECT id FROM node_runs WHERE namespace_id = $1)`, ns.ID,
	).Scan(&survivorCompletions); err != nil {
		t.Fatalf("count survivor completions: %v", err)
	}
	if survivorCompletions != total {
		t.Fatalf("completions recorded by fault-survivor = %d, want %d (every victim-abandoned item must be reclaimed and completed by the surviving worker)\n--- victim ---\n%s\n--- survivor ---\n%s",
			survivorCompletions, total, victim.out.String(), survivor.out.String())
	}
}

// TestFaultDuplicateSignalExactlyOneEffectiveCompletion proves §20.4's "SQS
// signal is duplicated | PostgreSQL claim permits one current owner" one
// level up the stack: a duplicated work signal (e.g. an at-least-once queue
// redelivering, or a caller retrying an enqueue it wasn't sure landed)
// produces two independent work_items rows for the same node_run_id. Both
// are legitimately claimable and both technically complete (work_items.state
// = 'completed' for both, proving the claim/complete path itself has no
// bug) -- but test_work_results' UNIQUE(node_run_id, attempt) guard admits
// only one effective completion, because both work items reach their own
// first attempt (attempt=1) independently. This is the repo's "domain
// outcome is not the same as technical status" ground rule (CLAUDE.md),
// exercised at the work-claiming layer rather than the ledger layer (t8).
func TestFaultDuplicateSignalExactlyOneEffectiveCompletion(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "fault-dup-signal")
	runID := mustRun(t, s, ns.ID)
	nodeRunID := mustNodeRun(t, s, ns.ID, runID)

	// Two independent enqueue calls for the SAME logical unit of work, as
	// a duplicated signal would produce.
	mustEnqueue(t, s, ns.ID, nodeRunID)
	mustEnqueue(t, s, ns.ID, nodeRunID)

	workerA := startWorker(t, workerConfig{namespaceID: ns.ID, workerID: "fault-dup-a-" + store.NewULID(), leaseSeconds: 5.0, limit: 2, workMS: 20, idleTimeoutMS: 3000})
	workerB := startWorker(t, workerConfig{namespaceID: ns.ID, workerID: "fault-dup-b-" + store.NewULID(), leaseSeconds: 5.0, limit: 2, workMS: 20, idleTimeoutMS: 3000})

	if err := workerA.wait(t, 30*time.Second); err != nil {
		t.Fatalf("worker A: %v\n--- worker A output ---\n%s", err, workerA.out.String())
	}
	if err := workerB.wait(t, 30*time.Second); err != nil {
		t.Fatalf("worker B: %v\n--- worker B output ---\n%s", err, workerB.out.String())
	}

	var completedCount int
	if err := s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM work_items WHERE node_run_id = $1 AND state = 'completed'`, nodeRunID,
	).Scan(&completedCount); err != nil {
		t.Fatalf("count completed work_items: %v", err)
	}
	if completedCount != 2 {
		t.Fatalf("completed work_items for node_run %s = %d, want 2 (both work items must reach technical completion independently)\n--- worker A ---\n%s\n--- worker B ---\n%s",
			nodeRunID, completedCount, workerA.out.String(), workerB.out.String())
	}

	var resultsCount int
	if err := s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM test_work_results WHERE node_run_id = $1`, nodeRunID,
	).Scan(&resultsCount); err != nil {
		t.Fatalf("count test_work_results: %v", err)
	}
	if resultsCount != 1 {
		t.Fatalf("test_work_results rows for node_run %s = %d, want exactly 1 (the duplicated signal must yield exactly one effective completion)\n--- worker A ---\n%s\n--- worker B ---\n%s",
			nodeRunID, resultsCount, workerA.out.String(), workerB.out.String())
	}
}
