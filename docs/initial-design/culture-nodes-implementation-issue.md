# Implement Culture Nodes Phase 0/1: graph runtime, work ledger, and headspace code node

## Announcement

Build **Culture Nodes**, a durable, graph-native workflow orchestrator where agents, code, services, policies, and people are nodes with explicit contracts and owners.

The first vertical slice must prove three things together:

1. a versioned graph can run durably through external actors;
2. agent work becomes typed, reviewable, and mechanically verifiable through a Devague-derived work ledger;
3. code can run through headspace-cli in a disposable Docker boundary and return structured evidence without exposing the control plane to arbitrary execution.

> **Every node has a contract. Every result has evidence.**

## Product decisions

- Product: **Culture Nodes**
- Surface: `nodes.culture.dev`
- Repository: `agentculture/culture-nodes`
- CLI: `nodes`
- API group: `nodes.culture.dev/v1alpha1`
- Visual standard: the latest Culture.dev and AgentCulture graph system
- Runtime: Go
- UI: TypeScript, React, Vite, React Flow
- Durable state: PostgreSQL
- Local queue: PostgreSQL leases and `SKIP LOCKED`
- AWS queue: SQS as a work signal; PostgreSQL remains authoritative
- Contracts: JSON Schema Draft 2020-12
- Conditions: CEL
- Canonical definitions and ledgers: JSON
- Friendly authoring: YAML or JSON
- Code runner: headspace-cli Docker adapter

The previous honeycomb, cell, and bee metaphor is out of scope. Nodes are simply nodes, and the graph must remain freeform, fluid, groupable, and reusable.

## Product model

| Concept | Meaning |
| --- | --- |
| Workflow | Immutable, versioned directed graph |
| Node | Typed unit of work or control |
| Edge | Eligible transition |
| Actor | Agent, service, runner, or human identity |
| Run | One execution of a workflow version |
| Token | Unit of control moving through the graph |
| Node run | One logical execution of a node |
| Attempt | One dispatch attempt |
| Contract | Input, output, error, and ledger boundary |
| Work ledger | Typed authoritative work state |
| Evidence | Attributable observation with explicit coverage |

Actor identity, graph token, node run, and attempt are separate.

## First reference workflow

~~~mermaid
flowchart LR
    I["Intake agent"] --> P["Plan agent"]
    P --> B["Build agent"]
    B --> T["Test in headspace"]
    T --> V["Verify agent"]
    V --> F["Finish"]
    T -->|failed| B
    V -->|changes required| B
    V -->|blocked| H["Human review"]
    H --> B
~~~

Expected behavior:

- intake proposes scope, claims, assumptions, and questions;
- plan proposes tasks and mechanical success signals;
- build claims a task and proposes a result;
- headspace runs the pinned test operation and appends runner-observed evidence;
- acceptance logic verifies or rejects the result;
- verify returns a domain outcome such as `passed`, `changes_required`, or `blocked`;
- expected negative outcomes follow graph edges rather than masquerading as runtime failure;
- loops are bounded by transition, node-visit, time, parallelism, token, and optional cost budgets.

## Work ledger

The work ledger is not a chat transcript and not the runtime event stream. It is durable, machine-readable project state.

MVP record types:

- `announcement`;
- `claim`;
- `assumption`;
- `question`;
- `task`;
- `decision`;
- `success_signal`;
- `evidence`;
- `result`;
- `review`.

Every record includes:

- stable ID;
- schema version;
- run, node-run, and attempt IDs;
- producer kind and identity;
- authority;
- typed data;
- provenance references;
- optional superseded-record reference;
- timestamp;
- content digest.

Authority values:

| Authority | Who may create it |
| --- | --- |
| `proposed` | Agent, service, or human |
| `confirmed` | Authorized human or explicit policy gate |
| `observed` | Trusted runner or tool, limited to directly measured fields |
| `derived` | Deterministic engine or validator from referenced records |
| `rejected` | Authorized reviewer or validator |
| `superseded` | Runtime projection of an appended replacement |

Rules:

- an agent may not confirm its own proposal;
- an agent saying “done” creates a completion claim, not verified evidence;
- records are immutable; correction appends a replacement;
- review batches are transactional;
- reviews include the ledger version or frame checksum and reject stale state;
- Markdown is generated from JSON projections and is not authoritative;
- empty review sets still return valid machine-readable JSON;
- stdout and file-write behavior are part of CLI contracts.

