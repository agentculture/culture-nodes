package e2etest

import (
	"context"
	"encoding/json"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/internal/runners/headspace"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// The live variant of the Phase-1 slice: the same reference workflow, the
// same worker, the same engine — but the `test` code node's operation is
// dispatched to the REAL headspace-cli Docker bridge, which really pulls the
// pinned python:3.12-slim image, really creates a disposable workspace,
// really runs the argv inside it, and really reports what it measured.
//
// It is capability-gated rather than build-tagged, matching
// internal/runners/headspace/live_test.go's own convention: on a machine that
// has headspace-cli and a reachable Docker engine, `go test ./...` runs it
// for real, which is the point of having it. Everywhere else it skips with a
// reason.

// referenceImageDigest is the digest the reference workflow's `test` node
// pins. It is python:3.12-slim's digest, which is also what headspace-cli
// 0.11.0's `python3.12` profile resolves to — so mapping it to that profile
// is a statement of fact, not a convenience.
const referenceImageDigest = "sha256:57cd7c3a7a273101a6485ba99423ee568157882804b1124b4dd04266317710de"

// requireRealHeadspace skips unless a real headspace binary is on PATH,
// reports healthy, and a Docker engine is reachable.
func requireRealHeadspace(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("headspace"); err != nil {
		t.Skip("skipping the live-runner slice: no `headspace` binary on PATH")
	}
	out, err := exec.Command("headspace", "doctor", "--json").Output()
	if err != nil {
		t.Skipf("skipping the live-runner slice: `headspace doctor --json` failed: %v", err)
	}
	var doctor struct {
		Healthy bool `json:"healthy"`
	}
	if jsonErr := json.Unmarshal(out, &doctor); jsonErr != nil || !doctor.Healthy {
		t.Skipf("skipping the live-runner slice: `headspace doctor --json` reported unhealthy: %s", out)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("skipping the live-runner slice: no `docker` binary on PATH")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("skipping the live-runner slice: docker engine not reachable (`docker info` failed)")
	}
}

// TestPhase1VerticalSliceWithRealHeadspaceRunner runs the reference workflow
// end to end with the code node dispatched to the real Docker boundary, and
// asserts the evidence in the ledger carries headspace's OWN provenance
// rather than anything this test supplied.
func TestPhase1VerticalSliceWithRealHeadspaceRunner(t *testing.T) {
	requireRealHeadspace(t)

	s := pgtest.RequireStore(t, testStore)
	ns := pgtest.MustNamespace(t, s, "e2e-live")

	bridge, err := headspace.New(headspace.BridgeConfig{
		Profile:     map[string]string{referenceImageDigest: headspace.DefaultProfilePython312},
		Provider:    "docker",
		StopTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("headspace.New: %v", err)
	}

	agentIDs := map[string]string{}
	agents := newDeliveryAgents(t, agentIDs)
	registered, runnerID := registerActors(t, s, ns.ID, agents.server.URL)
	agents.mu.Lock()
	for node, id := range registered {
		agentIDs[node] = id
	}
	agents.mu.Unlock()

	stack := startStack(t, stackConfig{
		namespaceID:   ns.ID,
		agentsURL:     agents.server.URL,
		runner:        bridge,
		runnerName:    headspace.RunnerName,
		runnerActorID: runnerID,
	})
	defer stack.stop()

	digest := stack.publishWorkflow(t)

	started := time.Now()
	runID := stack.createRun(t, digest, json.RawMessage(`{"request":"add a /healthz endpoint","repository":"example/service"}`))
	// Two real container runs (the loop runs `test` twice), each an image
	// resolve, a workspace create, a run, and a destroy.
	view := stack.waitForTerminal(t, runID, 10*time.Minute)
	elapsed := time.Since(started)

	if failures := agents.scriptFailures(); len(failures) > 0 {
		t.Fatalf("the scripted agents refused an invocation: %v", failures)
	}
	if view.Run.State != string(engine.RunCompleted) {
		dumpRunState(t, stack, runID)
		t.Fatalf("run state = %s, want completed (worker errors: %v)", view.Run.State, stack.errors())
	}
	t.Logf("live delivery-loop run finished in %s (two real headspace container executions)", elapsed.Round(time.Millisecond))

	// The loop still ran exactly once, through a real runner.
	if got := agents.callCount("build"); got != 2 {
		t.Errorf("build was invoked %d times, want 2", got)
	}
	testRuns := nodeRunsFor(view, "test")
	if len(testRuns) != 2 {
		t.Fatalf("the code node ran %d times, want 2", len(testRuns))
	}
	for _, nr := range testRuns {
		if nr.Outcome != "passed" {
			t.Errorf("code node outcome = %q, want passed (the container exited 0)", nr.Outcome)
		}
	}

	// ---- The evidence carries headspace's own measurements ----

	led := ledgerFor(t, stack.db, ns.ID)
	records, err := led.Records(context.Background(), runID)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	var evidence []ledger.Record
	for _, rec := range records {
		if rec.RecordType == ledger.RecordEvidence {
			evidence = append(evidence, rec)
		}
	}
	if len(evidence) != 2 {
		t.Fatalf("run has %d evidence records, want 2", len(evidence))
	}
	for _, rec := range evidence {
		if rec.Authority != ledger.AuthorityObserved || rec.Origin.Kind != ledger.OriginRunner {
			t.Errorf("evidence %s is %s/%s, want observed/runner", rec.ID, rec.Authority, rec.Origin.Kind)
		}
		if rec.Origin.ActorRevision != bridge.RunnerRevision() {
			t.Errorf("evidence %s pins runner revision %q, want the deployed bridge's %q",
				rec.ID, rec.Origin.ActorRevision, bridge.RunnerRevision())
		}
		data, decodeErr := rec.DataMap()
		if decodeErr != nil {
			t.Fatalf("decode evidence: %v", decodeErr)
		}
		// The image digest headspace itself resolved and ran — not the one
		// this test asked for. They agree because the pin held.
		if got, _ := data["environment_digest"].(string); got != referenceImageDigest {
			t.Errorf("evidence environment_digest = %q, want headspace's resolved %q", got, referenceImageDigest)
		}
		measurements, _ := data["measurements"].(map[string]any)
		if measurements == nil {
			t.Fatalf("evidence %s carries no measurements: %s", rec.ID, rec.Data)
		}
		if code, ok := measurements["exit_code"].(float64); !ok || code != 0 {
			t.Errorf("evidence measurements exit_code = %v, want a measured 0", measurements["exit_code"])
		}
		// headspace's own job id, lifted into the evidence because the
		// bridge declares it measured under the canonical key. This is the
		// workspace-side identity that ties the ledger record back to the
		// container that produced it.
		requestID, ok := measurements["platform_request_id"].(string)
		if !ok || requestID == "" {
			t.Errorf("evidence %s carries no measured platform_request_id: %s", rec.ID, rec.Data)
		}
		// The runner declares no `duration` observation of its own, so
		// duration_ms is CORRECTLY absent: an unmeasured field must not
		// appear in observed evidence (PRD §10.5).
		if _, present := measurements["duration_ms"]; present {
			t.Errorf("evidence %s claims a duration the runner never declared measured: %s", rec.ID, rec.Data)
		}
		// A real container run: peak memory is a figure the provider
		// measured, so it IS admitted.
		if _, present := measurements["max_memory_mib"]; !present {
			t.Errorf("evidence %s carries no measured memory figure: %s", rec.ID, rec.Data)
		}
		t.Logf("live evidence %s payload: %s", rec.ID, rec.Data)
	}

	// ---- The stored runner operation carries the full provenance ----
	//
	// The evidence record carries only what runners.BuildCompletion lifts out
	// of a Result (PRD §10.4's "only fields they directly measured"). The
	// workspace and job ids headspace minted live in the runner_operations
	// row alongside the request that produced them, which is the replay
	// manifest PRD §13.7 asks for.
	results := storedRunnerResults(t, stack, runID)
	if len(results) != 2 {
		t.Fatalf("runner_operations holds %d code rows for this run, want 2", len(results))
	}
	for _, res := range results {
		if res.Environment.ImageDigest != referenceImageDigest {
			t.Errorf("stored result image digest = %q, want %q", res.Environment.ImageDigest, referenceImageDigest)
		}
		if res.Environment.PlatformRequestID == "" {
			t.Error("stored result carries no platform request id (headspace's job id)")
		}
		if res.Environment.RunnerRevision != bridge.RunnerRevision() {
			t.Errorf("stored result runner revision = %q, want %q", res.Environment.RunnerRevision, bridge.RunnerRevision())
		}
		workspace, ok := res.Observations.Get("workspace_id")
		if !ok || !workspace.Measured {
			t.Errorf("stored result declares no measured workspace_id observation: %+v", res.Observations.Additional)
		}
		if workspace.Scope == "" {
			t.Error("the workspace_id observation states no scope")
		}
		job, ok := res.Observations.Get("job_id")
		if !ok || !job.Measured {
			t.Error("stored result declares no measured job_id observation")
		}
		if !res.Observations.ExitStatus.Measured {
			t.Error("stored result does not declare the exit status measured; a real container wait status IS measured")
		}
		t.Logf("live runner provenance: image=%s job=%s workspace_scope=%q",
			res.Environment.ImageDigest, res.Environment.PlatformRequestID, workspace.Scope)
	}

	// ---- PRD §21.2's headspace conformance property ----
	//
	// The same operation ran twice with identical input, image, and policy.
	// The two results must therefore agree on everything except the
	// identities and timings that are per-execution by definition: same
	// policy digest, same image digest, same result-envelope structure.
	if results[0].Environment.PolicyDigest != results[1].Environment.PolicyDigest {
		t.Errorf("two runs of one operation reported different policy digests: %s vs %s",
			results[0].Environment.PolicyDigest, results[1].Environment.PolicyDigest)
	}
	firstKeys, secondKeys := observationKeys(results[0]), observationKeys(results[1])
	if !slices.Equal(firstKeys, secondKeys) {
		t.Errorf("two runs of one operation declared different observation sets: %v vs %v", firstKeys, secondKeys)
	}
	if results[0].Environment.PlatformRequestID == results[1].Environment.PlatformRequestID {
		t.Error("two separate executions reported the same job id; each run is its own execution")
	}
	t.Logf("live conformance: identical policy digest %s across two executions, observation set %v",
		results[0].Environment.PolicyDigest, firstKeys)
}

// observationKeys returns a result's declared observation names, sorted.
func observationKeys(res runners.Result) []string {
	names := res.Observations.Names()
	slices.Sort(names)
	return names
}

// storedRunnerResults reads back the runners.Result documents the worker
// recorded for this run's code-node dispatches.
func storedRunnerResults(t *testing.T, s *stack, runID string) []runners.Result {
	t.Helper()
	rows, err := s.db.Pool().Query(context.Background(), `
		SELECT ro.result
		FROM runner_operations AS ro
		JOIN attempts AS a ON a.id = ro.attempt_id
		JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND ro.operation_kind = 'code' AND ro.result IS NOT NULL
		ORDER BY ro.created_at
	`, runID)
	if err != nil {
		t.Fatalf("read runner operations: %v", err)
	}
	defer rows.Close()

	var out []runners.Result
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan runner operation: %v", err)
		}
		var res runners.Result
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatalf("decode stored runner result: %v", err)
		}
		out = append(out, res)
	}
	return out
}
