package e2etest

import (
	"bytes"
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/runners/headspace"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// TestPhase1VerticalSlice is the acceptance run: the reference workflow from
// examples/delivery-loop, published and started through the HTTP API, driven
// autonomously by a real worker and a real scheduler against a real
// PostgreSQL — through a simulated process restart — until it completes.
//
// It asserts, in order:
//
//  1. the run pins the published workflow digest and completes;
//  2. `verify.changes_required` was walked EXACTLY once, as a domain
//     outcome, with the attempt that produced it recorded as technically
//     SUCCEEDED (PRD §3.4);
//  3. the `test` code node appended runner-observed evidence (authority
//     observed, origin runner) through the ledger's own authority matrix;
//  4. build's completion claim stayed `proposed` — no agent-origin record in
//     the run ever reached confirmed, and the projections say so;
//  5. every committed transition emitted an event, with per-run monotonic
//     sequence numbers and no gaps, streamed over SSE;
//  6. the run survived a full restart: the control plane was torn down after
//     build's first pass, the pool closed, and a brand-new stack finished
//     the run from PostgreSQL alone;
//  7. the Run-view payload carries everything the web front renders.
func TestPhase1VerticalSlice(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ns := pgtest.MustNamespace(t, s, "e2e-slice")

	runner := &scriptedRunner{}
	agents, runnerID := setupDeliveryAgentsAndActors(t, s, ns.ID)

	first, runID, digest, buildsBeforeRestart := publishAndStartRun(t, s, ns.ID, agents, runner, runnerID)

	second, sseDone := restartStack(t, first, ns.ID, agents, runner, runnerID, runID)
	defer second.stop()

	view := second.waitForTerminal(t, runID, 90*time.Second)
	if failures := agents.scriptFailures(); len(failures) > 0 {
		t.Fatalf("the scripted agents refused an invocation: %v", failures)
	}

	// ---- 1. The run completed against the pinned digest ----
	assertRunCompleted(t, second, view, digest)

	// ---- 6. Restart survival ----
	assertRestartSurvival(t, agents, buildsBeforeRestart)

	// ---- 2. changes_required is a DOMAIN outcome, walked exactly once ----
	assertChangesRequiredOnce(t, view)

	// The edge itself was followed, once, and its event says so.
	events, sse := drainSSE(t, sseDone)
	if sse != nil {
		t.Fatalf("SSE stream: %v", sse)
	}
	assertEdgeTransitions(t, events)
	// No attempt anywhere in the run failed technically. The loop is domain
	// routing from end to end.
	assertNoTechnicalFailures(t, view)

	// ---- 3 & 4. Ledger honesty: runner-observed evidence, agent-origin
	// records stay proposed, and the projections agree ----
	assertLedgerHonesty(t, second, ns.ID, runID, runnerID)

	// ---- 5. Every committed transition emitted an event ----
	assertEventDensity(t, second, ns.ID, runID, events)

	// ---- 7. The Run view carries what the web front renders ----
	assertRunViewContract(t, second, runID, view, digest)

	// The code node genuinely went through the runner boundary: two typed,
	// digest-pinned operations, no shell.
	assertRunnerOperations(t, runner)
}

// setupDeliveryAgentsAndActors brings up the scripted agents server and
// registers the actors rows the reference workflow's `uses` references
// resolve against. The agents server must exist before the actors rows can
// name its URL, and the rows must name the actor ids the agents stamp on
// their records — so the map is filled in after both exist and the agents
// read it under their own lock at request time.
func setupDeliveryAgentsAndActors(t *testing.T, db *postgres.Store, namespaceID string) (*deliveryAgents, string) {
	t.Helper()

	agentIDs := map[string]string{}
	agents := newDeliveryAgents(t, agentIDs)
	registered, runnerID := registerActors(t, db, namespaceID, agents.server.URL)
	agents.mu.Lock()
	for node, id := range registered {
		agentIDs[node] = id
	}
	agents.mu.Unlock()

	return agents, runnerID
}

// publishAndStartRun runs Phase 1: it starts the first incarnation of the
// control plane with a stop predicate that halts the worker the moment
// build's first pass has committed — checked between dispatches, so the
// restart point is exact and the `test` node is guaranteed not to have been
// claimed — publishes the reference workflow, creates a run, and waits for
// the worker to reach its stop point. It then asserts the pre-restart
// invariants captured while the first incarnation is still alive, so the
// post-restart assertions can prove the run genuinely continued rather than
// started over.
func publishAndStartRun(t *testing.T, db *postgres.Store, namespaceID string, agents *deliveryAgents, runner *scriptedRunner, runnerID string) (first *stack, runID, digest string, buildsBeforeRestart int) {
	t.Helper()

	var runIDHolder atomic.Value
	first = startStack(t, stackConfig{
		namespaceID:   namespaceID,
		agentsURL:     agents.server.URL,
		runner:        runner,
		runnerName:    headspace.RunnerName,
		runnerActorID: runnerID,
		stopAfter: func() bool {
			id, _ := runIDHolder.Load().(string)
			return id != "" && countCompletedNodeRuns(db, id, "build") >= 1
		},
	})

	digest = first.publishWorkflow(t)
	runID = first.createRun(t, digest, json.RawMessage(`{"request":"add a /healthz endpoint","repository":"example/service"}`))
	runIDHolder.Store(runID)

	first.awaitWorkerStoppedOrDump(t, runID, 60*time.Second)

	buildsBeforeRestart = agents.callCount("build")
	if buildsBeforeRestart != 1 {
		t.Fatalf("build was invoked %d times before the restart, want exactly 1", buildsBeforeRestart)
	}
	if got := len(runner.operations()); got != 0 {
		t.Fatalf("the runner ran %d operations before the restart; the `test` node comes after build", got)
	}
	if state := runState(t, first.db, runID); state != engine.RunRunning {
		t.Fatalf("run state before the restart = %s, want running", state)
	}
	return first, runID, digest, buildsBeforeRestart
}

// restartStack is the restart itself: nothing survives but PostgreSQL. It
// tears the first incarnation down, brings up a second one against the same
// namespace, and attaches an SSE consumer to the NEW server from sequence
// 0 — including every event the previous, now-dead incarnation committed,
// which is what a client that missed a whole process incarnation does. It
// does not wait for the run to finish; the caller observes that through the
// returned stack and channel.
func restartStack(t *testing.T, first *stack, namespaceID string, agents *deliveryAgents, runner *scriptedRunner, runnerID, runID string) (*stack, <-chan sseResult) {
	t.Helper()

	first.stop()

	second := startStack(t, stackConfig{
		namespaceID:   namespaceID,
		agentsURL:     agents.server.URL,
		runner:        runner,
		runnerName:    headspace.RunnerName,
		runnerActorID: runnerID,
	})

	sseDone := make(chan sseResult, 1)
	go func() {
		events, err := streamRunEvents(t, second.server.URL, runID, 90*time.Second)
		sseDone <- sseResult{events: events, err: err}
	}()

	return second, sseDone
}

// assertRunCompleted checks assertion 1: the run completed against the
// pinned digest and its output carries the verifier's passing verdict.
func assertRunCompleted(t *testing.T, s *stack, view runView, digest string) {
	t.Helper()
	if view.Run.State != string(engine.RunCompleted) {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", view.Run.State, s.errors())
	}
	if view.Run.WorkflowDigest != digest {
		t.Errorf("run pinned digest %q, want the published %q", view.Run.WorkflowDigest, digest)
	}
	if !bytes.Contains(view.Run.Output, []byte(`"passed"`)) {
		t.Errorf("run output = %s, want the verifier's passing verdict", view.Run.Output)
	}
}

// assertRestartSurvival checks assertion 6: the restart resumed the run
// rather than restarting it, and the restart point really was build pass 1.
func assertRestartSurvival(t *testing.T, agents *deliveryAgents, buildsBeforeRestart int) {
	t.Helper()
	if got := agents.callCount("build"); got != 2 {
		t.Errorf("build was invoked %d times, want exactly 2 (one loop iteration)", got)
	}
	if got := agents.callCount("intake"); got != 1 {
		t.Errorf("intake was invoked %d times, want 1: the restart must resume, not restart the run", got)
	}
	if got := agents.callCount("plan"); got != 1 {
		t.Errorf("plan was invoked %d times, want 1: the restart must resume, not restart the run", got)
	}
	if buildsBeforeRestart != 1 {
		t.Errorf("build ran %d times before the restart; the restart point is meant to be build pass 1", buildsBeforeRestart)
	}
}

// assertChangesRequiredOnce checks assertion 2: verify.changes_required was
// walked exactly once, and the node run and attempt that produced it are
// shaped the way a domain outcome must be — completed, one attempt,
// technically succeeded.
func assertChangesRequiredOnce(t *testing.T, view runView) {
	t.Helper()
	verifyRuns := nodeRunsFor(view, "verify")
	if len(verifyRuns) != 2 {
		t.Fatalf("verify ran %d times, want 2", len(verifyRuns))
	}
	changesRequired := 0
	for _, nr := range verifyRuns {
		if nr.Outcome != "changes_required" {
			continue
		}
		changesRequired++
		if nr.State != string(engine.NodeRunCompleted) {
			t.Errorf("the changes_required node run is %s, want completed", nr.State)
		}
		if len(nr.Attempts) != 1 {
			t.Fatalf("the changes_required node run has %d attempts, want 1", len(nr.Attempts))
		}
		if got := nr.Attempts[0].Status; got != string(engine.StatusSucceeded) {
			t.Errorf("the attempt that reported changes_required has technical status %q, want succeeded: "+
				"an expected negative outcome follows a graph edge, it does not masquerade as a runtime failure (PRD §3.4)", got)
		}
	}
	if changesRequired != 1 {
		t.Errorf("verify returned changes_required %d times, want exactly 1", changesRequired)
	}
}

// assertEdgeTransitions checks the edge-transition counts that go with
// assertion 2: the loop edges were each walked the expected number of times.
func assertEdgeTransitions(t *testing.T, events []sseEvent) {
	t.Helper()
	if got := countEdgeTransitions(events, "verify.changes_required"); got != 1 {
		t.Errorf("the verify.changes_required edge was walked %d times, want exactly 1", got)
	}
	if got := countEdgeTransitions(events, "verify.passed"); got != 1 {
		t.Errorf("the verify.passed edge was walked %d times, want exactly 1", got)
	}
	if got := countEdgeTransitions(events, "test.passed"); got != 2 {
		t.Errorf("the test.passed edge was walked %d times, want 2", got)
	}
}

// assertNoTechnicalFailures checks that no attempt anywhere in the run
// failed technically: the loop is domain routing from end to end.
func assertNoTechnicalFailures(t *testing.T, view runView) {
	t.Helper()
	for _, nr := range view.NodeRuns {
		for _, attempt := range nr.Attempts {
			if attempt.Status != string(engine.StatusSucceeded) {
				t.Errorf("node %s attempt %d status = %q; no attempt in this run should fail technically (result: %s)",
					nr.NodeID, attempt.AttemptNumber, attempt.Status, attempt.Result)
			}
		}
	}
}

// assertLedgerHonesty checks assertions 3 and 4: the code node's evidence
// carries runner authority and observed origin, agent-origin records never
// escape `proposed`, and the confirmed_claims / delivery_summary
// projections agree with both.
func assertLedgerHonesty(t *testing.T, s *stack, namespaceID, runID, runnerID string) {
	t.Helper()

	led := ledgerFor(t, s.db, namespaceID)
	records, err := led.Records(context.Background(), runID)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}

	assertRunnerObservedEvidence(t, records, runnerID)
	assertAgentRecordsStayProposed(t, records)
	assertConfirmedClaimsProjection(t, led, runID)
	assertDeliverySummaryProjection(t, led, runID)
}

