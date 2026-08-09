package worker_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/runners"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// Code-node dispatch (task t27's wiring gap): the worker builds a typed
// runners.Operation from the pinned IR, executes it through the configured
// CodeRunner, and maps the Result onto a completion with
// runners.BuildCompletion.
//
// Everything below runs against a real PostgreSQL, the real engine, and the
// real Worker loop. The only fake is the Runner itself — the one piece that
// genuinely belongs to somebody else's process, and whose three interesting
// answers (exit 0, exit nonzero, refused dispatch) a real container run
// cannot be made to produce on demand. The live variant against the real
// headspace bridge lives in tests/e2e.

const (
	fakeCodeRunnerRevision = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	// fakeCodeRunnerName is the DISPATCH name stamped on the operation. The
	// producer identity the evidence is attributed to is a separate,
	// registered actor id — see Options.CodeRunnerActorID.
	fakeCodeRunnerName = "headspace"
)

// scriptedRunner is a runners.Runner test double driven by a caller-supplied
// function. It records every Operation it was handed so a test can assert on
// what the worker actually built from the IR.
type scriptedRunner struct {
	mu   sync.Mutex
	ops  []runners.Operation
	next func(op runners.Operation, call int) (runners.Result, error)
}

func (s *scriptedRunner) Execute(_ context.Context, op runners.Operation) (runners.Result, error) {
	s.mu.Lock()
	s.ops = append(s.ops, op)
	call := len(s.ops)
	s.mu.Unlock()
	return s.next(op, call)
}

func (s *scriptedRunner) operations() []runners.Operation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]runners.Operation(nil), s.ops...)
}

var _ runners.Runner = (*scriptedRunner)(nil)

// codeRunResult is shaped the way an isolating runner (headspace) reports:
// the process exit genuinely measured from the container's wait status, the
// workspace comparison and resource usage honestly not measured at all. The
// asymmetry is the point — the evidence builder must carry the first and
// leave out the second.
func codeRunResult(op runners.Operation, exitCode int) runners.Result {
	code := exitCode
	finished := time.Now().UTC()
	return runners.Result{
		OperationID: op.OperationID,
		State:       runners.StateCompleted,
		Exit:        &runners.Exit{Code: &code},
		Timing: runners.Timing{
			StartedAt:  finished.Add(-30 * time.Millisecond),
			FinishedAt: finished,
			DurationMs: 30,
		},
		Environment: runners.Environment{
			RunnerRevision:    op.RunnerRevision,
			ImageDigest:       op.Execution.ImageDigest,
			PolicyDigest:      "sha256:" + strings.Repeat("c", 64),
			PlatformRequestID: "ws_fake_0001",
		},
		Changes: runners.Changes{Complete: false},
		Observations: runners.Observations{
			ExitStatus:    runners.Observation{Measured: true, Complete: true, Method: "container_wait_status"},
			ChangedPaths:  runners.Observation{Measured: false, Complete: false},
			Logs:          runners.Observation{Measured: true, Complete: true, Method: "container_log_capture"},
			ResourceUsage: runners.Observation{Measured: false, Complete: false},
			Additional: map[string]runners.Observation{
				"image_digest":        {Measured: true, Complete: true, Method: "container_inspect"},
				"platform_request_id": {Measured: true, Complete: true, Method: "headspace_workspace_id"},
				"duration":            {Measured: true, Complete: true, Method: "runner_clock"},
			},
		},
	}
}

// codeHarness is worker_test.go's harness with a CodeRunner wired in. A code
// node needs no actor registry, no signer, and no callback endpoint, so the
// worker it drives is deliberately built with none of them.
type codeHarness struct {
	*harness
	runner    *scriptedRunner
	runnerID  string
	ledgerRun *ledger.Ledger
}

