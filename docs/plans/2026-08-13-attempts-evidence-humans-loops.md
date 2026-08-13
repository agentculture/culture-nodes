# Build Plan — attempts-evidence-humans-loops

slug: `attempts-evidence-humans-loops` · status: `exported` · from frame: `attempts-evidence-humans-loops`

> Culture Nodes accounts for every attempt and orchestrates humans, loops, and itself: failed attempts carry actor attribution and usage; evidence bindings resolve real run evidence and bridge-measured diffs; nodes pre-announce success criteria that are mechanically checked; humans execute nodes through the same lifecycle as agents; ad-hoc runs are first-class; and the system proves it live with a PR-upkeep flow sweeping Qodo and SonarCloud findings on this repo

## Tasks

### t1 — Pre-build citation re-verification: spot-check every file:line the spec's scope entries s1-s14 cite still matches reality; document any drift before build tasks merge

- instruction: Read docs/specs/2026-08-13-attempts-evidence-humans-loops.md Scope exploration section; for each s1-s14 entry open the cited files and verify the described state at the cited lines; write the checklist to the task branch as a markdown note; flag drift to the operator before wave 0 merges
- covers: c22, h18
- acceptance:
  - A checklist run over all 14 scope entries confirms each cited file:line still shows the described state, or records the drift and its impact on the affected task

### t2 — Issue 40: thread actor attribution through worker failure completions — ActorRowID on DispatchContext populated post-Resolve, failAttempt/completeTechnicalFailure carry it, code-path failures stamp codeRunnerActorID

- instruction: Add ActorRowID string to DispatchContext (internal/worker/seams.go:49); populate at internal/worker/dispatch.go:92 after Registry.Resolve via w.actorRowID; change failAttempt/completeTechnicalFailure (worker.go:474,491) to accept the context or an actor id and set CompletionRequest.ActorID; sites: dispatch.go:115,216,236,308,336, hooks.go:359 have it in scope; budget.go:91, dispatch.go:56,83, hooks.go:304 fire pre-resolution and stay NULL; code.go:238-341 + runnerasync.go:234 stamp w.codeRunnerActorID() like code.go:314 does; attempts.`actor_id` exists since migration 0002 — no migration
- covers: c2, h2
- acceptance:
  - A forced agent-node dispatch failure persists an attempt row with the resolved `actor_id` and GET /v1alpha1/actors/{id}/stats counts it in retry burn
  - Failure sites that fire before actor resolution persist NULL `actor_id` (no guessed attribution) — covered by a test on a pre-resolution refusal
  - Code and runner-service failure completions carry the code-runner actor id exactly like their success paths

### t3 — Issue 32 Go side: optional usage field on FailedPayload and persistence in the callback EventFailed branch; record the PRD 13.2 amendment as an ADR

- instruction: Add Usage \*Usage json:usage to FailedPayload (internal/actors/protocol.go:250); in completionFor's EventFailed branch (internal/actors/callback.go:692-703) set req.Usage = payload.Usage.ToEngine(); ADR in docs/adr/ amending PRD 13.2 (payload shapes are protocol.go's construction, PRD defines no per-kind payload schema); persistence via engine/complete.go:299 is already generic
- covers: c3
- acceptance:
  - A callback test posting a failed event with usage persists real token counts on the attempt row
  - A failed event without usage persists NULL — never zeros
  - ADR documents the 13.2 amendment (usage on the failed event) and conformance fixtures compile against it

### t4 — Issue 32 bridges: all three bridges (claude-code, codex, colleague) emit usage on the non-domain failure branch when a terminal result object exists

- instruction: In each bridge's mapping.py non-domain failure branch (claude mapping.py:382-391, codex :365-375, colleague :340-370 area) attach usage from the terminal result: claude `usage_from_result` (mapping.py:197), codex `usage_from_task_result` (:182), colleague (:174); the CLIs already return usage on error/incomplete (codex `codex_cli.py`:197,208; colleague `colleague_cli.py`:157); do NOT touch the result-is-None timeout/crash branches; all three bridges plus fixtures in one change
- depends on: t3
- covers: c3, h3
- acceptance:
  - Per-bridge tests: an error/incomplete terminal result yields a failed event carrying that result's real token counts
  - Crash and timeout branches (result is None) emit no usage key — asserted per bridge
  - All three bridges change together (all-backends rule) with matching fixtures

### t5 — Issue 32 sync path: bridge 500 error body carries usage when a result exists; completeFromInvocationError persists it; docs state the honest narrowing (cancelled and no-result attempts stay unreported)

