# self-hosted phase-2 cycle

> Culture Nodes now runs its own development: the deferred approval surface landed, claude-code is a first-class actor from dev (spark) to production (thor+orin) sharing one Postgres, and operators watch runs through a cards board, a jobs timeline table, and a time-range filter on latest update.
> instruction: Verify by walking the success signal (c22) end to end on the thor+orin pair before claiming the cycle done

## Audience

- OSS audience: anyone may run Culture Nodes — hosting it on their own machine or their own cloud, plugging in their own runtimes and harnesses through the runner and actor contracts. The first concrete users are the AgentCulture mesh: Ori as owner/approver, claude-code (dev + production) and codex (test/support) sessions as working agents, and culture-nodes itself as the first workflow author and subject (self-hosting)

## Before → After

- After: A culture-nodes run drives culture-nodes development end to end: the delivery-loop workflow published via the API, a claude-code actor doing the work, thor+orin production workers on thor's shared Postgres, approval nodes pausing runs for human decision, and the operator watching it on a runs board, a cross-run jobs timeline, and a time-filtered runs list

## Why it matters

- Self-hosting is the credibility test: every PRD promise — contracts, evidence, approvals, placement-unaware execution — gets exercised by real daily development work, so gaps surface as our own pain instead of a user's

## Requirements

