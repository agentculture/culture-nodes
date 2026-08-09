# Build Plan — self-hosted phase-2 cycle

slug: `self-hosted-phase-2-cycle` · status: `exported` · from frame: `self-hosted-phase-2-cycle`

> Culture Nodes now runs its own development: the deferred approval surface landed, claude-code is a first-class actor from dev (spark) to production (thor+orin) sharing one Postgres, and operators watch runs through a cards board, a jobs timeline table, and a time-range filter on latest update.

## Tasks

### t1 — Cycle baseline: re-run all suites on main and record results

- instruction: Run on main before branching. Commands are in CLAUDE.md; record results in a dated note under docs/. For the AWS check: confirm the awslive-tagged tests report skip without `NODES_TEST_LAMBDA_ARN` and the fake-backed SQS/Lambda suites ran in the default pass.
- covers: c10, h8, c9, h7
- acceptance:
  - go test ./... green including tests/fault; uv run pytest -n auto green; adapters/colleague suite green; web vitest + Playwright green
  - awslive tag confirmed skipped without credentials; fake-backed SQS/Lambda suites confirmed in the default run
  - baseline recorded in a dated note under docs/ for the delivery summary to cite

### t2 — `waiting_external` deadline timers fail attempts

- instruction: Timers live in internal/scheduler (t11 precedent: durable timers + advisory-lock single-active); the failure path appends through the outbox. Guard on attempt state so a completed attempt never receives a late deadline failure. `waiting_external` parking semantics are documented in internal/worker/doc.go.
- covers: c11, h9
- acceptance:
  - an attempt whose `waiting_external` deadline passes is failed by the scheduler with an appended run event, and the run routes its failure edge (regression test)
  - completed attempts never receive a late deadline failure (test)

### t3 — run.output end-node binding fix

- instruction: Reproduce first: the null run.output was observed on the live compose smoke's end-node binding (delivery follow-ups). The binding path is in internal/engine; migration `0008_run_output` holds the column. Write the regression against the non-fixture path.
- covers: c11, h9
- acceptance:
  - the live-smoke scenario reproduces a non-null run.output: end-node output binding populates run output outside e2e fixtures (regression test)

### t4 — Migration 0010: runs.`updated_at` index + node-runs listing index

- instruction: New migrations/`0010_`\*.sql following 0009's (state, `updated_at`) index shape on `actor_invocations`. Extend the N-1 binary-compatibility harness the way 0005-0008 did. Capture the EXPLAIN output in the task evidence.
- covers: c5
- acceptance:
  - migration adds the index(es) under expand-contract; the N-1 compatibility harness passes
  - EXPLAIN on the `updated_since`/`updated_until` filter query shows an index scan, recorded in the task evidence

### t5 — Runner-service wire contract + runner-neutral registry identity

- instruction: schemas/runner/\*.json are the wire payloads verbatim — do not fork them. Protocol doc lands beside api/openapi. In internal/runners/registry.go add a second, endpoint+digest+secret-ref identity form; FunctionIdentity stays valid for the legacy Lambda adapter. State auth as mandatory (c25) and async-only semantics with the callback documented optional (c23).
- covers: c14
- acceptance:
  - the runner protocol is documented: execute/status endpoints reusing schemas/runner operation/result verbatim, async-only semantics (dispatch returns 202, completion via status sampling), completion callback documented as strictly optional
  - registry identity accepts endpoint + pinned digest + secret ref alongside the legacy ARN form; ARN-only validation no longer blocks non-Lambda runners (tests)
  - the contract states caller authentication is mandatory and the no-shell-wrapper policy language binds implementations

### t6 — Engine: approval nodes create `human_tasks` and pause leaselessly

- instruction: Engine dispatch of the approval node kind writes `human_tasks` (table from migrations/0002, fields per PRD §9.9: decision schema, approver role/group, deadline, context refs, allowed outcomes) and parks the token leaselessly — follow the `waiting_external` precedent, not the code-node lease-holding path.
- covers: c2
- acceptance:
  - dispatching an approval node writes a `human_tasks` row carrying decision schema, approver role, deadline, and context refs (unit test)
  - the paused run holds no `work_items` lease and no open transaction while waiting (test asserts both)

### t7 — Human-tasks API: list/get/decision endpoints

