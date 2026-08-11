package loadtest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// One measured fleet, end to end.
//
// The shape of a measurement is always the same, and every test in this
// package is a comparison between two of them:
//
//	seed     create `ops` runs of the one-code-node workflow through the real
//	         engine, so every work item the worker claims is one the engine
//	         produced
//	dispatch start the worker process; it claims, POSTs, and parks until all
//	         `ops` operations are in flight
//	observe  let it sit in the steady state for `observe`, reading its own
//	         goroutine count, heap and RSS once per loop while it samples
//	release  optionally tell the stub every operation has finished, and time
//	         how long the fleet takes to commit through the ordinary path
//
// The observation window deliberately starts one settle interval AFTER the
// last operation parked. The transient — a hundred dispatches in flight, the
// pgx pool warming, the first sampler passes catching a backlog — is real, but
// it is not the steady state the c17/h12 claim is about, and averaging it in
// would understate the sampling cadence and overstate the memory.

type fleetConfig struct {
	// name labels the fleet in test output and in docs/benchmarks.md.
	name string
	// ops is the number of concurrent in-flight runner operations.
	ops int
	// poll is the sampling interval the worker is configured with, and the
	// denominator of the expected sampling rate (ops / poll).
	poll time.Duration
	// loop is how long the worker sleeps between iterations. It is
	// deliberately much shorter than poll: the cadence that reaches the
	// runner must come from next_poll_at in the table, not from how often the
	// process happens to wake up. A loop far faster than poll is what proves
	// that.
	loop time.Duration
	// sampleBatch bounds one sampler pass. Sized at or above ops so a pass is
	// never the thing that limits the cadence.
	sampleBatch int
	// claimBatch bounds one Tick's claim.
	claimBatch int
	// opDuration is how long the stub holds an operation non-terminal. It is
	// the variable the duration-independence test varies and nothing else
	// does.
	opDuration time.Duration
	// settle is how long after the last park the observation window starts.
	settle time.Duration
	// observe is the length of the steady-state observation window.
	observe time.Duration
	// release drives the fleet to completion afterwards, proving the parked
	// rows were genuine in-flight work.
	release bool
	// dispatchTimeout bounds how long the dispatch phase may take before the
	// measurement is declared failed.
	dispatchTimeout time.Duration
	// releaseTimeout bounds the release phase the same way.
	releaseTimeout time.Duration
}

type fleetResult struct {
	cfg fleetConfig

	seedWall     time.Duration
	dispatchWall time.Duration
	releaseWall  time.Duration

	// Steady-state process measurements, over the observation window.
	medGoroutines, maxGoroutines int
	medThreads, maxThreads       int
	medRSSKB, maxRSSKB           int64
	hwmKB                        int64
	medHeapAllocKB               int64
	maxHeapAllocKB               int64
	maxHeapSysKB                 int64
	maxSysKB                     int64

	// Steady-state sampling measurements, over the same window.
	windowWall    time.Duration
	passes        int
	sampledTotal  int
	passNSTotal   int64
	medPassNS     int64
	p95PassNS     int64
	perOpSampleNS int64
	dutyCycle     float64
	// dbOpsPerSample is the database work one sample costs: statements (and
	// transactions) the worker process sent over the observation window,
	// divided by the operations it sampled in it. It is a count rather than a
	// duration, which is what makes it comparable between two fleets measured
	// at different times -- see the sampling-cost test.
	dbOpsInWindow  int64
	dbOpsPerSample float64
	statusReads    int
	statusRate     float64
	expectedRate   float64
	minPerOpReads  int
	maxPerOpReads  int
	dispatchesSeen int
	redispatches   int
	unauthorized   int

	completedRuns int
	attempts      int
	parkedPeak    int
}

