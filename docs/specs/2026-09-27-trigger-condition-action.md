# trigger-condition-action

> culture-nodes has one primitive: a trigger-condition-action declaration. Every workflow, sweep and loop is a set of declarations chained by 'must follow' / 'may follow' ordering; actions are agent work, chat/GitHub/Jira messages; triggers are external facts like a PR approved or a Jira issue created; a node is the stop between an action called and the trigger that captures its reaction.

## Audience

- Operators and workflow authors (humans), and mesh agents (codex, claude-code, colleague, pi, qwen) who declare, activate, inspect and debug automation in culture-nodes through the CLI, API and web UI.

## Before → After

- Before: Automation lives in about 128 workflow graph files (nine node kinds, edges, per-run digest pinning) plus separate schedules, the nodes-notifier daemon, repair routing and poller scripts. Causal links across systems are guessed from run input (web/src/domain/ticket-key.ts) or hand-built per flow (examples/jira-question-round-trip/`question_correlation.py`). There is no 'PR approved' trigger and no GitHub messaging action.
- After: Every automation is a named trigger-condition-action declaration moving between declared nodes, chained causally with must/can links, grouped by movable nestable aliases, and shown as one global graph with focus and distance. Humans and agents author and activate through one CLI/API. The graph engine is retired after all workflows migrate behind a before/after toggle.

## Why it matters

- One primitive that humans and agents both read and write gives clear control over what reacts to what. 'When X happens, if Y, do Z' becomes one declaration instead of a new graph, and causal context replaces per-workflow plumbing for carrying facts like the Jira issue behind a PR.

## Requirements

- The basic declaration of culture-nodes is trigger-condition-action (TCA).
  - honesty: A named declaration with exactly one trigger, one CEL condition (possibly 'true') and one action is the only authorable automation unit; the engine executes nothing that is not a declaration once the toggle is 'after'.
- Declarations chain with two ordering relations: 'A must appear after B' and 'A can appear after B'.
  - honesty: 'A must appear after B' and 'A can appear after B' are the only two ordering relations, with the lineage meaning confirmed in c45.
    - instruction: Engine tests for both relations: A fires / does not fire depending on whether B is in the triggering event's lineage.
- All existing logic and configuration is split into TCA declarations.
  - honesty: Every workflow in examples/ and every item listed in c61 (schedules, notifier posts, repair routing, hand-turns, affinity) has a declaration form, and the migration is tracked per item.
    - instruction: A migration table in the spec PR lists each of the ~128 definitions and each c61 item with its declaration form and parity status.
- Actions include: requesting an agent to do work, posting to a chat (Discord), messaging on GitHub, messaging on Jira.
  - honesty: Agent work, Discord post, GitHub message, Jira message (comment, transition, create) and running code are each available as an action kind with a registered actor or runner behind it.
    - instruction: One contract test per action kind dispatching through the real registry; the GitHub messaging actor is new (c11).
- Triggers include: GitHub PR approved, Jira issue created, etc.
  - honesty: github.pr.approved and jira.issue.created exist as named trigger events on the `signal_events` log, each emitted by at least one real source.
    - instruction: Integration tests: a GitHub review-approved webhook payload and a Jira created webhook payload each deliver the named event exactly once (dedup on redelivery).
- A node is the stop between an action being called and a trigger capturing the reaction.
  - honesty: Every declaration names a start node and a landing node; a node records the action that opened it and stays visibly waiting until a declaration starting there consumes a reaction, or it closes with a recorded reason (orphaned, timed out, loop limit).
    - instruction: Engine test: a node's open/closed state and reason are queryable through the API for each closing path.
- GitHub messaging becomes an action with its own actor. Today no actor posts to GitHub: PR replies/comments live inside runner scripts (examples/land/`land_reply.py`), and the cicd skill is an operator tool outside the engine.
  - honesty: A GitHub actor exposes at least `post_comment` and `reply_to_review_thread`, with a repo allowlist, and returns proposed records like the jira actor.
    - instruction: Port the calls in examples/land/`land_reply.py` behind a new adapters/github bridge; the byte-identical shared bridge modules apply.
- 'GitHub PR approved' becomes a trigger. Today it is not detectable: there is no GitHub webhook receiver, and the pr-upkeep sweep (examples/pr-upkeep/sweep.py, `pr_upkeep_emit.py`) emits only pr.opened / pr.merged / pr.closed; readiness.py reads unresolved threads, never reviewDecision.
  - honesty: A PR review approval reaches the log as github.pr.approved via a webhook receiver (preferred, c54) with the poller as fallback emitting the same event name and payload shape.
    - instruction: Add POST /v1alpha1/webhooks/github with signature verification modeled on internal/api/jirawebhook.go; extend `pr_upkeep_emit.py` to read reviewDecision.
- 'Jira issue created' becomes a named trigger. Today creation surfaces only indirectly, as a synthetic `pr-upkeep.jira.transitioned.<initial-status>` with `from_status` '' built from fields.created (internal/api/jirawebhook.go jiraEmissions), and trigger event names carry the pr-upkeep.\* prefix of the loop that first needed them.
  - honesty: jira.issue.created is emitted by both the webhook and the JQL poller with the same payload, and the pr-upkeep.jira.\* names are replaced by neutral jira.\* names during migration.
    - instruction: Test that a created issue yields exactly one jira.issue.created across both sources (watermark dedup).
- The split of 'all existing logic and configuration' (U3) covers about 128 workflow definitions: 25 YAMLs in examples/ (pr-upkeep/workflow.yaml alone is 880 lines, 11 nodes, 18 edges, a trigger and affinity rules), plus compiler/worker/engine testdata, web `__golden__` fixtures and schemas/examples.
  - honesty: The migration count is measured, not estimated: the spec PR lists every workflow file found by the examples guard and the testdata globs.
    - instruction: Generate the list with git ls-files '\*.workflow.yaml' 'examples/\*\*/workflow.yaml' and commit it with the migration table.