- instruction: internal/api + api/openapi/openapi.json. The decision commits through the existing atomic stale-guarded review transaction in internal/ledger (t8-era). Auth follows the existing API auth model; unauthenticated decision POSTs refused.
- depends on: t6
- covers: c2
- acceptance:
  - GET /v1alpha1/human-tasks, GET /human-tasks/{id}, POST /human-tasks/{id}/decision are implemented and documented in api/openapi; unauthenticated decision POST is refused
  - a decision commits atomically through the review transaction, rejects stale ledger versions, and resumes the run (tests)

### t8 — Worker: register a real HumanDispatcher

- instruction: The HumanDispatcher seam is in internal/worker/seams.go (left unregistered by design in the last cycle — issue #3). Register a real implementation that parks the item leaselessly; the unit test drives the seam the way the worker dispatch tests do.
- depends on: t6
- covers: c2
- acceptance:
  - approval work items route through a registered HumanDispatcher that parks without holding the lease instead of erroring (unit test through the t12-era seam)

### t9 — Worker: async runner dispatch — park/resume + idempotent completion ingest

- instruction: New dispatch path beside internal/worker/code.go — do not extend the lease-holding path. Precedent for resume-under-fencing: internal/actors.HandleCallback. Deadlines ride t2's timers. The fault test follows tests/fault's two-OS-process pattern (kill -9 mid-operation, survivor resumes within the stated bound).
- depends on: t2, t5
- covers: c24, h17
- acceptance:
  - runner-protocol dispatch parks the work item as `waiting_external`, holding no lease and no goroutine between status samples (test)
  - duplicate or racing completion reports (two samples, sample+callback) are harmless under the fencing discipline (test)
  - a worker killed mid-operation strands nothing: the surviving worker resumes tracking after handoff, within the stated bound (fault test)

### t10 — Reference runner service: headspace wrapped behind the contract, with auth

- instruction: Wrap the existing internal/runners/headspace bridge behind the t5 contract as a separate service process; hold per-operation status like the colleague bridge holds flight state. Auth case goes into a runner conformance kit shaped like tests/conformance. A live test mirrors headspace's `live_test.go`.
- depends on: t5
- covers: c25, h18, c14
- acceptance:
  - a runner-service process wraps the headspace bridge behind the protocol, holds per-operation status, and serves it over the status endpoint
  - unauthenticated execute/status requests are refused with 401/403 — proven by a runner conformance-kit auth case against the live service
  - the runner conformance kit passes end to end against the wrapped headspace deployment

### t11 — API: run-list time params + cross-run node-runs endpoint

- instruction: internal/api/queries.go for the runs filter (uses t4's index) and a new cross-run node-runs query; document in api/openapi/openapi.json. Keep JSON byte-stable — the Python front passes it through verbatim (t16 parity).
- depends on: t4, t7
- covers: c5, h5
- acceptance:
  - GET /v1alpha1/runs accepts `updated_since`/`updated_until` and explicit `updated_at` sort, documented in api/openapi (tests)
  - a cross-run node-runs listing endpoint returns run, node, actor/runner, status, outcome, started, updated with a time window and pagination, using the 0010 index (tests)

### t12 — claude-code actor adapter (adapters/claude-code)

- instruction: Mirror adapters/colleague's layout (`async_runner`, callbacks, flightfiles, idempotency, mapping, server, config). Drive headless claude (claude -p) as contract-v1 subprocess dispatch. Min-version gate at startup (fleet runs 2.1.220-2.1.226 today). CI runs the conformance kit like colleague's `run_conformance_kit.sh`.
- covers: c3, h3
- acceptance:
  - the adapter drives headless claude via contract-v1 subprocess dispatch and passes the actor conformance kit unmodified in CI
  - an incomplete or crashed claude session maps to failure, never success (test)
  - dispatch below the pinned minimum claude CLI version is refused with an honest DispatchError naming both versions (test)

### t13 — codex actor adapter (adapters/codex)

- instruction: Same sibling shape as t12, driving headless codex exec (codex-cli 0.144.6 verified on dev). Same conformance kit, no adapter-specific exemptions (h20).
- covers: c30, h20
- acceptance:
  - adapters/codex drives headless codex exec via contract-v1 subprocess dispatch and passes the same actor conformance kit unmodified in CI
  - an incomplete or crashed codex session maps to failure, never success (test)

### t14 — Web: runs board — cards on state columns

- instruction: New web/src/routes/BoardView.tsx + route registration in App.tsx. Reuse StatusChip/NodeCard and culture-design tokens; columns are run states; cards navigate to RunView. Render only API data (h5). Follow the existing vitest + Playwright split (web/e2e).
- depends on: t11
- covers: c5, h5
- acceptance:
  - a board route renders runs as cards grouped into state columns (queued/running/waiting/completed/failed) from API data only; approval-paused runs appear under waiting (vitest + Playwright)
  - keyboard navigation and reduced-motion behavior hold on the new route (a11y checks)

### t15 — Web: jobs timeline table + time-range filter

- instruction: web/src/routes/JobsView.tsx; land after t14 so App.tsx route edits don't collide. Table follows LedgerTable/EventTimeline patterns; the range filter writes `updated_since`/`updated_until` into the query and the URL search params — never filters client-side (h5).
- depends on: t11, t14
- covers: c5, h5
- acceptance:
  - a jobs route renders cross-run node runs newest-first with run/node/actor/status/outcome/started/updated columns from the API (tests)
  - the time-range filter drives `updated_since`/`updated_until` API params server-side — never a client-side filter — and the active range is reflected in the URL (tests)

### t16 — Python CLI + parity harness for every new surface

- instruction: New `culture_nodes`/cli/`_commands` modules per the register(subparsers) pattern; every new verb needs an explain-catalog entry (tests introspect `known_paths`()). Extend tests/parity so a missing surface fails. Zero new runtime deps; teken cli doctor . --strict stays green.
- depends on: t7, t11
- covers: c6, h6
- acceptance:
  - the nodes CLI gains human-tasks verbs and the new run-list/node-runs params with byte-exact --json passthrough (tests)
  - tests/parity fails if any of the new endpoints/params is missing from openapi, the Python CLI, or the web client layer
  - teken cli doctor . --strict stays green

### t17 — Acceptance not-mets: Markdown projection rendering + mechanical acceptance.requires

- instruction: Markdown rendering derives from JSON projections deterministically (PRD: Markdown is never authoritative). acceptance.requires evaluation belongs in the acceptance harness. If either is deferred instead, the ADR goes under docs/adr/ with reasons, and docs/acceptance.md stays truthful.
- covers: c11, h9
- acceptance:
  - Markdown summaries render deterministically from JSON projections with tests, or an ADR records the deferral with reasons
  - acceptance.requires is evaluated mechanically in the acceptance harness, or an ADR records the deferral; docs/acceptance.md checkboxes change only with evidence

### t18 — Load test: 100 concurrent runner operations, bounded worker

- instruction: Stub runner service = in-process HTTP honoring the t5 contract with configurable operation duration. Hold 100 in-flight ops; record worker RSS and sampling cadence; show independence from op duration. Results append to docs/benchmarks.md with the 1000-case method stated.
- depends on: t9
- covers: c17, h12
- acceptance:
  - a harness holds 100 concurrent in-flight operations against a stub runner service; worker RSS stays bounded and is recorded
  - status-sampling load is shown independent of operation duration; the 1000 case is tested or extrapolated with the method stated in docs/benchmarks.md

### t19 — Production setup: thor+orin compose profiles + credential authorize flow

- instruction: Per-machine compose profiles under deploy/compose; deploy scripts use the ssh thor / ssh orin aliases with argv-only invocations (reachy-mini-cli ssh.py precedent). Decide and document the non-SSH secret mechanism (risk r5). Verify both worker ids appear in `work_items` against thor's DB.
- depends on: t10
- acceptance:
  - compose profiles exist for thor (postgres/minio/api/scheduler/worker/runner-service) and orin (worker + runner-service pointing at thor); deploy scripts drive them over ssh thor / ssh orin with argv-only invocations
  - secrets reach both machines through an explicit, confirmed authorize step — never in argv, never typed through the tool (c8 pattern); the mechanism chosen for non-SSH secrets is documented
  - both production workers claim work against thor's single Postgres, each worker id visible in `work_items` (live check)

### t20 — thor Postgres backup job + restore drill

- instruction: Backup job as a compose sidecar/cron in thor's profile (`pg_dump` or WAL-based — choose and justify in the runbook). The drill restores on a different machine and verifies a run's ledger projections reproduce digest-stable.
- depends on: t19
- covers: c26, h19
- acceptance:
  - a scheduled backup job ships in thor's compose profile with a written restore runbook
  - one restore drill actually ran: a backup from thor's DB restored on another machine reproduces a run's ledger with content digests intact, recorded as evidence

### t21 — e2e: the human-review branch

- instruction: Sibling of tests/e2e/`slice_test.go`. Restore the approval node the reference workflow omitted citing d1 (examples/delivery-loop). Assert the pause holds no lease and the decision arrives as a human-authority review.
- depends on: t7, t8
- covers: c2, h2
- acceptance:
  - tests/e2e walks verify.blocked -> human-review -> decision -> build resume; the run holds no lease during the pause; the decision lands as a human-authority review in the ledger
  - both existing loop edges still pass alongside the new branch

### t22 — Placement-free proof: same digest on two machines

- instruction: Registry config points the same workflow digest at spark's runner service, then thor's. No workflow-definition edits allowed between the two runs (h11). Record both run ledgers as the evidence pair.
- depends on: t10, t19
- covers: c14, h11
- acceptance:
  - the same workflow digest runs with its code node executing on spark's runner service and then thor's, with zero workflow-definition changes — only registry config differs; both ledgers recorded as evidence

### t23 — Self-hosted run: culture-nodes develops itself on the production pair

- instruction: Pick a real open work item from this cycle's follow-ups as the run's task. The claude-code actor produces the diff; the PR goes through the cicd lane; the human decision goes through the t7 endpoint. Runs on thor+orin (h14) with the views watched live. Keep the run id + ledger export with the delivery evidence.
- depends on: t12, t14, t15, t19, t21
- covers: c1, h1, c4, h4, c20, h14, c22, h16
- acceptance:
  - a dev-loop run published via the API executes on thor+orin with the claude-code actor doing a real repo task; the produced diff goes through the normal PR lane and merges
  - the run id, its ledger records, and the merged diff are citable — the ledger holds the proposed claim, observed evidence, and the human review through the decision endpoint
  - the board, jobs timeline, and time filter show the run live while it executes

### t24 — Docs + README: OSS standup path, runner protocol, operations views

- instruction: README reframe + quickstart verified verbatim in a clean container. Update the known template-drift strings (learn, argparse description, explain root catalog — flagged in CLAUDE.md). Document the runner protocol, approval surface, and views as shipped, with t25's screenshots referenced once they exist.
- depends on: t10
- covers: c19, h13
- acceptance:
  - a fresh-machine quickstart shows compose up + docs sufficing to publish and run a workflow with a custom runner/actor stub — no AgentCulture-mesh prerequisite; verified by following the doc verbatim in a clean environment
  - README and docs/ describe the runner protocol, the approval surface, and the new views as shipped; stale template self-description strings (learn/explain/argparse prose) are updated

### t25 — Live-testing evidence: screenshots of the operations views against the production run

- instruction: Use Playwright against the live production run for pixel-true captures; name files like the existing docs/assets set. Board, filtered jobs timeline, RunView, LedgerView. Reference them from README/docs and the delivery summary.
- depends on: t23
- covers: h1
- acceptance:
  - screenshots of the board, jobs timeline (filtered), run view, and ledger view captured against the live self-hosted production run land under docs/assets and are referenced from the docs and the delivery summary

### t26 — Integration gate: version bump, lanes check, delivery summary before PR

- instruction: Order matters: /version-bump, vendored-skills diff check, friction filed as issues/deviations/ADRs, then /summarize-delivery writes docs/deliveries/<date>-self-hosted-phase-2-cycle.md citing t23's run ledger, and only then the cicd lane opens the PR.
- depends on: t1, t3, t13, t16, t17, t18, t20, t22, t23, t24, t25
- covers: c12, h10, c21, h15
- acceptance:
  - the PR carries a version bump; git diff over .claude/skills/ against main is empty; no code reimplements culture/events-cli/agenda
  - dogfooding friction found during the cycle is filed as issues, deviations, or ADRs before merge — none absorbed silently
  - the delivery summary is produced via /summarize-delivery and lands in docs/deliveries BEFORE the PR opens via the cicd lane, citing the self-hosted run's ledger as evidence

## Risks

- [follow_up] Real-AWS lane stays deferred: SQS/Lambda/Fargate decisions tracked in issue #7; nothing in this plan may require AWS credentials (c9)
- [unknown_nonblocking] Runaway-cost containment for self-hosted claude/codex actors: bounded loops and attempt timeouts are the only brakes until PRD Phase-4 budgets
- [unknown_nonblocking] Self-hosting recursion: a broken control-plane build deployed by the run that built it bricks the loop — dev-first staging discipline is process, not enforcement (task t23)
- [unknown_nonblocking] orin memory headroom (~8 GiB free): hosting claude-code actor sessions there may not fit alongside the worker and runner service — actor placement may end up thor-only (task t19)
- [unknown_nonblocking] Non-SSH secret distribution mechanism (DB password, HMAC secrets, `ANTHROPIC_API_KEY`): OS keyring vs env files vs compose secrets — decided during t19 design, documented in its acceptance evidence (task t19)