// sample mirrors testdata/loadworker's own JSONL line. It is duplicated rather
// than shared because the fixture lives under testdata/ precisely so nothing
// imports it.
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
	DBOps        int64  `json:"db_ops"`
	Goroutines   int    `json:"goroutines"`
	Threads      int    `json:"threads"`
	HeapAlloc    uint64 `json:"heap_alloc"`
	HeapInuse    uint64 `json:"heap_inuse"`
	HeapSys      uint64 `json:"heap_sys"`
	Sys          uint64 `json:"sys"`
	NumGC        uint32 `json:"num_gc"`
	RSSKB        int64  `json:"rss_kb"`
	HWMKB        int64  `json:"hwm_kb"`
}

func (s sample) at() time.Time { return time.UnixMilli(s.UnixMS) }

// effectivePeriod is how often one operation was actually sampled, derived
// from the measured rate rather than from the configuration. It is always at
// least the configured interval, because claiming a row is what reschedules
// it — see assertSampleRate.
func (r fleetResult) effectivePeriod() time.Duration {
	if r.statusRate <= 0 {
		return 0
	}
	return time.Duration(float64(r.cfg.ops) / r.statusRate * float64(time.Second))
}

// measureFleet runs one whole measurement and returns what it read.
func measureFleet(t *testing.T, s *postgres.Store, cfg fleetConfig) fleetResult {
	t.Helper()

	ns := mustNamespace(t, s, "load-"+cfg.name)
	actorID := mustRunnerActor(t, s, ns.ID)

	stub := newStubRunner(cfg.opDuration, 250*time.Millisecond)
	server := stub.start()
	defer server.Close()

	seedStart := time.Now()
	seedRuns(t, s, ns.ID, cfg.ops)
	res := fleetResult{cfg: cfg, seedWall: time.Since(seedStart)}

	proc := startLoadWorker(t, loadWorkerEnv{
		namespaceID: ns.ID,
		workerID:    "load-worker-" + cfg.name,
		endpoint:    server.URL,
		actorID:     actorID,
		cfg:         cfg,
	})
	defer proc.stop(t)

	// Dispatch phase: every operation POSTed, accepted, and parked.
	dispatchStart := time.Now()
	parked := proc.waitFor(t, cfg.dispatchTimeout, fmt.Sprintf("all %d operations parked", cfg.ops),
		func(samples []sample) bool {
			for _, sm := range samples {
				if sm.Parked >= cfg.ops {
					return true
				}
			}
			return false
		})
	res.dispatchWall = time.Since(dispatchStart)
	_ = parked

	// Steady state.
	time.Sleep(cfg.settle)
	windowStart := time.Now()
	time.Sleep(cfg.observe)
	windowEnd := time.Now()

	res.statusReads = stub.readsBetween(windowStart, windowEnd)
	res.windowWall = windowEnd.Sub(windowStart)
	res.statusRate = float64(res.statusReads) / res.windowWall.Seconds()
	res.expectedRate = float64(cfg.ops) / cfg.poll.Seconds()
	res.minPerOpReads, res.maxPerOpReads = stub.perOperationReads()
	res.dispatchesSeen, res.redispatches, res.unauthorized = stub.counts()

	all := proc.snapshot()
	window := make([]sample, 0, len(all))
	for _, sm := range all {
		at := sm.at()
		if sm.Phase == "observe" && !at.Before(windowStart) && at.Before(windowEnd) {
			window = append(window, sm)
		}
	}
	if len(window) == 0 {
		t.Fatalf("fleet %s: no observe-phase samples inside the observation window\n%s", cfg.name, proc.diagnostics())
	}
	summarise(&res, all, window)

	if cfg.release {
		releaseStart := time.Now()
		stub.finishAll()
		res.completedRuns = waitForCompletedRuns(t, s, ns.ID, cfg.ops, cfg.releaseTimeout, proc)
		res.releaseWall = time.Since(releaseStart)
	}
	res.attempts = countAttempts(t, s, ns.ID)
	return res
}

