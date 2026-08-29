# Delivery Summary — jira-flow-spec-read-related-bugs

plan: `jira-flow-spec-read-related-bugs` · run: `partial` · date: `2026-08-29`
baseline: `devague summary skeleton`
(`devague summary --plan jira-flow-spec-read-related-bugs`, rendered 2026-08-29 10:35Z)

## Intent

> Jira flow closes its loop: the merged flow-store cycle is deployed and proven on prod records, the spec a ticket is deciding on is readable from the ticket itself as a rendered page, the /think leg can be driven from the board, and the bugs that bit the last cycle (deploy.sh un-granting the sweep, the bridge scope-boundary false denial, the dial-in 401 loop) no longer reproduce

After: Prod's /v1alpha1/version names a revision containing c0f6c4a and the prod registry lists pr-upkeep-sweep-cycle v2 and jira-comment-consumer; a real ticket carries one in-place-updated HTML spec page from intake to PR merge; a ticket fact mints the spec chain and a marked question answered on the board transacts exactly that frame move; deploy.sh preserves the sweep's grants; #207 and #205 no longer reproduce; #192 #193 #194 #197 #198 are closed on prod evidence via #230

## Planned Work

- `t1` — Fix #191: deploy.sh's runner.env write carries `NODES_API_URL` and a jira-bearing `PR_UPKEEP_REPOSITORIES` forward from the existing file, or refuses loudly before touching it
- `t2` — deploy.sh becomes the two-host r4 procedure: preflight nodes doctor + checkout-state check BEFORE the thor stack is stopped, a forced pg dump before migrate, orin's worker stopped across migrate/cutover and restarted after, and an api↔worker revision-parity check before the sweep schedule resumes
- `t3` — Register the re-mint producer: wire Options.RemintProducerActorID to `NODES_REMINT_PRODUCER_ACTOR_ID` in cmd/nodes/worker.go (default `engine_remint_scheduler`) and make deploy/prod/register-actor.sh able to register it as a kind=engine actor
- `t4` — Fix #207: the claude bridge's workflow-scope boundary measures git diff --name-only <branch-base>..HEAD plus uncommitted changes, never the workspace delta from the pre-base-move tree
- `t5` — Fix #205: dialin.py backs off exponentially (ceiling) on HTTP 401 and surfaces the credential mismatch once; propagated byte-identically to all five adapters; the developer unit's dial token is reissued
- `t6` — internal/ticketreport gains package tests (#231): start and finish for one run, delivery through the jira bridge only, retry/backoff to terminal failure, and the sub-interval case where finish arrives before start delivered
- `t7` — Operator lane: measure the before-state, reconcile the prod checkouts per q3 (discard; no qwen config), then run the r4 deploy — deploy.sh thor, deploy.sh orin — publish pr-upkeep-sweep-cycle v2 and jira-comment-consumer, register `engine_remint_scheduler`; every hand-turn a comment on #230
- `t8` — t13 live proof of the deployed flow-store legs on a real ticket, from prod records only (signals 1, 2, 5; a bounded re-mint; a start/finish report; a bare comment reaching its consumer); close #192 #193 #194 #197 #198 on that evidence and #202 with the ledger.propose audit
- `t9` — Ticket projection in the control plane: migration `ticket_frames` (append-only, keyed by ticket id + version), POST /v1alpha1/tickets/{id}/frame (decision-token guarded), GET /v1alpha1/tickets/{id} composing runs by subject, ledger, human tasks, ticket reports, replies, and the latest frame; a subject filter on listRuns; openapi updated
- `t10` — Reply endpoint: POST /v1alpha1/tickets/{id}/replies (decision-token guarded, required replier) appends a signal event in-process with origin.kind human and the replier named, and enqueues a display-only mirror comment 'via <replier>' through the jira bridge outbox; the page-reply fact and the sweep's comment fact validate against one schema
- `t11` — Web route /tickets/:id: renders the projection (frame claims with state, questions/decisions, reports, runs, reply thread), a link back to the Jira ticket, a reply box using the existing decision-token store, and a freeze banner; PRD §8.8 rules hold on the route; no secret enters the bundle
- `t12` — Link at intake and freeze: jira-intake's intake node posts ONE page-link comment carrying a link marker (re-posted/edited in place, never duplicated); the sweep emits pr.merged for closed PRs with `merged_at` (watermarked) and links the PR to its ticket by branch name or body carrying the ticket id; a pr.merged fact freezes the ticket's page (frozen flag + merged PR pointer on the projection)
- `t13` — Board-driven /think leg (q4 = B): declare .devague/ custody for the owe-developer lane; a lane workflow minted from a ticket fact runs the devague moves in the lane worktree, commits .devague/, posts the frame snapshot to POST /tickets/{id}/frame after each move, and surfaces frame decisions as marked questions the human answers on the ticket or the page
- `t14` — Live proof of the ticket-page legs on a real ticket (signals 3, 4, 8, 9) from prod records only; close #199 and #200 on that evidence
- `t15` — Session accounting and closure: the split plan declares model-session counts per wave before any dispatch (colleague for review/explore, codex + claude-developer for build; qwen parked); /summarize-delivery is written BEFORE the cycle's final PR merges; every hand-turn is a comment on #230 or the issue it touches

## Actual Delivery

| Plan task | Status | What actually landed |
|-----------|--------|----------------------|
| `t1` | delivered | `98f8e4d` (codex-orin, WP-C under d1) + gate `81a1ddc`; first-deploy default restored by t2 (`75088f4`); the literal-`$HOME` defect it shipped hit prod on t7 and was fixed in `82d8bc5` with a harness assertion |
| `t2` | delivered | `7d6cbfe` (developer, WP-F; second session after the operator killed the first): preflight before anything stops, forced pre-migrate dump, orin quiesced across migrate/cutover, parity-gated sweep resume; no gate fixup. Two prod-only defects fixed in the operator lane during t7: cutover DB address (`5d9be94`), scheduler actor tokens (`ae48e40`) |
| `t3` | delivered | `7f7e2fa` (codex-thor, WP-A): `NODES_REMINT_PRODUCER_ACTOR_ID` wiring + `register-actor.sh --engine`; `engine_remint_scheduler` registered on prod at t7 |
| `t4` | delivered | `4284626` (codex-orin, WP-B) + gate `b4a16a9`; hardened by WP-B2 under d2 (`5801c31`, bridge-trusted `base_ref`, mirrors aligned) + gate `c5d2efd`; refspec guard from the wave-1 review (`ab13fce`) |
| `t5` | delivered | `52453dd` (codex-orin, WP-D under d1): backoff on 401, warn once, byte-identical across seven adapters; developer dial-in credential issued and delivered at t7 (`f9a2e69` unit template), presence `connected` from 10:22Z |
| `t6` | delivered | `bf876ae` (codex-thor, WP-A) + gate `bec177a` (schema-valid fixture); review-driven dispatcher fixes `f1142d5` |
| `t7` | delivered | operator lane: RESTORE.md rehearsed on spark; checkouts reconciled (q3); three `deploy.sh thor` attempts (runner.env `$HOME`, cutover DB address) then orin; prod at `5d9be94`, cutover 4/4, parity, sweep resumed; actor registered; sweep-cycle v2 / intake v5 / comment-consumer v1 published; hand-turns 10–16 on #230 |
| `t8` | partial | signals 1, 5 (deploy grants), re-mint + human task, start/finish reports, creation-as-first-sighting (WP-H `80c9822`, found by this proof), comment consumer round trip all measured on SCRUM-5; **signal 2 unverifiable** — the operator's Jira user is the system credential; #192–#198 closures pending the final read |
| `t9` | delivered | `e4b156f` (codex-thor, WP-A): migration 0046, `POST/GET /v1alpha1/tickets/{id}`, subject filter; index `0047`; projection listed no runs until the sweep sent subjects (`229844a`, found by t8) |
| `t10` | delivered | `0224689` (codex-thor, WP-D) + gates `1ee8bb3`, `342db74`; reply ordering from the wave-1 review (`ab13fce`); proven live on SCRUM-5 (signal 8) |
| `t11` | delivered | `6a7841b` (codex-orin, WP-E) + gate `974673b` (11 TypeScript errors, e2e probes) — vitest 511, build, ticket e2e green; LAN-only (r2) |
| `t12` | partial | `0224689` (WP-D): `pr.merged` watermark + issue link (scoped to the Jira project after the phantom ADR-0002 freeze, `bf821c3`), freeze on the projection; **page-link singleton not honoured** (two marker comments on SCRUM-5) — WP-I in flight |
| `t13` | delivered | `2331bcc..74161f7` (developer, WP-G): custody declaration, assign refusal, `examples/spec-chain-lane`, operator docs; custody-over-identity fix `244b83e` after the first prod runs were 403; a live lane run is executing devague moves on SCRUM-5 at the time of writing |
| `t14` | partial | on SCRUM-5 from prod records: signal 8 end to end (facts `01M16EQT0RX98WWGNTM97C073W`/`01M16EQVZPCVT6RS09P8Z0A77S`, comments 10188/10189, projection `replies: 2`); signal 4 (spec-chain lane run `01M16GMQMWYCA0EW0V7MHHQFWN` from a ticket fact, 27 frame snapshots on `ticket_frames`, marked question 10207, page answer `01M16H6ZZNDD357BKXVSFVVXCR` resumed the run through correlate → transact); signal 9 milestone 1 held (one link line in 10191) but **milestone 2 failed** (a second link in 10205 — WP-I moves the link into an engine-owned singleton row, deployed at the next control-plane deploy); signal 3 (page changing across milestones) holds through `latest_frame` v27; **signal 2 unverifiable** on this site; #199/#200 closures deferred to the PR |
| `t15` | partial | session declaration posted on #230 before wave 0 (≈10 sessions declared; ≈16 used: 4 lost to lane defects, +WP-B2/WP-H/WP-I/WP-J follow-ups); this summary written before the final PR merges; every hand-turn (17 by t8) commented on #230 |

## Mid-work Decisions

- `d1` — Re-route t1 (#191) from colleague to codex-orin; t5 (#205) follows the same route if its colleague run ends incomplete — colleague run 11b1701688e2 ended incomplete with zero code: model turns averaged 374s against a 300s request timeout, the stream guard tripped at 900s and the drive stalled after 6 turns — three concurrent colleague runs (t1, t5, review) on one local vLLM, GPU at 96%; capacity, not comprehension. codex-orin just delivered t4 cleanly and the sandbox/network pattern is now known
- `d2` — Add package WP-B2 (t4 hardening, #242): the scope guard's baseline becomes bridge-trusted — assign gains an optional `base_ref` run input, the bridge fetches it on the host before the session and measures `trusted_base`..HEAD plus working tree, falling back to `head_before`; mirrored to the codex and colleague scope guards; tests for the committed-edit positive case and the upstream-repoint bypass. Routed to codex-thor as one warm session — colleague's review of the t4 merge found the fix measures against @{upstream}, which the guarded session sets: a committed .github edit plus an upstream re-point reads as completed (a regression from the `head_before` baseline); the codex/colleague mirrors were not updated, breaking the all-backends rule. A bridge-trusted base also retires the operator pre-fetch hand-turn every codex dispatch has needed (no network in the sandbox)

- Decisions no deviation record covers, captured here directly:
  - four follow-up packages beyond the split plan (WP-H creation-as-first-sighting, WP-I engine-owned page link, WP-J deploy.sh split under the 1000-line guard; WP-B2 is `d2`) — each a defect the live proof or a guard surfaced, each routed to the free codex host with the operator's approval implicit in the goal ("/deviate as needed"); the session count rose from ≈10 declared to ≈16.
  - colleague was kept as reviewer only after `d1` (three reviews, three real findings); one colleague run at a time.
  - the operator restarted the developer bridge mid-session (hand-turn 8) and re-dispatched with the partial diff as context — an operator error, recorded.
  - prod was deployed from the integration branch (`5d9be94`), not from main, so the proofs could run before the PR; the branch tip moved past it (sweep pair re-granted three times: `308d2c7`, `229844a`; control plane still `5d9be94`).
  - the operator answered the lane's q1 on the user's behalf (B: signal 2 unverifiable on this site), citing the user's own account finding; recorded on #230.
  - the phantom `ADR-0002` freeze row and the scheduler's missing actor tokens were fixed on prod by hand (hand-turns 13–14) with the code fixes landing the same hour.

## Drift From Plan

| Plan item | Reason for divergence | Classification |
|-----------|------------------------|-----------------|
| `t1` (`d1`) | colleague run 11b1701688e2 ended incomplete with zero code: model turns averaged 374s against a 300s request timeout, the stream guard tripped at 900s and the drive stalled after 6 turns — three concurrent colleague runs (t1, t5, review) on one local vLLM, GPU at 96%; capacity, not comprehension. codex-orin just delivered t4 cleanly and the sandbox/network pattern is now known | `acceptable` |
| `t4` (`d2`) | colleague's review of the t4 merge found the fix measures against @{upstream}, which the guarded session sets: a committed .github edit plus an upstream re-point reads as completed (a regression from the `head_before` baseline); the codex/colleague mirrors were not updated, breaking the all-backends rule. A bridge-trusted base also retires the operator pre-fetch hand-turn every codex dispatch has needed (no network in the sandbox) | `needs-follow-up` |

## Evidence

- tests (read-only on the integration tip): `go test ./...` with `NODES_TEST_DATABASE_URL` — all packages ok after the two guard fixes (`8df8f7f`); one timing flake seen once under the full parallel run (`internal/scheduler` `TestSchedulerFiresDeadlineTimerFailsWaitingExternalAttemptAndRoutesEdge`, a package this branch never touched; 3/3 green in isolation); `uv run pytest -n auto` — 618 passed; adapters claude-code 395 / codex 384 / colleague 325 / human-inbox 196 / notify 167 / jira 22; web vitest 511 + build + ticket e2e; `scripts/lint-all.sh` — all three jobs green
- prod reads (2026-08-29): `GET /v1alpha1/version` → `5d9be9404e91…` (contains c0f6c4a); `GET /v1alpha1/workflows` → `pr-upkeep-sweep-cycle` v2, `jira-intake` v6, `jira-comment-consumer` v1, `spec-chain-lane` v1; `GET /v1alpha1/actors` → `engine_remint_scheduler`; `GET /v1alpha1/dial-in-presence` → `company/developer: connected`; `GET /v1alpha1/tickets/SCRUM-5` → `runs: 3, replies: 2, latest_frame.version: 27`; `signal_events`, `trigger_remints`, `ticket_frames`, `jira_ticket_report_outbox` rows quoted on #230
- commits: integration branch `feat/jira-flow-spec-read-related-bugs`, `5a0cbdb..HEAD` (worker packages `4284626`, `7f7e2fa..e4b156f`, `98f8e4d`, `52453dd`, `5801c31`, `6a7841b`, `0224689`, `75088f4`+`7d6cbfe`, `2331bcc..74161f7`, `80c9822`, `2f2778f`; gate/operator fixes named per row above)
- PRs / issues: #234 (delivery summary of the previous cycle), #236 (spec), #238 (plan), #230 (dispatch log, 17 hand-turns), #240 #241 #242 #243 #235 #237 (filed), #191 #205 #207 (fixed), #192 #193 #194 #197 #198 #199 #200 (closure decisions in the PR)
- audit: `docs/audits/2026-08-29-fleet-and-bridge-evaluation-jira-flow-cycle.md`

## Delivery Claims

| Claim | Confidence | Evidence |
|-------|------------|----------|
| prod runs the flow-store cycle (history-faithful sweep, re-mint, ticket reports, store) — signal 1 | high | `GET /v1alpha1/version` → `5d9be94…`; cutover 4/4; #230 comments 13:30Z |
| deploy.sh preserves the sweep's grants and refuses to un-grant — signal 5 | high | `98f8e4d`, `82d8bc5`, `75088f4`; `tests/test_deploy_runner_env.py` (17 cases); three deploys today kept `NODES_API_URL` + Jira config |
| a technically-failed trigger-created run re-mints twice then parks on a human (#194) | high | `trigger_remints` attempts 1–2 for event `01M16FS8T7…`; human task `01M16FX0BWK9X6TKE9BHAAW88Y` (`trigger_remint_exhausted`) |
| runs minted from ticket facts post start and finish reports (#198) | high | SCRUM-5 comments 10190/10192 (and 10193–10198, 10200–10206); outbox rows published |
| a page reply becomes one human-origin fact and one mirrored comment and resumes a parked run (#200 signal 8; t10) | high | facts `01M16EQT0RX…`/`01M16EQVZPC…`; comments 10188/10189; reply `01M16H6ZZ…` resumed run `01M16GMQMWY…` via correlate → transact |
| the /think leg runs from the board: ticket fact → lane run → frame snapshots → marked question → answer transacts the move (#199, signal 4) | high | run `01M16GMQMWYCA0EW0V7MHHQFWN`; 27 `ticket_frames` rows by `actor://company/developer`; comment 10207 `question_id=scrum-5.q1` |
| a freshly created issue is first-sighted once (#193 regression fixed) | high | fact `transitioned.to-do` @10:02:02 for SCRUM-5 after `80c9822`; intake run `01M16FG5XRJSQVCCV0H1YGN95T` |
| the ticket page lists a ticket's runs, replies and latest frame (#200 / t9, t11) | high | `GET /v1alpha1/tickets/SCRUM-5` → `runs: 3, replies: 2, latest_frame.version 27`; web route + e2e (`6a7841b`, `974673b`) |
| the developer bridge dials in without a 401 loop (#205) | medium | presence `connected` from 10:22Z; the one-hour journal read (h7) is recorded on #230 when it lands |
| the scope guard measures a bridge-trusted base, mirrored across three bridges (#207, #242) | high | `5801c31`, `c5d2efd`, `ab13fce`; `test_upstream_repoint_cannot_hide_committed_workflow_edit` |
| exactly one page-link comment per ticket across milestones (signal 9) | unverified | milestone 2 measured TWO (10191, 10205); WP-I (`2f2778f`) fixes it in the engine but is not yet deployed — not claimed done |
| two human comments inside one sweep interval emit two facts in order (signal 2) | unverified | unverifiable on this site: the operator's Jira user is the sweep credential; two ordered *transition* facts in one interval were measured (10:22:00) as the shape |
| #192 #193 #194 #197 #198 #199 #200 are closed on prod evidence | unverified | closure is the PR reviewer's read of this summary; not claimed here |

## Remaining Work / Follow-up

- **Deploy the integration tip** (WP-I's engine-owned page link, the subject-bearing sweep already granted, intake v6 published): one more `deploy.sh thor` + `deploy.sh orin` after the PR merges — owner: operator (#230).
- **Signal 9 milestone 2** — re-measure on the next new ticket after that deploy; until then unverified.
- **Signal 2** — needs a second human Jira account or per-user page replies (#235); unverified by design here.
- **The spec-chain lane parks on its next question** (q2 on frame `scrum-5`): answer on the ticket page or leave parked; the loop is proven, not finished.
- **`trigger_remint_exhausted` human task `01M16FX0BWK9X6TKE9BHAAW88Y`** on prod — decide it (the failure it records is fixed by `244b83e`).
- **Prod grant lacks `jira_bot_account_id`** — transition self-echo is off; add it to `PR_UPKEEP_REPOSITORIES` and re-grant (audit §3.14).
- **`deploy.sh` split into sourced lanes** (`deploy/prod/lanes/`, WP-J `05b216e`, 1139 → 707 lines; the deploy text guards now read the lanes as one program, `ab976a2`) — the next prod deploy is the first to exercise the sourced form end to end.
- **Automation backlog** from the audit: #243 (agents as OS users), a gate node, #241 reasons on records, bridge commit-on-exit + drain, service-level token parity test, `nodes-cutover --check` in PREFLIGHT, assign self-test.
- **Wave-2 colleague review**: the four-package brief step-stalled (run `1e77e1e7d0d9`, no findings); re-run narrowed to WP-H's creation fact (replay / cutover-adoption / self-echo) — result appended when it lands.
- **PR #244 review fixes (in-branch)**: replies refused on a frozen ticket (409, same statement as the write); client idempotency id on replies (retry resumes the row, page sends one per submission); merged-PR discovery paginates inside a 30-day window; preflight doctor fails closed (`FIRST_DEPLOY=1` is the declared exception) and doctors orin too; the three `workspace.py` mirrors byte-aligned. Thread 1 (lane commits) answered as the recorded q1 decision.
- **Follow-ups filed**: #235 (per-user reply identity), #237 (headspace spec-chain graph), #231/#232 from the previous cycle still open.