- Changing the core primitive away from the PRD's node/edge graph is recorded as an explicit deviation (ADR), not drift, per the CLAUDE.md rule 'Record deviations from the PRD explicitly'.
  - honesty: An ADR records the departures from the PRD: declarations replace the node/edge graph, per-firing replaces per-run pinning, and activation-by-declaration.
    - instruction: docs/adr/00NN-trigger-condition-action.md committed in the spec PR and cited by issue #328.
- Code run (script, bash, python, js) is also an action, whether it runs internally or through an actor.
  - honesty: A code action runs bash, python or js either in the server-side sandbox or on a registered runner, chosen per declaration.
    - instruction: Runner conformance test for each language on the built-in sandbox runner.
- Each declaration (a trigger-condition-action set) has a name.
  - honesty: Declaration names are unique within the namespace and stable across versions; a version changes the digest, not the name.
    - instruction: Store constraint plus test: publishing a second declaration with an existing name creates a new version of it.
- Declarations can be viewed individually, or as declaration chains, which render as a graph.
  - honesty: The UI and CLI show a single declaration or a chain rendered as a graph from the same API response.
    - instruction: Web test on the new view plus a CLI golden for `show` and `chain show`.
- Focusing on a declaration shows a 'distance' neighborhood: distance 0 is only that declaration, 1 adds the declarations directly connected to it, 2 the next ring, and so on.
  - honesty: Focus at distance N returns exactly the declarations within N hops (both directions, both link types, filterable, c63).
    - instruction: Unit test over a fixture graph checking the returned set for N=0,1,2 and each filter.
- A declaration that appears after another can reference variables produced by earlier declarations (e.g. Jira issue owner, Jira replier, Discord replier).
  - honesty: A later declaration can read variables of earlier declarations in its lineage through the {N:name:default} / {declaration:name} template.
    - instruction: Engine test on a three-declaration chain reading {1:x}, {2:y} and {decl:z}.
- Publish warns (never refuses) on every template reference that may be missing and has no default: it crosses only 'can' links, or step N back may not exist. Today internal/compiler/contract.go checkPointer only checks that /nodes/<id> names a declared node, not that it runs before the reader.
  - honesty: Publish warns on every template reference that may be missing (crosses a 'can' link, or step N may not exist) and has no default.
    - instruction: Compiler test: each case produces the warning; a reference with a default produces none.
- The CLI and API let a user or an agent declare new declarations and build graphs from them, so agents and humans have clear control over what exists.
  - honesty: Every declaration operation (declare, link, alias, activate, show, focus) has an API route and a CLI verb with --json, and agents and humans call the same routes.
    - instruction: API/CLI parity test enumerating routes against CLI verbs, like tests introspecting `known_paths`().
- Chains can be named, like an alias.
  - honesty: A chain alias resolves by name in every verb that accepts a chain.
    - instruction: CLI test: `chain show <alias>` and `focus` accept the alias.
- Every CLI verb for declarations and chains (declare, list, show, link, alias, focus --distance N) is a thin client over a v1alpha1 API route with --json, so agents and humans use the same surface. This follows the existing split: Go cmd/nodes plus the Python `culture_nodes`/cli/`_commands`/workflow.py (generate / validate / publish / list / get) over /v1alpha1/workflows.
  - honesty: CLI verbs are thin: they call the API and render; no declaration logic lives in the CLI.
    - instruction: Test that each verb's handler only calls the API client (mocked) and formats.
- Alias nesting is checked at write time: a move or nest that would make an alias contain itself, directly or through other aliases, is refused with the cycle named.
  - honesty: Moving or nesting an alias so it contains itself is refused, naming the cycle.
    - instruction: Store test with a three-alias cycle.
- Every activation, deactivation and alias move is an append-only record naming who or which declaration did it and which version it points at, so 'why is this declaration live?' is always answerable (records are immutable; corrections append with supersedes).
  - honesty: Every activation, deactivation and alias move is an append-only record naming the actor or declaration that did it and the target version.
    - instruction: Ledger test: the history of a declaration's activation state is reconstructable from records alone.
- Chaining carries causal context: when trigger B fires on something action A produced, B's run can read the variables of the declaration run that led to A (e.g. 'PR spec created' knows the Jira issue behind the intake that committed the PR).
  - honesty: A trigger event whose origin marker traces to a firing inherits that firing's lineage; an event with no marker starts a fresh lineage.
    - instruction: Engine test with stamped and unstamped PR events.
- The system suggests chain links: it knows what each action produces (e.g. a PR) and what each trigger consumes (e.g. a PR event), marks a producing->consuming pair as relevant, and the user chooses whether to chain them.
  - honesty: For a new declaration, the system lists candidate predecessor/successor declarations whose produces/consumes types match, and linking is always the user's or agent's explicit choice.
    - instruction: API test: suggestions are returned but no link is created without an explicit link call.
- Declaration chains need loop protection.
  - honesty: Loop protection is always on: every chain is subject to the re-entry limit (default 3) and the backstops in c46.
    - instruction: See the c46 tests.