func newCodeHarness(t *testing.T, next func(op runners.Operation, call int) (runners.Result, error)) *codeHarness {
	t.Helper()
	base := newHarness(t, refusingActor)
	runner := &scriptedRunner{next: next}
	// The ledger attributes evidence to a registered actor identity
	// (ledger_records.origin_actor_id is a real foreign key), so the code
	// runner has to exist as an actor before its evidence can be appended —
	// the same obligation a real deployment has.
	runnerID := mustHookRunnerActor(t, base.store, base.ns.ID)

	wk, err := worker.New(base.store, base.engine, worker.Options{
		WorkerID:           "worker-code-" + t.Name(),
		NamespaceID:        base.ns.ID,
		LeaseDuration:      30 * time.Second,
		HeartbeatInterval:  200 * time.Millisecond,
		PollInterval:       20 * time.Millisecond,
		CodeRunner:         runner,
		CodeRunnerName:     fakeCodeRunnerName,
		CodeRunnerActorID:  runnerID,
		CodeRunnerRevision: fakeCodeRunnerRevision,
		OnError: func(err error) {
			base.mu.Lock()
			base.errs = append(base.errs, err)
			base.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	led, err := storepg.NewLedger(base.store, base.ns.ID)
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}

	// Drive this worker, not the base harness's actor-facing one.
	base.worker = wk
	return &codeHarness{harness: base, runner: runner, runnerID: runnerID, ledgerRun: led}
}

// refusingActor is the actor handler for a harness whose workflows contain no
// agent node: reaching it at all is a bug in the test, so it says so loudly
// rather than answering something plausible.
func refusingActor(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
	http.Error(w, "no agent node in this workflow should ever have been dispatched", http.StatusTeapot)
}

func (h *codeHarness) records(runID string) []ledger.Record {
	h.t.Helper()
	records, err := h.ledgerRun.Records(context.Background(), runID)
	if err != nil {
		h.t.Fatalf("ledger.Records: %v", err)
	}
	return records
}

func (h *codeHarness) evidenceRecords(runID string) []ledger.Record {
	h.t.Helper()
	var out []ledger.Record
	for _, rec := range h.records(runID) {
		if rec.RecordType == ledger.RecordEvidence {
			out = append(out, rec)
		}
	}
	return out
}

type attemptRow struct {
	Status string
	Result []byte
}

// attemptRows returns every attempt against nodeKey, in attempt order.
func (h *codeHarness) attemptRows(runID, nodeKey string) []attemptRow {
	h.t.Helper()
	rows, err := h.store.Pool().Query(h.ctx, `
		SELECT a.status, a.result
		FROM attempts AS a JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = $2
		ORDER BY a.attempt_number
	`, runID, nodeKey)
	if err != nil {
		h.t.Fatalf("read attempts: %v", err)
	}
	defer rows.Close()
	var out []attemptRow
	for rows.Next() {
		var row attemptRow
		if err := rows.Scan(&row.Status, &row.Result); err != nil {
			h.t.Fatalf("scan attempt: %v", err)
		}
		out = append(out, row)
	}
	return out
}

func (h *codeHarness) nodeOutcome(runID, nodeKey string) string {
	h.t.Helper()
	var outcome *string
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT outcome FROM node_runs WHERE run_id = $1 AND node_key = $2`, runID, nodeKey).Scan(&outcome); err != nil {
		h.t.Fatalf("read node run outcome for %q: %v", nodeKey, err)
	}
	if outcome == nil {
		return ""
	}
	return *outcome
}

// Exit 0: the declared success outcome is routed, and the runner's observed
// evidence lands in the ledger through the node's own declared `observe`
// permission — inside the completion transaction, not beside it.
func TestCodeNodeExitZeroRoutesTheSuccessOutcomeAndAppendsObservedEvidence(t *testing.T) {
	h := newCodeHarness(t, func(op runners.Operation, _ int) (runners.Result, error) {
		return codeRunResult(op, 0), nil
	})

	run := h.createRun("code.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", state, h.workerErrors())
	}
	if outcome := h.nodeOutcome(run.ID, "test"); outcome != "passed" {
		t.Errorf("code node outcome = %q, want passed", outcome)
	}
	attempts := h.attemptRows(run.ID, "test")
	if len(attempts) != 1 {
		t.Fatalf("code node ran %d attempts, want 1", len(attempts))
	}
	if engine.TechStatus(attempts[0].Status) != engine.StatusSucceeded {
		t.Errorf("attempt status = %q, want succeeded", attempts[0].Status)
	}

	// The operation the worker built from the pinned IR.
	ops := h.runner.operations()
	if len(ops) != 1 {
		t.Fatalf("runner executed %d operations, want 1", len(ops))
	}
	op := ops[0]
	if op.Runner != fakeCodeRunnerName || op.RunnerRevision != fakeCodeRunnerRevision {
		t.Errorf("operation runner = %q@%q, want %q@%q", op.Runner, op.RunnerRevision, fakeCodeRunnerName, fakeCodeRunnerRevision)
	}
	if !strings.HasPrefix(op.Execution.ImageDigest, "sha256:") {
		t.Errorf("operation image digest = %q, want the pin extracted from the IR image reference", op.Execution.ImageDigest)
	}
	if len(op.Command.Argv) != 3 || op.Command.Argv[0] != "python3" {
		t.Errorf("operation argv = %v, want the IR's argv array", op.Command.Argv)
	}
	if op.Policy.Network != runners.NetworkNone {
		t.Errorf("operation network = %q, want none", op.Policy.Network)
	}
	if op.Policy.TimeoutSeconds != 300 {
		t.Errorf("operation timeout = %ds, want the node's declared 5m", op.Policy.TimeoutSeconds)
	}
	if op.Context == nil || op.Context.RunID != run.ID || op.Context.AttemptID == "" {
		t.Errorf("operation context = %+v, want the run/node-run/attempt correlation", op.Context)
	}
	if op.OperationID == "" {
		t.Error("operation carries no operation id; it is the dispatch idempotency key")
	}

	// The evidence record: runner origin, observed authority, and a payload
	// carrying only what the Result declared measured.
	evidence := h.evidenceRecords(run.ID)
	if len(evidence) != 1 {
		t.Fatalf("run has %d evidence records, want exactly the runner's one", len(evidence))
	}
	got := evidence[0]
	if got.Authority != ledger.AuthorityObserved {
		t.Errorf("evidence authority = %q, want observed", got.Authority)
	}
	if got.Origin.Kind != ledger.OriginRunner || got.Origin.ActorID != h.runnerID {
		t.Errorf("evidence origin = %+v, want runner %q", got.Origin, h.runnerID)
	}
	if got.Origin.ActorRevision != fakeCodeRunnerRevision {
		t.Errorf("evidence actor revision = %q, want the pinned runner revision", got.Origin.ActorRevision)
	}
	if got.AttemptID.String() == "" || got.NodeRunID.String() == "" {
		t.Errorf("evidence record is not stamped with its attempt/node run: %+v", got)
	}
	data, err := got.DataMap()
	if err != nil {
		t.Fatalf("decode evidence payload: %v", err)
	}
	measurements, _ := data["measurements"].(map[string]any)
	if measurements == nil {
		t.Fatalf("evidence payload has no measurements block: %s", got.Data)
	}
	if _, ok := measurements["exit_code"]; !ok {
		t.Errorf("evidence measurements = %v, want the measured exit_code", measurements)
	}
	if _, ok := measurements["max_memory_mib"]; ok {
		t.Errorf("evidence claims a resource measurement the result declared unmeasured: %v", measurements)
	}

	// A runner_operations row keyed to the attempt: the same bookkeeping a
	// hook's dispatch leaves behind, under its own operation kind.
	var recorded int
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT count(*) FROM runner_operations AS ro
		JOIN attempts AS a ON a.id = ro.attempt_id
		JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND ro.operation_kind = 'code'
	`, run.ID).Scan(&recorded); err != nil {
		t.Fatalf("count runner operations: %v", err)
	}
	if recorded != 1 {
		t.Errorf("runner_operations rows for the code node = %d, want 1", recorded)
	}
}

// Exit nonzero with a declared failure outcome: PRD §3.4's headline. A test
// suite that ran to completion and reported failures dispatched successfully;
// the workflow's own `failed` edge is followed rather than the engine
// retrying.
func TestCodeNodeNonzeroExitIsADomainOutcomeWhenTheNodeDeclaresOne(t *testing.T) {
	h := newCodeHarness(t, func(op runners.Operation, _ int) (runners.Result, error) {
		return codeRunResult(op, 1), nil
	})

	run := h.createRun("code.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed via the `failed` edge (worker errors: %v)", state, h.workerErrors())
	}
	if outcome := h.nodeOutcome(run.ID, "test"); outcome != "failed" {
		t.Errorf("code node outcome = %q, want failed", outcome)
	}
	attempts := h.attemptRows(run.ID, "test")
	if len(attempts) != 1 {
		t.Fatalf("code node ran %d attempts, want 1: a domain outcome is not retried", len(attempts))
	}
	if engine.TechStatus(attempts[0].Status) != engine.StatusSucceeded {
		t.Errorf("attempt status = %q; the dispatch itself succeeded, the tests did not pass", attempts[0].Status)
	}
	if state := h.nodeOutcome(run.ID, "giveup"); state == "" {
		// An end node records no outcome of its own; its existence is the
		// proof the edge was followed.
		var exists bool
		if err := h.store.Pool().QueryRow(h.ctx,
			`SELECT EXISTS (SELECT 1 FROM node_runs WHERE run_id = $1 AND node_key = 'giveup')`, run.ID).Scan(&exists); err != nil {
			t.Fatalf("look for the giveup node run: %v", err)
		}
		if !exists {
			t.Error("the `failed` edge was not followed to the giveup end node")
		}
	}

	// The observed evidence still lands: a failing run is measured too.
	if got := len(h.evidenceRecords(run.ID)); got != 1 {
		t.Errorf("evidence records = %d, want 1 even for a failing run", got)
	}
}

