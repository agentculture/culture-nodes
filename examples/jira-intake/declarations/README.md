# jira-intake declaration sources

These files migrate `workflow.yaml` to trigger-condition-action declarations
(issue #328, tasks t31 and t31b). They are additive: the live graph workflow
and its legacy trigger event stay as they are.

## How the declarations chain

Each declaration starts on its predecessor's landing node and triggers on
the reaction to that predecessor's action:

`intake` (`agent.work`) → `agent.result` → `post-comment` (`jira.comment`) →
`jira.comment` → `transition` (`jira.transition`) →
`jira.issue.transitioned` → `stage-intake` (`jira.comment`) →
`jira.comment` → `picked-up`.

The graph sends a `gh:` issue from `transition` straight to `picked-up`.
Here that is `picked-up-gh`, which starts on the same node as `stage-intake`
with the opposite condition. The graph's `input.id.startsWith("gh:")` reads
`lineage["jira-intake-intake"].id`. `picked-up` is a graph `end` node; it
becomes a `code.run` that records the issue, so shadow parity has a firing
to compare.

`manifest.json` lists each link and why it is `must` or `can`. Every link
here is `must`: each declaration has one predecessor, and later declarations
read the intake issue through `{jira-intake-intake:...}`.

## Entry

`jira-intake-intake` triggers on `jira.issue.created` at `root`. The webhook
and the JQL poller both emit that event. The condition keeps the graph's Jira
source and the `To Do` status. The per-issue cap of 2 is
`trigger.max_concurrent_subject`. The legacy trigger
`pr-upkeep.jira.transitioned.to-do` is kept only as a note in
`manifest.json`.

Parity difference: the legacy event also fires when an existing issue moves
back to To Do. `jira.issue.created` fires only when an issue is created.

## What does not fire yet

- `agent.result` and `jira.issue.transitioned` were registered by t31b.
  t31c emits `jira.issue.transitioned`, and the `jira.*` renames are t37's
  (deviation d1). Since t38e the control plane's reaction pass emits
  `agent.result`, and the worker envelope carries the agent's own outcome
  (`intake_drafted`, from `graph_config.contract.outcomes`) instead of
  `completed`.
- Since t38f the Jira poller and webhook pass the stamped `origin`, so
  `post-comment` → `transition` (on `jira.comment`) and `transition` →
  `stage-intake` / `picked-up-gh` (on `jira.issue.transitioned`, raised by
  the marker comment the transition posts) continue the lineage. That needs
  the emitter's configured bot account (`jira_bot_account_id`, or the
  webhook's). Without one no origin is attached and each hop starts a fresh
  lineage. `stage-intake` → `picked-up` rides the same `jira.comment`
  reaction but is not covered by a conformance test yet.

## Publish warnings (t30, t30b)

This chain has no sensitivity warnings: its variables come from Jira or the
agent (team audience) and render into Jira, the agent, a human approver, or a
narrower `code.run`. The blocked human route needs no `exposes` entry.

## Blocked intake

`jira-intake-blocked-intake` listens for an `agent.result` with outcome
`blocked` from the intake agent. It asks `group/platform-maintainers` to
review the agent's reason and issue identity. `retry-intake` dispatches the same intake actor and issue inputs;
`abandon-intake` records the decision and issue identity without transitioning
Jira; `acknowledge-intake` records closure. The task offers retry, abandon,
and acknowledged, plus implied expiry. The blocked and retry declarations
each permit one reentry, bounding the lineage to two retry dispatches. A
future operator workflow could comment on Jira after an abandonment.
