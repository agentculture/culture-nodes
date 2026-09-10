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
schedule (pr-upkeep-sweep-5m, interval_seconds: 300)
    │  pr-upkeep.sweep.due
    ▼
sweep-cycle.workflow.yaml ── one code node ── sweep.py + pr_upkeep_jira.py
    │                                              + pr_upkeep_emit.py
    │  pr-upkeep.pr          (one PR, one finding, one work_item)
    │  pr-upkeep.jira.*      (transitions, comments)
    ▼
workflow.yaml ── analyse ──packaged──▶ fix ──completed──▶ readiness ──▶ human-merges-pr ──▶ finish
                    └─────no_fix─────────────────────────────────────────────────────────────▶ finish
```

Four properties make this a *repeat* process rather than a script someone
runs:

- **The clock starts it.** A durable schedule row fires
  `pr-upkeep.sweep.due` every 5 minutes — the live row is
  `pr-upkeep-sweep-5m` (`interval_seconds: 300`); the original 30-minute row
  the example comment still shows is disabled, not gone — and a published
  workflow's trigger turns that into a run. No human, no cron on somebody's
  laptop.
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
for each one emits **at most one `pr-upkeep.pr` fact carrying one file's
findings** — every finding not already held by a pr-upkeep run that sits on
the same file as the PR's highest-priority finding.

That unit is the load-bearing choice, and it has moved twice. It was the whole
PR until 0.46.0 (issue #268), which made a parked run suppress findings it had
never touched; it was exactly one finding from 0.46.0 until 2.5.0, which made
bundling impossible — no run ever held two findings, so nothing could see that
thirteen of them were the same rule in the same file. It is now the file, which
is the unit a fix actually has. Three consequences worth holding in your head
when you watch the lane:

- **A PR takes one tick per file, not one tick per finding.** The `analyse`
  node judges every finding the fact carries — FIX, PUSHBACK, DUPLICATE or
  SKIP, each with a one-line reason — and bundles the FIX ones into packages
  of one rule in one file. Thirteen instances of `python:S1192` in
  `_output.py` are **one** dispatch, one PR update and one approval, not
  thirteen. A PR whose findings span F files therefore takes roughly F ticks
  at a given head, worked in priority order — SonarCloud/Qodo severities and
  failed CI checks on one shared ladder — and the findings on a file that the
  package it worked did not cover wait for the **post-push re-scan**, because
  the moment the fix pushes their line numbers have moved. That deferral is
  not a second mechanism: it is the head-SHA clause below, said out loud in
  the analysis instruction.
- **A tick can buy no session at all.** If every finding on the chosen file is
  pushed back, duplicate or already resolved, `analyse` reports `no_fix` and
  the run ends at `finish` carrying the verdicts. That is the return on the
  node: the loop used to buy a full developer session to discover a thread had
  been resolved.
- **A finding already answered at this commit is not asked again.** A run
  that ended — the actor said `no_change`, a person rejected the fix — is an
  answer, and the sweep does not re-buy it. A push re-opens every finding on
  the PR, because new code is a new question. This is the second dedupe
  clause; until it was stated, the old byte-identical watermark had been
  enforcing it by accident. Answering it means reading the runs that have
  *ended*, so the sweep walks the whole `pr-upkeep` run listing by
  `next_cursor` rather than its newest page — and if the listing does not end
  within the page bound, it says so on stderr, because past that point the
  clause is a window rather than a guarantee.
- **A finding is not blocked by the run before it.** Previously a fact
  carried the whole findings list, the dedupe read every id off the running
  run's `input.findings`, and that run then sat parked on `human-merges-pr`
  until a human merged — so the second finding could not be dispatched until
  the first fix was merged. One fix per PR per merge, measured on PR #267.

## The work item

Every `pr-upkeep.pr` fact names the work item it belongs to, in its payload,
under `work_item` (issue #310; decision c41). A triggered run's input *is* the
payload, and the engine's trigger reads that one top-level field and stamps it
on the minted run's own `work_item` column (`internal/engine/trigger.go`,
`workItemFromPayload`; migration 0057) — so
`GET /v1alpha1/runs?work_item=SCRUM-9` returns exactly a ticket's runs with no
second write, and `run.category` is untouched. The value is never empty and
has exactly two shapes:

| Shape | Example | When | What the graph does with it |
| --- | --- | --- | --- |
| the correlated Jira key | `SCRUM-9` | the head branch names a key, else the PR body does — the same correlation `pr.opened` / `pr.merged` already use (`pr_upkeep_emit._correlated_issue_key`), narrowed to the configured `jira_project` when the repository has one | `route` selects `keyed`; the run goes straight to `fix` |
| the transient GitHub form | `gh:agentculture/culture-nodes#307` | no key anywhere on the PR | `route` selects `orphan`: `intake-orphan` creates the ticket, `stamp-pr` writes the key into the PR body, then `fix` |
| the orphan ticket | `SCRUM-12`, labelled `orphan`, `auto-created`, `source:github`, `repo:agentculture/culture-nodes` | a `gh:` fact reached `intake-orphan` (task t4) | every later fact for that PR arrives keyed to it; the run that created it is re-keyed to it by `PATCH /v1alpha1/runs/{id} {"work_item": "SCRUM-12"}` — the one transition PATCH admits, once |