// summarise turns the raw JSONL into the numbers the assertions and
// docs/benchmarks.md are written from.
func summarise(res *fleetResult, all, window []sample) {
	goroutines := make([]int, 0, len(window))
	threads := make([]int, 0, len(window))
	rss := make([]int64, 0, len(window))
	heap := make([]int64, 0, len(window))
	passes := make([]int64, 0, len(window))

	for _, sm := range window {
		goroutines = append(goroutines, sm.Goroutines)
		threads = append(threads, sm.Threads)
		rss = append(rss, sm.RSSKB)
		heap = append(heap, int64(sm.HeapAlloc)/1024)
		if sm.Sampled > 0 {
			passes = append(passes, sm.PassNS)
		}
		res.passNSTotal += sm.PassNS
		res.sampledTotal += sm.Sampled
		res.maxHeapSysKB = maxInt64(res.maxHeapSysKB, int64(sm.HeapSys)/1024)
		res.maxSysKB = maxInt64(res.maxSysKB, int64(sm.Sys)/1024)
	}
	res.passes = len(window)

	res.medGoroutines, res.maxGoroutines = medianInt(goroutines), maxInt(goroutines)
	res.medThreads, res.maxThreads = medianInt(threads), maxInt(threads)
	res.medRSSKB, res.maxRSSKB = medianInt64(rss), maxInt64s(rss)
	res.medHeapAllocKB, res.maxHeapAllocKB = medianInt64(heap), maxInt64s(heap)
	res.medPassNS, res.p95PassNS = medianInt64(passes), percentileInt64(passes, 0.95)

	// Database work over the window, differenced between its first and last
	// samples. Both endpoints are read after their own iteration's timed
	// region, so the difference covers exactly the passes summed above.
	res.dbOpsInWindow = window[len(window)-1].DBOps - window[0].DBOps
	if res.sampledTotal > 0 {
		res.dbOpsPerSample = float64(res.dbOpsInWindow) / float64(res.sampledTotal)
	}
	if res.sampledTotal > 0 {
		res.perOpSampleNS = res.passNSTotal / int64(res.sampledTotal)
	}
	if res.windowWall > 0 {
		res.dutyCycle = float64(res.passNSTotal) / float64(res.windowWall.Nanoseconds())
	}

	// The high-water mark and the parked peak are read over EVERY sample,
	// including the dispatch transient: a peak that happened while a hundred
	// operations were being POSTed is still a peak this process reached.
	for _, sm := range all {
		res.hwmKB = maxInt64(res.hwmKB, sm.HWMKB)
		if sm.Parked > res.parkedPeak {
			res.parkedPeak = sm.Parked
		}
	}
}

// report prints one fleet as a block, in the form the benchmarks document
// quotes. Tests always log both fleets, pass or fail, because a measurement
// that only prints on failure is a measurement nobody records.
func (r fleetResult) report(t *testing.T) {
	t.Helper()
	t.Logf(`fleet %s — %d in-flight operations, %s sampling interval
  seed / dispatch / release wall : %s / %s / %s
  goroutines (median / max)      : %d / %d      threads (median / max): %d / %d
  RSS KiB (median / max / HWM)   : %d / %d / %d
  heap_alloc KiB (median / max)  : %d / %d      heap_sys max: %d KiB   sys max: %d KiB
  observation window             : %s over %d passes, %d operations sampled
  sample pass ns (median / p95)  : %d / %d
  per-operation sample cost      : %s          sampler duty cycle: %.4f
  database work per sample       : %.2f operations (%d over the window)
  status reads in window         : %d (%.2f/s measured, ceiling %.2f/s = ops/interval)
  effective per-operation period : %s (configured interval %s)
  per-operation reads (min/max)  : %d / %d
  runner dispatches / re-sends / unauthorized : %d / %d / %d
  peak parked / attempts / completed runs     : %d / %d / %d`,
		r.cfg.name, r.cfg.ops, r.cfg.poll,
		r.seedWall.Round(time.Millisecond), r.dispatchWall.Round(time.Millisecond), r.releaseWall.Round(time.Millisecond),
		r.medGoroutines, r.maxGoroutines, r.medThreads, r.maxThreads,
		r.medRSSKB, r.maxRSSKB, r.hwmKB,
		r.medHeapAllocKB, r.maxHeapAllocKB, r.maxHeapSysKB, r.maxSysKB,
		r.windowWall.Round(time.Millisecond), r.passes, r.sampledTotal,
		r.medPassNS, r.p95PassNS,
		time.Duration(r.perOpSampleNS), r.dutyCycle,
		r.dbOpsPerSample, r.dbOpsInWindow,
		r.statusReads, r.statusRate, r.expectedRate,
		r.effectivePeriod().Round(time.Millisecond), r.cfg.poll,
		r.minPerOpReads, r.maxPerOpReads,
		r.dispatchesSeen, r.redispatches, r.unauthorized,
		r.parkedPeak, r.attempts, r.completedRuns)
}