- instruction: Bridge 500 error bodies ({error, class, `workspace_measured`}) gain usage when a result exists; parse in completeFromInvocationError (internal/worker/dispatch.go:300-316) and thread into completeTechnicalFailure's CompletionRequest; document the narrowing (cancelled + result-less attempts unreported) in the usage docs near the 0012 sentinel note
- depends on: t2, t4
- covers: c4, h4
- acceptance:
  - A sync bridge failure with a parseable result persists usage on the failed attempt (worker test + one bridge test)
  - The h24 narrowing — cancelled attempts and result-less crashes unreported — is stated in the usage docs, not left implicit

### t6 — Issue 34: EvidenceForSubject empty subject means all live run evidence; reconcile the two doc comments; empty-subject unit subtest; `bindings_test` stub projects real records; e2e asserts delivery-loop verify receives non-empty testEvidence

- instruction: internal/ledger/projection.go:353-370: make ref==empty mean all live evidence records (keep non-empty semantics untouched); fix the false doc comments at projection.go:353-355 and internal/worker/bindings.go:186-190; add empty-subject subtest to TestEvidenceForSubject (`projection_test.go`:207); upgrade the always-empty Projection stub (`bindings_test.go`:18-37) to project real records; e2e assertion in tests/e2e (delivery-loop verify testEvidence non-empty after test node appends evidence)
- covers: c5, h5
- acceptance:
  - TestEvidenceForSubject gains an empty-subject subtest returning all live evidence records
  - The bindings-resolution test resolves /ledger/projections/evidence to actually-appended records (stub upgraded from always-empty)
  - An e2e test proves the delivery-loop verify node's testEvidence binding is non-empty after the test node appends evidence

### t7 — Issue 33b: node-run-scoped evidence selector (join `node_runs` on `run_id`+`node_key`) and /nodes/<id>/evidence resolution in both worker and engine resolvers; compiler-vs-resolver surface agreement test

- instruction: Evidence identity is NodeRunID (node evidence carries no SubjectRef — nothing sets it); selector joins `node_runs` on `run_id`+`node_key` (pattern: internal/store/postgres/async.go:664 NodeOutput; `node_run_id` stored per internal/store/postgres/`ledger_store.go`:74); resolve /nodes/<id>/evidence in internal/worker/bindings.go:133-137 and internal/engine/binding.go:45; flip `bindings_test.go`:127; agreement test over internal/compiler/contract.go:14-31 surface list
- depends on: t6
- covers: c8, h25
- acceptance:
  - A downstream node binding /nodes/<id>/evidence receives exactly that node's evidence records (worker test over real rows)
  - TestUnresolvableBindingsFailLoudly expectation flipped; a new test asserts every compiler-accepted binding surface resolves or is compiler-rejected
  - The engine OutputFrom resolver accepts the same surface set as the worker resolver

### t8 — Issue 33a: typed `workspace_measured` field on InvocationResult, CompletedPayload, FailedPayload; folded into the node's persisted output so downstream nodes bind it via /nodes/<id>/output; conformance fixtures round-trip it for all three backends

- instruction: Typed field on InvocationResult (internal/actors/protocol.go:124), CompletedPayload (:238), FailedPayload (:250); fold into node output at completion (worker dispatch completeFromResult + callback completionFor) so /nodes/<id>/output carries it; bridges already attach it on every branch (workspace.py:129-175 shape: measured, repo, branch, `head_before`/after, `status_porcelain`, `changed_files`, diffstat); unmeasured shape is workspace.py:110-127; fixtures in tests/conformance + tests/runnerconformance; web/src/domain/evidence.ts:13-25 documents the anticipated diffstat
- depends on: t3
- covers: c7, h6
- acceptance:
  - A completed invocation carrying `workspace_measured` persists it inside the node output and a downstream binding receives it
  - The unmeasured case round-trips as measured:false — never as an empty diff
  - Runner-conformance fixtures for all three backends carry the block; authority stays actor-reported (no observed evidence written)

### t9 — Issue 39 wait foundation: production WaitDispatcher wired via Options.Waiter; until.duration and until.timestamp park durably on the existing wait timer and resume through planTransition with bounds enforced

- instruction: Implement WaitDispatcher (seam at internal/worker/seams.go:159, called worker.go:394-399, refusal today dispatch.go:514); wait timer kind already exists (internal/store/postgres/timers.go:44-66) with scheduler effects at internal/scheduler/scheduler.go:471-520; resume re-enters planTransition (internal/engine/transition.go:65) with checkBounds (:115); until shapes compile per internal/compiler/model.go:235-239
- depends on: t2
- covers: c10
- acceptance:
  - A wait node with until.duration parks its run (no lease held) and the scheduler resumes it after the timer fires, in a store-backed test
  - Loop bounds (maxTransitions, maxVisitsPerNode, maxDuration) apply across the resume
  - An undelivered wait leaves the run parked and inspectable, never wedged

