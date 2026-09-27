# Build Plan — trigger-condition-action

slug: `trigger-condition-action` · status: `exported` · from frame: `trigger-condition-action`

> culture-nodes has one primitive: a trigger-condition-action declaration. Every workflow, sweep and loop is a set of declarations chained by 'must follow' / 'may follow' ordering; actions are agent work, chat/GitHub/Jira messages; triggers are external facts like a PR approved or a Jira issue created; a node is the stop between an action called and the trigger that captures its reaction.

## Tasks

### t1 — ADR 0014 records the PRD departures \[docs/adr/0014-\*.md — already committed in PR #329\]

- covers: c21, h15
- acceptance:
  - docs/adr/0014-trigger-condition-action-declarations.md exists on main and is cited by #328

### t2 — Measured migration inventory and consumer table \[docs/migration/tca-inventory.md, scripts/tca-inventory.sh\]

- covers: c16, h11, c67, h44, c85, h58
- acceptance:
  - scripts/tca-inventory.sh regenerates the table from git ls-files; running it twice yields no diff
  - Table lists every workflow file (examples, testdata, web goldens, schemas), every c61 item, both generator lanes, and every run-surface consumer (5 web endpoints, notifier lifecycle types, collect-handover.py, nodes-operator), each with target declaration form and a parity-status column
  - Before-state counts (workflow files, node kinds, missing PR-approved trigger and GitHub actor) are printed by the script, not typed

### t3 — Declaration schema and model \[schemas/declaration/declaration.schema.json, internal/decl/model.go, internal/decl/parse.go, internal/decl/digest.go\]

- covers: c2, h1
- acceptance:
  - A declaration has exactly one name, trigger, condition (default 'true'), action, start node and landing node; two triggers or two actions are refused with a hint naming the fan-out form
  - Nodes are declared with a deadline or explicit 'none'; a node without either is refused
  - YAML and JSON forms canonicalize to the same digest via internal/contracts CanonicalJSON; golden fixtures under internal/decl/testdata
  - Trigger carries re-entry limit (default 3, 0 allowed), hop limit (default 20) and rate ceiling (default 30/h), each overridable

### t4 — Template grammar parser and renderer \[internal/decl/template/\]