// seedRuns creates `ops` runs of the one-code-node workflow through the real
// engine. The creation is parallel only to keep the fixture's cost off the
// measurement — it happens before the worker starts, and the worker is the
// only thing being measured.
func seedRuns(t *testing.T, s *postgres.Store, namespaceID string, ops int) {
	t.Helper()
	cw := compileWorkflow(t)
	eng, err := postgres.NewEngine(s, namespaceID, engine.WithRetryDelays(0, 0))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	const seedConcurrency = 8
	if err := runSeedWorkers(seedWork(ops), seedConcurrency, cw, eng); err != nil {
		t.Fatalf("seed %d runs: %v", ops, err)
	}
}

// seedWork returns a closed, pre-filled channel of the run indices seedRuns
// hands out to its worker goroutines — pre-filled and closed up front so a
// worker goroutine's `for range work` simply drains it and exits.
func seedWork(ops int) chan int {
	work := make(chan int, ops)
	for i := 0; i < ops; i++ {
		work <- i
	}
	close(work)
	return work
}

// runSeedWorkers fans `work` out across `concurrency` goroutines, each
// calling eng.CreateRun once per item, and returns the first error any
// goroutine hit (nil if none did). A goroutine that errors stops claiming
// further work rather than piling up more failures behind the first one.
func runSeedWorkers(work <-chan int, concurrency int, cw *compiler.CompiledWorkflow, eng *engine.Engine) error {
	var (
		wg      sync.WaitGroup
		errMu   sync.Mutex
		firstEr error
	)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range work {
				if _, err := eng.CreateRun(context.Background(), cw, json.RawMessage(`{}`)); err != nil {
					errMu.Lock()
					if firstEr == nil {
						firstEr = err
					}
					errMu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstEr
}

type loadWorkerEnv struct {
	namespaceID string
	workerID    string
	endpoint    string
	actorID     string
	cfg         fleetConfig
}

// workerProc is one running copy of testdata/loadworker, with its JSONL
// measurement stream parsed as it arrives.
type workerProc struct {
	cmd     *exec.Cmd
	mu      sync.Mutex
	samples []sample
	stderr  strings.Builder
	scanned chan struct{}
}

func startLoadWorker(t *testing.T, env loadWorkerEnv) *workerProc {
	t.Helper()
	cfg := env.cfg

	// The self-destruct is generous but finite: whatever the test does, this
	// process is gone within it.
	maxSeconds := int((cfg.dispatchTimeout + cfg.settle + cfg.observe + cfg.releaseTimeout).Seconds()) + 60

	cmd := exec.Command(loadWorkerBin)
	cmd.Env = append(os.Environ(),
		"LOAD_DB_URL="+testDBURL,
		"LOAD_NAMESPACE_ID="+env.namespaceID,
		"LOAD_WORKER_ID="+env.workerID,
		"LOAD_RUNNER_REF="+stubRunnerRef,
		"LOAD_RUNNER_ENDPOINT="+env.endpoint,
		"LOAD_RUNNER_DIGEST="+stubDigest,
		"LOAD_SECRET_REF="+stubSecretRef,
		"LOAD_SECRET="+stubSecret,
		"LOAD_ACTOR_ID="+env.actorID,
		"LOAD_RUNNER_NAME="+stubRunnerNm,
		fmt.Sprintf("LOAD_POLL_MS=%d", cfg.poll.Milliseconds()),
		fmt.Sprintf("LOAD_LOOP_MS=%d", cfg.loop.Milliseconds()),
		fmt.Sprintf("LOAD_CLAIM_BATCH=%d", cfg.claimBatch),
		fmt.Sprintf("LOAD_SAMPLE_BATCH=%d", cfg.sampleBatch),
		fmt.Sprintf("LOAD_TARGET=%d", cfg.ops),
		fmt.Sprintf("LOAD_MAX_SECONDS=%d", maxSeconds),
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	p := &workerProc{cmd: cmd, scanned: make(chan struct{})}
	cmd.Stderr = &lockedWriter{p: p}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start loadworker %s: %v", env.workerID, err)
	}

	go func() {
		defer close(p.scanned)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for scanner.Scan() {
			var sm sample
			if err := json.Unmarshal(scanner.Bytes(), &sm); err != nil {
				continue
			}
			p.mu.Lock()
			p.samples = append(p.samples, sm)
			p.mu.Unlock()
		}
	}()

	t.Cleanup(func() {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	})
	return p
}

// lockedWriter funnels the child's stderr into the same mutex the samples use,
// so a test can quote diagnostics while the process is still running.
type lockedWriter struct{ p *workerProc }

func (w *lockedWriter) Write(b []byte) (int, error) {
	w.p.mu.Lock()
	defer w.p.mu.Unlock()
	return w.p.stderr.Write(b)
}

func (p *workerProc) snapshot() []sample {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]sample, len(p.samples))
	copy(out, p.samples)
	return out
}

