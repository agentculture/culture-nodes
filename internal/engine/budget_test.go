package engine_test

import (
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
)

// The declared economic budget, engine side (task t11, spec claim c6 /
// honesty h5).
//
// Two things are being pinned here. First, that a run's budget travels in the
// pinned IR like every other declared bound — the enforcement site reads what
// the run agreed to, never a live setting somebody changed since. Second,
// that a refusal ROUTES: the PRD is explicit (§3.4) that a domain answer
// follows a graph edge and technical failure is not a way to express one, and
// an author who declared a budget is entitled to say what happens when it
// runs out.

func TestBudgetIsPinnedOnTheLoadedWorkflow(t *testing.T) {
	f := newFixture(t, "budget-route.workflow.yaml")

	wf, err := engine.LoadWorkflow(f.cw.Digest, f.cw.Normalized)
	if err != nil {
		t.Fatalf("LoadWorkflow: %v", err)
	}
	if !wf.Budget.Declared() {
		t.Fatal("loaded workflow declares no budget; the fixture declares one")
	}
	if wf.Budget.MaxSessions != 1 {
		t.Errorf("Budget.MaxSessions = %d, want 1", wf.Budget.MaxSessions)
	}
	if wf.Budget.MaxUncachedInput != 250000 {
		t.Errorf("Budget.MaxUncachedInput = %d, want 250000", wf.Budget.MaxUncachedInput)
	}
}

func TestWorkflowWithoutABudgetLoadsUnbudgeted(t *testing.T) {
	f := newFixture(t, "technical-route.workflow.yaml")

	wf, err := engine.LoadWorkflow(f.cw.Digest, f.cw.Normalized)
	if err != nil {
		t.Fatalf("LoadWorkflow: %v", err)
	}
	if wf.Budget.Declared() {
		t.Errorf("Budget = %+v for a workflow that declares none, want undeclared", wf.Budget)
	}
}

// The refusal follows the edge the author declared. The node run failed — no
// dispatch happened, so no domain answer exists — but the RUN did not: it is
// still running, on the branch the workflow said to take when it cannot pay.
func TestRefusalOutcomeFollowsItsDeclaredEdge(t *testing.T) {
	f := newFixture(t, "budget-route.workflow.yaml")
	run := f.createRun(`{"subject":"expensive"}`)

	build := f.readyNodeRun(run.ID)
	result := f.step("worker-a", build.ID, engine.CompletionRequest{
		TechStatus:     engine.StatusPolicyDenied,
		RefusalOutcome: engine.OutcomeBudgetExhausted,
	})

	if result.Retried {
		t.Fatal("a refused dispatch must not be retried: the budget will refuse the retry for the same reason")
	}
	if result.NextNodeID != "summarise" {
		t.Fatalf("budget_exhausted routed to %q, want summarise (%s)", result.NextNodeID, result.Diagnostic)
	}
	if result.EdgeFrom != "build."+engine.OutcomeBudgetExhausted {
		t.Errorf("followed edge %q, want build.%s", result.EdgeFrom, engine.OutcomeBudgetExhausted)
	}
	if result.NodeRunState != engine.NodeRunFailed {
		t.Errorf("node run state = %s, want failed — the node did not do its work", result.NodeRunState)
	}
	if result.RunState != engine.RunRunning {
		t.Errorf("run state = %s, want running — a refusal the author routed is not a failed run", result.RunState)
	}

	done := f.step("worker-a", result.NextNodeRunID, succeeded("completed", `{"stopped":true}`))
	if done.RunState != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (%s)", done.RunState, done.Diagnostic)
	}
}

// A refusal nobody routed ends the run, and says which budget refused it.
// Silence here would be the worst of both worlds: money not spent and no
// record of the decision.
func TestUnroutedRefusalEndsTheRunSayingWhy(t *testing.T) {
	f := newFixture(t, "technical-route.workflow.yaml")
	run := f.createRun(`{"subject":"expensive"}`)

	build := f.readyNodeRun(run.ID)
	result := f.step("worker-a", build.ID, engine.CompletionRequest{
		TechStatus:     engine.StatusPolicyDenied,
		RefusalOutcome: engine.OutcomeBudgetExhausted,
	})

	if result.RunState != engine.RunFailed {
		t.Fatalf("run state = %s, want failed: nothing routes budget_exhausted in this workflow", result.RunState)
	}
	if !strings.Contains(result.Diagnostic, engine.OutcomeBudgetExhausted) {
		t.Errorf("diagnostic = %q, want it to name %s", result.Diagnostic, engine.OutcomeBudgetExhausted)
	}
}

// A refusal name is meaningless on a succeeded attempt: an attempt that
// produced a domain answer was dispatched, and nothing refused it.
func TestRefusalOutcomeOnASucceededAttemptIsRejected(t *testing.T) {
	f := newFixture(t, "budget-route.workflow.yaml")
	run := f.createRun(`{"subject":"expensive"}`)
	build := f.readyNodeRun(run.ID)
	claimed := f.claim("worker-a", build.ID)

	req := completion(claimed, "worker-a", engine.CompletionRequest{
		TechStatus:     engine.StatusSucceeded,
		Outcome:        "completed",
		Output:         []byte(`{}`),
		RefusalOutcome: engine.OutcomeBudgetExhausted,
	})
	if _, err := f.engine.CompleteAttempt(f.ctx, req); err == nil {
		t.Fatal("CompleteAttempt accepted a refusal outcome on a succeeded attempt; it must not")
	}
}