Task execution status and assurance are separate.

Execution status:

- proposed;
- ready;
- claimed;
- running;
- blocked;
- completed;
- failed;
- cancelled.

Assurance:

- unverified;
- evidence_attached;
- verified;
- rejected.

## Node ledger contract

Every node declares:

- ledger projections it may read;
- record types it may propose;
- fields it may set;
- required provenance;
- maximum record and payload counts;
- required human review;
- evidence and success signals required for each outcome.

An actor returns a proposed ledger delta. The runtime validates schema, permissions, authority, references, and limits before appending it.

## Devague integration

Use Devague as both a design source and a deterministic integration.

- proposed versus confirmed claims map directly to ledger authority;
- scope, frame, challenge, plan, and delivery artifacts map to ledger projections;
- review maps to an atomic, stale-protected transaction;
- agents choose proposed next moves but never invent confirmation;
- challenge findings, blockers, warnings, parked items, and required next moves remain typed;
- a Devague conformance fixture must round-trip to the same ledger projection and digest;
- the actual Devague CLI can later run as a code node through headspace.

Do not copy Devague internals into the scheduler. Integrate through schemas and a conformance adapter.

## headspace-cli code node

The execution boundary is:

~~~mermaid
flowchart LR
    O["Typed operation"] --> P["Enforced policy"]
    P --> H["Docker headspace"]
    H --> R["Structured result"]
    H --> E["Observed evidence"]
~~~

The node submits:

- operation ID and attempt idempotency key;
- headspace runner revision;
- immutable workspace/artifact reference and digest;
- pinned container image digest;
- command argv, not an implicit shell string;
- working directory and explicit environment references;
- CPU, memory, process, disk, and time limits;
- network policy;
- allowed output paths;
- requested observations.

Safe defaults:

- no network;
- non-root user;
- read-only container root;
- copy-on-write workspace;
- no host home;
- no implicit host environment;
- no secrets unless individually granted;
- bounded logs with complete log artifacts;
- no Docker socket in the `nodes` API, scheduler, or worker.

The headspace adapter returns:

- operation state;
- exit code and signal;
- start, end, and duration;
- runner, image, input, and policy digests;
- before/after workspace snapshot digests;
- changed paths and a truthful completeness flag;
- diff, stdout, stderr, and output-workspace artifact references;
- resource measurements the runner directly observed.

Trust rule:

> Docker provides isolation, not truth.

The runner may create `observed` evidence only for facts it measured. Text printed inside the container remains process-reported content. A test-report parser may create `derived` evidence when its parser revision, input report, and coverage are recorded.

Every result should be replayable from an emitted manifest containing the pinned input, image, runner, argv, declared environment, policy, and expected output contract.

## Runtime semantics

PostgreSQL is authoritative for:

- workflow versions;
- runs and graph tokens;
- node runs and attempts;
- work leases and timers;
- ledger records and review transactions;
- artifacts;
- idempotency;
- runtime events;
- transactional outbox.

SQS carries disposable work references only.

Workers claim work with:

- atomic state transition;
- lease owner and expiry;
- attempt number;
- monotonic fencing token.

When an attempt completes, one transaction:

1. verifies current state and fencing;
2. validates the output contract;
3. validates the ledger delta and producer authority;
4. appends accepted ledger records and evidence references;
5. records the result;
6. selects eligible edges;
7. creates the next token/node run;
8. appends runtime events and outbox records;
9. commits.

External side effects remain at-least-once and require idempotency or reconciliation.

Long-running actors transition to durable `waiting_external` state and release worker capacity. Callback events are idempotent and fenced.

## Visual implementation

Use the latest AgentCulture graph design as the source of truth.

Before implementing the UI:

1. identify the current tokens, typography, graph nodes, edges, cards, controls, and interaction states;
2. reuse the shared implementation when available;
3. otherwise extract a versioned shared layer instead of copying by eye;
4. pin its source revision in an ADR and visual regression fixture.

The first UI is read-only:

