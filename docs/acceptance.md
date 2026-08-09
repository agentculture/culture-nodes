# Phase 0/1 acceptance evidence

This document maps every checkbox in
`docs/initial-design/culture-nodes-implementation-issue.md` to the thing that
proves it — a test, a benchmark, a file — or records honestly that it is **not
met**.

The rule this document is written under: **a box is not checked without named
evidence.** "It looks implemented" is not evidence. Where something is
partially delivered, it is listed as partial with the part that is missing
spelled out, not rounded up.

- **Date:** 2026-08-09
- **Task:** t27 (Phase-1 vertical slice: assemble and prove)
- **Commands the evidence below was produced by:**
  - `go test ./...` (the whole suite, including `tests/e2e`)
  - `go test ./tests/e2e/ -bench . -benchtime 300x`
  - `scripts/idle-rss.sh`
  - `uv run nodes validate examples/delivery-loop/workflow.yaml`

**Summary: 42 met, 6 partial, 0 not met.**

## The reference run, in one paragraph

`tests/e2e/slice_test.go`'s `TestPhase1VerticalSlice` publishes
`examples/delivery-loop/workflow.yaml` through the HTTP API, creates a run
through the HTTP API, and lets a real worker and a real scheduler drive it
against a real PostgreSQL: intake → plan → build → test (a code node through
the runner boundary) → verify → `changes_required` → build → test → verify →
`passed` → finish. After build's first pass the entire control plane is torn
down — worker, scheduler, API server, and the database pool itself — and a
brand-new one finishes the run from PostgreSQL alone.
`tests/e2e/live_test.go` runs the same workflow with the code node dispatched
to the real headspace-cli Docker bridge.

---

## Milestone 0 — Contracts and compiler

| Box | Verdict | Evidence |
| --- | --- | --- |
| Workflow YAML/JSON schema | **met** | `schemas/workflow/workflow.schema.json`; `internal/contracts/validate_test.go`'s `TestAllEmbeddedSchemasCompile`, `TestValidWorkflowFixture` |
| Normalized canonical JSON IR and digest | **met** | `internal/compiler/normalize.go`, `internal/contracts/canonical.go`; `TestCompileIsDeterministic`, `TestCanonicalJSONGolden`, `TestDigestValueIgnoresKeyOrder`, `TestCanonicalJSONStableAcrossRepeatedRuns` |
| Node, edge, owner, actor, runner, contract, and policy validation | **met** | `internal/compiler/{graph,owners,contract,policy,ledger}.go`; `TestDeliberateErrorFixtures` drives one fixture per diagnostic class in `internal/compiler/testdata/err-*.workflow.yaml` |
| JSON Schema 2020-12 contracts | **met** | `github.com/santhosh-tekuri/jsonschema/v6`; every file under `schemas/` declares `draft/2020-12`; `TestAllEmbeddedSchemasCompile` |
| CEL conditions | **met** | `internal/compiler/cel.go` (compile time), `internal/engine/workflow.go` (rebuild at run time); `TestCompiledCELPrograms`, `TestPlanTransitionFirstMatchingGuardWins` |
| Ledger record schemas and producer/authority matrix | **met** | `schemas/ledger/*.schema.json`, `internal/ledger/authority.go`; `TestAppendEnforcesProducerAuthorityMatrix`, `TestValidLedgerRecordsCoverEveryType`, `TestCheckAuthorityMatchesAppend` |
| Deterministic ledger projections | **met** | `internal/ledger/projection.go`; `TestProjectionDigestIgnoresStorageOrder`, `TestProjectionDigestSurvivesSerialisation`, `TestPropertyProjectionDigestsIgnoreStorageOrder`; and `BenchmarkLedgerProjection` fails the run if two identical projections disagree on their digest |
| Atomic stale-protected review schema | **met** | `internal/ledger/ledger.go`'s `CommitReview`; `TestCommitReviewRejectsAStaleLedgerAndAppliesNothing`, `TestLedgerStaleReviewRollsBackTheWholeTransaction`, `TestPropertyStaleReviewCommitsChangeNothing` |
| Devague mapping fixtures | **met** | `internal/devague/`; `TestAuthorityHonestyMatchesLedgerRules`, `internal/devague/roundtrip_test.go`, `claims_test.go`, `deliverables_test.go` |
| headspace operation/result/evidence schemas | **met** | `schemas/runner/operation.schema.json`, `schemas/runner/result.schema.json`, `schemas/ledger/evidence.schema.json`; `internal/runners/schema_test.go` |
| `nodes validate` with precise diagnostics | **met** | `cmd/nodes/validate.go`; `cmd/nodes/validate_test.go`; `TestEveryDiagnosticCarriesAHint` (every diagnostic carries a remediation), `TestDiagnosticOrderIsDeterministic` |
| Delivery-loop example compiles deterministically | **met** | `examples/delivery-loop/workflow.yaml`; `tests/e2e/reference_test.go`'s `TestReferenceWorkflowCompilesCleanlyAndDeterministically` asserts **zero** diagnostics (not merely zero errors) and an identical digest across two compilations. `nodes validate examples/delivery-loop/workflow.yaml` prints `valid: delivery-loop 1.0.0 (0 errors, 0 warnings)`. `internal/compiler`'s `TestDeliveryLoopExampleCompilesWithoutErrors` covers the PRD §11.1 listing verbatim as a separate fixture. |