// Exit nonzero with NO declared failure outcome: one technical failure per
// attempt, retried per the node's own policy, never a fabricated domain
// answer.
func TestCodeNodeNonzeroExitWithoutADeclaredOutcomeIsATechnicalFailure(t *testing.T) {
	h := newCodeHarness(t, func(op runners.Operation, _ int) (runners.Result, error) {
		return codeRunResult(op, 2), nil
	})

	run := h.createRun("code-nofailure.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(30*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunFailed {
		t.Fatalf("run state = %s, want failed (worker errors: %v)", state, h.workerErrors())
	}
	if outcome := h.nodeOutcome(run.ID, "test"); outcome != "" {
		t.Errorf("code node outcome = %q; a technical failure has no domain answer to route", outcome)
	}
	attempts := h.attemptRows(run.ID, "test")
	if len(attempts) != 2 {
		t.Fatalf("code node ran %d attempts, want 2 (maxAttempts: 2)", len(attempts))
	}
	for i, a := range attempts {
		if engine.TechStatus(a.Status) != engine.StatusFailed {
			t.Errorf("attempt %d status = %q, want failed", i+1, a.Status)
		}
	}
}

// A refused dispatch (no Result at all) is a classified technical failure,
// never an invented outcome and never an evidence record about an execution
// that did not happen.
func TestCodeNodeDispatchRefusalIsAClassifiedTechnicalFailure(t *testing.T) {
	h := newCodeHarness(t, func(op runners.Operation, _ int) (runners.Result, error) {
		return runners.Result{}, &runners.DispatchError{
			Kind:        runners.ErrorAuthOrPolicy,
			OperationID: op.OperationID,
			Detail:      "the runner registry holds no such identity",
			Err:         runners.ErrUnregisteredFunction,
		}
	})

	run := h.createRun("code.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunFailed {
		t.Fatalf("run state = %s, want failed", state)
	}
	attempts := h.attemptRows(run.ID, "test")
	if len(attempts) == 0 {
		t.Fatal("no attempt was recorded for the refused dispatch")
	}
	if engine.TechStatus(attempts[0].Status) != engine.StatusPolicyDenied {
		t.Errorf("attempt status = %q, want policy_denied for an auth_or_policy refusal", attempts[0].Status)
	}
	if !strings.Contains(string(attempts[0].Result), "runner") {
		t.Errorf("attempt result = %s, want a diagnostic naming the runner refusal", attempts[0].Result)
	}
	if got := len(h.evidenceRecords(run.ID)); got != 0 {
		t.Errorf("a refused dispatch produced %d evidence records, want 0", got)
	}
}

