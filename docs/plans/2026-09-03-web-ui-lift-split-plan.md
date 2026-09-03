# web-ui-lift — implementation split plan (human gate 2)

Plan: `docs/plans/2026-09-03-web-ui-lift.md` (20 tasks, 8 waves). Engine:
**culture-nodes itself** — every delegable task is one `assign` dispatch through
the control plane to a registered actor (`nodes-operator` skill), never a local
subagent. The operator lane (interactive Claude) keeps the merge gates,
validate-delivery, issue work and human steps.

## Lane facts on 2026-09-03 (pre-checks)

- spark claude lane (`developer`, `planner`, `verifier`): OAuth valid
  (`expiresAt` in the future); editable checkouts on spark, npm present.
- codex-thor / codex-orin: clean checkouts; **orin has no npm** (web tasks go
  to thor or the developer lane); codex sandbox has **no sockets** (#119), so
  Postgres-backed Go tests run at the operator's merge gate with a throwaway
  `postgres:17-alpine`; the bridge **write path is unproven** (#18) — patches
  are harvested with `git fetch culture-codex@<host>:git/culture-nodes-agent`.
- codex window ≈ 12 packages per window, **shared with the pr-upkeep sweep's
  own dispatches** (four failed runs between 16:26Z and 17:22Z today); reroute
  on `capacity_exhausted`, never retry.
- colleague lane: no competing run; kept to review/explore only.

## Integration branch

`feat/web-ui-lift`, cut from `spec/web-ui-lift` (PR #284, unmerged) and pushed,
is the `--base-ref` for every dispatch and the target of every TDD-gated merge.
Task branches: `agent-web/<task-id>` in the actor's own checkout. Wave N+1 is
dispatched only after every wave-N branch is merged and its tests pass.

## Routing table

| Task | Wave | Lane | Sandbox / timeout | Why this lane |
| --- | --- | --- | --- | --- |
| t1 server tail-only stream | 1 | codex-thor | workspace-write / 45m | Go + tests, no DB needed at write time; PG tests at gate |
| t3 CultureNode | 1 | codex-thor | workspace-write / 45m | big web package, npm on thor |
| t4 token lint guard | 1 | developer (spark) | — / 30m | small Go test, file-disjoint |
| t5 worker presence table | 1 | codex-orin | workspace-write / 45m | Go only, no npm needed |
| t11 demo README map | 1 | operator | — | docs, needs the frame's scope entries |
| t2 shared stream reconcile | 2 | codex-thor | workspace-write / 45m | web, depends on t1's frame shape |
| t6 mesh endpoint + collector | 2 | codex-orin | workspace-write / 60m | Go, depends on t5 |
| t8 Design gallery | 2 | developer (spark) | — / 45m | web, npm on spark, spreads the codex window |
| t7 Mesh rebuild | 3 | codex-thor | workspace-write / 60m | biggest web package |
| t9 nav on the spine | 3 | developer (spark) | — / 45m | web, medium |
| t12 yaml round-trip spike | 3 | developer (spark) | — / 30m | needs node + yaml pkg; time-boxed |
| t10 fluidity | 4 | codex-thor | workspace-write / 45m | web + Playwright |
| t13 reopen record + issues | 4 | operator | — | issue types are GraphQL-only; open-issue.sh |
| t14 workflow-document | 4 | developer (spark) | — / 45m | pure module + golden tests |
| t15 design canvas | 5 | codex-thor | workspace-write / 90m | biggest package of wave B |
| t16 operator screenshot review | 5 | human (operator) | — | h12: a person judges |
| t18 live test on prod | 5 | human (operator) | — | Access session + deploy hand-turn |
| t17 walkthrough e2e | 6 | codex-thor | workspace-write / 45m | Playwright on fixtures |
| t19 validate-delivery | 7 | operator (Claude) | — | the skill runs agent-side |
| t20 fixes cycle | 8 | operator + codex-thor | as needed | bounded to two rounds |

## Session accounting (nodes-operator template)

```yaml
# Wave 1: server stream mode, node component, token lint, presence table, README
# - Tasks: t1, t3, t4, t5, t11
# - Model sessions: 4 (codex-thor ×2: t1, t3; codex-orin ×1: t5; developer ×1: t4)
# - Operator lane: t11 + four merge gates (PG tests for t1/t5 at the gate)
# - Non-billable: token lint, README
#
# Wave 2: reconcile, mesh endpoint, gallery
# - Tasks: t2, t6, t8
# - Model sessions: 3 (codex-thor: t2; codex-orin: t6; developer: t8)
#
# Wave 3: mesh rebuild, nav, spike
# - Tasks: t7, t9, t12
# - Model sessions: 3 (codex-thor: t7; developer ×2: t9, t12)
# - Gate: the spike's decision record + operator go/no-go before wave 4's t14
#
# Wave 4: fluidity, reopen record, workflow-document
# - Tasks: t10, t13, t14
# - Model sessions: 2 (codex-thor: t10; developer: t14); operator: t13
#
# Wave 5: canvas, operator review, live test
# - Tasks: t15, t16, t18
# - Model sessions: 1 (codex-thor: t15); human: t16, t18 (deploy = hand-turn)
#
# Wave 6–8: walkthrough e2e, validate-delivery, fixes
# - Tasks: t17 (codex-thor, 1 session), t19 (operator), t20 (operator + ≤2 codex-thor)
#
# Totals: codex ≈ 9 packages + ≤2 fixes (one window if spread over the day's
# waves); developer ≈ 6 sessions; operator: 20 merge/gate turns.
```

## Deviations

A wave that exhausts a lane or comes in well under budget gets a `/deviate`
record with the observed session count (issue #48 item 5). Every hand-turn
(deploy, harvest, re-grant) is counted on #283 or its successor.
