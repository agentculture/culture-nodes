# Recorded benchmarks

PRD §21.1 asks for a resource-budget profile and §21.2 for an initial
benchmark profile. This file records what was actually measured, on the
machine it was measured on.

**These numbers are recorded, not gated.** No CI job fails on them. They exist
so a later change that halves throughput is visible rather than invisible, and
so nobody has to guess what "fast enough" meant on the day.

## The host, stated plainly

| | |
| --- | --- |
| Date | 2026-08-09 |
| Machine | shared arm64 development box (`aarch64`, 20 logical CPUs, 121 GiB RAM, **99 GiB of it already in use by other work**) |
| Kernel | Linux 6.17.0-1008-nvidia |
| Go | go1.26.5 |
| Docker | 29.1.3 |
| PostgreSQL | `postgres:17-alpine` in a Docker container **on the same host**, default configuration, no tuning |
| headspace-cli | 0.11.0, `python3.12` profile (`python:3.12-slim@sha256:57cd7c3a…`) |

**This is not PRD §21.2's reference environment.** That section specifies "a
documented 4-vCPU, 8-GiB Linux environment with PostgreSQL". This box has more
CPUs and far more RAM, but it is *shared* and under load, and its PostgreSQL
is an untuned container competing for the same disk. Every number below is
therefore a **floor with unknown headroom**, not a capacity claim. Comparing
them against §21.2's targets tells you whether the shape is plausible, not
whether the target is met.

## How to reproduce

```bash
# Transition throughput and ledger projection cost.
go test ./tests/e2e/ -run XXX -bench . -benchtime 200x

# The §21.2 100,000-record ledger profile (takes ~7.5 minutes).
NODES_BENCH_LEDGER_RECORDS=100000 go test ./tests/e2e/ -run XXX \
    -bench BenchmarkLedgerProjection -benchtime 5x -timeout 1800s

# Idle resident memory of `nodes all`.
scripts/idle-rss.sh --settle 30
```

## 1. Transition throughput

`BenchmarkTransitions` (`tests/e2e/bench_test.go`) measures the control
plane's own cost per committed transition: claim one work item
(`SKIP LOCKED` + fencing), run the whole PRD §12.5 completion transaction
(verify fencing → validate output contract → validate ledger delta → record
result → select edge → create the next token and node run → append events and
outbox → commit), against a real PostgreSQL. It drives
`engine.CompleteAttempt` directly rather than through a Worker, because a
worker's wall time is dominated by the *actor's* latency, which is not what
this number is about.

```text
BenchmarkTransitions-20    200    7801569 ns/op    128.2 transitions/sec    27295 B/op    420 allocs/op
```

| Measure | Value |
| --- | --- |
| Per committed transition | **7.80 ms** |
| Sequential throughput | **128 transitions/sec** |
| Allocations | 420 allocs, 27 kB per transition |

**Against §21.2's "sustain 250 committed node transitions per second for ten
minutes":** not comparable as measured, and **not demonstrated**. Two reasons,
both worth being precise about:

1. This is a **single sequential loop** — one claim, one commit, repeat, with
   no concurrency at all. §21.2's 250/s is a *system* figure across
   concurrently claiming workers, and `SKIP LOCKED` claiming is designed to
   scale across them. A concurrent measurement has not been taken.
2. It is dominated by round trips to a containerised PostgreSQL sharing this
   host with a heavily-loaded workload.

What the number does establish: 7.8 ms of database work per transition, and
420 allocations — no per-transition growth proportional to run history, which
is §21.1's "no worker memory growth proportional to total historical runs"
read at the transition level.

## 2. Ledger projection

`BenchmarkLedgerProjection` appends N task records to one run and then
projects `delivery_summary` repeatedly. `delivery_summary` is the most
expensive §10.9 projection — it walks every record, resolves supersession,
and counts on both the execution and assurance axes — so it bounds the rest.

