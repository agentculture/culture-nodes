# jira-intake declaration sources

`manifest.json` maps every graph node and edge to its declaration source and
predecessor link. Each source names its start and landing node with explicit
deadlines, carries the graph node configuration in `action.with.legacy`, and
uses declaration template syntax for input bindings. The entry source carries
the two in-flight firings per Jira issue cap.

The live graph continues to consume `pr-upkeep.jira.transitioned.to-do`. The
entry declaration uses the new `jira.issue.created` vocabulary and records
the legacy event in `trigger.with`; these are different events. Graph outcome
routing and the final `end` state still need shadow parity before activation
or cutover. Rendering Jira or agent variables into GitHub or Discord later
requires the owner's sensitivity widening approval when publish warns.
