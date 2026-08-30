package engine_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

func TestCompleteAttemptUsesActorInvocationAgeForDurationPercentiles(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")

	for i := 0; i < 2; i++ {
		run := f.createRun(`{"subject":"duration"}`)
		nodeRun := f.readyNodeRun(run.ID)
		claimed := f.claim("worker-duration", nodeRun.ID)
		attemptID := "protocol-" + claimed.ID
		if _, err := f.store.Pool().Exec(f.ctx, `
			INSERT INTO actor_invocations (
				attempt_id, namespace_id, run_id, node_run_id, work_id, worker_id,
				fencing_token, attempt, node_key, actor_id, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'intake', $9, clock_timestamp() - interval '3 seconds')`,
			attemptID, f.ns.ID, run.ID, nodeRun.ID, claimed.ID, "worker-duration",
			claimed.FencingToken, claimed.Attempt, f.actor,
		); err != nil {
			t.Fatalf("seed actor invocation: %v", err)
		}

		if _, err := f.engine.CompleteAttempt(f.ctx, completion(claimed, "worker-duration", engine.CompletionRequest{
			ActorID: f.actor, TechStatus: engine.StatusSucceeded, Outcome: "completed",
			Output: json.RawMessage(`{"scope":"duration"}`),
		})); err != nil {
			t.Fatalf("CompleteAttempt: %v", err)
		}
	}

	statsStore, err := storepg.NewEngineStore(f.store, f.ns.ID)
	if err != nil {
		t.Fatalf("NewEngineStore: %v", err)
	}
	stats, err := statsStore.ActorStats(f.ctx, f.actor)
	if err != nil {
		t.Fatalf("ActorStats: %v", err)
	}
	got := stats.Total.DurationPercentiles
	if got == nil || got.Count != 2 {
		t.Fatalf("duration percentiles = %+v, want two known durations", got)
	}
	if got.P50Seconds < 2.5 || got.P90Seconds < 2.5 || got.P99Seconds < 2.5 {
		t.Fatalf("duration percentiles = %+v, want non-zero values near the seeded 3-second invocation age", got)
	}
}