// filterRecords returns the subset of records whose RecordType is
// recordType.
func filterRecords(records []ledger.Record, recordType ledger.RecordType) []ledger.Record {
	var out []ledger.Record
	for _, rec := range records {
		if rec.RecordType == recordType {
			out = append(out, rec)
		}
	}
	return out
}

// assertRunnerObservedEvidence checks the `test` code node appended
// runner-observed evidence — authority observed, origin runner — through
// the ledger's own authority matrix.
func assertRunnerObservedEvidence(t *testing.T, records []ledger.Record, runnerID string) {
	t.Helper()
	evidence := filterRecords(records, ledger.RecordEvidence)
	if len(evidence) != 2 {
		t.Fatalf("run has %d evidence records, want 2 (one per code-node run)", len(evidence))
	}
	for _, rec := range evidence {
		assertEvidenceRecord(t, rec, runnerID)
	}
}

func assertEvidenceRecord(t *testing.T, rec ledger.Record, runnerID string) {
	t.Helper()
	if rec.Authority != ledger.AuthorityObserved {
		t.Errorf("evidence %s authority = %q, want observed", rec.ID, rec.Authority)
	}
	if rec.Origin.Kind != ledger.OriginRunner {
		t.Errorf("evidence %s origin kind = %q, want runner", rec.ID, rec.Origin.Kind)
	}
	if rec.Origin.ActorID != runnerID {
		t.Errorf("evidence %s actor = %q, want the registered runner %q", rec.ID, rec.Origin.ActorID, runnerID)
	}
	data, decodeErr := rec.DataMap()
	if decodeErr != nil {
		t.Fatalf("decode evidence payload: %v", decodeErr)
	}
	if _, ok := data["covered_scope"]; !ok {
		t.Errorf("evidence %s declares no covered_scope; scoped evidence is the point (PRD §10.5)", rec.ID)
	}
	measurements, _ := data["measurements"].(map[string]any)
	if _, ok := measurements["exit_code"]; !ok {
		t.Errorf("evidence %s carries no measured exit_code: %s", rec.ID, rec.Data)
	}
}