- Approval/human-task surface closes deviation d1 (issue #3): the engine creates `human_tasks` rows when dispatching approval nodes (table exists since migrations/0002 but nothing writes it), the API adds human-tasks list/get + POST /v1alpha1/human-tasks/{id}/decision, the worker registers a real HumanDispatcher in the seam t12 left unregistered, and the e2e walks the verify.blocked -> human-review branch — prerequisite for PRD §9.9
  - instruction: Engine writes `human_tasks` on approval-node dispatch; API adds GET /v1alpha1/human-tasks, GET /human-tasks/{id}, POST /human-tasks/{id}/decision; worker registers a real HumanDispatcher; extend tests/e2e with the human-review branch; close issue #3 and confirm deviation d1
  - honesty: An approval node's run pauses without holding a worker lease or a DB transaction, resumes on POST decision, and the decision lands as a human-authority review in the ledger — proven by an e2e that walks verify.blocked -> human-review -> build, not by endpoint unit tests alone
- A claude-code actor adapter (sibling of adapters/colleague) implements the provider-neutral actor protocol over the claude CLI so agent nodes bind to claude-code; it must pass the existing actor conformance kit (tests/conformance), like the colleague bridge did; thor already has /usr/local/bin/claude installed
  - instruction: Build adapters/claude-code as a sibling of adapters/colleague: contract-v1 subprocess dispatch driving headless claude, flight-file/status polling, Bearer callback auth; run the conformance kit against the live bridge in CI
  - honesty: The claude-code adapter passes the existing actor conformance kit unmodified — the kit as shipped, not a fork — and an incomplete or crashed claude session never maps to success (incomplete-never-success, as the colleague bridge proved)
- Self-hosting: culture-nodes develops itself — the repo's own delivery loop (examples/delivery-loop: intake→plan→build→test→verify with the `changes_required` loop) runs as a real culture-nodes run with claude-code as the agent actor on this machine, exercising internal/devague's conformance mapping of plan/delivery artifacts to ledger projections
  - instruction: Publish the delivery-loop (or a dev-loop variant) via the API, run it with the claude-code actor and the wrapped headspace runner, and keep the run's ledger as the evidence artifact for the delivery summary
  - honesty: The self-hosted run's work is real: the claude-code actor produces an actual committed diff in this repo through the run — not a demo payload — and internal/devague projections of the run match the plan artifacts
- Web operations surface: a clean cards-on-columns board view, a jobs timeline table, and a time-range filter keyed on each flow run's latest update time; runs already serialize `updated_at` (internal/api/types.go) but GET /v1alpha1/runs accepts only state+limit, so the filter needs store query + API params + OpenAPI + web UI together
  - instruction: Add `updated_since`/`updated_until` (+ explicit sort by `updated_at`) to GET /v1alpha1/runs and a cross-run node-runs listing endpoint; build the Board (runs-by-state columns) and Jobs timeline (node runs, newest first) routes in web/src/routes; document both in api/openapi
  - honesty: Board columns and timeline rows render only committed state read from the API — no client-side invention — and the time-range filter is server-side (store query + API params over runs.`updated_at`), never a client filter over an unbounded list
- CLI/Web/API parity holds for every new surface: human-task endpoints and new run-list params land in api/openapi + the Python thin CLI front (`culture_nodes`/, a pure API client with byte-exact --json passthrough) + web alike — 'parity by construction' is a standing delivery claim (medium confidence in docs/deliveries) and must not regress
  - instruction: Extend tests/parity to cover human-tasks endpoints and the new run-list params; the Python front exposes them as verbs/flags with byte-exact --json passthrough; keep teken cli doctor --strict green
  - honesty: Every new endpoint and query param appears in api/openapi, the Python CLI, and the web in the same release — the parity harness fails when one surface lags, rather than parity being re-asserted by hand
- Phase-1 delivery remainders beyond approvals land or get explicit ADRs: the two NOT-MET acceptance boxes (Markdown-from-JSON projection rendering; mechanical acceptance.requires evaluation, per docs/acceptance.md), the null run.output observed on the live smoke's end-node binding, and `waiting_external` deadline timers that today never fail attempts
  - instruction: Fix run.output and `waiting_external` timers with regression tests; implement the two acceptance not-mets or record an ADR saying why they wait; update docs/acceptance.md checkboxes only with evidence
  - honesty: Each phase-1 remainder lands with a test or an explicit ADR deferral — none is silently dropped: Markdown-from-JSON projection rendering, mechanical acceptance.requires evaluation, the null run.output end-node binding, and `waiting_external` deadline timers
- Custom runner support: code nodes can execute on the local machine, on a machine on the network (e.g. thor/orin), and through a custom runner solution that abstracts where execution happens — the operator picks the target without the workflow changing
  - instruction: Define the runner-service contract (wrap headspace behind it as the reference deployment), replace the ARN-shaped registry identity with a runner-neutral one (pinned digest + endpoint), deploy on spark and thor, and run the same digest on both
  - honesty: A workflow definition is placement-free: moving a code node from one machine to another requires zero workflow changes — only registry/deployment config differs — proven by running the same workflow digest against runners on two different machines
- The runtime stays lightweight at 100 and 1000 concurrent runners: execution is distributed, the runtime holds no execution in-process and no per-runner open connection, and it tracks runner status by periodic sampling (every x seconds) — tracking distributed runners is easy; hosting them is what bloats a pod
  - instruction: Add a load test that holds 100 concurrent in-flight operations against a stub runner service and asserts bounded worker RSS and sampling cadence; record results beside docs/benchmarks.md
  - honesty: With 100 concurrent in-flight runner operations the worker/control-plane memory stays bounded — no per-operation held connection or goroutine that scales with running work — and status-sampling load scales with runners/interval, not with operation duration; demonstrated at 100 with the 1000 case load-tested or extrapolated with stated method
- Runner-protocol operations ride the park/resume path, not lease-held Execute: today internal/worker/code.go:254 heartbeats the lease for the operation's whole duration (doc.go says agent nodes instead park as `waiting_external` and internal/actors.HandleCallback re-leases) — at 100 concurrent runner ops the lease-holding model contradicts c17, so protocol dispatch parks the work item and resumes on status-sample or callback; the `waiting_external` deadline-timer fix (c11) becomes a load-bearing prerequisite for runners, not cleanup
  - instruction: Extend the `waiting_external` park/resume path (internal/actors.HandleCallback is the precedent) to runner status ingest; decide q9 (sync fast-path) before wiring short-op dispatch
  - honesty: A ten-minute runner operation holds no lease and no goroutine between status samples; killing the worker mid-operation strands nothing — the parked item is picked up by the surviving worker's sampler after lease/park handoff, proven by a fault test
- Runner services authenticate their callers: a runner service accepting operations over the network is a remote-code-execution surface, so operations are accepted only from an authenticated control plane (secret-based this cycle per c9's boundary), the registry pins endpoint + digest + secret ref per runner, and an unauthenticated execute/status request is refused — precedent: t22's Ingress callback 401 and the operation schema's enforced-policy-boundary language
  - instruction: Bearer/HMAC secret per runner registration; registry schema carries endpoint + digest + secret ref; the runner conformance kit gains an auth-refusal case; secrets reach thor/orin via the c8 authorize pattern
  - honesty: An unauthenticated execute or status request against a runner service is refused with 401/403 — proven by a conformance-kit auth case run against the reference headspace-wrapped deployment, not asserted from config
- thor's Postgres gets a documented, exercised backup/restore path before the first production run is claimed: the ledger is the authoritative evidence store and thor is a single machine — losing its disk must not mean losing the ledger
  - instruction: Add a scheduled `pg_dump` (or WAL-based) backup job to thor's compose profile, write the restore runbook, and run one drill end to end
  - honesty: A restore drill actually ran: a backup taken from thor's production Postgres, restored on a different machine, reproduces a run's ledger with content digests intact — recorded as evidence before the after-state (h14) is claimed
- A codex actor adapter (adapters/codex) lands this cycle as a third conformant actor-protocol adapter alongside colleague and claude-code — provider-neutrality proven three-deep, with codex also serving as the second independent mind for testing/support (codex-cli 0.144.6 verified on dev)
  - instruction: Build adapters/codex as a sibling of adapters/colleague and adapters/claude-code: contract-v1 subprocess dispatch over headless codex exec, flight/status polling, Bearer callback auth; run the conformance kit against it in CI
  - honesty: The codex adapter passes the same actor conformance kit unmodified, and an incomplete or crashed codex session never maps to success — the same bar c3 sets for claude-code, no adapter-specific exemptions

## Honesty conditions

- The announcement only holds if all three legs land verifiably: the approval surface (issue #3 closed), claude-code as a conformant actor on both dev and production, and the three operations views live against real runs — a partial landing re-scopes the announcement rather than shipping it quietly
- No task in this cycle's plan requires AWS credentials for CI to pass; the awslive tag stays skipped; SQS/Lambda code paths stay compiled and fake-tested so they do not rot into dead code
- The already-delivered list is verified, not trusted: leases/fencing, multi-worker safety, the SQS driver, artifact store, and fault suites are green on main today, re-run as this cycle's baseline before any new work builds on them
- No code in this cycle reimplements culture, events-cli, or agenda functionality, and vendored .claude/skills script bodies are byte-identical before and after — git diff over the vendored paths proves it at PR time
- The OSS-audience claim is honest only if a stranger can stand the system up without this machine's context: compose up plus the docs suffice to run a workflow with their own runner/actor implementations — no AgentCulture-mesh prerequisite baked into the product path
- The after-state counts only when it happens on the real production pair — thor+orin workers on thor's Postgres — a dev-only demo on spark does not satisfy it
- Self-hosting pain gets recorded, not absorbed: friction found while dogfooding lands as issues, deviations, or ADRs — otherwise the credibility-test claim is theater
- The success signal is measured off the actual run's ledger — the run id, its ledger records, and the merged diff are citable in the delivery summary; nothing is reconstructed after the fact

## Success signals

- A real development task of this repo completes as a culture-nodes run: claude-code produced the actual diff, a human approved through the new decision endpoint, the run survived the approval pause without holding a worker, the board/timeline/filter views showed it live, and the ledger holds the proposed claim, observed evidence, and the human review

## Scope / boundaries

- AWS-facing Phase-2 items stay deferred: no AWS credentials exist on any machine, so the real-SQS lane, real-Lambda awslive verification (risk r1), and ECS/Fargate deployment are out of this cycle; the Postgres queue driver and MinIO remain the production signal/artifact paths; existing SQS/Lambda code stays tested against fakes only
  - instruction: Keep the fake-backed SQS/Lambda suites in the default test run; record the real-AWS lane as a `follow_up` park in the plan, not silent scope loss
- Phase-2 items already delivered in the phase-0/1 cycle are not redone: leases/fencing (t7), multi-worker safety (kind replicas=2), the SQS driver code (t16), S3/MinIO artifacts (t15), and the fault suites all landed per docs/deliveries; the genuine Phase-2 delta is OpenTelemetry (stub today), workload auth, load/benchmark gates (250/s sustained never demonstrated), and the thor+orin deployment story
  - instruction: Run go test ./... (including tests/fault) and the web/python suites on main as the cycle baseline; only then treat OTel, workload auth, load gates, and the thor+orin deployment as the genuine Phase-2 delta
- Lane boundaries from the build brief (issue #1) hold: no reimplementation of culture (substrate), events-cli (events), or agenda (tasks); vendored .claude/skills script bodies stay untouched per docs/skill-sources.md
  - instruction: Check at PR review: diff .claude/skills/ against main; any mesh-substrate need becomes an issue on the sibling repo per the build brief

## Assumptions

- Topology: this machine (spark) is dev; thor and orin are production, both running workers against one shared authoritative Postgres — two machines on the same DB imitate the k8s multi-pod topology with same/near-same config; deployment reuses deploy/compose per machine, no k8s/ECS required
- Credential distribution to production follows reachy-mini-cli PR #161's ssh pattern (reachy/discover/ssh.py): argv-only ssh plus a separate explicitly-confirmed ssh-copy-id authorize step, no typed secret through the tool; live probes show user@host accepts the current key (aarch64, claude + docker present) while user@host refuses it (publickey,password) — an authorize flow is a real prerequisite, not polish
- codex-cli is installed on this machine (verified: codex-cli 0.144.6 on PATH) and serves as a testing/support actor — a second, independent CLI backend that can exercise the actor protocol alongside claude-code, proving provider-neutrality with two real external actors
- Custom runners are distributed services behind a runner protocol (mirroring the actor protocol), never in-process: schemas/runner/\*.json — already runner-neutral — become the wire contract; the operation schema's refusal of check-policy-then-shell wrappers binds every implementation; the registry's Lambda/ARN-shaped FunctionIdentity needs a runner-neutral identity (pinned digest + endpoint); remote runners share c8's ssh/credential prerequisites
- The fleet runs three different claude CLI versions today (spark 2.1.226, orin 2.1.221, thor 2.1.220 — live probes): the claude-code adapter checks a minimum CLI version at startup and refuses dispatch with an honest DispatchError on incompatible versions, so headless-flag drift across machines can never silently alter behavior
- Completion ingest is idempotent across arrival paths: an optional callback and a status sample can both report the same completion (or two workers race a sample) — the fencing/idempotency discipline that guards actor callbacks (attempt-scoped tokens, re-lease under fencing) extends to runner status/callback ingest, making duplicate completion reports harmless
- runs.`updated_at` is unindexed today (grep migrations/: only `actor_invocations` got a (state, `updated_at`) index in 0009) — the server-side time-range filter and `updated_at` sort need a migration adding the index, under the existing expand-contract and N-1 compatibility discipline

## Scope exploration

- `s1` — `issue #3 + docs/deliveries/2026-08-08-culture-nodes-app-design.md (d1)`: t19 shipped runs/ledger/reviews/SSE with no approval endpoints; the deviation record enumerates the exact gaps: engine never writes `human_tasks` (table exists from migrations/0002), worker's HumanDispatcher seam is unregistered, e2e's verify.blocked branch is deferred
  - seeds: `c2`
- `s2` — `adapters/colleague + tests/conformance`: the colleague bridge is the reference external actor adapter (contract-v1 subprocess dispatch, flight-file polling, conformance kit PASS against the live bridge); a claude-code adapter follows the same proven shape
  - seeds: `c3`
- `s3` — `examples/delivery-loop + internal/devague`: the reference workflow (intake→plan→build→test→verify with both loop edges) and the devague conformance adapter (real 0.22.0 fixtures → deterministic ledger projections) are e2e-tested; self-hosting composes what exists rather than inventing a new workflow shape
  - seeds: `c4`
- `s4` — `api/openapi/openapi.json + internal/api/queries.go + web/src/routes/`: GET /v1alpha1/runs accepts only state+limit; runs already SELECT and serialize `updated_at`; web routes are RunsList/RunView/LedgerView with an EventTimeline component — no board view, no cross-run timeline, no time filter anywhere
  - seeds: `c5`
- `s5` — `culture_nodes/ Python front (t24)`: pure API client, zero deps, byte-exact --json passthrough, teken 26/26 strict — new endpoints and params must land here too or the parity delivery claim regresses
  - seeds: `c6`
- `s6` — `live ssh probes: user@host, user@host`: thor reachable with the current key: aarch64, /usr/local/bin/claude and docker present; orin refused (publickey,password) — the production pair is half-provisioned today
  - seeds: `c7`, `c8`
- `s7` — `../reachy-mini-cli PR #161 (reachy/discover/ssh.py)`: the cited credential pattern: argv-only ssh with HostKeyAlias, and key install isolated in a separate explicitly-confirmed ssh-copy-id authorize verb so no typed secret passes through the tool
  - seeds: `c8`
- `s8` — `docs/initial-design/culture-nodes-prd-spec.md §23 (Phase 2) vs docs/deliveries`: of the PRD's Phase-2 list, leases/fencing, multi-worker, SQS driver code, S3 artifacts, and fault suites already shipped in phase-0/1; OTel is a stub, workload auth absent, load gates unrecorded (250/s never demonstrated), ECS/Fargate unbuilt
  - seeds: `c10`
- `s9` — `deploy/aws + delivery risk r1`: the IAM template targets placeholder account 000000000000 and the awslive test tag is skipped without `NODES_TEST_LAMBDA_ARN`; no AWS credentials exist on any machine, so real-AWS verification cannot run this cycle
  - seeds: `c9`
- `s10` — `docs/acceptance.md + delivery follow-ups`: 41 met / 5 partial / 2 not met; the not-mets are Markdown-from-JSON projection rendering and mechanical acceptance.requires evaluation; run.output was observed null on the live smoke and `waiting_external` deadline timers never fail attempts
  - seeds: `c11`
- `s11` — `issue #1 build brief`: the brief's lane boundaries are explicit: culture is the substrate (do not reimplement), events-cli owns events, agenda owns tasks; devague is the closest existing graph model and the delivered product integrates rather than absorbs it
  - seeds: `c12`
- `s12` — `deploy/compose`: a complete local system in one command exists (postgres/minio/migrate/api/scheduler/worker + opt-in colleague-bridge) with manifest tests proving no docker.sock; per-machine compose is the natural thor+orin packaging
  - seeds: `c7`
- `s13` — `local CLI probe: which codex / which claude`: codex-cli 0.144.6 and claude 2.1.226 are both on PATH on this machine — two independent agent CLIs available as actors in dev
  - seeds: `c13`
- `s14` — `internal/runners/{runner.go,registry.go} + schemas/runner/`: the Runner interface is one typed Execute method with no shell escape hatch and an honest DispatchError contract; operation/result schemas are runner-neutral by design and the operation schema explicitly refuses check-policy-then-shell wrappers; the registry's FunctionIdentity validates Lambda ARNs only, so network/custom runners need a runner-neutral registry identity
  - seeds: `c14`, `c15`
- `s15` — `live ssh re-probe: user@host (key installed mid-frame)`: orin reachable: aarch64, /usr/bin/docker present, no claude CLI, 52/61 GiB memory already used — worker/runner capable today; claude-code actor hosting needs install + memory headroom
  - seeds: `c7`
- `s16` — `corrected probe: user@host via login shell`: claude 2.1.221 IS installed on orin (~/.local/bin/claude) — the earlier 'no claude CLI' finding was a non-login-shell PATH artifact, user-verified and re-probed with bash -lc; orin is actor-capable as well as worker-capable, memory headroom (~8 GiB free) remains the real constraint
  - seeds: `c7`
- `s17` — `challenge pass / adjacent-systems lens: internal/worker/{code.go,doc.go,seams.go}`: CodeRunner.Execute holds and heartbeats the lease for the operation's whole duration while agent nodes park as `waiting_external` and resume via HandleCallback re-lease — the async runner protocol must adopt the park path or contradict c17
  - seeds: `c24`
- `s18` — `challenge pass / security lens: schemas/runner/operation.schema.json + t22 deploy NOTES`: the operation schema demands an enforced policy boundary and t22 shipped an authless-trust-boundary warning with a 401-tested callback Ingress; nothing yet requires auth on a runner service's own endpoints — seeded the runner-auth requirement
  - seeds: `c25`
- `s19` — `challenge pass / operations+recovery lens: thor as production DB host`: single-machine Postgres holding the authoritative ledger; no backup/restore claim existed anywhere in the frame — seeded the backup requirement
  - seeds: `c26`
- `s20` — `challenge pass / lifecycle lens: claude CLI versions across the fleet (live probes)`: spark 2.1.226, orin 2.1.221, thor 2.1.220 — three versions live today; seeded the min-version-check assumption
  - seeds: `c27`
- `s21` — `challenge pass / concurrency lens: internal/actors callback fencing`: attempt-scoped tokens + re-lease under fencing guard actor callbacks; runner completion can arrive by sample, callback, or both — seeded the idempotent-ingest assumption
  - seeds: `c28`
- `s22` — `challenge pass / store lens: migrations/ index audit`: only `actor_invocations` carries a (state, `updated_at`) index (0009); runs does not — seeded the index-migration assumption
  - seeds: `c29`
- `s23` — `challenge pass / adjacent-systems lens: orin->thor LAN`: clean: ping orin->thor succeeds, so the two-worker-one-DB topology has network reach; TLS/ports for Postgres and the API remain deployment detail
- `s24` — `challenge pass / reversibility+migration lens: migrations N-1 harness + image digests`: clean: expand-contract discipline with the N-1 binary-compatibility harness (t6) and digest-pinned images (t21) give downgrade/rollback paths; no new claim needed
- `s25` — `challenge pass / failure-modes lens: self-hosting recursion`: restart survival (t9/t27) covers control-plane restarts mid-run; the residual — a broken build deployed by its own run — is parked as v6, mitigated by dev-first staging discipline

## Decisions

- Placement-unaware runtime: step/agent work always executes as an external process reached over the runner/actor boundary, possibly on another machine; the control-plane/worker pod stays lightweight and never assumes work runs locally — 'local machine' and 'machine on network' are deployments of one abstraction, not distinct runner kinds
- headspace is the reference runtime-on-another-machine: wrapped with the right contract it holds its own run status and shares it over the status surface — a runner-protocol deployment like any other, not a dev-only exception inside the worker
- Runner status model is hybrid with clear ownership: polling is the runtime's responsibility — the worker samples runner status every x seconds and the system is fully correct on polling alone; completion callbacks are the runner's side and strictly optional — a runner that never calls back still works, callbacks only tighten completion latency. The runner-service contract documents the callback as optional; no deployment is required to provide a callback route

## Open parks

- [unknown_nonblocking] Exact secret-distribution mechanism for non-SSH credentials on thor/orin (DB password, HMAC callback secrets, `ANTHROPIC_API_KEY` for claude-code): OS keyring vs env files vs compose secrets — the reachy pattern covers SSH key install only
- [unknown_nonblocking] Runaway-cost containment for self-hosted claude actors: bounded loops (max visits) and attempt timeouts are the only brakes; PRD parks cost budgets to Phase 4 — a buggy loop burning real API spend surfaces only through the board/timeline
- [unknown_nonblocking] Self-hosting recursion: restart survival covers a control-plane restart mid-run, but a broken control-plane build deployed by the very run that built it bricks the loop — dev-first staging discipline is process, not enforcement
- [follow_up] OpenTelemetry beyond the stub: traces/metrics for engine transitions, runner dispatch, and actor callbacks — parked to a follow-up cycle (issue to open)
- [follow_up] OIDC / workload authentication for API and runner/actor callbacks — parked until a cloud lane exists (issue to open); thor+orin deployment uses the existing secret-based auth

## Resolved vagueness

- [unknown_nonblocking] orin's actual account layout and tooling: the probe used user@host and was refused at auth; whether claude/docker exist there is unverifiable until a key lands — resolved: ssh user@host now works (key landed mid-frame): aarch64, docker present, claude CLI NOT installed, and memory is tight — 52 of 61 GiB used, ~8 GiB available — so orin runs a worker/runner fine but hosting claude-code actor sessions there needs an install and a memory budget check
