# Notification declarations

Each file is one Discord notification case. Publish and activate the declarations in `manifest.json` to enable them; deactivate an individual declaration to mute that case. The `notify-*` envelope runs are skipped by the legacy notifier. The registered `company/notify-discord` actor must advertise cn1 stamping, and the digest placeholder must be replaced with its registered revision. Listed `exposes` entries require the publishing owner’s approval before variable values can reach Discord.

The routine five-minute sweep has no notification. Its failed result does.

## Declaration set

Every message leads with WHAT happened and LINKS to where (task t48, owner
decision d21). The title says what happened; the first line of the
description carries the link, in Discord markdown `[text](url)`:

| Trigger | First-line link | Built from |
|---|---|---|
| `pr-upkeep.pr`, and every PR-lineage `agent.result` / `human.decision` | `https://github.com/{repository}/pull/{number}` | the fact, or `{pr-upkeep-route:*}` in lineage |
| `jira.issue.created`, and every jira-intake `agent.result` / `human.decision` | `https://agentculture.atlassian.net/browse/{id}` | the fact, or `{jira-intake-intake:*}` in lineage |
| `action.failed`, `code.result` (sweep failed), `node.expired` (merge deadline) | `https://nodes.culture.dev/runs/{firing_id}` | the event's own `firing_id` |
| `human.requested`, and every `agent.result` (agent blocked) | `https://nodes.culture.dev/inbox` | fixed; a human-needed message also links its run |

Three examples, as written in the declarations:

- `notify-pr-work-item` — title `PR #{number}: {0:title:}`; description
  `[{repository} PR #{number}](https://github.com/{repository}/pull/{number}) has {0:finding_count:new} finding(s); first: {0:finding_title:}`.
  `title`, `finding_title` and `finding_count` are flat display fields the
  sweep puts on the fact (`pr_upkeep_emit.upkeep_pr_fact`), because a
  template renders only flat values.
- `notify-agent-blocked-pr-upkeep-blocked-fix` — title
  `PR #{pr-upkeep-route:number} fix blocked: {pr-upkeep-route:title:}`;
  description links the PR and `[Decide in the inbox](https://nodes.culture.dev/inbox)`.
- `notify-action-failed-pr-upkeep-fix` — title `pr-upkeep: fix failed`;
  description `[Open the failed run](https://nodes.culture.dev/runs/{firing_id}) · status: {status} · class: {class}`.
  The status is rendered beside the class because `class` is often empty in
  production (a hook rejection records none); the status is `failed` or
  `contract_rejected`.

A lineage reference resolves only for a reaction whose verified origin
continues that lineage. The engine emits `action.*`, `node.expired` and
`human.requested` without an origin, so those messages can use only their own
event's variables (`firing_id`, `class`, `status`, `node_id`, `node_name`,
`human_task_id`). `examples/notify/declarations/render_test.go` renders every
declaration against its trigger's real event shape and pins that `exposes` is
exactly the set of references each message renders. The cn1 marker the bridge
stamps rides in the embed footer, never the first line
(`adapters/notify/README.md`).

