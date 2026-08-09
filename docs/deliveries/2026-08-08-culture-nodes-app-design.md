# Delivery Summary — culture-nodes app design

plan: `culture-nodes-app-design` · run: `complete` · date: `2026-08-08`
baseline: `devague summary skeleton`

## Intent

Execute the converged 27-task, 8-wave plan that turns this repo from a PRD +
mesh-agent scaffold into the Phase-0/1 Culture Nodes product: a Go control
plane implementing the PRD's durable graph runtime, a React Flow web front
carrying the agentculture.org design system, a thin Python CLI front over one
OpenAPI surface, Docker/k8s-native packaging, and trigger-only external
agents behind the provider-neutral actor protocol. One task per agent per
wave in isolated worktrees, TDD-gated merges by the main agent.

## Planned Work

Quoted verbatim from the `devague summary` skeleton:

- `t1` — Scaffold the Go monorepo layout per prd-spec §18 while preserving the mesh scaffold
- `t2` — Author workflow/ledger/runner JSON Schemas (2020-12) with canonical JSON normalization and SHA-256 digesting
- `t3` — Build the compiler and nodes validate: YAML/JSON to normalized IR with all validation levels
- `t4` — Cite the agent-first CLI contract into Go: output/error/exit conventions plus whoami/learn/explain/overview/doctor
- `t5` — Extract the culture-design layer from a pinned org revision: tokens, Mark, node palette, edge conventions
- `t6` — Postgres store: sqlc/pgx schema, migrations with expand-contract and N-1 binary compatibility
- `t7` — Work claiming: `work_items` with SKIP LOCKED, leases, fencing tokens, and two-worker fault tests
- `t8` — Ledger runtime: append with authority enforcement, projections, review transactions, supersession
- `t9` — Engine state machine: runs, tokens, node runs, attempts, edges, domain outcomes, bounded loops, transaction boundary
- `t10` — Transactional outbox and the narrow queue abstraction with the Postgres driver
- `t11` — Scheduler: durable timers and single-active lease holder with standbys
- `t12` — Actor protocol: invocation client, async callbacks with attempt-scoped tokens, provider-neutrality lint, conformance kit
- `t13` — Runner boundary and the Lambda adapter: typed operations, IAM-scoped registry-pinned dispatch, honest evidence mapping
- `t14` — Pre-run and post-run code hooks around agent attempts
- `t15` — Pod-agnostic artifact store: S3/MinIO driver plus Postgres small-artifact fallback
- `t16` — SQS queue driver with duplicate/reorder/drop chaos tests
- `t17` — AWS package isolation and credential chain: depguard lint plus IRSA-ready session resolution
- `t18` — Headspace local bridge: the dev-profile runner adapter driving the real CLI by subprocess
- `t19` — OpenAPI 3.1 API with SSE run events and the CLI/Web parity harness
- `t20` — Web front: React Flow Run and Ledger read-only views with progressive detail, a11y, and the agent-state test node
- `t21` — Container release lane: multi-arch control-plane images by digest to ghcr, alongside the PyPI lane
- `t22` — Helm chart with kind-cluster CI: per-role Deployments, probes, migration job, Ingress, replicas=2 safety
- `t23` — Docker compose local profile: complete local system in one command
- `t24` — Python thin CLI front: the nodes CLI becomes an API client with zero engine logic
- `t25` — Devague conformance adapter: plan-waves and deliverables fixtures round-trip to ledger projections
- `t26` — Colleague reference bridge: an external adapter image implementing the actor protocol over colleague subprocess dispatch
- `t27` — Phase-1 vertical slice end-to-end: the reference workflow, restart survival, acceptance checklist, benchmarks

## Actual Delivery

All 27 tasks merged. One (`t19`) is partial by recorded deviation.

