# Build Plan — culture-nodes app design

slug: `culture-nodes-app-design` · status: `exported` · from frame: `culture-nodes-app-design`

> Culture Nodes ships as an agent-first, agent-native workflow app: a Go control plane implementing the PRD's durable graph runtime, a react-flow web UI carrying the agentculture.org design system, and a Python agent-first CLI — with full CLI/API/Web parity, Docker-native packaging that deploys cleanly to k8s, AWS-backed SQS signaling over authoritative Postgres, and agent actors reachable through claude-agent-sdk, claude-agent-api, or colleague adapters behind the provider-neutral actor protocol

## Tasks

### t1 — Scaffold the Go monorepo layout per prd-spec §18 while preserving the mesh scaffold

- instruction: Create the Go module (module github.com/agentculture/culture-nodes; Go 1.23+) with the prd-spec §18 tree: cmd/nodes plus internal/{api,auth,compiler,contracts,ledger,engine,scheduler,worker,actors,runners,queue,store,artifacts,events,policy,telemetry}, api/{openapi,actor-protocol}, schemas/, migrations/, web/, deploy/{compose,helm,aws}, tests/{conformance,fault,ledger,load}. Stub packages with doc.go so go build ./... passes. Do NOT touch `culture_nodes`/ (Python), culture.yaml, AGENTS.colleague.md, or .claude/. Add a make target and a minimal Go CI job; the existing Python CI jobs must stay green untouched.
- covers: c19, h16
- acceptance:
  - Repo gains cmd/nodes, internal/{api,compiler,contracts,ledger,engine,scheduler,worker,actors,runners,queue,store,artifacts,events,policy,telemetry}, schemas/, migrations/, web/, deploy/ with a building go.mod; go build ./... passes
  - culture.yaml, AGENTS.colleague.md, and .claude/skills stay at repo root; existing Python CI (teken cli doctor --strict, zero-runtime-dep check) stays green

### t2 — Author workflow/ledger/runner JSON Schemas (2020-12) with canonical JSON normalization and SHA-256 digesting

- instruction: Author schemas/ from prd-spec §10.2-10.3 (ledger: 10 record types, common envelope, authority enum), §9.3 (node contracts), and the runner operation/result shape of §13.7 adapted for runner-agnostic use. JSON Schema Draft 2020-12. Implement internal/contracts canonical-JSON: stable key order, UTF-8, no insignificant whitespace, then SHA-256 digest; golden-file tests prove identical input yields byte-identical canonical form and digest. Malformed fixtures must fail with JSON-Pointer diagnostics.
- depends on: t1
- acceptance:
  - schemas/ contains workflow, ledger (10 record types + authority enum), and runner operation/result schemas; a normalization library produces byte-identical canonical JSON and identical digests for identical definitions (golden tests)
  - Deliberately malformed fixtures fail schema validation with precise pointer-path diagnostics

### t3 — Build the compiler and nodes validate: YAML/JSON to normalized IR with all validation levels

- instruction: Implement internal/compiler per prd-spec §11: YAML/JSON in, normalized JSON IR out with expanded defaults, resolved owners, pinned digests, validated JSON-Pointer bindings, compiled CEL (cel-go), normalized edge order, content digest. Validation levels §11.4 in order, each with precise diagnostics. Wire the nodes validate verb onto t4's CLI layer. Include the PRD §11.1 delivery-loop example as a fixture compiling deterministically. Reject code nodes whose timeout exceeds the runner cap (Lambda 900s) or inline payloads above runner limits, naming the cap in the diagnostic (claim c40).
- depends on: t2
- covers: c18, h15, c40, h35
- acceptance:
  - The delivery-loop example compiles deterministically (identical digest across runs); deliberate ownership, graph, contract, ledger, and policy errors each produce a precise diagnostic (Milestone-0 exit demonstrated in CI)
  - A code node with a 20-minute timeout or an inline payload above the runner limit is rejected at validate time with a diagnostic naming the cap

### t4 — Cite the agent-first CLI contract into Go: output/error/exit conventions plus whoami/learn/explain/overview/doctor

