# ADR 0014: trigger-condition-action declarations replace the workflow graph

- Status: accepted (design); implementation pending `/spec-to-plan`
- Date: 2026-09-27
- Spec: `docs/specs/2026-09-27-trigger-condition-action.md` (devague frame
  `trigger-condition-action`)
- Issues: #328 (this redesign), #327 (goals and constraints, parked as a
  follow-up)

## Context

`CLAUDE.md` requires that deviations from the PRD
(`docs/initial-design/culture-nodes-prd-spec.md`) be recorded explicitly,
never left to drift. The trigger-condition-action spec departs from the PRD in
four places. This ADR is where those departures are recorded (spec claim c21).

Today automation lives in about 128 workflow graph definitions. Each graph has
nine node kinds, outcome-sourced edges, and a digest pinned per run. Several
separate configuration surfaces sit beside the graphs:

- schedules;
- the nodes-notifier daemon;
- repair routing;
- poller scripts.

Causal links across systems are guessed from run input
(`web/src/domain/ticket-key.ts`) or hand-built per flow
(`examples/jira-question-round-trip/question_correlation.py`).

## Decision

### 1. The declaration replaces the node/edge graph (PRD §3.2, §9.2)

The one authorable unit is a named **declaration**:

- one trigger;
- one CEL condition;
- one action;
- a start node and a landing node.

A **node** is the declared waiting state between an action and the trigger
that captures its reaction. Every node has a deadline or an explicit "none".

Declarations chain causally. "B must appear after A" means B fires only when
its triggering event's lineage contains a firing of A. "B can appear after A"
means B fires either way.

**Aliases** are movable, nestable names over groups of chains. There is one
global declaration graph.

The nine node kinds map onto declarations as follows:

| Node kind | Declaration form |
|---|---|
| approval | A human action plus `human.decision` triggers |
| decision | Mutually exclusive conditions leaving one node |
| parallel | Several declarations leaving one node |
| join | A declaration that must appear after several |
| wait | A timer trigger |
| loop | Re-entry, bounded by the loop limits |
| end | A terminal node |
| agent, code | Actions |

### 2. Versions pin per firing, not per run (PRD §9.1, §9.10)

A declaration upgrades live, including in chains already in flight. Every
version stays immutable and content-addressed. Each firing records the digests
of the trigger, condition and action it evaluated.

A node whose reacting declaration no longer matches after an upgrade closes
with an "orphaned by upgrade" record. It is never silently dropped.

### 3. Activation is itself declared (extends PRD §10.4)

A declaration becomes active when an activation declaration fires for it.
Activation declarations are the root of trust:

- none can activate itself, or another activation declaration by the same
  author;
- the first ones are activated by a human principal;
- the recorded author is always the authenticated request principal.

This keeps the §10.4 rule: no actor promotes its own proposal.

### 4. The run vocabulary is mapped, not dropped (PRD §3.1)

| PRD term | Declaration era |
|---|---|
| run | lineage: the causal chain of firings, tied by verified origin markers |
| token | none; lineage replaces token movement |
| node run | firing: one trigger match, condition evaluation and action dispatch |
| attempt | attempt, unchanged: one dispatch of a firing's action to an actor or runner |
| (none) | node: a waiting state with a deadline |

Every consumer of today's run surfaces keeps a read path until it migrates:

- the web UI endpoints;
- the nodes-notifier lifecycle events;
- `scripts/collect-handover.py`;
- the nodes-operator skill.

## What does not change

- **Stack**: Go control plane, with Postgres as the authority.
- **Substrate**, reused as-is:
  - the `signal_events` log and its emitters;
  - CEL;
  - actor and runner protocols with callbacks, leases and fencing tokens.
- **Ledger authority model**: agent actions yield only `proposed` records.
- **Execution boundary**: code runs only in a sandboxed server-side runner or a
  registered remote runner, never inside the control-plane process.

## Consequences

- **A new declaration engine is built on the existing substrate.** It runs in
  shadow first, recording "would fire" without dispatching, beside the graph
  engine. A global before/after switch flips only after recorded parity.
  Flipping back freezes open nodes rather than dropping them.
- **Everything migrates:**
  - all workflows;
  - schedules, notifier posts, repair routing, hand-turns and affinity;
  - workflow generation and devague plan import.

  After that, the graph engine and its node kinds are deleted.
- **New surfaces:**
  - a GitHub webhook and a GitHub messaging actor;
  - `jira.issue.created`;
  - signed origin markers in every bridge (all-backends rule);
  - loop limits, per-subject concurrency, budgets at node, machine,
    declaration and alias level;
  - variable sensitivity marking;
  - overlap detection;
  - "why did or didn't this fire" explanations.
- **Accepted risk:** overlap detection is approximate, because CEL
  satisfiability is undecidable in general. Evaluation-record retention is not
  designed yet. Both are parked on the frame.
