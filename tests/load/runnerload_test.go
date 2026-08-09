package loadtest

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/runners"
)

// The thresholds. Each is a named constant with a stated reason, because a
// load test whose numbers are unexplained magic is a load test nobody can
// argue with — and arguing with it is the point.
const (
	// goroutineBand is how much the goroutine count may differ between a
	// 10-operation worker and a 100- (or 1,000-) operation one. It is small
	// and absolute on purpose: this is the assertion that discriminates the
	// failure mode. A design that held one goroutine per in-flight operation
	// would show +90 here (or +990), not +8. The band is not zero because a
	// worker's goroutine count legitimately breathes — pgx pool health checks,
	// HTTP transport readLoop/writeLoop for the one connection in use, the
	// runtime's own — and none of those scale with parked operations.
	goroutineBand = 8
	// workerRSSBudgetKB is PRD §21.1's per-role resident-memory target for a
	// worker: 64 MiB. It is an absolute ceiling on the loaded process, not a
	// comparison, and it is the number a deployment actually has to live
	// within.
	workerRSSBudgetKB = 64 * 1024
	// marginalRSSPerOpKB bounds the RSS a worker may spend per additional
	// in-flight operation. 64 KiB is deliberately loose — it is a boundedness
	// check, not a tight budget — but it is finite, which is the claim: the
	// cost per in-flight operation must not be a growing per-operation
	// structure.
	marginalRSSPerOpKB = 64
	// sampleRateCeiling is the slack on the UPPER bound of the status-read
	// rate, which is ops/interval and is a contract property rather than a
	// performance one: "a runtime that sampled faster than a runner asked for
	// would be generating load the runner said it did not want"
	// (api/runner-protocol). Claiming a row pushes its next_poll_at out by a
	// whole interval, so the rate can only ever fall short of ops/interval,
	// never exceed it; the 5% is for the half-open window edges, not for the
	// mechanism.
	sampleRateCeiling = 1.05
	// sampleRateFloor is the slack on the LOWER bound, which is
	// ops/(interval + loop sleep + p95 pass). That denominator is the real
	// effective period: a sample's next due time is stamped when its own
	// status read returns, so an operation's period is the interval plus its
	// position in the pass plus however long the worker's loop happened to be
	// sleeping. All three terms are bounded by configuration; none is a
	// function of how long the operation runs.
	sampleRateFloor = 0.8
	// durationRatioTolerance bounds how much two fleets that differ ONLY in
	// how long their operations run may differ in sampling cost. A factor of
	// two either way is loose, and it has to be: these are wall-clock
	// measurements of database round trips on a shared box. What it excludes
	// is the shape that would matter — a cost that tracked operation duration
	// would show a factor of ten here, because the durations differ by ten.
	durationRatioTolerance = 2.0
)

// c17/h12, the memory half: a worker holding 100 concurrent in-flight runner
// operations costs no more goroutines than one holding 10, and its resident
// memory stays inside §21.1's worker budget.
//
// The control fleet is what makes this a measurement rather than an assertion
// about a number somebody once saw. Both fleets run the same binary, against
// the same PostgreSQL, on the same host, minutes apart; the only difference is
// how many operations are in flight. A per-operation goroutine, a per-
// operation held connection, or a per-operation timer would all show up as a
// slope between the two points. The test asserts there is no slope worth
// speaking of.
//
// It also proves the fleet was real: 100 operations parked, each dispatched to
// the runner exactly once, and all 100 committed through
// engine.CompleteAttempt once the stub released them.
func TestLoadHundredConcurrentRunnerOperationsBoundedWorker(t *testing.T) {
	s := requireStore(t)

	base := fleetConfig{
		poll:            time.Second,
		loop:            200 * time.Millisecond,
		sampleBatch:     256,
		claimBatch:      32,
		opDuration:      0, // never finishes on its own; released deliberately
		settle:          time.Second,
		observe:         8 * time.Second,
		release:         true,
		dispatchTimeout: 90 * time.Second,
		releaseTimeout:  120 * time.Second,
	}

	controlCfg, loadCfg := base, base
	controlCfg.name, controlCfg.ops = "control-10", 10
	loadCfg.name, loadCfg.ops = "load-100", 100

	control := measureFleet(t, s, controlCfg)
	control.report(t)
	load := measureFleet(t, s, loadCfg)
	load.report(t)

	assertFleetIsHonest(t, control)
	assertFleetIsHonest(t, load)
	assertBounded(t, control, load)
}

