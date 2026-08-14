# upkeep-actors-jira — mid-cycle state (pre-compaction handoff)

> **Superseded for reporting purposes.** The cycle's accountability artifact is
> [`2026-08-14-upkeep-actors-jira.md`](2026-08-14-upkeep-actors-jira.md) — the
> delivery summary (plan task t18), with all eighteen tasks accounted for and
> every claim carrying evidence. This file is kept because it holds the
> operational diagnosis and the hard-won cautions the summary does not repeat.

Written 2026-08-14 as a compaction checkpoint. Everything here is recoverable
from disk, git, GitHub or the ledger — this file is the index, not the source.

## Where the work lives

- branch: `upkeep/scope-spec-plan`, pushed; PR [#85](https://github.com/agentculture/culture-nodes/pull/85)
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
| t11 | merged `1e84a3f` | prod.env merges, never rewrites; `remove-secret.sh` (#69 item 1). Graded 5 |
| t16 | merged `032c01b` | committed demos are portable; sweep source is a granted value (#48). Graded 5 |
| t15 | merged `0c9d62e` | preflight capability surface on all four bridges (#67). Graded 4, see #83 |
| — | committed `3332057` | **operator lane:** preflight check 7 + provisioning docs (#63), closing its last gap |
| t12 | merged `f4f2757` | post-deploy credential audit (#69 item 2). Graded 5 — found orin's missing token live |
| — | committed `a5438a3` | **operator lane:** the claude actor token now reaches orin too |
| t17 | **partial** | the sweep works again and a cycle completed, but 0 items reached the merge gate — blocked by #79 |
| t18 | delivered | the delivery summary beside this file |

## Immediate next actions

All plan tasks are done and the PR is open. What remains is not this cycle's:

1. Land PR #85 once its checks are green.
2. **#79 is the one blocker left on the pr-upkeep loop.** Everything else in
   it works now; see the delivery summary and the live evidence on the issue.
3. Re-running the loop needs `PR_UPKEEP_SWEEP_SOURCE_URL` /
   `_SHA256` re-granted, pinned to whatever commit serves the intended
   `sweep.py` — `deploy.sh` rewrites `runner.env` every deploy, so the grant
   must be supplied to the deploy, not hand-edited afterwards.

## Why pr-upkeep has 16 runs and 1 completion (the t17 diagnosis)

Measured from thor's postgres, not inferred. The 13 failures are **three
different causes**, and two are already resolved:

| cause | where | state |
|---|---|---|
| `NODES_CODE_RUNNER_REVISION` was `1`, not a `sha256:` digest, so the runner boundary refused every code dispatch with a 400 | `sweep`, cascading to `triage` ("no succeeded attempt, so it has no output") | **resolved** — the running worker carries the correct digest since its 09:26 restart |
| `policy_denied` / 401 | `review`, `sweep` | **resolved** — the destroyed `NODES_ACTOR_CLAUDE_TOKEN`; t11 fixes the cause |
| the sweep's script source is now a granted environment value | `sweep` | **resolved** — thor was redeployed with `PR_UPKEEP_SWEEP_SOURCE_URL`/`_SHA256` pinned to commit `0abf042`, whose bytes were digest-matched against the local file first |

A fourth cause was found only once the first three were cleared, and it is the
one still open: the sweep exits 0 having found work, but a code node's
persisted output is runner metadata, and no artifact ingest route is mounted —
so the item list never reaches `fix`. That is **#79**, now the single remaining
blocker, with live evidence attached to the issue.

A caution that cost time: the worker image is distroless and has no
`printenv`, so `docker exec … printenv | grep` returns empty and reads as
"the variable is unset". Use `docker inspect -f '{{range .Config.Env}}…'`.

## The two counts, kept separate

They are different measures and the plan uses both. Do not merge them.

- **Items through the pr-upkeep loop** (t17's metric): baseline **1**. Workflow
  completions went to **2**, but the second terminated at `handoff-blocked`, so
  items driven to a merged PR are **still 1**.
  `select wv.workflow_key, count(*) filter (where r.status='completed')
  from runs r join workflow_versions wv on wv.id = r.workflow_version_id
  where wv.workflow_key='pr-upkeep' group by 1`
- **Plan tasks executed through the engine this cycle** (t18's metric):
  **14 runs, 14 distinct tasks, 13 completed**.
  `select count(*), count(distinct category), count(*) filter (where
  status='completed') from runs where category like 'upkeep-%'`

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
- **#73 is closed.** Both halves shipped (t5's CI gate, t6's literal binding);
  verified that `examples/pr-upkeep/workflow.yaml` binds the observable as
  `observe: {literal: {kind: github_pr_merged}}` with the per-cycle `pr`
  number kept as a separate pointer, so the declaration is readable in the
  graph rather than only compiling.

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
| #83 | The capability surface reads two sysctls instead of probing, so it can advertise a sandbox mode the host cannot deliver |

Ten issues were **closed** with evidence citations (41, 43, 45, 46, 47, 49,
56, 64, 65, 68); 54, 48 and 66 left open with written reasons. **#73** was
closed afterwards on the merged t5 + t6 implementation. **#63** now has both
halves — the provisioning docs it asked for and a preflight check that
enforces them — and is ready to close once this branch merges.

## Correction to an earlier version of this file

An earlier draft gave one query — `count(distinct category) … like 'upkeep-%'`
— for a baseline of 1. That conflated the two measures now separated under
"The two counts, kept separate" above. That query counts *this cycle's plan
tasks*, and would have returned 0 at cycle start, not 1; the baseline of 1 is
completions of the **pr-upkeep workflow**, which is a different query against
a different table. Use the two queries above, and record which one any stated
number came from.

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
