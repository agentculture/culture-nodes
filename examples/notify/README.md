# Notification declarations

Each file is one Discord notification case. Publish and activate the declarations in `manifest.json` to enable them; deactivate an individual declaration to mute that case. The `notify-*` envelope runs are skipped by the legacy notifier. The registered `company/notify-discord` actor must advertise cn1 stamping, and the digest placeholder must be replaced with its registered revision. Listed `exposes` entries require the publishing owner’s approval before variable values can reach Discord.

The routine five-minute sweep has no notification. Its failed result does.

## Declaration set

| Name | Trigger | Start node | Condition | Message |
|---|---|---|---|---|
| `notify-pr-work-item` | `pr-upkeep.pr` | `root` | `true` | `PR upkeep work item \| Work item {work_item}` |
| `notify-jira-intake` | `jira.issue.created` | `root` | `event.source == "jira" && event.status == "To Do"` | `Jira intake requested \| Issue {id}` |
| `notify-action-failed-pr-upkeep-analyse-orphan` | `action.failed` | `pr-upkeep-analyse-done` | `true` | `Action failed: analyse-orphan \| Class {class}` |
| `notify-action-failed-pr-upkeep-analyse` | `action.failed` | `pr-upkeep-analyse-done` | `true` | `Action failed: analyse \| Class {class}` |
| `notify-action-failed-pr-upkeep-blocked-analyse` | `action.failed` | `pr-upkeep-blocked-analyse-done` | `true` | `Action failed: blocked-analyse \| Class {class}` |
| `notify-human-needed-pr-upkeep-blocked-analyse` | `human.requested` | `pr-upkeep-blocked-analyse-done` | `true` | `Decision needed: pr-upkeep-blocked-analyse \| Task {human_task_id}` |
| `notify-human-decided-pr-upkeep-blocked-analyse` | `human.decision` | `pr-upkeep-blocked-analyse-done` | `true` | `Decision made: pr-upkeep-blocked-analyse \| Outcome {outcome}` |
| `notify-agent-blocked-pr-upkeep-blocked-analyse` | `agent.result` | `pr-upkeep-analyse-done` | `event.outcome == "blocked"` | `Agent blocked: pr-upkeep-blocked-analyse \| Outcome {outcome}` |
| `notify-action-failed-pr-upkeep-blocked-fix` | `action.failed` | `pr-upkeep-blocked-fix-done` | `true` | `Action failed: blocked-fix \| Class {class}` |
| `notify-human-needed-pr-upkeep-blocked-fix` | `human.requested` | `pr-upkeep-blocked-fix-done` | `true` | `Decision needed: pr-upkeep-blocked-fix \| Task {human_task_id}` |
| `notify-human-decided-pr-upkeep-blocked-fix` | `human.decision` | `pr-upkeep-blocked-fix-done` | `true` | `Decision made: pr-upkeep-blocked-fix \| Outcome {outcome}` |
| `notify-agent-blocked-pr-upkeep-blocked-fix` | `agent.result` | `pr-upkeep-fix-done` | `event.outcome == "blocked"` | `Agent blocked: pr-upkeep-blocked-fix \| Outcome {outcome}` |
| `notify-action-failed-pr-upkeep-blocked-stamp-pr` | `action.failed` | `pr-upkeep-blocked-stamp-pr-done` | `true` | `Action failed: blocked-stamp-pr \| Class {class}` |
| `notify-human-needed-pr-upkeep-blocked-stamp-pr` | `human.requested` | `pr-upkeep-blocked-stamp-pr-done` | `true` | `Decision needed: pr-upkeep-blocked-stamp-pr \| Task {human_task_id}` |
| `notify-human-decided-pr-upkeep-blocked-stamp-pr` | `human.decision` | `pr-upkeep-blocked-stamp-pr-done` | `true` | `Decision made: pr-upkeep-blocked-stamp-pr \| Outcome {outcome}` |
| `notify-agent-blocked-pr-upkeep-blocked-stamp-pr` | `agent.result` | `pr-upkeep-stamp-pr-done` | `event.outcome == "blocked"` | `Agent blocked: pr-upkeep-blocked-stamp-pr \| Outcome {outcome}` |
| `notify-action-failed-pr-upkeep-close-blocked-analyse` | `action.failed` | `pr-upkeep-close-blocked-analyse-done` | `true` | `Action failed: close-blocked-analyse \| Class {class}` |
| `notify-action-failed-pr-upkeep-close-blocked-fix` | `action.failed` | `pr-upkeep-close-blocked-fix-done` | `true` | `Action failed: close-blocked-fix \| Class {class}` |
| `notify-action-failed-pr-upkeep-close-blocked-stamp-pr` | `action.failed` | `pr-upkeep-close-blocked-stamp-pr-done` | `true` | `Action failed: close-blocked-stamp-pr \| Class {class}` |
| `notify-action-failed-pr-upkeep-finish-expired` | `action.failed` | `pr-upkeep-finish-done` | `true` | `Action failed: finish-expired \| Class {class}` |
| `notify-action-failed-pr-upkeep-finish-no-change` | `action.failed` | `pr-upkeep-finish-done` | `true` | `Action failed: finish-no-change \| Class {class}` |
| `notify-action-failed-pr-upkeep-finish-no-fix` | `action.failed` | `pr-upkeep-finish-done` | `true` | `Action failed: finish-no-fix \| Class {class}` |
| `notify-action-failed-pr-upkeep-finish` | `action.failed` | `pr-upkeep-finish-done` | `true` | `Action failed: finish \| Class {class}` |
| `notify-action-failed-pr-upkeep-fix-orphan` | `action.failed` | `pr-upkeep-fix-done` | `true` | `Action failed: fix-orphan \| Class {class}` |
| `notify-action-failed-pr-upkeep-fix` | `action.failed` | `pr-upkeep-fix-done` | `true` | `Action failed: fix \| Class {class}` |
| `notify-action-failed-pr-upkeep-human-merges-pr` | `action.failed` | `pr-upkeep-human-merges-pr-done` | `true` | `Action failed: human-merges-pr \| Class {class}` |
| `notify-human-needed-pr-upkeep-human-merges-pr` | `human.requested` | `pr-upkeep-human-merges-pr-done` | `true` | `Decision needed: pr-upkeep-human-merges-pr \| Task {human_task_id}` |
| `notify-human-decided-pr-upkeep-human-merges-pr` | `human.decision` | `pr-upkeep-human-merges-pr-done` | `true` | `Decision made: pr-upkeep-human-merges-pr \| Outcome {outcome}` |
| `notify-action-failed-pr-upkeep-intake-orphan` | `action.failed` | `pr-upkeep-intake-orphan-done` | `true` | `Action failed: intake-orphan \| Class {class}` |
| `notify-action-failed-pr-upkeep-readiness-orphan` | `action.failed` | `pr-upkeep-readiness-done` | `true` | `Action failed: readiness-orphan \| Class {class}` |
| `notify-action-failed-pr-upkeep-readiness` | `action.failed` | `pr-upkeep-readiness-done` | `true` | `Action failed: readiness \| Class {class}` |
| `notify-action-failed-pr-upkeep-route` | `action.failed` | `pr-upkeep-route-done` | `true` | `Action failed: route \| Class {class}` |
| `notify-action-failed-pr-upkeep-stage-dispatch` | `action.failed` | `pr-upkeep-stage-dispatch-done` | `true` | `Action failed: stage-dispatch \| Class {class}` |
| `notify-action-failed-pr-upkeep-stage-pr-open` | `action.failed` | `pr-upkeep-stage-pr-open-done` | `true` | `Action failed: stage-pr-open \| Class {class}` |
| `notify-action-failed-pr-upkeep-stamp-pr` | `action.failed` | `pr-upkeep-stamp-pr-done` | `true` | `Action failed: stamp-pr \| Class {class}` |
| `notify-action-failed-pr-upkeep-sweep-failed` | `action.failed` | `pr-upkeep-sweep-failed-done` | `true` | `Action failed: sweep-failed \| Class {class}` |
| `notify-action-failed-pr-upkeep-sweep` | `action.failed` | `pr-upkeep-sweep-done` | `true` | `Action failed: sweep \| Class {class}` |
| `notify-action-failed-pr-upkeep-swept` | `action.failed` | `pr-upkeep-swept-done` | `true` | `Action failed: swept \| Class {class}` |
| `notify-action-failed-jira-intake-blocked-intake` | `action.failed` | `jira-intake-blocked-intake-done` | `true` | `Action failed: blocked-intake \| Class {class}` |
| `notify-human-needed-jira-intake-blocked-intake` | `human.requested` | `jira-intake-blocked-intake-done` | `true` | `Decision needed: jira-intake-blocked-intake \| Task {human_task_id}` |
| `notify-human-decided-jira-intake-blocked-intake` | `human.decision` | `jira-intake-blocked-intake-done` | `true` | `Decision made: jira-intake-blocked-intake \| Outcome {outcome}` |
| `notify-agent-blocked-jira-intake-blocked-intake` | `agent.result` | `jira-intake-intake-done` | `event.outcome == "blocked"` | `Agent blocked: jira-intake-blocked-intake \| Outcome {outcome}` |
| `notify-action-failed-jira-intake-close-blocked-intake` | `action.failed` | `jira-intake-close-blocked-intake-done` | `true` | `Action failed: close-blocked-intake \| Class {class}` |
| `notify-action-failed-jira-intake-intake` | `action.failed` | `jira-intake-intake-done` | `true` | `Action failed: intake \| Class {class}` |
| `notify-action-failed-jira-intake-picked-up-gh` | `action.failed` | `jira-intake-picked-up-done` | `true` | `Action failed: picked-up-gh \| Class {class}` |
| `notify-action-failed-jira-intake-picked-up` | `action.failed` | `jira-intake-picked-up-done` | `true` | `Action failed: picked-up \| Class {class}` |
| `notify-action-failed-jira-intake-post-comment` | `action.failed` | `jira-intake-post-comment-done` | `true` | `Action failed: post-comment \| Class {class}` |
| `notify-action-failed-jira-intake-stage-intake` | `action.failed` | `jira-intake-stage-intake-done` | `true` | `Action failed: stage-intake \| Class {class}` |
| `notify-action-failed-jira-intake-transition` | `action.failed` | `jira-intake-transition-done` | `true` | `Action failed: transition \| Class {class}` |
| `notify-sweep-failed` | `code.result` | `pr-upkeep-sweep-done` | `event.outcome == "failed"` | `PR upkeep sweep failed \| Outcome {outcome}` |
| `notify-merge-deadline` | `node.expired` | `pr-upkeep-human-merges-pr-done` | `true` | `PR merge deadline passed \| Node {node_name}` |
