# pr-upkeep declaration sources

These files migrate `workflow.yaml` and `sweep-cycle.workflow.yaml` to
trigger-condition-action declarations (issue #328, tasks t31 and t31b). They
are additive: the live graph workflows, the sweep and every event name are
unchanged, and nothing here is active until an owner activates it.

## How the declarations chain

Each declaration starts on its predecessor's landing node. It triggers on
the reaction to that predecessor's action:

| Predecessor action | Reaction trigger | Condition reads |
|---|---|---|
| `code.run` | `code.result` | `event.outcome`: `passed` or `failed` |
| `human.ask` | `human.decision` | `event.outcome`: `approved`, `rejected` or `expired` |
| `agent.work` | `agent.result` | `event.outcome`: the agent's reported outcome |
| `jira.create` | `jira.issue.created` | the created issue |
| `jira.comment` | `jira.comment` | the posted comment |

A graph node reached from two predecessors becomes two declarations. Each
has its own start node and trigger, and both land on the same node. For
example, `analyse` runs after `route` (a keyed item) and
`analyse-orphan` runs after `stamp-pr` (an orphan PR). A graph `decision`
or `end` node becomes a `code.run` on `runner://headspace/docker` that
records its input, so it has a firing to compare in shadow parity.

Graph edge conditions become CEL conditions on the successor. The graph's
`input.work_item.startsWith("gh:")` reads the run input. Here it reads
`lineage["pr-upkeep-route"].work_item`, the pr-upkeep.pr fact that `route`
fired on.

Keyed (non-`gh:`) path:
`route` → `analyse` → `stage-dispatch` → `fix` → `stage-pr-open` →
`readiness` → `human-merges-pr` → `finish`.

Orphan (`gh:`) path:
`route` → `intake-orphan` → `stamp-pr` → `analyse-orphan` → `fix-orphan` →
`readiness-orphan` → `human-merges-pr` → `finish`.

Other branches:

- `finish-no-fix` runs after either analyse when the outcome is `no_fix`.
- `finish-no-change` runs after either fix when the outcome is `no_change`.
- `finish-expired` runs when `human-merges-pr-done` passes its 168h deadline.

The sweep path is `sweep` (timer entry) → `swept` or `sweep-failed`.

`manifest.json` lists each link and why it is `must` or `can`. It also maps
every graph node to its declarations, and every graph edge to the links that
realize it. Every non-entry declaration `must` follow `pr-upkeep-route`,
because it reads route's work item. A `can` link is used only where two
alternative predecessors land on the same node, or where the trigger
(`node.expired`) carries no lineage.

## Entries

- `pr-upkeep-route` triggers on `pr-upkeep.pr` at `root`, the fact the graph
  workflow triggers on. The graph guard's `event.payload.X` reads as
  `event.X`, because the engine's event variables are the payload itself.
- `pr-upkeep-sweep` is a genuine timer entry. Its condition matches a `timer`
  event whose payload names `schedule: pr-upkeep-sweep-5m`. The live schedule
  row emits `pr-upkeep.sweep.due` and stays as it is. Cutover needs a second
  row that emits `timer`; that is an operator hand-turn, not made here.

## Blocked agent routes

Every agent action can now return `blocked` with a reason. The three
`blocked-*` declarations listen on the landing nodes shared by stamp-pr,
analyse/analyse-orphan, and fix/fix-orphan. They show the agent's full report,
step, PR identity, work item, and available ticket to
`group/platform-maintainers`. Each task offers `retry`, `abandon`, and `acknowledged` (plus implied
`expired`). `retry-*` reuses the original actor and input templates, lands on
the original step node, and lets the existing downstream reactions continue.
Keyed and orphan analyse/fix retries select their matching original action.
`abandon-*` records decision, step, work item, and ticket without changing
Jira or the PR; `acknowledge-*` records closure. A future operator workflow
could add a Jira comment or transition after separate approval. The blocked
and retry declarations allow one reentry, so at most two retry dispatches
occur in the same lineage. The `retry_of` lineage alias binds downstream
references to the latest retried output and retains the retry input fields.
The optional ticket reference uses a template default on keyed lineages.

The blocked human routes need no new `exposes` entries: `human.ask` has the
operators audience, which is no wider than the agent result or the route
fields it receives. The existing exposure approvals below still apply.

## What does not fire yet

These gaps are recorded, not hidden. Shadow parity (t32) will show each one:

- **The timer entry needs the operator's schedule row.** `pr-upkeep-sweep`
  fires only when the second 300-second row emits `timer` with payload
  `{"schedule":"pr-upkeep-sweep-5m"}`. External ingress cannot assert this
  reserved kind or claim a `schedule:` emitter. See the cutover runbook in
  `docs/operations/pr-upkeep-lane.md` before disabling the legacy row.
- **Technical dispatch refusal is visible through the action result.** A
  missing runner registry entry, digest mismatch, or missing environment grant
  fails the dispatch run. It produces no `code.result`, so neither `swept`
  nor `sweep-failed` fires; the sweep landing node expires after one hour.
  Inspect `action.*` results and declaration evaluations in that case.

- **Reaction kinds registered by t31b.** t31b registered `pr-upkeep.pr`,
  `agent.result` and `jira.issue.transitioned` in `internal/decl/kinds`.
  t31c emits `jira.issue.transitioned`. Since t38e the control plane's
  reaction pass emits `agent.result` for a completed `agent.work` run. The
  worker envelope declares the outcomes in the declaration's
  `graph_config.contract.outcomes`, so `packaged` and `no_fix` reach the
  reaction as the agent reported them. The outcome is a proposed claim, not
  evidence.
- **Jira reactions need a configured bot account.** Since t38f the Jira
  poller and webhook read the cn1 marker back out of the comment or issue
  the jira bridge created and pass the stamped `origin`, so
  `stage-dispatch` → `fix`, `stage-pr-open` → `readiness` and
  `intake-orphan` → `stamp-pr` continue their lineage (the conformance test
  drives `stage-dispatch` → `fix`; the other two ride the same emitter code
  and are pinned only by unit tests). The bridge account in
  that origin comes only from the emitter's configuration
  (`jira_bot_account_id` for the poller, the webhook's bot account). Without
  one no origin is attached, and each of those hops starts a fresh lineage.
  The legacy `pr-upkeep.jira.comment` still drops the system's own comments.
- **GitHub reactions carry no origin.** No bridge stamps a pull request: an
  `agent.work` firing's `github.pr` marker is bound to its handover ref, and
  a `github.comment` action's comment has no reaction trigger. Nothing here
  chains on a GitHub reaction today.
- **Strings, not typed values.** A template renders a string, so `number`
  and `findings` reach the action as JSON text. `readiness.py` refuses a
  non-integer `number`. `fix`'s affinity rules are kept in
  `action.with.actor_selection` as graph provenance; the engine does not
  evaluate them.
- **A dispatch per work item.** `route`, the `finish` variants, `swept` and
  `sweep-failed` each dispatch a small docker run in 'after', where the graph
  ran a free decision or end node.

## Exposure lists and publish warnings (t30, t30b)

After linking, a validate reports 65 sensitivity warnings, one for each
reference below (34 for the original routes, 31 more for the t47 `retry-*`
routes, which re-render the original step's inputs to the same agent). These routes render variables of `pr-upkeep-route`
or `pr-upkeep-analyse` into a wider audience. The blocked human routes
introduce no additional sensitivity warnings.

Exposure is approved per variable (owner decision d4). Each declaration
below lists the variables it renders in its `exposes` list, so every warning
reads `exposure listed, no approval task yet`. The first firing that renders
one is `sensitivity-blocked` and opens one approval task for that
(declaration, entry, owner). The owner is the author of `pr-upkeep-route`
(or `pr-upkeep-analyse` for `packages`). When the owner approves, later
firings go through, including after the declaration is republished. A new
author of the producing declaration needs a new approval. Nothing here is
approved yet: every entry needs its owner's approval before cutover
(`docs/migration/tca-inventory.md` lists them).

| Declaration | Target | `exposes` |
|---|---|---|
| `pr-upkeep-analyse`, `pr-upkeep-analyse-orphan`, `pr-upkeep-stamp-pr`, `pr-upkeep-fix-orphan` | agent (team) | `pr-upkeep-route:` `findings`, `head_sha`, `number`, `repository`, `source`, `work_item` |
| `pr-upkeep-fix` | agent (team) | the same six route entries, plus `pr-upkeep-analyse:packages` |
| `pr-upkeep-retry-analyse`, `pr-upkeep-retry-analyse-orphan`, `pr-upkeep-retry-stamp-pr`, `pr-upkeep-retry-fix-orphan` (t47) | agent (team) | the same six route entries as the step they retry |
| `pr-upkeep-retry-fix` (t47) | agent (team) | the same six route entries, plus `pr-upkeep-analyse:packages` |
| `pr-upkeep-intake-orphan`, `pr-upkeep-stage-dispatch`, `pr-upkeep-stage-pr-open` | jira (team) | `pr-upkeep-route:work_item` |

`internal/api/declaration_examples_test.go`
(`TestExampleDeclarationsValidatePublishAndLink`) logs the full list and
fails if a widening is unlisted or the count changes.
