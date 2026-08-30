# Build Plan — presentable-floor-before-oauth

slug: `presentable-floor-before-oauth` · status: `exported` · from frame: `presentable-floor-before-oauth`

> Culture Nodes is presentable and operable from Jira and its own site: the 5-minute sweep runs green again and a failed sweep names its reason where an operator looks; pending human decisions are collected in one view with up to 3-4 options each, echoed onto the Jira ticket, and stale ones expire when their PR merges; repeat sweep runs no longer bury real work in the runs list; the site's four visible defects are fixed and every ticket has a reachable page link; the backlog is honest — every issue that no longer reproduces is closed on evidence and every big feature is a named issue — and every fix package in this cycle is assigned through culture-nodes itself. This is the clean floor before the OAuth / login-from-anywhere cycle.

## Tasks

### t1 — \[operator\] Close six issues on cited evidence: #205 (0 dial-in 401s/24h, bridge d2f8ad2), #203 (delivery record), #125 (runs 01M16KGAEH/01M16JXY2Z/01M16JMT12), #117 (audit run 01M08681PJ), #136 (dup of #121); retype #248 to Record and close via scripts/close-issue.sh --artifact docs/audits/2026-08-29-agents-as-os-users-cutover.md; regenerate docs/triage

- covers: c21, h14
- acceptance:
  - each closing comment cites the run id, commit or docs/ path named in c21 and is signed
  - \#248 shows type Record and is closed by close-issue.sh; docs/triage/open-issues.md regenerated with zero untyped rows

### t2 — \[operator\] Write docs/audits/2026-08-30-lan-and-credential-dependencies.md: every LAN address, HTTP-only surface, shared secret and personal credential from c16 with file:line or the prod GET that shows it

- covers: c16, h21
- acceptance:
  - every item in c16 appears with a file:line or a GET; the file is tracked by git
  - no cycle PR changes auth, TLS, `NODES_HUMAN_DECISION_TOKEN_SECRET` or the Jira credential (checked at the final gate)

### t3 — \[operator\] File the named big features as typed Feature issues via scripts/open-issue.sh (decisions on the ticket page beyond c6, system moves the board to Done, actor-side Jira read, fleet-health sweep, #173 silence timeout comment, Runs server-side filters beyond c4, Inbox pagination beyond c5); comment on #173 naming c28's freeze handling; comment on #111 and #6 linking the t2 audit

- depends on: t2
- covers: c25, h22, c29, h23, c34, h27
- acceptance:
  - each named feature exists as an open typed issue; none is touched by a cycle PR except by comment
  - \#173, #111 and #6 each carry the linking comment

### t4 — \[codex-orin\] Write three docs/decisions/ records and close their issues with close-issue.sh --artifact: #129 (polled-forever orphan accepted or bounded, runnerasync.go:408), #221 (SonarCloud boundary split; nodes sonar verb is #218), #171 (unauthenticated artifact read posture + one publish->consume example)

- covers: c23, h16
- acceptance:
  - three files exist under docs/decisions/ on the branch, each closed issue points at its file

### t5 — \[developer\] Deploy grant safety: `install_jira_runner_env` merges into runner-secrets.env (replaces only the Jira keys, refuses on a non-first deploy when the pair is unset); new deploy/prod/lanes/grant-check.sh diffs environmentRefs of the latest version per `workflow_key` that has an enabled schedule/trigger against runner.env+runner-secrets.env key names on each host and fails the deploy naming the key, printing names never values; every lane that rewrites runner\*.env writes a .bak-`ts` first and prints the restore command; deploy/prod/README.md names the five grants, their files and the rollback

- covers: c2, h1, c41, h34, c42, h35
- acceptance:
  - tests/deploy: a runner-secrets.env holding five keys survives the Jira lane with the pair unset (three non-Jira keys intact)
  - tests/deploy: a fake host missing one declared ref fails the deploy naming that key; a superseded workflow version's extra ref does not fail it; grepping the output for a fixture secret value finds nothing
  - tests/deploy: after a run, runner.env.bak-`ts` and runner-secrets.env.bak-`ts` hold the prior bytes; README documents the restore command

