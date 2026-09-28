# pr-upkeep declaration sources

`manifest.json` lists the declaration file for every node in `workflow.yaml`
and `sweep-cycle.workflow.yaml`, and each original edge as a predecessor link.
The JSON files preserve the graph node configuration in `action.with.legacy`,
including actor/runner uses, bindings, outcomes, and instructions. The `fix`
action also carries the ordered affinity rules. `variables` shows bindings in
the declaration template grammar; defaults on predecessor references keep
optional paths explicit. Every start and landing node has a deadline.

These are additive migration sources. The current declaration vocabulary has
no trigger for `pr-upkeep.pr`, `pr-upkeep.sweep.due`, or arbitrary graph node
outcomes, and no direct GitHub PR body edit action. The relevant legacy event
and reaction are recorded in `trigger.with`; the live graph files continue to
handle them. `decision` and `end` nodes are represented by `code.run` forms,
but their graph control behavior still needs declaration engine routing and
shadow parity before activation or cutover. A publisher may warn when a Jira,
timer, or agent value is rendered into GitHub or Discord; the owner must approve
each sensitivity widening before dispatch.
