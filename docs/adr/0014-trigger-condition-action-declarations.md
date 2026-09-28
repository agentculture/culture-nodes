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

A code step's result is reacted to through a `code.result` trigger, the
counterpart of `human.decision` (task t38c, deviation d3). No bridge stamps
either: the control plane binds the decided human task or the runner
operation to the firing's marker and emits the reaction, so a chain through a
human decision or a code step stays one lineage.

### 2. Versions pin per firing, not per run (PRD §9.1, §9.10)

A declaration upgrades live, including in chains already in flight. Every
version stays immutable and content-addressed. Each firing records the digests
of the trigger, condition and action it evaluated.

A node whose reacting declaration no longer matches after an upgrade closes
with an "orphaned by upgrade" record. It is never silently dropped.

### 3. Activation is itself declared (extends PRD §10.4)

A declaration becomes active when an activation declaration fires for it.
Activation declarations are the root of trust, and the chain is one level
deep:

- only a human principal can activate an activation declaration;
- an activation declaration may only activate ordinary declarations, never
  another activation declaration;
- the recorded author is always the authenticated request principal.

This keeps the §10.4 rule: no actor promotes its own proposal. An earlier
draft allowed human-activated rules to activate other activation declarations
by different authors. The PR #329 review showed that an agent could then
propose "activate everything" under a broader human rule, or two agents could
activate each other's rules. The owner accepts the one-level rule as current
and may relax it later.

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

- **A new declaration engine is built on the existing substrate, and the
  cutover is staged:**
  - **Shadow:** the new engine records "would fire" without dispatching,
    beside the graph engine. Shadow stamps no markers, so it derives lineage
    from the graph engine's real runs. Parity is compared hop by hop, each
    shadow firing against the graph step that handled the same event.
  - **Flip:** a global switch goes to "after" only after recorded parity. It
    drains rather than strands: new events go to the declaration engine,
    while graph runs already open finish on the graph engine.
  - **Rollback:** flipping back freezes open declaration nodes. Events that
    arrive for a frozen node are stored and its deadline is paused; on
    flip-forward the node replays them in order.
- **Everything migrates:**
  - all workflows;
  - schedules, notifier posts, repair routing, hand-turns and affinity;
  - workflow generation and devague plan import.

  After that, the graph engine and its node kinds are deleted.
- **New surfaces:**
  - a GitHub webhook and a GitHub messaging actor;
  - `jira.issue.created`;
  - signed origin markers in every bridge (all-backends rule). A marker is a
    MAC over (firing id, artifact kind, nonce). The created artifact's id is
    recorded from the action's result and must match on the incoming event,
    because providers assign that id only after creation;
  - loop limits (N re-entries, default 3, 0 = none; hop limit 20; 30 firings
    per declaration per hour), per-subject concurrency, budgets at node, machine,
    declaration and alias level;
  - variable sensitivity marking;
  - overlap detection;
  - "why did or didn't this fire" explanations.
- **Accepted risk:** overlap detection is approximate, because CEL
  satisfiability is undecidable in general. Evaluation-record retention is not
  designed yet. Both are parked on the frame.

## Addendum (2026-09-28): owner decisions during the build

### Redelivery decisions (t40a)

The post-commit delivery hook still offers watermark duplicates to the
declaration engine so a delivery committed before `Handle` ran can recover.
For each event and declaration, the engine checks the existing evaluation
trail once per event, across declaration versions. A decided outcome prevents
another evaluation row or firing on redelivery. Progress rows alone permit
recovery. Recorded condition, evaluation and dispatch failures are final for
that event: retrying a failed action requires a new signal event. A queued
`deferred` event is final for ordinary redelivery but the subject queue's
explicit drain may resume it. Stored freeze/thaw reactions remain first-time
events until their declarations reach a decision.

