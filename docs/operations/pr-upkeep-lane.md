# The pr-upkeep lane: the repeat process, one tick at a time

pr-upkeep is this repo's worked example of a **repeating** loop — the one
process that runs on a clock, without a person starting it, and keeps working
this repo's own quality debt. Everything else in `examples/` demonstrates a
shape; this one is in production and has been running against
`agentculture/culture-nodes` itself.

This page is the operator half of that loop: what one tick actually does, how
to read a tick after the fact, and what changes when you edit the sweep. The
graph's own design — why discovery is split from work, why the fix node may
never merge — lives in
[`examples/pr-upkeep/README.md`](../../examples/pr-upkeep/README.md). The
person-facing half, for someone who only ever sees a Jira ticket, is
[`docs/drive-from-jira.md`](../drive-from-jira.md).

## The loop, in one picture

```text
schedule (interval_seconds: 1800)
    │  pr-upkeep.sweep.due
    ▼
sweep-cycle.workflow.yaml ── one code node ── sweep.py + pr_upkeep_jira.py
    │  pr-upkeep.pr          (one PR, one finding)
    │  pr-upkeep.jira.*      (transitions, comments)
    ▼
workflow.yaml ── fix ──completed──▶ human-merges-pr ──approved/rejected/expired──▶ finish
                  └───no_change───────────────────────────────────────────────────▶ finish
```

Four properties make this a *repeat* process rather than a script someone
runs:

- **The clock starts it.** A durable schedule row fires
  `pr-upkeep.sweep.due` every 30 minutes; a published workflow's trigger
  turns that into a run. No human, no cron on somebody's laptop.
- **Facts, not calls.** The sweep only appends cursor-guarded facts. Which
  workflow consumes them is not the sweep's business, and a fact the trigger
  declines is still durable and queryable.
- **The cursor is the memory.** The control plane advances a watermark in the
  same transaction that appends the event, so a tick that dies halfway cannot
  re-report the position it already reported.
- **A person is always in it.** `fix` opens or updates a PR and stops. The
  merge is an approval node, and no actor in this deployment holds a merge
  credential.

## One tick, precisely

A tick sweeps up to `PR_UPKEEP_MAX_PRS_PER_SWEEP` (default 10) open PRs, and
for each one emits **at most one `pr-upkeep.pr` fact carrying exactly one
finding** — the highest-priority finding not already held by a running
pr-upkeep run.

That "one" is the load-bearing number, and it changed in 0.46.0 (issue #268).
It has two consequences worth holding in your head when you watch the lane:

- **A PR with N findings takes N ticks**, roughly N × 30 minutes, and the
  findings are worked in priority order — SonarCloud/Qodo severities and
  failed CI checks on one shared ladder.
- **A finding is not blocked by the run before it.** Previously a fact
  carried the whole findings list, the dedupe read every id off the running
  run's `input.findings`, and that run then sat parked on `human-merges-pr`
  until a human merged — so the second finding could not be dispatched until
  the first fix was merged. One fix per PR per merge, measured on PR #267.

## Reading a tick

The tick's own report is JSON on the code node's stdout:

```json
{
  "sweep": "pr-upkeep",
  "emitted": 3,
  "skipped_findings": ["pr267-qodo-1"],
  "deferred_findings": ["pr267-qodo-3"]
}
```

Read the three lists as three different states, because they are:

| Key | What it means | What to do |
| --- | --- | --- |
| `emitted` | facts appended this tick (PR findings **and** Jira facts) | nothing; the triggers took it from here |
| `skipped_findings` | held by a **running** run — in flight, possibly parked on a human | decide the approval, or leave it |
| `deferred_findings` | read, outranked this tick, **emittable next tick** | nothing; it is taking its turn |

A finding that is neither emitted nor in either list was not found this tick —
it was resolved, dismissed, or its source surface went quiet.

To see what the facts became:

```bash
bash .claude/skills/nodes-operator/scripts/nodes-op.sh running     # runs in flight, with their input
bash .claude/skills/nodes-operator/scripts/nodes-op.sh run <id>    # one run's nodes, outcomes, attempts
bash .claude/skills/nodes-operator/scripts/nodes-op.sh ledger <id> # what it claimed, and under what authority
bash .claude/skills/nodes-operator/scripts/nodes-op.sh tasks       # approvals waiting on a person
```

A run's `input.findings` is a one-item list naming the finding it is working.
That is what the next tick's dedupe reads, so it is also the answer to "why
was this finding not emitted again".

## The decision that reaches a person

`fix.completed` parks the run on `human-merges-pr` and fans the decision out
three ways — a Jira comment, a board move to `Pending`, and a Discord post.
Two things about that message are contracts, not styling (issue #265):

- it names the node by its **id** (`human-merges-pr`), so the message can be
  traced to a line in the graph;
- it offers only the outcomes **a person may give**. `expired` is implied for
  every approval node and is never offered, because it is what the control
  plane records when it *reads* a fact — the PR was already merged, the
  deadline passed. `DecideHumanTask` refuses it from a decider, and the web
  buttons do not render it.

An approval whose PR gets merged some other way does not sit forever: the
sweep's `pr.merged` fact expires it with reason `pr_merged` and the run ends
down the `expired` edge.

## Changing the sweep

The sweep is **not** in the image. The workflow fetches two files at dispatch
time and refuses bytes whose sha256 does not match the digest it was granted:

| File | Owns |
| --- | --- |
| `examples/pr-upkeep/sweep.py` | sweeping: which repo, which PRs, which findings, naming the stage a failure happened at — and it is the **sole** writer to the control plane |
| `examples/pr-upkeep/pr_upkeep_jira.py` | the Jira surface: what a Jira fact *is* (`jira_emissions`), its cursor position and watermark, self-echo, and the granted Basic-auth pair (`jira_credentials`) |

That split is enforced, not merely intended: `tests/test_pr_upkeep_sweep_jira.py`
asserts the Jira module has no control-plane write path, and the exact-set
environment-read guard in `tests/test_pr_upkeep_sweep_config.py` AST-scans
**both** files, so a credential read cannot escape coverage by moving one file
over.

The recipe for a sweep change:

1. Edit the file that owns the concern. `sweep.py` runs close to the repo's
   1000-line hard limit (`tests/lint`), so a change that does not fit is
   telling you a concern belongs in a module of its own — that is how
   `pr_upkeep_jira.py` got `jira_emissions`.
2. `uv run pytest -n auto -q` and `scripts/lint-all.sh`.
3. Merge. `deploy/prod/deploy.sh` derives **both** URLs and **both** digests
   from the shipped revision, so there is no digest to hand-edit — but there
   is nothing to fetch until the revision is on the branch the deployment
   tracks.
4. Redeploy, then wait one interval and read the next tick's report.

**A sweep change needs no workflow republish**, and that is worth preserving.
`workflow.yaml` is published and content-addressed; runs pin its digest. As
long as an emitted fact still satisfies its input contract, its trigger
condition, and its fix instruction, the loop picks the change up on the next
tick with nothing else to do. #268 was deliberately shaped to keep that true.

## What is deliberately not here

- **No merge credential anywhere in the loop.** The fix actor opens or updates
  a PR; a person merges.
- **No discovery credential in the upkeep graph.** The sweep holds the read
  credentials and the narrow event-ingress token, and it performs no triage.
- **No GitHub PR comment channel** in the fan-out: nothing registered in this
  deployment can write to a PR thread, so a PR-sourced decision goes to
  Discord rather than queueing a message nothing could deliver.
- **No parallel agents on one PR.** One finding per tick is also what keeps
  two fix sessions off the same branch.