## Milestone 1 — Durable vertical slice

| Box | Verdict | Evidence |
| --- | --- | --- |
| PostgreSQL schema and migrations | **met** | `migrations/*.sql`, `internal/store/postgres`; `TestMigrateIsIdempotent`; ADR 0002 records the migration policy |
| Sequential durable engine with bounded loops | **met** | `internal/engine`; `TestRunWalksTheLoopAndCompletes`, `TestLoopBoundStopsAnUnboundedRun`, `TestPlanTransitionEnforcesEachBound`, `TestPropertyTransitionsNeverExceedMaxTransitions` |
| Sync and async HTTP actors | **met** | `internal/actors`, `internal/worker/dispatch.go`; `TestWorkerDrivesASynchronousRunToCompletion`, `TestWorkerParksAsyncInvocationAndCompletesFromCallback`, `TestInvokeSynchronousResult`, `TestInvokeAsynchronousAcceptance` |
| Ledger append, supersession, projection, and review | **met** | `internal/ledger/{record,supersede,projection,review}_test.go`; `internal/store/postgres/ledger_store_test.go`'s `TestLedgerAppendProjectReviewSupersedeEndToEnd` |
| headspace-cli Docker runner adapter | **met** | `internal/runners/headspace`; `bridge_test.go` (fake binary, every exit band), `live_test.go` (real binary + real Docker), and `tests/e2e/live_test.go` (the real bridge inside a whole run) |
| Contract and mechanical success-signal validation | **partial** | Contract validation is met: `TestOutputContractViolationIsATechnicalStatus`, `TestUndeclaredOutcomeIsRejected`, `TestLedgerDeltaBeyondTheNodeContractIsRejected`. Mechanical **success-signal** validation is partial: 2 of the 9 named acceptance check kinds are mechanically evaluated — see acceptance criterion 14 below. |
| Retries, timeouts, cancellation, leases, and fencing | **met** | `TestTechnicalFailureRetriesTheSameNodeRun`, `TestExhaustedRetriesFailTheRun`, `TestBackoffFollowsThePolicy`, `TestCancellationEndsTheRunWithoutRetryOrRouting`, `TestClaimWorkIsExclusiveUnderConcurrency`, `TestReclaimExpiredThenClaimGetsHigherFencingToken`, `TestStaleFencingTokenCommitsNothing` |
| Runtime events and transactional outbox | **met** | `internal/engine/events.go`, `internal/events/relay.go`; `TestEventSequencePerRunIsStrictlyMonotonic`, `TestInsertEventConcurrentSameAggregateStaysMonotonic`, `TestInsertOutboxCarriesPayloadAndAvailableAt`, `TestChaosDroppedSendRepairedByOutboxRelay` |
| API and CLI run/inspect flow | **met** | `internal/api` (`TestWorkflowLifecycle`, `TestRunLifecycleEventsLedgerAndReviews`, `TestOpenAPIRoutesAreServed`); `cmd/nodes`; the Python front in `culture_nodes/`; `tests/parity/parity_test.go` holds both CLI surfaces to the documented operations |
| Read-only AgentCulture-aligned graph Run view | **met** | `web/src/routes`, `web/src/components`; `web/e2e/run-view.spec.ts` (graph render, walked-vs-untaken edges, keyboard-only navigation, reduced motion) |
| Ledger and evidence view | **met** | `web/e2e/ledger-view.spec.ts`; `web/src/components/AuthorityChip.test.tsx`, `NodeDetailPanel.test.tsx` |
| Complete Docker Compose profile | **met** | `deploy/compose/docker-compose.yml`, `deploy/compose/smoke.sh`; `tests/deploy/compose_test.go` (five structural assertions including no Docker socket and no elevated privileges) |

