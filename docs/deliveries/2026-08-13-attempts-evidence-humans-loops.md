# Delivery Summary — attempts-evidence-humans-loops

plan: `attempts-evidence-humans-loops` · run: `complete` · date: `2026-08-13`
baseline: `devague summary skeleton`

## Intent

Deliver the eight-issue batch (#32 #33 #34 #36 #37 #38 #39 #40) plus its
live proof: every attempt — failed included — carries actor attribution and
usage where reportable; evidence bindings resolve real run evidence and
bridge-measured facts; flows park on signals and humans and resume; humans
execute nodes through the same actor protocol as agents; acceptance criteria
route; ad-hoc runs are first-class; and a PR-upkeep loop sweeps this repo's
own SonarCloud/Qodo debt through the product, human-gated. Executed as 23
plan tasks in 6 waves, fanned out per the approved split (spark claude-code
bridges + codex analysis + local subagents), TDD-gated merges, followed by a
production deploy and ten live loop cycles.

## Planned Work

Quoted verbatim from the `devague summary` skeleton:

- `t1` — Pre-build citation re-verification: spot-check every file:line the spec's scope entries s1-s14 cite still matches reality; document any drift before build tasks merge
- `t2` — Issue 40: thread actor attribution through worker failure completions — ActorRowID on DispatchContext populated post-Resolve, failAttempt/completeTechnicalFailure carry it, code-path failures stamp codeRunnerActorID
- `t3` — Issue 32 Go side: optional usage field on FailedPayload and persistence in the callback EventFailed branch; record the PRD 13.2 amendment as an ADR
- `t4` — Issue 32 bridges: all three bridges (claude-code, codex, colleague) emit usage on the non-domain failure branch when a terminal result object exists
- `t5` — Issue 32 sync path: bridge 500 error body carries usage when a result exists; completeFromInvocationError persists it; docs state the honest narrowing (cancelled and no-result attempts stay unreported)
- `t6` — Issue 34: EvidenceForSubject empty subject means all live run evidence; reconcile the two doc comments; empty-subject unit subtest; `bindings_test` stub projects real records; e2e asserts delivery-loop verify receives non-empty testEvidence
- `t7` — Issue 33b: node-run-scoped evidence selector (join `node_runs` on `run_id`+`node_key`) and /nodes/&lt;id&gt;/evidence resolution in both worker and engine resolvers; compiler-vs-resolver surface agreement test
- `t8` — Issue 33a: typed `workspace_measured` field on InvocationResult, CompletedPayload, FailedPayload; folded into the node's persisted output so downstream nodes bind it via /nodes/&lt;id&gt;/output; conformance fixtures round-trip it for all three backends
- `t9` — Issue 39 wait foundation: production WaitDispatcher wired via Options.Waiter; until.duration and until.timestamp park durably on the existing wait timer and resume through planTransition with bounds enforced
- `t10` — Issue 39 event surface (event-first per c35): first-class emit/subscribe event records, authenticated inbound event delivery route, until.signal subscribes to an event and resumes; schema forward-compatible with multi-token pickup and mid-execution emission
- `t11` — Issue 38 lifecycle: human-timescale deadlines authorable per node; long-park proven against deadline timers and the dispatch budget without actor-kind branching
- `t12` — Issue 38a: human-inbox bridge adapter speaking the section-13 protocol (202 accept, durable pending task, authenticated callback with the human's submission) plus a kind=human actor registration lane
- `t13` — Actor registration API: authenticated POST /v1alpha1/actors creating append-only actor revisions, replacing the raw-SQL-only lane
- `t14` — Issue 38b: web inbox surface — list pending human tasks, submit a decision/result with token auth; waiting-on-human runs actionable from the browser
- `t15` — Auth hardening gate: negative tests proving every mutating route added by this batch (event delivery, actor registration, human submission) refuses unauthenticated requests; read-only surfaces stay authless
- `t16` — Issue 37 schema: enforce policy on acceptance.requires (route-as-technical-status, route-as-domain-outcome, or observe-only) in the workflow schema and compiler, with publish-time validation
- `t17` — Issue 37 evaluation and routing: acceptance checks evaluated on agent and code nodes before routing per the declared enforce policy; retry composition explicit; evaluations land as derived-authority records
- `t18` — Issue 37 success signals: evaluator for mechanical success_signal records; non-mechanical signals honestly stay unevaluated
- `t19` — Issue 36: first-class ad-hoc runs — API endpoint renders a canonical one-node workflow from an instruction, publishes idempotently by digest, creates the run; Go nodes run verb implements the same lane
- `t20` — Invariant gates: post-merge sweep proving provider neutrality and the authority ladder survived the batch
- `t21` — PR-upkeep workflow (culture-nodes only): sweep code nodes for SonarCloud unresolved issues and PR Qodo findings, triage, fix on the spark claude-code bridge actor, independent codex review, human-approves-PR gate looping back, human-prepares-next-item human node; external driver script calling POST /v1alpha1/runs
- `t22` — Live run: drive the PR-upkeep loop on the real repo (four standing Sonar items), human gates and grades exercised, and execute all eight c24 success-signal checks recording each outcome
- `t23` — Delivery closeout: summarize-delivery artifact with the audience-to-surface map and the after-state-to-requirement-and-signal cross-map; stats-epoch note for issue 28 consumers

## Actual Delivery

| Plan task | Status | What actually landed |
|-----------|--------|----------------------|
| `t1` | delivered | codex-thor run `01KZWV5QZE…` verified all 14 scope entries; found the one real drift (20th failAttempt site in runnerasync.go), already covered by t2's brief |
| `t2` | delivered | merged `454013f`; `internal/worker/attribution_test.go`; proven live (failed attempt attributed, retry burn 4→5) |
| `t3` | delivered | merged `9315181`; ADR `docs/adr/0008-usage-on-failed-event.md` |
| `t4` | delivered | merged `9763f52`; all three bridges + fixtures together |
| `t5` | delivered | merged `a17a379`; h24 narrowing documented (cancelled + result-less attempts) |
| `t6` | delivered | merged `5b2ceb1`; empty-subject = all live run evidence; e2e delivery-loop assertion |
| `t7` | delivered | merged `dc08f1e`; `/nodes/<id>/evidence` resolves by node run; surface trap closed both ways |
| `t8` | delivered | merged `6eaa0c4`; `workspace_measured` observed riding a live task payload (cycle 8) |
| `t9` | delivered | merged `27b046f`; timer-backed WaitDispatcher, cancel reaps timers |
| `t10` | delivered | merged (3 commits, `cfdb105`…`ca2079a`); migration 0016; proven live (signal-4 park/resume + 401) |
| `t11` | delivered | merged `3a1b3cc`; honest finding: long timeouts already unclamped — delivery is proof-tests + authoring docs |
| `t12` | delivered | merged; `adapters/human-inbox/` 62/62 tests; ran production human tasks live (3 completed submissions) |
| `t13` | delivered | merged `6749983`; used live to register `company/operator-claude` and `company/human-ops` |
| `t14` | delivered | merged; web `/inbox` route, 340 web tests post-merge |
| `t15` | delivered | merged; caught + closed a real c27 violation (t19's ad-hoc lane shipped unauthed); table-driven 401 gate |
| `t16` | delivered | merged `9cd0cbf`; enforce vocabulary with observe default |
| `t17` | delivered | merged (`8338032`); pre-routing evaluation, 6 DB-backed tests incl. loop-back self-edge |
| `t18` | delivered | merged `6862b4b`; seam conflict with t17 resolved by operator (t17's pre-routing kept, t18's evaluator added) |
| `t19` | delivered | merged; `POST /v1alpha1/adhoc-runs` + `nodes run`; used live for the signal-1/7 checks |
| `t20` | delivered | merged `8a0c3f8`; `internal/invariants` + `docs/invariants.md`; zero violations, allowlists encode verified reality |
| `t21` | delivered | merged `ebd900b` + 5 live-test hardening commits; workflow validated and executed 10 live cycles |
| `t22` | delivered | ten live cycles ending in run `01KZXFYJR1Y6KHCZHT843PTMEG` `completed` — full graph incl. loop re-entry, human-merge task, empty-sweep park; all four Sonar items fixed and verified; eight signal checks recorded below |
| `t23` | delivered | this artifact |

## Mid-work Decisions

- `d1` — Wave-2/3 execution switched from spark claude-code bridges to paced local subagents (serial): the shared Claude session exhausted mid-wave — t10's local agent and t11's bridge run both died on the provider session limit — remaining tasks ran locally after the window reset, paced to survive it (approved; economics recorded in issues #47/#48).
- `d2` — pr-upkeep sweep runs `network: full` instead of the declared egress-allowlist — headspace-cli 0.11.0 supports only a disabled/enabled posture and the runner boundary rejected the allowlist live (proposed; follow-up issue #50).
- `d3` — Bridges forward engine-resolved bindings into the session as a serialized block (all three; before this a node's `input.bindings` were silently invisible to the model — cycle 5's review honestly reported its bindings missing) (proposed).
- `d4` — Bridges honor a session-declared `{outcome, output}` final message verbatim, engine contract validation enforcing declaredness — two-outcome agent nodes were undrivable while bridges hardcoded the outcome (proposed).
- Not covered by a record: orin's worker was stopped mid-live-test — it still ran the pre-batch binary and claimed attempts with NULL attribution (heterogeneous-fleet finding); it stays stopped until redeployed from the merged main.
- Not covered by a record: the pr-upkeep sweep script is fetched pinned to a commit SHA and exec'd — nothing ships code-node scripts into runner images today (found live: python exit 2 on the missing path).
- Not covered by a record: per-host review repo paths (`review_repo` run input) — thor's codex bridge 403'd on a spark path (cycle 4).
- Not covered by a record: the human-inbox claim statement falls back through `output.note` to a generated sentence — an empty statement contract-rejected an otherwise-valid human decision live.
- User decision (recorded as issue #54): the merge IS the submission — a human never acts twice for one action; the operator relayed merge submissions this run, and auto-observation is the follow-up.
- User decision (spec c35 addendum applied in t10's brief): the event surface shipped event-first.

## Drift From Plan

| Plan item | Reason for divergence | Classification |
|-----------|-----------------------|----------------|
| `t10`/`t11`/`t15` (`d1`) | session exhaustion forced the lane switch from bridges to paced local subagents; work content unchanged | needs-follow-up |
| `t21`/`t22` (`d2`) | headspace 0.11.0 cannot honor egress-allowlist; sweep runs network:full with declared intent documented | needs-follow-up |
| `t22` (`d3`) | binding forwarding added to all three bridges mid-live-test — the plan assumed bindings reached sessions | acceptable |
| `t22` (`d4`) | session-declared outcomes added mid-live-test — the plan assumed two-outcome agent nodes were drivable | acceptable |
| `t11` | no production change was needed (long timeouts already unclamped); delivery is proofs + docs, honest per its own brief | acceptable |
| `t22` | grades were posted for build/live runs but the four live-test cycle failures (runs `01KZXA0P…`, `01KZXACT…`, `01KZXAJ7…`, `01KZXBD3…`, `01KZXC2W…`, `01KZXFFD…`) each consumed a billable fix/review session before its blocking gap was found — burn the plan did not price | needs-follow-up |

## Evidence

- tests: full `go test ./...` on the merged branch — pass (no FAIL lines); `uv run pytest -n auto` — pass; web `npm test` — 340/340 pass; adapter suites codex 140 / claude-code 162 / colleague 127+4 env-failures pre-existing on spark (pass in CI) / human-inbox 62 — pass
- invariant gate: `go test ./internal/invariants/` — pass on the final tree
- lint: `markdownlint-cli2` on new docs — 0 errors
- commits: `b975a60..356c5e4` (60 commits on `build/attempts-evidence-humans-loops`)
- PRs merged by the human owner from the live loop: #51 (S3516 BLOCKER), #53 (S3776 codexsmoke), #55 (S3776 codexworkenv), #57 (S8193)
- SonarCloud `agentculture_culture-nodes`: 4 unresolved before → **0 unresolved after**, each fix verified by post-merge re-analysis
- live runs (prod, thor): build-lane `01KZWV54EY…`/`…54HG`/`…54K1`/`…54NE`/`01KZWVVDY8`/`…VDZH`/`01KZWW09C9`/`…09DE`/`01KZWWS4S7`/`…S4V2`/`01KZWX06E8`/`01KZWXQHN2`/`…QHP9`; live-test cycles `01KZXA0P…`, `01KZXACT…`, `01KZXAJ7…`, `01KZXATS…` (PR #51), `01KZXBD3…`, `01KZXC2W…`, `01KZXFFD…` (PR #55), `01KZXFYJR1Y6KHCZHT843PTMEG` (PR #57 + full graph, `completed`); signal checks `01KZXA2FSP…`, `01KZXAC27D…`, `01KZXAM2M4…`
- grades: 22 proposed grade records by `company/operator-claude` (`ledger_01KZX9YB…`…`ledger_01KZXHDG…`), awaiting human review
- issues filed during the run: #47 (user), #48 budget-aware execution, #50 egress allowlist, #54 merge-equals-submission; deferred earlier: #41 tracker transport, #43 parallel tokens
- deviations: `devague deviate --list` — d1 approved, d2–d4 proposed

### The eight c24 success signals

| # | Signal | Outcome |
|---|--------|---------|
| 1 | failed agent attempt persists actor_id and counts in retry burn | **live**: run `01KZXAC27D…` attributed to intake, burn 4→5 (first check exposed orin's stale worker — ops finding, then re-proven) |
| 2 | delivery-loop verify receives non-empty testEvidence | merged e2e test (t6); pass |
| 3 | all three bridges round-trip usage on failure + workspace_measured binds | merged tests (t4/t5/t8); workspace_measured observed live in cycle-8 payload |
| 4 | wait node parks and resumes on a delivered signal | **live**: run `01KZXAM2M4…` completed via authenticated event; unauthenticated delivery 401 |
| 5 | kind=human actor completes a node through the callback path | **live**: three human-task submissions completed attempts in runs `01KZXD60…`/`01KZXFYJ…`; neutrality test unmodified and green |
| 6 | acceptance-failing node routes its declared outcome | merged DB-backed tests (t17, incl. loop-back edge); pass |
| 7 | one CLI/API call runs an ad-hoc instruction | **live**: `nodes run` created pinned-digest runs twice (`01KZXA2F…`, `01KZXAC2…`) |
| 8 | PR-upkeep flow completes a live run with a human-gated merged fix | **live**: four merged fixes; final run `completed` through the whole graph; Sonar 0 unresolved |

Live-harm evidence attached per c23/h19: pre-fix failed run `01KZW2XDR7YD2GER787QZ0K67M` (2026-08-13 morning, unattributed + usage-less) versus post-fix `01KZXAC27D…` (attributed, honest NULL usage).

## Delivery Claims

| Claim | Confidence | Evidence |
|-------|------------|----------|
| Failed worker-path attempts carry actor attribution; retry burn counts them | high | commit `454013f` · live burn 4→5 on `01KZXAC27D…` |
| Failed attempts persist real usage when a terminal result existed; never fabricated | high | commits `9315181`/`9763f52`/`a17a379` · ADR 0008 |
| Evidence bindings resolve: run-wide projection and per-node `/nodes/<id>/evidence` | high | commits `5b2ceb1`/`dc08f1e` · e2e tests |
| workspace_measured lands in node output and reaches downstream consumers | high | commit `6eaa0c4` · cycle-8 live payload |
| Flows park on timers, signals, and humans, and resume; bounds enforced | high | t9/t10/t11 merges · live runs `01KZXAM2M4…`, `01KZXFYJ…` |
| Humans execute nodes through the standard actor protocol, zero dispatch branching | high | adapters/human-inbox · live submissions · invariant gate |
| Acceptance enforce policies route; mechanical success signals evaluated | high | t16/t17/t18 merges, DB-backed tests |
| Ad-hoc runs are first-class (API + CLI), bearer-gated | high | t19+t15 merges · live `nodes run` |
| The PR-upkeep loop cleared the repo's whole standing Sonar debt through the product, human-gated, each fix verified by re-analysis | high | PRs #51/#53/#55/#57 · Sonar 0 unresolved · run `01KZXFYJR1Y6KHCZHT843PTMEG` |
| Per-actor stats epoch: pre-fix NULL-actor attempts read as unattributed-era, not zero burn | medium | this note + `docs/adr/0008`; no backfill possible by design |
| Qodo findings sweep is exercised end-to-end | medium | sweep extractor parses recorded Qodo fixtures (18 tests); live cycles found no open Qodo PR findings to work (0 open PRs during the test) |

### Audience-to-surface map (c20/h16)

operators → ad-hoc runs + the pr-upkeep flow + web inbox; workflow authors →
working evidence bindings + routable acceptance + wait/event nodes; the actor
fleet → attribution and usage on failure + binding forwarding + declared
outcomes; analytics (#28) → true retry burn + the stats-epoch note. Every
after-state clause maps to a delivered task above and a recorded signal
(c21/h17); no orphan clauses remain.

## Remaining Work / Follow-up

- Redeploy orin from merged main and restart its worker (stopped during the live test; running the pre-batch binary would reintroduce unattributed claims) — operator, after the final PR merges.
- #50 — headspace egress-allowlist; then tighten the sweep back from `network: full` (d2).
- #54 — observe externally-completed human actions and auto-submit (merge = submission); #41 tracker transport is its sibling.
- #48 — budget-aware execution: pacing, capacity circuit breaker, session stickiness, cache telemetry (d1's structural fix; insights comment attached).
- #43 — parallel tokens / split-join / event-driven continuation (c35 direction, deferred by c18).
- Artifact refs bound from code-node outputs were unretrievable by downstream agent sessions (every fix session re-ran the sweep read-only as a workaround) — file and fix; candidate cause: artifact fetch surface not exposed through bridges.
- Grades (22 proposed) await human confirm/reject through the review surface.
- d2–d4 deviation records await owner confirmation (`devague deviate --confirm`).
- Sync-failure usage for the colleague bridge's 4 spark-environment integration failures: pre-existing environmental, passes in CI — track only if it recurs there.
