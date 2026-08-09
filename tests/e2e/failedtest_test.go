package e2etest

import (
	"context"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/internal/runners/headspace"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// The other half of the implementation issue's acceptance criterion
// "changes_required AND FAILED TESTS loop to build without becoming engine
// failures": TestPhase1VerticalSlice drives the verify loop, this drives the
// TEST loop.
//
// The scripted runner exits 1 on its first execution and 0 afterwards, so the
// `test.failed` edge is walked exactly once and the run still completes. The
// property under test is the same one §3.4 states: a test suite that RAN and
// reported failures dispatched successfully. The attempt that produced
// `failed` must be recorded technically SUCCEEDED, and the engine must not
// have retried it.
type failThenPassRunner struct {
	inner *scriptedRunner
}

func (f *failThenPassRunner) Execute(ctx context.Context, op runners.Operation) (runners.Result, error) {
	res, err := f.inner.Execute(ctx, op)
	if err != nil {
		return res, err
	}
	if len(f.inner.operations()) == 1 {
		one := 1
		res.Exit = &runners.Exit{Code: &one}
	}
	return res, nil
}

var _ runners.Runner = (*failThenPassRunner)(nil)

func TestFailedTestSuiteLoopsToBuildAsADomainOutcome(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ns := pgtest.MustNamespace(t, s, "e2e-failloop")

	runner := &failThenPassRunner{inner: &scriptedRunner{}}
	agentIDs := map[string]string{}
	agents := newDeliveryAgents(t, agentIDs)
	// The verifier passes on sight here: this test is about the `test.failed`
	// edge, and a second loop through verify would only blur which edge the
	// assertions are counting.
	agents.verifyRequestsChanges = false
	registered, runnerID := registerActors(t, s, ns.ID, agents.server.URL)
	agents.mu.Lock()
	for node, id := range registered {
		agentIDs[node] = id
	}
	agents.mu.Unlock()

	stack := startStack(t, stackConfig{
		namespaceID:   ns.ID,
		agentsURL:     agents.server.URL,
		runner:        runner,
		runnerName:    headspace.RunnerName,
		runnerActorID: runnerID,
	})
	defer stack.stop()

	digest := stack.publishWorkflow(t)
	runID := stack.createRun(t, digest, []byte(`{"request":"add a /healthz endpoint"}`))

	sseDone := make(chan sseResult, 1)
	go func() {
		events, err := streamRunEvents(t, stack.server.URL, runID, 90*time.Second)
		sseDone <- sseResult{events: events, err: err}
	}()

	view := stack.waitForTerminal(t, runID, 90*time.Second)
	if failures := agents.scriptFailures(); len(failures) > 0 {
		t.Fatalf("the scripted agents refused an invocation: %v", failures)
	}
	if view.Run.State != string(engine.RunCompleted) {
		dumpRunState(t, stack, runID)
		t.Fatalf("run state = %s, want completed (worker errors: %v)", view.Run.State, stack.errors())
	}

	// The failing suite was a domain answer, recorded on a technically
	// SUCCEEDED attempt, and never retried.
	testRuns := nodeRunsFor(view, "test")
	if len(testRuns) != 2 {
		t.Fatalf("the code node ran %d times, want 2 (one failing, one passing)", len(testRuns))
	}
	failed := 0
	for _, nr := range testRuns {
		if nr.Outcome != "failed" {
			continue
		}
		failed++
		if len(nr.Attempts) != 1 {
			t.Errorf("the failing code-node run has %d attempts, want 1: a domain outcome is not retried", len(nr.Attempts))
		}
		for _, attempt := range nr.Attempts {
			if attempt.Status != string(engine.StatusSucceeded) {
				t.Errorf("the attempt reporting `failed` has technical status %q, want succeeded (PRD §3.4)", attempt.Status)
			}
		}
	}
	if failed != 1 {
		t.Errorf("the code node reported `failed` %d times, want exactly 1", failed)
	}

	// build ran twice: once before the failing suite, once after the loop.
	if got := agents.callCount("build"); got != 2 {
		t.Errorf("build was invoked %d times, want 2", got)
	}
	// verify ran once: the loop went back to build BEFORE reaching verify.
	if got := agents.callCount("verify"); got != 1 {
		t.Errorf("verify was invoked %d times, want 1: a failing suite must not reach the verifier", got)
	}

	events, err := drainSSE(t, sseDone)
	if err != nil {
		t.Fatalf("SSE stream: %v", err)
	}
	if got := countEdgeTransitions(events, "test.failed"); got != 1 {
		t.Errorf("the test.failed edge was walked %d times, want exactly 1", got)
	}
	if got := countEdgeTransitions(events, "test.passed"); got != 1 {
		t.Errorf("the test.passed edge was walked %d times, want exactly 1", got)
	}

	// No attempt in the whole run failed technically: the loop is domain
	// routing from end to end.
	for _, nr := range view.NodeRuns {
		for _, attempt := range nr.Attempts {
			if attempt.Status != string(engine.StatusSucceeded) {
				t.Errorf("node %s attempt %d status = %q, want succeeded (result: %s)",
					nr.NodeID, attempt.AttemptNumber, attempt.Status, attempt.Result)
			}
		}
	}

	// A failing run is measured too: both executions left observed evidence.
	led := ledgerFor(t, stack.db, ns.ID)
	records, err := led.Records(context.Background(), runID)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	evidence := 0
	for _, rec := range records {
		if rec.RecordType == ledger.RecordEvidence && rec.Authority == ledger.AuthorityObserved {
			evidence++
		}
	}
	if evidence != 2 {
		t.Errorf("run has %d observed evidence records, want 2 (one per code-node run, failing included)", evidence)
	}
}