## Acceptance criteria

| # | Criterion | Verdict | Evidence |
| --- | --- | --- | --- |
| 1 | A run pins an immutable workflow digest | **met** | `TestRunsPinOneImmutableWorkflowVersion`; `TestPhase1VerticalSlice` asserts the run's `workflow_digest` equals the digest the publish call returned, and `internal/api/runs.go` resolves runs from the stored IR rather than recompiling |
| 2 | Every compiled node has an explicit owner | **met** | `internal/compiler/owners.go`; `err-missing-owner.workflow.yaml` fixture in `TestDeliberateErrorFixtures`; `TestNormalizedIRExpandsDefaultsAndResolvesOwners`, `TestResolveOwnerFallsBackToWorkflowMetadata` |
| 3 | Every actor, runner, component, image, schema, and policy reference is pinned | **partial** | The reference workflow IS fully pinned, and that is asserted (`TestReferenceWorkflowCompilesCleanlyAndDeterministically` demands zero warnings, and `CodePolicyComponentUnpinned`/`CodePolicyImageUnpinned` are warnings). At run time the pins are enforced hard: `internal/runners/headspace`'s `validate` refuses an operation whose runner revision or image digest it does not recognise, and the worker refuses an unpinned image before dispatch (`TestCodeNodeExitZeroRoutes…` asserts the digest is extracted from the reference). **What is missing:** the compiler *warns* rather than *rejects*, so PRD §21.3's "publishing rejects mutable production references" is not enforced at publish time. |
| 4 | Intake → plan → build → test → verify runs end to end | **met** | `TestPhase1VerticalSlice` (scripted runner) and `TestPhase1VerticalSliceWithRealHeadspaceRunner` (real Docker) |
| 5 | `changes_required` and failed tests loop to build without becoming engine failures | **met** | Both halves, separately: `TestPhase1VerticalSlice` walks `verify.changes_required` exactly once and asserts the attempt that produced it is technically `succeeded`; `TestFailedTestSuiteLoopsToBuildAsADomainOutcome` walks `test.failed` exactly once with the same assertion. Both tests additionally assert that **no** attempt anywhere in the run has a non-`succeeded` status. |
| 6 | Agent-origin records remain proposed until authorized | **met** | `TestPropertyAgentRecordsNeverReachConfirmed`, `TestActorCannotSelfConfirmThroughALedgerDelta`; `TestPhase1VerticalSlice` walks every agent-origin record in the finished run and asserts `proposed`, and asserts the `confirmed_claims` projection is empty |
| 7 | Task completion and verification are separate | **met** | `internal/ledger/projection.go`'s `VerificationQueue`/`DeliverySummary`; `TestDeliverySummaryCountsHonestly`; `TestPhase1VerticalSlice` asserts `confirmed_claims == 0` while `results_awaiting_review > 0` and `undecided_claims >= 3` — build said "done" and nothing verified it |
| 8 | Stale review transactions fail atomically | **met** | `TestCommitReviewRejectsAStaleLedgerAndAppliesNothing`, `TestCommitReviewRejectsAVersionTheRequestNeverRead`, `TestLedgerStaleReviewRollsBackTheWholeTransaction`, `TestPropertyStaleReviewCommitsChangeNothing` |
| 9 | Markdown is a generated reflection of JSON state | **met** | `internal/ledger/markdown.go`'s `Projection.Markdown()` renders every PRD §10.9 projection kind deterministically: a projection's `Items` are already sorted by id, and every Go map this renderer walks — a record's own `Data` payload (canonicalized through `internal/contracts.CanonicalJSON` before indenting, so sorted keys survive even when the stored bytes were not) and `DeliveryCounts`' three count maps — is walked in explicit sorted-key order rather than trusted to Go's randomized map iteration. `TestMarkdownIsDeterministic` proves storage-order independence and repeated-call stability across all nine kinds; `TestMarkdownSummaryMapOrderIsStable` renders one `delivery_summary` projection 25 times and diffs every render against the first. Wired live, not just as a library function: `GET /v1alpha1/runs/{id}/ledger/projections/{name}?format=markdown` (`internal/api/ledger.go`) renders the identical computed projection through the same function the JSON response used — `TestGetLedgerProjectionMarkdownFormat` asserts both response bodies carry the same digest. The rendered text states its own non-authoritative status (PRD §10.9) and nothing in this runtime reads it back. |
| 10 | headspace runs the test in a disposable Docker boundary | **met** | `TestPhase1VerticalSliceWithRealHeadspaceRunner` runs the reference workflow's `test` node twice through real headspace-cli + real Docker (create → run → destroy per execution, distinct workspace ids `hs-…` per run), and asserts the workspace-destroy observation is present |
| 11 | The `nodes` control-plane containers have no Docker socket | **met, with a deployment caveat** | `tests/deploy/compose_test.go`'s `TestNoServiceMountsTheDockerSocket` and `tests/deploy/helm_test.go`'s `TestNoDockerSocketAnywhere` grep the rendered deployment. **Caveat worth stating:** the shipped compose/helm topologies wire no local runner into the worker, so no container needs Docker. A deployment that DOES configure `worker.Options.CodeRunner` with the headspace bridge would be running headspace-cli as a worker subprocess, and that worker would need Docker access — which is a real tension with this criterion and is not resolved by anything in this build. The Lambda adapter (`internal/runners/lambda`) is the topology that keeps the criterion true with code nodes enabled. |
| 12 | headspace returns environment, exit, snapshot, diff, and artifact evidence | **partial** | Environment (`TestPhase1VerticalSliceWithRealHeadspaceRunner` asserts the real resolved image digest, runner revision, and platform job id), exit (measured wait status), and artifacts (`TestExecute_ArtifactExport`) are all returned. **Snapshot and diff are not**: headspace-cli 0.11.0's result package has no snapshot/diff section at all, so the bridge declares `changed_paths` **unmeasured** rather than fabricating it — recorded as confirmed spec claim **c12**. The evidence record's `covered_scope` says so in words. |
| 13 | Container output cannot grant itself `observed` authority | **met** | `internal/ledger/authority.go`'s manifest check; `TestRunnerManifestBoundsWhatCanBeObserved`, `TestRunnerManifestMustBelongToTheRecordsActor`, `TestPropertyRunnerMayOnlyProduceEvidence`. Proven live: the real run's evidence carries `exit_code`, `max_memory_mib` and `platform_request_id` (all measured by the provider) and **omits** `duration_ms` and any changed-path claim, because the bridge declared those unmeasured — `TestPhase1VerticalSliceWithRealHeadspaceRunner` asserts the omission, not just the presence |
| 14 | Required success signals mechanically verify or reject the task | **partial** | What exists: a code node's exit status mechanically decides its domain outcome (proven live, criterion 5), a `post_run` hook mechanically rejects assurance with a derived record (`TestPostRunRejectAssuranceAppendsDerivedRecord`, honesty condition h32), and — new — a node's own `acceptance.requires` block is now mechanically evaluated rather than merely declarative: `internal/runners/acceptance.go`'s `EvaluateAcceptance` checks `process_exit` (exit code) and `workspace_diff` (workspace-change completeness) against the exact `runners.Result` a code node's own evidence is built from, and refuses to answer at all — `Evaluated: false`, never a fabricated pass — when the underlying observation says the fact was not directly measured (`TestEvaluateAcceptanceProcessExit`, `TestEvaluateAcceptanceWorkspaceDiff`). `internal/worker/acceptance.go` wires this into a code node's own dispatch (`dispatchCode` in `code.go`) and appends the verdict as a derived, validator-origin `review` record pointing at the evidence it checked — proven live against `code.workflow.yaml`'s own `acceptance: requires: [{kind: process_exit, equals: 0}]`: `TestCodeNodeExitZeroRoutesTheSuccessOutcomeAndAppendsObservedEvidence` asserts a `confirm` verdict, `TestCodeNodeNonzeroExitIsADomainOutcomeWhenTheNodeDeclaresOne` asserts a `reject` verdict on the identical check against a failing exit, and `TestCodeNodeWithNoAcceptanceBlockAppendsNoVerdict` proves evaluation is opt-in per node. `TestPhase1VerticalSliceWithRealHeadspaceRunner` exercises the same wiring against the real headspace-cli bridge. **What is still missing, and why this stays partial:** only 2 of the 9 kinds `internal/compiler/vocabulary.go`'s `acceptanceKinds` names are mechanically evaluable — `TestEvaluateAcceptanceUnknownKindFailsHonestly` proves the other 7 (`schema_valid`, `artifact_digest`, `dependency_delta`, `parity_fixtures`, `changed_paths_within_policy`, `claims_confirmed`, `no_blocking_questions`) fail honestly rather than being silently skipped or assumed to pass; and the verdict is recorded as a derived fact alongside the evidence it verifies, not (yet) threaded into a `task` record's `assurance_state` — this reference workflow's code nodes propose no `task` record for a code dispatch to correlate with, so rewiring a task's own assurance lifecycle from an acceptance verdict is open work. |
| 15 | Run and ledger state survive process restart | **met** | `internal/engine/restart_test.go`'s `TestRunSurvivesAProcessRestart` (engine level) and `TestPhase1VerticalSlice`'s full-stack restart: after build's first pass the worker, scheduler, API server and **database pool** are all torn down, a brand-new stack is built, and the run finishes — asserted by intake and plan each having been invoked exactly once across both incarnations |
| 16 | Long-running actors do not hold worker leases | **met** | `TestWorkerParksAsyncInvocationAndCompletesFromCallback` asserts the work item is `waiting` with a NULL `lease_owner`, that further ticks claim nothing, and that the run completes from the callback alone; `TestStartAsyncWaitReleasesTheClaimWithoutCompletingIt` |
| 17 | Duplicate callbacks and runner completions are harmless | **met** | `TestCallbackDuplicateEventIsIdempotent`, `TestCallbackAfterNewerAttemptIsLateAndCommitsNothing`, `TestCallbackSequenceIsMonotonic`; `TestFaultDuplicateSignalExactlyOneEffectiveCompletion` (two ready rows for one logical node run, one effective completion), `TestChaosDuplicateDeliveryClaimedExactlyOnce`, `TestPublishIsIdempotentByWorkID` |
| 18 | Two workers cannot commit the same current transition | **met** | `TestFaultTwoWorkersNoDoubleCommit` — two real OS processes, 50 items, each completed exactly once; `TestClaimWorkIsExclusiveUnderConcurrency`, `TestStaleFencingTokenCommitsNothing`, `TestCompleteWorkStaleTokenRejected` |
| 19 | Every committed transition emits a runtime event | **met** | `TestEventSequencePerRunIsStrictlyMonotonic`; `TestPhase1VerticalSlice` asserts the run's `events` rows are dense `1..N` with no gaps, that the SSE stream delivers exactly those N in strictly increasing order, and that exactly 8 `token.transitioned` events were emitted for the 8 transitions the loop makes |
| 20 | The graph and ledger UI derive only from committed state | **met** | The UI reads `GET /v1alpha1/runs/{id}`, `GET /v1alpha1/workflows/{digest}`, `GET /v1alpha1/runs/{id}/ledger` and the SSE stream — all served from committed rows (`internal/api/events.go` polls the `events` table, never in-process state). `TestPhase1VerticalSlice`'s `assertRunViewContract` asserts the payload the Run view renders carries node runs with states, attempts with fencing tokens, tokens, the pinned IR, and the ledger. `web/e2e/*.spec.ts` covers the rendering itself. |
| 21 | The UI uses a pinned current AgentCulture design revision | **met** | ADR 0001 pins `agentculture/org` commit `b4d939ba0aa354a5ae53065319a773e0013de698`; `scripts/check-culture-design.mjs` verifies the extracted layer has not drifted |
| 22 | Fault-injection tests cover failure before dispatch, after dispatch, before commit, after commit, during callback, and during headspace completion | **partial** | Five of six have named tests, listed below. "After dispatch" is covered only indirectly. |
| 23 | Idle-memory, ledger-projection, transition-throughput, and headspace conformance benchmarks are recorded | **met** | `docs/benchmarks.md` records all four: idle RSS of `nodes all` = **30.1 MiB** (`scripts/idle-rss.sh`, against a ≤128 MiB target); ledger projection = **12.4 ms** over 2,000 records and **441 ms** over 100,000, digest-stable (`BenchmarkLedgerProjection`); transitions = **128/sec sequential**, 7.8 ms each (`BenchmarkTransitions`); headspace conformance = identical policy digest, image digest and observation set across two real executions of one operation, asserted by `TestPhase1VerticalSliceWithRealHeadspaceRunner`. The doc states the host caveat and lists what is *not* measured. |