The `gh:` form is a placeholder, and the rule is that **it never survives
intake** (decision c42): a PR with no ticket is an *orphan*, the graph's
`intake-orphan` node creates a ticket for it in the configured project
(through the jira bridge's `create_issue` verb, with the four labels above)
and `stamp-pr` puts the key on the PR, so the next sweep correlates the PR to
the ticket and anything downstream that joins on the work item — handover
refs, opened issues, the cleanup record — sees a Jira key. Idempotency is the
PR itself: the jira bridge has no search verb, so "does this PR already have
a ticket" is answered by the stamped body on the following tick, and an
already-keyed fact never reaches `intake-orphan`; a replayed fact (same
source key and watermark) is deduped by the control plane before any run
exists. `intake-orphan` runs with `maxAttempts: 1` because a retried create
is a second ticket.

The run that did the creating still carries the `gh:` key in its own
`work_item` column — the graph cannot address its own run through the API.
Re-keying that column is `PATCH /v1alpha1/runs/{id}` with `{"work_item":
"<key>"}`, accepted only while the column starts with `gh:` and only to a
Jira-shaped key (a 409 otherwise); until a control-plane step performs it,
it is an operator turn and is counted as one. The mapping is readable before
the PATCH regardless: the run's ledger holds the jira actor's proposed
`create_issue` claim naming the key, beside the run input's `gh:` work item.
The sweep does none of this; it only emits the fact with the shape it can
see. (The sweep still has no Jira write path, and
`tests/test_pr_upkeep_sweep_jira.py` still asserts so.)

Two things `work_item` is deliberately **not**:

- It is not `subject`. A `pr-upkeep.pr` fact carries no subject, and still
  does not — subject re-enters the one-active-run-per-subject guard #268
  removed (see "What is deliberately not here").
- It is not `category`. The work item is its own run column with its own
  filter; `category` keeps meaning what it meant.

Because `workflow.yaml`'s input contract is `additionalProperties: false`,
admitting the field meant widening the contract (`work_item` is required, a
non-empty string) and republishing: this is the one kind of sweep change that
*does* need a workflow republish (see "Changing the sweep"); the workflow
became `2.2.0` for it and `2.3.0` when the orphan intake nodes were added. A
sweep emitting `work_item` against a deployment still on
the `2.1.0` contract is refused by the trigger's contract check, loudly, per
fact — which is the right failure, not a silent drop.

## The stage record

The board says where a work item is, and the loop reads its own writing back
as memory. Both halves are one mechanism (issue #311, decision c9): a **stage
comment** whose first line is machine-readable.

```text
culture-nodes:stage=dispatch
The pr-upkeep loop dispatched a developer session for ...
```

Six stages, in this order — the order is load-bearing, because a recorded
stage closes every transition at or before it:

| Stage | Posted by | At which node | Bound key |
| --- | --- | --- | --- |
| `intake` | `examples/jira-intake` | `stage-intake`, after the board move to In Progress | `/run/input/id` |
| `spec` | **nothing yet** | — | — |
| `dispatch` | `examples/pr-upkeep` | `stage-dispatch`, on the `keyed` route before `fix` | `/run/input/work_item` |
| `pr-open` | `examples/pr-upkeep` | `stage-pr-open`, on `fix.completed` before the approval | `/run/input/work_item` |
| `merged` | `examples/cleanup` | `stage-merged`, on the `pr.merged` route before the code node | `/run/input/issue_key` |
| `cleanup` | `examples/cleanup` | `stage-cleanup` / `stage-cleanup-declined`, on `cleanup.passed` | `issue_key` / `work_item` |

`spec` is in the vocabulary and has no writer. Its natural home is the
spec-chain lane (`examples/spec-chain-lane`), which this change did not touch;
until a node there posts it, a reader will never see a `spec` comment and the
enum entry is a reservation, not a behaviour.

**A stage comment is posted by a graph node, never by the sweep.** That is not
a stylistic preference: the sweep has no Jira write path at all
(`pr_upkeep_jira.py` is GET-only, asserted by
`tests/test_pr_upkeep_sweep_jira.py`), and keeping it that way is what keeps
"a sweep change needs no workflow republish" true in one direction and "a
board write is an allowlisted bridge verb under an actor identity" true in the
other. Each node uses the same narrow `post_comment` verb
`examples/jira-intake` already drove, with the bridge's exact-key input
`{verb, issue, comment}`; the bridge appends its own jira-actor marker, and a
graph that wrote one itself would be forging the identity the self-echo filter
trusts.

**Only a Jira-shaped work item can carry a stage.** A `gh:<owner>/<repo>#<n>`
item has no ticket to comment on, so every edge into a stage node is gated —
by its own CEL `when` on the item's shape, or by the decision node whose
outcome it leaves, which tested the same thing. The orphan path
(`intake-orphan` → `stamp-pr`) posts no stage: its ticket is created inside
that run, so the run's input still holds the `gh:` form. The next, keyed fact
for the pull request carries the stages.

One trap is worth naming, because it is invisible in the document. The engine
selects the first eligible edge in the **compiler's normalized order** — source
node, outcome, target, guard text (`internal/compiler/normalize.go`,
`normalizeEdges`) — not in the order the file lists them. A stage node's target
name often sorts *after* the node it diverts from (`stage-pr-open` after
`human-merges-pr`, `stage-cleanup` after `cleaned`), so an unguarded sibling
edge would win before the stage guard was ever evaluated and no ticket would
ever see the stage. Every outgoing edge of a diverted outcome therefore carries
a mutually exclusive guard, and `tests/test_stage_write_back_graphs.py` pins
that no guarded outcome has an unguarded sibling.

### Reading it back as a record

The sweep reads a stage comment as a stage **record** (`jira_stage_record`):
the stage it names, when it was posted, and where it sits on the ticket's
timeline. A record closes exactly one transition — the ticket's pickup —
`STAGE_DRIVEN_BY`, in full:

| Fact | Stage it closes |
| --- | --- |
| `pr-upkeep.jira.transitioned.to-do` | `intake` |

**What is not on that list is the load-bearing half.** `pr.merged` and
`pr.closed` were on it until the two-PR case was measured, and were removed. A
stage comment names a **ticket** and cannot name the pull request it was posted
for: the jira actor's `post_comment` takes exactly
`{verb, issue, comment, question_id}`, and a graph binding is a pointer *or* a
literal, never a composition — so no node can write `pr=<number>` into the
first line. Two pull requests citing one ticket is an admitted case (the
`intake-orphan` path above), and the failure was total rather than partial: PR
A merges at 11:00, cleanup records `merged` at 12:00, PR B merged at 11:30 and
is listed on a later tick, and B's `pr.merged` is then suppressed for the whole
30-day closed lookback. A fact the sweep never sends cannot be deduplicated
downstream — only lost — so B's refs are never cleaned and nothing says why.
Per-pull-request identity exists in the `source_key`, which is where the
dedupe now happens, alone.

`pr-upkeep.pr` — the finding dispatch — was never on the list either. A stage
comment cannot name a head SHA or a finding, and this lane promises a finding
is not blocked by the run before it, so finding dispatch keeps the run-input
walk (`pr_upkeep_emit.undispatched_findings`) as its dedupe. Nothing about the
stage record changes the cadence claim above.

One ordering consequence is worth knowing before an outage surprises someone.
Because no lifecycle fact is gated any more, the closed-PR listing is read and
emitted **early** — after the `pr.opened` facts and *before* both the per-PR
finding loop and the Jira read. A broken SonarCloud, an unreadable check-runs
endpoint or an unreachable Jira still fails the tick, but the merge that
already happened has gone out first: a `pr.merged` is not held hostage by a
finding surface it has nothing to do with. Both facts are re-emitted every tick
by design (the control plane keys on `source_key` plus an immutable-timestamp
watermark and answers a repeat with `duplicate=true`), so a tick over an
already-finished ticket costs one deduplicated signal and mints no run.

Three properties follow from where the write lives:

- a person typing `culture-nodes:stage=merged` into a comment does not make a
  record: only the configured bot account id (or, absent one, the bridge's own
  marker) makes a comment a stage record;
- a stage comment is not itself a work item — the sweep reads it as a record
  and never re-emits it as a `pr-upkeep.jira.comment` fact;
- a ticket moved back to **To Do** after a stage was recorded re-fires pickup
  by design (decision c24): a stage recorded *before* the transition cannot
  close it, and the comparison is by position on the ticket's timeline, not by
  clock.

## Reading a tick

The tick's own report is JSON on the code node's stdout:

```json
{
  "sweep": "pr-upkeep",
  "emitted": 3,
  "skipped_findings": ["pr267-qodo-1"],
  "worked_findings": ["pr267-qodo-4"],
  "deferred_findings": ["pr267-qodo-3"],
  "pushbacks": [
    {"id": "pr267-qodo-2", "reason": "the owner replied that the hint line is deliberately absent from --json", "run_id": "01M19YG9ZJ"}
  ]
}
```

Read the lists as five different states, because they are:

| Key | What it means | What to do |
| --- | --- | --- |
| `emitted` | facts appended this tick (PR findings **and** Jira facts) | nothing; the triggers took it from here |
| `skipped_findings` | held by a **running** run — in flight, possibly parked on a human. A finding is held when its **package** is held: one member in flight holds the file | decide the approval, or leave it |
| `worked_findings` | already dispatched **at this head SHA**; that run has ended (`no_change`, a rejected fix, a failure) | nothing until the PR moves — a push re-opens them all |
| `deferred_findings` | read, outranked this tick, **emittable next tick** | nothing; it is taking its turn |
| `pushbacks` | a **person** declined this finding on the PR thread, and `analyse` recorded it with their reason | tell the source surface — dismiss the Sonar issue, resolve the thread — or the loop reads it again next tick |

`pushbacks` is the only one of the five that is not about scheduling. It is
read off the run **outputs** the dedupe walk already fetched (since 2.5.0 a
pr-upkeep run's output *is* its analysis document), and it exists so a PR
owner's own objection is named back to them rather than silently re-proposed.
The loop does not act on it: nothing here dismisses a SonarCloud issue or
resolves a GitHub thread, so a pushed-back finding keeps arriving until
somebody closes it at the source. What changes is that no session is bought
for it — `analyse` marks it PUSHBACK and it is never packaged.

A finding that is neither emitted nor in either list was not found this tick —
it was resolved, dismissed, or its source surface went quiet.

To see what the facts became:

```bash
bash .claude/skills/nodes-operator/scripts/nodes-op.sh running     # runs in flight, with their input
bash .claude/skills/nodes-operator/scripts/nodes-op.sh run <id>    # one run's nodes, outcomes, attempts
bash .claude/skills/nodes-operator/scripts/nodes-op.sh ledger <id> # what it claimed, and under what authority
bash .claude/skills/nodes-operator/scripts/nodes-op.sh tasks       # approvals waiting on a person
```

A run's `input.findings` lists one file's findings — every one the tick
dispatched, not only the ones the fix took. That is what the next tick's
dedupe reads, so it is also the answer to "why was this finding not emitted
again". Its `output.verdicts` is the other half: what `analyse` decided about
each of them, and why.

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
down the `expired` edge. A PR closed *without* merge raises a `pr.closed`
fact from the same closed listing (source key `github:{repo}:pr:{n}:closed`,
watermark `closed_at`, subject = work item), which nothing consumes yet: the
cleanup node that cancels its parked runs is task t13.

### The readiness block

Before task t18 the approver got two unresolved pointers and assembled the
rest by hand — `workflow.sh status` beside the task, which shells out to
`pr-status.sh`. That was a hand-turn per merge, and it was the one the
approver most needed not to be doing.

It is now a node. `readiness` (`examples/pr-upkeep/readiness.py`) sits between
the fix and the approval and writes one JSON document:

| Field | What it is | Read from |
| --- | --- | --- |
| `ci` | every check on the head commit as `{name, state}` | GitHub check-runs **and** the combined commit-status API — the two surfaces `gh pr checks` merges |
| `sonar` | `gate`, `open_issues`, `hotspots` | the same three SonarCloud queries `pr-status.sh` issues, with the same filters |
| `threads` | `unresolved`, `total` | the GraphQL `reviewThreads` / `isResolved` read `pr-status.sh` performs |
| `devague` | the `proposed` record ids in the checkout, and the `scope` of documents read | the checkout's `.devague` tree |
| `evidence` | the evidence record ids whose outcome is not `pass` | the same tree |

`human-merges-pr` binds it as `readiness: /nodes/readiness/output` and is
reachable from `readiness` and from nowhere else, so **the task is not created
until the collector completed** — a property of the edges, not a habit. The
binding is a pointer: `context_refs` on a human task carries the binding as
authored (`internal/engine/humantask.go`), so what the surface resolves is the
code node's output document, whose `artifacts.stdout_ref` is the block.

Two things to read correctly when a block looks thin:

- **A `null` field is not a zero.** A source the collector could not read is
  `null` plus a named entry in `failures`. "SonarCloud did not answer" and
  "SonarCloud reports no open issues" are different facts and the block never
  merges them.
- **A `readiness.failed` outcome still reaches you.** A collector that
  produced no block at all routes to the approval anyway: the merge authority
  is a human (PRD §10.4), and ending the run instead would turn a missing
  measurement into a dropped decision. The node run's outcome is what says
  which of the two happened.

The `devague` field is also a measurement of something this repo cannot fix
itself. Confirming a plan's proposed records is one CLI invocation per record
per noun in `devague 0.24.0` — 236 of them across the committed tree at the
time of writing — so a one-transaction bulk confirm is an upstream
`agentculture/devague` change, not a task here (spec decision **c14**). The
text of that ask is committed at
[`docs/triage/devague-bulk-confirm-issue.md (posted as agentculture/devague#119, https://github.com/agentculture/devague/issues/119)`](../triage/devague-bulk-confirm-issue.md);
record the issue URL there and in the spec's non-goal once the operator posts
it.

To change the block, change `readiness.py` and re-grant its digest — the same
recipe as the sweep, below.

## Changing the sweep

The sweep is **not** in the image. The workflow fetches three files at
dispatch time, each under its own granted URL and digest, and refuses bytes
whose sha256 does not match:

| File | Owns |
| --- | --- |
| `examples/pr-upkeep/sweep.py` | sweeping: which repo, which PRs, which findings, naming the stage a failure happened at — and it is the **sole** writer to the control plane |
| `examples/pr-upkeep/pr_upkeep_jira.py` | the Jira surface: what a Jira fact *is* (`jira_emissions`), its cursor position and watermark, self-echo, the granted Basic-auth pair (`jira_credentials`), and the granted REST base those two authenticate at (`jira_api_base` — a scoped service-account token is accepted only at the Atlassian gateway; browse links stay on the site host) |
| `examples/pr-upkeep/pr_upkeep_emit.py` | what goes out this tick: both dedupe clauses (`dispatched_finding_ids`, `undispatched_findings`), the unit one fact carries (`finding_package`, keyed by `FINDING_PACKAGE_KEY`), the cursor it is emitted under (`emission_watermark`, `newest_comment_timestamp`) and what the tick reports back off the run outputs (`pushback_findings`). Pure — no credential, no socket; the sweep hands it the run listing |

That split is enforced, not merely intended: `tests/test_pr_upkeep_sweep_jira.py`
asserts the Jira module has no control-plane write path, the exact-set
environment-read guard in `tests/test_pr_upkeep_sweep_config.py` AST-scans
**both** fetched siblings so a credential read cannot escape coverage by moving
one file over, and each sibling is asserted stdlib-only — the bootstrap fetches
exactly the granted set, so an import outside it names a module that will not
be on disk in production.

The recipe for a sweep change:

1. Edit the file that owns the concern. A change that reads a NEW granted
   environment value is the same shape of change as a new sibling file, and
   costs the same four edits: the name in `ALLOWED_ENVIRONMENT_READS`
   (`tests/test_pr_upkeep_sweep_config.py` holds an exact set, not a subset),
   `environmentRefs` in `sweep-cycle.workflow.yaml`, the README's grant table,
   and whichever deploy lane writes it — `install-secrets.sh` for anything
   sitting in `runner-secrets.env`. The runner boundary refuses an operation
   by NAME before it runs, so a granted name the deployment does not carry
   fails every sweep in 1ms rather than degrading. `JIRA_API_BASE` (task t21)
   is the worked example.

   `sweep.py` runs close to the repo's 1000-line hard limit
   (`tests/lint`), so a change that does not fit is telling you a concern
   belongs in a module of its own — that is how
   `pr_upkeep_jira.py` got `jira_emissions` and `pr_upkeep_emit.py` got the
   dedupe. A new sibling needs its URL + digest pair in
   `deploy/prod/lanes/runner-env-write.sh`, the bootstrap's source list and
   `environmentRefs` in `sweep-cycle.workflow.yaml`, `GRANT_CHECK_DEPLOY_GRANTS`
   in `lanes/grant-check.sh` (or the deploy refuses itself), and the README's
   grant table.
2. `uv run pytest -n auto -q` and `scripts/lint-all.sh`.
3. Merge. `deploy/prod/deploy.sh` derives **every** URL and digest
   from the shipped revision, so there is no digest to hand-edit — but there
   is nothing to fetch until the revision is on the branch the deployment
   tracks.
4. Redeploy, then wait one interval and read the next tick's report.

**A sweep change needs no workflow republish**, and that is worth preserving.
`workflow.yaml` is published and content-addressed; runs pin its digest. As
long as an emitted fact still satisfies its input contract, its trigger
condition, and its fix instruction, the loop picks the change up on the next
tick with nothing else to do. #268 was deliberately shaped to keep that true.
The exception is a change to the fact's *shape*: the input contract is
`additionalProperties: false`, so a new payload key (as `work_item` was, #310)
widens the contract and the workflow is republished in the same change.

## What is deliberately not here

- **No merge credential anywhere in the loop.** The fix actor opens or updates
  a PR; a person merges.
- **No discovery credential in the upkeep graph.** The sweep holds the read
  credentials and the narrow event-ingress token, and it performs no triage.
- **No GitHub PR comment channel** in the fan-out: nothing registered in this
  deployment can write to a PR thread, so a PR-sourced decision goes to
  Discord rather than queueing a message nothing could deliver.
- **No *unjudged* batching of findings into one fix.** One dispatch per PR per
  tick, and the fix node works one package — one rule in one file — which the
  `analyse` node assembled after reading each finding's thread. A package is
  not "the findings that happened to arrive together": findings a person
  pushed back on, findings whose thread is resolved, and findings that
  duplicate another are given a verdict and left out of it. Two packages are
  never merged, and the rest of the file waits for the post-push re-scan.

  What this does **not** promise is that only one fix session ever touches a
  PR. If a fix outlasts the 5-minute tick, the next tick dispatches the next
  finding while the first session is still working, and both sessions can push
  to the same branch. That is the deliberate trade of #268 — waiting for the
  first fix to *merge* was the bug — but it is a real property, not an absence
  of one. It is also not new in kind: the sweep already mints up to ten runs a
  tick across different PRs, all dispatching to the same actor, and nothing
  serializes them. The engine's one-active-run-per-subject guard does not
  apply, because a `pr-upkeep.pr` fact deliberately carries no subject —
  giving it one would re-introduce exactly the block #268 removed. The fact
  does carry a `work_item` (see "The work item"), and that is a different
  thing on purpose: the engine stamps it on the run as its own column and
  never keys any guard on it. Bounding
  same-PR overlap would mean suppressing a dispatch while an earlier run is
  still in `fix` (as opposed to parked on the approval), which is tracked
  separately.
