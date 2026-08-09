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
  wait** — the design property holds by construction (a parked invocation
  holds no goroutine at all; `TestWorkerParksAsyncInvocationAndCompletesFromCallback`
  proves the item is released), but the 1,000-wait load has not been run.
- **UI performance on a 500-node graph** (§21.4) — not measured.
