// Command loadworker is a throwaway load-test fixture, not a culture-nodes
// product binary. It lives under testdata/ so Go's tooling leaves it out of
// `go build ./...`, `go vet ./...` and `go test ./...` package discovery.
//
// tests/load `go build`s this package and execs one copy of it per measured
// fleet, against one ephemeral PostgreSQL and one in-test stub runner service.
// It exists so the numbers in docs/benchmarks.md are read out of a REAL worker
// PROCESS — its own RSS, its own goroutines, its own heap — rather than out of
// a test binary that also hosts the stub service, the pgx pool that creates
// the runs, and the measurement code itself.
//
// It runs the real internal/worker code and nothing else: worker.Tick to claim
// and dispatch, worker.SampleRunnerOperations to sample parked operations.
// Nothing about the dispatch or the sampling path is special-cased for the
// measurement.
//
// # Two phases, and why they are separate
//
// worker.Tick ends by calling SampleRunnerOperations itself, so a loop that
// called both would run two sampler passes per iteration and only the first
// would find anything due. That would make "how long does a sampling pass
// take" unmeasurable. So this process has two phases:
//
//	dispatch  call Tick until LOAD_TARGET operations are parked. Sampling
//	          happens inside Tick, untimed, because during this phase the
//	          number of in-flight operations is still changing.
//	observe   call SampleRunnerOperations ONLY, timed. This is the steady
//	          state the sampling-load claim is about: N operations parked,
//	          nothing ready to claim, one authenticated status read per
//	          operation per interval.
//
// The phase flips on its own, as soon as the parked count reaches the target,
// and every emitted sample says which phase it was taken in.
//
// # Output
//
// One JSON object per loop iteration on stdout, newline delimited. Diagnostics
// go to stderr. Every field is measured immediately after the iteration's work
// and outside its timing:
//
//	{"seq":41,"phase":"observe","unix_ms":...,"pass_ns":183492011,"sampled":100,
//	 "total_sampled":300,"parked":100,"db_ops":1407,"goroutines":11,
//	 "heap_alloc":6291456,"rss_kb":31240,"hwm_kb":31240,"threads":9, ...}
//
// runtime.ReadMemStats and the /proc/self/status read both happen after the
// timed region, so neither inflates pass_ns. No GC is ever forced: heap_alloc
// and rss_kb are what this process actually holds, not what it could be
// squeezed down to.
//
// Required env vars:
//
//	LOAD_DB_URL             postgres connection string
//	LOAD_NAMESPACE_ID       namespace to work in
//	LOAD_WORKER_ID          lease owner / worker identity
//	LOAD_RUNNER_REF         registry name the code node's `uses` declares
//	LOAD_RUNNER_ENDPOINT    the stub runner service's base URL
//	LOAD_RUNNER_DIGEST      the pinned execution-environment digest
//	LOAD_SECRET_REF         credential reference the registry stores
//	LOAD_SECRET             material the resolver returns for that ref
//	LOAD_ACTOR_ID           registered actor the evidence is attributed to
//	LOAD_RUNNER_NAME        logical runner name stamped on the operation
//	LOAD_POLL_MS            sampling cadence (the runner-protocol interval)
//	LOAD_LOOP_MS            how long the loop sleeps between iterations
//	LOAD_CLAIM_BATCH        work items claimed per Tick
//	LOAD_SAMPLE_BATCH       operations sampled per pass
//	LOAD_TARGET             parked operations that end the dispatch phase
//	LOAD_MAX_SECONDS        hard self-destruct, so a leaked process dies
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "loadworker: %v\n", err)
		os.Exit(1)
	}
}

type config struct {
	dbURL, namespaceID, workerID           string
	runnerRef, endpoint, digest            string
	secretRef, secret, actorID, runnerName string
	poll, loop, maxRun                     time.Duration
	claimBatch, sampleBatch, target        int
}

// sample is one JSONL line: what the iteration did, and what this process
// looked like immediately afterwards.
type sample struct {
	Seq          int    `json:"seq"`
	Phase        string `json:"phase"`
	UnixMS       int64  `json:"unix_ms"`
	Dispatched   int    `json:"dispatched"`
	TickNS       int64  `json:"tick_ns"`
	Sampled      int    `json:"sampled"`
	PassNS       int64  `json:"pass_ns"`
	TotalSampled int    `json:"total_sampled"`
	Parked       int    `json:"parked"`
	// DBOps is the pgx pool's cumulative acquire count: one per statement
	// this process has sent to PostgreSQL outside a transaction, one per
	// transaction it has opened. Differenced across the observation window it
	// gives the database work per sample -- a COUNT, which unlike a wall-clock
	// cost does not move when the host underneath gets slower.
	DBOps int64 `json:"db_ops"`

	Goroutines int    `json:"goroutines"`
	Threads    int    `json:"threads"`
	HeapAlloc  uint64 `json:"heap_alloc"`
	HeapInuse  uint64 `json:"heap_inuse"`
	HeapSys    uint64 `json:"heap_sys"`
	Sys        uint64 `json:"sys"`
	NumGC      uint32 `json:"num_gc"`
	RSSKB      int64  `json:"rss_kb"`
	HWMKB      int64  `json:"hwm_kb"`
}