// assertAgentRecordsStayProposed checks build's completion claim (and every
// other agent-origin record) stayed `proposed`: no agent-origin record in
// the run ever reached confirmed.
func assertAgentRecordsStayProposed(t *testing.T, records []ledger.Record) {
	t.Helper()
	var claims, agentRecords int
	for _, rec := range records {
		if rec.Origin.Kind != ledger.OriginAgent {
			continue
		}
		agentRecords++
		if rec.Authority != ledger.AuthorityProposed {
			t.Errorf("agent-origin record %s (%s) has authority %q; an agent may only propose (PRD §10.4)",
				rec.ID, rec.RecordType, rec.Authority)
		}
		if rec.RecordType == ledger.RecordClaim {
			claims++
		}
	}
	if agentRecords == 0 {
		t.Fatal("no agent-origin records were appended; the ledger contract was never exercised")
	}
	if claims < 3 {
		t.Errorf("run has %d agent claims, want at least 3 (intake's, and build's two completion claims)", claims)
	}
}

func assertConfirmedClaimsProjection(t *testing.T, led *ledger.Ledger, runID string) {
	t.Helper()
	confirmed, err := led.ProjectRun(context.Background(), runID, ledger.KindConfirmedClaims, "")
	if err != nil {
		t.Fatalf("project confirmed_claims: %v", err)
	}
	if len(confirmed.Items) != 0 {
		t.Errorf("confirmed_claims projects %d items; nothing in this run was ever confirmed by a human review",
			len(confirmed.Items))
	}
}

