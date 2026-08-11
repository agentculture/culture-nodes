# Delivery Summary — engine-hardening-dispatch-safety

plan: `engine-hardening-dispatch-safety` · run: `complete` · date: `2026-08-12`
baseline: `devague summary skeleton`

## Intent

Make actor dispatch safe to leave unattended (issues #16, #19): terminal-commit
failures visible in events and logs, a failed commit never permanently consuming
the callback sequence, a bounded dispatch retry budget parking exhausted nodes
as failed with a recorded cause, and run cancellation that reaps waiting/leased
work items and propagates Cancel to in-flight sessions. Executed as a 5-task,
2-wave `/assign-to-workforce` run (4 parallel wave-0 agents, wave-1 operator
acceptance on the thor+orin production pair), after `/scope`, `/think`, and a
rigorous `/challenge` pass.

## Planned Work

Quoted verbatim from the `devague summary` skeleton:

- `t1` — Ratchet fix + terminal-commit failure event (internal/actors/callback.go + store async.go)
- `t2` — API logging facility + 5xx visibility (internal/api server/middleware only — do NOT touch runs.go)
- `t3` — Retry budget 3 + terminal-run lease guard + exhaustion Cancel (store claiming.go + worker)
- `t4` — Cancel reap + propagation (internal/api/runs.go + both prod compose files — do NOT touch server/middleware)
- `t5` — Live acceptance on thor+orin (ops): forced-failure park, live cancel SIGTERM, log visibility, pre-fix red proof

## Actual Delivery

| Plan task | Status | What actually landed |
|-----------|--------|----------------------|
| `t1` | delivered | Advance-then-conditional-rollback compensation (approach recorded in commit `67a8128`): failed terminal commit rolls back the sequence mark, reparks the resumed item (`ReparkResumedWork` — discovered necessary beyond the brief, killing the lease-expiry loop motor), records `TypeCallbackCommitFailed`; incident-1 regression red-then-green at pgtest level |
| `t2` | delivered | slog through the `writeAPIError` funnel (every 5xx logs its chain) + a narrow callback-route wrapper logging terminal-commit failures with attempt id; four handler tests |
| `t3` | delivered | Budget `MaxDispatchAttempts = 3` enforced worker-side on claimed work (parking flows through CompleteAttempt as `failed` with the exhaustion cause), terminal-run guard in claim+reclaim SQL, exhaustion Cancel, waiting-accrues-no-attempts proof; incident loop reproduced red (tests timed out on unbounded re-dispatch) then bounded green |
| `t4` | delivered | cancelRun reaps `ready`+`waiting`+`leased` under the existing advisory lock; post-commit best-effort `actors.Client.Cancel` per pending invocation with one recorded `cancel-requested` event each; thor api service carries actor tokens; orin has no api service (guard test pins that instead — task-level adaptation, architecture-correct) |
| `t5` | delivered | Live on thor+orin: cancel run `01KZSCKN7MR5D6YS044EJRG5JD` exposed keep-alive starvation (fixed, see below); final proof run `01KZSC...` (third session): cancel API answered in 0.031s, the bridge access-logged the api container's cancel POST 202 mid-session, propagation event `sent`, codex process dead, exactly one dispatch — no loop; api ERROR log lines captured live (h18) |

## Mid-work Decisions

- `d1` — the dispatch budget bounds one WORK ITEM's re-lease cycle at 3, not one node run's total dispatches: an engine-level workflow retry policy enqueues a fresh work item with a fresh budget, so a node run's total dispatches are bounded by 3 x its declared maxAttempts — both finite, composing deliberately (task t3, approved) [acceptable]
- t1 discovered the ratchet fix alone was insufficient: the failed commit also leaks the resumed lease, so `ReparkResumedWork` (and reparking on §13.4 refusals) landed as required-by-h3 scope, documented in the commit.
- Colleague review (graded 5/5) found swallowed compensation errors and unlogged propagation — both fixed pre-PR; its unconfigurable-budget and lock-breadth notes are recorded as future work.
- Live t5 found the cancel POST never reached the bridge: Go client keep-alive + the bridge's single-threaded HTTPServer starved the accept backlog. Engine-side fix shipped (`DisableKeepAlives` on the actors client + 30s propagation timeout); bridge threading filed as #21. Two of the three authorized billable sessions ran to natural completion because their cancels could not land — reported as spend, not smoothed over.
- The forced-failure budget scenario was proven at pgtest level (the red tests reproduce the unbounded loop exactly) rather than live: post-fix production has no reachable deterministic-commit-failure path to force cheaply, and the terminal-run guard had no dangerous rows left to demonstrate on — the live lane instead proved the cancel/reap/propagation/logging half end-to-end.

## Drift From Plan

| Plan item | Reason for divergence | Classification |
|-----------|-----------------------|----------------|
| `t3` (`d1`) | the dispatch budget bounds one WORK ITEM's re-lease cycle at 3, not one node run's total dispatches: an engine-level workflow retry policy enqueues a fresh work item with a fresh budget, so a node run's total dispatches are bounded by 3 x its declared maxAttempts — both finite, composing deliberately | acceptable |
| `t4` | orin's compose has no api service (control plane lives on thor) — token envs landed on thor's api only, with a guard test that fails if an api service ever appears on orin | acceptable |
| `t5` | h15's "run the four regression tests against a pre-fix checkout at review time" is evidenced by the agents' recorded red-phase outputs (staged declarations-only pre-fix runs) rather than a literal post-hoc pre-fix checkout — the new tests reference symbols that do not compile pre-fix | acceptable |
| `t1` | scope grew by `ReparkResumedWork` + refusal-path reparking — required to satisfy h3 against the real code, not optional hardening | acceptable |

## Evidence

- tests: `go test ./...` — all packages ok (incl. new pgtest suites in internal/actors, internal/store/postgres, internal/worker, internal/api); `uv run pytest -n auto` — 112 passed
- red-phase evidence: t1's `TestCallbackTerminalCommitFailureIsRecoverableOnRedelivery` pre-fix output (four incident facets failing verbatim) and t3's budget tests timing out on unbounded re-dispatch — quoted in the agents' recorded reports
- commits: 16 on `spec/engine-hardening-dispatch-safety` (`b7d3a6e..7b29133`): spec, challenge amendments, plan, 4 task merges, interface reconcile, review fixes, keep-alive fix, timeout widening, version bump 0.10.0
- production runs: `01KZSCKN7MR5D6YS044EJRG5JD` (cancel timed out — exposed keep-alive starvation), final proof run (cancel landed: bridge 202 on `/v1/invocations/{id}/cancel` from 172.21.0.2, event `sent`, session dead, 0.031s response)
- issues: #21 filed + root-caused; colleague review artifact `.colleague/8d37051942d0...json`, rated 5
- deployed live to thor+orin (three deploys during acceptance); healthz + all four systemd units active

## Delivery Claims

| Claim | Confidence | Evidence |
|-------|------------|----------|
| A failed terminal commit is recoverable on redelivery and leaves a recorded failure event | high | `TestCallbackTerminalCommitFailureIsRecoverableOnRedelivery` (red-then-green) · commit `67a8128` |
| Actor dispatch is bounded: 3 attempts per work item, then parked failed with cause, Cancel issued | high | t3 test suite (pre-fix timeout reproduction) · d1 record |
| No work item of a terminal run can be leased or reclaimed | high | `TestClaimWorkRefusesItemsOfTerminalRuns` / `TestReclaimExpiredRefusesItemsOfTerminalRuns` |
| Run cancel reaps all items and lands SIGTERM on in-flight sessions within 0.1s | high | live run evidence: bridge 202 cancel log · event `sent` · dead session · 0.031s cancel response |
| Every API 5xx and terminal-commit failure is visible in logs | high | live prod-api-1 ERROR lines captured during t5 · t2 handler tests |
| The budget-exhaustion path parks correctly in production | medium | pgtest-proven against a real engine+store; not forced live (no reachable deterministic failure remains post-fix) |

## Remaining Work / Follow-up

- #21 — bridge-side ThreadingHTTPServer (all three adapters) + profiling the 2.2s idle cancel path; adapter-cycle scope.
- Colleague review deferrals: `MaxDispatchAttempts` operator configurability; cancelRun advisory-lock breadth; `:-` compose defaults meaning silent missing tokens (a doctor-style preflight would catch).
- t3's noted follow-up: exhaustion Cancel does not close the `actor_invocations` row (`waiting_external` lingers in the hot-set view) — cosmetic, decision pending.
- Cancel propagation runs synchronously inside the cancel handler (worst case = timeout × invocations before the response returns); consider moving to the outbox if cancel latency ever matters.
- The v1-parked 180s-timeout mystery from the codex cycle remains parked — now diagnosable with the new logging when it next appears.
