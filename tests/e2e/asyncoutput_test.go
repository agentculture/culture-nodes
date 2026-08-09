package e2etest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/runners/headspace"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// Regression test for the delivery note "run.output observed null for the
// live smoke's end-node binding — verify the end-node output path outside
// the e2e fixtures" (docs/deliveries/2026-08-08-culture-nodes-app-design.md,
// "Remaining Work / Follow-up").
//
// Every existing test that proves `examples/delivery-loop`'s `finish` node
// (bound to /nodes/verify/output) resolves to a real value drives completion
// one of two ways:
//
//   - deliveryAgents (harness_test.go): a real HTTP actor, but one that
//     always answers §13.2 SYNCHRONOUSLY (HTTP 200 with the result inline).
//   - internal/worker's own async test (TestWorkerParksAsyncInvocation...):
//     genuinely asynchronous (HTTP 202 + a real §13.4 callback POST), but the
//     run is created by calling engine.CreateRun directly, never through
//     POST /v1alpha1/workflows + POST /v1alpha1/runs, and the assertion reads
//     engine.Run.Output straight from the store rather than the HTTP
//     GET /v1alpha1/runs/{id} view every other client (web front, CLI,
//     smoke.sh) actually reads.
//
// Neither combination matches how a genuinely external actor — the
// colleague bridge (adapters/colleague), or any real §13 implementation —
// behaves when driven through the real deployment surface: publish and run
// over HTTP, accept asynchronously, report back through the real callback
// route. This test drives exactly that combination.
//
// asyncDeliveryAgents is a variant of deliveryAgents (harness_test.go) that
// answers every §13 invocation asynchronously and reports the terminal event
// a short time later through the real callback URL the worker minted — the
// scripted judgement itself is unchanged (delegated to deliveryAgents.script)
// so the two harnesses can never drift about what the agents are supposed to
// say.
type asyncDeliveryAgents struct {
	server *httptest.Server
	client *http.Client

	mu       sync.Mutex
	actorIDs map[string]string // node id -> registered actors.id
	calls    map[string]int    // node id -> invocation count
	failures []string
}

func newAsyncDeliveryAgents(t *testing.T, actorIDs map[string]string) *asyncDeliveryAgents {
	t.Helper()
	a := &asyncDeliveryAgents{actorIDs: actorIDs, calls: map[string]int{}, client: http.DefaultClient}
	a.server = httptest.NewServer(http.HandlerFunc(a.handle))
	t.Cleanup(a.server.Close)
	return a
}

// setupAsyncDeliveryAgents mirrors setupDeliveryAgentsAndActors exactly
// (same registration order, same lock discipline around the shared actorIDs
// map), only swapping in the asynchronous agent harness.
func setupAsyncDeliveryAgents(t *testing.T, db *postgres.Store, namespaceID string) (*asyncDeliveryAgents, string) {
	t.Helper()

	agentIDs := map[string]string{}
	agents := newAsyncDeliveryAgents(t, agentIDs)
	registered, runnerID := registerActors(t, db, namespaceID, agents.server.URL)
	agents.mu.Lock()
	for node, id := range registered {
		agentIDs[node] = id
	}
	agents.mu.Unlock()

	return agents, runnerID
}

func (a *asyncDeliveryAgents) handle(w http.ResponseWriter, r *http.Request) {
	var req actors.InvocationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad invocation", http.StatusBadRequest)
		return
	}

	a.mu.Lock()
	a.calls[req.Node.ID]++
	state := invocationState{
		call:        a.calls[req.Node.ID],
		verifyCalls: a.calls["verify"],
		actorID:     a.actorIDs[req.Node.ID],
	}
	a.mu.Unlock()

	// The scripted judgement is identical to the synchronous harness; only
	// the transport below differs.
	scripted := &deliveryAgents{verifyRequestsChanges: false}
	result, err := scripted.script(req, state)
	if err != nil {
		a.mu.Lock()
		a.failures = append(a.failures, err.Error())
		a.mu.Unlock()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(actors.AsyncAccepted{
		InvocationID:          "async_" + req.AttemptID,
		HeartbeatAfterSeconds: 30,
	})

	// Report the terminal event through the real §13.1 callback URL, off the
	// request goroutine — exactly what a genuinely asynchronous actor does:
	// answer 202 to free the worker, then call back later with the result.
	go a.reportCompletion(req, result)
}