func assertDeliverySummaryProjection(t *testing.T, led *ledger.Ledger, runID string) {
	t.Helper()
	summary, err := led.ProjectRun(context.Background(), runID, ledger.KindDeliverySummary, "")
	if err != nil {
		t.Fatalf("project delivery_summary: %v", err)
	}
	if summary.Summary == nil {
		t.Fatal("delivery_summary carries no counts")
	}
	if summary.Summary.ConfirmedClaims != 0 {
		t.Errorf("delivery_summary confirmed_claims = %d, want 0", summary.Summary.ConfirmedClaims)
	}
	if summary.Summary.UndecidedClaims < 3 {
		t.Errorf("delivery_summary undecided_claims = %d, want at least 3: build's completion claim is unverified",
			summary.Summary.UndecidedClaims)
	}
	if summary.Summary.ResultsAwaitingReview == 0 {
		t.Error("delivery_summary results_awaiting_review = 0; build's own result was never reviewed by anyone")
	}
	if summary.Summary.EvidenceRecords != 2 {
		t.Errorf("delivery_summary evidence_records = %d, want 2", summary.Summary.EvidenceRecords)
	}
}

// assertEventDensity checks assertion 5: every committed transition emitted
// an event, with per-run monotonic sequence numbers and no gaps, and the
// events table agrees with what the SSE stream delivered.
func assertEventDensity(t *testing.T, s *stack, namespaceID, runID string, events []sseEvent) {
	t.Helper()
	assertMonotonicSequences(t, events)

	dbSequences := eventSequences(t, s.db, namespaceID, runID)
	if len(dbSequences) != len(events) {
		t.Errorf("the events table holds %d rows for this run but the SSE stream delivered %d",
			len(dbSequences), len(events))
	}
	for i, seq := range dbSequences {
		if seq != int64(i+1) {
			t.Fatalf("events sequence %d is %d; a run's event sequence must be dense and monotonic", i, seq)
		}
	}

	// One token.transitioned event per transition, and the run's final
	// transition count agrees with them.
	transitions := 0
	for _, ev := range events {
		if ev.Type == engine.TypeTokenTransitioned {
			transitions++
		}
	}
	// intake→plan, plan→build, build→test, test→verify, verify→build,
	// build→test, test→verify, verify→finish.
	if transitions != 8 {
		t.Errorf("the run emitted %d transition events, want 8 for one loop iteration", transitions)
	}
	if !hasEventType(events, engine.TypeRunCreated) {
		t.Error("no run.created event")
	}
	if !hasEventType(events, engine.TypeRunCompleted) {
		t.Error("no run.completed event")
	}
	if !hasEventType(events, engine.TypeLedgerAppended) {
		t.Error("no ledger.record-appended event")
	}
}

