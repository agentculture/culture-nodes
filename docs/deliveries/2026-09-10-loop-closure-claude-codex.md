# Delivery Summary — loop-closure-claude-codex

plan: `loop-closure-claude-codex` · run: `partial` · date: `2026-09-10`
baseline: `devague summary skeleton`

## Intent

Close the idea-to-shipped loop on the claude and codex lanes: one work-item id
through every artifact, a land node that owns the handover, a cleanup node, a
lane-liveness fact the router reads, an analysis node before fix, triage rows
on every opened issue, hand-turn records in the ledger, Jira stage write-back,
and a readiness block on the merge gate — the spec
`docs/specs/2026-09-07-loop-closure-claude-codex.md` (issues #308–#319), built
as 19 tasks in six waves on `build/loop-closure-claude-codex`.

The run is `partial` on one count only: every build task merged and gated, but
`t19` — this summary and the validate-delivery evidence — is the operator's,
and its obligations (`o1`–`o17`) and deviations (`d1`–`d4`) are approved; the
evidence (`e1`–`e17`) and deltas (`b1`–`b3`) are `proposed` until the owner confirms them.

## Planned Work

Quoted verbatim from the `devague summary` skeleton (the skeleton lists the
sixteen confirmed tasks; `t4`, `t7` and `t19` are quoted from the plan state
because they were amended after export — code spans around placeholders —
and await re-confirmation):

- `t1` — `work_item` as its own run column and filter (Go + CLI): migration adds runs.`work_item`; POST /v1alpha1/runs accepts `work_item`; GET /v1alpha1/runs?`work_item`=KEY filters; run create/list CLI gain --work-item; openapi + explain catalog updated; run.category untouched (decision c41)
- `t2` — sweep carries the work item: pr-upkeep.pr payload gains `work_item` from `_correlated_issue_key`, else the transient gh:owner/repo#N form; the upkeep workflow input contract admits `work_item`; the emitter sets it on the event so the minted run's `work_item` is stamped; lane doc explains the two shapes and that the gh form never survives intake (c42); sweep gains no other write
- `t3` — jira bridge `create_issue` gains labels: the exact-key allowlist admits an optional labels list of plain identifiers, the REST payload sets fields.labels, capabilities prose names it; tests cover accept, reject a non-list, reject a label with spaces
- `t4` — orphan-ticket intake node: for a pr-upkeep.pr fact whose work_item is the gh form, a graph step creates the orphan Jira ticket through the jira actor (create_issue with labels orphan, auto-created, source:github, `repo:<repo>`), re-keys the run's work_item to the new Jira key (PATCH gains work_item for this one transition, or the run is re-created keyed), and records the mapping; a second tick for the same PR finds the key on the ticket and creates nothing
- `t5` — culture-land engine account and credentials: lanes/unix-user.sh learns engine 'land' (no model credential), install-secrets.sh delivers bridge-push.env (Contents write) and a separate pull-requests:write token file for the account, register-actor.sh registers company/land-thor with `os_user` and `handover_remote` metadata; cutover.sh accepts the engine; the token permissions are recorded in deploy/prod/README.md
- `t6` — land node core (runner code node): inputs `handover_ref`, `target_branch`, `work_item`, `actor_id`; takes a per-checkout lease before resetting the producing actor's checkout and a per-target-branch lease before pushing; fetches the ref via the actor's `handover_remote`; rebases onto the branch tip (re-fetch and rebase again once on staleness, then route); pushes without --force; writes one record per step (fetch, lease, rebase, gate, push, reply, resolve) and is idempotent on re-run
- `t7` — land node gate and single version bump: the land node runs the operator's pre-push chain (target pytest, go test ./tests/lint, `scripts/lint-all.sh <job>`, file-length) in the rebased worktree, declares go/uv/node+markdownlint-cli2 as required toolchains, and performs exactly one version bump per landing via bump.py fed a JSON changelog on stdin naming the findings landed; a red gate routes with the #102 record and pushes nothing
- `t8` — land node reply and resolve: after a successful push the node replies on the finding's PR thread naming the commit and resolves the thread with the pull-requests:write token, signed as the land node; if the finding is a Sonar issue with no thread, it comments once on the PR; idempotent (existing reply detected); no merge call
- `t9` — bridge-side liveness: the shared preflight `HOST_KEYS` gain liveness {`session_ok`, reason, `checked_at`, mode}; per-lane mode LOCK|CHECK from bridge config; codex CHECK probe is a dry read-only exec bounded at 20 s classifying the spent-refresh-token text; codex LOCK sets `session_ok`=false when a run's output carries that text; claude-code reads ~/.claude/.credentials.json expiresAt; a bridge re-derives its half at start; all copies byte-identical and the preflightsurface lint extended to the new key
- `t10` — control-plane liveness and router fallback: a persisted `actor_liveness` row written by the mesh collector from the bridge fact and by the worker from an attempt outcome class `credential_spent`; actor metadata `fallback_actor`; the worker refuses a lease when the row is fresh (<5 min), `session_ok`=false and a fallback is registered, appends a derived routing record (internal/repair shape, router=`lane_liveness`) and dispatches to the fallback; unlock is POST /actors/{id}/resume AND a subsequent healthy bridge fact (decision c43); stale rows never refuse
- `t11` — liveness detectors: nodes doctor gains a warning-severity `lane_liveness` check (five checks; teken assertion updated), deploy.sh's thor/orin detector tail and codex-preflight.sh report the fact, register-actor.sh docs show --metadata `fallback_actor`, the nodes-operator skill gains a 'pre-check lanes' step that reads it before a fan-out
- `t12` — pr.closed fact: the human-inbox tracker's `github_pr_closed` observation is emitted by the sweep as a pr.closed fact (`source_key` github:{repo}:pr:{n}:closed, watermark `closed_at`, subject = `work_item`), mirroring pr.merged; the workflow vocabulary in the README lists it
- `t13` — cleanup node (runner code node) triggered by pr.merged or pr.closed for a work item: lists review-fix/\* and refs/culture-nodes/\* refs of the item, deletes only those reachable from main (reports the rest), cancels the item's runs parked at human-merges-pr through POST /runs/{id}/cancel with reason `pr_merged`|`pr_closed` via the cancelRunWithReason seam, drops per-item sweep state, and writes one derived record listing every deletion, every declined ref and every cancelled run
- `t14` — analysis node before fix in examples/pr-upkeep: per finding reads thread replies and resolution state, outputs FIX|PUSHBACK|DUPLICATE with a one-line reason, bundles same-rule findings in one file into one package for fix, defers further findings on a file until the post-push re-scan, and surfaces pushbacks in the tick summary; README graph and lane doc (cadence sentence rewritten) change in the same PR
- `t15` — open-issue.sh --disposition 'bucket|text|evidence': appends the dispositions.csv and issue-types.csv rows and regenerates open-issues.md in the same invocation via a small helper (scripts/triage-rows.py) so the wrapper stays deletable; every issue the loop opens (orphan-ticket intake, cleanup, liveness) goes through it
- `t16` — `hand_turn` ledger kind and the human-guided observation loop (decision c25): record kind `hand_turn` {what, stage, `work_item`, `definition_ref`} under the existing authority matrix; a `hand_turn_definition` record the human confirms; an observing agent code node that reads a work item's runs, refs, PR events and issue comments and writes proposed `hand_turn` records against the definition; actor stats gain `hand_turns_by_stage` counting confirmed records per work item and cycle; nodes hand-turn CLI verb + catalog entry for direct entry
- `t17` — Jira stage write-back and stage watermark: a stage enum {intake, spec, dispatch, pr-open, merged, cleanup}; each loop graph posts one structured stage comment per transition through the jira actor's `post_comment` verb (never the sweep); the sweep reads the latest stage comment as the ticket's watermark and a tick that finds the stage already recorded emits nothing for it; self-echo filtering by account id unchanged
- `t18` — merge-gate readiness collector: a deterministic code node (stdlib) that assembles CI check states, SonarCloud open-issue count, unresolved thread count, proposed devague records (.devague/\*.json) and failing evidence for the PR, whose output human-merges-pr binds through `context_refs` so the task is presented with the block; file the bulk-confirm ask on agentculture/devague through the communicate skill and cite it in the spec's non-goal
- `t19` — validate-delivery and summary: re-measure every before-state number, check the nine success signals live against named runs/records/ref listings, map every announcement clause to its requirement and signal (strike clauses without both), report the hand-turn count from confirmed hand_turn records, name the audience each node served, and confirm the process boundary (technical citations appear only as evidence)

## Actual Delivery

Every task ran as a build in an isolated checkout, was merged `--no-ff` by the
operator behind the full gate (pytest, `go test` for lint/deploy/e2e/API,
`scripts/lint-all.sh root`, teken doctor), and left a merge commit named
`merge tN:` on the build branch. "Lane" names who built it.

| Plan task | Status | What actually landed |
|-----------|--------|----------------------|
| `t1` | delivered | migration `0057_run_work_item.sql`; `work_item` on POST/GET/PATCH-refused; engine stamps it from the event payload; `run create/list --work-item`; openapi + catalog. Lane: local subagent (Go). Merge `4772eae`. |
| `t2` | delivered | `pr_upkeep_emit.work_item_for_pull` + `upkeep_pr_fact`; workflow 2.2.0 requires `work_item`; lane doc corrected to the live 300 s schedule. Lane: local subagent. Merge `3faa2d7`. |
| `t3` | delivered | `create_issue` admits `labels` (pattern widened to allow `/` so `repo:owner/name` passes). Lane: local subagent. Merge `f9744bb`. |
| `t4` | delivered | workflow 2.3.0: `route → intake-orphan → stamp-pr → fix`; `PATCH /runs/{id}` admits one gh:→Jira re-key; e2e proves one ticket with four labels. Lane: local subagent. Merge `92933bd`. See `d2`. |
| `t5` | delivered | engine `land` in unix-user.sh; `lanes/land-secrets.sh` delivers two token files; `register-actor.sh --runner-account`; `cutover.sh thor land`; README section. Lane: local subagent. Merge `a32ad07`. |
| `t6` | delivered | `examples/land/{land.py,workflow.yaml,README.md}`: mkdir-atomic checkout and branch leases, fetch via `handover_remote`, bounded rebase, idempotent push, per-step records, `land_routing` human record on conflict. Lane: local subagent. Merge `0b5247d`. |
| `t7` | delivered | `examples/land/land_gate.py` (gate chain, toolchain refusal by name, one bump via stdin JSON), `lanes/land-toolchain.sh` detector. Lane: **developer actor**, run `01M25VSZBCWKX52JWWTWYZVKHM`, handover `21db875`. Merge `44bf16f`; operator fixup `cb4a89c` (see `b1`). |
| `t8` | delivered | `examples/land/land_reply.py`: one signed reply + resolve per finding, one PR comment for thread-less findings, idempotent, no merge endpoint. Lane: local subagent. Merge `f2905cc`. |
| `t9` | delivered | shared `liveness.py` + `HOST_KEYS.liveness` in 7 adapters (byte-identical, lint-guarded); codex CHECK probe and LOCK latch; `credential_spent` class; claude expiry reader. Lane: local subagent. Merge `51da3d0`. |
| `t10` | delivered | migration `0058_actor_liveness.sql`; collector sink; worker `livenessGate` with `fallback_actor` and a derived `lane_liveness` record; resume clears the lock only. Lane: local subagent (Go). Merge `a0ed3a3`. |
| `t11` | delivered | `nodes doctor` fifth check `lane_liveness` (`checks=5`); `lanes/liveness-detector.sh` in deploy.sh; codex preflight login check advisory; `fallback_actor` docs; pre-check step in nodes-operator. Lane: local subagent. Merge `c7943eb`. |
| `t12` | delivered | `closed_pr_fact` / `closed_pull_event`; `fetch_closed_pulls` reuses the merged-PR listing. Lane: local subagent. Merge `e65ea8e`. |
| `t13` | delivered | `examples/cleanup/{workflow.yaml,cleanup.py}`; `internal/api/cancelreason.go` (`pr_merged`/`pr_closed` reasons). Lane: local subagent. Merge `3056b5e`. See `d3`. |
| `t14` | delivered | workflow 2.5.0 `analyse` node with verdicts + packages; one file per fact; cadence doc rewritten; `driver.sh` deleted. Lane: **developer actor**, run `01M25YD5Z7PZD7B0MJEZN9HF53`, handover `d83fb76`. Merge `18304aa`. |
| `t15` | delivered | `scripts/triage-rows.py` + `open-issue.sh --disposition`; dogfooded twice (#323, #324). Lane: local subagent. Merge `ee6c687`. |
| `t16` | delivered | `hand_turn` + `hand_turn_definition` kinds; `POST /v1alpha1/hand-turns` and `/hand-turn-definitions`; `hand_turns_by_stage` (confirmed only); `examples/hand-turn-observer`; `nodes hand-turn`. Lane: local subagent (Go). Merge `34ddc44` + fixup `57a0ea4`. |
| `t17` | delivered | stage nodes in pr-upkeep (2.4.0), cleanup (1.1.0), jira-intake (1.8.0); `pr_upkeep_jira` stage watermark. Lane: **planner actor**, run `01M25VT24ZXDA79W3B80VYESKZ`, handover `0de8b3a`. Merge `dd425ea`; operator fixup `8d8927b` (e2e fakes and fixture the actor could not run). |
| `t18` | delivered | workflow 2.6.0 `readiness` code node bound into `human-merges-pr`; upstream ask posted as agentculture/devague#119. Lane: **developer actor**, run `01M260089KYQBCXKZN3TH48QWM`, handover `3f63537`. Merge `f08ea40`. |
| `t19` | partial | this summary; obligations `o1`–`o17`, evidence `e1`–`e17`, deltas `b1`–`b3` filed (proposed). Not done: the owner's confirmations; the plan-md re-export (blocked on `plan confirm t4 t7 t19`). |

## Mid-work Decisions

All four deviation records were approved by the owner on 2026-09-10; quoted
here as the recorded decision.

- `d1` — wave 0: `t9` and `t3` re-routed from codex-thor/codex-orin to local subagents — both codex runs failed at execution with "refresh token was revoked" minutes after `codex login status` said logged in (#303); plan risk r3 prescribed exactly this.
- `d2` — `t4`: the run-column re-key from `gh:` to the Jira key is admitted by PATCH but no graph step performs it (the graph cannot address its own run); two deployment literals ride in the workflow; one developer session per orphan PR stamps the key into the PR body.
- `d3` — `t13`: the cleanup node's cancels need a principal the cancel route admits; agent bearers are not, so production needs the operator cookie or a bounded policy widening.
- `d4` — 2026-09-10: codex lanes still dead (same text, runs `01M25T4X…`/`01M25T4Z…`); `t7` → developer actor, `t17` → planner actor, per the direction "Culture Nodes actors with claude and codex".
- Not covered by a record: Go is not installed on thor, orin or the culture-claude account, so every Go-touching task (`t1`, `t5`, `t10`, `t13`, `t16`) ran on local subagents and every e2e/lint/markdownlint gate for the claude actors' packages was run by the merger.
- Not covered by a record: the shared subscription window hit its limit twice (11:00 IDT on the 7th with five parallel local builders; the credit wall on the 10th); the interrupted builders' worktrees kept every edit and were resumed or handed to actors as WIP commits (plan risk r9).
- Not covered by a record: the sweep schedule was paused for the build (r4/r8) and resumed at delivery, both by hand (#321).
- Not covered by a record: the build branch was split from the spec PR's branch (PR #322 stays documents-only).

## Drift From Plan

| Plan item | Reason for divergence | Classification |
|-----------|-----------------------|----------------|
| `t3`, `t9` (`d1`) | codex lanes refused every dispatch on a revoked refresh token; re-routed to local subagents | needs-follow-up |
| `t4` (`d2`) | no graph step performs the run re-key; two deployment literals in the workflow; an extra developer session per orphan PR | needs-follow-up |
| `t13` (`d3`) | the cancel route does not admit the runner/cleanup principal; production cancels need a policy decision | needs-follow-up |
| `t7`, `t17` (`d4`) | codex still dead on the 10th; built by the developer and planner actors instead | needs-follow-up |
| `t7` | `land_gate.py` resolved the land module by `sys.modules` name; with two copies of `land.py` loaded its refusals escaped the caller's handler — fixed in `cb4a89c` (recorded as `b1`) | acceptable |
| `t17` | the actor's e2e fakes and fixture were incomplete (no Go on its host to see it); fixed in `8d8927b` | acceptable |
| `t1`, `t5`, `t10`, `t13`, `t16` | split plan routed these to codex; built locally because no nodes actor has Go | acceptable |
| `t19` | records filed, not confirmed; plan-md not re-exported | needs-follow-up |

## Evidence

Read-only checks at commit `f08ea40` (t18 merge) and `cb4a89c` (evidence
run), 2026-09-10:

- tests: `uv run pytest -n auto -q` — 1476 passed, 6 subtests passed
- tests (serial, order-sensitive): `uv run pytest -p no:xdist tests/test_pr_upkeep_readiness.py tests/test_land_node.py tests/test_land_gate.py` — 74 passed
- tests: `go test ./tests/lint/... ./tests/e2e/... ./internal/compiler/... ./cmd/nodes/...` — all ok
- tests: `go test ./internal/api/... ./internal/worker/... ./internal/ledger/... ./internal/store/postgres/... ./internal/mesh/... ./internal/repair/... ./tests/deploy/...` (obligation selection, live Postgres via pgtest) — all ok
- adapters: `adapters/codex tests/test_liveness.py` 36 passed; `adapters/claude-code tests/test_liveness.py` 10 passed; `adapters/jira tests/test_create_issue.py` 18 passed
- lint: `scripts/lint-all.sh root` — all lint steps passed (triage ran: 89 open issues, all dispositioned)
- doctor: `uv run teken cli doctor . --strict` — healthy, `checks=5`
- commits: `7f65d31..f08ea40` on `build/loop-closure-claude-codex` (44 commits, 18 `merge tN:` commits, 242 files, +39806/−787)
- nodes runs: `01M25VSZBCWKX52JWWTWYZVKHM` (t7), `01M25VT24ZXDA79W3B80VYESKZ` (t17), `01M25YD5Z7PZD7B0MJEZN9HF53` (t14), `01M260089KYQBCXKZN3TH48QWM` (t18) — completed with handover refs; `01M1X6JM…`, `01M1X6JV…`, `01M25T4X…`, `01M25T4Z…`, `01M25XVB…`, `01M25XVBM…`, `01M25Y1R…` — codex, failed on the revoked token
- PRs / issues: #322 (spec + plan), #303, #308–#319, #321, #323, #324, agentculture/devague#119
- devague records: `o1`–`o17` — approved; `e1`–`e17` (all `pass`), `b1`–`b3` — proposed; `d1`–`d4` — approved

## Delivery Claims

| Claim | Confidence | Evidence |
|-------|------------|----------|
| `work_item` is its own run column and filter, stamped from the event payload, and every pr-upkeep.pr fact carries it | high | `e1` · `internal/api/runs_workitem_test.go` · `tests/test_pr_upkeep_sweep_work_item.py` |
| a handover ref lands on the target branch through the land node with leases, rebase, gate, one bump, reply and resolve, idempotently, never merging | high (tests) / unverified (live) | `e2`, `e10`, `e12`–`e14` · `tests/test_land_node.py`, `test_land_gate.py`, `test_land_reply.py` · no live landing ran: the land account is not bootstrapped on thor and Go is absent there |
| a spent codex credential reads `session_ok=false` within one probe, LOCK holds until resume AND a healthy fact, the worker reroutes to `fallback_actor` with a derived record | high (tests) / unverified (live) | `e3`, `e9` · adapters liveness tests · `internal/worker` liveness tests · the bridges on thor/orin still run pre-t9 code |
| the analysis node bundles same-rule findings and emits per-finding verdicts; the cadence doc says one tick per file | high | `e4` · `tests/test_pr_upkeep_analysis.py` |
| the cleanup node deletes only main-reachable refs, cancels parked runs with a reason, and lists everything | high (tests) / unverified (live) | `e5`, `e11` · `tests/test_cleanup_node.py` · 26 parked runs and 8 branches still present: the node is not deployed and its cancel principal is undecided (`d3`) |
| opening an issue through the script leaves the triage check green in one invocation | high | `e6` · #323 and #324 opened that way |
| `hand_turn` records exist under the unchanged authority matrix, stats count confirmed ones, the observer proposes the #307 turns | high (tests) / unverified (live) | `e7` · `internal/ledger/handturn_test.go` · the control plane on thor runs `94415be` (pre-t16), so no live `hand_turn` record exists; this cycle's hand-turn count is still prose |
| one stage comment per transition and the stage read back as the watermark | high (tests) / unverified (live) | `e8` · `tests/e2e/stagewriteback_test.go` · no ticket was driven live |
| the merge approval carries a readiness block and is reachable only after the collector | high (tests) / unverified (live) | `e17` · `tests/test_pr_upkeep_readiness.py` · e2e context_refs assertion |
| the loop's hand-turn count this cycle comes from the ledger | unverified | not met: counted in prose below, exactly as before this cycle |

## Code Review Round (2026-09-10)

`/code-review medium` over the build branch confirmed 18 findings. The eight
ranked ones were fixed as four packages and merged behind the same gate:

| Findings | Merge | Lane |
|---|---|---|
| 1 checkout lease misses live attempts; 2 lint-all exit 2 read as red; push rejections misclassified | `31954e9` | developer actor run `01M267E2QD42BQD42KN2R375V4` |
| 3 stage watermark suppressed `pr.merged`/`pr.closed`; 6 observer had no input binding; facts behind the Sonar loop; work-item URL encoding | `f03cf26` | planner actor run `01M267E5EHR6KE635WNV8RHZ64` |
| 4 LOCK expired after five minutes on async lanes; liveness gate ran after the per-actor gates | `feb9e59` + `84d6541` | local subagent (Go) |
| 5 deploy detector read the wrong document shape; 7 `JIRA_CREATE_PROJECTS` never written | `1cc5d69` + `1227f14` | qwen-developer actor run `01M268NAJ90JA6SD7S0ESMXS92` (first dispatch refused `yolo`; this bridge takes `auto-edit`) |
| 8 never-logged-in lane read as live; doctor all-clear over null facts | `c69226e` | developer actor run `01M26B4ZASPNEDDREXVERDM7S1` |

Findings 4 and 5 contradicted confirmed claims (LOCK holds; the detector shows
a dead lane) and are corrections to what this summary's Delivery Claims table
asserted at test strength; the tests now assert the corrected behaviour. The
five below-cap findings are issue #325.

## Remaining Work / Follow-up

- **Owner confirmations**: `devague plan confirm t4 t7 t19`, then `devague plan export` (the plan-md re-export into PR #322); `devague evidence`/`delta` confirms for `e1`–`e17`, `b1`–`b3` — 20 single confirms today, which is agentculture/devague#119's point.
- **Codex lanes** (#303, #324): `codex login` as `culture-codex` on thor with `CODEX_HOME=/home/culture-codex/.codex`; orin later; stop copying `auth.json` at bootstrap. Every codex dispatch this cycle (7 runs) failed on the revoked token.
- **Deploy and publish**: control plane (migrations 0057/0058, hand-turn routes, liveness), all seven bridges (liveness), the sweep and the six republished workflows (pr-upkeep 2.6.0, cleanup 1.1.0, jira-intake 1.8.0, land, hand-turn-observer) must land together with the sweep redeploy (a 2.6.0 contract refuses a pre-t2 fact). Then the live success signals above flip from `unverified`.
- **Land account on thor**: `bootstrap-accounts.sh thor` (root hand-turn), install Go for the land gate, `cutover.sh thor land`, two GitHub tokens; egress allowlist for `ssh://culture-claude@localhost/...` fetches from the runner (`d2`'s and r1's open point).
- **Cleanup principal** (`d3`): decide whether the runner/cleanup principal may cancel runs; until then the 26 parked #307 runs and 8 `review-fix/*` branches stay.
- **Orphan re-key** (`d2`): a control-plane step that performs the gh:→Jira PATCH.
- **pr.opened dormant in prod** (#323, found by t2): carry `state`/`created_at`/`html_url` through the listing.
- **Schedule pause/resume verb** (#321): two hand-typed PATCHes this cycle.
- **Hand-turns this cycle, in prose** (the number the ledger should carry next cycle): two sweep pauses/resumes, seven checkout resets, four handover harvests, three merge-conflict reconciles, two actor-gate fixups, two codex re-login attempts in the wrong home, one Go/node absence discovered per host, one root bootstrap still owed — the six recurring turns from PR #307 are now nodes or scripts on the branch, none is yet proven live.