// The same measurement at 1,000 in-flight operations, at the runner protocol's
// own default 5-second sampling interval.
//
// Opt-in rather than default because it seeds and commits 1,000 runs through a
// real engine against a real PostgreSQL, which is a minute or two of database
// work — worth running, not worth running on every `go test ./...`:
//
//	NODES_LOAD_1000=1 go test ./tests/load/ -run Thousand -v -timeout 20m
//
// The interval is 5s rather than the 100-case's 1s deliberately: it is
// runners.DefaultPollInterval, so this fleet is what a deployment that changed
// no knobs would actually experience.
func TestLoadThousandConcurrentRunnerOperationsBoundedWorker(t *testing.T) {
	if os.Getenv("NODES_LOAD_1000") == "" {
		t.Skip("set NODES_LOAD_1000=1 to run the 1,000-operation fleet (seeds and commits 1,000 runs)")
	}
	s := requireStore(t)

	base := fleetConfig{
		poll:            runners.DefaultPollInterval,
		loop:            250 * time.Millisecond,
		sampleBatch:     2048,
		claimBatch:      64,
		opDuration:      0,
		settle:          runners.DefaultPollInterval,
		observe:         30 * time.Second,
		release:         true,
		dispatchTimeout: 10 * time.Minute,
		releaseTimeout:  10 * time.Minute,
	}

	controlCfg, loadCfg := base, base
	controlCfg.name, controlCfg.ops = "control-10-5s", 10
	loadCfg.name, loadCfg.ops = "load-1000-5s", 1000

	control := measureFleet(t, s, controlCfg)
	control.report(t)
	load := measureFleet(t, s, loadCfg)
	load.report(t)

	assertFleetIsHonest(t, control)
	assertFleetIsHonest(t, load)
	assertBounded(t, control, load)
}

// c17/h12, the sampling half: the cost of tracking in-flight operations is a
// function of how many there are and how often they are sampled, and of
// nothing else — in particular not of how long the operations run.
//
// The method is a controlled pair. Two fleets, identical in every respect —
// same operation count, same sampling interval, same batch, same host, same
// PostgreSQL — except that one fleet's operations finish after 30 seconds and
// the other's after 300. Neither fleet's operations finish DURING the
// observation window, so what is compared is the steady-state cost of waiting
// on work of two very different lengths.
//
// If sampling cost tracked duration in any way, a tenfold difference in
// duration would have to show up somewhere in a tenfold-ish shape. The
// assertions bound the observed difference at a factor of two, which is loose
// for wall-clock measurements and still an order of magnitude away from what a
// duration-dependent design would produce.
func TestLoadSamplingCostIsIndependentOfOperationDuration(t *testing.T) {
	s := requireStore(t)

	base := fleetConfig{
		ops:             50,
		poll:            time.Second,
		loop:            200 * time.Millisecond,
		sampleBatch:     128,
		claimBatch:      32,
		settle:          time.Second,
		observe:         8 * time.Second,
		release:         false,
		dispatchTimeout: 90 * time.Second,
		releaseTimeout:  30 * time.Second,
	}

	shortCfg, longCfg := base, base
	shortCfg.name, shortCfg.opDuration = "duration-30s", 30*time.Second
	longCfg.name, longCfg.opDuration = "duration-300s", 300*time.Second

	short := measureFleet(t, s, shortCfg)
	short.report(t)
	long := measureFleet(t, s, longCfg)
	long.report(t)

	for _, f := range []fleetResult{short, long} {
		assertFleetIsHonest(t, f)
		assertSampleRate(t, f)
		if f.sampledTotal == 0 {
			t.Fatalf("fleet %s sampled nothing in its observation window", f.cfg.name)
		}
	}

	// Nothing finished while either fleet was observed: both were measured
	// mid-flight, which is the only state in which the comparison means
	// anything.
	for _, f := range []fleetResult{short, long} {
		if f.completedRuns != 0 {
			t.Errorf("fleet %s completed %d runs during observation; it was not measured in flight",
				f.cfg.name, f.completedRuns)
		}
	}

	rateRatio := ratio(long.statusRate, short.statusRate)
	if rateRatio < 1/durationRatioTolerance || rateRatio > durationRatioTolerance {
		t.Errorf("status-read rate: %.2f/s at 300s operations vs %.2f/s at 30s operations (ratio %.2f); "+
			"a tenfold change in operation duration must not move the sampling rate",
			long.statusRate, short.statusRate, rateRatio)
	}

	costRatio := ratio(float64(long.perOpSampleNS), float64(short.perOpSampleNS))
	if costRatio < 1/durationRatioTolerance || costRatio > durationRatioTolerance {
		t.Errorf("per-operation sample cost: %s at 300s operations vs %s at 30s operations (ratio %.2f); "+
			"the cost of one sample must not depend on how long the operation runs",
			time.Duration(long.perOpSampleNS), time.Duration(short.perOpSampleNS), costRatio)
	}

	dutyRatio := ratio(long.dutyCycle, short.dutyCycle)
	if dutyRatio < 1/durationRatioTolerance || dutyRatio > durationRatioTolerance {
		t.Errorf("sampler duty cycle: %.4f at 300s operations vs %.4f at 30s operations (ratio %.2f)",
			long.dutyCycle, short.dutyCycle, dutyRatio)
	}

	t.Logf("duration independence: 30s vs 300s operations — status rate %.2f/s vs %.2f/s (ratio %.2f), "+
		"per-sample cost %s vs %s (ratio %.2f), duty cycle %.4f vs %.4f (ratio %.2f)",
		short.statusRate, long.statusRate, rateRatio,
		time.Duration(short.perOpSampleNS), time.Duration(long.perOpSampleNS), costRatio,
		short.dutyCycle, long.dutyCycle, dutyRatio)
}