### Criterion 22 in detail

| Failure point | Verdict | Evidence |
| --- | --- | --- |
| Before dispatch | **met** | `tests/fault/claiming_fault_test.go`'s `TestFaultKilledWorkerReclaimedBySurvivor` — a real `kill -9` before dispatch, a real lease expiry, a survivor that picks the work up |
| After dispatch | **partial** | No test kills a worker *between* dispatching to an actor and committing. The property that makes it safe is tested (`TestStaleFencingTokenCommitsNothing`: a completion under a lost claim writes nothing; `withHeartbeat` cancels an in-flight invocation when the lease is lost, `internal/worker/dispatch.go`). The end-to-end injection is **not written**. |
| Before commit | **met** | `TestFaultTwoWorkersNoDoubleCommit`, `TestStaleFencingTokenCommitsNothing`, `TestEngineInTxRollsBackEverything` |
| After commit | **met** | `TestSchedulerCrashBetweenEffectAndMarkFiredRefiresIdempotently` (an injected abort after the effect, before the mark); `TestRunSurvivesAProcessRestart`; `TestPhase1VerticalSlice`'s stack restart |
| During callback | **met** | `TestCallbackDuplicateEventIsIdempotent`, `TestCallbackAfterNewerAttemptIsLateAndCommitsNothing`, `TestCallbackRefusesForeignAndForgedTokens` |
| During headspace completion | **met** | `TestExecute_DestroyRefusal_FallsBackToForce`, `TestExecute_Cancellation_UsesSeparateStopProcess`, `TestExecute_CreateFailure_NeverAttemptsRun`, and the exit-band table `TestExecute_ExitBandTable` |

