package worker_test

import (
	"bytes"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
)

// The declared economic contract, enforced at the dispatch site (task t11,
// spec claims c6/c46, honesty h5).
//
// The failure these tests encode is the one the 2026-08 fan-out actually
// paid for: a wave discovered mid-flight that its session window was gone,
// after the sessions had already been billed. A budget declared on the
// workflow moves that discovery BEFORE the invocation and turns it into a
// routing decision the author made in advance.
//
// The subtle half is c46, and it has its own test below: `maxSessions`
// counts COLD STARTS. A warm resumed turn continues a session already paid
// for and charges nothing — otherwise stickiness (c3) and the budget would
// fight, since a workstream of N turns on one warm session would count N and
// always exhaust the budget it was designed to conserve.

// writeSyncResultWithUsage is writeSyncResult plus a §13.2 usage block.
// cached < 0 means the backend reported NO cache telemetry at all (the key is
// omitted, never zero-filled — ADR 0009).
func writeSyncResultWithUsage(w http.ResponseWriter, outcome, output string, input, cached int64) {
	w.Header().Set("Content-Type", "application/json")
	usage := fmt.Sprintf(`{"input_tokens":%d,"output_tokens":10}`, input)
	if cached >= 0 {
		usage = fmt.Sprintf(`{"input_tokens":%d,"output_tokens":10,"cached_input_tokens":%d}`, input, cached)
	}
	_, _ = fmt.Fprintf(w, `{"outcome":%q,"output":%s,"ledger_delta":{"records":[]},"usage":%s}`,
		outcome, output, usage)
}

// runSessionStarts is the durable count of NEW provider sessions the run
// opened (migration 0023) — what maxSessions bounds.
func runSessionStarts(t *testing.T, h *harness, runID string) int {
	t.Helper()
	var count int
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT count(*)::int FROM run_sessions WHERE run_id = $1`, runID).Scan(&count); err != nil {
		t.Fatalf("count run sessions: %v", err)
	}
	return count
}

// nodeRunOutcome reads the outcome recorded on a run's named node run.
func nodeRunOutcome(t *testing.T, h *harness, runID, nodeKey string) (state, outcome string) {
	t.Helper()
	var recorded *string
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT status, outcome FROM node_runs WHERE run_id = $1 AND node_key = $2
	`, runID, nodeKey).Scan(&state, &recorded); err != nil {
		t.Fatalf("read node run %q: %v", nodeKey, err)
	}
	if recorded != nil {
		outcome = *recorded
	}
	return state, outcome
}

// c46, the load-bearing one: a warm workstream of N node turns consumes ONE
// session, not N. All three turns run under maxSessions: 1 because turns two
// and three resume the session turn one opened.
func TestWarmWorkstreamConsumesOneSessionNotN(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, req actors.InvocationRequest) {
		// Every turn offers a handle, so every later turn is dispatched warm.
		ref := "sess-warm"
		writeSyncResultWithContinuation(w, "completed", `{"summary":"turn done"}`, ref)
		_ = req
	})
	useAttributingWorker(t, h)

	run := h.createRun("budget-sessions.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed: three warm turns fit in a one-session budget (worker errors: %v)",
			state, h.workerErrors())
	}
	invocations := h.invocations()
	if len(invocations) != 3 {
		t.Fatalf("actor was invoked %d time(s), want 3: no warm turn may be refused", len(invocations))
	}
	if invocations[0].ContinuationRef != nil {
		t.Error("the first turn carried a continuation ref; it had no prior conversation")
	}
	for i, inv := range invocations[1:] {
		if inv.ContinuationRef == nil {
			t.Fatalf("turn %d carried no continuation ref, so it was a cold start and the test proves nothing", i+2)
		}
	}
	if got := runSessionStarts(t, h, run.ID); got != 1 {
		t.Errorf("run charged %d sessions, want 1: a warm workstream of 3 turns opens one session", got)
	}
}