// The stub's authentication is not decorative: it refuses an unauthenticated
// status read, so every sample the fleets above measured paid for a bearer
// credential the way a real deployment does. Needs no database.
func TestLoadStubRunnerRefusesUnauthenticatedRequests(t *testing.T) {
	stub := newStubRunner(0, 0)
	server := stub.start()
	defer server.Close()

	for _, tc := range []struct {
		name   string
		header string
	}{
		{"no credential", ""},
		{"wrong credential", "Bearer " + stubSecret + "-tampered"},
		{"the reference rather than the material", "Bearer " + stubSecretRef},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, server.URL+runners.OperationsPath+"/op_whatever", nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			if tc.header != "" {
				req.Header.Set(runners.AuthorizationHeader, tc.header)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("status read: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status read answered %d, want 401: an unauthenticated runner service is not the one the "+
					"load numbers were measured against", resp.StatusCode)
			}
		})
	}

	if _, _, unauthorized := stub.counts(); unauthorized != 3 {
		t.Fatalf("stub counted %d refusals, want 3", unauthorized)
	}
}

// assertFleetIsHonest checks the things that must hold for a fleet's numbers to
// mean anything at all: the operations really were dispatched, exactly once
// each, and the worker really was measured while they were in flight.
func assertFleetIsHonest(t *testing.T, f fleetResult) {
	t.Helper()
	if f.dispatchesSeen != f.cfg.ops {
		t.Errorf("fleet %s: the runner accepted %d operations, want %d", f.cfg.name, f.dispatchesSeen, f.cfg.ops)
	}
	if f.redispatches != 0 {
		t.Errorf("fleet %s: the runner saw %d re-sends of an already-accepted operation, want 0: "+
			"a parked operation must not be dispatched twice", f.cfg.name, f.redispatches)
	}
	if f.unauthorized != 0 {
		t.Errorf("fleet %s: %d requests reached the runner without a valid credential", f.cfg.name, f.unauthorized)
	}
	if f.parkedPeak != f.cfg.ops {
		t.Errorf("fleet %s: peak parked operations = %d, want %d: the fleet was never fully in flight",
			f.cfg.name, f.parkedPeak, f.cfg.ops)
	}
	// An attempts row is written when an attempt COMPLETES, so a fleet
	// measured mid-flight has none and a released fleet has exactly one per
	// operation. Both are checked, because "more than one" would mean the
	// parked path retried something it had already dispatched.
	if f.attempts > f.cfg.ops {
		t.Errorf("fleet %s: %d attempts recorded for %d operations; something was attempted twice",
			f.cfg.name, f.attempts, f.cfg.ops)
	}
	if f.cfg.release && f.attempts != f.cfg.ops {
		t.Errorf("fleet %s: %d attempts recorded after release, want %d (one per operation, no retries)",
			f.cfg.name, f.attempts, f.cfg.ops)
	}
	if f.sampledTotal == 0 {
		t.Errorf("fleet %s: no operation was sampled during the observation window", f.cfg.name)
	}
	if f.medRSSKB <= 0 {
		t.Skipf("fleet %s reported no RSS (no /proc on this platform); the memory claim cannot be measured here",
			f.cfg.name)
	}
	if f.cfg.release && f.completedRuns != f.cfg.ops {
		t.Errorf("fleet %s: %d runs completed after release, want %d: the parked rows were not real in-flight work",
			f.cfg.name, f.completedRuns, f.cfg.ops)
	}
}