### t6 — \[codex-thor\] Rejection reason persisted and rendered: dispatch.go StateRejected path carries res.Error.Message into attempts.result.error.{class,detail}; run projection, attempt.completed event payload, scripts/nodes-op.sh run and web RunView node panel render result.error.detail (closes #241)

- covers: c3, h2
- acceptance:
  - internal/worker test with a fake runner returning error.message X asserts X on GET /v1alpha1/runs/{id} attempt result and in the attempt.completed payload
  - nodes-op.sh run output and a RunView vitest show the detail string

### t7 — \[codex-orin\] Runs API filters + list UI: GET /v1alpha1/runs accepts `workflow_key` and state filters (join runs->workflows); RunsList shows workflow key, a state filter, groups consecutive failed runs of one workflow under one row with a count badge, and a Load more using the Jobs cursor; .runs-`board__columns` gets overflow-x auto with a visible scrollbar at 1440px

- covers: c4, h3, c8, h7
- acceptance:
  - API test: ?`workflow_key`=pr-upkeep-sweep-cycle&state=failed returns only those runs
  - vitest: 50 consecutive failed sweep runs render one collapsed row with count 50 and the workflow key; Load more fetches the next page; board container has overflow-x auto

### t8 — \[codex-thor\] Stats and Node Graphs fixes: cache ratio = cached/(input+cached) in internal/api/types.go:205 and web/src/domain/usage.ts:40; NodeGraphs recent runs use GET /v1alpha1/runs?`workflow_key`= so a workflow with runs never shows 'No runs yet'

- depends on: t7
- covers: c8, h7
- acceptance:
  - unit tests pin the ratio <= 100% for `cache_read` outside `input_tokens`
  - NodeGraphs vitest: a workflow with runs in the fixture renders them, not the empty state

### t9 — \[developer\] Schedule backoff and singleton alert: when the previous run of a schedule ended `contract_rejected` with identical result.error.detail, the next mint is suppressed (suppressed counter on the schedule) but a probe run still mints at most every 30 minutes; the Nth consecutive failure (`NODES_SWEEP_FAILURE_ALERT_AFTER`) creates one pending human task per schedule, re-raised only after it is decided; migration 0049 adds the columns; migrations/pending/0036 untouched

- covers: c40, h33, c4, h3, c44, h37
- acceptance:
  - engine test: with a fixed failing runner a 5-minute schedule mints at most one run per 30 minutes and exactly one pending alert task exists; when the runner is fixed the next probe completes with no operator action
  - `migrations_test.go` passes with 0036 still in pending/; new migration is 0049

### t10 — \[codex-orin\] Pending decisions view: GET /v1alpha1/human-tasks?status=pending (plus pending proposed claims) grouped by run/ticket feeds a Decisions 'Pending' tab paginated 25 per page; each item renders only the outcomes the engine accepts for its kind (`allowed_outcomes` + `decision_schema_ref`; defer = deadline extension, not an outcome) with schema-valid payloads; the decider actor id is remembered with the held token; the per-record checkbox is labelled

- covers: c5, h4
- acceptance:
  - vitest with a 200-item fixture: 8 pages of 25, option buttons per item, clicking one POSTs the matching decision under the held token and removes the item
  - table test over kinds approval and `trigger_remint_exhausted` renders only accepted outcomes with schema-validated payloads

### t11 — \[developer\] Human-task fan-out and expiry in the engine: on human-task creation emit one fan-out per task — for a run whose input names a Jira key: one Jira comment (options + absolute page link) and a transition to Pending (second allowlisted target on the jira bridge); for a `github_pr`-sourced run: one GitHub PR comment; always one notify (Discord) post carrying the options and the page — and a pr.merged consumer that expires tasks whose subject PR merged (reason `pr_merged`); backfill command for the 26 existing stale approvals