// assertRunnerOperations checks the code node genuinely went through the
// runner boundary: two typed, digest-pinned operations, no shell.
func assertRunnerOperations(t *testing.T, runner *scriptedRunner) {
	t.Helper()
	ops := runner.operations()
	if len(ops) != 2 {
		t.Fatalf("the runner executed %d operations, want 2", len(ops))
	}
	for _, op := range ops {
		if op.Execution.ImageDigest == "" {
			t.Error("a dispatched operation carried no pinned image digest")
		}
		if op.Command.RequiresShell != nil && *op.Command.RequiresShell {
			t.Error("a dispatched operation asked for a shell; the reference workflow declares an argv array")
		}
		if len(op.Command.Argv) == 0 {
			t.Error("a dispatched operation carried no argv")
		}
	}
}

// assertRunViewContract checks the payload the web front's Run view renders
// from (web/src/api, t20). The browser end-to-end test lives in web/; this is
// the data contract behind it.
func assertRunViewContract(t *testing.T, s *stack, runID string, view runView, digest string) {
	t.Helper()

	assertRunViewShape(t, view, runID)
	assertRunViewNodeRuns(t, view)
	assertWorkflowVersionPayload(t, s, digest)
	assertLedgerEndpointPayload(t, s, runID)
}

// assertRunViewShape checks the top-level Run-view fields the Run page reads
// directly.
func assertRunViewShape(t *testing.T, view runView, runID string) {
	t.Helper()
	if view.Run.ID != runID {
		t.Errorf("run view id = %q, want %q", view.Run.ID, runID)
	}
	if len(view.Run.Input) == 0 {
		t.Error("run view carries no input; the Run page shows it")
	}
	if len(view.Tokens) == 0 {
		t.Error("run view carries no tokens; the graph draws the control path from them")
	}
}

// assertRunViewNodeRuns checks every node the run touched is present, with a
// state and every attempt under it, and that the reference workflow's whole
// node set is represented.
func assertRunViewNodeRuns(t *testing.T, view runView) {
	t.Helper()
	seen := map[string]bool{}
	for _, nr := range view.NodeRuns {
		seen[nr.NodeID] = true
		assertNodeRunShape(t, nr)
	}
	for _, node := range []string{"intake", "plan", "build", "test", "verify", "finish"} {
		if !seen[node] {
			t.Errorf("the run view omits node %q, which the graph must render", node)
		}
	}
}