- Each action and trigger kind declares a typed produces / consumes signature (e.g. agent intake produces github.pr; trigger github.pr.created consumes github.pr), which is what lets the system suggest links (the user's c41) and check variable references at publish.
  - honesty: Every action and trigger kind declares a produces/consumes type from a closed, versioned vocabulary (e.g. github.pr, jira.issue, discord.message).
    - instruction: Schema test that every registered kind declares its signature.
- Loop protection works on causal lineage (c40, c43). The re-entry limit (c51) is declared on the trigger, defaulting to 3 appearances of the same declaration in one lineage, with 0 meaning the declaration may not re-appear in its own lineage. Backstops: a hop limit on total lineage depth, no self-retrigger unless the declaration opts in, and a per-declaration rate ceiling. Hitting any limit stops the chain and raises a visible record for a human, never a silent drop. Generalizes `jira_comment_is_self_echo`, maxTransitions/maxVisitsPerNode, the repair bound (internal/repair) and subject deferral (`deferred_triggers`).
  - honesty: A re-entry beyond the trigger's limit, a lineage deeper than the hop limit, an unopted self-retrigger, or a rate breach each stops the chain and writes a visible record; nothing is silently dropped.
    - instruction: One engine test per limit, asserting the record.
- An in-flight node (an action called, its reaction not yet captured) keeps the version that called the action; the reaction is evaluated by whatever versions are current when it arrives. If the upgrade removed the declaration or changed its trigger so the reaction no longer matches, the node closes with a visible 'orphaned by upgrade vN' record, never a silent drop.
  - honesty: A node whose reaction arrives after its reacting declaration was removed or changed so it no longer matches closes with an 'orphaned by upgrade' record naming both versions.
    - instruction: Engine test: park a node, upgrade the reacting declaration, deliver the reaction.
- Variable resolution follows the user's template rule (c64): {N:var:default} walks N steps back along the causal lineage (or to the named declaration, c65); a missing value renders the default, and with no default the placeholder stays verbatim. Publish still reports, as a warning, every reference that can be missing (it crosses a 'can' link, or step N may not exist) and has no default, so an author sees 'Hi {1:owner}, ...' is possible before it reaches a Jira comment.
  - honesty: Template rendering follows c64 exactly: value, else default, else the placeholder text verbatim; {N:x:} renders empty.
    - instruction: Table-driven renderer test covering each row of the rule.
- Lineage survives at-least-once delivery: a redelivered event with the same event id is deduplicated (`signal_event_watermarks`) and never fires a declaration twice; a re-minted trigger (`trigger_remints`) keeps the original lineage, so must-appear-after and re-entry counts see one chain, not two.
  - honesty: Delivering the same event twice produces one firing, and a re-minted firing counts once toward re-entry.
    - instruction: Engine tests: duplicate delivery, and remint after a failed firing, each asserting one lineage entry.
- When two declarations share similar conditions (overlapping trigger and condition), the user can see it, or be notified of it.
  - honesty: At publish and on every activation, the system reports each pair of active declarations that start on the same node with the same trigger and conditions that can both be true (checked for satisfiable overlap where CEL allows, otherwise flagged as 'possibly overlapping'); the report is shown in focus/show and, when configured, emitted as a declaration.overlap event that a declaration can turn into a notification.
    - instruction: Test: two declarations on one node, same trigger, conditions priority=='High' and priority!='Low' -> reported; conditions priority=='High' and priority=='Low' -> not reported. A declaration.overlap event reaches the log.
- Origin markers are unforgeable: each marker carries a MAC over (firing id, artifact kind, artifact id) keyed by the control plane, and an event inherits lineage only when its marker verifies; where the provider reports authorship, the artifact must also be authored by the bridge's own account. Anything else is treated as unmarked. Today's markers are plain text (adapters/jira/src/`jira_bridge`/mapping.py `marked_text`), so anyone who can edit a PR body or comment could copy one to inherit another chain's variables, satisfy a 'must appear after', and make actions fire.
  - honesty: A copied, edited or hand-written marker never yields lineage; only a marker minted for that exact artifact verifies.
    - instruction: Tests: valid marker inherits; marker copied to another PR, tampered firing id, and unsigned legacy marker each start a fresh lineage and write a 'marker rejected' record.
- Migration runs the declaration engine in shadow first: it evaluates triggers and conditions and records 'would fire' firings without dispatching actions, while the graph engine still acts. Parity (c70) compares shadow firings against graph runs. Only after parity does a scope switch to 'after', so no Jira comment, Discord post or agent session is ever produced twice.
  - honesty: While a scope is in shadow, the declaration engine dispatches zero actions; switching to 'after' stops the graph engine acting for that scope in the same transaction.
    - instruction: Test: shadow mode on a real trigger produces a would-fire record and no actor invocation; flipping produces exactly one dispatcher.
- Flipping a scope back from 'after' to 'before' is safe: open declaration nodes are frozen with a visible record (not dispatched and not dropped) and resume if the scope is flipped forward again; the graph engine never adopts declaration nodes. Rollback is a supported, tested path, not an emergency improvisation.
  - honesty: A flip back and forward again loses no open node and dispatches no action twice.
    - instruction: Test: open a node, flip back, deliver its reaction (recorded, not fired), flip forward, redeliver: one firing.
- Every node declares a deadline (or an explicit 'none'); at expiry it emits node.expired, which declarations can take as a trigger (remind, escalate to a human, give up). Today's model already has timeout/deadline fields (internal/compiler/model.go) and scheduler timers (internal/store/postgres/timers.go); without them a node whose reaction never comes waits forever, invisible.
  - honesty: No node can be open without a deadline or a declared 'none', and an expired node always emits node.expired exactly once.
    - instruction: Test with a scheduler clock: expiry fires once, and a late reaction after expiry is recorded, not fired.
- An action's technical result is its own trigger family (action.failed, action.`timed_out`, action.rejected, action.`capacity_exhausted`), separate from domain reactions, so a declaration can route failures and a failed action never leaves its node silently open (PRD §3.4: domain outcome is not technical status).
  - honesty: Every non-success actor/runner status produces exactly one action.\* trigger event linked to the node.
    - instruction: Test per status from internal/actors error classes.
- Declarations keep per-subject concurrency: a declaration may cap how many of its firings are in flight per subject key (e.g. per Jira issue, per PR) and defers the rest, preserving today's limits.maxConcurrentSubjectRuns and `deferred_triggers` (examples/jira-intake, examples/jira-comment-consumer: 2). Without it, a burst of Jira comments dispatches one agent session each against a capped subscription window.
  - honesty: A burst of N events on one subject with cap K yields at most K in-flight firings, and the rest run later in arrival order or collapse per today's replace rule.
    - instruction: Engine test mirroring internal/store/postgres/subjectconcurrency.go cases.
- The PRD run vocabulary is mapped, not dropped: the ADR (c21) defines lineage, firing, node and attempt against run, token, node run and attempt, and every consumer of today's run surfaces keeps a working read path until migrated. Consumers: web UI (/v1alpha1/runs, /node-runs, /human-tasks, /pending-decisions, /tickets in web/src/api/client.ts), the nodes-notifier (dev.culture.nodes.run.created/completed/failed/cancelled/bounded in internal/notifier/lifecycle.go), scripts/collect-handover.py, and the nodes-operator skill.
  - honesty: The migration table lists every consumer of run/node-run/token surfaces, each with its declaration-era read path and parity status.
    - instruction: Grep-derived consumer list committed with the migration table; each consumer has a test on its new read path.
- The two lanes that produce workflow source produce declarations after migration: workflow generation (internal/api/`workflow_generations.go`, an actor drafting source) and devague plan import (/v1alpha1/plan-imports, cmd/nodes/planimport.go). They were missing from the c61 list.
  - honesty: Generation and plan import each emit declarations that validate and publish, with their existing tests ported.
    - instruction: Port `workflow_generations` and planimport tests to declaration output.
- Stamping is a capability each bridge advertises on /v1/capabilities (with its revision); the engine refuses, with a visible record, to dispatch an action that needs stamping to a bridge that does not advertise it. The codex and notify bridges are uv tool install copies that go stale silently (CLAUDE.md), so one stale bridge would otherwise produce unstamped artifacts and quietly break causal context on its lane.
  - honesty: A bridge without the stamping capability cannot receive a stamping-required dispatch; the refusal names the bridge and its revision.
    - instruction: Test with a fake bridge advertising an old capability set.
- Every trigger match is explainable: for each declaration whose trigger matched an event, the engine records the outcome (fired, condition false, required lineage missing, loop-limited, deferred, overlap-suppressed, shadow) and 'why did / didn't X fire for event E' is answerable from the API and CLI.
  - honesty: For any event id and declaration name, the API returns the evaluation outcome and its reason.
    - instruction: API test covering each outcome kind.

## Honesty conditions

- After migration, every automation that runs in culture-nodes can be listed as named declarations, and nothing fires that is not a declaration.
  - instruction: Test: with the toggle on 'after', start from a fresh store, publish only declarations, and assert the engine has no other path from an event to an action.
- An agent-backed action yields only proposed records; external triggers become observed only when emitted by a trusted receiver that measured them (webhook signature verified or poller read the source).
  - instruction: Ledger tests: an actor returning observed is capped to proposed; an unsigned webhook is refused.
- Each firing's ledger record names the declaration id and exact version digest of the trigger, condition and action it evaluated.
  - instruction: Engine test: upgrade a declaration mid-chain; the two firings record different digests, and both versions remain fetchable.
- No action path executes code inside the control-plane process; code actions reach only the server-side sandbox runner or a registered remote runner.
  - instruction: Extend the existing neutrality/import guards: the control-plane packages import no exec/os.StartProcess path outside the runner client.
- An activation declaration cannot activate itself or another activation declaration by the same author, and the first activation declaration can only be activated by a human principal.
  - instruction: Engine tests for each refusal, with the refusal recorded as a visible record.
- Server-side code runs in a separate sandboxed process with its own timeout; killing or hanging it does not stall the control plane.
  - instruction: Fault test: a hanging script is killed at its timeout while API health stays green.
- Every capability offered to the audience is reachable by both a human principal and an agent principal through the same CLI verb and API route; only activation authority differs (c33).
  - instruction: Parity test driving each verb once as a human session and once as an agent token.
- The before-state figures are reproduced from the tree at the spec commit, not carried from this conversation: workflow-file count, node kinds, and the absence of a PR-approved trigger and a GitHub messaging actor.
  - instruction: Commit the counting commands and their output alongside the migration table.
- After migration, one new 'when X, if Y, do Z' behavior is added as exactly one declaration with no engine, schema or migration change, shown once on a real behavior.
  - instruction: Record the one-declaration addition in the delivery summary with its digest.
- Each element of the after state maps to at least one success signal c70-c74, and none is claimed delivered without its signal's evidence.
  - instruction: The spec export lists the element-to-signal mapping; summarize-delivery checks it.
- The parity comparison is recorded evidence (run ids for both engines on identical inputs, with outcome and ledger-kind diff), not an agent's statement.
  - instruction: A comparison script writes the diff as an observed evidence record.
- The rendered Jira key in the PR declaration's run comes from the stamped lineage, verified by the run's lineage record, on a live Jira issue and PR.
  - instruction: Live run ids and lineage record cited in the delivery summary.
- Both stops (limit 3 and limit 0) occur on live declarations and each leaves a visible record queryable through the API.
  - instruction: Live run ids plus the stop records.
- The agent performs every step through CLI --json with an agent credential; no human edits happen between steps except activating the first activation declaration.
  - instruction: Agent transcript and ledger records cited.
- The graph engine packages, node kinds and graph examples are removed from main, and tests pass without them.
  - instruction: The removal PR, with the lint guard updated to glob declaration files.
- A declaration body claiming a different author is ignored; the recorded author matches the request principal.
  - instruction: API test publishing a body with a forged author field.

## Success signals

- Side-by-side parity: pr-upkeep and jira-intake, migrated to declarations, run with the toggle on 'after' against the same inputs as the graph engine and produce the same domain outcomes and ledger record kinds (a recorded comparison, not a claim).
- Causal context live: a Jira issue created -> agent intake commits a PR -> a 'PR spec created' declaration renders {1:key} (or {jira-intake:key}) with that Jira key, taken from the stamped lineage, with no ticket-key guessing; a human-opened PR fires the same declaration with the placeholder rendered by the template rule.
- Loop protection live: two declarations that re-trigger each other stop at the default re-entry limit of 3, and a trigger set to 0 stops at the first re-entry; each stop leaves a visible record.
- Agent control live: an agent declares a declaration, links it, validates it, and has it activated by an active activation declaration, all through `--json` CLI verbs; `focus <name> --distance 2` shows it with its neighbours.
- Retirement: the graph engine and its node kinds are deleted, and every example in examples/ exists only as declarations.

## Scope / boundaries

- The ledger authority model is unchanged by TCA: an action executed by an agent actor still yields only proposed records; a trigger from an external system is an observed fact only where a trusted emitter measured it directly; no declaration promotes its own action's proposal (PRD §10.4, CLAUDE.md design ground rules).
- Every declaration version is immutable and content-addressed (internal/contracts/canonical.go). The pin moves from the run to the firing: each firing records the exact version it evaluated (trigger, condition, action), so a live upgrade never rewrites history and 'which version did this?' is always answerable. A new version applies from the next firing, including in chains already in flight (q16). This deviates from PRD §9.1/§9.10 'runs pin an immutable digest' and is recorded in the ADR (c21).
- Code still executes only through the runner boundary (headspace-cli / FunctionRegistry); a TCA action never becomes a shell, script or Docker call inside the control-plane process.
- Activation policy is itself a set of declarations (q13), and those activation declarations are the root of trust: an activation declaration cannot activate itself or another activation declaration it authored, and the first ones are activated by a human. Without this, an agent could declare 'activate everything I propose', self-activate it, and hold standing authority to post, comment, dispatch or run code with no human ever having granted it (PRD §10.4: no actor promotes its own proposal).
- 'On the server' means a built-in runner on the control-plane host, in a separate sandboxed process (headspace-style workspace), never inside the control-plane process. This keeps c19: a hung, crashing or hostile script cannot stall or tamper with the service that holds authoritative state. Remote runners are registered and offered like agent actors.
- The author of a declaration, which activation declarations condition on (c35, c33), is the authenticated principal recorded by the API (the Access identity or actor credential), never a field in the declaration body.

## Non-goals

- No stack or substrate rewrite: Go, Postgres as authority, the `signal_events` log, CEL, actor/runner protocols with callbacks, leases/fencing and the ledger stay. What is replaced is the graph interpreter (tokens, edges, entry/end, parallel/join, per-run pinning), which the declaration engine supersedes after migration.

## Assumptions

- The trigger half already exists and is reused, not rebuilt: every emitter appends to one `signal_events` log via DeliverSignalEvent (internal/store/postgres/signal.go), and published workflows already declare spec.triggers `{onEvent, when: <CEL>}` (internal/compiler/model.go, internal/engine/trigger.go). TCA triggers are named signal events on this log.
- The condition half already exists as CEL, type-checked at publish (internal/compiler/cel.go, variables input/output/outcome/event/node/budget) and evaluated at runtime by internal/engine/transition.go evaluateGuard. TCA conditions are CEL over the trigger event (plus whatever else q-scope decides).
- The action half already exists as actor dispatch: agent work (adapters/codex, claude-code, pi, qwen, colleague), Discord (adapters/notify, actor://company/notify-discord), Jira (adapters/jira: `post_comment`, `transition_issue`, `create_issue`, `read_issue`), humans (adapters/human-inbox). Each is an actor record resolved from the actors table by digest-pinned uses: ref.
- A node (U6) maps onto the existing parked state between dispatch and reaction: an actor invocation that answered 202 and waits for callbacks (`actor_invocations`, migration 0009), a wait node parked on until.signal (`signal_subscriptions`), or an approval parked on a human decision. The TCA model names that parked state 'node' instead of naming the action step 'node'.
- Ordering today is 'may follow' only: an edge from `<node>.<outcome>` (or onEvent) to a target, first match wins in normalized order. 'Must follow' has no general form; it exists only as special cases: join barriers after parallel, wait nodes, approval pauses.
- The chain graph and distance view build on the existing web graph stack (web/src/domain/graph.ts parseWorkflowGraph, the useElkLayout hook, web/src/routes/DesignCanvas.tsx, ActiveGraphCanvas) and add a focus-plus-radius filter. It is a new view, not a new renderer.
- An alias is a movable name over a group of trigger-chains and/or other aliases (nesting allowed, q14). It does not lock declaration versions: it resolves to whatever version of each declaration is current (q16).
- Causal links resolve through origin markers stamped by actions, generalizing the marker contract in examples/jira-question-round-trip/`question_correlation.py`. Today the jira actor appends '\[culture-nodes:jira-actor `question_id`=<id>\]' to the comments it posts, the sweep stamps `originating_question_id` on the reply fact, and the consumer checks it (a wait node wakes on every event of that name with no payload filter, internal/worker/wait.go). Under TCA every action stamps its declaration-run id into what it creates (PR body or branch, Jira comment, Discord message), and an incoming trigger event traced to a marker inherits that run's variables.
- The q15 answer suggests an interpretation of q2 in lineage terms: 'B must appear after A' means B fires only on an event whose causal lineage contains a run of A, so A's variables are guaranteed; 'B can appear after A' means B fires either way and A's variables are present only when the lineage contains A. Pending the user's confirmation.
- With q4, the graph is nodes joined by declarations: a declaration is a transition from its start node to its landing node, guarded by trigger + condition, performing its action. B follows A when A's landing node is B's start node; 'must/can' then says whether B requires A in its lineage (c45). Declarations whose trigger is an outside fact (jira.issue.created) start on a shared root node.
- Q6 proposal, how today's control kinds become declarations: approval = an action asking a human (human-inbox) landing on a waiting node, then declarations triggered by human.decision with condition outcome == 'approved' / 'rejected'; decision/select = several declarations from one node with mutually exclusive conditions; parallel = several declarations from one node that all match; wait(duration) = a timer trigger; continue.while = a declaration that lands back on its own start node, bounded by the re-entry limit (c51); end = a terminal node with no outgoing declarations; join = a declaration that must-follow several predecessors and fires once all are in its lineage.
- Q7 proposal, what configuration becomes declarations: schedules (timer triggers), the nodes-notifier lifecycle Discord posts (internal/notifier; declarations on system events like run.failed), repair routing after a red merge gate (internal/repair), hand-turn definitions, and affinity rules (which actor receives the action, as part of the action). What stays configuration: the actor and runner registry, credentials and grants, deployment, and the declaration engine's own settings.
- Q10 proposal: there is one global declaration graph. Today's workflow boundaries disappear; aliases are the only grouping, so 'appears after' may link any two declarations (jira-intake -> pr-upkeep -> land) and the distance view may cross what are separate workflows today.
- Q11 proposal: distance counts hops in both directions (what leads here and what follows), across both must and can links, with filters for direction and link type. Distance is measured over declarations; nodes are drawn but do not count as hops.

## Scope exploration

- `s1` — `internal/store/postgres/signal.go + internal/engine/trigger.go`: One `signal_events` log already fans out to three consumers: start a run (spec.triggers match), wake a wait node (`signal_subscriptions`), pick up inside a run (`event_routes`). Emitters: POST /v1alpha1/events, POST /v1alpha1/webhooks/jira, schedules (internal/scheduler/schedules.go), and the pr-upkeep poller posting to /events.
  - seeds: `c8`
- `s2` — `internal/compiler/cel.go + internal/engine/transition.go`: Conditions are CEL strings on triggers, edges, decision select\[\] and affinity; there is no separate condition type. A guard that errors at runtime counts as no match.
  - seeds: `c9`
- `s3` — `internal/actors + adapters/*`: Actions today are two dispatch paths: actor (kind agent, uses actor://, async 202 + HMAC callbacks, results capped at proposed) and runner (kind code, uses runner://, FunctionRegistry allowlist). Verbs live only inside each bridge's input parser; the engine has no verb list.
  - seeds: `c10`
- `s4` — `examples/land/land_reply.py`: The only engine-driven GitHub writes are review-thread replies, resolves and one PR comment per landing, performed by a runner code node's script, not by an actor. U4's 'messaging on GitHub' has no action surface today.
  - seeds: `c11`
- `s5` — `examples/pr-upkeep/sweep.py + pr_upkeep_emit.py + readiness.py`: GitHub facts reach the event log only by polling (schedule -> pr-upkeep.sweep.due -> sweep-cycle.workflow.yaml code node -> POST /events). Emitted: pr.opened, pr.merged, pr.closed, pr-upkeep.pr work items. No approval event exists.
  - seeds: `c12`
- `s6` — `internal/api/jirawebhook.go + examples/pr-upkeep/pr_upkeep_jira.py`: Jira reaches the log by webhook (HMAC/URL-token, re-fetches the issue) and by JQL polling. Emitted names: pr-upkeep.jira.comment / .changed / .transitioned.\*; no created event, and the namespace is tied to one loop.
  - seeds: `c13`
- `s7` — `internal/compiler/vocabulary.go (node kinds)`: Today 'node' means a step: nine kinds (agent, code, action.http, decision, approval, wait, parallel, join, end). U6 redefines node as the waiting stop between an action and its reacting trigger, which is closest to wait/approval/async-invocation state, not to agent/code steps.
  - seeds: `c14`
- `s8` — `internal/compiler/graph.go + parallel.go + docs/design/2026-08-13-parallel-tokens-full.md`: Edges express permitted succession; join{all|any|quorum} is the only barrier. The compiler rejects unreachable nodes, missing reachable end, end inside a split, join outside a split. U2's two relations do not map one-to-one onto today's edge/join model.
  - seeds: `c15`
- `s9` — `examples/ + */testdata + web/src/domain/__golden__`: Counted about 128 workflow files. tests/lint/`examplescompile_test.go` compiles every example (floor 11); `developmentloop_test.go` and `specchain_test.go` pin specific node shapes; web/src/domain/workflow-document.ts round-trips YAML surgically for the canvas.
  - seeds: `c16`
- `s10` — `docs/initial-design/culture-nodes-prd-spec.md §10.4 + actor result caps`: Actor results are capped at authority proposed (e.g. jira `post_comment` returns a claim with outcome `comment_posted`). A TCA whose trigger is 'agent said done' must not read as verified evidence.
  - seeds: `c17`
- `s11` — `internal/contracts/canonical.go + api/openapi/openapi.yaml`: Digest pinning and immutable publish are PRD ground rules (§9.1, §9.10) and have golden fixtures in internal/contracts/testdata/golden/.
  - seeds: `c18`
- `s12` — `internal/runners/ + CLAUDE.md runtime ground rule`: Runner dispatch refuses unregistered names before any call leaves the process; the IAM policy renders from the same registry.
  - seeds: `c19`
- `s13` — `cmd/nodes + internal/* + migrations/`: The runtime already has triggers, CEL, actor dispatch, event routes, subject concurrency (`deferred_triggers`), trigger remints and schedules; replacing it would discard 58+ migrations of durable behavior.
  - seeds: `c20`
- `s14` — `docs/initial-design/culture-nodes-prd-spec.md §3, §9 + docs/adr/`: PRD §3.1 vocabulary (workflow, node, edge, actor, run, token, node run, attempt) and §9.2 node kinds define the graph as the core model; ADR 0007 (authoring slice) and the canvas decisions (docs/decisions/2026-09-03-\*) build on it.
  - seeds: `c21`
- `s15` — `internal/runners/ + examples/land/land.py + examples/pr-upkeep/sweep.py`: Code already runs as an action through kind: code / uses: runner:// (headspace runner on a host, or Lambda), e.g. land.py and sweep.py. 'Internally' collides with the CLAUDE.md ground rule that code never runs as a shell/script/Docker call inside the control-plane process (claim c19).
  - seeds: `c22`, `c19`
- `s16` — `web/src/domain/graph.ts + routes/DesignCanvas.tsx + hooks/useElkLayout`: The web already parses a workflow into a graph and lays it out with ELK for the Design canvas and active runs. There is no focus-on-one-node or distance-radius view; the unit shown is a whole workflow, not a named declaration or a cross-workflow chain.
  - seeds: `c24`, `c25`, `c29`
- `s17` — `internal/compiler/model.go bindingValue + contract.go checkPointer`: Bindings are JSON Pointers (/run/input/..., /nodes/<id>/output/...) or literals. checkPointer verifies the node id exists in spec.nodes but has no reachability or ordering check, so a binding to a node that never ran before the reader passes publish today.
  - seeds: `c26`, `c27`, `c28` (rejected)
- `s18` — `internal/api/workflows.go handlePublishWorkflow + server.go routes`: Publish compiles and stores an immutable version idempotently by digest. The handler makes no agent-versus-human distinction; authority comes only from edge auth (the Access cookie since 0.47.0). Workflows are listed and fetched by digest, with metadata.name/version as labels, not as a resolvable alias.
  - seeds: `c30`, `c33`, `c34`
- `s19` — `internal/api/workflow_generations.go + culture_nodes/cli/_commands/workflow.py`: Agent authoring already exists as a proposal lane: generation runs a fixed orchestration graph in which a registered fleet actor turns prose into source; the control plane holds no model credential and the output is source + diff + digest + diagnostics, not a publish. The Python CLI exposes workflow generate/validate/publish/list/get.
  - seeds: `c30`, `c32`, `c33`
- `s20` — `examples/jira-question-round-trip/question_correlation.py + internal/worker/wait.go`: A working causal-correlation prototype already exists for one case: the jira actor stamps a `question_id` marker on the comment it posts, the sweep carries it to the reply fact as `originating_question_id`, and the resumed leg checks the reply answers the question it asked. Correlation is consumer-side because wait nodes wake on every event of a name with no payload filter.
  - seeds: `c40`, `c43`
- `s21` — `pr_upkeep_jira.py jira_comment_is_self_echo + limits + internal/repair + deferred_triggers`: Loop protection today is scattered: self-echo filtering of the system's own Jira comments by account id, per-run transition and per-node visit limits, a repair bound of 2 per 24 h, and one active run per subject. None of it works across runs along a causal chain, which is where TCA loops will live (action A posts -> trigger B -> action B posts -> trigger A).
  - seeds: `c42`, `c46`
- `s22` — `web/src/domain/ticket-key.ts`: Today the only cross-system link from a run to its ticket is a key dug out of the run's input (`ticket_key` / `issue_key` / `jira_key`); there is no ticket column on runs. Causal lineage would replace this guesswork with a recorded link.
  - seeds: `c40`, `c43`
- `s23` — `challenge pass / security lens: adapters/jira/src/jira_bridge/mapping.py + examples/pr-upkeep/pr_upkeep_jira.py`: Markers are unsigned text appended to comments. The self-echo check already distrusts them: a configured bot account id is authoritative 'so a human quoting the actor marker cannot suppress their own fact'. Causal lineage would raise the stakes from suppression to injection.
  - seeds: `c79`
- `s24` — `challenge pass / migration lens: deploy/prod/*.sh NODES_NAMESPACE_ID + o12`: Prod runs one namespace, so o12's per-namespace toggle cannot stage one loop at a time, and running both engines live would duplicate every external action.
  - seeds: `c80`
- `s25` — `challenge pass / failure-mode lens: internal/compiler/vocabulary.go routable statuses + PRD §3.4, §13.5`: Today technical statuses (failed, `timed_out`, `policy_denied`, `contract_rejected`) are routable edge outcomes. The frame had no equivalent for declarations: a failed action had no reaction trigger.
- `s26` — `challenge pass / concurrency lens: internal/store/postgres/subjectconcurrency.go + examples/jira-*/workflow.yaml`: Subject concurrency and deferral are load-bearing in two live loops and were absent from the frame.
  - seeds: `c84`
- `s27` — `challenge pass / adjacent-systems lens: web/src/api/client.ts + internal/notifier/lifecycle.go + .claude/skills/nodes-operator`: Five web endpoints, the notifier's five run lifecycle event types, the handover collector and the operator skill all read run-shaped state that the frame retires.
  - seeds: `c85`
- `s28` — `challenge pass / operations lens: CLAUDE.md install_mode note + adapters/*/capabilities.py`: Bridges advertise preflight facts and deployment revision; stamping (o3) must join that surface or staleness becomes silent context loss.
  - seeds: `c87`
- `s29` — `challenge pass / cheap probes: mapping.py, deploy/prod namespace, notifier lifecycle, web client endpoints, subjectconcurrency, model.go timeouts`: Six read-only probes run; findings F1, F2, F4, F6, F7 and F9 each rest on one of them. Not probed: load behaviour of lineage lookups under real event rates, and GitHub webhook delivery semantics (redelivery, ordering), which were read from the frame's claims, not from GitHub's docs.
- `s30` — `challenge pass / lenses examined`: All six lenses swept: adjacent systems, unstated assumptions, overlooked actors/lifecycle/failure modes, security-migration-concurrency-operations-reversibility, observability-containment-rollback, cheap probes. Not examined: web UI component-level impact beyond API endpoints; the Helm and AWS deploy paths (deploy/helm, deploy/aws); cost of the engine rewrite itself.

## Decisions

- Activation is self-governed through trigger-condition-action: a declaration becomes active when an activation declaration fires (trigger: declaration proposed; condition: author, action kinds, etc.; action: activate). An agent may self-activate when such a declaration allows it.
- An alias can be moved to point at a new version, and aliases can nest, so two aliases can be chained.
- The trigger is part of a declaration; aliases do not carry triggers. Aliases group trigger-chains.
- An alias does not lock declarations: when a declaration upgrades, the new version applies wherever it is used, including chains already in flight. Every version is kept.
- Loop limit: default 3, configurable, and 0 is an allowed value.
- Q1 recommendation: a new, smaller engine core that executes declarations directly, built on the existing substrate, with the graph engine run side by side until the examples are migrated and then retired. One firing is one short transaction: event arrives -> match triggers -> trace lineage -> evaluate CEL condition -> dispatch action -> open a node that waits for the reaction. Kept: `signal_events` log and emitters, CEL (internal/compiler/cel.go), actor and runner dispatch with callbacks, leases and fencing, the ledger and its authority model, Postgres, the API server. Retired: tokens, edges, entry/end, parallel/join, per-run digest pinning. Compiling into today's IR is rejected because live upgrade (q16), cross-chain causal lineage (q3) and alias nesting (q14) do not fit a graph pinned to one digest per run.
- The loop limit is a re-entry limit on causal lineage: default 3 appearances of the same declaration in one lineage, 0 = no re-appearance at all. It is set per context, depending on the trigger.
- Nodes are declared explicitly and sit between declarations: every declaration starts on a node and lands on a node.
- All existing workflows are migrated, with a before/after toggle between the old graph engine and the new declaration engine.
- GitHub triggers: a webhook receiver is preferred; the existing poller (examples/pr-upkeep/sweep.py) is an acceptable fallback.
- Code actions run either on the server or on runners offered the way agents are offered; runners are for harder or more complex code.
- Variable syntax: {owner} reads this declaration run's variable; {1:owner} reads owner from the previous declaration in the lineage (N = steps back).
- Variable template form is {N:Name:DefaultValue}. When there is no relevant value, the default is used; with no default given, the placeholder text itself stays in the output. {10:Name:} (empty default) renders as an empty string when there is no declaration 10 steps back.
- The step selector in a variable may be a declaration name instead of a number, e.g. {jira-intake:owner}; the name form is required when one step back is ambiguous (a join).
- Template grammar: the first segment is always the step, 0 = this run: {owner}, {0:owner:x}, {1:owner}, {jira-intake:owner:}. A literal brace is escaped as {{.
- One action per declaration. Simpler; several effects are several declarations leaving the same node.
- The before/after/shadow switch is global (namespace-wide) for now.
- Budgets can be set at four levels: node, machine, declaration and alias. Any of them may carry a budget.
- Variables carry a sensitivity marking; a variable's sensitivity cannot be elevated (exposed more widely, e.g. rendered from Jira into a wider Discord audience) without the owner's approval.
- Loop backstop defaults: hop (lineage depth) limit 20, rate ceiling 30 firings per declaration per hour, both configurable on the trigger.

## Hard questions

- Is one action per declaration right, or should a declaration allow an ordered action list (e.g. land, then notify Discord) that fails as a unit? One action keeps nodes meaningful; a list reduces declaration count. (Same as q1, which was misfiled on c3.) (resolved: User: one action per declaration. Simpler; parallel effects are several declarations from one node.)
- Is one action per declaration right, or should a declaration allow an ordered action list (e.g. land, then notify Discord) that fails as a unit? One action keeps nodes meaningful; a list reduces declaration count. (resolved: Misfiled duplicate of q2 (on c2); answered there: one action per declaration.)

## Open parks

- [unknown_nonblocking] Retention and volume of per-match evaluation records (F10): every event times every matching declaration could be large; a retention policy is not designed yet (docs/operations/artifact-retention.md covers artifacts only).
- [unknown_nonblocking] Condition overlap detection (c78) can only be approximate: CEL satisfiability is not decidable in general, so some pairs will be reported as 'possibly overlapping' and some real overlaps missed when conditions call functions.
- [follow_up] Issue #327 (goals and constraints as first-class state) is related but separate: a goal and its constraints could attach to an alias, the unit that groups trigger-chains. Not designed in this cycle; the declaration model must not preclude it.

## Resolved vagueness

- [unknown_blocking] A general 'must follow' relation over events that can arrive in any order, be retried or re-minted (`trigger_remints`), or be deduped (`signal_event_watermarks`) needs defined semantics under at-least-once delivery; not decidable until Q2/Q3 are answered. — resolved: Answered by the user's confirmed c45 (must/can defined on causal lineage) and q3 (lineage ties chains); the at-least-once rules are captured as c75.