---

## The first reference workflow, against §"First reference workflow"

The implementation issue's mermaid diagram has one node this build does not
have.

| Element | Verdict | Note |
| --- | --- | --- |
| `I → P → B → T → V → F` | **met** | `examples/delivery-loop/workflow.yaml`, run end to end |
| `T --> B` on `failed` | **met** | `TestFailedTestSuiteLoopsToBuildAsADomainOutcome` |
| `V --> B` on `changes required` | **met** | `TestPhase1VerticalSlice` |
| `V --> H` on `blocked` (Human review), `H → B` | **NOT MET** | **Deviation d1, github issue #3.** The approval / human-task surface is deferred: `internal/engine` creates no human-task rows and `internal/worker` registers no `HumanDispatcher`, so an approval node fails an attempt with a `not_implemented` diagnostic (`TestUnwiredKindsFailWithADiagnosticRatherThanSucceeding`). Rather than ship a reference workflow with a dead end, `verify` declares only `passed` and `changes_required`; the fixture's own header comment records this. Restoring `blocked` + the approval node is the first thing to do when d1 lifts. |

Expected-behavior prose from the same section:

- intake proposes scope/claims/assumptions/questions — **met** (the scripted
  intake agent proposes all four record types and the run's ledger holds
  them);
- plan proposes tasks and mechanical success signals — **met as records**
  (`task`, `success_signal` records are proposed and projected), **not met as
  enforcement** (criterion 14);
