package e2etest

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Benchmarks for the numbers PRD §21.1 asks to be RECORDED, not gated. See
// docs/benchmarks.md for the measured figures and the host caveat: this box
// is a shared arm64 development machine, not §21.2's reference environment,
// so the numbers are a floor and a regression tripwire, never a spec claim.
//
// The other two §21.1 profiles: ledger-projection cost is measured by
// internal/ledger's own benchmarks, and idle memory by scripts/idle-rss.sh
// (a whole-process figure a Go benchmark cannot produce, because the process
// under test is `nodes all`, not the test binary).

// loopWorkflowSource is a two-node loop: `work` routes back to itself on
// `again` and to the end node on `done`. One CompleteAttempt with outcome
// `again` is exactly one transition plus one new node run — the unit
// BenchmarkTransitions measures.
const loopWorkflowSource = `
apiVersion: nodes.culture.dev/v1alpha1
kind: Workflow
metadata:
  name: transition-benchmark
  version: 1.0.0
  ownerRef: team/platform-ai
spec:
  entry: work
  contract:
    input:
      schema:
        type: object
    output:
      schema:
        type: object
  limits:
    maxDuration: 24h
    maxTransitions: 1000000
    maxVisitsPerNode: 1000000
    maxParallelTokens: 1
  ledger:
    schemaVersion: nodes.culture.dev/ledger/v1alpha1
    maxRecordsPerNode: 10
  nodes:
    work:
      kind: agent
      ownerRef: team/platform-ai
      uses: actor://bench/worker@sha256:9999999999999999999999999999999999999999999999999999999999999999
      input:
        from: /run/input
      contract:
        outcomes:
          again:
            schema:
              type: object
          done:
            schema:
              type: object
      policy:
        timeout: 5m
    finish:
      kind: end
      ownerRef: team/platform-ai
      output:
        from: /nodes/work/output
  edges:
    - from: work.again
      to: work
    - from: work.done
      to: finish
`

// BenchmarkTransitions measures sequential committed-transition throughput:
// claim one work item, complete its attempt with a domain outcome, follow the
// edge, create the next node run — the whole PRD §12.5 transaction, against a
// real PostgreSQL.
//
// It deliberately drives engine.CompleteAttempt through the store's own
// ClaimWork rather than through a Worker: a worker's dispatch time is the
// ACTOR's latency, which is not what this number is about. This is the
// control plane's own cost per transition.
func BenchmarkTransitions(b *testing.B) {
	if testStore == nil {
		b.Skip("no PostgreSQL available: set NODES_TEST_DATABASE_URL, or ensure Docker can run postgres:17-alpine")
	}
	ctx := context.Background()

	eng, compiled := setUpTransitionsBenchmarkEngine(b, ctx)
	if _, err := eng.CreateRun(ctx, compiled, json.RawMessage(`{"subject":"bench"}`)); err != nil {
		b.Fatalf("CreateRun: %v", err)
	}

	workerID := "bench-worker"
	output := json.RawMessage(`{"n":1}`)

	b.ReportAllocs()
	b.ResetTimer()
	start := time.Now()

	for i := 0; i < b.N; i++ {
		claimed, claimErr := testStore.ClaimWork(ctx, workerID, time.Minute, 1)
		if claimErr != nil {
			b.Fatalf("ClaimWork: %v", claimErr)
		}
		if len(claimed) != 1 {
			b.Fatalf("claimed %d items, want 1: the loop should always have exactly one ready item", len(claimed))
		}
		if _, err := eng.CompleteAttempt(ctx, engine.CompletionRequest{
			WorkID:       claimed[0].ID,
			WorkerID:     workerID,
			FencingToken: claimed[0].FencingToken,
			Attempt:      int(claimed[0].Attempt),
			TechStatus:   engine.StatusSucceeded,
			Outcome:      "again",
			Output:       output,
		}); err != nil {
			b.Fatalf("CompleteAttempt: %v", err)
		}
	}

	elapsed := time.Since(start)
	b.StopTimer()
	if elapsed > 0 {
		b.ReportMetric(float64(b.N)/elapsed.Seconds(), "transitions/sec")
	}
}

// setUpTransitionsBenchmarkEngine creates a fresh namespace and engine and
// compiles the loop workflow, failing the benchmark on any setup error —
// including a compile diagnostic at error level.
func setUpTransitionsBenchmarkEngine(b *testing.B, ctx context.Context) (*engine.Engine, *compiler.CompiledWorkflow) {
	b.Helper()
	ns, err := testStore.CreateNamespace(ctx, "bench-transitions-"+randomSuffix(), "Benchmark Namespace")
	if err != nil {
		b.Fatalf("CreateNamespace: %v", err)
	}
	eng, err := postgres.NewEngine(testStore, ns.ID)
	if err != nil {
		b.Fatalf("NewEngine: %v", err)
	}

	compiled, diags, err := compiler.Compile([]byte(loopWorkflowSource), compiler.FormatYAML)
	if err != nil {
		b.Fatalf("compile: %v", err)
	}
	requireNoCompileErrors(b, diags)

	return eng, compiled
}

// requireNoCompileErrors fails the benchmark on the first error-level
// compile diagnostic.
func requireNoCompileErrors(b *testing.B, diags []compiler.Diagnostic) {
	b.Helper()
	for _, d := range diags {
		if d.Level == compiler.LevelError {
			b.Fatalf("compile: %s %s: %s", d.Code, d.Path, d.Message)
		}
	}
}