- depends on: t9
- covers: c6, h5
- acceptance:
  - engine test: one task -> exactly one Jira comment record, one transition, one notify record (Jira-keyed run); one PR comment + one notify (PR-sourced run); the same task twice emits nothing more
  - engine test: a pr.merged fact expires the matching pending task with reason `pr_merged` and its run routes human-merges-pr.expired -> finish
  - the notify payload contains no value derived from `NODES_HUMAN_DECISION_TOKEN_SECRET`

### t12 — \[codex-orin\] Sweep dedupe by finding id: emission is keyed by finding id and consults GET /v1alpha1/runs?state=running&`workflow_key`=pr-upkeep for an existing run carrying that finding id before emitting

- depends on: t7
- covers: c7, h6
- acceptance:
  - sweep test: the same open finding across two head SHAs with a seeded running run emits one pr-upkeep.pr event, not two

### t13 — \[codex-thor\] Ticket description reaches agents: `jira_work_items` facts carry description; jira-intake and spec-chain-lane input contracts and instructions accept it; workflows republished

- covers: c13, h11
- acceptance:
  - `pr_upkeep_jira` test: the fact carries description
  - on the measurement ticket the intake run's claims quote a phrase that exists only in the ticket body

### t14 — \[codex-orin\] Runner completion 405: align the runner's completion POST (protocolclient.go:48 CallbackPathFormat) with the API's callback route and add an API test for the runner completion path

- covers: c14, h12
- acceptance:
  - API test: the runner's completion POST returns 2xx
  - one hour of thor's nodes-runner journal after deploy has zero 'refused with 405' lines

### t15 — \[codex-thor\] Code-node stdout readable: GET /v1alpha1/attempts/{id}/artifacts/stdout resolves a code node's `stdout_ref` (artifact store id), or GET /v1alpha1/artifacts/{id} is added; test covers the code-node path

- covers: c27, h18
- acceptance:
  - API test: a code-node attempt with a `stdout_ref` returns the bytes
  - on prod a green sweep run's stdout is returned (run id cited)

### t16 — \[developer\] Reachable page link: `NODES_UI_BASE_URL` wired into compose.thor.yml and compose.orin.yml (api, worker, scheduler) and both prod.env via install-secrets.sh; jiraticketreport.go renders an absolute link; docs state the LAN/tailscale reachability limit

- depends on: t5
- covers: c10, h9
- acceptance:
  - tests/deploy: both compose files and both prod.env carry the variable after a fake deploy
  - store test: with the variable set the page-link comment is an absolute URL

### t17 — \[developer\] Freeze handling: handleFreezeTicket cancels runs whose subject is the ticket with reason `ticket_frozen` when the ticket is Done, parks them with the same reason otherwise; the reason shows on each run and in the ticket page banner

- depends on: t16
- covers: c28, h19
- acceptance:
  - API test: freezing a ticket with two running subject runs leaves both cancelled (Done) or parked (other) with reason `ticket_frozen`
  - after deploy, prod run 01M16GMQMWYCA0EW0V7MHHQFWN is no longer running (cited in the measurement note)

### t18 — \[codex-thor\] Ticket page options: GET /v1alpha1/tickets/{id} lists the ticket's pending human tasks with their options and a `ticket_url` composed from the fact's `details_url`; TicketView renders the options as buttons under the held token and always shows Open in Jira

- depends on: t17, t10
- covers: c6, h5, c10, h9
- acceptance:
  - API test: a ticket with one pending task returns it with `allowed_outcomes` and `ticket_url`
  - TicketView vitest: options render and a click POSTs the decision; Open in Jira renders from `ticket_url`

### t19 — \[codex-thor\] #116: CompleteAttempt takes StartedAt from the invocation row (`actor_invocations`/`runner_invocations`.`created_at`) instead of stamping StartedAt=CompletedAt=now (internal/engine/complete.go:300); async-path test

- covers: c22, h15
- acceptance:
  - test fails on main, passes on branch: `duration_percentiles` for two completed attempts is non-zero
  - PR closes #116

### t20 — \[codex-orin\] #162: DispositionTableError(ValueError) in scripts/triage-report.py raised at the five dispositions() sites and caught before the broad clause so a malformed table exits 1 (finding) not 2; test in tests/`test_triage_report_types.py`