### t10 — Issue 39 event surface (event-first per c35): first-class emit/subscribe event records, authenticated inbound event delivery route, until.signal subscribes to an event and resumes; schema forward-compatible with multi-token pickup and mid-execution emission

- instruction: Event-first per spec decision c35: events table migration (expand-only, ADR 0002 policy), emit+subscribe records, authenticated inbound route following requireDecisionAuth pattern (internal/api/humantasks.go:94-123); until.signal subscribes by event name; schema fields emitter/name/payload/subscription must not preclude multi-token pickup or mid-execution emission (issue 43 forward-compat); e2e park-and-resume via authenticated POST
- depends on: t9
- covers: c10, h8
- acceptance:
  - An e2e test parks a run on until.signal and resumes it via an authenticated POST delivering the event
  - An unauthenticated delivery is refused (negative test)
  - The event record schema carries emitter, name, payload, and subscription matching such that a future multi-token consumer needs no schema change (reviewed against decision c35 in the spec)

### t11 — Issue 38 lifecycle: human-timescale deadlines authorable per node; long-park proven against deadline timers and the dispatch budget without actor-kind branching

- instruction: Deadline comes from the async wait bound (internal/store/postgres/async.go deadline timer) and dispatch budget (internal/worker/budget.go, 3/work-item from engine hardening); make the bound authorable per node (compiler schema timeout/deadline field if absent); time-manipulated store test parks a human-actor attempt simulated multi-day; neutrality: config-driven, zero actor-kind reads
- depends on: t9
- covers: c28, h23
- acceptance:
  - A time-manipulated test parks a human-actor attempt for a simulated multi-day span with an authored long deadline: no timeout, no retry, no budget exhaustion
  - No engine or worker code branches on actor kind to achieve it (grep-backed assertion in the test)

### t12 — Issue 38a: human-inbox bridge adapter speaking the section-13 protocol (202 accept, durable pending task, authenticated callback with the human's submission) plus a kind=human actor registration lane

- instruction: New adapter dir (adapters/human-inbox/) mirroring the three bridge layouts (server.py, mapping.py, `async_runner.py`, callbacks.py); speaks section-13: 202 AsyncAccepted, durable pending store (sqlite or file), authenticated callback on submission; registration lane extends deploy/prod/register-actor.sh or docs pointing at t13's API with kind=human; e2e with a stub submission; internal/actors/`neutrality_test.go` untouched
- covers: c11, h9
- acceptance:
  - An agent-kind node dispatched to a registered kind=human actor parks async without a lease and completes through the standard callback when the human submits
  - internal/actors/`neutrality_test.go` is unmodified and green
  - The bridge persists pending tasks across restart (durable inbox, not in-memory)

### t13 — Actor registration API: authenticated POST /v1alpha1/actors creating append-only actor revisions, replacing the raw-SQL-only lane

- instruction: POST /v1alpha1/actors in internal/api/actors.go (which currently documents no-registration at :49-53); bearer-token auth via the requireDecisionAuth pattern with its own env secret; INSERT follows deploy/prod/register-actor.sh:147 semantics — append-only revisions (actors table, migrations/0001:39-55); negative tests; route registration in server.go (expect trivial merge with t19)
- covers: c12, c27
- acceptance:
  - A registration request with the bearer token creates a new actor revision readable via GET /v1alpha1/actors
  - An unauthenticated or wrong-token request is refused (negative test)
  - Registering an existing `actor_key` appends a revision, never updates in place

### t14 — Issue 38b: web inbox surface — list pending human tasks, submit a decision/result with token auth; waiting-on-human runs actionable from the browser

- instruction: web/src: new route (inbox) listing GET /v1alpha1/human-tasks?status=pending with submit posting the decision; extend web/src/api/client.ts (today only two POSTs at :138); reuse AuthorityChip/waiting visual vocabulary (web/src/components, domain/run-state.ts); token entry per r1 risk — settle per-user vs shared secret here; component test against stub API (pattern: web/src/routes/Workflows.test.tsx)
- depends on: t12, t13
- covers: c12, h10
- acceptance:
  - A pending human task renders in the web inbox and a submission through the UI completes it (Playwright or component test against a stub API)
  - The submission path reuses the existing bearer-token auth; no unauthenticated mutation from the browser

### t15 — Auth hardening gate: negative tests proving every mutating route added by this batch (event delivery, actor registration, human submission) refuses unauthenticated requests; read-only surfaces stay authless