- build claims a task and proposes a result — **met**;
- headspace runs the pinned test operation and appends runner-observed
  evidence — **met** (live);
- acceptance logic verifies or rejects the result — **not met** (criterion 14);
- verify returns `passed` / `changes_required` / `blocked` — **partial**:
  `passed` and `changes_required` yes, `blocked` deferred with d1;
- expected negative outcomes follow graph edges rather than masquerading as
  runtime failure — **met** (criterion 5);
- loops bounded by transition, node-visit, time, parallelism, token — **met**
  for transitions, visits, and duration (`TestPlanTransitionEnforcesEachBound`);
  parallelism/token bounds exist in the schema and the sequential engine holds
  `maxParallelTokens: 1` by construction, but no test drives a parallel bound
  because §9.8 parallelism is explicitly out of scope for this issue. Cost
  budgets are not implemented (also listed as optional).

---

## Also not met, and named here so it is not mistaken for done

- **`waiting_external` deadline timers do not fail attempts.** The scheduler
  fires a deadline timer and appends an outbox event
  (`TestSchedulerFiresDeadlineTimerInsertsOutboxEvent`), but nothing turns
  that into a failed attempt for an actor that never called back. A run
  parked on a silent async actor stays parked. Known gap, carried forward.
- **`approval` and `wait` node kinds have seams, not implementations.**
  `internal/worker/seams.go` declares `HumanDispatcher` and `WaitDispatcher`;
  neither has a production implementation, and an unregistered seam is a
  diagnosed failure rather than a silent success — which is the right failure
  mode, but it is a failure mode, not a feature.
