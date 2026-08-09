# Delivery Summary — self-hosted phase-2 cycle

plan: `self-hosted-phase-2-cycle` · run: `complete` · date: `2026-08-09`
baseline: `devague summary skeleton`

## Intent

Execute the converged 26-task, 8-wave plan that closes the phase-1
remainders (the deferred approval surface above all), makes claude-code a
first-class production actor, replaces in-worker code execution with a
placement-unaware runner protocol, grows the operations web surface (board,
jobs timeline, time filter), deploys the thor+orin production pair on one
shared Postgres, and proves it all by having culture-nodes drive its own
development — a real dev task executed as a production run with a human
ship decision.

## Planned Work

Quoted verbatim from the `devague summary` skeleton:

- `t1` — Cycle baseline: re-run all suites on main and record results
- `t2` — `waiting_external` deadline timers fail attempts
- `t3` — run.output end-node binding fix
- `t4` — Migration 0010: runs.`updated_at` index + node-runs listing index
- `t5` — Runner-service wire contract + runner-neutral registry identity
- `t6` — Engine: approval nodes create `human_tasks` and pause leaselessly
- `t7` — Human-tasks API: list/get/decision endpoints
- `t8` — Worker: register a real HumanDispatcher
- `t9` — Worker: async runner dispatch — park/resume + idempotent completion ingest
- `t10` — Reference runner service: headspace wrapped behind the contract, with auth
- `t11` — API: run-list time params + cross-run node-runs endpoint
- `t12` — claude-code actor adapter (adapters/claude-code)
- `t13` — codex actor adapter (adapters/codex)
- `t14` — Web: runs board — cards on state columns
- `t15` — Web: jobs timeline table + time-range filter
- `t16` — Python CLI + parity harness for every new surface
- `t17` — Acceptance not-mets: Markdown projection rendering + mechanical acceptance.requires
- `t18` — Load test: 100 concurrent runner operations, bounded worker
- `t19` — Production setup: thor+orin compose profiles + credential authorize flow
- `t20` — thor Postgres backup job + restore drill
- `t21` — e2e: the human-review branch
- `t22` — Placement-free proof: same digest on two machines
- `t23` — Self-hosted run: culture-nodes develops itself on the production pair
- `t24` — Docs + README: OSS standup path, runner protocol, operations views
- `t25` — Live-testing evidence: screenshots of the operations views against the production run
- `t26` — Integration gate: version bump, lanes check, delivery summary before PR

## Actual Delivery

All 26 tasks delivered; `t8` and `t19` delivered under recorded deviations.