- covers: c22, h15
- acceptance:
  - test: malformed dispositions.csv -> exit 1
  - PR closes #162

### t21 — \[codex-thor\] #172: rewrite examples/pr-upkeep/README.md §The graph and §Idle vs blocked against sweep-cycle.workflow.yaml + v2 workflow.yaml; keep Deployment configuration

- covers: c22, h15
- acceptance:
  - README names sweep-cycle.workflow.yaml and the v2 nodes fix/human-merges-pr/finish; no mention of the removed v1 nodes
  - PR closes #172

### t22 — \[codex-orin\] #186: POST /v1alpha1/namespaces over the existing sqlc insert (+OpenAPI, test); deploy/prod/register-actor.sh namespace lookup moves from `run_psql` to the API

- covers: c22, h15
- acceptance:
  - API test: POST creates and GET lists the namespace; register-actor.sh has no psql namespace query
  - PR closes #186

### t23 — \[codex-thor\] #178: gate reports set ledger AttemptID only when the attempts row exists and keep the ref in record data (internal/api/gatereports.go:217/298/322); test

- covers: c22, h15
- acceptance:
  - test: an in-attempt gate report no longer violates `ledger_records_attempt_id_fkey`
  - PR closes #178

### t24 — \[codex-orin\] #183 legibility half: a completion whose `origin_actor_id` mismatches the dispatched actor is refused at accept time as `contract_rejected` with the mismatch named, no redispatch; custody half stays with #111

- covers: c22, h15
- acceptance:
  - worker test: mismatched origin -> `contract_rejected` naming both ids, dispatch budget untouched
  - PR closes #183 or narrows it to the custody half by comment

### t25 — \[developer\] #133 + #135: audit-credentials.sh compares the password embedded in `NODES_DATABASE_URL` to `POSTGRES_PASSWORD` and reports divergence by name; `env_has` becomes last-wins like `env_get`; no `DATABASE_SSLMODE` written beside a URL; the thor:18080 literal is parametrised (tests/deploy/`prodenvmerge_test.go`)

- depends on: t5
- covers: c22, h15
- acceptance:
  - tests/deploy cover each of the four behaviours and fail on main
  - PRs close #133 and #135

### t26 — \[operator\] #147 interim, #128, #10: add the per-bridge dial-in EnvironmentFile line to the intake/planner/verifier units and issue their credentials; delete thor's pre-deploy dump and decide the runner event token on #128; close #10 items 1-2 by comment and split item 3 (runner-service revision column) into its own Feature

- covers: c22, h15
- acceptance:
  - GET /v1alpha1/actors presence shows intake/planner/verifier dialled (not `never_dialled`)
  - \#128 and #10 closed with citing comments; the new #10 item-3 issue exists

### t27 — \[codex-orin\] Site polish bundle: route-specific document.title in App.tsx RouteWatcher; Header link to docs and a /v1alpha1/version readout; run id once on RunView; shared control classes and labels-above-inputs on PlanView and GenerateWorkflow; RunStateChip + humanised timestamps on RunsList and JobsTable; label on the Decisions per-record checkbox; claim statements rendered as text; scroll affordance on board columns and mobile tables; 'load a sample' on /workflows/new; Tickets entry in the header nav; before/after screenshots from scratchpad/shots in the PR

- depends on: t7, t8, t10, t18
- covers: c9, h8
- acceptance:
  - each item is a checked box in the PR body with a before/after screenshot pair
  - vitest suites for Header, RunView, PlanView, GenerateWorkflow, RunsList and Decisions pass

### t28 — \[codex-thor\] Human-facing docs: docs/drive-from-jira.md (what a ticket must contain; To Do -> intake, In Progress -> /think, bare comment -> consumer, marked-question reply, PR merged -> freeze; what each system comment means; how to open the page, where the token comes from, how to decide; what Done means and who moves it; the reachability limit; answer on the page now that the bot id is set) linked from README.md and the Header; examples/jira-intake/README.md; deploy/prod/README.md lists the five runner grants