func TestCompleteAttemptLeavesUnknownStartNull(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")
	run := f.createRun(`{"subject":"sync"}`)
	nodeRun := f.readyNodeRun(run.ID)
	f.step("worker-sync", nodeRun.ID, engine.CompletionRequest{
		ActorID: f.actor, TechStatus: engine.StatusSucceeded, Outcome: "completed",
		Output: json.RawMessage(`{"scope":"sync"}`),
	})

	var startedAt *time.Time
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT started_at FROM attempts WHERE node_run_id = $1`, nodeRun.ID,
	).Scan(&startedAt); err != nil {
		t.Fatalf("read attempt start: %v", err)
	}
	if startedAt != nil {
		t.Fatalf("started_at = %s, want NULL when no invocation row records the start", startedAt.UTC())
	}
}

func TestCompleteAttemptUsesRunnerInvocationStart(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")
	run := f.createRun(`{"subject":"runner"}`)
	nodeRun := f.readyNodeRun(run.ID)
	claimed := f.claim("worker-runner", nodeRun.ID)
	if _, err := f.store.Pool().Exec(f.ctx, `
		INSERT INTO runner_invocations (
			attempt_id, namespace_id, run_id, node_run_id, work_id, worker_id,
			fencing_token, attempt, node_key, runner_ref, endpoint, operation_id, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'intake', 'runner', 'https://runner.invalid', $1,
			clock_timestamp() - interval '3 seconds')`,
		"protocol-"+claimed.ID, f.ns.ID, run.ID, nodeRun.ID, claimed.ID, "worker-runner",
		claimed.FencingToken, claimed.Attempt,
	); err != nil {
		t.Fatalf("seed runner invocation: %v", err)
	}
	if _, err := f.engine.CompleteAttempt(f.ctx, completion(claimed, "worker-runner", succeeded("completed", `{"scope":"runner"}`))); err != nil {
		t.Fatalf("CompleteAttempt: %v", err)
	}

	var duration float64
	if err := f.store.Pool().QueryRow(f.ctx, `
		SELECT EXTRACT(EPOCH FROM (completed_at - started_at))
		FROM attempts WHERE node_run_id = $1`, nodeRun.ID).Scan(&duration); err != nil {
		t.Fatalf("read attempt duration: %v", err)
	}
	if duration < 2.5 {
		t.Fatalf("attempt duration = %v, want non-zero value near seeded 3-second runner invocation age", duration)
	}
}

// advanceToWork drives a fresh run to the point where the `work` node run is
// ready, and returns its id. Several tests below are about what happens at
// `work` specifically, because it is the node with a retry budget.
func advanceToWork(t *testing.T, f *fixture, subject string) (runID, workNodeRunID string) {
	t.Helper()
	run := f.createRun(`{"subject":"` + subject + `"}`)
	intake := f.readyNodeRun(run.ID)
	result := f.step("worker-a", intake.ID, succeeded("completed", `{"scope":"s"}`))
	if result.NextNodeID != "work" {
		t.Fatalf("intake routed to %q, want work", result.NextNodeID)
	}
	return run.ID, result.NextNodeRunID
}

// A technical failure consults the node's retry policy and buys another
// attempt against the *same* node run — the same logical execution, tried
// again, which is why the node run keeps its identity and its attempt history.
func TestTechnicalFailureRetriesTheSameNodeRun(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")
	runID, workNodeRun := advanceToWork(t, f, "retry")

	first := f.step("worker-a", workNodeRun, engine.CompletionRequest{TechStatus: engine.StatusFailed})
	if !first.Retried {
		t.Fatalf("work declares maxAttempts 3; attempt 1 should have been retried (%s)", first.Diagnostic)
	}
	if first.NextNodeRunID != "" {
		t.Errorf("a retry creates no new node run, got %q", first.NextNodeRunID)
	}
	if got := f.nodeRun(workNodeRun).State; got != engine.NodeRunReady {
		t.Errorf("node run state = %s, want ready", got)
	}

	// The origin is named because a timeout without one is refused rather than
	// retried (task t10, and see retry_test.go). What this test is about is
	// that a retry reuses the node run, so it states the case where a retry
	// legitimately happens: the actor itself reported the timeout.
	second := f.step("worker-a", workNodeRun, engine.CompletionRequest{
		TechStatus:    engine.StatusTimedOut,
		TimeoutOrigin: engine.TimeoutOriginActor,
	})
	if !second.Retried {
		t.Fatalf("attempt 2 of 3 should have been retried")
	}
	if second.AttemptNumber != 2 {
		t.Errorf("attempt number = %d, want 2", second.AttemptNumber)
	}

	// The third attempt succeeds and the run moves on, carrying three
	// attempts against one node run.
	third := f.step("worker-a", workNodeRun, succeeded("completed", `{"revision":9}`))
	if third.NextNodeID != "check" {
		t.Fatalf("routed to %q, want check", third.NextNodeID)
	}
	if third.AttemptNumber != 3 {
		t.Errorf("attempt number = %d, want 3", third.AttemptNumber)
	}

	attempts, err := f.engine.Store().Attempts(f.ctx, workNodeRun)
	if err != nil {
		t.Fatalf("Attempts: %v", err)
	}
	if len(attempts) != 3 {
		t.Fatalf("recorded %d attempts, want 3", len(attempts))
	}
	wantStatus := []engine.TechStatus{engine.StatusFailed, engine.StatusTimedOut, engine.StatusSucceeded}
	for i, attempt := range attempts {
		if attempt.Number != i+1 {
			t.Errorf("attempt %d numbered %d", i, attempt.Number)
		}
		if attempt.Status != wantStatus[i] {
			t.Errorf("attempt %d status = %s, want %s", i+1, attempt.Status, wantStatus[i])
		}
	}
	if got := f.nodeRunCount(runID, "work"); got != 1 {
		t.Errorf("work has %d node runs, want 1 — retries reuse the node run", got)
	}
}

// When the retry budget is spent and the workflow declares no edge from the
// technical status, the node run fails and the run fails with it.
func TestExhaustedRetriesFailTheRun(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")
	runID, workNodeRun := advanceToWork(t, f, "exhausted")

	var last engine.CompletionResult
	for i := 0; i < 3; i++ {
		last = f.step("worker-a", workNodeRun, engine.CompletionRequest{TechStatus: engine.StatusFailed})
	}

	if last.Retried {
		t.Fatal("the third of three attempts should not have been retried")
	}
	if last.RunState != engine.RunFailed {
		t.Fatalf("run state = %s, want failed", last.RunState)
	}
	if last.Diagnostic == "" {
		t.Error("a failed run should say why")
	}
	if got := f.nodeRun(workNodeRun).State; got != engine.NodeRunFailed {
		t.Errorf("node run state = %s, want failed", got)
	}
	if got := f.run(runID).State; got != engine.RunFailed {
		t.Errorf("stored run state = %s, want failed", got)
	}
}

// Output that violates its outcome's schema is a *technical* refusal: the
// attempt is recorded contract_rejected and the domain outcome is not routed.
// This is the §3.4 line the engine must not blur — a bad payload is not a
// business answer.
func TestOutputContractViolationIsATechnicalStatus(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")
	run := f.createRun(`{"subject":"bad output"}`)
	intake := f.readyNodeRun(run.ID)

	// intake.completed requires `scope`; this attempt reports success without
	// it.
	result := f.step("worker-a", intake.ID, succeeded("completed", `{"note":"forgot the scope"}`))

	if result.Rejection == nil || result.Rejection.Kind != engine.RejectionOutput {
		t.Fatalf("rejection = %+v, want an output rejection", result.Rejection)
	}
	if result.TechStatus != engine.StatusContractRejected {
		t.Errorf("recorded status = %s, want %s", result.TechStatus, engine.StatusContractRejected)
	}
	if result.Outcome != "" {
		t.Errorf("outcome = %q; a rejected completion routes no domain outcome", result.Outcome)
	}
	if result.NextNodeID != "" {
		t.Errorf("routed to %q; a rejected completion follows no edge", result.NextNodeID)
	}
	if result.RunState != engine.RunFailed {
		t.Errorf("run state = %s, want failed (intake has no retry budget and no edge from contract_rejected)", result.RunState)
	}

	types := f.eventTypes(run.ID)
	if !contains(types, engine.TypeContractRejected) {
		t.Errorf("events = %v, want one of type %s", types, engine.TypeContractRejected)
	}

	attempts, err := f.engine.Store().Attempts(f.ctx, intake.ID)
	if err != nil {
		t.Fatalf("Attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Status != engine.StatusContractRejected {
		t.Fatalf("attempts = %+v, want one recorded contract_rejected", attempts)
	}
}

func TestUndeclaredOutcomeIsRejected(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")
	run := f.createRun(`{"subject":"bad outcome"}`)
	intake := f.readyNodeRun(run.ID)

	result := f.step("worker-a", intake.ID, succeeded("shipped", `{"scope":"s"}`))
	if result.Rejection == nil || result.Rejection.Kind != engine.RejectionOutcome {
		t.Fatalf("rejection = %+v, want an outcome rejection", result.Rejection)
	}
	if result.TechStatus != engine.StatusContractRejected {
		t.Errorf("recorded status = %s, want %s", result.TechStatus, engine.StatusContractRejected)
	}
}

// A node may only write the record types its ledger delta contract declares
// (PRD §10.7). A delta that oversteps is refused whole: no partial ledger.
func TestLedgerDeltaBeyondTheNodeContractIsRejected(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")
	run := f.createRun(`{"subject":"ledger overreach"}`)
	intake := f.readyNodeRun(run.ID)

	// intake declares propose: [announcement, claim]; `decision` is not on it.
	result := f.step("worker-a", intake.ID, engine.CompletionRequest{
		TechStatus: engine.StatusSucceeded,
		Outcome:    "completed",
		Output:     json.RawMessage(`{"scope":"s"}`),
		LedgerDelta: []ledger.Record{
			{
				RecordType: ledger.RecordClaim,
				Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: f.actor},
				Data:       json.RawMessage(`{"statement":"this one is allowed"}`),
			},
			{
				RecordType: ledger.RecordDecision,
				Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: f.actor},
				Data:       json.RawMessage(`{"statement":"this one is not"}`),
			},
		},
	})

	if result.Rejection == nil || result.Rejection.Kind != engine.RejectionLedger {
		t.Fatalf("rejection = %+v, want a ledger rejection", result.Rejection)
	}
	if len(result.LedgerRecords) != 0 {
		t.Errorf("appended %d records; a refused delta is refused whole", len(result.LedgerRecords))
	}
	if got := f.countScalar(`SELECT COUNT(*)::int FROM ledger_records WHERE run_id = $1`, run.ID); got != 0 {
		t.Errorf("%d ledger records were written for a refused delta", got)
	}
}

// An actor cannot confirm its own work. Confirmation is a review transaction
// (PRD §10.8), and no completion path reaches it.
func TestActorCannotSelfConfirmThroughALedgerDelta(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")
	run := f.createRun(`{"subject":"self confirm"}`)
	intake := f.readyNodeRun(run.ID)

	result := f.step("worker-a", intake.ID, engine.CompletionRequest{
		TechStatus: engine.StatusSucceeded,
		Outcome:    "completed",
		Output:     json.RawMessage(`{"scope":"s"}`),
		LedgerDelta: []ledger.Record{{
			RecordType: ledger.RecordClaim,
			Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: f.actor},
			Authority:  ledger.AuthorityConfirmed,
			Data:       json.RawMessage(`{"statement":"I checked my own work"}`),
		}},
	})

	if result.Rejection == nil || result.Rejection.Kind != engine.RejectionLedger {
		t.Fatalf("rejection = %+v, want a ledger rejection", result.Rejection)
	}
	if got := f.countScalar(`SELECT COUNT(*)::int FROM ledger_records WHERE run_id = $1`, run.ID); got != 0 {
		t.Errorf("%d ledger records written; a self-confirmation must write none", got)
	}
}

// A run whose input violates the workflow's input contract is never created:
// there is no half-born run to clean up.
func TestCreateRunRejectsInputThatViolatesTheContract(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")

	before := f.countScalar(`SELECT COUNT(*)::int FROM runs WHERE namespace_id = $1`, f.ns.ID)
	_, err := f.engine.CreateRun(f.ctx, f.cw, json.RawMessage(`{"topic":"wrong field"}`))
	if !errors.Is(err, engine.ErrContract) {
		t.Fatalf("CreateRun err = %v, want a contract error", err)
	}
	after := f.countScalar(`SELECT COUNT(*)::int FROM runs WHERE namespace_id = $1`, f.ns.ID)
	if after != before {
		t.Errorf("a rejected input created %d run rows", after-before)
	}
}

// Two runs of the same definition pin the same immutable version: the digest
// resolves to one row, published once.
func TestRunsPinOneImmutableWorkflowVersion(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")

	first := f.createRun(`{"subject":"one"}`)
	second := f.createRun(`{"subject":"two"}`)

	if first.WorkflowVersionID != second.WorkflowVersionID {
		t.Errorf("runs pinned different versions of one definition: %s and %s",
			first.WorkflowVersionID, second.WorkflowVersionID)
	}
	versions := f.countScalar(
		`SELECT COUNT(*)::int FROM workflow_versions WHERE namespace_id = $1 AND content_digest = $2`,
		f.ns.ID, f.cw.Digest)
	if versions != 1 {
		t.Errorf("%d workflow versions for one digest, want 1", versions)
	}
}

func TestCompleteAttemptRejectsAnIncompleteRequest(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")

	cases := []struct {
		name string
		req  engine.CompletionRequest
	}{
		{"no work id", engine.CompletionRequest{WorkerID: "w", TechStatus: engine.StatusFailed}},
		{"no worker id", engine.CompletionRequest{WorkID: "x", TechStatus: engine.StatusFailed}},
		{"unknown status", engine.CompletionRequest{WorkID: "x", WorkerID: "w", TechStatus: "shipped"}},
		{"succeeded with no outcome", engine.CompletionRequest{WorkID: "x", WorkerID: "w", TechStatus: engine.StatusSucceeded}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.engine.CompleteAttempt(f.ctx, tc.req); err == nil {
				t.Fatal("expected the request to be refused")
			}
		})
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// A completion that reports a §13.2 Usage block gets it recorded on the
// attempt row it commits, including the independent nullability of Cost and
// Currency (task t1 acceptance: "the Usage block the bridge reported" is
// persisted at the completion seam, not derived or rounded).
func TestCompleteAttemptPersistsReportedUsage(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")
	_, workNodeRun := advanceToWork(t, f, "usage")

	cost := 0.0021
	currency := "USD"
	result := f.step("worker-a", workNodeRun, engine.CompletionRequest{
		TechStatus: engine.StatusSucceeded,
		Outcome:    "completed",
		Output:     json.RawMessage(`{"revision":1}`),
		Usage: &engine.Usage{
			InputTokens:  120,
			OutputTokens: 340,
			Cost:         &cost,
			Currency:     &currency,
		},
	})
	if result.AttemptNumber != 1 {
		t.Fatalf("attempt number = %d, want 1", result.AttemptNumber)
	}

	attempts, err := f.engine.Store().Attempts(f.ctx, workNodeRun)
	if err != nil {
		t.Fatalf("Attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("recorded %d attempts, want 1", len(attempts))
	}
	usage := attempts[0].Usage
	if usage == nil {
		t.Fatal("Usage = nil, want the reported §13.2 block")
	}
	if usage.InputTokens != 120 || usage.OutputTokens != 340 {
		t.Errorf("tokens = %d/%d, want 120/340", usage.InputTokens, usage.OutputTokens)
	}
	if usage.Cost == nil || *usage.Cost != cost {
		t.Errorf("cost = %v, want %v", usage.Cost, cost)
	}
	if usage.Currency == nil || *usage.Currency != currency {
		t.Errorf("currency = %v, want %v", usage.Currency, currency)
	}
}

// An actor that reports token counts without pricing its work leaves Cost
// and Currency null while the tokens are still recorded — the "reports
// usage, does not price it" case §13.2's nullable Cost/Currency exists for.
func TestCompleteAttemptPersistsUsageWithoutPricing(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")
	_, workNodeRun := advanceToWork(t, f, "usage-no-price")

	f.step("worker-a", workNodeRun, engine.CompletionRequest{
		TechStatus: engine.StatusSucceeded,
		Outcome:    "completed",
		Output:     json.RawMessage(`{"revision":1}`),
		Usage:      &engine.Usage{InputTokens: 10, OutputTokens: 20},
	})

	attempts, err := f.engine.Store().Attempts(f.ctx, workNodeRun)
	if err != nil {
		t.Fatalf("Attempts: %v", err)
	}
	usage := attempts[0].Usage
	if usage == nil {
		t.Fatal("Usage = nil, want a block with tokens set")
	}
	if usage.InputTokens != 10 || usage.OutputTokens != 20 {
		t.Errorf("tokens = %d/%d, want 10/20", usage.InputTokens, usage.OutputTokens)
	}
	if usage.Cost != nil {
		t.Errorf("cost = %v, want nil (not reported, not fabricated as zero)", *usage.Cost)
	}
	if usage.Currency != nil {
		t.Errorf("currency = %v, want nil", *usage.Currency)
	}
}

// A completion that reports no Usage block at all leaves the attempt's
// usage NULL end to end — no fabricated zero, no backfill (task t1
// acceptance).
func TestCompleteAttemptWithoutUsageStaysNil(t *testing.T) {
	f := newFixture(t, "loop.workflow.yaml")
	_, workNodeRun := advanceToWork(t, f, "no-usage")

	f.step("worker-a", workNodeRun, succeeded("completed", `{"revision":1}`))

	attempts, err := f.engine.Store().Attempts(f.ctx, workNodeRun)
	if err != nil {
		t.Fatalf("Attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("recorded %d attempts, want 1", len(attempts))
	}
	if attempts[0].Usage != nil {
		t.Errorf("Usage = %+v, want nil for an attempt that reported none", attempts[0].Usage)
	}
}