// A code node whose declared outcomes the worker cannot map onto an exit
// status is refused with a diagnostic, before dispatch. It must never guess:
// picking one of `green`/`red` for exit 0 is exactly how a failing suite gets
// routed down the happy edge.
func TestCodeNodeWithUnmappableOutcomesIsRefusedRatherThanGuessed(t *testing.T) {
	var executed int
	h := newCodeHarness(t, func(op runners.Operation, _ int) (runners.Result, error) {
		executed++
		return codeRunResult(op, 0), nil
	})

	run := h.createRun("code-unmappable.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunFailed {
		t.Fatalf("run state = %s, want failed", state)
	}
	if executed != 0 {
		t.Errorf("the runner was executed %d times; an unmappable node must not dispatch at all", executed)
	}
	attempts := h.attemptRows(run.ID, "test")
	if len(attempts) == 0 {
		t.Fatal("no attempt was recorded")
	}
	if !strings.Contains(string(attempts[0].Result), "outcome") {
		t.Errorf("attempt result = %s, want a diagnostic naming the outcome mapping", attempts[0].Result)
	}
}

// The higher-level RunnerDispatcher seam still wins when both are configured:
// a deployment that registered one has already said how it wants code
// dispatched, and a lower-level option must not silently override it.
func TestRunnerDispatcherSeamTakesPrecedenceOverCodeRunner(t *testing.T) {
	var (
		seamCalled bool
		runnerRan  bool
	)
	base := newHarness(t, refusingActor)
	runner := &scriptedRunner{next: func(op runners.Operation, _ int) (runners.Result, error) {
		runnerRan = true
		return codeRunResult(op, 0), nil
	}}

	wk, err := worker.New(base.store, base.engine, worker.Options{
		WorkerID:          "worker-both-" + t.Name(),
		NamespaceID:       base.ns.ID,
		LeaseDuration:     30 * time.Second,
		PollInterval:      20 * time.Millisecond,
		Runner:            dispatcherFunc(func() { seamCalled = true }),
		CodeRunner:        runner,
		CodeRunnerName:    fakeCodeRunnerName,
		CodeRunnerActorID: mustHookRunnerActor(t, base.store, base.ns.ID),
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	base.worker = wk

	run := base.createRun("code.workflow.yaml", `{"subject":"widget"}`)
	base.runUntil(20*time.Second, func() bool { return base.run(run.ID).State.Terminal() })

	if !seamCalled {
		t.Error("the registered RunnerDispatcher was never called")
	}
	if runnerRan {
		t.Error("the low-level CodeRunner ran even though a RunnerDispatcher was registered")
	}
}

// dispatcherFunc is a RunnerDispatcher that succeeds with the node's success
// outcome and records that it was asked.
type dispatcherFunc func()

func (d dispatcherFunc) DispatchCode(_ context.Context, _ worker.DispatchContext, _ string, _ json.RawMessage) (worker.SeamResult, error) {
	d()
	return worker.SeamResult{
		TechStatus: engine.StatusSucceeded,
		Outcome:    "passed",
		Output:     json.RawMessage(`{"via":"seam"}`),
	}, nil
}

var _ worker.RunnerDispatcher = dispatcherFunc(nil)

// ConventionalCodeOutcomes' own rules, in isolation from the loop.
func TestConventionalCodeOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name            string
		declared        []string
		success         string
		failure         string
		wantRefusal     bool
		refusalContains string
	}{
		{name: "prd reference names", declared: []string{"failed", "passed"}, success: "passed", failure: "failed"},
		{name: "success only", declared: []string{"passed"}, success: "passed"},
		{name: "completed as success", declared: []string{"completed"}, success: "completed"},
		{
			name:            "unrecognised vocabulary is refused",
			declared:        []string{"green", "red"},
			wantRefusal:     true,
			refusalContains: "guessing",
		},
		{name: "no outcomes at all is refused", declared: nil, wantRefusal: true, refusalContains: "exit-0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := worker.ConventionalCodeOutcomes("test", tc.declared)
			if tc.wantRefusal {
				if err == nil {
					t.Fatalf("ConventionalCodeOutcomes(%v) = %+v, want a refusal", tc.declared, got)
				}
				if !strings.Contains(err.Error(), tc.refusalContains) {
					t.Errorf("refusal = %q, want it to mention %q", err, tc.refusalContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("ConventionalCodeOutcomes(%v): %v", tc.declared, err)
			}
			if got.Success != tc.success || got.Failure != tc.failure {
				t.Errorf("outcomes = %+v, want success %q failure %q", got, tc.success, tc.failure)
			}
		})
	}
}