| Name | Trigger | Start node | Condition | Title | Exposes |
|---|---|---|---|---|---|
| `notify-pr-work-item` | `pr-upkeep.pr` | `root` | `true` | `PR #{number}: {0:title:}` | `finding_count`, `finding_title`, `number`, `repository`, `title` |
| `notify-jira-intake` | `jira.issue.created` | `root` | `event.source == "jira" && event.status == "To Do"` | `Jira {id}: {0:summary:}` | `id`, `summary` |
| `notify-action-failed-pr-upkeep-analyse` | `action.failed` | `pr-upkeep-analyse-done` | `true` | `pr-upkeep: analyse failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-pr-upkeep-blocked-analyse` | `action.failed` | `pr-upkeep-blocked-analyse-done` | `true` | `pr-upkeep: blocked-analyse failed` | `class`, `firing_id`, `status` |
| `notify-human-needed-pr-upkeep-blocked-analyse` | `human.requested` | `pr-upkeep-blocked-analyse-done` | `true` | `Decision needed: pr-upkeep analyse is blocked` | `firing_id` |
| `notify-human-decided-pr-upkeep-blocked-analyse` | `human.decision` | `pr-upkeep-blocked-analyse-done` | `true` | `PR #{pr-upkeep-route:number} analyse decision: {outcome}` | `outcome`, `pr-upkeep-route:number`, `pr-upkeep-route:repository` |
| `notify-agent-blocked-pr-upkeep-blocked-analyse` | `agent.result` | `pr-upkeep-analyse-done` | `event.outcome == "blocked"` | `PR #{pr-upkeep-route:number} analyse blocked: {pr-upkeep-route:title:}` | `pr-upkeep-route:number`, `pr-upkeep-route:repository`, `pr-upkeep-route:title` |
| `notify-action-failed-pr-upkeep-blocked-fix` | `action.failed` | `pr-upkeep-blocked-fix-done` | `true` | `pr-upkeep: blocked-fix failed` | `class`, `firing_id`, `status` |
| `notify-human-needed-pr-upkeep-blocked-fix` | `human.requested` | `pr-upkeep-blocked-fix-done` | `true` | `Decision needed: pr-upkeep fix is blocked` | `firing_id` |
| `notify-human-decided-pr-upkeep-blocked-fix` | `human.decision` | `pr-upkeep-blocked-fix-done` | `true` | `PR #{pr-upkeep-route:number} fix decision: {outcome}` | `outcome`, `pr-upkeep-route:number`, `pr-upkeep-route:repository` |
| `notify-agent-blocked-pr-upkeep-blocked-fix` | `agent.result` | `pr-upkeep-fix-done` | `event.outcome == "blocked"` | `PR #{pr-upkeep-route:number} fix blocked: {pr-upkeep-route:title:}` | `pr-upkeep-route:number`, `pr-upkeep-route:repository`, `pr-upkeep-route:title` |
| `notify-action-failed-pr-upkeep-blocked-stamp-pr` | `action.failed` | `pr-upkeep-blocked-stamp-pr-done` | `true` | `pr-upkeep: blocked-stamp-pr failed` | `class`, `firing_id`, `status` |
| `notify-human-needed-pr-upkeep-blocked-stamp-pr` | `human.requested` | `pr-upkeep-blocked-stamp-pr-done` | `true` | `Decision needed: pr-upkeep stamp-pr is blocked` | `firing_id` |
| `notify-human-decided-pr-upkeep-blocked-stamp-pr` | `human.decision` | `pr-upkeep-blocked-stamp-pr-done` | `true` | `PR #{pr-upkeep-route:number} stamp-pr decision: {outcome}` | `outcome`, `pr-upkeep-route:number`, `pr-upkeep-route:repository` |
| `notify-agent-blocked-pr-upkeep-blocked-stamp-pr` | `agent.result` | `pr-upkeep-stamp-pr-done` | `event.outcome == "blocked"` | `PR #{pr-upkeep-route:number} stamp-pr blocked: {pr-upkeep-route:title:}` | `pr-upkeep-route:number`, `pr-upkeep-route:repository`, `pr-upkeep-route:title` |
| `notify-action-failed-pr-upkeep-finish` | `action.failed` | `pr-upkeep-finish-done` | `true` | `pr-upkeep: finish failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-pr-upkeep-fix` | `action.failed` | `pr-upkeep-fix-done` | `true` | `pr-upkeep: fix failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-pr-upkeep-human-merges-pr` | `action.failed` | `pr-upkeep-human-merges-pr-done` | `true` | `pr-upkeep: human-merges-pr failed` | `class`, `firing_id`, `status` |
| `notify-human-needed-pr-upkeep-human-merges-pr` | `human.requested` | `pr-upkeep-human-merges-pr-done` | `true` | `Decision needed: a pull request is ready to merge` | `firing_id` |
| `notify-human-decided-pr-upkeep-human-merges-pr` | `human.decision` | `pr-upkeep-human-merges-pr-done` | `true` | `PR #{pr-upkeep-route:number} merge decision: {outcome}` | `outcome`, `pr-upkeep-route:number`, `pr-upkeep-route:repository` |
| `notify-action-failed-pr-upkeep-intake-orphan` | `action.failed` | `pr-upkeep-intake-orphan-done` | `true` | `pr-upkeep: intake-orphan failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-pr-upkeep-readiness` | `action.failed` | `pr-upkeep-readiness-done` | `true` | `pr-upkeep: readiness failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-pr-upkeep-route` | `action.failed` | `pr-upkeep-route-done` | `true` | `pr-upkeep: route failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-pr-upkeep-stage-dispatch` | `action.failed` | `pr-upkeep-stage-dispatch-done` | `true` | `pr-upkeep: stage-dispatch failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-pr-upkeep-stage-pr-open` | `action.failed` | `pr-upkeep-stage-pr-open-done` | `true` | `pr-upkeep: stage-pr-open failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-pr-upkeep-stamp-pr` | `action.failed` | `pr-upkeep-stamp-pr-done` | `true` | `pr-upkeep: stamp-pr failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-pr-upkeep-sweep-failed` | `action.failed` | `pr-upkeep-sweep-failed-done` | `true` | `pr-upkeep: sweep-failed failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-pr-upkeep-sweep` | `action.failed` | `pr-upkeep-sweep-done` | `true` | `pr-upkeep: sweep failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-pr-upkeep-swept` | `action.failed` | `pr-upkeep-swept-done` | `true` | `pr-upkeep: swept failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-jira-intake-blocked-intake` | `action.failed` | `jira-intake-blocked-intake-done` | `true` | `jira-intake: blocked-intake failed` | `class`, `firing_id`, `status` |
| `notify-human-needed-jira-intake-blocked-intake` | `human.requested` | `jira-intake-blocked-intake-done` | `true` | `Decision needed: jira-intake intake is blocked` | `firing_id` |
| `notify-human-decided-jira-intake-blocked-intake` | `human.decision` | `jira-intake-blocked-intake-done` | `true` | `{jira-intake-intake:id} intake decision: {outcome}` | `jira-intake-intake:id`, `jira-intake-intake:summary`, `outcome` |
| `notify-agent-blocked-jira-intake-blocked-intake` | `agent.result` | `jira-intake-intake-done` | `event.outcome == "blocked"` | `{jira-intake-intake:id} intake blocked: {jira-intake-intake:summary:}` | `jira-intake-intake:id`, `jira-intake-intake:summary` |
| `notify-action-failed-jira-intake-intake` | `action.failed` | `jira-intake-intake-done` | `true` | `jira-intake: intake failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-jira-intake-picked-up` | `action.failed` | `jira-intake-picked-up-done` | `true` | `jira-intake: picked-up failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-jira-intake-post-comment` | `action.failed` | `jira-intake-post-comment-done` | `true` | `jira-intake: post-comment failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-jira-intake-stage-intake` | `action.failed` | `jira-intake-stage-intake-done` | `true` | `jira-intake: stage-intake failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-jira-intake-transition` | `action.failed` | `jira-intake-transition-done` | `true` | `jira-intake: transition failed` | `class`, `firing_id`, `status` |
| `notify-sweep-failed` | `code.result` | `pr-upkeep-sweep-done` | `event.outcome == "failed"` | `PR upkeep sweep failed` | `firing_id` |
| `notify-merge-deadline` | `node.expired` | `pr-upkeep-human-merges-pr-done` | `true` | `PR merge deadline passed with no decision` | `firing_id` |
| `notify-action-failed-pr-upkeep-acknowledge-stamp-pr` | `action.failed` | `pr-upkeep-acknowledge-stamp-pr-done` | `true` | `pr-upkeep: acknowledge-stamp-pr failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-pr-upkeep-abandon-stamp-pr` | `action.failed` | `pr-upkeep-abandon-stamp-pr-done` | `true` | `pr-upkeep: abandon-stamp-pr failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-pr-upkeep-acknowledge-analyse` | `action.failed` | `pr-upkeep-acknowledge-analyse-done` | `true` | `pr-upkeep: acknowledge-analyse failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-pr-upkeep-abandon-analyse` | `action.failed` | `pr-upkeep-abandon-analyse-done` | `true` | `pr-upkeep: abandon-analyse failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-pr-upkeep-acknowledge-fix` | `action.failed` | `pr-upkeep-acknowledge-fix-done` | `true` | `pr-upkeep: acknowledge-fix failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-pr-upkeep-abandon-fix` | `action.failed` | `pr-upkeep-abandon-fix-done` | `true` | `pr-upkeep: abandon-fix failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-jira-intake-acknowledge-intake` | `action.failed` | `jira-intake-acknowledge-intake-done` | `true` | `jira-intake: acknowledge-intake failed` | `class`, `firing_id`, `status` |
| `notify-action-failed-jira-intake-abandon-intake` | `action.failed` | `jira-intake-abandon-intake-done` | `true` | `jira-intake: abandon-intake failed` | `class`, `firing_id`, `status` |

There is exactly ONE notification per trigger kind and start node (the
manifest test refuses a second). Every active declaration an event matches
fires on it, and the engine emits `action.*` with no parent firing, so two
`action-failed` declarations on one landing node posted the same failure
twice, one titled with the wrong step. Task t48 folded the seven that shared a
node into the one named after it: `pr-upkeep-finish-done` had four (finish,
finish-expired, finish-no-fix, finish-no-change), and `pr-upkeep-analyse-done`,
`pr-upkeep-fix-done`, `pr-upkeep-readiness-done` and
`jira-intake-picked-up-done` two each (the orphan and `-gh` variants).

Only `action.failed` notifies today. `action.rejected`, `action.timed_out`
and `action.capacity_exhausted` (the engine's other three `action.*` kinds,
`internal/declengine/nodes.go` `actionTriggerFor`) have no notification, and a
declaration takes one trigger kind, so covering them is three more
declarations per landing node; that is recorded as a follow-up, not done here.

The four `human-decided-*-blocked-*` cases use condition `true`, so retry,
abandon, and acknowledged decisions all emit their outcome notification.
