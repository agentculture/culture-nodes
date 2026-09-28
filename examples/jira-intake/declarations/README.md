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
  Nothing emits them yet: the t38c reaction pass covers only
  `human.decision` and `code.result`, and the `jira.*` renames are t37's
  (deviation d1). The declaration worker envelope also reports an agent's
  outcome as `completed`, not `intake_drafted`.
- A `jira.comment` reaction continues the lineage only if the emitter passes
  the stamped `origin`. No Jira emitter does that yet, and the poller drops
  the system's own comments as self-echo.

## Publish warnings (t30)

After linking, a validate reports no sensitivity warnings for this set.
Every variable comes from Jira or the agent (team audience) and renders into
Jira or the agent (also team).