| Plan task | Status | What actually landed |
|-----------|--------|----------------------|
| `t1` | delivered | all suites green on the branch point; `docs/baselines/2026-08-09-phase2-baseline.md`; Go toolchain found absent on dev and installed user-space (recorded friction) |
| `t2` | delivered | scheduler fails `waiting_external` attempts at deadline and routes `timed_out` edges; verified failing-without-fix |
| `t3` | delivered | root cause was a park-vs-callback race (fast 202→callback beat the invocation row commit, permanent 404); bounded lookup retry + full-HTTP async e2e regression |
| `t4` | delivered | migration 0010: `(namespace_id, updated_at)` on runs and node_runs; EXPLAIN-proven index scans; N-1 harness extended + manual old-binary simulation |
| `t5` | delivered | `api/runner-protocol/README.md` (async-only, 202+status sampling, resultless optional header callbacks, mandatory auth); `ServiceIdentity` registry form beside the ARN form; doc-pinned-to-code tests |
| `t6` | delivered | approval dispatch writes `human_tasks` (§9.9 payload) and inserts node runs as `waiting_human` — no work item ever exists; leaseless pause proven by advisory-lock test |
| `t7` | delivered | GET/GET/POST human-tasks endpoints; decisions commit through the review transaction as genuine human authority; resume-once via status guard + SQL WHERE (concurrency test) |
| `t8` | delivered (`d1`) | re-scoped: no-work-item invariant proven both directions; HumanDispatcher kept as documented refuse-loudly guard; worker docs made honest |
| `t9` | delivered | `runner_invocations` (0011), claim-is-reschedule sampler, fencing-tuple-only completion dedup, deadline reuse, SIGKILL fault test proven on polling alone |
| `t10` | delivered | `internal/runners/runnerservice` + `cmd/nodes-runner`: fsynced FileStore status survives kill-9 byte-identically, auth-before-existence, runner conformance kit PASS live vs Docker headspace |
| `t11` | delivered | `updated_since`/`updated_until`/`sort` on GET /runs; GET /v1alpha1/node-runs with keyset `(updated_at, id)` cursor; EXPLAIN evidence |
| `t12` | delivered | adapters/claude-code: 127 tests, conformance kit exit 0, min-version gate 2.1.220, incomplete-never-success at three layers |
| `t13` | delivered | adapters/codex: 103+5 tests, unmodified kit PASS against the real authenticated codex; SIGTERM-exit-0-no-terminal-event measured and refused as success |
| `t14` | delivered | /board — cards on the real state-enum columns, culture-design tokens only, 36 unit + 9 e2e tests |
| `t15` | delivered | /jobs — cross-run table, server-side time filter in URL params, cursor load-more; 145 vitest / 32 Playwright total after merge |
| `t16` | delivered | human-tasks + node-runs CLI verbs (token never logged), run-list time flags; parity harness now three-front and drift-catching (verified by deliberate break) |
| `t17` | delivered | Markdown rendered deterministically from projections incl. live `?format=markdown` (met); mechanical `acceptance.requires` for 2 of 9 kinds with honest refusal for the rest (partial, said so); acceptance sheet 42 met / 6 partial / 0 not met |
| `t18` | delivered | measured, not assumed: 6 goroutines flat at 10/100/1000 in-flight; HWM 27.4 MiB vs 64 MiB budget; 1000 case run (104 s); duration-independence ratio 1.00; `tests/load` + benchmarks section |
| `t19` | delivered (`d2`) | compose.thor/compose.orin profiles, argv-only `deploy.sh` (git-archive ship, native build), ssh-stdin secret install, runner as systemd user unit on both machines; both worker ids (`thor-worker-1`, `orin-worker-1`) recorded against thor's single Postgres |
| `t20` | delivered | scheduled pg_dump service + `RESTORE.md`; drill ran for real: production dump restored on the dev machine, all ledger record ids + content digests identical (first comparison attempt was vacuous and was redone) |
| `t21` | delivered | approval node restored in examples/delivery-loop (d1 lifted, inverted assertion); both decision edges walked over the real HTTP API; leaseless pause re-proven; 7/7 e2e incl. live headspace |
| `t22` | delivered | same digest `sha256:9bf42e9a…` executed on spark's runner (`job-bed88c03fa62`) then thor's (`job-ea6b464d0450`) with zero workflow changes — four worker cmd env gaps surfaced and fixed en route |
| `t23` | delivered | run `01KZJYNC884FJHZ46XA4TW0MMF` (self-hosting-loop, digest `sha256:b24033c9…`) completed on the production pair: 4 claude actor sessions, headspace code node, ship-review approved by the human through POST /human-tasks/{id}/decision; the claude-authored docs merged as the run's diff |
| `t24` | delivered | quickstart verified verbatim in a clean environment (twice); runner/actor extension points, adapters, topology documented; template-drift strings closed; actor-protocol README filled in |
| `t25` | delivered | `docs/assets/phase2-{board,jobs-timeline,selfhosted-run,selfhosted-ledger}.png` captured against thor's live UI showing the completed run |
| `t26` | delivered | version 0.8.0 + CHANGELOG; vendored skills byte-identical to main; friction filed as issues #8/#9/#10; this summary precedes the PR per the confirmed order |

## Mid-work Decisions

- `d1` — t8's planned acceptance assumed approval work items reach the worker
  and route through a registered HumanDispatcher; t6 shipped engine-side
  parking instead — approval nodes insert node_runs directly as
  `waiting_human` and never enqueue a work item. t8 re-scoped to prove the
  no-work-item invariant and keep the seam as an honest refuse-loudly guard.
  (Recorded via `/deviate`, proposed — owner confirmation pending at this PR
  gate.)
- `d2` — t19's runner-service placement: deployed as a host process under a
  systemd user unit on both machines instead of a compose service, because
  the code-execution boundary needs the host's Docker and headspace toolchain
  while the control-plane compose stack stays deliberately socketless.
  (Recorded via `/deviate`, proposed.)
- Four worker cmd env gaps (`NODES_RUNNER_SERVICES_FILE`,
  `NODES_CODE_RUNNER_NAME`, `_REVISION`, `_ACTOR_ID`) were discovered one
  live failure at a time during t22 and wired between waves by the operator —
  no task owned the cmd surface for t9's library work (issue #8).