The benchmark **fails** if two projections of the same record set produce
different digests, so determinism is an assertion, not an observation.

```text
BenchmarkLedgerProjection-20    200    12357442 ns/op    432.5 appends/sec    80.92 projections/sec    2000 records
BenchmarkLedgerProjection-20      5  441090792 ns/op    440.2 appends/sec     2.267 projections/sec  100000 records
```

| Corpus | Per projection | Projections/sec | Append rate |
| --- | --- | --- | --- |
| 2,000 records | **12.4 ms** | 80.9 | 433/sec |
| 100,000 records | **441 ms** | 2.27 | 440/sec |

Cost scales close to linearly with corpus size (50× the records, 35.7× the
time), which is what a single-pass projection should do.

**Against §21.2's "append and project 100,000 ledger records with
deterministic projection digests": demonstrated.** 100,000 records were
appended (in ~3 minutes 47 seconds at 440 appends/sec) and projected five
times with an identical digest each time; a digest change would have failed
the benchmark.

The append rate is one record per round trip through
`ledger.Append` — schema validation, authority check, digest computation, and
an `INSERT` under the run's advisory lock. Batching appends is not implemented
and is not needed by anything in Phase 1 (a node's whole delta is small), but
it is where the headroom is if that ever changes.

## 3. Idle memory

`scripts/idle-rss.sh` starts an ephemeral PostgreSQL, migrates it, starts
`nodes all` (API + scheduler + worker in one process), waits for
`/v1alpha1/healthz`, idles for 30 seconds, and reads `VmRSS` from
`/proc/<pid>/status`.

```json
{"mode":"all","settle_seconds":30,"vm_rss_kb":30824,"vm_rss_mib":30.1,
 "vm_hwm_kb":30824,"vm_hwm_mib":30.1,"go":"go1.26.5",
 "kernel":"Linux 6.17.0-1008-nvidia","arch":"aarch64"}
```

| Measure | Value | §21.1 target | Verdict |
| --- | --- | --- | --- |
| `nodes all` idle RSS | **30.1 MiB** | ≤ 128 MiB (all-in-one, excluding PostgreSQL) | **well within** |
| `nodes all` peak RSS since start | 30.1 MiB | — | no startup spike |

The per-role targets (≤ 64 MiB for a worker, ≤ 96 MiB for the API) are not
separately measured: `nodes all` runs all three roles in one process and comes
in at 30 MiB, so each role alone is necessarily smaller. Measuring them
individually would take three more script invocations and tell us something we
can already bound.

Two observations worth recording: `VmHWM` equals `VmRSS`, so nothing
transiently ballooned during startup and migration; and the figure includes
the embedded JSON schemas, the embedded migrations, the pgx pool, and the HTTP
server.

## 4. headspace conformance

PRD §21.2 asks to "run the headspace conformance fixture repeatedly with
identical input, image, policy, and result-envelope structure". This is
asserted rather than timed, by
`tests/e2e/live_test.go`'s `TestPhase1VerticalSliceWithRealHeadspaceRunner`:
the reference workflow's `test` node executes **twice** in one run, through
real headspace-cli and real Docker, from one identical operation document.
The test asserts the two executions agree on:

- the resolved image digest
  (`sha256:57cd7c3a7a273101a6485ba99423ee568157882804b1124b4dd04266317710de`),
- the policy digest
  (`sha256:303e469d66c1afa5301c4f0ca905c76501e90204bacc2a9529bd1a3692cc1e9d`),
- the runner revision, and
- the declared observation set
  (`artifacts_export changed_paths exit_status image_digest job_id logs
  platform_request_id policy_summary resource_usage workspace_cleanup
  workspace_id`),

and that they **disagree** on the job id — each execution is its own
execution, and a runner reporting the same job id twice would be reporting a
cached answer.

Measured wall time for the whole live run (six agent invocations plus **two
full container lifecycles**: create → run → destroy, on a pre-pulled image):

```text
live delivery-loop run finished in 3.57s (two real headspace container executions)
```

That is roughly **1.5 s per container execution end to end**, including
workspace creation and destruction. It is not a §21.2 target — none is
stated for runner latency — but it is the number to watch if code-node
dispatch ever feels slow.

## Phase 2 — 2026-08-09: concurrent runner operations (task t18)

Same host as above, same day, same untuned `postgres:17-alpine` container.
This section measures the asynchronous runner-dispatch path
(`internal/worker/runnerasync.go`, task t9) under concurrent load, and it is
the first section in this file whose numbers come from a *load* harness rather
than a microbenchmark: `tests/load/`.

### The claim under test

Spec requirements **c17/h12**: with many concurrent in-flight runner
operations, the worker's memory stays bounded — no per-operation goroutine and
no per-operation held connection — and the status-sampling load scales with
runners × interval rather than with how long an operation runs.

t9 built the path that way deliberately (park, no goroutine between samples,
claim-is-reschedule). What follows is the measurement, not the assumption.

### What was measured, precisely

The worker runs as its own OS process (`tests/load/testdata/loadworker`), so
every figure below is that process alone — not a test binary that also hosts
the stub service and the connection pool that seeded the runs. Once per loop
iteration, immediately after the iteration's work and outside its timing, the
process records:

- `VmRSS`, `VmHWM` and `Threads` from `/proc/self/status` — real resident
  memory, its high-water mark, and OS threads;
- `runtime.NumGoroutine()`;
- `runtime.MemStats` (`HeapAlloc`, `HeapInuse`, `HeapSys`, `Sys`, `NumGC`);
- the wall time of the `SampleRunnerOperations` pass it just ran, and how many
  operations that pass sampled.

**No GC is ever forced.** `heap_alloc` and `rss_kb` are what the process
actually held, not what it could have been squeezed down to.

Real in this harness: the worker, PostgreSQL, the engine, the compiler, the
runner-protocol client, and a real HTTP hop. Stubbed: the runner service
itself — an in-test HTTP server speaking `api/runner-protocol` (202 +
`Acceptance`, `accepted` → `running` until a configurable completion time,
bearer auth on *every* request including status reads). It is not headspace:
holding a hundred operations in flight for a controlled length of time is a
property of the stub's clock, not of a container runtime.

Every measurement is a **comparison against a control fleet** of 10 operations
run through the same binary on the same host minutes apart. "Goroutines did
not scale with in-flight operations" is a statement about a slope, and a slope
needs two points.

### How to reproduce

```bash
# The 100-operation case and the duration-independence case (~45 s).
go test ./tests/load/ -v -count=1

# The 1,000-operation case, opt-in (~105 s).
NODES_LOAD_1000=1 go test ./tests/load/ -run Thousand -v -count=1 -timeout 30m
```

Both need PostgreSQL: `NODES_TEST_DATABASE_URL`, or Docker able to run
`postgres:17-alpine`. Absent either, the tests skip rather than fail.

### 5. Worker memory at 100 and at 1,000 in-flight operations

The 1,000 case was **run, not extrapolated.**

At a 1-second sampling interval (100-operation fleet, `SampleBatch` 256,
`ClaimBatch` 32, 8-second observation window):

| Measure | 10 in flight | 100 in flight |
| --- | --- | --- |
| Goroutines (median / max) | 6 / 6 | **6 / 6** |
| OS threads (median / max) | 16 / 16 | 16 / 16 |
| RSS median / peak / HWM (KiB) | 25,280 / 25,308 / 25,352 | 26,272 / 26,300 / 26,848 |
| `heap_alloc` median (KiB) | 3,750 | 3,890 |
| Dispatch wall (all parked) | 151 ms | 1.16 s |
| Release wall (all committed) | 406 ms | 2.43 s |

At the runner protocol's own `DefaultPollInterval` of 5 seconds
(1,000-operation fleet, `SampleBatch` 2048, `ClaimBatch` 64, 30-second
observation window):

| Measure | 10 in flight | 1,000 in flight |
| --- | --- | --- |
| Goroutines (median / max) | 6 / 6 | **6 / 6** |
| OS threads (median / max) | 16 / 16 | 19 / 19 |
| RSS median / peak / HWM (KiB) | 26,116 / 26,164 / 26,164 | 27,560 / 28,096 / 28,096 |
| `heap_alloc` median (KiB) | 3,964 | 4,408 |
| Seed / dispatch / release wall | 139 ms / 152 ms / 507 ms | 4.45 s / 9.44 s / 19.24 s |

| Derived | 10 → 100 | 10 → 1,000 |
| --- | --- | --- |
| Goroutine delta | **0** | **0** |
| Marginal RSS per additional in-flight operation | 11.0 KiB | **1.5 KiB** |
| High-water RSS against §21.1's 64 MiB worker budget | 26.2 MiB | **27.4 MiB** |

**Verdict: bounded.** Three things say so, in descending order of how much
they discriminate:

1. **Goroutines do not move at all.** Six at ten operations, six at a hundred,
   six at a thousand. A design that held one goroutine per in-flight operation
   would show 990 more in the last column. This is the assertion that
   distinguishes the two designs, and it is the one `tests/load` fails on if
   the parked path ever grows a per-operation goroutine.
2. **The marginal RSS cost falls as the fleet grows** — 11.0 KiB per operation
   from 10→100, 1.5 KiB per operation from 10→1,000. A genuinely per-operation
   retained structure would hold that slope constant or raise it. What this
   sub-linearity says is that the ~1.4 MiB difference is the allocator's
   working set for one sampling *pass* (bounded by `SampleBatch`, transient),
   not state retained per parked operation. Median `heap_alloc` moving 3,964 →
   4,408 KiB across a hundredfold increase in in-flight work is the same fact
   from the heap's side.
3. **High-water RSS is 27.4 MiB against a 64 MiB budget** with a thousand
   operations in flight — below the 30.1 MiB `nodes all` idle figure in §3,
   though that comparison is loose, since `nodes all` also carries the API and
   the scheduler in the same process.

The fleets are proven real rather than parked-forever artefacts: at every size,
the runner accepted exactly one dispatch per operation (**no re-sends**), the
peak parked count equalled the fleet size, and when the stub released them all
1,000 committed through `engine.CompleteAttempt`, producing exactly 1,000
`attempts` rows — one per operation, no retries.

### 6. Status-sampling load

Sampling load is bracketed between two bounds the design actually promises,
and both were asserted, not eyeballed.

| Fleet | Interval | Measured reads/s | Ceiling (ops/interval) | Effective per-operation period | Sampler duty cycle | Cost per operation-sample |
| --- | --- | --- | --- | --- | --- | --- |
| 10 in flight | 1 s | 8.75 | 10.00 | 1.143 s | 0.021 | 2.43 ms |
| 100 in flight | 1 s | 82.2 | 100.0 | 1.216 s | 0.167 | 2.03 ms |
| 10 in flight | 5 s | 1.87 | 2.00 | 5.357 s | 0.007 | 3.67 ms |
| 1,000 in flight | 5 s | 189.8 | 200.0 | 5.269 s | 0.374 | 1.97 ms |

The **ceiling**, ops/interval, is a contract property rather than a
performance one: claiming a row is what reschedules it (`next_poll_at = now +
interval`, in the same statement that returns it), so no matter how fast the
worker's loop spins, a runner can never be sampled faster than the interval it
was configured with. The measured rate sits *below* the ceiling in every case
and never above it — the protocol document's "sampling faster than a runner
asked for is load it said it did not want", measured.

The gap between measured and ceiling is fully explained and is not slippage:
an operation's next due time is stamped when *its own* status read returns, so
its period is the interval plus its position in the pass plus whatever the
loop was sleeping. All three terms are configuration; none is a function of
the work. At 1,000 operations that shows as a 5.269 s effective period against
a 5 s interval — a 5% overshoot from a 2.1 ms/op pass and a 250 ms loop.

**Derived capacity, not measured:** at ~2.0 ms per operation-sample, one
worker's sampler saturates when in-flight operations × 2.0 ms approaches the
interval — roughly **2,500 operations at the 5-second default**, roughly 500 at
a 1-second interval. The 1,000-operation fleet ran at a 37% duty cycle, which
is consistent with that bound and is the number to watch. Sharding across
workers is unaffected by any of this: `FOR UPDATE SKIP LOCKED` hands disjoint
sets to concurrent samplers.

### 7. Sampling cost is independent of operation duration

A controlled pair. Two fleets identical in every respect — 50 operations, 1-second
interval, same batch, same host, same PostgreSQL — except that one fleet's
operations finish after 30 seconds and the other's after 300. Neither fleet's
operations finish *during* the observation window, so what is compared is the
steady-state cost of waiting on work of two very different lengths.

| Measure | 30 s operations | 300 s operations | Ratio |
| --- | --- | --- | --- |
| Status reads/s | 46.00 | 46.00 | **1.00** |
| Effective per-operation period | 1.087 s | 1.087 s | 1.00 |
| Cost per operation-sample | 2.166 ms | 2.081 ms | 0.96 |
| Sampler duty cycle | 0.0996 | 0.0957 | 0.96 |
| Goroutines (median) | 6 | 6 | 1.00 |
| RSS median (KiB) | 26,048 | 25,796 | 0.99 |

**Verdict: independent.** A tenfold change in operation duration moved the
sampling rate by 0.00% and the per-sample cost by 4% — the latter being
ordinary wall-clock noise on a shared box, in the wrong direction for a
duration effect (the *longer* operations sampled marginally *cheaper*). A
design whose cost tracked duration would have to show a roughly tenfold
difference somewhere in this table. The test bounds the observed ratios at a
factor of two, which is loose for wall-clock work and still an order of
magnitude away from what a duration-dependent design would produce.

### What this section does not say

- These are **single runs**, not distributions. Nothing here is averaged over
  repeated executions, and the host was shared and loaded throughout.
- The runner service is a **stub**. Per-sample cost includes a real HTTP round
  trip and a real `UPDATE`, but to a localhost server with no work to do; a
  runner across a network adds latency to the *pass*, which raises the duty
  cycle and lowers the capacity bound derived above.
- The capacity figure (~2,500 operations per worker at the default interval)
  is **derived arithmetic from the per-sample cost**, not a fleet that was run.
- Only the **runner** async path is measured. The actor async path
  (`internal/actors`, callback-driven) parks the same way and is expected to
  behave the same way, but was not loaded.
- Nothing here is gated. As with the rest of this file, no CI job fails on
  these numbers.

## What is not measured here

Named so the absence is deliberate rather than assumed:

- **Concurrent transition throughput** (§21.2's real 250/s target) — only the
  sequential figure exists.
- **10,000 non-terminal runs stored** — not attempted.
- **p95 ready-to-claim latency** — not instrumented; there is no telemetry
  layer to measure it with (ADR 0005, D3).
- **Killed-worker recovery within lease expiry + 5 s** — the *behaviour* is
  proven by `tests/fault/claiming_fault_test.go`'s `kill -9` test, which
  asserts a deadline of lease expiry + 5 s; the *timing distribution* is not
  recorded.
- **1,000 concurrent waiting external invocations without one goroutine per
  wait** — measured for the *runner* half in §5 above (1,000 parked runner
  operations, goroutine delta zero). The *actor* half is still design-only: a
  parked actor invocation holds no goroutine either, and
  `TestWorkerParksAsyncInvocationAndCompletesFromCallback` proves the item is
  released, but that path has not been loaded.
- **UI performance on a 500-node graph** (§21.4) — not measured.