- instruction: Table-driven Go test enumerating the batch's new mutating routes (event delivery from t10, actor registration from t13, any new submission route from t12/t14 server-side) asserting refusal without token; assert list/get surfaces stay authless; place in internal/api
- depends on: t10, t13, t14
- covers: c27, h22
- acceptance:
  - A table-driven test enumerates the batch's new mutating routes and asserts 401/403 without the token
  - The authless read-only posture is asserted unchanged for list/get surfaces

### t16 — Issue 37 schema: enforce policy on acceptance.requires (route-as-technical-status, route-as-domain-outcome, or observe-only) in the workflow schema and compiler, with publish-time validation

- instruction: Schema: enforce field on nodeAcceptance (schemas/workflow/workflow.schema.json:636-660) with values `route_technical`|`route_outcome`:<name>|observe (default observe); compiler validation in internal/compiler/ledger.go:59-76 area — `route_outcome` must name a declared contract outcome (model.go:23-39); publish-time error otherwise
- covers: c13
- acceptance:
  - A workflow declaring each enforce mode compiles; an enforce mode naming an undeclared domain outcome is a compile error
  - Schema docs state the default (observe-only, today's behavior) so existing workflows are unaffected

### t17 — Issue 37 evaluation and routing: acceptance checks evaluated on agent and code nodes before routing per the declared enforce policy; retry composition explicit; evaluations land as derived-authority records

- instruction: Wire evaluation pre-routing in internal/worker (today post-hoc code-success-only at internal/worker/acceptance.go:47-88, never routing per :29-33); evaluator internal/runners/acceptance.go (2 of 9 kinds evaluable — extend where mechanical); `route_technical` yields `contract_rejected` composing with failOrRetry (internal/engine/complete.go:382) — retry semantics explicit in the enforce config; `route_outcome` follows planTransition edges; derived records via OriginValidator/AuthorityDerived like worker/acceptance.go:76-80
- depends on: t16, t2
- covers: c13, h11, c29, h24
- acceptance:
  - A node failing its pre-announced check under route-technical-status completes `contract_rejected` and follows the declared retry policy — no silent unbounded paid retries (test both retry-on and retry-off)
  - Under route-domain-outcome the run follows the declared edge (loop-back tested); under observe-only routing is unchanged
  - Every evaluation appends a derived-authority record; no undeclared outcome is ever invented

### t18 — Issue 37 success signals: evaluator for mechanical `success_signal` records; non-mechanical signals honestly stay unevaluated

- instruction: Evaluator over `success_signal` records (schemas/ledger/`success_signal`.schema.json: statement, check.kind, mechanical bool); mechanical:true evaluated through the validator origin producing derived records; mechanical:false rendered not-machine-checkable; vocabulary internal/compiler/vocabulary.go:69
- depends on: t16
- covers: c13
- acceptance:
  - A run proposing a mechanical `success_signal` gets a derived evaluation record; a mechanical:false signal gets none and renders as not-machine-checkable
  - Evaluator runs through the validator origin, never promoting the proposal itself

### t19 — Issue 36: first-class ad-hoc runs — API endpoint renders a canonical one-node workflow from an instruction, publishes idempotently by digest, creates the run; Go nodes run verb implements the same lane

- instruction: Mirror .claude/skills/nodes-operator templates/assign.workflow.yaml render semantics in Go: instruction -> canonical one-node workflow -> contracts digest publish (idempotent) -> CreateRun; API handler in new internal/api file + route in server.go (expect trivial merge with t13); implement cmd/nodes/modes.go:18-29 run verb against the API; run input {instruction, repo, sandbox, `success_outcome`}
- covers: c14, h12
- acceptance:
  - One API call (and one CLI invocation) takes an instruction and yields a normal pinned-digest run; identical instruction re-renders to the same digest (idempotent publish)
  - The run is indistinguishable from a hand-published one in ledger and run APIs — no publish-semantics bypass
  - The nodes run stub is replaced; nodes run --help documents the lane

### t20 — Invariant gates: post-merge sweep proving provider neutrality and the authority ladder survived the batch

- instruction: CI-runnable sweep: git diff of internal/actors/`neutrality_test.go` vs batch base is empty and test green; grep internal/{worker,engine} for OriginRunner/AuthorityObserved writes outside the runner boundary and for AuthorityConfirmed writers outside ledger.CommitReview; codify as a test or make-target the delivery summary cites
- depends on: t12, t17
- covers: c16, h14, c17, h15
- acceptance:
  - internal/actors/`neutrality_test.go` unmodified since the batch's base commit and green
  - A grep/test sweep shows no observed-authority writes from agent paths and CommitReview as the only confirmed-authority writer

### t21 — PR-upkeep workflow (culture-nodes only): sweep code nodes for SonarCloud unresolved issues and PR Qodo findings, triage, fix on the spark claude-code bridge actor, independent codex review, human-approves-PR gate looping back, human-prepares-next-item human node; external driver script calling POST /v1alpha1/runs

- instruction: examples/pr-upkeep/: workflow.yaml + sweep code nodes (SonarCloud issues API componentKeys=`agentculture_culture`-nodes resolved=false; Qodo PR-comment extractor unit-tested on recorded fixtures from PR 35/42); fix node uses actor company/developer (spark claude-code bridge, probe-verified registered); review node on codex-thor READ-ONLY (r4/issue 18); approval node human-approves-PR with approved looping to sweep and `changes_required` to fix; human-prepares-next-item as kind=human actor node (t12); driver script POSTs /v1alpha1/runs (external, per spec decision); repo hard-coded culture-nodes (c26)
- depends on: t6, t7, t12, t17
- covers: c15, c26, h21
- acceptance:
  - The workflow validates and publishes; its sweep configuration names culture-nodes and nothing else (grep-backed)
  - Sweep extractors deterministically parse the SonarCloud issues API and Qodo comment structure into a work-item list, unit-tested against recorded fixtures
  - The human gate's approved outcome loops back to the sweep; `changes_required` routes back to fix; the flow holds zero merge credentials

### t22 — Live run: drive the PR-upkeep loop on the real repo (four standing Sonar items), human gates and grades exercised, and execute all eight c24 success-signal checks recording each outcome

- instruction: Preflight per r2/r3: bridge health on 192.168.1.157:8085-8089, repo allowlists, operator billable confirm; drive against the four standing Sonar items (BLOCKER python:S3516 `culture_nodes`/cli/`_commands`/`node_runs.py`:42, 2x CRITICAL go:S3776 tests/deploy, MINOR godre:S8193 `registeractor_test.go`:187); execute the eight c24 signal checks recording outcomes; attach live-harm evidence (codex-orin run 01KZW2XDR7YD2GER787QZ0K67M; pre-fix empty testEvidence repro); grade every assigned run; demote unrunnable signals to follow-up issues
- depends on: t21, t5, t8, t10, t11, t15, t18, t19, t20
- covers: c1, h1, c15, h13, c23, h19, c24, h20
- acceptance:
  - At least one live sweep completes with a human-gated merged fix; failed or empty sweeps render honestly
  - All eight c24 signals executed with recorded outcomes; any unrunnable signal demoted to an explicit follow-up issue
  - Live-harm evidence attached: the 2026-08-13 codex-orin failed-run id for attribution/usage, and a pre-fix empty testEvidence repro
  - Every assigned run gets a grade record (dogfooding assessment reflex)

### t23 — Delivery closeout: summarize-delivery artifact with the audience-to-surface map and the after-state-to-requirement-and-signal cross-map; stats-epoch note for issue 28 consumers

- instruction: Run /summarize-delivery; artifact includes audience-to-surface map (c20), after-state clause cross-map to requirements+signals (c21), attribution-epoch note for issue 28 consumers, h24 narrowing statement; lands in docs/deliveries/ like docs/deliveries/2026-08-12-operate-through-the-ui.md
- depends on: t22
- covers: c20, h16, c21, h17
- acceptance:
  - The delivery summary maps each audience to its shipped surface and each after-state clause to a confirmed requirement plus an executed c24 signal; orphans are cut or demoted explicitly
  - The attribution-epoch note (pre-fix NULLs are unattributed-era, not zero burn) lands in the stats docs

## Risks

- [unknown_nonblocking] Web-inbox auth mechanics (how the browser holds and presents the token; per-user vs shared secret) — settled inside t14, parked from the spec (v3) (task t14)
- [unknown_nonblocking] Spark phase-2 bridge services (developer/intake/planner/verifier) health and repo allowlists unverified — t22 preflight checks them before the live run (spec park v4) (task t22)
- [unknown_nonblocking] Live runs in t22 are billable (claude-code and codex sessions); operator confirms budget per the nodes-operator guard before each drive cycle (task t22)
- [unknown_nonblocking] Issue 18 (bwrap) keeps codex sessions analysis-only; t21's review node must stay read-only on codex or the live run blocks — fix work stays on the spark claude-code actor (task t21)
- [follow_up] Deferred by design: parallel tokens/split-join and event-driven continuation engine work (issue 43), human-node tracker transport (issue 41) — excluded from every task here
