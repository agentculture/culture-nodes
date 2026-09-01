# pr-upkeep — the loop that keeps this repo's own quality debt worked

Plan task t21 (spec claims c15/c26, honesty condition h21): culture-nodes
uses itself to sweep its **own** unresolved SonarCloud issues, open Qodo
PR findings and failed CI check runs, and works the resulting list one item
at a time through a human-gated fix/review cycle. Every loop iteration
passes a person; the flow can propose, fix, and review — it can never
merge.

This file is the **graph's** documentation: what the nodes are and why they
are shaped this way. The **operator's** half — what one tick does, how to read
one after the fact, and the recipe for changing the sweep — is
[`docs/operations/pr-upkeep-lane.md`](../../docs/operations/pr-upkeep-lane.md).
The person-facing half, for a reader who only ever sees a Jira ticket, is
[`docs/drive-from-jira.md`](../../docs/drive-from-jira.md).

## Deployment configuration

Everything this example needs from the world outside its graph, and where
each value comes from. Loading it into a deployment that is not this one
means supplying these — it never means editing `workflow.yaml`.

| Value | Where it comes from |
| --- | --- |
| `repo`, `review_repo` | **Run input.** Two per-host working directories: `repo` is where the fix actor works, `review_repo` is the review actor's own allowlisted checkout on its own machine. Neither is the work under review — that crosses as `handoff` (see "The cross-machine handoff"). |
| `fix_instruction`, `review_instruction`, `ask_instruction`, `await_reply_instruction`, `merge_instruction`, `notify_title`, `notify_description`, `review_sandbox` | **Run input.** The words each actor is given, authored per run; [`driver.sh`](driver.sh) carries this deployment's defaults and every one is overridable by an environment variable it documents. |
| `actor://company/developer` | **Actor registry.** The fix lane, and the only identity here holding a GitHub write credential. |
| `actor://company/codex-thor` | **Actor registry.** The review lane — deliberately a different backend *and* host from the fix lane. The `thor` in the id is a registry key naming a role, not a hostname you must own: `internal/worker/registry.go` resolves the identity against your actors table (with the `@sha256` revision suffix stripped), so you register the same id against your own endpoint. |
| `actor://company/human-ops` | **Actor registry.** The `kind=human` inbox bridge. |
| `actor://company/notify-discord` | **Actor registry.** The notify adapter (issue #68). |
| `runner://headspace/docker` | **Runner registry.** The code-node runner boundary the sweep dispatches through. |
| `PR_UPKEEP_SWEEP_SOURCE_URL` | **Granted environment value** on the sweep operation. Where `sweep.py` is fetched from at dispatch time. |
| `PR_UPKEEP_SWEEP_SOURCE_SHA256` | **Granted environment value.** The sha256 those fetched bytes must have; the bootstrap refuses to execute anything else. |
| `PR_UPKEEP_SWEEP_JIRA_SOURCE_URL` | **Granted environment value.** Where the sibling `pr_upkeep_jira.py` read/replay module is fetched from. |
| `PR_UPKEEP_SWEEP_JIRA_SOURCE_SHA256` | **Granted environment value.** The independently checked sha256 for the Jira module. |
| `PR_UPKEEP_SWEEP_EMIT_SOURCE_URL` | **Granted environment value.** Where the sibling `pr_upkeep_emit.py` dedupe/cursor module is fetched from. |
| `PR_UPKEEP_SWEEP_EMIT_SOURCE_SHA256` | **Granted environment value.** Its independently checked sha256. |
| `PR_UPKEEP_REPOSITORIES` | **Granted environment value.** An ordered JSON object containing `cycle` and the closed `repositories` set. Each entry supplies `github_repo` and `sonar_component`; optional `jira_site` and `jira_project` (required together) enable Jira for that repo. `jira_bot_account_id` is independently optional: the system's own Jira `accountId`, used only to filter self-authored comments out of the comment/resume event (task t9) — see "Jira event vocabulary" below. The cycle index selects exactly one entry per sweep. |
| `JIRA_ACCOUNT_EMAIL`, `JIRA_API_TOKEN` | **Granted environment values.** The two separately configured Jira Cloud Basic-auth values. They are never run input, argv, output, or fixture data. |
| `JIRA_API_BASE` | **Granted environment value**, optional (empty is a valid grant). The REST base those two authenticate at. A *scoped* Jira Cloud service-account token is accepted only at the Atlassian gateway (`.../ex/jira/<cloudId>`) and the site URL answers 401 for it; empty means `https://<jira_site>`. Refused at parse time unless it is an https URL with no query or fragment. Browse links (`details_url`) always use the site host — the gateway serves the API, never the board. |
| `PR_UPKEEP_MAX_PRS_PER_SWEEP`, `PR_UPKEEP_REQUIRED_CHECKS`, `GITHUB_TOKEN` | **Process environment of the sweep.** These remain optional; the GitHub token only changes rate-limit headroom. |

The granted environment values are the ones a reader cannot trace from the
document alone: they resolve in the worker process that dispatches the
operation, and the runner boundary refuses the operation **by name** when
one is unset (`internal/runners/headspace/bridge.go`'s `resolveEnv`). That
is why they are named in the workflow's own header as well as here.

Until task t16 the sweep's script came from a `raw.githubusercontent` URL
pinned to one org, one commit and one path. A third party who loaded this
example got a graph that silently fetched and executed *our* bytes — a
supply-chain property, not a portability wart. Fork `sweep.py`, publish your
copy, and grant its URL and digest:

```bash
sha256sum examples/pr-upkeep/sweep.py examples/pr-upkeep/pr_upkeep_jira.py \
  examples/pr-upkeep/pr_upkeep_emit.py
```

Both values are resolved by the **runner** process, from its own
environment — see [`deploy/prod/README.md`](../../deploy/prod/README.md)'s
"Granted environment values" for where they live on this deployment and how
`deploy.sh` re-grants them.

A digest mismatch, or either value unset, exits nonzero, which is the
sweep node's technical-failure path: `sweep.failed` routes to the
`sweep-failed` end node and the run says so. The `0` / `10` / other
exit-code contract is untouched — `0` (events emitted) and `10` (nothing to
emit) are both successful emitter passes, and anything else fails.

## The graph

PR upkeep v2 separates discovery from work on a finding. The two workflows
communicate through durable events; neither loops back to discover another
item.

```text
  schedule
      │ pr-upkeep.sweep.due
      ▼
  sweep-cycle.workflow.yaml
      │ runs sweep.py and emits pr-upkeep.pr events
      ▼
  workflow.yaml v2 (one run per matching event)

  fix.completed ──▶ human-merges-pr ──approved/rejected/expired──▶ finish
      │
      └──no_change───────────────────────────────────────────────▶ finish
```

- [`sweep-cycle.workflow.yaml`](sweep-cycle.workflow.yaml) is triggered by
  the scheduled `pr-upkeep.sweep.due` event. Its single code node runs
  [`sweep.py`](sweep.py) through the
  `runner://headspace/pr-upkeep-sweep` runner. The script reads the configured
  repositories and emits `pr-upkeep.pr` events for GitHub PR findings. Exit
  codes `0` (events emitted) and `10` (nothing to emit) both complete the
  emitter successfully; other exit codes take its technical-failure path.
- [`workflow.yaml`](workflow.yaml) is v2 of the upkeep workflow. It starts a
  run for each `pr-upkeep.pr` event whose payload is from a GitHub PR and has
  at least one finding. The event payload is the run input, so the repository,
  PR identity, head SHA, and the finding are durable before an actor starts.
  One event carries **one** finding — the highest-priority one still
  undispatched (see "One finding per fact" below).
- **fix** is the agent node. Actor affinity selects the security developer
  when the finding on the event is a security finding and the general
  developer otherwise. The actor works that finding and either reports
  `completed` after opening or updating a PR, or `no_change` when a fix would
  be inappropriate.
- **human-merges-pr** is the approval node reached by `fix.completed`. A
  platform maintainer decides the merge outcome; `approved`, `rejected`, and
  `expired` are all terminal for this run.
- **finish** is the end node. It receives `fix.no_change` directly and every
  terminal outcome from `human-merges-pr`, then returns the original event
  payload.

## Idle vs blocked (issue #71)

The v2 split makes these different states explicit:

- **Idle means there is no upkeep run.** The schedule starts the discovery
  workflow. When its script has no new event to emit, it exits successfully
  and the run ends. The next schedule event starts a fresh discovery pass;
  no human task or parked upkeep run represents an idle repository.
- **Blocked means one event-created run is waiting at `human-merges-pr`.**
  The actor has already produced a proposed fix, and the approval node holds
  that run until a maintainer approves or rejects it, or its deadline expires.
  Each outcome routes to `finish`; it never restarts discovery or reuses the
  run for another finding.

## The human-merges rule

**This flow holds zero merge credentials in the control plane.** The sweep
is read-only against its configured finding APIs; the fix actor can push
branches and open PRs but never merge; the review actor is read-only outright; the control
plane process holds no GitHub credential at all (spec claim c13, issue #54).

**How the workflow's `approved` outcome works now:** The human merges the PR
in their own session, and a separate **merge tracker** (adapters/human-inbox,
outside the deployment) observes the merge event through the GitHub API. When
the tracker observes the PR transitioning to `merged: true`, it automatically
submits the task through the bridge's existing submit surface with an
**observed-submission claim** that names the merge commit. The engine records
this as a `data.kind: "observed-submission"` ledger entry carrying the
`collection_method: "github_pr_merged"` and the merge commit SHA — honest
attribution without claiming runner-observed authority.

**The credential boundary:** The GitHub credential (`GITHUB_TOKEN`) lives
only in the tracker process beside the human-inbox bridge, outside the
control-plane deployment. The tracker is stdlib-only and polls GitHub only
for the declared observable (the PR number). The control plane continues to
make zero GitHub API calls and holds zero GitHub credentials.

**Override lanes stay intact:** Tasks with no declared `observe` block
behave exactly as today — purely manual submit through the inbox. A person
can still submit a task manually at any time, even if an observable is
declared; the manual path is the override for dropped work and merges without
a PR. A PR closed without merging does NOT auto-complete `human-merges-pr` —
only the merged state is unambiguous enough to trigger automatic submission
there.

## The closed repository-set boundary (claim c26)

The boundary remains deliberate, but moved from one code-pinned repo to a
closed set granted by the deployment. `fetch_open_pulls` enumerates every
open PR and reads its comments and checks with the sweep credential, so run
input still cannot name or select a repository. `PR_UPKEEP_REPOSITORIES`
contains the ordered set and a cycle index; one entry is swept and named in
the report. Before adding an entry, provision its checkout and exact-match
allowlist on both fix and review hosts, and scope the read credential to the
same set.

Jira is additive inside that selected entry. `jira_site` is a host name and
`jira_project` a project key; neither is a module constant. Jira Cloud REST
v3 uses HTTP Basic from `JIRA_ACCOUNT_EMAIL:JIRA_API_TOKEN` and the sweep
budgets at the measured 350-request window. The committed acceptance is the
recorded `fixtures/jira-search.json` response because the probed live backlog
is empty. A live proof is a separate gate, blocked until backlog content
exists; it is not a success signal for this batch.

- the `repo` run input is the local checkout path the fix/review bridges
  allowlist; a run naming a non-allowlisted repo is refused by the bridges
  themselves.

### Jira event vocabulary (task t9)

A Jira issue's current state and "a comment appeared" are different facts
and carry different event names, so a workflow trigger can subscribe to one
without ever receiving the other (#118 step 1's only remaining structural
gap in the sweep):

- Every fetched issue raises `pr-upkeep.jira.transitioned.<status-slug>`
  (`jira_transition_event_name`) on its own `:status` source key — e.g. an
  issue in `Ready for Dev` raises `pr-upkeep.jira.transitioned.ready-for-dev`.
  This is attempted every sweep pass for every fetched issue, the same as
  `pr-upkeep.pr`; the control plane's watermark-equality dedup, not this
  process, is what makes an unchanged status a silent no-op instead of a
  repeat delivery.
- A fresh comment separately raises `pr-upkeep.jira.comment` on its own
  `:comment` source key. `jira_comment_is_self_echo` accepts either the
  configured `jira_bot_account_id` or the Jira actor's fixed body marker;
  the marker keeps filtering correct while the deployed token belongs to a
  person. A question marker carries its `question_id`, and the next human
  reply's event copies that value as `originating_question_id` alongside the
  answer comment. The watermark position is unfiltered, so it already sits
  past the actor's own comment once a real reply lands.

The GitHub lifecycle vocabulary is separate. A correlatable open PR raises
`pr.opened` with `source=github_pr`, repository, number, URL, `opened_at`, and
`issue_key`; its immutable opening timestamp is the durable once-per-PR
watermark. A correlatable merge raises `pr.merged` with the same identity and
`merged_at`. Both correlations prefer the head branch and then the body, and
when `jira_project` is configured they accept only that project's keys.

## Dedupe by finding id (spec c7/h6)

The watermark answers *did this PR move* — head SHA plus newest comment
timestamp. It does not answer *is this finding already being worked*, and
those are different questions. A push moves the watermark, so every
still-open finding was re-emitted, and each emission minted a fresh
pr-upkeep run and a fresh `human-merges-pr` approval. On prod,
`pr236-qodo-1` sat in four running runs at once
(`01M1636D…`, `01M163RN…`, `01M1641W…`, `01M164B1…`) — four approvals for
one piece of work.

The second key is the finding id. Before emitting, the sweep reads
`GET /v1alpha1/runs?workflow_key=pr-upkeep&state=running`
(`fetch_running_finding_ids`) and drops every finding id one of those runs
already carries in its `input.findings` — a triggered run's input *is* the
event payload, so that field is exactly what was emitted for it. The
surviving findings are emitted normally; the skipped ids are named in the
sweep's stdout summary under `skipped_findings`.

Three decisions worth stating:

- **The watermark logic is unchanged.** This is a second key layered on top,
  not a replacement: an unmoved watermark is still a no-op at the control
  plane, and a moved one still reaches this filter.
- **A PR whose findings are *all* in flight emits nothing at all**, rather
  than an event with an empty findings list. An empty list would consume
  that watermark for a fact the trigger declines
  (`size(event.payload.findings) > 0`), which would strand those findings at
  that head SHA even after the runs holding them end. Skipping leaves the
  position unconsumed for a later cycle. A PR that genuinely has no findings
  is untouched by this — nothing was deduped away, so it emits as before.
- **An unreadable runs list fails the sweep**, with its own `attempting`
  stage, rather than degrading to "emit everything": that fallback is the
  duplicate-approval bug, and restoring it silently is worse than a named
  failure. The read hits the same host the emission POSTs to, so a control
  plane that cannot answer it could not have accepted the events either.

One honest limit: the runs list is cursor-paginated and newest-first, and
the sweep follows `next_cursor` for at most `RUNS_MAX_PAGES` pages of
`RUNS_PAGE_LIMIT`. A run history longer than that is read as a window rather
than as the population, and a finding answered by a run past the bound can be
dispatched again — reported on stderr when it happens, never silently.

Another: findings suppressed at a given head SHA are re-offered
only once the watermark moves again. If a run in flight ends while the PR
sits still, its finding waits for the next push or comment. Closing that
would mean changing the watermark, which this task deliberately did not.

Issue #268, below, changed the watermark — but **not** this limit. It
un-strands a *different* finding at an unmoved head; the *same* finding, on a
stationary PR, whose run has ended, still waits for the next push. That is no
longer an accident of the cursor either: it is dedupe clause 2, stated
deliberately, because a run that ended is an answer and re-asking at the same
commit re-buys it.

## One finding per fact (issue #268)

The dedupe above reads *every* id on a running run's `input.findings`. The
fix node works exactly one — "take the HIGHEST-PRIORITY item from the
prioritised findings list and work only that one item" — so a fact carrying
the whole list named N findings, worked one, and made the other N-1
undispatchable for as long as that run lived. And a pr-upkeep run lives until
a human merges: it parks on `human-merges-pr`. Net effect, measured on PR
\#267 (run `01M19YG9ZJ…` carried `pr267-qodo-1` and `pr267-qodo-2`, worked
`-1`, parked): **one fix per PR per merge**, with the second finding worked by
hand in-session.

A fact now carries exactly the finding it dispatches — `findings` is a
one-item list — so the run's input is an honest statement of what is in
flight, which is what the dedupe was already assuming it was. Findings that
lost the priority ordering are reported separately from suppressed ones, as
`deferred_findings`: they are emittable next cycle, not waiting on anything.

That alone is not enough, and the second half is easy to miss. The control
plane's duplicate check is an **equality test** of the submitted watermark
against the one row stored for this source key
(`internal/store/postgres/signal.go`, `deliverSignalEventTx`). At an unchanged
head SHA with no new comments, the second finding's fact carries a byte-
identical watermark, is answered `duplicate=true`, and never mints a run — the
dedupe fix undone one layer down. So the dispatched finding id joins the
watermark (`emission_watermark`): head SHA and newest comment answer *did this
PR move*, the finding id answers *is this a different piece of work*.

Two properties fall out of that shape:

- **A PR with N findings gets N fixes before its merge**, one per tick, in
  priority order — not N dispatched at once. (It does *not* promise only one
  fix session ever touches a PR: if a fix outlasts the tick, the next
  dispatch overlaps it. See the recipe's "one tick, precisely".)
- **A PR with no findings keeps the two-key watermark it always had**, so
  rolling this out does not re-deliver every clean PR's current head.

### The clause the old cursor was enforcing by accident

Adding the finding to the cursor removed something nobody had written down.
When the watermark was `{head_sha, newest_comment_at}` it was byte-identical
on every tick, so `duplicate=true` suppressed not only "this finding again"
but **"anything again, at this commit"** — including a finding whose run had
already ended. Make the cursors differ per finding and that goes away: two
findings that both end `no_change` alternate forever, one billable agent
session every 30 minutes, each re-working a finding an actor already declined.

So the implied rule is now a stated one, in `pr_upkeep_emit.py`, and a
finding is emitted only when **both** clauses hold:

1. **no run is working it**, at any head sha — a second dispatch of a finding
   in flight is a second `human-merges-pr` approval for one change (the t12
   clause);
2. **no run has already worked it at THIS head sha**, whatever that run
   decided — `no_change`, a rejected fix and a technical failure are all
   answers, and re-asking at the same commit re-buys an answer already given.

Clause 2 is scoped to the commit, not the finding: a push is new code, so a
moved head re-opens every finding on the PR. The net contract is the old one
plus exactly what #268 wanted — **nothing new until the PR moves, except a
finding nobody has worked yet**.

Answering clause 2 is why the run listing is no longer filtered to
`state=running`: the runs it asks about are precisely the ones that have
ended. Four consequences follow, all four caught by a Qodo review of PR #269
before they shipped — and the first fixed by **this loop itself**, which
picked the finding up off #269 and pushed the commit:

- **The listing is followed to the end.** It is cursor-paginated and
  newest-first, so ended runs are exactly what a single page drops, and a
  dropped run's finding becomes eligible again at the same head. Past a page
  bound the sweep says so — on stderr *and* as `dedupe_complete: false` in the
  report — because past it clause 2 is a window rather than a guarantee.
- **In flight means non-terminal, not `running`.** `waiting` is a real state
  and a future control plane may add more, so anything outside
  `completed / failed / cancelled` counts as in flight. A finding wrongly held
  back appears in `skipped_findings` where a reader sees it; a finding wrongly
  released is a second billable session nobody sees.
- **The answer is scoped to the repository being swept.** Finding ids carry a
  PR number and an index, never a repository, so a fork and its upstream —
  which share commit shas by construction — would otherwise answer each
  other's questions.
- **The environment-read guard scans every fetched module.** The helper
  written to stop coverage escaping by moving code between files did not list
  the file the code then moved into; a test now reads the bootstrap's own
  source tuples, so the guard's list and the workflow cannot drift.

The tick's summary keeps the refusals apart — `skipped_findings` (in flight,
possibly waiting on a person right now) and `worked_findings` (settled until
the PR moves) — because a reader acts on them differently.

On the first tick after the deploy, a PR *with* findings has a stored
two-key watermark and will be sent a three-key one, which is not a duplicate —
so it emits. Clause 2 then decides whether that becomes a run: if a previous
run already carried that finding at this head sha, it is refused as
`worked_findings`, and only a finding nobody has worked goes out. The rollout
is therefore quiet by construction rather than by luck, and the first tick is
worth reading (`worked_findings` will be long).

### What moved, and why the split is where it is

`sweep.py` sat at 998 of the repo's 1000-line hard limit
(`tests/lint`), so this change did not fit until something left. What left is
the Jira half: `pr_upkeep_jira.py` — already a separately fetched module under
its own granted URL and digest — now owns **what a Jira fact is**
(`jira_emissions` shapes each `raise_event` call; `jira_credentials` reads the
two granted Basic-auth halves), and `sweep.py` keeps the half that is actually
the sweep's: naming the stage a failure happened at, and being the **sole**
writer to the control plane. The Jira event vocabulary moved with it, out of
the sweep's module docstring and out of its import list — re-exporting names
nothing in the file called made the sweep look like it had opinions about Jira
that it does not.

Two guards moved with the code rather than being left behind:

- the exact-set environment-read guard now AST-scans **both** fetched modules
  (`tests/test_pr_upkeep_sweep_config.py`), because a credential read that
  escapes coverage by moving one file over is exactly the erosion that set
  exists to stop;
- `pr_upkeep_jira.py` is asserted to have no control-plane write path at all,
  so "sweep.py is the sole emitter" stays a fact rather than a habit.

Then the emission concern followed it into `pr_upkeep_emit.py` — both dedupe
clauses and the cursor — under its own granted URL and digest. Both siblings
are asserted stdlib-only for the same reason the sweep is: the bootstrap
fetches exactly the files it has been granted, so an import outside that set
names a module that will not be on disk in production.

`deploy/prod/deploy.sh` derives all three digests from the shipped revision,
so this needs no hand-edited grant — only a redeploy. `grant-check.sh` knows
the two new keys, or the deploy that first grants them would be refused by
its own preflight (`tests/deploy/grantsafety_test.go` pins that pairing).

`workflow.yaml` is untouched, deliberately: a one-item list satisfies its
published input contract (`findings` minItems 1), its trigger
(`size(event.payload.findings) > 0`), and its fix instruction verbatim — so
this ships by deploying the sweep, with no workflow republish. Its
`security-findings` affinity rule gets sharper for free: `findings.exists(f,
f.kind == "security")` now asks whether the finding *being dispatched* is a
security finding, rather than whether any finding on the PR is.

## The cross-machine handoff (issue #74)

`fix` and `review` are deliberately different actors — that is the whole
independent-review pattern — and different actor increasingly means
**different machine**: `company/developer` is the spark claude-code bridge,
`company/codex-thor` is the codex bridge on thor. A filesystem path does not
survive that boundary, and this graph used to hand one across it anyway.

What it looked like, in run `01KZZSGSWH11J7R7P4V2HPTZZQ` — a v1 run, back
when discovery still ran inside this graph:

```text
fix     completed  completed     <- real session on spark, committed b01608c
review  failed     auth_or_policy (HTTP 403): actor answered Forbidden
```

The bridge was right. It was handed `/home/spark/git/.worktrees.culture-nodes/upkeep-fix`,
which is outside thor's allowlist and does not exist on thor at all. The
problem is that the error names **authorization** when the cause is
**topology** — which points a reader at credentials and away from the actual
defect.

Three things changed, and the invariants are locked by
[`tests/lint/crosshosthandoff_test.go`](../../tests/lint/crosshosthandoff_test.go):

1. **`fix.completed` requires a handle, and `review` requires one too.** Both
   contracts embed the same rule, declared once in
   [`schemas/workflow/handoff.schema.json`](../../schemas/workflow/handoff.schema.json),
   and it admits **two carriers** (spec decision `q9`, task t6):
   - a runner's **changes** travel as `kind: git_ref` —
     `git+<https|ssh>://<remote>#refs/culture-nodes/<run-id>/<node-run-id>`
     plus the `commit` it pins. A remote, a ref name and a sha carry no host
     and no path, which is the property #74 actually required;
   - **context and data** that is not naturally a git object travel as
     `kind: artifact` — an `artifact://<namespace>/<id>` reference, which
     "never carries or implies a filesystem path"
     ([`internal/artifacts/doc.go`](../../internal/artifacts/doc.go)) and
     resolves through the store from any host. The sweep's prioritised item
     list is JSON, so it is on this side of the rule.

   Each carrier's ref is pattern-constrained so neither can quietly become a
   path again — including the near misses, `git+file://` and a bare branch
   name. Because the engine validates a completion against the outcome schema
   ([`internal/engine/complete.go`](../../internal/engine/complete.go)'s
   `checkOutput`), a fix that produced no handle **cannot report
   `completed`**. This is enforced, not advised.
2. **A fix host that cannot produce one says so, by name.** The
   `handoff_unavailable` domain outcome requires a `missing_capability` from
   a closed set — `artifact_publish`, `git_ref_publish`, `workspace_export`,
   `handoff_too_large` — so the answer is a name, not a sentence to
   interpret. Two carriers means two publish capabilities, and a host can be
   missing either one alone.
3. **That outcome never reaches `review`.** It routes to the terminal
   `handoff-blocked` node, which carries the fix node's output (where
   `missing_capability` lives) as the run's output. `finish` would have
   buried it under the sweep report.

**This section used to say a git ref was impossible**, because a probe of the
spark bridge host found HTTPS origin, no SSH key, no `credential.helper` and
no token in the bridge process. That was true when it was written and no
longer is: task t1 provisioned `GITHUB_TOKEN_WORKER` into the bridge units,
and issue #91 measured that a codex session under `--sandbox workspace-write`
can write `.git` once the dispatch widens `writable_roots` (deviation `d6`).
So [issue #74](https://github.com/agentculture/culture-nodes/issues/74)'s own
recommendation — "option 1, with 2 for anything not naturally a git object" —
is what the graph now says, and the widening was a decided change rather than
drift: it retired boundary `c3`, which its own honesty condition `h17` had
already pinned as falsifiable.

**What is not wired yet.** Both carriers are half-built, and in the same half:
a producer can create the handle, and no consumer can yet follow it.

- **`artifact`** — task t5 mounted the publish side
  (`POST /v1alpha1/attempts/{attemptID}/artifacts`, attempt-token scoped).
  There is still no fetch route on the control plane's HTTP surface, and
  `/nodes/<id>/artifacts` is not a bindable surface — refused today by both
  [`internal/compiler/contract.go`](../../internal/compiler/contract.go) and
  [`internal/worker/bindings.go`](../../internal/worker/bindings.go), and
  those two verdicts must change together.
- **`git_ref`** — the bridge-side producer is `preserve.handover_ref`, in all
  three code adapters (all-backends rule). It mints
  `refs/culture-nodes/<run-id>/<node-run-id>` out of the **same**
  write-tree/commit-tree/update-ref plumbing preserve-on-failure uses, and it
  **never pushes**: `AGENTS.md` lets an agent create a handover ref and
  forbids it to push or to commit onto a branch, so the handle it reports
  carries `publication: pending` and the ref is reachable from no branch until
  the operator or the control plane moves it. Nothing moves it yet, no
  dispatch requests one yet, and no node fetches one yet.

So today a fix host still lands on `handoff_unavailable`, naming whichever
publish capability it is missing. That is the true state of the system, and a
far better answer than reviewing thor's own checkout and calling it a review
of spark's work.

## The extractor and its fixtures

[`sweep.py`](sweep.py) is stdlib-only Python 3.12 (the reference bridges'
no-PyPI-graph constraint). Its parsing is deterministic and unit-tested in
[`tests/test_pr_upkeep_sweep.py`](../../tests/test_pr_upkeep_sweep.py)
against **recorded** fixtures:

- `fixtures/sonarcloud-issues.json` — the live issues-search response for
  this repo, recorded 2026-08-13: the four standing unresolved issues
  (1 BLOCKER `python:S3516`, 2 CRITICAL `go:S3776`, 1 MINOR `godre:S8193`)
  the t22 live run targets.
- `fixtures/qodo-pr35-code-review.body.txt` and
  `fixtures/qodo-pr42-code-review.body.txt` — the verbatim
  `Code Review by Qodo` comment bodies from PR #35 and PR #42 (stored as
  `.txt` so the repo-wide markdownlint sweep does not lint recorded HTTP
  payloads as repo documentation). Both recorded reviews carry only
  resolved/dismissed findings, so the tests exercise the open-finding path
  by stripping the `✓ Resolved` / `✗ Dismissed` / `<s>` markers from the
  same recorded bodies — which is exactly the shape of an open finding.
- `fixtures/github-check-runs-pr60.json` — the live check-runs response for
  PR #60's head commit `67672519`, recorded 2026-08-14. This is the exact
  payload issue #61 was filed on: a red `lint` job the two-source sweep
  could not see, beside a **passing** `SonarCloud Code Analysis` check.
- `fixtures/github-check-runs-pr60-sonar-gate-failed.json` — the same
  recording with the SonarCloud check's quality gate flipped to failed, so
  one payload carries a failed Sonar-named check **and** a failed non-Sonar
  check and the skip below is provable. It is derived rather than recorded
  because culture-nodes has never had a red SonarCloud check run in its
  recorded history (checked across the last 60 commits on main on
  2026-08-14: two failing check runs, both `github-actions`). Exactly three
  fields differ from the verbatim recording — `conclusion`, `output.title`,
  `output.summary` on that one check run — and a test asserts that rather
  than leaving the provenance as prose.

### The check-runs source (issue #61)

A red CI check is neither a SonarCloud issue nor a Qodo review body, so the
two-source sweep reported *nothing* for PR #60 while its `lint` job sat
red. The third source reads
`GET /repos/{repo}/commits/{head_sha}/check-runs` for each swept PR — the
same `MAX_PRS_PER_SWEEP`-capped set the other two per-PR queries use, one
more request per PR, no new host and no new credential. Three decisions
worth stating here:

- **Sonar-named checks are skipped** (matched on the check name *and* the
  `sonarqubecloud` app slug, so a renamed check is still caught). A red
  quality gate is not separate work from the issues that made it red, and
  those already arrive through the Sonar feed carrying a rule, a file and a
  line — where the check run carries only "the gate failed".
- **Severity maps onto the existing ladder**, not a fourth vocabulary: a
  failed required check takes `CRITICAL` (rank 1, the HIGH/CRITICAL band),
  a failed optional one takes `MEDIUM` (rank 2).
- **Required-ness is declared, not read.** The check-runs API does not
  report it, and `GET /repos/{repo}/branches/main/protection` answers 404
  *Branch not protected* for this repo (checked 2026-08-14), so
  `REQUIRED_CHECKS` names the three merge gates CLAUDE.md documents
  (`test`, `lint`, `version-check`). Override with
  `PR_UPKEEP_REQUIRED_CHECKS`; when branch protection does land, that
  declared list should be replaced by the protection API's own answer.

Only *completed* check runs with a `failure`, `timed_out` or
`action_required` conclusion become work. `cancelled` deliberately does
not: a cancelled run is a superseded or interrupted job, not a finding.

Run them with the normal suite:

```bash
uv run pytest tests/test_pr_upkeep_sweep.py -v
```

## Validate and run

```bash
# compile locally until clean (0 errors, 0 warnings):
go run ./cmd/nodes validate examples/pr-upkeep/workflow.yaml

# drive one cycle against a live deployment — BILLABLE, human-guarded:
CONFIRM_BILLABLE=yes examples/pr-upkeep/driver.sh
```

[`driver.sh`](driver.sh) is the external driver (scheduling stays outside
the engine by design): it validates, publishes idempotently by digest, and
POSTs **one** run to `/v1alpha1/runs` — one upkeep cycle-bundle, looping
inside the workflow under `spec.limits` (`maxTransitions: 64`,
`maxVisitsPerNode: 6`, `maxDuration: 168h`) until a terminal edge or bound
ends it. It refuses to run without `CONFIRM_BILLABLE=yes`, exactly like
`examples/codex-smoke-pair/run-smoke.sh`, because every loop iteration
dispatches a real claude-code fix session and a real codex review session.
It deliberately does **not** poll to a terminal state: the run parks on
people (the merge assignment, or the decision) for hours or days;
manual overrides happen in the web `/inbox` or via
`POST /v1alpha1/human-tasks/{id}/decision`. When a run ends, re-invoke the
driver for the next cycle — issue #71 means that is rarer now, since an
empty sweep re-sweeps on its own instead of ending the run.

## Operational notes

- The sweep operation **fetches** the extractor at dispatch time from the
  URL its deployment grants and verifies its sha256 before executing it (see
  "Deployment configuration"); the pinned `python:3.12-slim` image needs
  nothing baked in, since the script is stdlib-only. **Egress: DECLARED
  intent is `network:
  egress-allowlist` (sonarcloud.io + api.github.com + the granted Jira
  host).** Headspace-cli
  0.11.0 supports only disabled/enabled network posture, so the boundary
  honestly rejects the allowlist as `rejected_input` (issue #50). The
  workflow runs with `network: full` until headspace ships allowlist support;
  restore `network: egress-allowlist` and add a runner conformance fixture
  then. No proxy workaround lands in the control plane (spec claim c29).
- `company/human-ops` is the human-inbox bridge README's registration
  example (`kind=human`, same append-only actor revisions as every agent).
- The `uses:` digests are the same referenced identities the sibling
  examples pin (`company/developer`, `company/codex-thor`,
  `company/notify-discord` — the last from `examples/notify-message`,
  `runner://headspace/docker`) — shared placeholders for the same actors,
  not fresh ones.
