# ADR 0013: the reference bridges stay single-threaded, and #21's cancel latency is not the server class

- Status: accepted
- Date: 2026-08-15
- Task: t12 (close-the-backlog plan,
  `docs/plans/2026-08-15-close-the-backlog.md`)
- Issues: #21 (single-threaded HTTPServer makes the §13.6 cancel path slow),
  #10 (bridge production follow-ups)

## Context

Issue #21 reports, from the 2026-08-12 engine-hardening live acceptance:

> the api's cancel propagation timed out against thor's codex-bridge — the
> bridge never even access-logged the cancel POST. Direct probes: an idle
> `POST /v1/invocations/{id}/cancel` answers 202 in ~2.2s; mid-session it
> exceeded the engine's (then) 10s propagation budget.

and attributes it to the deliberate `http.server.HTTPServer` choice, with
`ThreadingHTTPServer` as the leading candidate fix. Every bridge under
`adapters/` makes that choice on purpose and says so in its module
docstring: a reference adapter proving the actor protocol against a real
backend, not a production-scale actor host.

This ADR records the decision to keep it, because the issue's own remedy
does not match what the cancel path actually costs.

## Measurements

All from `adapters/codex/scripts/probe_cancel_latency.py`, committed beside
the bridge it measures, run on spark (arm64) on branch `ctb/k1` at commit
`90914fc` — through the real HTTP surface on loopback with
`server.start_background`, no real `codex` binary and no network:

| What was measured | Result |
|---|---|
| Idle `POST /…/cancel`, fresh connection, n=20 | **median 0.26 ms**, max 1.60 ms |
| `POST /v1/invocations` (async dispatch, real subprocess spawned) | **4 ms** to 202 |
| `POST /…/cancel` MID-SESSION against that live async invocation, n=10 | **median 0.23 ms**, max 1.02 ms |
| `POST /…/cancel` arriving while an unrelated **synchronous** invocation is 0.2 s into a 3.0 s provider call | **2801 ms** |

Three things follow, and they point away from the server class.

1. **The cancel handler costs nothing.** It is one `hmac.compare_digest`, one
   dict lookup under a lock, and — for codex — a `SIGTERM` to a process it
   owns. Sub-millisecond, idle or mid-session. The reported ~2.2 s idle
   figure was measured across the network against thor and cannot have been
   spent in this handler; it is transport, host, or connection setup, and
   nothing about `ThreadingHTTPServer` would touch it. **Whoever next
   re-measures #21 should isolate the transport before changing the server
   class**, because the local handler measurement leaves no room for 2.2 s.

2. **The serialization is real, and it is not on the path a cancel takes.**
   The 2801 ms row is exactly the single-thread claim, quantified: a cancel
   arriving behind an in-flight *synchronous* invocation waits out the
   remainder of that provider call. But the sessions a cancel targets are the
   long ones, and long work dispatches **asynchronously** — `decide_async`
   sends anything above `sync_max_steps` (default 6) to `AsyncRunner`, and
   `always_async` is set in production. An async dispatch frees the server in
   4 ms, and cancelling that session mid-flight measures 0.23 ms. So the
   session being cancelled never blocks its own cancel. Only an unrelated,
   concurrent, synchronous invocation can, on a bridge process that is also
   being used that way.

3. **Threading would remove the only bound on concurrent provider
   subprocesses.** The per-invocation state is already thread-safe — both
   `IdempotencyStore` and `SessionRegistry` hold a `threading.Lock` and are
   documented as safe for one process's concurrent threads, because the async
   poller threads already share them — so that is *not* the objection. The
   objection is capacity: the single-threaded accept loop is today the de
   facto limit on how many synchronous `codex exec` / `claude` / `colleague`
   processes one bridge host will run at once. `SessionRegistry` bounds
   in-flight work per `session_key`, not in aggregate. Swapping in
   `ThreadingHTTPServer` would replace a documented one-at-a-time property
   with thread-per-connection and no ceiling, on hosts (thor, orin) that are
   also running the worker, the runner and the model sessions themselves.

## Decision

1. **Keep `HTTPServer`.** The choice stays deliberate and stays documented in
   each bridge's module docstring, which already names the two supported ways
   to serve concurrent synchronous invocations: run several bridge processes,
   or fork the file into a threaded server for a deployment that wants one.
2. **Do not adopt `ThreadingHTTPServer` on the strength of #21's numbers.**
   They do not localize to the server class, and the one case they do
   localize to — a cancel queued behind a concurrent synchronous dispatch —
   is not the case the incident hit.
3. **The engine's 30 s propagation budget stands as the right layer for
   this.** PRD §13.6 makes cancellation durable in Culture Nodes and
   best-effort at the actor; as #21's own propagation design note records,
   a bridge that answers late is contained by design — the run still
   cancels, items still reap, and the failed propagation is recorded as a
   `cancel-requested outcome=failed` event.
4. **What would reopen this**: a measurement showing multi-second latency in
   the bridge process itself — an access log timestamp, or a probe from the
   bridge host's own loopback rather than across the network. That is a
   different finding from the one #21 records, and it should be filed with
   the transport isolated.

## What #21 got right and is worth keeping

The issue's third bullet — "the cancel handler should respond after
`terminate()` is *issued*, never after the process exits" — is already how
all three bridges behave, and it is worth keeping as an explicit invariant
rather than an accident. It is the reason the mid-session number above is
0.23 ms and not the length of a session. The two shapes differ and both
answer immediately: codex owns its `Popen` and `terminate()`s it inside
`cancel()`; claude-code and colleague dispatch DETACHED sessions, so
`cancel()` writes a `flightfiles` stop request and the poller turns it into
a real signal within one `poll_interval_seconds`. Neither waits for the
process to exit before answering 202.

## Consequences

- #21 closes as a design decision recorded here, not as an unfixed bug. The
  boundary it identified is real and now has numbers attached to it.
- A deployment that genuinely needs concurrent synchronous invocations on one
  host has a documented answer (more processes) rather than an implicit one.
- If the fleet later serves synchronous dispatches at concurrency, this ADR
  should be revisited *with a bound* — a threaded server plus an explicit
  in-flight-provider-call semaphore — never threading alone.

## Reproducing the measurements

```bash
cd adapters/codex && uv run python scripts/probe_cancel_latency.py
```

The script points `Config.codex_bin` at a fake executable that emits two
JSONL records and then sleeps until signalled, so a REAL subprocess is alive
while the mid-session cancel is timed; the serialization row monkeypatches
`codex_cli.run_sync` with a 3-second sleep to occupy the accept loop. The run
quoted above:

```text
idle cancel  n=20  median=0.26ms  max=1.60ms
async dispatch 202 in 4ms (status 202)
cancel MID-SESSION (async, real subprocess alive) n=10 median=0.23ms max=1.02ms
cancel during a 3.0s sync invocation: 2801ms (status 202)
```

Run it on thor before changing anything: if the bridge-side numbers there
look like these, #21's ~2.2 s is in the transport and this ADR stands. If
they do not, that is the new finding, and it belongs in a new issue with the
host's own loopback measurement attached.