| Plan task | Status | What actually landed |
|-----------|--------|----------------------|
| `t1` | delivered | PRD §18 tree, `go.mod`, stub `main`, Makefile, `go.yml`; Python CI untouched |
| `t2` | delivered | `schemas/{ledger,workflow,runner}` (15 files), `internal/contracts` canonical JSON + SHA-256, JSON-Pointer diagnostics |
| `t3` | delivered | `internal/compiler` (all §11.4 levels, CEL, runner-cap rejection), `nodes validate`; PRD example compiles deterministically |
| `t4` | delivered | `internal/clifmt` + introspection verbs; conformance test execs the real binary |
| `t5` | delivered | `web/src/culture-design` from `org@b4d939b` (ADR 0001), Mark, 7-color palette, dashed=proposed/solid=confirmed edges |
| `t6` | delivered | migrations 0001–0004 (20 tables, namespace-scoped, DB-enforced ledger immutability), sqlc/pgx store, `nodes migrate`, ADR 0002, N-1 harness |
| `t7` | delivered | SKIP LOCKED claiming with leases/fencing; two-OS-process fault suite (no double-commit, kill-9 reclaim, duplicate-signal dedup) |
| `t8` | delivered | `internal/ledger`: §10.4 authority matrix, 9 deterministic projections, atomic stale-guarded reviews, supersession, run advisory lock |
| `t9` | delivered | `internal/engine`: §12.5 transaction (step-mapped), bounded loops, domain outcomes route edges; restart survival proven |
| `t10` | delivered | CloudEvents envelopes, transactional outbox + crash-safe relay with stable IDs, 4-method queue interface + Postgres driver |
| `t11` | delivered | durable timers, single-active advisory-lock scheduler, standby-takeover test via `pg_terminate_backend` |
| `t12` | delivered | §13 protocol client, HMAC attempt tokens, fenced callback ingest, worker dispatch loop (agent/decision), neutrality lint, conformance kit |
| `t13` | delivered | `internal/runners` + Lambda adapter: registry-pinned ARN+digest dispatch, no-wildcard IAM template (ADR 0003), per-field evidence honesty |
| `t14` | delivered | pre_run/post_run schema+compiler+worker wiring; all three h32 failure paths tested; async+post_run refused at dispatch (documented) |
| `t15` | delivered | artifact Store interface, MinIO/S3 + Postgres-blob drivers, size-routing router, two-instance pod-agnostic proof, migration 0006 |
| `t16` | delivered | SQS driver over an in-process fake AWS protocol server; duplicate/reorder/drop chaos proven harmless against fenced Postgres claims |
| `t17` | delivered | go/parser AWS-isolation lint (proven by probe), `internal/awsauth` 5-link chain incl. IRSA (ADR 0004), migrated constructors |
| `t18` | delivered | headspace subprocess bridge: full exit-band map, separate-process `stop --apply` cancellation, secrets never in argv — live-Docker tested |
| `t19` | partial | OpenAPI 3.1 + `internal/api` (workflows/runs/ledger/reviews/SSE/health), parity harness, `nodes serve`; approval endpoints deferred (`d1`, issue #3) |
| `t20` | delivered | Vite/React/React Flow Run+Ledger views, ELK layout, zoom detail bands, keyboard nav, reduced motion, `#agent-state`, `web.yml` (vitest 75, Playwright 12, webglass pass) |
| `t21` | delivered | distroless multi-stage Dockerfile, `release.yml` (ghcr multi-arch by digest, SBOM/provenance), local build+run proof |
| `t22` | delivered | Helm chart (per-role Deployments, migration Job, PDB, Secret-refs, callback Ingress, authless-trust-boundary NOTES), real kind smoke at worker replicas=2 incl. Ingress-callback 401, `deploy.yml` |
| `t23` | delivered | compose profile (postgres/minio/migrate/api/scheduler/worker + opt-in colleague-bridge), manifest tests (no docker.sock), live smoke ran |
| `t24` | delivered | Python front = pure API client (urllib, zero deps), 88 tests, teken 26/26 strict, byte-exact `--json` passthrough |
| `t25` | delivered | `internal/devague` mapping real devague 0.22.0 fixtures to deterministic ledger records/projections; authority honesty proven |
| `t26` | delivered | `adapters/colleague` bridge: contract-v1 subprocess dispatch, flight-file polling, incomplete-never-success; actor conformance kit PASS against live bridge |
| `t27` | delivered | `examples/delivery-loop`, e2e with mid-run full-stack restart, both loop edges, live headspace variant (2 real containers, 3.57s), code-node worker wiring, `docs/acceptance.md`, `docs/benchmarks.md`, ADR 0005 |

## Mid-work Decisions

- `d1` — t19 shipped without the approval/human-task surface: the engine does
  not yet create `human_tasks` rows and the worker's HumanDispatcher seam is
  unregistered, so the API landed runs/ledger/reviews/SSE without approval
  endpoints rather than inventing endpoints over missing engine support —
  recorded via `/deviate` (proposed, awaiting owner confirmation), filed as
  issue #3. The reference workflow omits the approval node and cites `d1`.
- Lambda-first code running (user decision, spec c25) resolved the
  headspace-on-k8s park; headspace remains the dev-profile runner, wired live
  in the e2e's `e2elive` variant.
- Migration numbering collisions between parallel tasks (three 0005s) were
  renumbered at merge (0005 queue_signals, 0006 artifact_blobs, 0007
  ledger_origin_actor_revision, 0008 run_output); no deployed DB existed.
- t2's schema requires `ownerRef` on every node, making the PRD §9.4 owner
  inheritance unreachable through `Compile` — stricter than the PRD allows,
  recorded in t3's compiler docs; candidate ADR follow-up.
- Hook evidence appends as a second `ledger.Append` after the completion
  commit (agent nodes cannot declare `ledger.observe`); async actors with
  `post_run` are refused at dispatch, not compile time (sync/async is the
  actor's runtime choice).
- The web SPA embeds via a root `webassets` package + `-tags embedweb`;
  `nodes all` gained the in-process worker; the Dockerfile was rebuilt
  three-stage (it predated `go.sum`, `schemas/`, `migrations/`) — main-agent
  work between waves, no task owned it.
- The worker refuses agent dispatch without callback config (it could never
  hear a 202 back); surfaced during live testing, documented behavior.
- Actor-identity contract proven live: `ledger_records.origin_actor_id` FKs
  `actors.id`, so external adapters must stamp their **registered** actor row
  id — a deployment prerequisite, not a code defect.
- The worker-recovery fault test was made deterministic post-merge: it now
  measures h19's stated bound (reclaim within lease-expiry+5s) rather than a
  stricter all-completed proxy, starts the survivor after the kill, and
  scopes its accounting by namespace.

## Drift From Plan

| Plan item | Reason for divergence | Classification |
|-----------|-----------------------|----------------|
| `t19` (`d1`) | engine human-task support was an undeclared prerequisite the plan's task split missed; shipping honest scope beat shipping stub endpoints | needs-follow-up |
| `t6` | sqlc used as planned, but later tasks bypassed it with raw-SQL new files to honor new-files-only parallelism; `sqlcgen` models are stale for `origin_actor_revision` | acceptable |
| `t13`/`t27` | h22's "passes the runner conformance suite" is proven against a fake Lambda API + schema validation; no real-AWS invocation ran (risk r1: CI lane undecided; `awslive` tag exists, skipped without `NODES_TEST_LAMBDA_ARN`) | needs-follow-up |
| `t20` | webglass verified locally only with `WEBGLASS_ALLOW_UNSANDBOXED=1` (host apparmor); the CI job uses the documented sysctl path, untested on a real runner until the PR runs | acceptable |
| `t27` | two acceptance boxes NOT MET (Markdown-from-JSON projection rendering; mechanical `acceptance.requires` evaluation) and five partial — named in `docs/acceptance.md`, never checked without evidence | needs-follow-up |

## Evidence

- commits: `ead248a..df6fca9` on `nodes-app/build` (67 commits: 27 task
  merges + baseline, integration fixes, docs)
- tests (final full-tree runs on the merged branch): `go test ./...` — all 39
  packages ok, including `tests/e2e` (`TestPhase1VerticalSlice`,
  `TestFailedTestSuiteLoopsToBuildAsADomainOutcome`), real-Postgres suites,
  and `tests/fault` green under `-count=3` after the determinism fix;
  `uv run pytest -n auto` — 88 passed; `uv run --project adapters/colleague
  pytest adapters/colleague` — 100 passed; web: vitest 75 passed, Playwright
  12 passed
- live variants that actually ran: `TestPhase1VerticalSliceWithRealHeadspaceRunner`
  (real Docker containers); t22's kind cluster smoke (5 pods, replicas=2,
  Ingress callback 401); t23's compose smoke; the full-boundary live run
  documented below
- live full-boundary run (this session, then torn down): compose stack on
  :18080 (embedded SPA served from the distroless container), Python CLI
  publish (digest byte-identical to the Go binary's), run
  `01KZJ4VN58N5DK6Y92XSEC6YQJ` completed through the host worker → Bearer-
  authenticated actor protocol → colleague bridge → real `colleague work`
  (mock engine); ledger shows exactly one `claim` at authority `proposed`
- lint: `markdownlint-cli2` 0 errors; `gofmt`/`go vet` clean;
  black/isort/flake8/bandit clean; `teken cli doctor . --strict` 26/26;
  provider-neutrality and AWS-isolation lints green
- issues: #3 (`deviate: approval/human-task surface deferred`)
- artifacts: `docs/acceptance.md` (41 met / 5 partial / 2 not met),
  `docs/benchmarks.md` (idle RSS 30.1 MiB; 128 transitions/s sequential;
  100k-record projection 441 ms digest-stable), ADRs 0001–0005

## Delivery Claims

| Claim | Confidence | Evidence |
|-------|------------|----------|
| The Phase-1 slice runs end to end with the `changes_required` loop as a domain outcome and survives a full mid-run restart | high | test `tests/e2e/slice_test.go::TestPhase1VerticalSlice` |
| Multi-pod safety holds: no double-commit, kill-9 reclaim within h19's bound, duplicate signals harmless | high | `tests/fault` (3× consecutive green); kind smoke at worker replicas=2 |
| Agents cannot self-confirm: agent-origin records stay `proposed` until human review | high | `internal/ledger` property tests; live run ledger (`claim`/`proposed`) |
| Code executes only through external runners; no Docker socket in any control-plane container | high | manifest tests in `tests/deploy`; `internal/runners/{lambda,headspace}` |
| The UI is embedded in the Go binary and carries the pinned org design system | high | live compose curl of `/` (title "Culture Nodes"); `scripts/check-culture-design.mjs`; ADR 0001 |
| CLI/Web/API parity holds by construction | medium | `tests/parity`; live CLI drive of publish/run/ledger — approvals absent on all surfaces alike (d1) |
| A real external agent completes work through the actor protocol | high | conformance kit PASS vs the colleague bridge; live run `01KZJ4VN58N5DK6Y92XSEC6YQJ` |
| The Lambda adapter dispatches correctly against real AWS | unverified | only fake-API + schema tests ran; `awslive` tag awaits an AWS lane (r1) |
| The Helm chart deploys on a real k8s cluster | high | t22's local kind transcript (install, smoke, upgrade, Ingress callback) |
| Benchmarks meet §21.2's reference targets | unverified | recorded on a non-reference host; 250/s sustained concurrent not demonstrated (`docs/benchmarks.md`) |

## Remaining Work / Follow-up

- `t19`/`d1` — approval/human-task surface: engine `human_tasks` creation,
  API decision endpoints, worker HumanDispatcher, e2e human-review branch
  (issue #3). Blocking for PRD §9.9.
- Real-AWS Lambda lane (risk r1): decide test account vs LocalStack, arm the
  `awslive` tag in CI, then upgrade the Lambda delivery claim.
- Acceptance not-mets: Markdown projection rendering; mechanical
  `acceptance.requires` evaluation (`docs/acceptance.md`).
- `run.output` observed null for the live smoke's end-node binding — verify
  the end-node output path outside the e2e fixtures.
- `sqlcgen` staleness; owner-inheritance schema tension (candidate ADR);
  `waiting_external` deadline timers don't fail attempts; OpenTelemetry is a
  stub (Phase 2); cost budgets absent until Phase 4 (parked).
- Devague deviation `d1` is recorded but `proposed` — owner confirmation
  (`devague deviate --confirm d1`) pending at the PR gate.