// diagnostics is what a failing assertion prints: the worker's own stderr and
// the last few measurement lines, which between them say what the process was
// doing when the expectation was not met.
func (p *workerProc) diagnostics() string {
	samples := p.snapshot()
	tail := samples
	if len(tail) > 5 {
		tail = tail[len(tail)-5:]
	}
	var b strings.Builder
	b.WriteString("--- loadworker stderr ---\n")
	p.mu.Lock()
	b.WriteString(p.stderr.String())
	p.mu.Unlock()
	fmt.Fprintf(&b, "--- last %d of %d measurement lines ---\n", len(tail), len(samples))
	for _, sm := range tail {
		line, _ := json.Marshal(sm)
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// waitFor blocks until pred is satisfied by the samples emitted so far.
func (p *workerProc) waitFor(t *testing.T, timeout time.Duration, what string, pred func([]sample) bool) []sample {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		samples := p.snapshot()
		if pred(samples) {
			return samples
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s\n%s", timeout, what, p.diagnostics())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stop asks the worker to finish, then insists.
func (p *workerProc) stop(t *testing.T) {
	t.Helper()
	if p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = p.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = p.cmd.Process.Kill()
		<-done
	}
	<-p.scanned
}

func waitForCompletedRuns(t *testing.T, s *postgres.Store, namespaceID string, want int, timeout time.Duration, p *workerProc) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got int
	for {
		if err := s.Pool().QueryRow(context.Background(),
			`SELECT count(*) FROM runs WHERE namespace_id = $1 AND status = 'completed'`, namespaceID,
		).Scan(&got); err != nil {
			t.Fatalf("count completed runs: %v", err)
		}
		if got >= want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("completed runs = %d after %s, want %d: the released fleet did not commit\n%s",
				got, timeout, want, p.diagnostics())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func countAttempts(t *testing.T, s *postgres.Store, namespaceID string) int {
	t.Helper()
	var n int
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM attempts WHERE namespace_id = $1`, namespaceID).Scan(&n); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	return n
}

func medianInt(v []int) int {
	if len(v) == 0 {
		return 0
	}
	s := append([]int(nil), v...)
	sort.Ints(s)
	return s[len(s)/2]
}

func maxInt(v []int) int {
	out := 0
	for _, n := range v {
		if n > out {
			out = n
		}
	}
	return out
}

func medianInt64(v []int64) int64 { return percentileInt64(v, 0.5) }

func percentileInt64(v []int64, p float64) int64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := int(float64(len(s)-1) * p)
	return s[idx]
}

func maxInt64s(v []int64) int64 {
	var out int64
	for _, n := range v {
		out = maxInt64(out, n)
	}
	return out
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func ratio(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}