- live graph Run view;
- node and edge execution state from committed events;
- node detail with contract, owner, actor/runner, attempt, ledger delta, and evidence;
- Ledger view with projections, provenance, reviews, and verification;
- list and timeline alternatives;
- keyboard support and reduced motion.

The drag-and-drop editor is a later phase.

## Repository structure

Core packages:

- `internal/compiler`;
- `internal/contracts`;
- `internal/ledger`;
- `internal/engine`;
- `internal/scheduler`;
- `internal/worker`;
- `internal/actors`;
- `internal/runners/headspace`;
- `internal/queue/postgres`;
- `internal/queue/sqs`;
- `internal/store/postgres`;
- `internal/artifacts`;
- `internal/events`;
- `internal/policy`;
- `internal/telemetry`;
- `web/src/culture-design`;
- `schemas/workflow`;
- `schemas/ledger`;
- `schemas/runner`;
- `tests/conformance`;
- `tests/ledger`;
- `tests/headspace`;
- `tests/fault`.

Ship one Go binary with `serve`, `scheduler`, `worker`, `all`, `validate`, `run`, and `inspect` modes.

## Milestone 0 — Contracts and compiler

- [ ] Workflow YAML/JSON schema.
- [ ] Normalized canonical JSON IR and digest.
- [ ] Node, edge, owner, actor, runner, contract, and policy validation.
- [ ] JSON Schema 2020-12 contracts.
- [ ] CEL conditions.
- [ ] Ledger record schemas and producer/authority matrix.
- [ ] Deterministic ledger projections.
- [ ] Atomic stale-protected review schema.
- [ ] Devague mapping fixtures.
- [ ] headspace operation/result/evidence schemas.
- [ ] `nodes validate` with precise diagnostics.
- [ ] Delivery-loop example compiles deterministically.

## Milestone 1 — Durable vertical slice

- [ ] PostgreSQL schema and migrations.
- [ ] Sequential durable engine with bounded loops.
- [ ] Sync and async HTTP actors.
- [ ] Ledger append, supersession, projection, and review.
- [ ] headspace-cli Docker runner adapter.
- [ ] Contract and mechanical success-signal validation.
- [ ] Retries, timeouts, cancellation, leases, and fencing.
- [ ] Runtime events and transactional outbox.
- [ ] API and CLI run/inspect flow.
- [ ] Read-only AgentCulture-aligned graph Run view.
- [ ] Ledger and evidence view.
- [ ] Complete Docker Compose profile.

## Acceptance criteria

- [ ] A run pins an immutable workflow digest.
- [ ] Every compiled node has an explicit owner.
- [ ] Every actor, runner, component, image, schema, and policy reference is pinned.
- [ ] Intake → plan → build → test → verify runs end to end.
- [ ] `changes_required` and failed tests loop to build without becoming engine failures.
- [ ] Agent-origin records remain proposed until authorized.
- [ ] Task completion and verification are separate.
- [ ] Stale review transactions fail atomically.
- [ ] Markdown is a generated reflection of JSON state.
- [ ] headspace runs the test in a disposable Docker boundary.
- [ ] The `nodes` control-plane containers have no Docker socket.
- [ ] headspace returns environment, exit, snapshot, diff, and artifact evidence.
- [ ] Container output cannot grant itself `observed` authority.
- [ ] Required success signals mechanically verify or reject the task.
- [ ] Run and ledger state survive process restart.
- [ ] Long-running actors do not hold worker leases.
- [ ] Duplicate callbacks and runner completions are harmless.
- [ ] Two workers cannot commit the same current transition.
- [ ] Every committed transition emits a runtime event.
- [ ] The graph and ledger UI derive only from committed state.
- [ ] The UI uses a pinned current AgentCulture design revision.
- [ ] Fault-injection tests cover failure before dispatch, after dispatch, before commit, after commit, during callback, and during headspace completion.
- [ ] Idle-memory, ledger-projection, transition-throughput, and headspace conformance benchmarks are recorded.

## Not in this issue

- graphical workflow editing;
- parallel, join, foreach, or subworkflow execution;
- arbitrary in-process scripts;
- additional container or cloud runner backends;
- WASI transforms;
- public registry;
- exactly-once external effects;
- agent memory;
- provider-specific agent behavior.

## Companion specification

The full PRD and technical specification is in `culture-nodes-prd-spec.md`.
