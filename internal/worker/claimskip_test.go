package worker_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
)

// The #188 fix, pinned end to end against real PostgreSQL: a worker with no
// code dispatcher of any kind skips code-node work items (they stay ready
// for a code-capable worker) while still claiming and completing agent work.
func TestWorkerWithoutCodeRunnerSkipsCodeWorkAndClaimsAgentWork(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		writeSyncResult(w, "completed", `{"score":0.91,"summary":"claimed"}`)
	})

	codeRun := h.createRun("code.workflow.yaml", `{}`)
	agentRun := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)

	// Drive the worker loop until the agent run terminates — a run takes
	// several ticks (entry claim, dispatch, completion, end transition), so a
	// single Tick can only prove claiming, not completion.
	h.runUntil(20*time.Second, func() bool { return h.run(agentRun.ID).State.Terminal() })

	if got := h.workItemStates(codeRun.ID)["test"]; got != "ready" {
		t.Errorf("code work state = %q, want ready", got)
	}
	if got := h.run(agentRun.ID).State; got != engine.RunCompleted {
		t.Errorf("agent run state = %q, want completed (worker errors: %v)", got, h.workerErrors())
	}
	if got := len(h.invocations()); got != 1 {
		t.Errorf("agent invocations = %d, want 1", got)
	}
}
