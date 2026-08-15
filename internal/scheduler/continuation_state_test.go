package scheduler_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/scheduler"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The scheduler half of issues #95 and #105, against a real Postgres and the
// real deadline path. The engine half is
// internal/engine/continuation_undecidable_test.go; these two tests are what
// prove the fix reaches the ONE caller that evaluates a `continue.while`.

// #95. `deadlineContinuationHolds` used to hand DecideContinuation the literal
// NodeState: "incomplete", so `node.state == "incomplete"` was true in every
// run for every node and the continuation ran until a bound was spent rather
// than until the node said it was done.
//
// The node run's status is forced to a terminal value here while its actor
// invocation is left waiting. That combination does not arise on its own,
// and that is exactly the point: it is a durable state the fabricated literal
// could never have produced, so a pause here proves the decision followed a
// hardcoded string, and a refusal to pause proves it followed the record.
func TestSchedulerDeadlineReadsNodeStateFromTheNodeRunRow(t *testing.T) {
	s := requireStore(t)
	f := newDeadlineFixtureSource(t, s, func(source string) string {
		anchor := "      kind: agent\n      ownerRef: team/platform-ai"
		continuation := `      kind: agent
      continue:
        while:
          - node.state == "incomplete"
        bounds:
          maxContinuations: 3
          maxWallClock: 2h
          maxSessions: 4
        onExhausted: timed_out
      ownerRef: team/platform-ai`
		if !strings.Contains(source, anchor) {
			t.Fatalf("deadline fixture lacks build-node anchor %q", anchor)
		}
		return strings.Replace(source, anchor, continuation, 1)
	})
	f.startAsyncWait(time.Now().Add(-time.Second))

	// The durable record now says this node run is done. Whatever the
	// condition decides, it must decide it from here.
	if _, err := s.Pool().Exec(context.Background(),
		`UPDATE node_runs SET status = 'completed' WHERE id = $1`, f.buildNodeRunID,
	); err != nil {
		t.Fatalf("force node run terminal: %v", err)
	}

	sch := scheduler.New(s, scheduler.Options{TickInterval: 25 * time.Millisecond})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sch.Run(runCtx) }()

	// The fired deadline must be spent, not re-armed: a re-armed timer is
	// the signature of a pause, and a pause here means `node.state` was
	// still the literal "incomplete".
	waitFor(t, 10*time.Second, func() bool {
		return mustPendingDeadlineCount(t, s, f.buildNodeRunID) == 0
	})
	if pending := mustPendingDeadlineCount(t, s, f.buildNodeRunID); pending != 0 {
		t.Fatalf("pending deadline timers = %d, want 0: the continuation paused against a "+
			"node run the database says is complete, which is only possible if node.state "+
			"was fabricated rather than read (issue #95)", pending)
	}
}

// #105. A `continue.while` that cannot be EVALUATED used to return the same
// zero decision a false one did: the loop stopped, the ledger recorded a stop
// indistinguishable from a legitimate one, and the error value was discarded
// at the return. Nothing downstream could tell, and nothing ever would.
//
// The fix does not change WHETHER it stops -- fail-closed is still right, and
// propagating the error instead would wedge this timer into re-running the
// same deterministic CEL failure every tick forever. It changes whether the
// stop is silent.
func TestSchedulerRecordsAnUndecidableContinuationCondition(t *testing.T) {
	s := requireStore(t)
	f := newDeadlineFixtureSource(t, s, func(source string) string {
		anchor := "      kind: agent\n      ownerRef: team/platform-ai"
		// Compiles (the CEL variables are dynamically typed), fails at
		// evaluation: `input` is an empty map in the continuation
		// activation. This is the shape task t21's development-loop graph
		// wanted to write and declined to trust -- see #105's own body.
		continuation := `      kind: agent
      continue:
        while:
          - input.failed_gate_count > 0
        bounds:
          maxContinuations: 3
          maxWallClock: 2h
          maxSessions: 4
        onExhausted: timed_out
      ownerRef: team/platform-ai`
		if !strings.Contains(source, anchor) {
			t.Fatalf("deadline fixture lacks build-node anchor %q", anchor)
		}
		return strings.Replace(source, anchor, continuation, 1)
	})
	f.startAsyncWait(time.Now().Add(-time.Second))

	sch := scheduler.New(s, scheduler.Options{TickInterval: 25 * time.Millisecond})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sch.Run(runCtx) }()

	waitFor(t, 10*time.Second, func() bool {
		return undecidableContinuationRecords(t, s, f.runID) > 0
	})
	if got := undecidableContinuationRecords(t, s, f.runID); got == 0 {
		t.Fatal("an undecidable continue.while left no record: the stop is still " +
			"indistinguishable from a node deciding to stop (issue #105)")
	}

	// Fail-closed is unchanged: the deadline still does its §12.6 job rather
	// than keeping an unbounded session warm on a condition nobody could
	// evaluate.
	waitFor(t, 10*time.Second, func() bool {
		status, _ := mustNodeRunStatus(t, s, f.buildNodeRunID)
		return status == "failed" || status == "timed_out"
	})
	if status, _ := mustNodeRunStatus(t, s, f.buildNodeRunID); status != "failed" && status != "timed_out" {
		t.Fatalf("node run status = %q, want terminal: an undecidable condition must not "+
			"pause the deadline", status)
	}
}

// undecidableContinuationRecords counts the outbox rows the scheduler wrote
// for this run's undecidable conditions. The outbox is the same channel the
// deadline's own event rides, which is what makes this visible to a reader of
// the run rather than to a reader of the process's logs.
func undecidableContinuationRecords(t *testing.T, s *postgres.Store, runID string) int {
	t.Helper()
	var count int
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM outbox
		 WHERE topic = 'dev.culture.nodes.continuation.undecidable'
		   AND payload->>'run_id' = $1`, runID,
	).Scan(&count); err != nil {
		t.Fatalf("count undecidable-continuation records: %v", err)
	}
	return count
}
