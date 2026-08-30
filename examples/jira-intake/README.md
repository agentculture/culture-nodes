# Jira intake

This fixture is the subscriber for the sweep's **bare Jira transition
facts** — the joint that was missing while every piece either side of it was
deployed and healthy. `examples/pr-upkeep/sweep.py` had been appending
`pr-upkeep.jira.transitioned.to-do` to a topic with no listener; this graph
is the listener. It is what makes "drag a ticket to To Do" mean something,
and [`docs/drive-from-jira.md`](../../docs/drive-from-jira.md) is the same
loop written for the person doing the dragging.

`workflow.yaml`'s header is the normative document for the decisions below —
rollout, kill switch, session ceiling, and the deployment configuration this
graph needs from outside the file. This page explains the shape.

## The trigger, and what it deliberately declines

```yaml
- onEvent: pr-upkeep.jira.transitioned.to-do
  when: event.payload.source == "jira"
```

Trigger matching is exact (`internal/engine/trigger.go`), so one `onEvent`
entry subscribes to exactly one status slug. The guard is the one permanent
one: `pr-upkeep.pr` facts carry `source == "github_pr"` and must never mint
an intake run.

Two adjacent graphs consume the *complement* of this one, and the three do
not overlap:

- [`examples/jira-comment-consumer`](../jira-comment-consumer) takes the bare
  `pr-upkeep.jira.comment` facts — a human's comment that answers no marked
  question;
- [`examples/jira-question-round-trip`](../jira-question-round-trip) takes
  the ones that *do* carry an `originating_question_id`. Its trigger guard
  (`has(event.payload.question_id)`) exists precisely to decline the bare
  transition events this graph consumes — that was a review finding on
  PR #180, and it is why this is a separate document with its own workflow
  key rather than a branch inside that one.

Because the sweep writes a status watermark per issue, an already-idle To Do
ticket does not fire when this workflow is published; only a fresh transition
does. A ticket moved *back* to To Do fires pickup again, by design — that is
a human's re-triage signal.

## The contract it consumes

The input contract is the `jira_work_items` fact shape
(`examples/pr-upkeep/pr_upkeep_jira.py`), not a shape invented here:

```json
{"source": "jira", "id": "SCRUM-6", "project": "SCRUM", "severity": "Medium",
 "kind": "Task", "title": "…", "description": "…",
 "description_truncated": false, "status": "To Do",
 "details_url": "https://<site>/browse/SCRUM-6"}
```

`description` carries the ticket body (first 4000 characters, with
`description_truncated` saying whether that cut anything) and is bound into
the intake node's input. That is the whole of "the ticket description reaches
the agent": before it, an agent drafting the acknowledgement had the title
and nothing else, and the acknowledgement read like it.

## The three nodes

| Node | Actor | What it does |
| --- | --- | --- |
| `intake` | `actor://company/intake` | Reads the issue fields and drafts the acknowledging comment |
| `post-comment` | `actor://company/jira-comment` | Posts that text, verbatim, on the ticket |
| `transition` | `actor://company/jira-comment` | Moves the ticket to `In Progress` |

Then one named ending, `picked-up`. A run whose output does not name why it
stopped is the illegible failure this repo's example graphs are against, and
the longest path is four hops against a `maxTransitions: 8` ceiling that
leaves headroom without hiding a loop — there is none; every node is visited
at most once by construction.

Three constraints in those nodes were **measured live**, not designed, and
each is cited at its node in `workflow.yaml`:

- **The final message *is* the comment.** The claude bridge does not parse an
  outcome out of model prose: it reports the outcome named by
  `input.success_outcome` and an output whose comment-bearing field is
  `summary` — the session's final message. Run `01M0B5QWGSG2F86P0NC76AJT8V`
  was an honest draft wrapped in explanatory prose, and it was
  `contract_rejected`. The instruction therefore tells the session that its
  final message is posted verbatim.
- **The outcome name is the bridge's.** `transition_issue` reports
  `issue_transitioned`. Run `01M0B6WGCQNSQD4TBY5J5HBG61` moved the board
  successfully and had its completion rejected over exactly this name.
- **A node that proposes nothing rejects everything.** Each agent node
  declares `ledger: propose: [claim]`. A real bridge completion proposes a
  claim (PRD §10.4), and a node declaring no propose types rejects every
  honest completion as `contract_rejected`.

## Self-echo: this graph adds no marker

The narrow Jira bridge stamps `[culture-nodes:jira-actor]` on every comment
it posts, and `sweep.py`'s `jira_comment_is_self_echo` is what reads it (with
the configured `jira_bot_account_id` as the authoritative signal and the
marker as the fallback). That is what stops the acknowledgement this graph
posts from being read back as a human comment and minting a consumer run.

**This graph must not add its own marker.** The bridge owns that stamp; a
second one authored here would be a second, drifting claim about which
comments are the system's.

## The board move is the bridge's to refuse

The `transition` node binds `target: In Progress` as a literal, and the
**bridge** — not this file — is the enforcement point: `JIRA_TRANSITION_TARGET`
is its allowlist, and any other target, or an issue outside its configured
project prefix, is refused at the bridge. A mismatch between the literal here
and the bridge's allowlist is a policy refusal at dispatch, visible in the
run. Binding a different value here could not widen custody, which is the
point of putting the allowlist there.

Since the human-task fan-out landed, that allowlist is a comma-separated list
(`Done,Pending` style) because a pending decision moves the ticket to
`Pending` from the control plane. A single value still works unchanged.

## Deployment configuration

The workflow header lists this in full; in short, the graph needs two
registered actors — `actor://company/intake` (the claude push bridge, whose
host being up is a required condition) and `actor://company/jira-comment` (the
narrow Jira bridge) — and the sweep on the other side of the joint needs its
Jira read pair and its `PR_UPKEEP_REPOSITORIES` entry naming `jira_site` and
`jira_project`. `deploy/prod/README.md` is where those grants and their files
are recorded.

## Honest limits

- **Two pickups at a time** (`maxConcurrentSubjectRuns: 2`). Each intake run
  dispatches one bridge session against the operator's shared subscription
  window, so this ceiling is an economics decision, and raising it is a
  publish — i.e. a decision, not a tweak.
- **A status change made by the account culture-nodes posts under is
  filtered** as self-echo before any fact exists. With that account set to
  the operator's own, an operator dragging a ticket to To Do by hand does not
  fire intake; creating the ticket, or a move by anyone else, does.
- **A technical failure at any node fails the run visibly** — including a
  completion whose outcome or output shape the contract rejects, which is how
  both live intake failures above surfaced. There is no silent stall.
