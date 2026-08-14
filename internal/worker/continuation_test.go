package worker_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// Engine-side continuation carriage (task t4, spec claim c3 / honesty h2,
// docs/adr/0010-continuation-ref-on-request.md).
//
// The live cost these tests exist to make addressable (#47): every node turn
// started a fresh provider session, because the ref §13.2 lets an actor offer
// was read off the wire and dropped. Here the whole round trip runs against a
// real PostgreSQL, a real HTTP actor, and the real worker loop: turn one
// offers a handle, it persists on the attempt, and turn two's outbound
// request carries it.
//
// What these tests deliberately do NOT assert is that anything resumes. The
// bridges still hardcode `continuation_ref: None` (task t5); this is the
// engine half.

// writeSyncResultWithContinuation is writeSyncResult plus §13.2's
// continuation_ref — the handle a bridge offers for continuing the
// conversation it just had.
func writeSyncResultWithContinuation(w http.ResponseWriter, outcome, output, ref string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w,
		`{"outcome":%q,"output":%s,"ledger_delta":{"records":[]},"continuation_ref":%q}`,
		outcome, output, ref)
}

// attemptContinuationRef reads the persisted ref of the single attempt
// recorded against nodeKey; nil means SQL NULL.
func attemptContinuationRef(t *testing.T, h *harness, runID, nodeKey string) *string {
	t.Helper()
	var ref *string
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT a.continuation_ref
		FROM attempts AS a JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = $2
		ORDER BY a.attempt_number DESC
		LIMIT 1
	`, runID, nodeKey).Scan(&ref); err != nil {
		t.Fatalf("read attempt continuation_ref for %q: %v", nodeKey, err)
	}
	return ref
}

// useAttributingWorker rebuilds the harness worker with a registry that can
// answer ActorRowID, which is what the continuation lookup is scoped by: a
// ref belongs to the actor that issued it, so an unattributed dispatch has
// no identity to look one up for (ADR 0010 §4).
func useAttributingWorker(t *testing.T, h *harness) string {
	t.Helper()
	actorRowID := mustAgentActor(t, h.store, h.ns.ID)
	wk, err := worker.New(h.store, h.engine, worker.Options{
		WorkerID:          "worker-continuation-" + t.Name(),
		NamespaceID:       h.ns.ID,
		LeaseDuration:     30 * time.Second,
		HeartbeatInterval: 200 * time.Millisecond,
		Registry: attributingRegistry{
			StaticRegistry: worker.StaticRegistry{"actor://company/analyzer": {URL: h.actorServer.URL}},
			rowIDs:         map[string]string{"actor://company/analyzer": actorRowID},
		},
		Signer:          h.signer,
		CallbackBaseURL: h.callbackServer.URL,
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	h.worker = wk
	return actorRowID
}

// The round trip: the second turn against the same actor in the same run is
// dispatched with the ref the first turn returned — and the first turn, which
// had no prior conversation, was dispatched with none.
func TestNextTurnCarriesThePriorContinuationRef(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, req actors.InvocationRequest) {
		// Each turn offers its own fresh handle, so a test that saw the
		// wrong one would see a name, not an ambiguity.
		ref := "sess-first"
		if req.ContinuationRef != nil {
			ref = "sess-second"
		}
		writeSyncResultWithContinuation(w, "completed", `{"summary":"turn done"}`, ref)
	})
	useAttributingWorker(t, h)

	run := h.createRun("continuation.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", state, h.workerErrors())
	}

	invocations := h.invocations()
	if len(invocations) != 2 {
		t.Fatalf("actor was invoked %d time(s), want 2", len(invocations))
	}
	if invocations[0].ContinuationRef != nil {
		t.Errorf("first invocation carried continuation_ref %q, want none: no prior conversation exists",
			*invocations[0].ContinuationRef)
	}
	if invocations[1].ContinuationRef == nil {
		t.Fatal("second invocation carried no continuation_ref, want the ref the first turn returned")
	}
	if got := *invocations[1].ContinuationRef; got != "sess-first" {
		t.Errorf("second invocation continuation_ref = %q, want %q", got, "sess-first")
	}

	// Both refs are durable, which is what makes the lookup possible across
	// worker processes rather than only within one.
	if got := attemptContinuationRef(t, h, run.ID, "first"); got == nil || *got != "sess-first" {
		t.Errorf("attempt(first).continuation_ref = %v, want %q", got, "sess-first")
	}
	if got := attemptContinuationRef(t, h, run.ID, "second"); got == nil || *got != "sess-second" {
		t.Errorf("attempt(second).continuation_ref = %v, want %q", got, "sess-second")
	}
}

// An actor that offers no handle is dispatched cold, every turn, with the key
// absent from the request body — never an empty string and never a guess. A
// cold session costs more and is never wrong; the reverse is not true.
func TestDispatchWithoutAnOfferedContinuationRefStaysCold(t *testing.T) {
	h := newHarness(t, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		writeSyncResult(w, "completed", `{"summary":"turn done"}`)
	})
	useAttributingWorker(t, h)

	run := h.createRun("continuation.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", state, h.workerErrors())
	}

	for i, inv := range h.invocations() {
		if inv.ContinuationRef != nil {
			t.Errorf("invocation %d carried continuation_ref %q, want none: no actor ever offered one",
				i, *inv.ContinuationRef)
		}
	}
	for _, nodeKey := range []string{"first", "second"} {
		if got := attemptContinuationRef(t, h, run.ID, nodeKey); got != nil {
			t.Errorf("attempt(%s).continuation_ref = %q, want NULL", nodeKey, *got)
		}
	}
}
