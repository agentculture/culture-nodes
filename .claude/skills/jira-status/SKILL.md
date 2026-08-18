---
name: jira-status
description: >-
  Read and move Jira issue statuses from the operator lane, using the Jira
  credential already granted to the sweep on thor (the credential never
  leaves that host — the script runs its API calls there over ssh). Verbs:
  `status <ISSUE>` reads the current status and available transitions;
  `move <ISSUE> "<target status>"` executes the transition whose name
  matches. Use when the operator says "flip SCRUM-2", "move the ticket to
  Pending", "what status is SCRUM-N in", or when arming the sweep's status
  watermark for a pickup test (move away, wait for a sweep to record it,
  move back). This is an OPERATOR tool: it acts as the operator's own hand
  on the board, outside the engine — the system's own board moves go through
  the jira bridge actor's allowlisted transition verb, never through this.
type: command
---

# jira-status — operator-lane Jira status reads and moves

```bash
bash .claude/skills/jira-status/scripts/jira-status.sh status SCRUM-2
bash .claude/skills/jira-status/scripts/jira-status.sh move SCRUM-2 "To Do"
bash .claude/skills/jira-status/scripts/jira-status.sh move SCRUM-2 Pending
```

## What it does

- `status <ISSUE>` — prints the issue's current status and the transitions
  available from it (names are what `move` matches against).
- `move <ISSUE> <target>` — finds the available transition whose target
  status name equals `<target>` (case-insensitive) and executes it. Refuses
  with the available names when no transition matches — Jira transitions are
  workflow-dependent, so the error names your real options.

## Custody

The Jira credential pair lives in `~/.culture-nodes/runner-secrets.env` on
thor (granted to the sweep; reuse approved by the operator 2026-08-18). The
script sshes to thor and performs the REST calls there; neither the email
nor the token ever appears on the local machine, in argv, or in output.

Contrast with the system's own write path: workflow runs move boards through
the jira bridge actor (`adapters/jira`), whose `transition_issue` verb is
bridge-allowlisted to one project prefix and one target transition
(decision c19). This skill is the OPERATOR's hand — e.g. arming a pickup
test — and deliberately does not widen or bypass that custody: it is the
same act as the operator clicking the board, scripted.

## Watermark-arming recipe (the reason this exists)

The sweep emits a status fact only when the status it reads differs from
the last one it recorded (`signal_event_watermarks`). To make an issue
already sitting in To Do fire a `transitioned.to-do` fact:

```bash
bash .claude/skills/jira-status/scripts/jira-status.sh move SCRUM-2 Pending
# wait until a sweep records Pending (watch signal_event_watermarks or the
# monitor), then:
bash .claude/skills/jira-status/scripts/jira-status.sh move SCRUM-2 "To Do"
# the next sweep emits transitioned.to-do and subscribed workflows fire.
```

Issues #193 (history-aware sweep) and #194 (failed-trigger re-mint) exist
to make this recipe unnecessary; delete this section when they land.