// assertSampleRate brackets the measured status-read rate between the two
// bounds the design actually promises.
//
// The ceiling, ops/interval, is the one that matters to whoever operates the
// runner: no matter how eagerly this worker's loop spins, claiming a row
// pushes its next sample a whole interval out, so the runner cannot be
// hammered by a fast loop. The floor says the sampler is keeping up — it has
// not fallen so far behind that operations are being sampled at some slower
// cadence than configured.
func assertSampleRate(t *testing.T, f fleetResult) {
	t.Helper()

	ceiling := f.expectedRate * sampleRateCeiling
	if f.statusRate > ceiling {
		t.Errorf("fleet %s: measured %.2f status reads/s against a ceiling of %.2f/s (ops/interval = %d/%s); "+
			"the runtime must never sample a runner faster than the interval it was configured with",
			f.cfg.name, f.statusRate, ceiling, f.cfg.ops, f.cfg.poll)
	}

	effectivePeriod := f.cfg.poll + f.cfg.loop + time.Duration(f.p95PassNS)
	floor := float64(f.cfg.ops) / effectivePeriod.Seconds() * sampleRateFloor
	if f.statusRate < floor {
		t.Errorf("fleet %s: measured %.2f status reads/s against a floor of %.2f/s "+
			"(ops / (interval %s + loop %s + p95 pass %s)); the sampler is falling behind its own cadence",
			f.cfg.name, f.statusRate, floor, f.cfg.poll, f.cfg.loop, time.Duration(f.p95PassNS))
	}

	if f.minPerOpReads == 0 {
		t.Errorf("fleet %s: at least one operation was never sampled", f.cfg.name)
	}
}

// assertBounded is the comparison itself: what changed between a small fleet
// and a large one running the same code on the same host.
func assertBounded(t *testing.T, control, load fleetResult) {
	t.Helper()
	extraOps := load.cfg.ops - control.cfg.ops
	if extraOps <= 0 {
		t.Fatalf("assertBounded needs the load fleet to be larger than the control (%d vs %d)",
			load.cfg.ops, control.cfg.ops)
	}

	// The discriminating assertion. One goroutine per in-flight operation
	// would put extraOps more goroutines in the loaded process.
	goroutineDelta := load.medGoroutines - control.medGoroutines
	if goroutineDelta > goroutineBand {
		t.Errorf("goroutines: %d in flight → median %d, %d in flight → median %d (delta %d, band %d). "+
			"Something in the parked path scales with in-flight operations",
			control.cfg.ops, control.medGoroutines, load.cfg.ops, load.medGoroutines, goroutineDelta, goroutineBand)
	}
	if peakDelta := load.maxGoroutines - control.maxGoroutines; peakDelta > goroutineBand {
		t.Errorf("peak goroutines: %d vs %d (delta %d, band %d)",
			load.maxGoroutines, control.maxGoroutines, peakDelta, goroutineBand)
	}

	// OS threads are the second place a per-operation blocking structure would
	// surface, because a blocked syscall pins one.
	if threadDelta := load.maxThreads - control.maxThreads; threadDelta > goroutineBand {
		t.Errorf("OS threads: %d at %d in flight vs %d at %d in flight (delta %d, band %d)",
			load.maxThreads, load.cfg.ops, control.maxThreads, control.cfg.ops, threadDelta, goroutineBand)
	}

	// The budget: an absolute ceiling the loaded worker has to live within.
	if load.maxRSSKB > workerRSSBudgetKB {
		t.Errorf("peak RSS at %d in flight = %d KiB, over §21.1's %d KiB worker budget",
			load.cfg.ops, load.maxRSSKB, workerRSSBudgetKB)
	}

	// The slope: RSS per additional in-flight operation. Bounded, not zero.
	marginalKB := float64(load.medRSSKB-control.medRSSKB) / float64(extraOps)
	if marginalKB > marginalRSSPerOpKB {
		t.Errorf("marginal RSS = %.1f KiB per additional in-flight operation (%d KiB at %d ops vs %d KiB at %d ops), "+
			"over the %d KiB/op bound", marginalKB,
			load.medRSSKB, load.cfg.ops, control.medRSSKB, control.cfg.ops, marginalRSSPerOpKB)
	}

	assertSampleRate(t, load)

	t.Logf("boundedness: %d → %d in-flight operations moved goroutines %d → %d, OS threads %d → %d, "+
		"median RSS %d → %d KiB (%.1f KiB/op marginal), median heap_alloc %d → %d KiB; "+
		"peak RSS %d KiB against a %d KiB budget",
		control.cfg.ops, load.cfg.ops,
		control.medGoroutines, load.medGoroutines,
		control.medThreads, load.medThreads,
		control.medRSSKB, load.medRSSKB, marginalKB,
		control.medHeapAllocKB, load.medHeapAllocKB,
		load.maxRSSKB, workerRSSBudgetKB)
}
