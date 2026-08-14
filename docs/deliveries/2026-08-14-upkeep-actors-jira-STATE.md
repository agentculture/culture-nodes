# upkeep-actors-jira — mid-cycle state (pre-compaction handoff)

Written 2026-08-14 as a compaction checkpoint. Everything here is recoverable
from disk, git, GitHub or the ledger — this file is the index, not the source.

## Where the work lives

- branch: `upkeep/scope-spec-plan`, HEAD `9aeba35` (not yet pushed, no PR open)
- spec: `docs/specs/2026-08-14-upkeep-actors-jira.md` (converged, challenged, re-exported)
- plan: `docs/plans/2026-08-14-upkeep-actors-jira.md` (18 tasks, 5 waves, 69/69 targets)
- frame/plan state: `.devague/frames/` + `.devague/plans/upkeep-actors-jira.json`
- control plane: `http://192.168.1.146:18080` (thor). Operator verbs:
  `bash .claude/skills/nodes-operator/scripts/nodes-op.sh <verb>`

## Task status

| task | state | commit / note |
|---|---|---|
| t1 | merged | re-verified 9 items vs `fadfa1d` (codex-thor) |
| t2 | done | ten issues closed with evidence |
| t3 | merged `b142722` | credential + personal-identifier lint |
| t4 | merged `7312636` | notifier workflow name, empty actor omitted (#66) |
| t5 | merged `a2738d9` | offline example-compile CI gate (#73) |
| t6 | merged `2b402a8` | typed literal binding (#73 option A) |
| t7 | merged `5adf813` | check-runs finding source (#61) |
| t8 | merged `724b3ad` | tracker identity refusal (#72 runtime half) |
| t9 | done (probe) | **negative** — spark bridge service cannot push |
| t10 | merged `10675a1` | deploy placement + assertion; **deployed** (see below) |
| t13 | merged `58a903d` | `handoff_unavailable` domain outcome (#74 criterion 3) |
| t14 | merged `9aeba35` | clarify-then-commit gate, engine side (#67) |
| **t11** | **run completed, NOT merged** | run `01M00HD3YZK3TPP2XBWVSP1JJX`, worktree `upkeep-t11` |
| **t15** | **running** | run `01M00HD42V6J2NNSA6E2NZTR0K`, worktree `upkeep-t15` |
| **t16** | **running** | run `01M00HD46AVGWVF9W369X902FA`, worktree `upkeep-t16` |
| t12 | not started | post-deploy credential audit (#69 item 2), depends t11 |
| t17 | not started | three pr-upkeep items through the merge gate |
| t18 | not started | delivery summary (`/summarize-delivery`) |

## Immediate next actions

1. Gate-merge t11, t15, t16 as they land: **tests before AND after**, then
   `git worktree remove` and `nodes-op grade <run> --rating N --notes ...`.
   Worktrees are `/home/spark/git/.worktrees.culture-nodes/upkeep-<task>/`.
   A failed run needs `--actor actor_claude_developer_D0ANYDJHVDXB3FXY`
   because grade cannot discover an actor without a succeeded attempt.
2. Then t12, t17, t18.
3. PR at the END — operator decision, recorded as deviation `d1`.

## Deployment already performed

`company/human-ops` is registered at `192.168.1.157:8090` (spark). spark had a
working bridge+tracker pair; thor had a **second orphan pair** on `:8087`
logging `pending=0` forever. thor's `human-inbox-bridge` and
`human-inbox-tracker` are now **stopped and disabled**. One logical inbox
remains, on spark, seeing `pending=1`.

`kernel.apparmor_restrict_unprivileged_userns=0` is applied and persisted on
**all three hosts** (`/etc/sysctl.d/60-culture-nodes-userns.conf`), each
verified by running `bwrap --unshare-user`, not by reading the sysctl back.

## Findings that must not be lost

- **#18 is resolved for shell exec, NOT for writes.** Run
  `01M00AM5NME6TZ1PXDG4A454HE` proved codex can exec shell commands. The
  write path is unproven since the fix; do not close #18 on the read-only probe.
- **t9: the spark bridge service cannot push.** HTTPS remote, no authorised
  SSH key, no credential helper, no token in the service env. Consequence
  beyond this batch: t25's preserve-branch push leg has been silently
  local-only on spark.
- **t14 was recorded `failed` but delivered completely** — the engine's
  deadline expired while the session kept working and committed `3f9bd3c`.
  This is #82.
- **Two of nine batch items were already delivered** when the cycle started
  (#71 partly, #73's example half). t1 exists because of that; re-verify
  before implementing.
- **#73 now looks closable** — both halves shipped (t5 CI gate, t6 literal
  binding). Confirm the example reads as a declaration before claiming it.

## Issues filed this cycle

| # | what |
|---|---|
| #76 | Jira Cloud node-loop — fully scoped, deliberately deferred |
| #77 | No model/effort recorded — `usage_model` null on all 123 attempts |
| #78 | A timed-out node preserves nothing |
| #79 | `internal/artifacts` has zero production callers |
| #80 | Continuation: resume a node under a declared continuation condition |
| #81 | Author nodes/flows from text via an agent node |
| #82 | A node deadline does not stop the actor session |

Ten issues were also **closed** with evidence citations (41, 43, 45, 46, 47,
49, 56, 64, 65, 68); 54, 48 and 66 left open with written reasons.

## Dogfooding count (the t17 metric)

Baseline at cycle start: **1**. Query it the same way both times —
`select count(distinct category) from runs where category like 'upkeep-%'`
against thor's postgres. At this checkpoint: **13 distinct plan tasks**
dispatched through the engine.

## Actor quality, so far

- `company/developer` — 5,5,5,5,5,4 across t3–t16. Strong at scoped
  implementation with judgment; repeatedly found sites and issues beyond its
  brief. The one 4 (t7) recorded upstream payloads verbatim without scanning
  them, which the credential lint then caught.
- `company/codex-thor` — 5 then 3. Excellent at locating and citing code;
  **weak at judging whether something is done** (labelled #67 and #72
  "already delivered" when both were real work, and #72's own evidence
  contradicted its label). Route it at finding facts, not at verdicts.

Every grade carries a caveat per #77: it rates an actor endpoint, not a
known model.