The build (issue #328) raised questions the spec left open. The owner decided
them on 2026-09-28. Each is recorded as a devague deviation
(`devague deviate --list`) and implemented as its own task.

### Lineage through human decisions and code runs (d3, task t38c)

No bridge stamps `human.ask` or `code.run`, so the control plane is the
stamper for both. A scheduler pass binds the decided human task or the runner
result to the firing's marker and emits a `human.decision` or `code.result`
reaction that carries it. `code.result` joins the trigger vocabulary for this.
Without it, must-links broke at every human or code step, including
pr-upkeep's `readiness`, `human-merges-pr` and `finish` chain.

### Sensitivity: per-repository audience, per-variable exposure (d4, task t30b)

- GitHub's audience is decided per repository: a public repository ranks
  public, a private one ranks org. Most repositories, this one included, are
  public.
- Exposure is approved per variable. A declaration carries a list of the
  variables (data types) it may expose more widely than their source, and the
  variable's owner approves each entry. This replaces t30's approval per
  declaration version, source version, variable and target system.

How t30b implements it:

- Visibility comes from a namespace-scoped, append-only record
  (`repository_visibility`, `POST /v1alpha1/repository-visibility`) that an
  operator or actor sets. The engine never calls GitHub at firing time. The
  target repository is the rendered action's `input.repository`. A source's
  repository is the `repository` variable of the event that produced it.
- An unrecorded repository fails closed in both directions. As a target it
  ranks public, as decided. As a source it ranks org, not public, because
  ranking unknown data as public would let a private repository nobody
  recorded flow anywhere.
- The list is `exposes`. Each entry is a reference as templates write it:
  `summary`, `1:summary` or `jira-intake:reporter`. An approval is keyed on
  (declaration name, entry, owner). It survives a republish. A new author of
  the producing declaration needs a new approval. Removing the entry
  withdraws the approval, and relisting it needs the owner again. A widening
  reference that is not listed blocks without opening a task.
- t30's per-version tables are retired by migration 0068, not migrated.
  Moving per-version consent onto a per-name key would widen what an owner
  agreed to.

### Event mapping and start nodes (d6, task t38d)

- A delivered event maps onto the declaration engine through its payload:
  `node` names the node it arrives at (default `root`), and `origin` carries
  `{marker, artifact_kind, artifact_id, author, bridge_account}`. The marker
  is verified against the engine's key, and the artifact id must match the
  one the engine recorded, so a copied or forged origin starts a fresh
  lineage.
- An event without a verified marker always arrives at `root`, whatever its
  payload names.
- A declaration may start from any node (`start_from: any`), or from nodes of
  a type. Types are derived by the engine from the action that produced the
  node: the host it ran on (`spark`, `thor`, `orin`) and the actor kind
  (`claude`, `codex`, `qwen`, `human`, `code`). Authors cannot label them, so
  they stay truthful.

How t38d implements it:

- `EventFromSignal` always starts a delivered event at `root`. The payload's
  `node` is only a variable. `Handle` moves a verified reaction to the landing
  node its parent firing opened, as before. An unverified or rejected marker
  never leaves `root`.
- Task t38g removed one exception t38d had kept. A verified parent that
  opened no landing node no longer lets the payload name the node. Such a
  firing's dispatch failed after its run was created, so the run could still
  expose its marker, and anyone holding that marker could have picked any
  start node. The reaction is recorded as `parent opened no landing node`
  and nothing fires.
- Also from t38g: a verified marker proves which firing an event continues,
  not what the event says. So the control plane's own event names are
  reserved: `human.decision`, `code.result`, `agent.result`, `node.expired`
  and every `action.*` name. Every ingress refuses them and the engine's own
  emitter (`kinds.CheckExternalEvent`). `Handle` also records
  `reserved event rejected` and fires nothing for such a name from any other
  emitter.
- `declaration_nodes.host` and `actor_kind` (migration 0069) are written only
  by the engine. `human.ask` records `human` and `code.run` records `code` when
  the node opens. Otherwise the types come from the actor row the firing
  run's newest attempt recorded. `kind` `human` gives `human`,
  `metadata.harness` in the closed set gives the kind (`pi` and `colleague`
  join the list above), and `capabilities.preflight.host.hostname` gives the
  host. An unrecorded fact stays unset and matches no typed `start_from`. A
  set type is never rewritten.
- `start_from` applies only to an engine-opened node, so an event at `root`
  never matches one. A declaration whose trigger matched but whose start did
  not records `start not matched`, naming the types it needed.

### Break-glass stops, it does not start (d5, task t19b)

A human is a principal authenticated by Cloudflare Access. A break-glass
credential bound to a human actor may do two things on these routes, for
when Access is down: deactivate a declaration, and flip the engine switch to
`before`. It cannot publish, link, alias or activate. The owner accepts this
for now and may revisit it.

### Three reaction kinds for the migrated chains (d7, task t31b)

The migrated pr-upkeep and jira-intake declarations chain on three trigger
kinds added to the closed vocabulary:

- `pr-upkeep.pr`: the live sweep's work-item fact, under its existing name,
  so the pr-upkeep entry triggers on what the graph workflow triggers on.
- `agent.result`: the outcome of a completed `agent.work` run, as the agent
  reported it. It is a proposed claim, never evidence. `agent.work` now also
  produces `agent_work`; `github.pr` stays its first product, which the
  marker is minted for.
- `jira.issue.transitioned`: the neutral reaction to a `jira.transition`
  action, replacing the per-status `pr-upkeep.jira.transitioned.*` names
  (renamed under t37).

Both now have emitters. t31c emits `jira.issue.transitioned`. t38e emits
`agent.result` from the control plane's reaction pass. The pass mints a
second marker for the firing, of kind `agent.work`, and binds it to the run
id. It never hands that marker to an actor. The dispatch marker stays minted
for `github.pr`. An `agent.result` verifies only with an `agent.work`
marker, so the pull request's public marker cannot claim an agent outcome.
The outcome is the one the agent reported against the contract outcomes in
the declaration's `graph_config`. It is a proposed claim.