- The self-hosted run's first attempt (`01KZJY39XJ…`) failed honestly twice:
  the reference delivery-loop's input shape doesn't satisfy the claude
  bridge's contract-v1 (`{instruction, repo}`), producing
  `examples/self-hosting-loop` with per-role bindings and the human approval
  moved to the happy path (the bridge maps success to one declared outcome,
  so ship-vs-rework belongs to the human reviewing the real diff); and the
  bridge's sync default timed out the real developer session — rerun with
  `ALWAYS_ASYNC=1` (issue #10).
- The claude developer session edited the docs but could not run git
  (permission mode); the approved diff was committed by the operator after
  the ship-review decision, attributed in the commit message (issue #10).
- The approval-outcome vocabulary is fixed by the compiler; the variant uses
  `rejected` as its "changes required" loop edge, documented in the workflow.
- MinIO/S3 test flakes and a shared-DB queue-test contamination were
  verified pre-existing/unrelated by two independent task agents (issue #9).

## Drift From Plan

| Plan item | Reason for divergence | Classification |
|-----------|-----------------------|----------------|
| `t8` (`d1`) | t6 shipped engine-side parking — approval work items never reach the worker, so the planned dispatcher registration would have been stub-routing work that cannot exist | acceptable |
| `t19` (`d2`) | runner runs as a host systemd unit, not a compose service — docker.sock + headspace in a container would breach the socketless control-plane rule the repo enforces by test | acceptable |
| `t23` | delivered via `examples/self-hosting-loop` rather than the reference delivery-loop: the bridge's input contract and single-success-outcome mapping required per-role bindings and the human gate on the happy path; the reference workflow stays untouched | acceptable |
| `t17` | mechanical `acceptance.requires` is honestly partial (2 of 9 kinds evaluable); remainder refuse loudly rather than fabricate | needs-follow-up |

## Evidence

- run `01KZJYNC884FJHZ46XA4TW0MMF` — completed on thor+orin; ledger holds 8
  records: 4 agent claims `proposed`, runner evidence `observed`, validator
  review `derived`, human decision `proposed` + exactly one human review
  `confirmed` (the §10.4 authority matrix, live)
- placement runs `01KZJX4FFDTW9SV4BWP1GXWN9F` (spark) and
  `01KZJX68AW42NSQBHX3G79GC0F` (thor), same digest `sha256:9bf42e9a…`
- restore drill: 6 ledger records byte-identical (ids + content digests)
  after production-dump restore on the dev machine
- tests (pre-gate, full tree on the branch): `go test ./...` 33 packages ok
  (incl. e2e, fault, load, runnerconformance, live headspace); pytest 112;
  adapters 100/127/108; web vitest 145, Playwright 32
- lint: gofmt/go vet clean; black/isort/flake8/bandit clean; markdownlint 0
  errors; `teken cli doctor . --strict` 26/26; vendored `.claude/skills/`
  byte-identical to main
- benchmarks: `docs/benchmarks.md` phase-2 section (6 goroutines flat,
  27.4 MiB HWM, 1000-op case run)
- commits: `f5a2b26..ae86e1c` on `phase2/build` (58 commits: 20 task merges,
  operator wiring, deploy profiles, evidence)
- screenshots: `docs/assets/phase2-*.png` (live production UI)
- issues: #3 closed by this cycle's approval surface; #5 #6 #7 parked lanes;
  #8 #9 #10 filed friction
- deviations: `d1`, `d2` — recorded, proposed, awaiting owner confirmation

## Delivery Claims

| Claim | Confidence | Evidence |
|-------|------------|----------|
| The approval surface exists end to end and a human decision is the only path to `confirmed` authority | high | run `01KZJYNC884FJHZ46XA4TW0MMF` ledger; `tests/e2e/humanreview_test.go` |
| Code executes placement-unaware through the runner protocol; moving a node between machines is registry config only | high | runs `01KZJX4FFD…`/`01KZJX68AW…`, same digest, zero workflow edits |
| The worker stays bounded at 100–1000 concurrent runner operations | high | `tests/load`; `docs/benchmarks.md` (6 goroutines flat, 27.4 MiB HWM) |
| claude-code and codex are conformant actors; incomplete sessions never map to success | high | conformance kits (codex vs real CLI); 127/108 adapter tests; live run |
| The production pair runs two workers against one authoritative Postgres with backups proven restorable | high | worker ids in `runner_invocations`; restore drill digest-identical |
| The operations views render committed state with server-side time filtering | high | vitest 145 / Playwright 32; `docs/assets/phase2-*.png` |
| culture-nodes developed itself: a real repo change was produced by its own production run and human-approved | high | merged commit "docs(prod): document the runner-service registry file"; run ledger |
| CLI/Web/API parity holds for every new surface | high | `tests/parity` three-front harness (drift-catch verified by deliberate break) |
| Mechanical acceptance evaluation covers all nine kinds | unverified | 2 of 9 shipped; the rest refuse honestly (t17, needs-follow-up) |
| The claude bridge handles long sessions without operator intervention | low | required manual `ALWAYS_ASYNC=1` restart mid-cycle (issue #10) |

## Remaining Work / Follow-up

- Registration tooling for actors/runner services + deployment preflight
  (issue #8) — the biggest self-hosting friction.
- Test isolation on shared Postgres (issue #9).
- claude bridge: async-by-default, git-in-permission-mode decision,
  per-service runner revision (issue #10).
- Mechanical `acceptance.requires` beyond `process_exit`/`workspace_diff`
  (t17 partial).
- Deviations `d1`/`d2` await owner confirmation at this PR gate.
- Parked lanes unchanged: OTel (#5), OIDC incl. Entra ID (#6), real-AWS
  SQS/Lambda/Fargate decisions (#7).