- **Acceptance-check registry covers 2 of 9 kinds** (criterion 14). Mechanical
  `process_exit`/`workspace_diff` evaluation is real and wired into a code
  node's own dispatch, but `schema_valid`, `artifact_digest`,
  `dependency_delta`, `parity_fixtures`, `changed_paths_within_policy`,
  `claims_confirmed`, and `no_blocking_questions` are not evaluable yet, and
  a mechanical verdict does not (yet) reach a `task` record's
  `assurance_state` — see criterion 14's evidence for exactly what is and
  is not covered.
- **OpenTelemetry** — PRD §1 names it; `internal/telemetry` is a package doc
  and nothing else. See ADR 0005 (D3).
- **ECS/Fargate deployment** — PRD §1 says "ECS/Fargate first"; what exists is
  Helm/EKS and Compose. See ADR 0005 (D2).
- **A code node's success/failure outcome mapping is a worker-side
  convention, not a schema field.** `internal/worker/code.go`'s
  `ConventionalCodeOutcomes` recognises `passed`/`succeeded`/`completed`/`ok`
  and `failed`/`failure`, and **refuses to dispatch** a node whose declared
  outcomes it cannot map (`TestCodeNodeWithUnmappableOutcomesIsRefusedRatherThanGuessed`)
  rather than guessing. The workflow schema declares no mapping today — PRD
  §11.1's example names `passed`/`failed` but never says which is the exit-0
  port. Closing that gap means a schema field (the way a `post_run` hook's
  `on_failure.outcome` already does it) and is open work.
