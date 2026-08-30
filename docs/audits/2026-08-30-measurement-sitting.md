# Measurement sitting: the presentable-floor cycle on prod (2026-08-30)

Task t30 of `docs/plans/2026-08-30-presentable-floor-before-oauth.md`. This
is the sitting the spec's success signals asked for: the cycle was deployed to
prod (thor + orin) and each claimed behaviour was exercised on a real ticket,
**SCRUM-7**, created by the system's own Jira account. Every row cites the run,
comment or record that is the evidence; nothing below is a completion claim.

Deployed revisions during the sitting: `d6a253e` (0.45.0, PR #263) from
15:57 IDT, then `15fefde` (0.45.1, hotfix PR #264) from 20:42 IDT. Times in
Jira are IDT; run ids and API timestamps are UTC (IDT = UTC+3).

## What was measured

| # | Expected (spec / plan) | Observed | Evidence | Verdict |
|---|---|---|---|---|
| 1 | Intake fires on a ticket created by the system account (plan risk r2) | Fired on creation of SCRUM-7 | run `01M19PG0QGN6QA77Q6TNMSQQKJ`, comment 19:02:01 "started run" | proven |
| 2 | The page-link comment is an absolute URL (signal 9, #200) | `http://thor:18080/tickets/SCRUM-7` | comment 19:02:01 `[culture-nodes:ticket-page-link]` | proven |
| 3 | Signal 9 milestone 2: one link comment across two intake milestones | Second intake milestone (re-run 20:45) added no second link comment; SCRUM-7 carries exactly one | comment list, 5 comments total, one page link | proven |
| 4 | Intake claims quote a phrase that exists only in the description | `heliotrope-ledger-4471` quoted verbatim | run `01M19WDJC189VYJCKA1YFN08JJ` intake comment 20:45:40 | proven |
| 5 | A human task on a ticket-sourced run fans out: Jira comment with options | Posted 20:48:50 with task, run, options and the page link | fan-out row `jira_comment`; comment text below | proven, two defects (#265) |
| 6 | … plus a board move to `Pending` (q4) | `In Progress` → `Pending` after the comment | Jira status read 20:49 | proven |
| 7 | … plus a Discord post (q5) | Delivered, HTTP 204, 20:48:52 | notify idempotency record for attempt `01M19WKKH2HQMHESBGBM4G0F92`, claim "posted a Discord notification (status 204)" | proven |
| 8 | The decision endpoint resumes the run | `approved` accepted (HTTP 200), run ended `needs-human → finish` | decision record `ledger_01M19WN7B5QZYHY0AV3C6BBH6H` | proven |
| 9 | The ticket page frames the decision (q1 A+B) | Ticket projection lists the task under `human_tasks`, `pending_tasks` empties on decision | `GET /v1alpha1/tickets/SCRUM-7` | proven at the API; the SPA was not driven (no browser on the deploy host) |
| 10 | Runner 405s since deploy | 0 | thor runner journal | proven (earlier sitting, 0.45.0) |
| 11 | SCRUM-5 frozen by its PR merge; its zombie run cancelled (t17) | done | earlier sitting | proven |

The fan-out comment as posted (the only place a person sees the options):

```text
culture-nodes is waiting on a decision.

task: 01M19WKKGXPY1PRQAKRTHCERD1
run: 01M19WKKGTN1QSKH9V25F3019K
node: approval
options: approved, expired, rejected
decide: http://thor:18080/tickets/SCRUM-7
```

Rows 5–9 used a one-node probe workflow (`t30-fanout-probe`, digest
`sha256:03c45708…`, a single `kind: approval` node bound to SCRUM-7's
jira-shaped input) because no published workflow raises a human task on an
intake ticket: the spec lane's marked questions are `wait` nodes, not human
tasks. The probe bills nothing (no agent node) and is the smallest run that
reaches the fan-out path.

## What the sitting found that the plan did not predict

1. **The identity hotfix (#264) was necessary and is now proven twice.** On
   0.45.0 every spark claude-bridge completion was refused —
   `origin_actor_id actor_claude_<role>_* is not the dispatched actor
   actor_register_*` — because t24's custody check compared row ids, and a
   bridge's issued identity row is not the worker's registration row. First
   seen on the SCRUM-7 intake run above (`failed`), then on the pr-upkeep
   sweep's own attempt to fix #264's Qodo findings (run
   `01M19QXR5WW2DQRCHBFW3VCZNV`, `contract_rejected`). After 15fefde the
   same intake workflow completed (row 4) — the first accepted claude-bridge
   completion in prod after the fix.
2. **The merge-freeze fires on a mention.** PR #264's description cited
   SCRUM-7 as evidence; when it merged (17:39Z) the ticket was frozen
   (`merged_pr` = #264, reason `ticket_frozen`, 0 runs affected). That is
   the documented rule — "branch name or description names the ticket key" —
   but a measurement ticket froze because a hotfix *mentioned* it. New runs
   still start on a frozen ticket (rows 4–8 all post-date the freeze); the
   freeze closes the reply form and parks live runs at freeze time only.
   Recorded here rather than fixed: whether freeze should require a
   closes-style reference is a product decision for the next cycle.
3. **Fan-out lists `expired` as an option and names the node by kind** —
   filed as Bug #265 with the fix location.
4. **Nine pending human tasks predate the cycle.** Eight are pr-upkeep
   `human-merges-pr` approvals for PRs the operator merged on GitHub before
   deciding them (#223 ×6, #209 ×2) — the `pr_merged` expiry only covers
   ticket-keyed PRs. They were decided `approved` (the merge happened) by the operator at
   21:14 IDT via `decide-stale-approvals.sh` — a hand-turn, because the
   decision secret lives on thor and the assistant may not read it in bulk;
   the first attempt hit `ledger_version_moved` (409) until the script read
   each run's live `ledger_version`. Pending count 28 → 1. The ninth, `01M16FX0BWK9X6TKE9BHAAW88Y`, is a
   `trigger_remint_exhausted` alert on SCRUM-5 with no options; it is
   informational and stays until the alert kind gains an acknowledge path.
5. **The spec lane did not chain from intake on SCRUM-7.** `docs/drive-from-jira.md`
   says intake's move to In Progress starts the `/think` lane on the next
   sweep pass; two passes after the 20:45 move (17:46Z, 17:51Z) no
   `spec-chain-lane` run exists for SCRUM-7. Two confounds make this an
   unmeasured row rather than a defect: the ticket was already frozen by
   #264's merge, and the move was made by the system's own account, which
   the sweep discards as its echo. Re-measure on an unfrozen ticket moved
   by a second account.
6. **The shared agent checkout collided twice in one day** (#93): the
   sweep's fix node and the operator's gate touched the same working tree on
   PR #264. Per-run checkouts remain the fix.

## Hand-turns in this sitting (counted, per the operator-work rule)

- Reinstalled the Jira bridge on thor by hand after the 0.45.0 deploy
  (`JIRA_SITE` unset in the deploying shell; the deploy skips the actor).
  Not repeated for 0.45.1 — the fix is Go-only.
- Built and copied an arm64 `nodes` binary to thor for the backfill.
- Stashed the developer checkout under a live session (#93).
- Two deploys from main (`deploy.sh thor`, `deploy.sh orin`) at 0.45.1.
- Decided the eight stale approvals above `approved` at 21:14 IDT
  (`decide-stale-approvals.sh`; the decision secret lives on thor, so the
  operator ran it there). The first attempt was refused with
  `ledger_version_moved` (409) and re-run against each run's live
  `ledger_version`; pending count 28 → 1.

## How this closes

This audit is a Record: complete when written, closed on read. It is the
artifact SCRUM-7 points at.