- covers: c26, c59, h35
- acceptance:
  - Table-driven tests: {owner}, {0:owner:x}, {1:owner}, {jira-intake:owner:}, {10:x:} and {{ escape each parse to the documented AST
  - Rendering: value, else default, else placeholder verbatim; {N:x:} renders empty when step N is absent
  - The parser exposes every reference (step, name, default present) so publish can warn

### t5 — Action and trigger kind vocabulary with produces/consumes signatures \[internal/decl/kinds/\]

- covers: c44, h31
- acceptance:
  - Closed, versioned vocabulary including agent.work, discord.post, github.comment, github.`review_reply`, jira.comment, jira.transition, jira.create, code.run, human.ask; triggers github.pr.approved, github.pr.created, jira.issue.created, jira.comment, human.decision, node.expired, action.failed, action.`timed_out`, action.rejected, action.`capacity_exhausted`, timer, declaration.proposed, declaration.overlap
  - A test fails if any registered kind lacks a produces/consumes signature

### t6 — Declaration store: versions, links, aliases, activation records \[migrations/`0059_declarations`.sql, internal/store/postgres/declstore.go\]

- depends on: t3
- covers: c23, h17, c38, h26, c39, h27
- acceptance:
  - Names unique per namespace; publishing an existing name creates a new version with a new digest and keeps old versions fetchable
  - must/can links stored between declarations; alias rows are movable and nestable; a move or nest that creates a cycle is refused naming the cycle (three-alias cycle test)
  - Activation, deactivation and alias moves are append-only rows naming the principal or activating declaration and the target version; history is reconstructable from rows alone

### t7 — Firing, lineage, node and evaluation store \[migrations/`0060_firings`.sql, internal/store/postgres/firingstore.go\]

- depends on: t3
- covers: c75
- acceptance:
  - Tables for firings (with trigger/condition/action version digests), lineage edges, minted markers with recorded artifact ids, nodes (state, deadline, frozen events), and per-match evaluation records
  - A redelivered event id produces no second firing row (reuses `signal_event_watermarks` semantics); a re-minted firing keeps its lineage id

### t8 — Signed origin markers and lineage resolution \[internal/declengine/marker.go\]

- depends on: t7
- covers: c79, h52, c40, h28
- acceptance:
  - Mint: MAC over (firing id, artifact kind, nonce) with a control-plane key; the created artifact id from the action result is recorded against the firing
  - Verify: an event inherits lineage only when the MAC verifies AND its artifact id equals the recorded one (and, where reported, the author is the bridge account)
  - Tests: valid marker inherits; marker copied to another PR, tampered firing id, unsigned legacy marker each start a fresh lineage and write a 'marker rejected' record; an unmarked event never inherits a guessed lineage

### t9 — Declaration engine core firing loop \[internal/declengine/engine.go, match.go, dispatch.go\]

- depends on: t6, t7, t8, t4, t5
- covers: c3, h36, c18, h13, c17, h12, h20, h41
- acceptance:
  - Event -> matching active declarations -> lineage check -> CEL condition -> dispatch via existing worker/actor/runner paths -> node opened on the landing node; each step recorded
  - 'must appear after A' fires only when lineage contains a firing of A; 'can' fires either way and exposes A's variables only when present (engine tests for both)
  - Each firing records declaration id and version digests of trigger, condition and action; upgrading mid-chain yields two firings with different digests
  - Agent-actor results are capped at proposed; a three-declaration chain renders {1:x}, {2:y} and {decl:z} from lineage
  - Duplicate delivery fires once; a re-minted firing counts once toward re-entry

### t10 — Loop guard, rate ceiling and per-subject concurrency \[internal/declengine/guard.go, migrations/`0062_decl_concurrency`.sql\]

- depends on: t9
- covers: c42, h30, c46, h32, c84, h57
- acceptance:
  - Re-entry: N re-entries allowed (N+1 appearances), default 3, 0 = no re-entry; hop limit 20; no self-retrigger unless opted in; 30 firings/declaration/hour by default — one engine test per limit, each asserting a visible stop record
  - Per-subject cap K: a burst of N events on one subject yields at most K in-flight firings, the rest deferred per internal/store/postgres/subjectconcurrency.go semantics

### t11 — Budgets at node, machine, declaration and alias level \[internal/declengine/budget.go, migrations/`0063_decl_budgets`.sql\]

- depends on: t9
- acceptance:
  - A firing checks every budget that applies to it (its node, the target machine, the declaration, every alias containing it); any exhausted budget blocks dispatch
  - A blocked dispatch emits action.`budget_exhausted` linked to the node and writes a visible record; ADR 0011 budget semantics are preserved for migrated workflows

### t12 — Node lifecycle: deadlines, technical-result triggers, orphan by upgrade \[internal/declengine/nodes.go\]

- depends on: t9
- covers: c7, h40, c82, h55, c83, h56, c49, h33
- acceptance:
  - A node's open/closed state and reason are queryable for each closing path (consumed, expired, orphaned, loop-limited)
  - Expiry emits node.expired exactly once under a fake scheduler clock; a late reaction after expiry is recorded, not fired
  - Each non-success actor/runner status produces exactly one action.\* trigger linked to the node
  - A node whose reacting declaration was removed or changed so it no longer matches closes with 'orphaned by upgrade' naming both versions

### t13 — Why-did/didn't-it-fire evaluation records \[internal/declengine/explain.go\]

- depends on: t9
- covers: c88, h61
- acceptance:
  - Every trigger match records its outcome (fired, condition false, lineage missing, loop-limited, deferred, overlap-suppressed, shadow, budget-blocked) with a reason
  - An API test per outcome kind returns the evaluation for a given event id and declaration name

### t14 — Overlap detector \[internal/declengine/overlap.go\]

- depends on: t9
- covers: c78, h42
- acceptance:
  - Two declarations on one node, same trigger, conditions priority=='High' and priority!='Low' are reported; priority=='High' and priority=='Low' are not
  - Pairs the analysis cannot decide are reported as 'possibly overlapping'; a declaration.overlap event reaches the log when configured

### t15 — Activation by declaration with a human root of trust \[internal/declengine/activation.go\]

- depends on: t9
- covers: c33, h64, c89, h62
- acceptance:
  - Only a human principal's activate call makes an activation declaration active; an agent-authored activation declaration stays inactive under an 'activate everything' rule
  - An activation declaration whose action targets another activation declaration is refused at publish
  - The recorded author is the authenticated request principal; a body claiming a different author is ignored

### t16 — Shadow mode and the global engine switch \[internal/declengine/switch.go, migrations/`0061_engine_switch`.sql\]

- depends on: t9
- covers: c80, h53
- acceptance:
  - Switch values before/shadow/after, global per namespace, every flip recorded
  - In shadow the declaration engine dispatches zero actions and derives lineage from the graph run that handled each event; would-fire records reference that graph step

### t17 — Drain graph runs across the flip to 'after' \[internal/declengine/drain.go, internal/engine hook\]

- depends on: t16
- covers: c94, h63
- acceptance:
  - After the flip new events never start graph runs; graph runs already open complete on the graph engine including awaited callbacks
  - The open graph-run count is exposed via the API and reaches zero in the test

### t18 — Rollback freeze and replay \[internal/declengine/freeze.go\]

- depends on: t16, t12
- covers: c81, h54
- acceptance:
  - Flipping back freezes open declaration nodes with a record; events arriving for a frozen node are stored and its deadline is paused
  - Flip forward replays stored events in arrival order and fires once; a redelivery of the same event id fires nothing

### t19 — Declaration API routes \[internal/api/declarations.go, api/openapi/openapi.yaml\]

- depends on: t6, t4, t15
- covers: c27, h21
- acceptance:
  - Routes for declare, validate, link (must/can), alias (create, move, nest), activate, deactivate, show, list — each with an OpenAPI entry
  - Publish returns warnings (never errors) for every template reference that may be missing and has no default: crosses only 'can' links or step N may not exist; a reference with a default produces none

### t20 — Graph, focus-distance and link-suggestion API \[internal/api/declgraph.go\]

- depends on: t19
- covers: c25, h19, c41, h29
- acceptance:
  - GET focus returns exactly the declarations within N hops in both directions across must and can links, filterable by direction and link type; nodes are not hops (fixture graph, N=0,1,2 and each filter)
  - Suggestions list candidate predecessors/successors whose produces/consumes types match; no link is created without an explicit link call
  - One response shape serves single-declaration and chain views

### t21 — CLI verbs for declarations and chains \[`culture_nodes`/cli/`_commands`/decl.py, chain.py, `culture_nodes`/explain/catalog.py entries\]

- depends on: t20
- covers: c30, h22, c31, h23, c32, h24, c66, h43
- acceptance:
  - decl declare/validate/link/show/list/focus --distance N/activate and chain alias/show, each with --json, each a thin API client (handler tests mock the client only)
  - API/CLI parity test enumerates routes against verbs; every verb driven once as a human session and once as an agent token
  - chain show and focus accept an alias name; teken cli doctor --strict stays green

### t22 — Declarations view: single, chain graph, focus with distance ring \[web/src/routes/Declarations.tsx, web/src/domain/declgraph.ts\]

- depends on: t20
- covers: c24, h18
- acceptance:
  - Renders one declaration or a chain from the focus API response using the existing ELK layout hook
  - A distance control expands N and filters by direction/link type; component tests cover N=0,1,2

### t23 — GitHub webhook receiver emitting github.pr.approved and github.pr.created \[internal/api/githubwebhook.go\]

- depends on: t5
- covers: c12, h9
- acceptance:
  - Signature verified; an unsigned or bad-signature payload is refused and logged
  - A review-approved payload delivers github.pr.approved exactly once; redelivery is deduplicated

### t24 — Poller fallback emits github.pr.approved from reviewDecision \[examples/pr-upkeep/`pr_upkeep_emit.py`, sweep.py\]

- depends on: t23
- covers: c6, h39
- acceptance:
  - The poller emits github.pr.approved with the same name and payload shape as the webhook; a test compares the two shapes

### t25 — jira.issue.created and neutral jira.\* event names \[internal/api/jirawebhook.go, examples/pr-upkeep/`pr_upkeep_jira.py`\]

- depends on: t5
- covers: c13, h10
- acceptance:
  - Webhook and JQL poller each emit jira.issue.created with one payload shape; a created issue yields exactly one event across both sources
  - pr-upkeep.jira.\* names are replaced by jira.\* with a compatibility alias until the graph engine retires

### t26 — GitHub messaging actor \[adapters/github/\]

- depends on: t5
- covers: c11, h8
- acceptance:
  - Verbs `post_comment` and `reply_to_review_thread` with a repo allowlist; results carry proposed records; ports the calls from examples/land/`land_reply.py`
  - Shared bridge modules are byte-identical to the other adapters (tests/lint guards pass); zero runtime dependencies

### t27 — Stamping in every bridge plus the stamping capability \[adapters/\*/src/\*/stamping.py (byte-identical), adapters/\*/capabilities.py\]

- depends on: t8, t26
- covers: c87, h60
- acceptance:
  - Every artifact-creating verb in codex, claude-code, pi, qwen, colleague, jira, notify, github and human-inbox stamps the minted marker and returns the created artifact id
  - Each bridge advertises the stamping capability with its revision on /v1/capabilities; the engine refuses a stamping-required dispatch to a bridge that does not advertise it, naming bridge and revision
  - tests/lint byte-identity guard covers stamping.py across all adapters

### t28 — Server-side sandbox runner for code actions \[internal/runners/local/, cmd/nodes-runner-local/\]

- covers: c22, h16, c58, h34, c19, h14
- acceptance:
  - bash, python and js run in a separate process with a timeout; a hanging script is killed at its timeout while API health stays green
  - Registered like remote runners; a declaration chooses server sandbox or a registered runner
  - An import guard proves control-plane packages have no exec path outside the runner client

### t29 — Action kind conformance through the real registry \[tests/conformance/`declactions_test.go`\]

- depends on: t9, t26, t28, t27
- covers: c5, h38
- acceptance:
  - One contract test per action kind (agent work, Discord, GitHub, Jira comment/transition/create, code run) dispatching through the real registry

### t30 — Variable sensitivity and owner-approved widening \[internal/decl/sensitivity.go, internal/declengine/sensitivity.go\]

- depends on: t9, t19
- acceptance:
  - Variables carry a sensitivity mark from their source kind; rendering one into a system with a wider audience is blocked until the owner approves
  - Approval is a human-inbox task to the variable's owner; approval and refusal are append-only records; publish warns on every widening reference

### t31 — Migrate pr-upkeep and jira-intake to declarations \[examples/pr-upkeep/declarations/, examples/jira-intake/declarations/\]

- depends on: t2, t19, t10, t12
- covers: c4, h37
- acceptance:
  - Every node, edge, trigger, affinity rule and subject cap of both workflows has a declaration form listed in the inventory table
  - Both sets validate and publish; the inventory parity column is filled for both

### t32 — Shadow parity comparison on real traffic \[scripts/tca-parity.py\]

- depends on: t31, t16
- covers: c70, h47
- acceptance:
  - Over a recorded window with the switch on shadow, every shadow firing for pr-upkeep and jira-intake matches the graph step for the same event (outcome, rendered variables, ledger kinds)
  - The diff is written as an observed evidence record with run ids for both engines

### t33 — Migrate remaining examples and c61 configuration \[examples/\*/declarations/, schedules, notifier, repair, hand-turn, affinity\]

- depends on: t32
- acceptance:
  - Every remaining row of the inventory has a published declaration form and a parity status
  - The notifier's five lifecycle posts and repair routing run as declarations in shadow

### t34 — Declaration-era read paths for run-surface consumers \[internal/api/runscompat.go, internal/notifier, scripts/collect-handover.py\]

- depends on: t9, t2
- acceptance:
  - Web endpoints /runs, /node-runs, /human-tasks, /pending-decisions and /tickets serve lineage/firing data with their existing tests passing
  - The declaration engine emits the notifier's lifecycle event types; collect-handover.py reads firings

### t35 — Workflow generation and devague plan import emit declarations \[internal/api/`workflow_generations.go`, cmd/nodes/planimport.go\]

- depends on: t19
- covers: c86, h59
- acceptance:
  - Both lanes emit declarations that validate and publish; their existing tests are ported to declaration output

### t36 — Live success signals: causal context, loop stops, agent control \[docs/deliveries evidence\]

- depends on: t32, t21, t27, t15
- covers: c71, h48, c72, h49, c73, h50
- acceptance:
  - Jira issue -> intake PR -> 'PR spec created' renders the Jira key from verified lineage on real systems; a human-opened PR renders by the template rule (run ids cited)
  - A ping-pong pair stops at re-entry 3 and a 0-limit trigger stops at first re-entry, each with a queryable record
  - An agent declares, links, validates and is activated through CLI --json with an agent credential; focus --distance 2 shows it

### t37 — Flip to after, drain, and retire the graph engine \[internal/engine, internal/compiler graph parts, examples graph files, tests/lint guards\]

- depends on: t36, t33, t34, t35, t17, t18
- covers: c74, h51, c1, h2, c68, h45, c69, h46
- acceptance:
  - Switch flipped to after with recorded parity; open graph-run count reaches zero before deletion
  - Graph engine packages, node kinds and graph example files are deleted; tests/lint example guards glob declaration files; all suites pass
  - One new 'when X, if Y, do Z' behavior is added as exactly one declaration with no engine or schema change (digest cited)
  - The delivery summary maps each after-state element to its success-signal evidence; nothing fires that is not a declaration

## Risks

- [unknown_nonblocking] Shared files touched by several tasks: migrations/README.md (one row per migration), `culture_nodes`/explain/catalog.py, api/openapi/openapi.yaml and web/src/App.tsx routes. Same-wave tasks append to them; the merger reconciles these rows at each merge gate rather than splitting the tasks.
- [unknown_nonblocking] Codex write path is unproven (#18): workforce routing should send build packages to lanes that have landed writes (claude bridges, local subagents) and use codex for analysis and review until #18 closes.
- [unknown_nonblocking] Lineage lookup cost at real event rates was not examined in the challenge pass; t9 (core) adds a benchmark over a synthetic 10k-firing lineage table before t32 (parity) runs on real traffic. (task t9)
- [unknown_nonblocking] The GitHub webhook needs a public, authenticated ingress (nodes.culture.dev tunnel route + GitHub webhook secret grant). That is an operator hand-turn and must be filed as an issue when done. (task t23)
- [unknown_nonblocking] Overlap detection is approximate: CEL satisfiability is undecidable in general, so some real overlaps can be missed (parked v on the frame). (task t14)
- [follow_up] Retention of per-match evaluation records (t13) is undesigned; volume grows as events times matching declarations. (task t13)
- [follow_up] Helm and AWS deploy paths (deploy/helm, deploy/aws) were not examined; they may reference the graph engine, runs, or migrations. (task t37)
- [unknown_nonblocking] Live success signals (t36) and parity on real traffic (t32) consume model sessions from the shared subscription window; declare expected session counts per wave in the split plan (issue #48). (task t36)
- [unknown_nonblocking] Owner-approval mechanism for sensitivity widening (t30) is proposed as a human-inbox task; the owner may prefer approval through an activation-style declaration. (task t30)
- [unknown_nonblocking] Budget precedence (t11) is proposed as 'every applicable budget must have headroom'; the owner may want an override order instead. (task t11)