func assertNodeRunShape(t *testing.T, nr nodeRunView) {
	t.Helper()
	if nr.State == "" {
		t.Errorf("node run %s has no state", nr.ID)
	}
	if nr.VisitCount < 1 {
		t.Errorf("node run %s has visit_count %d", nr.ID, nr.VisitCount)
	}
	for _, attempt := range nr.Attempts {
		if attempt.ID == "" || attempt.AttemptNumber < 1 || attempt.Status == "" {
			t.Errorf("node %s carries an incomplete attempt: %+v", nr.NodeID, attempt)
		}
		if attempt.FencingToken <= 0 {
			t.Errorf("node %s attempt %d records no fencing token", nr.NodeID, attempt.AttemptNumber)
		}
	}
}

// assertWorkflowVersionPayload checks the IR the graph is drawn from,
// fetched by the digest the run pins.
func assertWorkflowVersionPayload(t *testing.T, s *stack, digest string) {
	t.Helper()
	var version struct {
		Digest       string          `json:"digest"`
		NormalizedIR json.RawMessage `json:"normalized_ir"`
		Source       string          `json:"source"`
	}
	if status := s.getJSON("/v1alpha1/workflows/"+digest, &version); status != 200 {
		t.Fatalf("GET workflow %s: status %d", digest, status)
	}
	if version.Digest != digest {
		t.Errorf("workflow digest = %q, want %q", version.Digest, digest)
	}
	var ir struct {
		Spec struct {
			Nodes map[string]json.RawMessage `json:"nodes"`
			Edges []json.RawMessage          `json:"edges"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(version.NormalizedIR, &ir); err != nil {
		t.Fatalf("decode normalized IR: %v", err)
	}
	// intake, plan, build, test, verify, human-review, finish — and the ten
	// edges PRD §11.1 declares between them, including the human-review
	// branch tests/e2e/humanreview_test.go drives. This run never reaches
	// that branch (its verifier never reports `blocked`), which is the point
	// of asserting the shape here: the graph the front end draws is the whole
	// definition, not only the path this run walked.
	if len(ir.Spec.Nodes) != 7 {
		t.Errorf("the IR declares %d nodes, want 7", len(ir.Spec.Nodes))
	}
	if len(ir.Spec.Edges) != 10 {
		t.Errorf("the IR declares %d edges, want 10", len(ir.Spec.Edges))
	}
}

// assertLedgerEndpointPayload checks the Ledger view's own endpoint.
func assertLedgerEndpointPayload(t *testing.T, s *stack, runID string) {
	t.Helper()
	var ledgerOut struct {
		Items         []ledger.Record `json:"items"`
		LedgerVersion int64           `json:"ledger_version"`
	}
	if status := s.getJSON("/v1alpha1/runs/"+runID+"/ledger", &ledgerOut); status != 200 {
		t.Fatalf("GET ledger: status %d", status)
	}
	if len(ledgerOut.Items) == 0 || ledgerOut.LedgerVersion == 0 {
		t.Errorf("ledger endpoint returned %d items at version %d", len(ledgerOut.Items), ledgerOut.LedgerVersion)
	}
}

// -----------------------------------------------------------------------
// Assertion helpers
// -----------------------------------------------------------------------

// sseResult carries a finished SSE consumption back to the test goroutine.
type sseResult struct {
	events []sseEvent
	err    error
}

func drainSSE(t *testing.T, ch <-chan sseResult) ([]sseEvent, error) {
	t.Helper()
	select {
	case res := <-ch:
		return res.events, res.err
	case <-time.After(30 * time.Second):
		t.Fatal("the SSE consumer never finished")
		return nil, nil
	}
}

func assertMonotonicSequences(t *testing.T, events []sseEvent) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("the SSE stream delivered no events")
	}
	previous := int64(0)
	for _, ev := range events {
		if ev.Sequence <= previous {
			t.Fatalf("event sequence %d followed %d; a run's sequence must be strictly monotonic", ev.Sequence, previous)
		}
		previous = ev.Sequence
	}
}

func hasEventType(events []sseEvent, want string) bool {
	for _, ev := range events {
		if ev.Type == want {
			return true
		}
	}
	return false
}

// countEdgeTransitions counts token.transitioned events whose payload names
// edge as the edge that was followed.
func countEdgeTransitions(events []sseEvent, edge string) int {
	count := 0
	for _, ev := range events {
		if ev.Type != engine.TypeTokenTransitioned {
			continue
		}
		var envelope struct {
			Data struct {
				Edge string `json:"edge"`
			} `json:"data"`
		}
		if err := json.Unmarshal(ev.Data, &envelope); err != nil {
			continue
		}
		if envelope.Data.Edge == edge {
			count++
		}
	}
	return count
}

func nodeRunsFor(view runView, nodeID string) []nodeRunView {
	var out []nodeRunView
	for _, nr := range view.NodeRuns {
		if nr.NodeID == nodeID {
			out = append(out, nr)
		}
	}
	return out
}

// countCompletedNodeRuns is the stop predicate's own read. It never touches
// *testing.T: it runs on the worker's goroutine, where a t.Fatalf would be a
// data race and a lie about which test failed. A query error simply reads as
// "not yet", and the surrounding timeout is what turns a persistent failure
// into a test failure — with dumpRunState's diagnostics attached.
func countCompletedNodeRuns(db *postgres.Store, runID, nodeKey string) int {
	var count int
	err := db.Pool().QueryRow(context.Background(), `
		SELECT count(*) FROM node_runs WHERE run_id = $1 AND node_key = $2 AND status = 'completed'
	`, runID, nodeKey).Scan(&count)
	if err != nil {
		return 0
	}
	return count
}

// awaitWorkerStoppedOrDump waits for a supervised worker's stopAfter
// predicate to end its loop, dumping every attempt the run recorded if it
// never does. A slice test that only says "timed out" is one nobody can
// debug.
func (s *stack) awaitWorkerStoppedOrDump(t *testing.T, runID string, timeout time.Duration) {
	t.Helper()
	select {
	case <-s.workerDone:
		return
	case <-time.After(timeout):
	}
	t.Errorf("the supervised worker did not reach its stop point within %s", timeout)
	dumpRunState(t, s, runID)
	t.FailNow()
}

// dumpRunState logs the worker's errors and every attempt the run recorded.
func dumpRunState(t *testing.T, s *stack, runID string) {
	t.Helper()
	for _, err := range s.errors() {
		t.Logf("worker/scheduler error: %v", err)
	}
	rows, err := s.db.Pool().Query(context.Background(), `
		SELECT nr.node_key, nr.status, COALESCE(nr.outcome, ''),
		       COALESCE(a.attempt_number, 0), COALESCE(a.status, ''), COALESCE(a.result::text, '')
		FROM node_runs AS nr LEFT JOIN attempts AS a ON a.node_run_id = nr.id
		WHERE nr.run_id = $1 ORDER BY nr.created_at, a.attempt_number
	`, runID)
	if err != nil {
		t.Logf("dump attempts: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var node, nrStatus, outcome, attemptStatus, result string
		var attempt int
		if err := rows.Scan(&node, &nrStatus, &outcome, &attempt, &attemptStatus, &result); err != nil {
			t.Logf("scan attempt dump: %v", err)
			return
		}
		t.Logf("node %-8s node_run=%-10s outcome=%-16s attempt=%d status=%s result=%s",
			node, nrStatus, outcome, attempt, attemptStatus, result)
	}
}

func runState(t *testing.T, db *postgres.Store, runID string) engine.RunState {
	t.Helper()
	var state string
	if err := db.Pool().QueryRow(context.Background(),
		`SELECT status FROM runs WHERE id = $1`, runID).Scan(&state); err != nil {
		t.Fatalf("read run state: %v", err)
	}
	return engine.RunState(state)
}

func eventSequences(t *testing.T, db *postgres.Store, namespaceID, runID string) []int64 {
	t.Helper()
	rows, err := db.Pool().Query(context.Background(), `
		SELECT sequence FROM events
		WHERE namespace_id = $1 AND aggregate_id = $2
		ORDER BY sequence
	`, namespaceID, runID)
	if err != nil {
		t.Fatalf("read event sequences: %v", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatalf("scan sequence: %v", err)
		}
		out = append(out, seq)
	}
	return out
}