func loadConfig() (config, error) {
	var cfg config
	var err error
	for _, field := range []struct {
		name string
		dest *string
	}{
		{"LOAD_DB_URL", &cfg.dbURL},
		{"LOAD_NAMESPACE_ID", &cfg.namespaceID},
		{"LOAD_WORKER_ID", &cfg.workerID},
		{"LOAD_RUNNER_REF", &cfg.runnerRef},
		{"LOAD_RUNNER_ENDPOINT", &cfg.endpoint},
		{"LOAD_RUNNER_DIGEST", &cfg.digest},
		{"LOAD_SECRET_REF", &cfg.secretRef},
		{"LOAD_SECRET", &cfg.secret},
		{"LOAD_ACTOR_ID", &cfg.actorID},
		{"LOAD_RUNNER_NAME", &cfg.runnerName},
	} {
		if *field.dest, err = requireEnv(field.name); err != nil {
			return cfg, err
		}
	}
	for _, field := range []struct {
		name string
		dest *time.Duration
	}{
		{"LOAD_POLL_MS", &cfg.poll},
		{"LOAD_LOOP_MS", &cfg.loop},
	} {
		ms, err := requireEnvInt(field.name)
		if err != nil {
			return cfg, err
		}
		*field.dest = time.Duration(ms) * time.Millisecond
	}
	for _, field := range []struct {
		name string
		dest *int
	}{
		{"LOAD_CLAIM_BATCH", &cfg.claimBatch},
		{"LOAD_SAMPLE_BATCH", &cfg.sampleBatch},
		{"LOAD_TARGET", &cfg.target},
	} {
		if *field.dest, err = requireEnvInt(field.name); err != nil {
			return cfg, err
		}
	}
	maxSeconds, err := requireEnvInt("LOAD_MAX_SECONDS")
	if err != nil {
		return cfg, err
	}
	cfg.maxRun = time.Duration(maxSeconds) * time.Second
	return cfg, nil
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	s, err := postgres.Connect(ctx, cfg.dbURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer s.Close()

	wk, err := buildWorker(s, cfg)
	if err != nil {
		return err
	}
	return pollLoop(ctx, s, wk, cfg)
}

// buildWorker wires the real worker exactly as a deployment that dispatches
// its code to a runner service would: a registry holding one ServiceIdentity,
// a protocol client over a secret source, and no in-process CodeRunner at all.
//
// No Signer and no CallbackBaseURL, so no completion callback is ever offered.
// That is deliberate and it is what makes the measurement mean something: the
// operations below are learned about by POLLING ALONE, which is the load the
// sampling claim is about. A callback would tighten latency and would make the
// sampling numbers a mixture of two mechanisms.
func buildWorker(s *postgres.Store, cfg config) (*worker.Worker, error) {
	eng, err := postgres.NewEngine(s, cfg.namespaceID, engine.WithRetryDelays(0, 0))
	if err != nil {
		return nil, fmt.Errorf("NewEngine: %w", err)
	}

	registry := runners.NewFunctionRegistry()
	if err := registry.RegisterService(cfg.runnerRef, runners.ServiceIdentity{
		Endpoint:               cfg.endpoint,
		ImageDigest:            cfg.digest,
		SecretRef:              cfg.secretRef,
		AllowInsecureTransport: true,
	}); err != nil {
		return nil, fmt.Errorf("RegisterService: %w", err)
	}
	client, err := runners.NewProtocolClient(runners.StaticSecrets{cfg.secretRef: cfg.secret})
	if err != nil {
		return nil, fmt.Errorf("NewProtocolClient: %w", err)
	}

	wk, err := worker.New(s, eng, worker.Options{
		WorkerID:          cfg.workerID,
		NamespaceID:       cfg.namespaceID,
		ClaimBatch:        cfg.claimBatch,
		LeaseDuration:     60 * time.Second,
		HeartbeatInterval: 20 * time.Second,
		PollInterval:      cfg.poll,
		CodeRunnerName:    cfg.runnerName,
		CodeRunnerActorID: cfg.actorID,
		RunnerService: worker.RunnerServiceOptions{
			Registry:     registry,
			Client:       client,
			PollInterval: cfg.poll,
			SampleBatch:  cfg.sampleBatch,
		},
		OnError: func(err error) {
			fmt.Fprintf(os.Stderr, "worker %s: %v\n", cfg.workerID, err)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("worker.New: %w", err)
	}
	return wk, nil
}

const (
	phaseDispatch = "dispatch"
	phaseObserve  = "observe"
)

// pollLoop drives the real worker and emits one measurement line per
// iteration, until the process is signalled or LOAD_MAX_SECONDS elapses.
func pollLoop(ctx context.Context, s *postgres.Store, wk *worker.Worker, cfg config) error {
	enc := json.NewEncoder(os.Stdout)
	phase := phaseDispatch
	deadline := time.Now().Add(cfg.maxRun)

	for seq, total := 1, 0; ; seq++ {
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return nil
		}

		line := sample{Seq: seq, Phase: phase}
		sampled, err := runPollPhase(ctx, wk, phase, &line)
		if err != nil {
			return err
		}
		total += sampled
		line.TotalSampled = total

		parked, err := parkedCount(ctx, s, cfg.namespaceID)
		if err != nil {
			return err
		}
		line.Parked = parked
		if phase == phaseDispatch && parked >= cfg.target {
			phase = phaseObserve
		}

		recordProcessStats(&line, s)
		if err := enc.Encode(line); err != nil {
			return fmt.Errorf("emit sample: %w", err)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(cfg.loop):
		}
	}
}

// runPollPhase runs one iteration's worker step for phase, filling line's
// phase-specific fields, and returns how many operations were sampled this
// iteration. It is always 0 during the dispatch phase, since sampling there
// happens inside wk.Tick itself, untimed — see the package doc's "Two
// phases" for why that pass is never folded into this one's count.
func runPollPhase(ctx context.Context, wk *worker.Worker, phase string, line *sample) (int, error) {
	if phase == phaseDispatch {
		// Tick claims ready work, dispatches it (POST + park), and runs its
		// own sampler pass. Timed as a whole; not comparable with an
		// observe-phase pass and never used as one.
		start := time.Now()
		dispatched, err := wk.Tick(ctx)
		line.TickNS = time.Since(start).Nanoseconds()
		if err != nil {
			return 0, fmt.Errorf("Tick: %w", err)
		}
		line.Dispatched = dispatched
		return 0, nil
	}

	// The steady state: one pass over whatever is due, nothing else.
	start := time.Now()
	sampled, err := wk.SampleRunnerOperations(ctx)
	line.PassNS = time.Since(start).Nanoseconds()
	if err != nil {
		return 0, fmt.Errorf("SampleRunnerOperations: %w", err)
	}
	line.Sampled = sampled
	return sampled, nil
}

// recordProcessStats fills line's runtime/process measurement fields. It is
// always called immediately before the line is encoded and after every timed
// region above, exactly like pollLoop's single inline block used to.
func recordProcessStats(line *sample, s *postgres.Store) {
	line.DBOps = s.Pool().Stat().AcquireCount()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	rss, hwm, threads := procStatus()
	line.UnixMS = time.Now().UnixMilli()
	line.Goroutines = runtime.NumGoroutine()
	line.Threads = threads
	line.HeapAlloc = ms.HeapAlloc
	line.HeapInuse = ms.HeapInuse
	line.HeapSys = ms.HeapSys
	line.Sys = ms.Sys
	line.NumGC = ms.NumGC
	line.RSSKB = rss
	line.HWMKB = hwm
}

func parkedCount(ctx context.Context, s *postgres.Store, namespaceID string) (int, error) {
	var n int
	if err := s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM runner_invocations WHERE namespace_id = $1 AND state = 'waiting_external'`,
		namespaceID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count parked runner operations: %w", err)
	}
	return n, nil
}

// procStatus reads this process's resident set size, its high-water mark, and
// its OS thread count out of /proc/self/status. Zeroes on any platform that
// has no procfs; tests/load treats a zero RSS as "not measured here" rather
// than as a passing measurement.
func procStatus() (rssKB, hwmKB int64, threads int) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, 0, 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "VmRSS":
			rssKB = n
		case "VmHWM":
			hwmKB = n
		case "Threads":
			threads = int(n)
		}
	}
	return rssKB, hwmKB, threads
}

func requireEnv(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return v, nil
}

func requireEnvInt(name string) (int, error) {
	v, err := requireEnv(name)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return n, nil
}