- instruction: Cite (copy, adapt, own) the agent-first CLI conventions into Go from the Python family: devague/devague/cli/`_output.py` and `_errors.py`, headspace-cli headspace/cli/, ec2bedrock-cli ec2bedrock/cli/. Contract: results stdout / errors+diagnostics stderr never mixed; CliError{code,message,remediation}; text errors as 'error: <msg>' + 'hint: <remediation>'; exit 0/1/2 (3+ reserved); --json on every verb including parse-time failures (pre-scan argv); verbs whoami, learn, explain <path>, overview, doctor, cli overview backed by a catalog keyed by command-path. Write the conformance test that locks all of this.
- depends on: t1
- covers: c13, h11
- acceptance:
  - A Go conformance test asserts results-to-stdout/errors-to-stderr never mixed, code+message+remediation error shape with error:/hint: text, exit 0/1/2, and --json on every verb including parse-time failures

### t5 — Extract the culture-design layer from a pinned org revision: tokens, Mark, node palette, edge conventions

- instruction: Extract from /home/spark/git/org at a pinned commit (record it in docs/adr/): site-astro/src/styles/global.css verbatim into web/src/culture-design/ (it is framework-agnostic custom properties), port site-astro/src/components/Mark.astro to a React SVG component, export the terminal 7-color categorical palette (#7fdcc9 teal, #7fb3f2 blue, #f2b774 amber, #b49cf2 violet, #f2789a pink, #9fd6a3 green, #e6cd7a yellow, #a9b0cf neutral) as node-kind constants, and encode the edge conventions from CliRuntimeStackDiagram.astro: solid --accent-strong = confirmed/active, dashed 9-7 = proposed. Dark mode follows prefers-color-scheme only. Add render-and-compare visual fixtures.
- depends on: t1
- covers: c9, h8, c20, h17
- acceptance:
  - web/src/culture-design contains global.css tokens extracted from a named org commit recorded in an ADR; Mark is a React SVG component; the terminal 7-color palette and dashed=proposed/solid=confirmed edge styles are exported constants
  - Visual regression fixtures render the components and compare against the pinned revision

### t6 — Postgres store: sqlc/pgx schema, migrations with expand-contract and N-1 binary compatibility

- instruction: Implement internal/store/postgres with pgx v5 + sqlc: all prd-spec §14 tables, `namespace_id` on every operational row from migration 0001. Migrations are numbered SQL applied by a 'nodes migrate' subcommand (usable as a k8s Job). Document and enforce expand-contract: additive first, destructive only after N+1. Ship the N-1 compatibility harness: CI checks out the previous tag's binary and runs its smoke against the migrated schema.
- depends on: t1
- covers: c29, h24, c39, h34
- acceptance:
  - All prd-spec §14 core tables exist with `namespace_id` from the first migration; sqlc-generated queries compile; pgx is the only database driver in go.mod
  - CI runs the N-1 binary against a schema-N database in a compatibility test; migrations apply via a standalone migrate command suitable for a pre-rollout job

### t7 — Work claiming: `work_items` with SKIP LOCKED, leases, fencing tokens, and two-worker fault tests

- instruction: Implement `work_items` claiming per prd-spec §12.4: claim via one UPDATE ... WHERE id IN (SELECT ... FOR UPDATE SKIP LOCKED) setting `lease_owner`/`lease_expiry`/`fencing_token`(+1 on reclaim); completion requires matching work id, expected state, attempt, fencing token. Fault tests (tests/fault): two workers + duplicate signal never double-commit; kill -9 a worker mid-claim, assert reclaim within lease expiry + 5s; stale-token commit rejected.
- depends on: t6
- covers: c22, h19
- acceptance:
  - Claiming is atomic under SELECT FOR UPDATE SKIP LOCKED; reclaim after lease expiry increments the fencing token; completion updates matching a stale token are rejected
  - Fault tests with two workers against one Postgres: duplicate signals never double-commit, a killed worker's lease is reclaimed within expiry plus five seconds

### t8 — Ledger runtime: append with authority enforcement, projections, review transactions, supersession

- instruction: Implement internal/ledger per prd-spec §10: append-only records with the §10.3 envelope, producer/authority matrix enforced at append (agents propose only; humans confirm/reject; runners observe declared-measured fields only; engine derives), deterministic projections (§10.9) with stable digests, supersession, atomic review transactions guarded by ledger version/frame checksum (stale -> reject whole batch). Property tests (prd-spec §22): agent-origin never confirmed without authorized review; process-reported text never observed; superseded never reappears; identical inputs -> identical projection digests.
- depends on: t2, t6
- covers: c5, h5, c35, h30
- acceptance:
  - Property tests prove an agent-origin record never reaches confirmed without an authorized review, process-reported text never becomes observed evidence, and superseded records never reappear in projections
  - Review batches are all-or-nothing and stale ledger versions are rejected; identical ledger inputs produce identical deterministic projection digests

### t9 — Engine state machine: runs, tokens, node runs, attempts, edges, domain outcomes, bounded loops, transaction boundary

- instruction: Implement internal/engine: runs, tokens, `node_runs`, attempts as separate records; edges from named domain outcomes with optional CEL guards; `changes_required` and peers are outcomes that follow edges, never engine failures (prd-spec §3.4). The single completion transaction per §12.5 (fencing check -> output contract -> ledger delta validation -> append -> attempt result -> node-run completion -> audit events -> eligible edges -> next token/node-runs -> outbox -> commit). Engine-enforced loop bounds: maxTransitions, maxVisitsPerNode, maxDuration, maxParallelTokens. Property tests: terminal node runs never revert; old fencing tokens never commit; event sequence monotonic per aggregate.
- depends on: t3, t7, t8
- acceptance:
  - The §12.5 completion transaction (fencing check, contract validation, ledger delta validation, append, edge selection, outbox insert) commits atomically; `changes_required` follows a graph edge and is never an engine failure
  - Loop bounds (max transitions, visits per node, wall clock) are engine-enforced with tests; no terminal node run ever becomes non-terminal (property test)

### t10 — Transactional outbox and the narrow queue abstraction with the Postgres driver

- instruction: Implement internal/events (CloudEvents-style envelopes, §15.1 types) + transactional outbox written inside the §12.5 commit, plus internal/queue with the four-method interface (publish/receive/ack/delay of work references only) and the Postgres driver. The outbox relay is the only publisher; a test drops a publication and proves outbox repair; every receive re-claims through t7's fenced path so duplicates/reorders are harmless.
- depends on: t6
- acceptance:
  - The outbox is the sole event/signal publisher; a dropped publication is republished from the outbox; the queue interface is exactly publish/receive/ack/delay of work references
  - Every receive performs a fenced Postgres claim so duplicate and reordered signals are harmless (test)

### t11 — Scheduler: durable timers and single-active lease holder with standbys

- instruction: Implement internal/scheduler: timers as durable rows (waits, retries, deadlines, lease recovery) claimed in bounded batches inside transactions; single-active scheduler via a Postgres advisory/lease lock with standby takeover. Test: kill the active scheduler with due timers pending; standby fires each timer exactly once (no loss, no double-fire).
- depends on: t10
- acceptance:
  - Waits, retries, and deadlines are durable rows claimed in bounded batches; killing the active scheduler promotes a standby without losing or double-firing due timers (test)

### t12 — Actor protocol: invocation client, async callbacks with attempt-scoped tokens, provider-neutrality lint, conformance kit

- instruction: Implement internal/actors: the prd-spec §13 protocol client — POST /v1/invocations with Idempotency-Key, sync 200 result, async 202 {`invocation_id`, `heartbeat_after_seconds`}, callback events with stable event id + monotonic sequence, dedup + fencing so late/duplicate callbacks are recorded but cannot commit stale state; attempt-scoped short-TTL callback tokens; §13.5 error classification (only declared-retryable retries). Provider-neutrality: CI grep forbids provider names in engine/api/compiler; binary links no agent SDK. Publish tests/conformance as a runnable kit (Go test binary + fixture doc) any adapter author can point at their endpoint.
- depends on: t9
- covers: c3, h3, c24, h21, c31, h26, c42, h37
- acceptance:
  - Sync 200 and async 202+callback flows work with idempotency keys and fencing; duplicate and late callbacks are harmless (tests); expired or foreign callback tokens are rejected with a structured error
  - A CI grep proves internal/engine, internal/api, internal/compiler contain no provider names; the control-plane binary links no agent SDK
  - A language-neutral actor conformance kit (auth, idempotency, sync, async, heartbeat, cancel, duplicate callbacks) is published and runnable against any adapter

### t13 — Runner boundary and the Lambda adapter: typed operations, IAM-scoped registry-pinned dispatch, honest evidence mapping

- instruction: Implement internal/runners: a runner-agnostic interface mirroring the schemas from t2, then the Lambda adapter — invoke digest-pinned container-image functions listed in a function registry (ARN + image digest per node policy); refuse unregistered identities at dispatch; evidence from Lambda-observed facts only (request id, function version, image digest, duration, billed memory, exit) with completeness flags for what Lambda cannot observe; oversize payloads via S3 refs. AWS SDK imports only in this package. Build against the interface with a fake for unit tests; the real-AWS integration test is gated behind an env flag (risk r1: CI lane undecided).
- depends on: t9
- covers: c25, h22, c41, h36
- acceptance:
  - The Lambda adapter invokes only registry-pinned function identities (ARN + image digest); dispatch to an unregistered function is refused (test); the reference IAM policy enumerates registered ARNs with no wildcard invoke
  - Evidence carries request id, function/image digests, duration, billed memory, exit status; unobservable fields (workspace snapshot/diff completeness) are explicitly marked incomplete, never fabricated; oversized results route through S3 artifact references

### t14 — Pre-run and post-run code hooks around agent attempts

- instruction: Extend the workflow schema (coordinate with schemas/ from t2 — additive change) so agent nodes declare `pre_run`/`post_run` code operations. Engine semantics: pre-run executes through the runner boundary before agent dispatch — failure fails the attempt (technical, retryable per policy) and the agent is never invoked; post-run executes after the agent returns — check failure maps to a declared domain outcome or assurance rejection, never silence. Hook executions are `runner_operations` rows with observed evidence. Tests for all three paths: pre-run fail, post-run fail, both pass.
- depends on: t9, t13
- covers: c37, h32
- acceptance:
  - A node can declare `pre_run`/`post_run` code operations executed through the runner boundary; a pre-run failure fails the attempt before agent dispatch and is retryable per policy; a post-run check failure maps to a declared domain outcome or assurance rejection (tests for all three paths)
  - Hook executions are recorded as runner operations with observed evidence; no hook code path executes in-process

### t15 — Pod-agnostic artifact store: S3/MinIO driver plus Postgres small-artifact fallback

- instruction: Implement internal/artifacts: driver interface + S3-compatible driver (MinIO in dev) + Postgres small-artifact store with enforced size caps; artifact refs are artifact:// URIs resolved via the store, never filesystem paths. Integration test at two API/worker instances sharing one Postgres+MinIO: artifact written through instance A reads through instance B.
- depends on: t6
- covers: c38, h33
- acceptance:
  - At replicas=2 an artifact written via pod A is readable via pod B (integration test); no code path writes artifacts to pod-local filesystem; inline payload size caps are enforced

### t16 — SQS queue driver with duplicate/reorder/drop chaos tests

- instruction: Implement internal/queue/sqs on t10's interface: SQS Standard, messages carry work references only, visibility timeout tuned to lease expiry. Chaos tests (LocalStack or fake): duplicate delivery, reordering, dropped messages — Postgres state stays correct and outbox republishes; SQS SDK imports confined to this package.
- depends on: t10
- covers: c30, h25
- acceptance:
  - The SQS driver implements the queue interface carrying work references only; chaos tests prove duplicate, reordered, and dropped deliveries never corrupt authoritative Postgres state and the outbox repairs lost publications

### t17 — AWS package isolation and credential chain: depguard lint plus IRSA-ready session resolution

- instruction: Add depguard (or equivalent golangci-lint rule) forbidding AWS SDK imports outside internal/queue/sqs, internal/artifacts/s3, internal/runners/lambda — CI fails on violation. Implement the shared AWS session resolver in an aws-internal package following open-bedrock-server src/`open_bedrock_server`/utils/`config_loader.py` `get_aws_session` order: `AWS_ROLE_ARN` AssumeRole -> web-identity token file (IRSA) -> `AWS_PROFILE` -> explicit keys -> ambient chain; unit test per link with fakes; map credential/region errors to actionable environment-errors (ec2-cli ec2/aws/client.py `build_client`/`aws_call` split as the model).
- depends on: t13, t16
- covers: c17, h14
- acceptance:
  - A depguard-style CI lint forbids AWS SDK imports outside internal/queue/sqs, internal/artifacts/s3, internal/runners/lambda and fails on violation
  - Credential resolution follows the standard chain (AssumeRole, IRSA web-identity, profile, keys, ambient) with a unit test per link, modeled on open-bedrock-server's resolver

### t18 — Headspace local bridge: the dev-profile runner adapter driving the real CLI by subprocess

- instruction: Implement the headspace bridge as the dev-profile runner adapter driving the real headspace CLI by subprocess (no daemon exists by design): create/put/run/export/destroy with --json --provider docker; branch on the frozen additive 0-8 exit band (docs/specs/2026-07-28-honest-failure-taxonomy.md in headspace-cli); secrets via --env NAME with values only in the child env (never argv); cancellation = spawn 'headspace stop <ws> --apply' from a separate process while run holds the flock; share one `HEADSPACE_HOME` across invocations. Mirror Go structs from headspace/core/result.py (status/exit vocabularies are append-only). Integration tests drive the real CLI; skip when Docker is unavailable.
- depends on: t13
- covers: c11, h10
- acceptance:
  - Integration tests drive the real headspace CLI as a subprocess asserting on real exit codes (0-8 band) and --json payloads, including cancellation via a second-process stop --apply; secrets pass as --env NAME with values only in the child environment

### t19 — OpenAPI 3.1 API with SSE run events and the CLI/Web parity harness

- instruction: Author api/openapi/openapi.yaml (OpenAPI 3.1) first, then implement internal/api on net/http: workflows (draft/publish/validate), runs (create/inspect/cancel), ledger (projections/records/review), human tasks (approve/reject), actors registry, SSE stream of committed events only (§15.1 envelopes). Version group nodes.culture.dev/v1alpha1. Parity harness: enumerate Python-front verbs and web actions, assert each maps to a documented operation; fail on any bypass.
- depends on: t9, t8
- covers: c6, h6
- acceptance:
  - Every engine capability (validate, publish, run, inspect, ledger read/review, approvals, cancel) is a documented OpenAPI operation; SSE streams committed events only
  - A parity test enumerates CLI verbs and web actions and fails if any bypasses the public API

### t20 — Web front: React Flow Run and Ledger read-only views with progressive detail, a11y, and the agent-state test node

- instruction: Build web/ with Vite+React+TS: React Flow canvas using t5's culture-design layer (node frames per kind with the 7-color palette; dashed=proposed/solid=confirmed edges mapping ledger authority; ELK.js optional layout), TanStack Query on t19's API, SSE live overlays from committed events only. Progressive detail per prd-spec §8.5 zoom bands; read-only Run + Ledger views plus list/timeline alternatives; full keyboard nav, visible focus, reduced-motion (org global.css already ships the kill switch). Publish <script type=application/json id=agent-state> mirroring graph/run/selection state with stable ids/data-attributes; add the CI webglass job per webglass-cli docs/ci-recipe.md (declared-targets policy profile, extract #agent-state, console lens zero page errors) plus a Playwright keyboard-drive test for canvas interactions.
- depends on: t5, t19
- covers: c10, h9, c21, h18
- acceptance:
  - The Run view renders the 500-node reference graph with smooth pan/zoom; each zoom band shows only its declared fields; list and timeline alternatives exist; keyboard-only traversal and automated a11y checks pass; reduced motion is honored
  - The UI publishes a machine-readable agent-state JSON script element with stable ids/data-attributes; a CI webglass job (declared-targets policy profile) opens the built UI, asserts on the agent-state node, and checks the console lens for zero page errors

### t21 — Container release lane: multi-arch control-plane images by digest to ghcr, alongside the PyPI lane

- instruction: Add the container release lane: goreleaser or docker/build-push-action building linux/amd64+arm64 images of the one nodes binary (distroless base), pushed to ghcr.io/agentculture/culture-nodes by digest on tags; SBOM+provenance attestation if cheap. Leave publish.yml's PyPI lanes untouched. CI smoke consumes the published digest, not a local build.
- depends on: t1
- covers: c43, h38
- acceptance:
  - A tagged release publishes multi-arch images by digest; the existing PyPI Trusted-Publishing lane still ships the Python front; CI smoke tests consume published images, not local builds

### t22 — Helm chart with kind-cluster CI: per-role Deployments, probes, migration job, Ingress, replicas=2 safety

- instruction: Author deploy/helm: per-role Deployments (api/scheduler/worker) of the single image pinned by digest, values for replicas, liveness/readiness probes, preStop drain, PodDisruptionBudget, Secret-ref env config, migration Job as pre-install/pre-upgrade hook (t6's nodes migrate), Ingress for API+callbacks with the attempt-scoped token flow, optional MinIO/Postgres subcharts for non-production. Docs state the Phase-1 trust boundary plainly: authless behind a private network (c45). kind-cluster CI: install, worker replicas=2, run the fault suite in-cluster, manifest test proving no Docker socket mounts anywhere, external-network actor completes the async callback flow through the Ingress.
- depends on: t21, t6, t11, t19, t20
- covers: c23, h20, c26, h23, c4, h4, c42, h37, c7, h7
- acceptance:
  - helm install on a kind cluster passes CI smoke (API healthy, run executes, UI serves) with worker replicas=2 and the multi-pod fault tests green in that topology; migrations apply via a job before new pods receive traffic
  - Probes, preStop drain, PodDisruptionBudget, and Secret-ref config exist; no control-plane container mounts a Docker socket (manifest test); the chart contains exactly the control-plane Deployments plus Postgres; the authless-private-network trust boundary is stated in the chart docs
  - An external-network actor completes the async callback flow through the Ingress with an attempt-scoped token

### t23 — Docker compose local profile: complete local system in one command

- instruction: Author deploy/compose: nodes all + PostgreSQL + MinIO + the t18 headspace bridge as the local runner (separate service owning the Docker access; never mounted into nodes), UI embedded. docker compose up -> a full local run of the reference workflow. Manifest test: no docker.sock volume on any nodes service.
- depends on: t18, t19, t20, t21
- covers: c7, h7, c4, h4
- acceptance:
  - docker compose up starts nodes all + PostgreSQL + the local runner bridge, UI embedded; a run executes end to end locally; no service mounts the Docker socket into a control-plane container (manifest test)

### t24 — Python thin CLI front: the nodes CLI becomes an API client with zero engine logic

- instruction: Narrow `culture_nodes`/ to a thin API client front: every product verb (validate/run/inspect/ledger/etc.) calls t19's REST API via stdlib urllib — zero engine logic, zero third-party runtime deps (keep dependencies = \[\]); keep the agent-first surface (whoami/learn/explain/overview/doctor, --json, error:/hint:, exit 0/1/2) and keep teken cli doctor --strict green. Mesh-agent scaffold verbs stay. Config: `NODES_API_URL` env + culture.yaml fallback.
- depends on: t19
- covers: c13, h11
- acceptance:
  - Every product verb calls the public API only (no engine logic in Python); teken cli doctor --strict stays green; the zero-runtime-dependency constraint holds or the dependency change is recorded by ADR

### t25 — Devague conformance adapter: plan-waves and deliverables fixtures round-trip to ledger projections

- instruction: Build the devague conformance adapter as fixtures + a mapping package: check in devague plan waves --json and deliverables --json fixture files, map claims/tasks/waves to ledger record types and projections per prd-spec §9.11 (proposed/confirmed -> ledger authority), assert identical projection digests across runs. Add a lint that no devague code is imported (fixtures only). Never write runtime state back into devague.
- depends on: t8
- covers: c14, h12
- acceptance:
  - devague plan waves --json and deliverables --json fixtures map to ledger projections with identical digests across runs; no devague code is imported into Go packages (lint)

### t26 — Colleague reference bridge: an external adapter image implementing the actor protocol over colleague subprocess dispatch

- instruction: Build the colleague reference bridge as a separate deliverable under adapters/ (Python is fine — it runs on the agent host, outside the product): an HTTP service implementing the §13 actor protocol that dispatches colleague work --json/--background, captures `task_id`, polls .colleague/flight/<id>.feed.jsonl for heartbeat/progress, maps ok/error/incomplete to outcomes (incomplete is NEVER success), cancels via cooperative flight stop, pinned to colleague docs/contract.md v1. Prove it with t12's conformance kit against the mock engine; publish as its own image.
- depends on: t12
- covers: c15, h13
- acceptance:
  - The bridge passes the actor conformance suite while driving colleague's mock engine; incomplete maps to a non-success outcome (test); the bridge ships as a separate image, never in the control-plane binary; pinned to colleague contract v1

### t27 — Phase-1 vertical slice end-to-end: the reference workflow, restart survival, acceptance checklist, benchmarks

- instruction: Assemble the Phase-1 vertical slice: author the reference workflow (intake/plan/build/test/verify per the implementation issue) with fake-but-external HTTP actors and a code node on the dev runner; e2e test drives it through the `changes_required` loop, kills and restarts processes mid-run (state survives), asserts an agent completion claim stays unverified until acceptance signals pass, and checks the web Run view reflects committed transitions (webglass assertion on #agent-state). Produce docs/acceptance.md mapping every implementation-issue checkbox to its test/benchmark/artifact evidence; record idle-RSS and transitions/sec benchmarks; audit go.mod+package.json against the PRD stack table (deviations -> ADR).
- depends on: t14, t15, t20, t22, t23, t24, t26
- covers: c1, h1, c2, h2, c32, h27, c33, h28, c34, h29, c36, h31
- acceptance:
  - intake→plan→build→test→verify runs end to end with the `changes_required` loop following a graph edge; the run survives process restart; an agent completion claim stays unverified until acceptance checks pass; the live Run view shows committed transitions
  - Every implementation-issue acceptance box is demonstrated by a test, benchmark, or recorded artifact — a checked box without evidence is a defect; idle-memory and transition-throughput benchmarks are recorded
  - go.mod and package.json match the PRD stack table with any substitution recorded by ADR; each named audience has its working surface (web views, agent-first CLI+API, helm/compose)

## Risks

- [unknown_nonblocking] Lambda-dependent acceptance tests (h22, h36) need either a dedicated AWS test account or an emulator; LocalStack's Lambda+IAM fidelity for digest-pinned container functions is unverified — which lane CI uses is undecided (task t13)
- [unknown_nonblocking] Go + web CI stand-up reshapes tests.yml (golangci-lint, go test, vitest, kind, webglass) alongside the existing Python/Sonar gates — sequencing and coverage-gate interplay is build detail the first waves must settle (task t1)
- [follow_up] The pinned org design revision will drift from the live site; a re-sync policy for web/src/culture-design (when to re-pin, who approves) is a follow-up (task t5)
- [follow_up] claude-agent-sdk / claude-agent-api adapters are deferred (frame park v3): the actor conformance kit is their contract, but no concrete adapter task exists in this plan — follow-up plan once their surfaces are explored
- [unknown_nonblocking] Lambda's 15-minute cap may prove too small for real test suites, forcing a second runner adapter (ECS RunTask or headspace remote) earlier than planned — watch the first real workloads (task t13)
