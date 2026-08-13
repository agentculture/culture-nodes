package worker_test

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/runners"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// Task t17 (issue #37): acceptance checks are evaluated BEFORE routing and
// the node's declared `acceptance.enforce` policy decides what a failing
// check does to the completion — observe (today's behavior exactly),
// route_technical (contract_rejected, composing with the node's own retry
// policy), or route_outcome:<name> (the declared domain edge). Every
// evaluation — pass or fail, any mode — still lands as a derived,
// validator-origin ledger record; checks that are not mechanically evaluable
// stay honestly unevaluated and unenforced.
//
// Everything here runs against a real PostgreSQL, the real engine, and the
// real Worker loop, exactly like code_test.go, whose harness this file
// borrows.

// measuredWorkspaceResult is codeRunResult with the changed-paths observation
// genuinely measured, so a workspace_diff check has an honest fact to
// evaluate against. complete is what the measurement reported.
func measuredWorkspaceResult(op runners.Operation, exitCode int, complete bool) runners.Result {
	res := codeRunResult(op, exitCode)
	res.Changes = runners.Changes{Complete: complete}
	res.Observations.ChangedPaths = runners.Observation{Measured: true, Complete: true, Method: "workspace_diff"}
	return res
}

// nodeRunOutcomes returns every node run of nodeKey in creation order with
// its recorded outcome — the loop-back test's view of a node visited twice.
func (h *codeHarness) nodeRunOutcomes(runID, nodeKey string) []string {
	h.t.Helper()
	rows, err := h.store.Pool().Query(h.ctx, `
		SELECT COALESCE(outcome, '')
		FROM node_runs
		WHERE run_id = $1 AND node_key = $2
		ORDER BY created_at, id
	`, runID, nodeKey)
	if err != nil {
		h.t.Fatalf("read node runs: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var outcome string
		if err := rows.Scan(&outcome); err != nil {
			h.t.Fatalf("scan node run outcome: %v", err)
		}
		out = append(out, outcome)
	}
	return out
}

// verdictPayload decodes one review record's payload map.
func verdictPayload(t *testing.T, rec ledger.Record) map[string]any {
	t.Helper()
	data, err := rec.DataMap()
	if err != nil {
		t.Fatalf("decode acceptance verdict payload: %v", err)
	}
	return data
}

// requireDerivedValidator asserts the invariant every acceptance record must
// hold whatever the enforce mode did: derived authority, validator origin.
func requireDerivedValidator(t *testing.T, rec ledger.Record) {
	t.Helper()
	if rec.Authority != ledger.AuthorityDerived {
		t.Errorf("acceptance record authority = %q, want derived", rec.Authority)
	}
	if rec.Origin.Kind != ledger.OriginValidator {
		t.Errorf("acceptance record origin kind = %q, want validator", rec.Origin.Kind)
	}
}

// route_technical with a retry budget: the failing pre-announced check
// converts each completion to contract_rejected, the node's declared
// maxAttempts: 2 grants exactly one retry, and after honest exhaustion the
// run fails. The node's own `failed` domain edge is never followed — the
// enforce policy turned the completion into a technical status, not a domain
// answer.
func TestEnforceRouteTechnicalRetriesPerDeclaredPolicyThenFailsHonestly(t *testing.T) {
	h := newCodeHarness(t, func(op runners.Operation, _ int) (runners.Result, error) {
		return codeRunResult(op, 1), nil
	})

	run := h.createRun("code-enforce-technical-retry.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(30*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunFailed {
		t.Fatalf("run state = %s, want failed (worker errors: %v)", state, h.workerErrors())
	}

	attempts := h.attemptRows(run.ID, "test")
	if len(attempts) != 2 {
		t.Fatalf("code node ran %d attempts, want 2: the declared retry policy grants exactly one retry", len(attempts))
	}
	for i, a := range attempts {
		if engine.TechStatus(a.Status) != engine.StatusContractRejected {
			t.Errorf("attempt %d status = %q, want contract_rejected", i+1, a.Status)
		}
	}
	if outcome := h.nodeOutcome(run.ID, "test"); outcome != "" {
		t.Errorf("code node outcome = %q, want none: the enforce policy converted the completion to a technical status", outcome)
	}

	// The declared `failed` domain edge must NOT have been followed.
	var giveupExists bool
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT EXISTS (SELECT 1 FROM node_runs WHERE run_id = $1 AND node_key = 'giveup')`, run.ID).Scan(&giveupExists); err != nil {
		t.Fatalf("look for the giveup node run: %v", err)
	}
	if giveupExists {
		t.Error("the failed edge was followed to giveup; route_technical must not route a domain outcome")
	}

	// Every evaluation appends a derived record: one per attempt.
	reviews := h.reviewRecords(run.ID)
	if len(reviews) != 2 {
		t.Fatalf("run has %d review records, want 2 (one evaluation per attempt)", len(reviews))
	}
	for _, rec := range reviews {
		requireDerivedValidator(t, rec)
		payload := verdictPayload(t, rec)
		if payload["verdict"] != "reject" {
			t.Errorf("acceptance verdict = %v, want reject", payload["verdict"])
		}
		enforcement, _ := payload["enforcement"].(string)
		if !strings.Contains(enforcement, "contract_rejected") {
			t.Errorf("enforcement = %q, want it to record the conversion to contract_rejected", enforcement)
		}
	}

	// The runner's measured evidence still rides along on each attempt: a
	// rejected completion is still a real, measured execution.
	if got := len(h.evidenceRecords(run.ID)); got != 2 {
		t.Errorf("evidence records = %d, want 2 (one per attempt)", got)
	}
}

// route_technical without a retry budget (maxAttempts: 1): exactly one
// attempt, contract_rejected, run failed. No retry is invented for a node
// that declared none — no silent unbounded paid retries.
func TestEnforceRouteTechnicalWithoutRetryBudgetFailsOnFirstAttempt(t *testing.T) {
	h := newCodeHarness(t, func(op runners.Operation, _ int) (runners.Result, error) {
		return codeRunResult(op, 1), nil
	})

	run := h.createRun("code-enforce-technical-noretry.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunFailed {
		t.Fatalf("run state = %s, want failed (worker errors: %v)", state, h.workerErrors())
	}
	attempts := h.attemptRows(run.ID, "test")
	if len(attempts) != 1 {
		t.Fatalf("code node ran %d attempts, want exactly 1: the node declared no retry", len(attempts))
	}
	if engine.TechStatus(attempts[0].Status) != engine.StatusContractRejected {
		t.Errorf("attempt status = %q, want contract_rejected", attempts[0].Status)
	}
	reviews := h.reviewRecords(run.ID)
	if len(reviews) != 1 {
		t.Fatalf("run has %d review records, want 1", len(reviews))
	}
	requireDerivedValidator(t, reviews[0])
}

// route_outcome:<name> follows the declared domain edge — here a loop-back
// edge to the node itself. Visit one measures an incomplete workspace diff,
// fails the pre-announced check, and is re-routed from `passed` to
// `needs_fix`, whose edge loops back; visit two measures a complete diff,
// passes, and the run finishes. Each visit is exactly one attempt: a domain
// answer is never retried, even though maxAttempts: 2 would have allowed the
// engine to.
func TestEnforceRouteOutcomeFollowsTheDeclaredLoopBackEdgeWithoutRetry(t *testing.T) {
	h := newCodeHarness(t, func(op runners.Operation, call int) (runners.Result, error) {
		return measuredWorkspaceResult(op, 0, call >= 2), nil
	})

	run := h.createRun("code-enforce-outcome-loop.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(30*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", state, h.workerErrors())
	}

	outcomes := h.nodeRunOutcomes(run.ID, "test")
	if len(outcomes) != 2 || outcomes[0] != "needs_fix" || outcomes[1] != "passed" {
		t.Fatalf("test node run outcomes = %v, want [needs_fix passed]: the enforce policy re-routes visit one down the loop-back edge", outcomes)
	}

	// One attempt per visit: re-routing is a domain answer, not a retry.
	attempts := h.attemptRows(run.ID, "test")
	if len(attempts) != 2 {
		t.Fatalf("attempts across both visits = %d, want 2 (one per visit, no retries)", len(attempts))
	}
	for i, a := range attempts {
		if engine.TechStatus(a.Status) != engine.StatusSucceeded {
			t.Errorf("attempt %d status = %q, want succeeded: route_outcome routes a domain answer", i+1, a.Status)
		}
	}

	reviews := h.reviewRecords(run.ID)
	if len(reviews) != 2 {
		t.Fatalf("run has %d review records, want 2 (one evaluation per visit)", len(reviews))
	}
	first, second := verdictPayload(t, reviews[0]), verdictPayload(t, reviews[1])
	if first["verdict"] != "reject" {
		t.Errorf("visit one verdict = %v, want reject", first["verdict"])
	}
	if enforcement, _ := first["enforcement"].(string); !strings.Contains(enforcement, "needs_fix") {
		t.Errorf("visit one enforcement = %q, want it to record the re-route to needs_fix", enforcement)
	}
	if second["verdict"] != "confirm" {
		t.Errorf("visit two verdict = %v, want confirm", second["verdict"])
	}
	for _, rec := range reviews {
		requireDerivedValidator(t, rec)
	}
}

// Explicit `enforce: observe` is today's behavior exactly: the failing check
// records its reject verdict and routing is untouched — the node's own
// `failed` domain edge is followed, one attempt, run completed.
func TestEnforceObserveLeavesRoutingUnchanged(t *testing.T) {
	h := newCodeHarness(t, func(op runners.Operation, _ int) (runners.Result, error) {
		return codeRunResult(op, 1), nil
	})

	run := h.createRun("code-enforce-observe.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed via the failed edge (worker errors: %v)", state, h.workerErrors())
	}
	if outcome := h.nodeOutcome(run.ID, "test"); outcome != "failed" {
		t.Errorf("code node outcome = %q, want failed: observe never touches routing", outcome)
	}
	attempts := h.attemptRows(run.ID, "test")
	if len(attempts) != 1 {
		t.Fatalf("code node ran %d attempts, want 1", len(attempts))
	}
	if engine.TechStatus(attempts[0].Status) != engine.StatusSucceeded {
		t.Errorf("attempt status = %q, want succeeded", attempts[0].Status)
	}
	reviews := h.reviewRecords(run.ID)
	if len(reviews) != 1 {
		t.Fatalf("run has %d review records, want 1", len(reviews))
	}
	requireDerivedValidator(t, reviews[0])
	payload := verdictPayload(t, reviews[0])
	if payload["verdict"] != "reject" {
		t.Errorf("verdict = %v, want reject", payload["verdict"])
	}
}

// An enforce policy on a check whose underlying fact the runner did not
// measure: the check stays unevaluated, nothing is enforced on a verdict
// nobody computed, and routing proceeds as if observe — the exit-0 `passed`
// edge is followed. The derived record says why enforcement did not apply.
func TestEnforceOnUnmeasuredCheckIsTheHonestObserveFloor(t *testing.T) {
	h := newCodeHarness(t, func(op runners.Operation, _ int) (runners.Result, error) {
		// codeRunResult leaves the changed-paths observation unmeasured —
		// the real headspace shape today.
		return codeRunResult(op, 0), nil
	})

	run := h.createRun("code-enforce-unmeasured.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed via the passed edge (worker errors: %v)", state, h.workerErrors())
	}
	if outcome := h.nodeOutcome(run.ID, "test"); outcome != "passed" {
		t.Errorf("code node outcome = %q, want passed: an unevaluated check must not be enforced", outcome)
	}
	attempts := h.attemptRows(run.ID, "test")
	if len(attempts) != 1 || engine.TechStatus(attempts[0].Status) != engine.StatusSucceeded {
		t.Fatalf("attempts = %+v, want one succeeded attempt", attempts)
	}

	reviews := h.reviewRecords(run.ID)
	if len(reviews) != 1 {
		t.Fatalf("run has %d review records, want 1", len(reviews))
	}
	requireDerivedValidator(t, reviews[0])
	payload := verdictPayload(t, reviews[0])
	checks, _ := payload["checks"].([]any)
	if len(checks) != 1 {
		t.Fatalf("checks = %v, want one workspace_diff check", payload["checks"])
	}
	check, _ := checks[0].(map[string]any)
	if check["evaluated"] != false {
		t.Errorf("check = %v, want evaluated=false: the fact was not measured", check)
	}
	enforcement, _ := payload["enforcement"].(string)
	if !strings.Contains(enforcement, "route_technical") || !strings.Contains(enforcement, "observe") {
		t.Errorf("enforcement = %q, want it to name the declared policy and the observe floor it fell back to", enforcement)
	}
}

// An agent node under an enforce policy: an agent dispatch produces no
// runner-measured Result, so its checks are not mechanically evaluable at
// all. The honest floor: routing proceeds exactly as the agent's own outcome
// dictates, and one derived validator record states the non-evaluability —
// never a fabricated verdict from the agent's self-report.
func TestAgentNodeEnforcePolicyIsHonestlyUnevaluable(t *testing.T) {
	base := newHarness(t, func(_ *harness, w http.ResponseWriter, req actors.InvocationRequest) {
		if !bytes.Contains(req.Input, []byte(`"widget"`)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		writeSyncResult(w, "completed", `{"score":0.91,"summary":"looks good"}`)
	})

	// The derived record's validator identity must exist as a registered
	// actor (ledger_records.origin_actor_id is a real foreign key), the same
	// obligation the code path's harness meets.
	validatorID := mustHookRunnerActor(t, base.store, base.ns.ID)
	wk, err := worker.New(base.store, base.engine, worker.Options{
		WorkerID:          "worker-agent-enforce-" + t.Name(),
		NamespaceID:       base.ns.ID,
		LeaseDuration:     30 * time.Second,
		HeartbeatInterval: 200 * time.Millisecond,
		PollInterval:      20 * time.Millisecond,
		Registry: worker.StaticRegistry{
			"actor://company/analyzer": {URL: base.actorServer.URL},
		},
		Signer:            base.signer,
		CallbackBaseURL:   base.callbackServer.URL,
		CodeRunnerName:    fakeCodeRunnerName,
		CodeRunnerActorID: validatorID,
		OnError: func(err error) {
			base.mu.Lock()
			base.errs = append(base.errs, err)
			base.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	base.worker = wk

	run := base.createRun("sync-acceptance-enforce.workflow.yaml", `{"subject":"widget"}`)
	base.runUntil(20*time.Second, func() bool { return base.run(run.ID).State.Terminal() })

	if state := base.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed: non-evaluable checks must not touch routing (worker errors: %v)",
			state, base.workerErrors())
	}

	led, err := storepg.NewLedger(base.store, base.ns.ID)
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	records, err := led.Records(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("ledger.Records: %v", err)
	}
	var reviews []ledger.Record
	for _, rec := range records {
		if rec.RecordType == ledger.RecordReview {
			reviews = append(reviews, rec)
		}
	}
	if len(reviews) != 1 {
		t.Fatalf("run has %d review records, want 1 non-evaluability record", len(reviews))
	}
	requireDerivedValidator(t, reviews[0])
	payload := verdictPayload(t, reviews[0])
	checks, _ := payload["checks"].([]any)
	if len(checks) != 1 {
		t.Fatalf("checks = %v, want the one declared process_exit check", payload["checks"])
	}
	check, _ := checks[0].(map[string]any)
	if check["evaluated"] != false || check["passed"] != false {
		t.Errorf("check = %v, want evaluated=false passed=false: nothing was mechanically measured", check)
	}
	enforcement, _ := payload["enforcement"].(string)
	if !strings.Contains(enforcement, "observe") {
		t.Errorf("enforcement = %q, want it to state the observe floor", enforcement)
	}
}
