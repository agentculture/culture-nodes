# Delivery Summary — presentable-floor-before-oauth

plan: `presentable-floor-before-oauth` · run: `partial` · date: `2026-08-30`
baseline: `devague summary skeleton`

## Intent

Make Culture Nodes presentable and operable from Jira and its own site
before the OAuth / login-from-anywhere cycle: a green sweep that names its
failures, pending decisions that reach the people who decide them (one view,
the ticket, the ticket page, Discord), repeat sweep runs collapsed, the site's
visible defects fixed, an honest backlog, and every fix package dispatched
through culture-nodes itself. Initiated from Jira SCRUM-6; spec
`docs/specs/2026-08-30-presentable-floor-before-oauth.md`; plan
`docs/plans/2026-08-30-presentable-floor-before-oauth.md` (31 tasks, 8
waves, 70 coverage targets). Executed as PR #263 (0.45.0, `d6a253e`) plus
hotfix PR #264 (0.45.1, `15fefde`), both deployed to thor and orin.

## Planned Work

Quoted verbatim from the `devague summary` skeleton (task id and summary;
the bracketed lane is part of the plan text):

- `t1` — [operator] Close six issues on cited evidence: #205 (0 dial-in 401s/24h, bridge d2f8ad2), #203 (delivery record), #125 (runs 01M16KGAEH/01M16JXY2Z/01M16JMT12), #117 (audit run 01M08681PJ), #136 (dup of #121); retype #248 to Record and close via scripts/close-issue.sh --artifact docs/audits/2026-08-29-agents-as-os-users-cutover.md; regenerate docs/triage
- `t2` — [operator] Write docs/audits/2026-08-30-lan-and-credential-dependencies.md: every LAN address, HTTP-only surface, shared secret and personal credential from c16 with file:line or the prod GET that shows it
- `t3` — [operator] File the named big features as typed Feature issues via scripts/open-issue.sh (decisions on the ticket page beyond c6, system moves the board to Done, actor-side Jira read, fleet-health sweep, #173 silence timeout comment, Runs server-side filters beyond c4, Inbox pagination beyond c5); comment on #173 naming c28's freeze handling; comment on #111 and #6 linking the t2 audit
- `t4` — [codex-orin] Write three docs/decisions/ records and close their issues with close-issue.sh --artifact: #129 (polled-forever orphan accepted or bounded, runnerasync.go:408), #221 (SonarCloud boundary split; nodes sonar verb is #218), #171 (unauthenticated artifact read posture + one publish->consume example)
- `t5` — [developer] Deploy grant safety: `install_jira_runner_env` merges into runner-secrets.env (replaces only the Jira keys, refuses on a non-first deploy when the pair is unset); new deploy/prod/lanes/grant-check.sh diffs environmentRefs of the latest version per `workflow_key` that has an enabled schedule/trigger against runner.env+runner-secrets.env key names on each host and fails the deploy naming the key, printing names never values; every lane that rewrites runner\*.env writes a `.bak-<ts>` first and prints the restore command; deploy/prod/README.md names the five grants, their files and the rollback
- `t6` — [codex-thor] Rejection reason persisted and rendered: dispatch.go StateRejected path carries res.Error.Message into attempts.result.error.{class,detail}; run projection, attempt.completed event payload, scripts/nodes-op.sh run and web RunView node panel render result.error.detail (closes #241)
- `t7` — [codex-orin] Runs API filters + list UI: GET /v1alpha1/runs accepts `workflow_key` and state filters (join runs->workflows); RunsList shows workflow key, a state filter, groups consecutive failed runs of one workflow under one row with a count badge, and a Load more using the Jobs cursor; .runs-`board__columns` gets overflow-x auto with a visible scrollbar at 1440px
- `t8` — [codex-thor] Stats and Node Graphs fixes: cache ratio = cached/(input+cached) in internal/api/types.go:205 and web/src/domain/usage.ts:40; NodeGraphs recent runs use GET /v1alpha1/runs?`workflow_key`= so a workflow with runs never shows 'No runs yet'
- `t9` — [developer] Schedule backoff and singleton alert: when the previous run of a schedule ended `contract_rejected` with identical result.error.detail, the next mint is suppressed (suppressed counter on the schedule) but a probe run still mints at most every 30 minutes; the Nth consecutive failure (`NODES_SWEEP_FAILURE_ALERT_AFTER`) creates one pending human task per schedule, re-raised only after it is decided; migration 0049 adds the columns; migrations/pending/0036 untouched
- `t10` — [codex-orin] Pending decisions view: GET /v1alpha1/human-tasks?status=pending (plus pending proposed claims) grouped by run/ticket feeds a Decisions 'Pending' tab paginated 25 per page; each item renders only the outcomes the engine accepts for its kind (`allowed_outcomes` + `decision_schema_ref`; defer = deadline extension, not an outcome) with schema-valid payloads; the decider actor id is remembered with the held token; the per-record checkbox is labelled
- `t11` — [developer] Human-task fan-out and expiry in the engine: on human-task creation emit one fan-out per task — for a run whose input names a Jira key: one Jira comment (options + absolute page link) and a transition to Pending (second allowlisted target on the jira bridge); for a `github_pr`-sourced run: one GitHub PR comment; always one notify (Discord) post carrying the options and the page — and a pr.merged consumer that expires tasks whose subject PR merged (reason `pr_merged`); backfill command for the 26 existing stale approvals
- `t12` — [codex-orin] Sweep dedupe by finding id: emission is keyed by finding id and consults GET /v1alpha1/runs?state=running&`workflow_key`=pr-upkeep for an existing run carrying that finding id before emitting
- `t13` — [codex-thor] Ticket description reaches agents: `jira_work_items` facts carry description; jira-intake and spec-chain-lane input contracts and instructions accept it; workflows republished
- `t14` — [codex-orin] Runner completion 405: align the runner's completion POST (protocolclient.go:48 CallbackPathFormat) with the API's callback route and add an API test for the runner completion path
- `t15` — [codex-thor] Code-node stdout readable: GET /v1alpha1/attempts/{id}/artifacts/stdout resolves a code node's `stdout_ref` (artifact store id), or GET /v1alpha1/artifacts/{id} is added; test covers the code-node path
- `t16` — [developer] Reachable page link: `NODES_UI_BASE_URL` wired into compose.thor.yml and compose.orin.yml (api, worker, scheduler) and both prod.env via install-secrets.sh; jiraticketreport.go renders an absolute link; docs state the LAN/tailscale reachability limit
- `t17` — [developer] Freeze handling: handleFreezeTicket cancels runs whose subject is the ticket with reason `ticket_frozen` when the ticket is Done, parks them with the same reason otherwise; the reason shows on each run and in the ticket page banner
- `t18` — [codex-thor] Ticket page options: GET /v1alpha1/tickets/{id} lists the ticket's pending human tasks with their options and a `ticket_url` composed from the fact's `details_url`; TicketView renders the options as buttons under the held token and always shows Open in Jira
- `t19` — [codex-thor] #116: CompleteAttempt takes StartedAt from the invocation row (`actor_invocations`/`runner_invocations`.`created_at`) instead of stamping StartedAt=CompletedAt=now (internal/engine/complete.go:300); async-path test
- `t20` — [codex-orin] #162: DispositionTableError(ValueError) in scripts/triage-report.py raised at the five dispositions() sites and caught before the broad clause so a malformed table exits 1 (finding) not 2; test in tests/`test_triage_report_types.py`
- `t21` — [codex-thor] #172: rewrite examples/pr-upkeep/README.md §The graph and §Idle vs blocked against sweep-cycle.workflow.yaml + v2 workflow.yaml; keep Deployment configuration
- `t22` — [codex-orin] #186: POST /v1alpha1/namespaces over the existing sqlc insert (+OpenAPI, test); deploy/prod/register-actor.sh namespace lookup moves from `run_psql` to the API
- `t23` — [codex-thor] #178: gate reports set ledger AttemptID only when the attempts row exists and keep the ref in record data (internal/api/gatereports.go:217/298/322); test
- `t24` — [codex-orin] #183 legibility half: a completion whose `origin_actor_id` mismatches the dispatched actor is refused at accept time as `contract_rejected` with the mismatch named, no redispatch; custody half stays with #111
- `t25` — [developer] #133 + #135: audit-credentials.sh compares the password embedded in `NODES_DATABASE_URL` to `POSTGRES_PASSWORD` and reports divergence by name; `env_has` becomes last-wins like `env_get`; no `DATABASE_SSLMODE` written beside a URL; the thor:18080 literal is parametrised (tests/deploy/`prodenvmerge_test.go`)
- `t26` — [operator] #147 interim, #128, #10: add the per-bridge dial-in EnvironmentFile line to the intake/planner/verifier units and issue their credentials; delete thor's pre-deploy dump and decide the runner event token on #128; close #10 items 1-2 by comment and split item 3 (runner-service revision column) into its own Feature
- `t27` — [codex-orin] Site polish bundle: route-specific document.title in App.tsx RouteWatcher; Header link to docs and a /v1alpha1/version readout; run id once on RunView; shared control classes and labels-above-inputs on PlanView and GenerateWorkflow; RunStateChip + humanised timestamps on RunsList and JobsTable; label on the Decisions per-record checkbox; claim statements rendered as text; scroll affordance on board columns and mobile tables; 'load a sample' on /workflows/new; Tickets entry in the header nav; before/after screenshots from scratchpad/shots in the PR
- `t28` — [codex-thor] Human-facing docs: docs/drive-from-jira.md (what a ticket must contain; To Do -> intake, In Progress -> /think, bare comment -> consumer, marked-question reply, PR merged -> freeze; what each system comment means; how to open the page, where the token comes from, how to decide; what Done means and who moves it; the reachability limit; answer on the page now that the bot id is set) linked from README.md and the Header; examples/jira-intake/README.md; deploy/prod/README.md lists the five runner grants
- `t29` — [operator] Deploy the integration tip to thor and orin (deploy.sh, first run of grant-check.sh and the new migration), add the second Jira transition target (Pending) to the jira bridge config on both hosts, verify /v1alpha1/version on both
- `t30` — [operator] Measurement sitting on one new SCRUM ticket: intake fires with the bot id set (park v2), two intake milestones -> one page-link comment (#200 signal 9), description quoted in intake claims, a human task -> Jira comment + Pending + page options + Discord + (PR-sourced) PR comment; decide human task 01M16FX0BWK9X6TKE9BHAAW88Y; run the stale-approval backfill; confirm the SCRUM-5 run is no longer running; record before/after pending counts and every id in docs/audits/2026-08-30-measurement-sitting.md; walk drive-from-jira.md as each reader role
- `t31` — [operator] Delivery summary and honesty gate: /summarize-delivery ticks every announcement clause against a run id, commit or docs/ artifact; every cycle PR body lists its culture-nodes run ids (in-session exceptions declared with reason); gh open-issue count and a triage regen with zero untyped rows cited; signal 1 (7 days of green sweeps) recorded as pending with the date it can be read

## Actual Delivery

Run ids are the culture-nodes runs that produced each package (the
dogfooding record in PR #263's body, grades included); "in-session" is the
operator lane declared per h13. Lanes that differ from the plan's bracket
are drift, listed below.

| Plan task | Status | What actually landed |
|-----------|--------|----------------------|
| `t1` | delivered | in-session: #205, #203, #125, #117, #136 closed on cited evidence; #248 retyped Record and closed with `--artifact`; triage regenerated (PR #263) |
| `t2` | delivered | in-session: `docs/audits/2026-08-30-lan-and-credential-dependencies.md` (PR #263) |
| `t3` | delivered | in-session: Features #255–#262 filed typed via `scripts/open-issue.sh`; comments on #173, #111, #6 (PR #263) |
| `t4` | delivered | codex-thor `01M194KT7AS2BNBZDM2VADXHSX` (grade 4): three `docs/decisions/2026-08-30-*.md`, #129 #221 #171 closed with `--artifact`; `examples/artifact-publish-consume/workflow.yaml` |
| `t5` | delivered | developer `01M196H64GFSKVPHKDNQYZMDR2` (grade 5): merge-not-truncate, `lanes/grant-check.sh`, `.bak-<ts>` on every grant rewrite, README; first live run passed on both hosts at t29 |
| `t6` | delivered | codex-thor `01M191YQWTPWF8SAJG0B1SEST5` (grade 4): `result.error.{class,detail}` persisted and rendered in run projection, event, `nodes-op.sh run`, RunView; closes #241 |
| `t7` | delivered | codex-orin `01M191YR0NKKC6WAF5D50JM6BP` (grade 4): `workflow_key`/state filters, grouped failed runs with counter, Load more, board scrollbar |
| `t8` | delivered | developer `01M19DQ8XWH7SZ6ABQ6PT1345T` (grade 4) after codex-thor `01M194XR594VP2G6B5X9F04XVW` was lost to `capacity_exhausted`: cache ratio ≤ 100 %, per-workflow recent runs |
| `t9` | delivered | developer `01M197N7C19V5QTAMYFFHRXAC5` (grade 4): schedule backoff to a 30-minute probe, singleton failure alert task, migration (renumbered 0050 at the gate; 0049 became the `started_at` nullable fix) |
| `t10` | delivered | codex-orin `01M192KMZR9X9ZFBB2P7Y74PYM` (grade 3): Decisions Pending tab, `allowed_outcomes`, remembered decider, labelled checkbox |
| `t11` | partial | developer `01M199DM50AGFV119XSGTC3SAF` (grade 5): fan-out outbox (migration 0051, unique per task+channel) → Jira comment + `Pending` transition + Discord; `pr.merged` expiry; `nodes expire-approvals` backfill. **Missing**: the GitHub PR-comment leg (`d1`) |
| `t12` | delivered | developer `01M19FTQ2HG3484H3EJSFA9CVS` (grade 4): sweep emission keyed by finding id, running-run check before emit |
| `t13` | delivered | codex-thor `01M192FVXCHT3N9QJ9RCYSY4KX` (grade 4): description on `jira_work_items` facts, contracts and instructions accept it; republished at t29 and measured (audit row 4) |
| `t14` | delivered | codex-orin `01M1934GCDB3HAGQ9T8TVETQM6` (grade 4): runner completion path aligned; 0 runner 405s since deploy |
| `t15` | delivered | codex-thor `01M192RW225VWZME89EPTA9HHC` (grade 4): code-node stdout resolvable through the attempt artifacts route |
| `t16` | delivered | developer `01M198S456MWRVN4CVK60T6HFX` (grade 4): `NODES_UI_BASE_URL` in both compose files and prod.env; absolute page link measured (audit row 2) |
| `t17` | delivered | developer `01M19CH41FNW22BQTKKBBCBMAX` (grade 4): freeze cancels/parks with `ticket_frozen`; proven on SCRUM-5 (run cancelled) and SCRUM-7 (frozen by #264's mention, 0 runs affected) |
| `t18` | delivered | developer `01M19EQ3CPDDY45Q01Y4AGYG63` (grade 5) — planned for codex-thor, moved to the developer lane after codex capacity ran out: ticket projection carries pending tasks + options + `ticket_url`; TicketView buttons and Open in Jira |
| `t19` | delivered | codex-thor `01M1937YSK3E98C7ZM8JJGJ1GJ` (grade 3): `StartedAt` from the invocation row; gate added migration 0049 (nullable `started_at`) and the pointer/`omitempty` serialization fix after the colleague review |
| `t20` | delivered | codex-orin `01M194BBJHZ2ED0NK62XJ2066S` (grade 4): `DispositionTableError`, exit 1 on a malformed table; closes #162 |
| `t21` | delivered | codex-orin `01M193KWE4ASK2FQ5ER4MZTGP1` (grade 3) left v1 node names; follow-up developer `01M19HDQ19ZVDKXY91316PMA3H` (grade 4) finished it; closes #172 |
| `t22` | delivered | codex-orin `01M194KT3XC8V99B1J5R5SSY5D` (grade 4): `POST /v1alpha1/namespaces`; register-actor.sh reads the API; closes #186 |
| `t23` | delivered | codex-thor `01M193KWJ3P0MQRTQSPDMTEKVV` (grade 4): ledger `AttemptID` only when the row exists; closes #178 |
| `t24` | delivered, then corrected | codex-thor `01M194BBPZPRDGFJXXA3FFYHF3` (grade 4) — planned for codex-orin: identity refusal at accept time. In prod it refused every spark claude-bridge completion (row id ≠ registration row); hotfix PR #264 (`15fefde`) compares by `actor_key`, keeps the refusal for unknown rows and different keys, and no longer turns a store error into a refusal (`d2`) |
| `t25` | delivered | developer `01M19BCV50V98GZM9XAWS2FZGE` (grade 5): password divergence by name, last-wins `env_has`, thor:18080 parametrised; closes #133 #135 |
| `t26` | delivered | in-session: dial-in EnvironmentFile lines and credentials for intake/planner/verifier; thor pre-deploy dump removed; #128 decided; #10 items 1–2 closed, item 3 split to #262 |
| `t27` | delivered | developer `01M19GEJG8Z6VR32JVX2TZCC4W` (grade 5) — planned for codex-orin: the polish bundle with before/after screenshots in PR #263 |
| `t28` | delivered | developer `01M19HTAA9KDK8DCM01XQ2ESDH` (grade 4) — planned for codex-thor: `docs/drive-from-jira.md`, `examples/jira-intake/README.md`, deploy README grants list |
| `t29` | delivered | in-session: `deploy.sh thor` + `orin` at `d6a253e` (migrations 0049–0052, first live `grant-check.sh` pass, `JIRA_TRANSITION_TARGET=In Progress,Pending`, `human_task_expiry` registered, sweep source re-pinned) and again at `15fefde`; `/v1alpha1/version` verified both times. Hand-turn: the Jira bridge was reinstalled by hand (`JIRA_SITE` unset in the deploying shell) |
| `t30` | partial | in-session: `docs/audits/2026-08-30-measurement-sitting.md` — 11 rows proven on SCRUM-7 (intake with the bot id, one page link over two milestones, description quoted, fan-out to Jira comment + Pending + Discord, decision round trip, SCRUM-5 freeze). **Missing**: the PR-comment leg (`d1`, no actor); the remint alert `01M16FX0BWK9X6TKE9BHAAW88Y` has no option to decide; 8 stale approvals staged for the operator (secret access), 19 expired via `nodes expire-approvals`; the reader-role walk of drive-from-jira.md was not performed |
| `t31` | delivered | this document; PR #263's body lists every run id with in-session exceptions declared; open issues 49 with zero untyped rows in `docs/triage/open-issues.md`; signal 1 pending until 2026-09-06 |

## Mid-work Decisions

- `d1` — q5 said PR-sourced human tasks echo as a GitHub PR comment plus Discord; t11 shipped Discord-only for PR-sourced tasks because no registered actor can post a GitHub PR comment (the human-inbox tracker writes only to its own submit surface; the agent bridges advertise no GitHub write verb) — recorded in internal/humanfanout's package doc and migration 0051's header. Also: the pr.merged consumer only expires tasks for PRs whose branch or body carries a Jira key, so the 26 stale approvals for #236/#238/#244/#246 need the operator-run 'nodes expire-approvals --pr …' backfill at t30 (the operator is the merge-fact source). — no GitHub-writing actor exists in the registry; adding one is a new bridge verb outside this quality cycle (c30). The PR-comment leg becomes a follow-up on #256/#258's fleet issues or a small Feature once a GitHub write verb exists. (approved)
- `d2` (proposed, not yet a decision) — Operator-lane hotfix outside the plan: t24's identity refusal (merged in PR #263) rejected every spark claude-bridge completion on prod because the bridge's issued identity row and the dispatched registration row are different rows of one actor_key. Fixed in-session (PR #264, 0.45.1: compare by actor_key via CallbackStore.ActorKey) rather than through the sweep's fix lane, because that lane runs on the developer actor the bug breaks. The user approved the hotfix and its deploy in-session ("Confirmed", "Squash merged"); the `d2` record itself awaits `devague deviate` confirmation.
- Codex lane exhausted after 13 sessions (`capacity_exhausted`, risk r7): t8, t18, t27, t28 and the t21 follow-up moved from the codex actors to the developer lane; the assignment table above names each move.
- Review fixes go through the system (user directive): PR #263's Qodo findings and PR #264's were dispatched by the pr-upkeep sweep to the developer lane, and the operator gate compared and adopted their diffs. On #264 the sweep's own completion was refused by the bug under repair, so the operator committed the lane's diff (867062c) — recorded on #93 with the checkout collision it caused (risk r8).
- The merge-freeze fires on a mention: #264's body cited SCRUM-7 as evidence and froze it on merge. Left as designed and recorded in the audit; whether freeze should need a closes-style reference is a decision for the next cycle.
- Migration numbering: t19's gate fix took 0049 (nullable `started_at`), so t9's columns shipped as 0050 and t11's outbox as 0051; 0052 registers `human_task_expiry`.

## Drift From Plan

| Plan item | Reason for divergence | Classification |
|-----------|-----------------------|----------------|
| `t11` (`d1`) | no GitHub-writing actor exists in the registry; adding one is a new bridge verb outside this quality cycle (c30). The PR-comment leg becomes a follow-up on #256/#258's fleet issues or a small Feature once a GitHub write verb exists. | needs-follow-up |
| `t24` (`d2`, proposed) | the refusal shipped by t24 compared row ids and broke every claude-bridge completion in prod; corrected by hotfix #264 in the operator lane because the sweep's fix lane was the lane it broke | risky |
| `t8`, `t18`, `t27`, `t28`, `t21` follow-up | planned for codex-thor/orin, delivered by the developer lane after the codex window hit `capacity_exhausted` (r7); same acceptance criteria, different actor in the comparative record | acceptable |
| `t9` | migration number 0050 instead of the planned 0049 (taken by the t19 gate fix) | acceptable |
| `t30` | PR-comment leg unmeasurable (`d1`); remint alert task has no decidable option; the 8 remaining stale approvals are staged, not decided (the assistant may not read the decision secret in bulk); reader-role walk of drive-from-jira.md not performed | needs-follow-up |
| `t29` | the Jira comment actor is skipped by `deploy.sh` when `JIRA_SITE` is unset in the deploying shell; reinstalled by hand once | needs-follow-up |

## Evidence

- tests: `go test ./...` on `867062c` (the #264 head) — pass, including `tests/lint` (1000-line, neutrality, workspace_measured guards) and `internal/actors` `TestOriginActorMismatch*` (4 new)
- tests: PR #263 gates — Go `./...` green, web 589/589, `scripts/lint-all.sh root` green, production web build green (PR body, "Gates")
- CI: PR #263 all checks green after one webglass re-run; PR #264 10 checks green, SonarCloud quality gate passed
- commits: `d6a253e` (PR #263), `15fefde` (PR #264); integration branch history squashed into those two
- prod: `GET /v1alpha1/version` on thor → `15fefdef6143…`, orin worker at parity (deploy log `deploy6.log`: "sweep schedule: resumed (api and workers all at 15fefdef6143)")
- runs: 26 dogfooded packages listed in PR #263's body; measurement runs `01M19PG0QGN6QA77Q6TNMSQQKJ` (failed, bug), `01M19QXR5WW2DQRCHBFW3VCZNV` (failed, bug), `01M19WDJC189VYJCKA1YFN08JJ` (completed), `01M19WKKGTN1QSKH9V25F3019K` (completed)
- issues: closed #116 #162 #172 #178 #186 #133 #135 #205 #203 #125 #117 #136 #248 #129 #221 #171 #10 #128; filed #253 #254 #255–#262 #265; comments on #93 #173 #111 #6; open count 49 with zero untyped rows in `docs/triage/open-issues.md`
- docs: `docs/audits/2026-08-30-measurement-sitting.md`, `docs/audits/2026-08-30-lan-and-credential-dependencies.md`, `docs/drive-from-jira.md`, three `docs/decisions/2026-08-30-*.md`

## Delivery Claims

Each announcement clause of the spec, ticked against evidence:

| Claim | Confidence | Evidence |
|-------|------------|----------|
| the 5-minute sweep runs green again | high | runs list: every `5764a3c2…` sweep-cycle run since 15:57 IDT `completed`; root cause #253 closed; `grant-check.sh` live pass in `deploy5.log` |
| a failed sweep names its reason where an operator looks | high | t6 run `01M191YQWTPWF8SAJG0B1SEST5`; `nodes-op.sh run 01M19QXR5WW2DQRCHBFW3VCZNV` prints `identity: origin_actor_id … is not the dispatched actor …` |
| pending human decisions are collected in one view with the options the engine accepts | medium | t10 run `01M192KMZR9X9ZFBB2P7Y74PYM`, `web/src/routes/Inbox.tsx` `/v1alpha1/human-tasks?status=pending`; not driven in a browser on prod |
| a decision is echoed onto the Jira ticket, moves it to Pending, and reaches Discord | high | audit rows 5–7: SCRUM-7 comment 20:48:50, status `Pending`, notify record 204 |
| the options are on the ticket page | medium | audit row 9: `GET /v1alpha1/tickets/SCRUM-7` lists the task; SPA not driven |
| stale approvals expire when their PR merges | medium | `pr.merged` consumer (t11); 19 expired by `nodes expire-approvals`; the merged-before-deploy 8 are staged (`d1`) |
| repeat sweep runs no longer bury real work | medium | t7 run `01M191YR0NKKC6WAF5D50JM6BP` (grouped failed runs + counter); t9 backoff — not re-observed on prod because the sweep has not failed since deploy |
| every ticket has a reachable page link | high | audit row 2: absolute `http://thor:18080/tickets/SCRUM-7` |
| agents receive the ticket description | high | audit row 4: `heliotrope-ledger-4471` quoted |
| freeze ends the ticket's runs — the ones live at freeze time | medium | c28 as scoped: `freezeTicketRuns` cancels (Done) or parks (any other status) the runs live when the freeze row is written. SCRUM-5's run cancelled (earlier sitting); SCRUM-7's #264 freeze affected 0 runs. It does **not** stop later runs — audit finding 2: rows 4–8 post-date the freeze — it closes the ticket's reply form. |
| the four site defects and the polish bundle are shipped | medium | t8/t27 runs; screenshots in PR #263; not re-verified on prod after deploy |
| the backlog is honest — closures on evidence, big features as typed issues | high | issue list above; `docs/triage/open-issues.md` regenerated, zero untyped |
| every fix package in this cycle is assigned through culture-nodes itself | high | 26 run ids in PR #263's body; in-session exceptions declared (t1 t2 t3 t26 t29 t30 t31 and the #264 hotfix) |
| a docs page tells a human how to drive the loop from Jira | high | `docs/drive-from-jira.md` (t28) — but the reader-role walk was not performed |
| PR-sourced decisions reach the PR thread | unverified | `d1`: no actor can post; not claimed |
| seven days of green sweeps (signal 1) | unverified | readable on 2026-09-06 |

## Remaining Work / Follow-up

- `t11` / `d1` — GitHub PR-comment leg of the fan-out: needs a bridge verb that writes to a PR thread (#256/#258 or a small Feature); until then PR-sourced tasks fan out to Discord only.
- `t30` — run `decide-stale-approvals.sh` (operator hand-turn: 8 approvals for merged #223/#209); give `trigger_remint_exhausted` alerts an acknowledge outcome so `01M16FX0BWK9X6TKE9BHAAW88Y` can close; walk `docs/drive-from-jira.md` as each reader role; re-measure the intake → spec-lane chain on an unfrozen ticket moved by a second account.
- `d2` — confirm or reject the deviation record in devague (user decision); the code is merged and deployed either way.
- #265 — fan-out lists `expired` as an option and names the node by kind.
- #93 — per-run checkouts; the shared checkout collided twice on 2026-08-30.
- `t29` — make `deploy.sh` reinstall the Jira comment actor without `JIRA_SITE` in the deploying shell (read it from the host's `jira-bridge.env`), or fail loudly; today it skips silently.
- Merge-freeze on mention — decide whether freeze requires a closes-style reference (product decision; SCRUM-7 froze on an evidence citation).
- Freeze does not gate new runs — a frozen ticket refuses page replies, but intake and the sweep still mint runs on it (audit finding 2). Whether a freeze should also refuse new runs is undecided; today it is a product question, not a delivered behaviour.
- Sweep fix node works one finding per tick — a PR with N findings takes N ticks; fold into the next sweep cycle.
- 26 stale worktrees under `../.worktrees.culture-nodes/` from this and earlier fan-outs — tear down with `git worktree remove`.
- Signal 1 — read on 2026-09-06: seven consecutive days of green sweep-cycle runs.