func (a *asyncDeliveryAgents) reportCompletion(req actors.InvocationRequest, result actors.InvocationResult) {
	fail := func(format string, args ...any) {
		a.mu.Lock()
		a.failures = append(a.failures, fmt.Sprintf(format, args...))
		a.mu.Unlock()
	}

	payload, err := json.Marshal(actors.CompletedPayload{
		Outcome:     result.Outcome,
		Output:      result.Output,
		LedgerDelta: result.LedgerDelta,
	})
	if err != nil {
		fail("node %s: encode completed payload: %v", req.Node.ID, err)
		return
	}
	body, err := json.Marshal(actors.CallbackEvent{
		EventID:  "ev_" + req.AttemptID,
		Sequence: 1,
		Kind:     actors.EventCompleted,
		Payload:  payload,
	})
	if err != nil {
		fail("node %s: encode callback event: %v", req.Node.ID, err)
		return
	}

	// Deliberately a SINGLE attempt, with no client-side retry: this fake
	// actor answers 202 and reports completion from its own goroutine as
	// fast as it can, exactly as a near-instant backend (a mock engine, for
	// instance) would. Before internal/actors/callback.go's HandleCallback
	// grew its own tolerance for this race (lookupInvocation), this raced
	// the worker's own park write (internal/worker/dispatch.go, after the
	// 202) often enough to 404 outright — with nothing here retrying, that
	// permanently stranded the run in waiting_external and left run.output
	// null forever, which is exactly the failure this test reproduces
	// without the fix and proves closed with it.
	httpReq, err := http.NewRequest(http.MethodPost, req.Callback.URL, bytes.NewReader(body))
	if err != nil {
		fail("node %s: build callback request: %v", req.Node.ID, err)
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.Callback.Token)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		fail("node %s: POST callback: %v", req.Node.ID, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		fail("node %s: callback answered %d: %s", req.Node.ID, resp.StatusCode, b)
	}
}

func (a *asyncDeliveryAgents) scriptFailures() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.failures...)
}

func (a *asyncDeliveryAgents) callCount(nodeID string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[nodeID]
}

// TestRunOutputResolvesTheEndNodeBindingThroughAnAsyncActorCallback publishes
// and runs examples/delivery-loop entirely through the real HTTP API (the
// same POST /v1alpha1/workflows, POST /v1alpha1/runs, and
// GET /v1alpha1/runs/{id} the web front, the Python CLI, and smoke.sh use),
// with every agent node completing through a genuine §13.3/§13.4 asynchronous
// round trip. It asserts the completed run's output — resolved by `finish`'s
// /nodes/verify/output binding — is present and correct, not null.
func TestRunOutputResolvesTheEndNodeBindingThroughAnAsyncActorCallback(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ns := pgtest.MustNamespace(t, s, "e2e-async-output")

	agents, runnerID := setupAsyncDeliveryAgents(t, s, ns.ID)
	runner := &scriptedRunner{}

	stack := startStack(t, stackConfig{
		namespaceID:   ns.ID,
		agentsURL:     agents.server.URL,
		runner:        runner,
		runnerName:    headspace.RunnerName,
		runnerActorID: runnerID,
	})
	defer stack.stop()

	digest := stack.publishWorkflow(t)
	runID := stack.createRun(t, digest, []byte(`{"request":"add a /healthz endpoint","repository":"example/service"}`))
	t.Cleanup(func() {
		if t.Failed() {
			dumpRunState(t, stack, runID)
			t.Logf("agent script failures: %v", agents.scriptFailures())
		}
	})

	view := stack.waitForTerminal(t, runID, 90*time.Second)
	if failures := agents.scriptFailures(); len(failures) > 0 {
		t.Fatalf("the async agents reported a failure: %v", failures)
	}
	if view.Run.State != string(engine.RunCompleted) {
		dumpRunState(t, stack, runID)
		t.Fatalf("run state = %s, want completed (worker errors: %v)", view.Run.State, stack.errors())
	}

	// The regression itself: `finish` binds /nodes/verify/output, and
	// verify's own completion committed through the async callback path.
	// run.output, read back through the very endpoint the web front and the
	// CLI read, must carry it — not null, not absent.
	if len(view.Run.Output) == 0 || bytes.Equal(bytes.TrimSpace(view.Run.Output), []byte("null")) {
		t.Fatalf("run output = %q, want the verifier's passing verdict (not null): "+
			"the end-node binding did not resolve through the async callback path", view.Run.Output)
	}
	if !bytes.Contains(view.Run.Output, []byte(`"passed"`)) {
		t.Errorf("run output = %s, want the verifier's passing verdict", view.Run.Output)
	}

	if got := agents.callCount("verify"); got != 1 {
		t.Errorf("verify was invoked %d times, want 1 (verifyRequestsChanges is off)", got)
	}
}