- depends on: t26, t27
- covers: c12, h10, c31, h24
- acceptance:
  - the page exists, is linked from README.md and the Header, and names every trigger, comment and status listed in c12
  - deploy/prod/README.md names `GITHUB_TOKEN`, `SONAR_TOKEN`, `NODES_EVENT_TOKEN` and the Jira pair with their file

### t29 — \[operator\] Deploy the integration tip to thor and orin (deploy.sh, first run of grant-check.sh and the new migration), add the second Jira transition target (Pending) to the jira bridge config on both hosts, verify /v1alpha1/version on both

- depends on: t5, t6, t9, t11, t16, t17, t14, t15
- covers: c14, h12
- acceptance:
  - GET /v1alpha1/version on thor and orin returns the merged revision; grant-check passed naming zero missing keys
  - one hour of thor's nodes-runner journal after deploy has zero 405 lines

### t30 — \[operator\] Measurement sitting on one new SCRUM ticket: intake fires with the bot id set (park v2), two intake milestones -> one page-link comment (#200 signal 9), description quoted in intake claims, a human task -> Jira comment + Pending + page options + Discord + (PR-sourced) PR comment; decide human task 01M16FX0BWK9X6TKE9BHAAW88Y; run the stale-approval backfill; confirm the SCRUM-5 run is no longer running; record before/after pending counts and every id in docs/audits/2026-08-30-measurement-sitting.md; walk drive-from-jira.md as each reader role

- depends on: t28, t27, t13, t29
- covers: c24, h17, c36, h29, c32, h25, c31, h24, c35, h28, c13, h11, c28, h19
- acceptance:
  - the audit note cites ticket key, comment ids, transition id, notify post id, run ids, task ids and pending counts before/after
  - GET /v1alpha1/tickets/`id` shows one `page_link` across two intake milestones; pending human tasks on prod contain none whose PR is merged

### t31 — \[operator\] Delivery summary and honesty gate: /summarize-delivery ticks every announcement clause against a run id, commit or docs/ artifact; every cycle PR body lists its culture-nodes run ids (in-session exceptions declared with reason); gh open-issue count and a triage regen with zero untyped rows cited; signal 1 (7 days of green sweeps) recorded as pending with the date it can be read

- depends on: t29, t1, t3, t4, t30
- covers: c1, h20, c33, h26, c15, h13, c37, h30, c38, h31
- acceptance:
  - docs/deliveries/`date`-presentable-floor-before-oauth.md exists with a planned-vs-actual table and every clause mapped to evidence or listed as not delivered
  - grep of every cycle PR body finds run ids that resolve on GET /v1alpha1/runs

## Risks

- [follow_up] Success signal 1 (seven days of green sweeps) cannot complete inside the cycle; the delivery summary records it as pending with the readable date, and the OAuth cycle's /scope reads it (task t30)
- [unknown_nonblocking] Ticket creation by the operator's own account with `jira_bot_account_id` set is believed to still trigger intake but is unverified until t29's ticket; if it does not, the fallback is a second Jira account (#235) and t29 records that instead (task t29)
- [unknown_nonblocking] codex sandboxes deny socket(2) (#119): t19, t23, t24 and t25 carry Go tests that may need a database — codex writes them, the operator runs and fixes them at the merge gate on spark; budget one gate pass per task (task t19)
- [unknown_nonblocking] Web tasks t7, t8, t10, t18 and t26 touch overlapping files (RunsList, Decisions, Header, RunView); t26 is sequenced last and the others are file-disjoint by construction, but a same-wave collision still resolves at the merge gate (task t26)
- [unknown_nonblocking] All actor sessions share one subscription window (#48): the split plan declares sessions per wave; codex-thor/orin alternate so no host runs two packages at once; the developer lane takes at most one package per wave
- [unknown_nonblocking] Using the existing 'Pending' Jira status as waiting-for-decision (q4) may collide with a human's own use of Pending on the board; the sweep treats a bot-made transition as self-echo, a human-made one as a fact — t29 watches for a false wake (task t11)