// randomSuffix keeps benchmark namespaces from colliding across runs.
func randomSuffix() string {
	return time.Now().UTC().Format("20060102150405.000000000")
}

// ledgerCorpusSize is how many records BenchmarkLedgerProjection appends
// before it measures projection cost. PRD §21.2's profile names 100,000; the
// default here is smaller so an ordinary `go test -bench .` finishes in
// seconds, and the full figure is reachable by setting the environment
// variable (docs/benchmarks.md records both).
const defaultLedgerCorpusSize = 2000

// BenchmarkLedgerProjection measures the §21.1 ledger-projection profile:
// what one deterministic projection over a run's whole ledger costs, and
// whether its digest is stable across repetitions.
//
// It projects delivery_summary, which is the most expensive of the §10.9
// projections — it walks every record, resolves supersession, and counts on
// both the execution and assurance axes — so the number is an upper bound on
// the rest.
func BenchmarkLedgerProjection(b *testing.B) {
	if testStore == nil {
		b.Skip("no PostgreSQL available: set NODES_TEST_DATABASE_URL, or ensure Docker can run postgres:17-alpine")
	}
	ctx := context.Background()

	corpus := resolveLedgerCorpusSize(b)
	led, runID, actorID := setUpLedgerProjectionBenchmark(b, ctx)
	appendElapsed := appendLedgerCorpus(b, ctx, led, runID, actorID, corpus)

	b.ReportAllocs()
	b.ResetTimer()
	start := time.Now()

	var digest string
	for i := 0; i < b.N; i++ {
		projection, projErr := led.ProjectRun(ctx, runID, ledger.KindDeliverySummary, "")
		if projErr != nil {
			b.Fatalf("ProjectRun: %v", projErr)
		}
		if digest == "" {
			digest = projection.Digest
		} else if projection.Digest != digest {
			b.Fatalf("projection digest changed between identical projections: %s then %s", digest, projection.Digest)
		}
	}

	elapsed := time.Since(start)
	b.StopTimer()
	b.ReportMetric(float64(corpus), "records")
	if elapsed > 0 {
		b.ReportMetric(float64(b.N)/elapsed.Seconds(), "projections/sec")
	}
	if appendElapsed > 0 {
		b.ReportMetric(float64(corpus)/appendElapsed.Seconds(), "appends/sec")
	}
}

// resolveLedgerCorpusSize returns defaultLedgerCorpusSize, or the value of
// NODES_BENCH_LEDGER_RECORDS when it is set to a positive integer.
func resolveLedgerCorpusSize(b *testing.B) int {
	b.Helper()
	raw := os.Getenv("NODES_BENCH_LEDGER_RECORDS")
	if raw == "" {
		return defaultLedgerCorpusSize
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		b.Fatalf("NODES_BENCH_LEDGER_RECORDS=%q is not a positive integer", raw)
	}
	return parsed
}

// setUpLedgerProjectionBenchmark creates a fresh namespace, engine, and
// ledger, compiles the loop workflow, creates one run, and registers the
// actor the appended records are attributed to.
func setUpLedgerProjectionBenchmark(b *testing.B, ctx context.Context) (led *ledger.Ledger, runID, actorID string) {
	b.Helper()
	ns, err := testStore.CreateNamespace(ctx, "bench-ledger-"+randomSuffix(), "Benchmark Namespace")
	if err != nil {
		b.Fatalf("CreateNamespace: %v", err)
	}
	eng, err := postgres.NewEngine(testStore, ns.ID)
	if err != nil {
		b.Fatalf("NewEngine: %v", err)
	}
	led, err = postgres.NewLedger(testStore, ns.ID)
	if err != nil {
		b.Fatalf("NewLedger: %v", err)
	}

	compiled, _, err := compiler.Compile([]byte(loopWorkflowSource), compiler.FormatYAML)
	if err != nil || compiled == nil {
		b.Fatalf("compile: %v", err)
	}
	run, err := eng.CreateRun(ctx, compiled, json.RawMessage(`{"subject":"bench"}`))
	if err != nil {
		b.Fatalf("CreateRun: %v", err)
	}

	actorID = "actor_bench_" + randomSuffix()
	if _, err := testStore.Pool().Exec(ctx, `
		INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
		VALUES ($1, $2, $1, 1, 'agent', 'http')`, actorID, ns.ID); err != nil {
		b.Fatalf("register bench actor: %v", err)
	}

	return led, run.ID, actorID
}

// appendLedgerCorpus appends corpus proposed task records to the run's
// ledger and returns how long that took.
func appendLedgerCorpus(b *testing.B, ctx context.Context, led *ledger.Ledger, runID, actorID string, corpus int) time.Duration {
	b.Helper()
	appendStart := time.Now()
	for i := 0; i < corpus; i++ {
		payload, _ := json.Marshal(map[string]any{
			"title":           "benchmark task",
			"status":          "completed",
			"assurance_state": "unverified",
			"n":               i,
		})
		if _, err := led.Append(ctx, ledger.Record{
			RecordType: ledger.RecordTask,
			RunID:      runID,
			Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: actorID},
			Authority:  ledger.AuthorityProposed,
			Data:       payload,
		}); err != nil {
			b.Fatalf("append record %d: %v", i, err)
		}
	}
	return time.Since(appendStart)
}