// The same graph against an actor that offers no handle: every turn is a cold
// start, the second one cannot be funded, and it is refused BEFORE the actor
// is invoked — then routed down the edge the author declared. The run does
// not fail; it lands where the workflow said to land.
func TestUnfundableDispatchIsRefusedAndRoutesItsDeclaredEdge(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		writeSyncResult(w, "completed", `{"summary":"turn done"}`)
	})
	useAttributingWorker(t, h)

	run := h.createRun("budget-sessions.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed: the refusal routes an edge, it does not fail the run (worker errors: %v)",
			state, h.workerErrors())
	}
	if got := len(h.invocations()); got != 1 {
		t.Fatalf("actor was invoked %d time(s), want 1: the unfundable dispatch must never reach it", got)
	}
	if got := runSessionStarts(t, h, run.ID); got != 1 {
		t.Errorf("run charged %d sessions, want 1: a refused dispatch opens no session", got)
	}

	state, outcome := nodeRunOutcome(t, h, run.ID, "second")
	if outcome != engine.OutcomeBudgetExhausted {
		t.Errorf("node run `second` outcome = %q, want %q", outcome, engine.OutcomeBudgetExhausted)
	}
	if state != string(engine.NodeRunFailed) {
		t.Errorf("node run `second` state = %q, want failed: the node did not do its work", state)
	}

	status, result := attemptRecord(t, h, run.ID, "second")
	if engine.TechStatus(status) != engine.StatusPolicyDenied {
		t.Errorf("attempt status = %q, want %q: a declared policy denied the dispatch",
			status, engine.StatusPolicyDenied)
	}
	if !bytes.Contains(result, []byte("budget_exhausted")) {
		t.Errorf("attempt result = %s, want the refusal recorded", result)
	}
	if !bytes.Contains(result, []byte("maxSessions")) {
		t.Errorf("attempt result = %s, want it to name the bound that refused the dispatch", result)
	}

	// The run's output came from the branch the refusal took.
	if got := h.run(run.ID).Output; !bytes.Contains(got, []byte("turn done")) {
		t.Errorf("run output = %s, want the first node's output the refusal branch binds", got)
	}
}

// A refusal nobody routed still ends the run, and says why. The alternative —
// a work item stranded ready forever — is exactly what budget.go's header
// refuses to allow.
func TestUnroutedRefusalFailsTheRunWithTheReasonRecorded(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		writeSyncResult(w, "completed", `{"summary":"turn done"}`)
	})
	useAttributingWorker(t, h)

	run := h.createRun("budget-unrouted.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunFailed {
		t.Fatalf("run state = %s, want failed: nothing routes budget_exhausted here (worker errors: %v)",
			state, h.workerErrors())
	}
	status, result := attemptRecord(t, h, run.ID, "second")
	if engine.TechStatus(status) != engine.StatusPolicyDenied {
		t.Errorf("attempt status = %q, want %q", status, engine.StatusPolicyDenied)
	}
	if !bytes.Contains(result, []byte("budget_exhausted")) {
		t.Errorf("attempt result = %s, want the refusal recorded", result)
	}
	if state := h.workItemStates(run.ID)["second"]; state != "completed" {
		t.Errorf("work item state = %q, want completed: a refused item must leave the dispatch loop", state)
	}
}

// The uncached-input ceiling, and the honesty rule that decides it: an
// attempt reporting 200 input tokens with NO cache telemetry spends the whole
// 200 against a 150-token budget, so the next dispatch is refused.
func TestUncachedInputAccumulatesAndRefusesWithoutCacheTelemetry(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		writeSyncResultWithUsage(w, "completed", `{"summary":"turn done"}`, 200, -1)
	})
	useAttributingWorker(t, h)

	run := h.createRun("budget-uncached.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", state, h.workerErrors())
	}
	if got := len(h.invocations()); got != 1 {
		t.Fatalf("actor was invoked %d time(s), want 1: 200 uncached tokens exhaust a 150-token budget", got)
	}
	_, outcome := nodeRunOutcome(t, h, run.ID, "second")
	if outcome != engine.OutcomeBudgetExhausted {
		t.Errorf("node run `second` outcome = %q, want %q", outcome, engine.OutcomeBudgetExhausted)
	}
	_, result := attemptRecord(t, h, run.ID, "second")
	if !bytes.Contains(result, []byte("maxUncachedInput")) {
		t.Errorf("attempt result = %s, want it to name the bound that refused the dispatch", result)
	}
}

// The same 200 input tokens, 190 of them served from cache, spend 10 — and
// the second turn is funded. Cache telemetry is what buys the headroom;
// absent telemetry does not.
func TestReportedCacheHitsBuyHeadroomUnderTheSameCeiling(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		writeSyncResultWithUsage(w, "completed", `{"summary":"turn done"}`, 200, 190)
	})
	useAttributingWorker(t, h)

	run := h.createRun("budget-uncached.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", state, h.workerErrors())
	}
	if got := len(h.invocations()); got != 2 {
		t.Errorf("actor was invoked %d time(s), want 2: 10 uncached tokens leave room under a 150-token budget", got)
	}
}

// A workflow that declares no budget is unbudgeted: nothing is refused and
// nothing is charged. The enforcement site must cost an unbudgeted run
// nothing at all, not even a session ledger row.
func TestUnbudgetedWorkflowIsNeverRefused(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		writeSyncResult(w, "completed", `{"summary":"turn done"}`)
	})
	useAttributingWorker(t, h)

	run := h.createRun("continuation.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", state, h.workerErrors())
	}
	if got := len(h.invocations()); got != 2 {
		t.Errorf("actor was invoked %d time(s), want 2", got)
	}
	if got := runSessionStarts(t, h, run.ID); got != 0 {
		t.Errorf("an unbudgeted run charged %d sessions, want 0: nothing is being spent against", got)
	}
}
