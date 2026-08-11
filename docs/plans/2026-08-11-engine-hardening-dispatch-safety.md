# Build Plan — engine-hardening-dispatch-safety

slug: `engine-hardening-dispatch-safety` · status: `exported` · from frame: `engine-hardening-dispatch-safety`

> Actor dispatch is safe to leave unattended: terminal-commit failures are visible in events and logs, a failed commit never permanently consumes the callback sequence, actor re-dispatch has a bounded retry budget that parks the node as failed with a recorded cause, and run cancellation reaps waiting and leased work items so a cancelled run can never dispatch again

## Tasks

### t1 — Ratchet fix + terminal-commit failure event (internal/actors/callback.go + store async.go)

- instruction: Files: internal/actors/callback.go, internal/store/postgres/async.go (+ their test files). Study handleClaimed's step-4 ordering and callback.go's error-release path first; choose advance-after-terminal-processing OR accept-equal-sequence-for-released-claim (both contract-safe; record the choice + why in the commit message per plan risk r3). The pgtest harness auto-starts docker postgres (internal/store/postgres/`testmain_test.go`); namespace-scope every new fixture (issue #9). Red-first: reproduce incident 1 exactly (see docs/deliveries/2026-08-11-codex-bridges-thor-orin.md evidence + issue #16).
- covers: c3, h3, c7, h8
- acceptance:
  - A pgtest regression reproduces incident 1 red-first: terminal commit fails once (injected store error), the same-id/same-sequence redelivery is today rejected out-of-order — and commits successfully after the fix (h3); the test asserts the work-item state sequence (waiting -> resumed-leased -> reclaim loop) pre-fix per c16
  - commitTerminal's error paths record a failure event on the attempt aggregate via deps.record naming the error class before returning (the event half of c2)
  - adapters/\*\* and tests/conformance are byte-unchanged; conformance kit passes against an unmodified bridge (h8)

### t2 — API logging facility + 5xx visibility (internal/api server/middleware only — do NOT touch runs.go)

- instruction: Files: internal/api/server.go + middleware/new files under internal/api; FORBIDDEN: internal/api/runs.go (t4 owns it this wave). Introduce log/slog with a Server-level logger (constructor option, default slog.Default), a response-writing wrapper that logs every 5xx with the unwrapped error chain, and explicit terminal-commit failure logging in the actor callback ingest handler. Handler test: inject a failing engine, capture slog output, assert event+line.
- covers: c2, h2, c18, h18
- acceptance:
  - internal/api gains a structured logger (log/slog) wired through Server; every 5xx response path logs the error chain; the callback ingest path logs terminal-commit failures with attempt id (h2's log half, h18)
  - A handler test captures log output and asserts both the failure event and the log line exist for a failed terminal commit (h2)

### t3 — Retry budget 3 + terminal-run lease guard + exhaustion Cancel (store claiming.go + worker)

- instruction: Files: internal/store/postgres/claiming.go (+test), internal/worker dispatch/complete (+test). Budget constant 3 (name it, document the q1 decision). Terminal-run guard: join runs.status in claimWorkSQL AND reclaimExpiredSQL. Parking: when a claim would be attempt 4, complete via the engine path with status failed and the exhaustion cause in the result JSON. Exhaustion Cancel: reuse the worker's actors client. Waiting-accrues-nothing test per c20.
- covers: c4, h4, c6, h6, c8, h9, c19, h19, c14, h14
- acceptance:
  - The 4th dispatch attempt of a node run never happens: claim/park logic completes the item as technical status failed with the exhaustion cause in the attempt result, flowing through the existing CompleteAttempt vocabulary — no new UPDATE/DELETE on actors/`ledger_records`/attempts (h4, h9)
  - Claim and reclaim SQL refuse items whose run status is terminal, covered by pgtest for cancelled AND failed runs (h6)
  - Budget exhaustion issues a best-effort actors.Client.Cancel for the pending invocation, asserted by test (h19); a store test proves a 'waiting' item's attempt counter is unchanged across ReclaimExpired sweeps (c20's honesty)
  - The budget applies to actor dispatch generically, not codex-specifically (h14)

### t4 — Cancel reap + propagation (internal/api/runs.go + both prod compose files — do NOT touch server/middleware)

- instruction: Files: internal/api/runs.go (+test), deploy/prod/compose.thor.yml, deploy/prod/compose.orin.yml; FORBIDDEN: internal/api/server.go and middleware (t2 owns them this wave). Extend cancelRun's `work_items` UPDATE to states ('ready','waiting','leased'); after tx commit, resolve pending invocations (`actor_invocations` by run), build endpoints via worker.DBRegistry (importable), call actors Client.Cancel best-effort, record one cancel-request event each; failures log, never fail the cancel. Compose: add the three `NODES_ACTOR_`\* env lines to the api service env blocks mirroring the worker's.
- covers: c5, h5, c17, h17
- acceptance:
  - cancelRun's `work_items` UPDATE extends to waiting and leased rows under the existing run advisory lock; after commit, zero items of the run are leasable (h5)
  - Post-commit, cancelRun best-effort invokes actors.Client.Cancel per pending invocation via the DBRegistry resolution the worker uses, recording a cancel-request event per invocation (h5); compose.thor.yml and compose.orin.yml api services gain the `NODES_ACTOR_`\* token envs (c17)

### t5 — Live acceptance on thor+orin (ops): forced-failure park, live cancel SIGTERM, log visibility, pre-fix red proof

- instruction: Ops task (main agent): deploy branch to thor+orin (deploy.sh), run the forced-failure scenario in a scratch namespace against a deliberately unregistered actor key, capture UI/events/logs evidence, run the live cancel-SIGTERM check against a real (billable, single) codex session with user awareness, verify pre-fix red by running the four named regression tests on the parent commit, and record all evidence for the delivery summary.
- depends on: t1, t2, t3, t4
- covers: c1, h1, c9, h7, c11, h11, c12, h12, c13, h13, c15, h15
- acceptance:
  - Deployed to both hosts; a forced deterministic failure (unregistered actor id in a scratch namespace or equivalent) parks at attempt 3 with the cause visible in UI/events — no loop (h1, h13, h15's scenarios 2 and 4 live)
  - A live cancel of a run with an in-flight codex session shows the bridge logging the cancel and the session terminating early (h17); prod-api-1 logs carry the failure lines during the check (h18 live); the four success-signal regression tests are shown failing on a pre-fix checkout (h15)
  - git diff proves internal/queue/sqs is byte-unchanged (h7); the before-state citations hold (h12); each after-state clause maps to a named test (h13); failure surfaces land where each audience looks (h11)

## Risks

- [unknown_nonblocking] store-level regression tests need the pgtest harness (ephemeral docker postgres) — slower than unit suites and subject to issue #9's namespace-contamination if written carelessly; new suites must be namespace-scoped
- [unknown_nonblocking] t2 and t4 both live in internal/api (different files: server/middleware vs runs.go) — same-wave merge risk is low but real; their briefs forbid touching each other's files
- [unknown_nonblocking] the ratchet fix choice (advance-after-processing vs accept-equal-for-released-id) is left to t1's agent against the real code; both are contract-safe per the spec, but the choice must be recorded in the commit message
