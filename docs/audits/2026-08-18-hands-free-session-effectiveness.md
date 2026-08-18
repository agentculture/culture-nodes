# Session audit: the hands-free-SCRUM-2 evening (2026-08-18)

A record of the operator session that took "I want the system to pick up
SCRUM-2, hands free" from idea to a live prod loop in one evening — what it
cost, what made it fast, and what fought back. Companion to the delivery
record (`docs/deliveries/2026-08-18-hands-free-scrum-2-pickup.md`), which
covers the *work*; this covers the *session*.

## Shape of the session

One operator Claude session ran the full devague chain — `/scope` → `/think`
→ `/challenge` → `/spec-to-plan` → `/assign-to-workforce` — and then drove
execution to the live proof without a handoff: 24 confirmed claims, 15
honesty conditions, a 10-task plan, wave-0 fan-out, six prod deploys, a
four-attempt live-fire debug ladder, and closure (three bugs closed on prod
evidence, PR #190 ready, review findings remediated).

Lanes actually used, against the split plan's declaration:

| Lane | Planned | Actual |
|---|---|---|
| codex bridge (billable) | 2 sessions (t1, t2) | 2 — both landed handover commits; the #18 write-path proof came free |
| local subagent | 1 (t3) | 1, plus one crash-resume |
| claude-intake bridge (product's own) | 1–2 dispatches | 4 (the debug ladder) |
| operator main loop | everything else | everything else — merges, deploys, live debugging, docs |

## What made it effective

1. **The observability built at 19:00 debugged the system at 19:30.** The
   #189 read-back (stdout artifact by one GET) diagnosed the
   container-DNS failure, and the `stdout_artifact` observation named both
   artifact-pipeline lifecycle gaps — from the API, no host access. The
   session's single best return on investment.
2. **Every failure was a run record, not a mystery.** Four intake attempts
   failed at four *different*, successively deeper nodes — dispatch,
   completion contract, credential, outcome name — and each rung's fix was
   one commit or one config change made within minutes, because the failing
   node said exactly what it refused and why.
3. **Monitors instead of polling.** Watermark-table watches turned "did the
   sweep see it?" from guesswork into an event, and the arming dance became
   a script (`jira-status` skill) that removed the human from the loop's
   test harness mid-session.
4. **Cadence as config.** Dropping the sweep from 30 to 5 minutes (one API
   call, operator-directed) collapsed every debug iteration sixfold.

## What fought back (the friction tax)

- **`deploy.sh` clobbered hand-granted runner env three times** (#191) —
  cost two failed sweep windows and one silently-Jira-blind "green" sweep.
  The mitigation (snapshot + restore bracket) is manual and counted.
- **The permission classifier blocked prod deploys** in every shape for
  ~40 minutes (including, erratically, `grep` and `black --check`),
  then permitted the identical command. Cost: one long stall, resolved
  partly by the user's keystrokes and partly by the classifier recovering.
- **Contract truths discoverable only live**: the claude bridge's
  `success_outcome`/`summary` shape, the Jira bridge's `issue_transitioned`
  outcome name, `input.repo` inference needing a *restarted* resident
  process, Jira's JQL index lag. None were knowable from the repo alone;
  all are now encoded in the workflow document, the skill docs, or memory.
- **Stale resident processes**: an Aug-15 intake bridge predated the #125
  inference it needed. Editable installs can't go stale on disk but their
  *processes* can.

## Counts

- Prod runs created this session: ~20 (sweeps at two cadences, 4 intake
  attempts, 2 codex assigns); final state: intake green, 8+ consecutive
  green sweeps.
- Deploys: thor ×4, orin ×2 (plus the operator's independent pair).
- Branch: 25+ commits, 3 CI red→green cycles (all first-try fixes), PR #190
  green with review findings remediated same-session.
- Issues: filed #191–#194 (all dispositioned), closed #187/#188/#189 on
  prod evidence, #18 write path proven, #118 gap retired.
- Deviations: d1 approved and extended twice by live findings — each
  extension measured, none silent.

## Honest limits

- The loop's trigger still needs a status *transition*; the flip dance is
  scripted, not eliminated (#193/#194 own that).
- The arming action was operator-scripted, not user-clicked, in the final
  proofs — sanctioned, but a purist's h1 reading prefers the 19:31 attempt
  chain where the user's own moves armed it.
- Second intake comment on SCRUM-2 (10070 then 10071) is re-triage
  semantics doing its job, but a reader of the ticket sees near-duplicates.
- The friction tax above consumed roughly as much wall-clock as the
  designed work; #191's fix would have paid for itself twice tonight.
