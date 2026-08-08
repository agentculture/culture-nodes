# Culture Nodes

## Agent-native workflow orchestration with contracts, ledgers, and evidence

**Status:** Proposed  
**Target repository:** `agentculture/culture-nodes`  
**Product surface:** `nodes.culture.dev`  
**CLI:** `nodes`  
**API group:** `nodes.culture.dev/v1alpha1`

> **Every node has a contract. Every result has evidence.**

---

## 1. Decision summary

Build the product as **Culture Nodes**, hosted at **`nodes.culture.dev`**.

Use the latest **Culture.dev and AgentCulture graph system** as the product's visual and interaction standard:

- every executable step is a node;
- workflows are freeform directed graphs;
- nodes can be grouped, nested, branched, looped, and customized without a geometric metaphor;
- workflow, ecosystem, ownership, dependency, and execution views share the same graph language;
- the implementation must reuse or extract the current Culture/AgentCulture design tokens and graph components instead of approximating them.

The honeycomb, cell, and bee vocabulary is intentionally removed.

Make a **Devague-derived work ledger** a core runtime primitive. Agents do not merely return prose; they propose typed claims, tasks, decisions, results, and evidence. Authority, provenance, review state, and verification are explicit.

Use **headspace-cli as the first code-runner integration**. Code nodes execute typed operations in disposable Docker headspaces and return structured results plus runner-observed evidence.

### Recommended stack

| Area | Decision |
| --- | --- |
| Control-plane runtime | Go |
| Web application | TypeScript, React, Vite |
| Graph canvas | React Flow using the shared Culture/AgentCulture graph components |
| Live execution | Node and edge state overlays derived from committed runtime events |
| UI system | Latest Culture/AgentCulture design tokens and components; Tailwind CSS and Radix only where the shared system uses or permits them |
| Automatic graph layout | ELK.js |
| API | REST/JSON described by OpenAPI 3.1 |
| Live UI updates | Server-Sent Events initially |
| Authoring | YAML or JSON |
| Canonical definition | Normalized JSON intermediate representation, addressed by SHA-256 digest |
| Contracts | JSON Schema Draft 2020-12 |
| Conditions | CEL |
| Durable state | PostgreSQL |
| Local work queue | PostgreSQL work table using leases and `SKIP LOCKED` |
| AWS queue | SQS as a delivery signal; PostgreSQL remains authoritative |
| Large artifacts | S3-compatible object storage |
| Work ledger | Append-only typed JSON records with provenance, authority, and evidence |
| Code runner | headspace-cli Docker adapter |
| Events | CloudEvents 1.0-compatible envelopes |
| Telemetry | OpenTelemetry |
| Local packaging | One binary plus PostgreSQL; Docker Compose |
| AWS deployment | ECS/Fargate first, EKS-compatible |
| Arbitrary code execution | External isolated runner; never inside the control-plane process |

### Product identity

The product is **Culture Nodes**. The canonical surface is **`nodes.culture.dev`**.

“Nodes” is both the product name and the correct technical abstraction:

- an agent invocation is a node;
- a code operation is a node;
- a policy, approval, transformation, wait, or subworkflow is a node;
- the graph remains fluid rather than being constrained to a visual tiling;
- the product naturally belongs beside Culture's mesh and AgentCulture's ecosystem graph.

The product definition is:

> **Culture Nodes is a durable, ledger-native workflow orchestrator for agents, code, services, and people.**

The brand line is:

> **Every node has a contract. Every result has evidence.**

---

## 2. Product thesis

Current workflow products primarily move payloads through functions. Agent frameworks primarily coordinate model calls inside a particular SDK or provider.

Culture Nodes occupies the layer between them:

- a durable workflow graph determines what work may happen;
- typed contracts determine what may cross each boundary;
- actors perform work through provider-neutral adapters;
- ownership and policy determine who is responsible and what is permitted;
- a typed work ledger distinguishes proposals, confirmations, observations, and verified results;
- code runs through a bounded, inspectable execution environment;
- an append-only execution history shows exactly what occurred;
- agents remain independently hosted and independently evolvable.

The workflow owns orchestration state. The actor owns its private runtime state. Memory remains external and is passed by reference.

Culture Nodes does not make nondeterministic actors deterministic. It makes their **placement, permissions, inputs, outputs, claims, evidence, transitions, retries, and accountability explicit**.

---

## 3. Core mental model

### 3.1 Canonical vocabulary

| Term | Meaning |
| --- | --- |
| Workflow | An immutable, versioned directed graph definition |
| Node | A typed unit of work or control |
| Edge | A possible transition between nodes |
| Actor | The identity that performs a node: agent, service, runner, or human |
| Run | One execution of a workflow version |
| Token | A unit of control moving through the graph |
| Node run | One node's logical execution within a run |
| Attempt | One dispatch attempt for a node run |
| Contract | Machine-verifiable input, output, error, and ledger boundary |
| Work ledger | Append-only typed records describing intended, claimed, observed, and verified work |
| Evidence | An attributable observation linked to a claim, task, or result |
| Artifact | Large or binary output stored outside the state document |

### 3.2 Graph model

A workflow is a graph without an imposed geometric metaphor.

- nodes may represent agents, code, services, decisions, policies, waits, humans, or subworkflows;
- edges represent eligible transitions;
- tokens represent active control flow;
- grouping and subgraphs express ownership or reusable units;
- layout is presentation metadata and never affects execution;
- the same graph can be rendered using Culture's current graph, list, timeline, or ownership views.

Actor identity, graph token, node run, and attempt remain separate. A run may use several agents, one agent may execute several nodes, and a parallel branch may create several tokens.

### 3.3 Ledger-native execution

~~~mermaid
flowchart TD
    L["Ledger projection"] --> N["Node contract"]
    N --> A["Actor or runner"]
    A --> D["Proposed ledger delta"]
    D --> V["Validate + append evidence"]
    V --> E["Eligible edge"]
~~~

The engine moves tokens and appends validated ledger records. It does not treat an agent's prose as authoritative state.

### 3.4 Domain outcome versus technical status

Do not use technical failure to represent an expected business outcome.

For example, a verification actor can execute successfully and return the outcome `changes_required`. The engine then follows the `changes_required` edge back to the build node.

Technical statuses include:

- succeeded;
- failed;
- timed out;
- cancelled;
- policy denied;
- contract rejected.

Domain outcomes are workflow-defined output ports such as:

- passed;
- changes_required;
- approved;
- rejected;
- blocked.

---

## 4. Users

### Platform engineer

Creates reusable nodes, actor adapters, policies, deployment templates, and observability.

### Workflow author

Composes agents, code, services, and approvals into a versioned workflow.

### Node owner

Owns a node implementation, its contracts, security posture, reliability, and upgrades.

### Operator

Inspects active runs, retries recoverable work, pauses or cancels runs, and diagnoses failures.

### Approver

Receives a bounded human task with the exact context and authority needed to decide.

### Auditor or security reviewer

Answers who ran what, under which definition and permission set, with which inputs and outputs.

---

## 5. Goals

Culture Nodes must:

1. Compose agents, deterministic code, services, waits, policies, and humans in one workflow.
2. Make every boundary contract-first and machine-verifiable.
3. Make ownership part of the compiled architecture.
4. Keep agents and large execution runtimes outside the control plane.
5. Preserve durable run state across process, worker, and host failure.
6. Support loops, waits, retries, cancellation, and eventually parallel branches.
7. Remain provider-neutral.
8. Be simple to run locally and natural to operate on AWS.
9. Support multiple stateless API and worker instances.
10. Produce an attributable event history for every run.
11. Make workflows reviewable as text and memorable as a visual system.
12. Minimize idle memory, runtime dependencies, and supply-chain surface.
13. Allow reusable, versioned nodes and workflows without coupling them to deployment infrastructure.
14. Expose resource and agent-usage data without making any provider's model schema part of the core.
15. Make tasks, claims, decisions, success signals, and evidence durable JSON records.
16. Let deterministic checks validate agent claims mechanically.
17. Execute code through headspace-cli without granting the orchestration worker a shell or Docker socket.

---

## 6. Non-goals

The first product is not:

- an LLM inference server;
- a model provider;
- an agent SDK;
- an agent memory system;
- a secrets manager;
- a Kubernetes replacement;
- a general distributed-computing framework;
- an IDE;
- a collaborative whiteboard;
- a promise of exactly-once external side effects;
- a safe place to execute arbitrary unsandboxed user code;
- a complete static proof system for all JSON Schema compatibility;
- a replacement for Claude Agent SDK, OpenAI/Codex, or company-specific agent runtimes.
- a claim that Docker alone makes untrusted code or its output truthful.

Existing systems are integrated through actors and adapters.

---

## 7. Product principles

### Contracts over assumptions

Every executable boundary has input, output, and failure contracts.

### Ownership is architecture

Every compiled production node resolves to an owner. An unowned executable node is invalid.

### Identity is not execution

Actor identity, workflow token, node run, and attempt are separate records.

### Durable state, disposable compute

No committed workflow fact exists only in worker memory.

### At-least-once delivery, idempotent state

Duplicate work signals are expected. State transitions are fenced and idempotent.

### Explicit continuation

Agent session, memory, workspace, and continuation references are passed explicitly. There is no invisible shared conversation.

### Safe configuration first

Network access, secrets, capabilities, limits, retry policy, and code isolation are declared rather than inferred.

### JSON is authoritative

YAML is a friendly authoring format. Published definitions, API records, events, and ledgers are canonical JSON.

### Proposals are not authority

An agent may propose claims, tasks, decisions, and completion. It may not silently confirm its own proposal or convert its own assertion into observed evidence.

### Evidence is scoped

Every evidence record says who or what observed it, how it was collected, what input and environment it covered, and whether it is complete. Unknown coverage remains unknown.

### Visuals project runtime truth

The graph does not invent a second workflow model. It edits and renders the same canonical graph, ledger projections, and committed execution events.

### Local claims must be earned

The system should be designed for a good local experience, but claims such as “excels remotely” are published only after benchmarks and failure tests prove them.

---

## 8. User experience and visual design

### 8.1 Source of truth

The latest AgentCulture visual system is the standard. Culture Nodes must not invent a sibling aesthetic.

Before UI implementation:

1. identify the current design tokens, typography, spacing, node treatment, edge treatment, cards, controls, and interaction states used by AgentCulture;
2. reuse them directly when a shared package exists;
3. otherwise extract them into a versioned shared layer before customizing Nodes;
4. record the exact source revision in an ADR and visual regression fixtures;
5. keep Culture Nodes-specific styles limited to workflow state and ledger semantics.

Do not copy colors or shapes by eye from the public site.

### 8.2 Graph language

Use the same visual language for:

- the AgentCulture ecosystem graph;
- Culture's agents, rooms, ownership, and relationships;
- Culture Nodes workflow definitions;
- Culture Nodes live runs;
- ledger provenance and evidence relationships.

The semantics differ, but navigation and interaction should feel native across them:

- pan, zoom, focus, filter, and search;
- expand and collapse groups;
- inspect a node without leaving graph context;
- follow ownership and provenance edges;
- switch between graph, list, and timeline projections;
- deep-link to the selected node, run, task, claim, or evidence record.

### 8.3 Node customization

Nodes are visually flexible and type-aware. The shared Culture node component supplies the frame; each node kind supplies concise content:

- agent node: actor identity and capability;
- code node: runner, image digest, and operation;
- decision node: rule and possible outcomes;
- approval node: authority and deadline;
- wait node: signal or resume time;
- subworkflow node: pinned workflow revision.

Node shape and layout are presentation. Node kind, contract, and policy remain data.

### 8.4 Live execution

Live state is shown through node and edge overlays driven only by committed runtime events:

- ready nodes are eligible but not presented as running;
- the current attempt is visibly active;
- completed paths remain inspectable;
- retry and loop counts are explicit;
- parallel branches show independent tokens or aggregated counts;
- waiting, blocked, failed, and policy-denied states are distinct;
- reduced-motion mode preserves the same information without animated transitions.

### 8.5 Progressive detail

At distant zoom:

- show topology, execution flow, failures, and ownership boundaries.

At medium zoom:

- show node name, kind, current state, actor or runner, and unresolved task count.

At close zoom:

- show ports, contract summaries, version, retry state, ledger delta, evidence, cost/usage, and owner.

### 8.6 Primary views

#### Design

Compose and validate workflow definitions.

#### Run

Watch tokens move and inspect a live or historical execution.

#### Contract view

Compare connected output and input schemas and inspect compiler diagnostics.

#### Ledger view

Inspect tasks, claims, assumptions, decisions, evidence, reviews, and their provenance.

#### Operations

Search runs, failures, waiting approvals, retries, and actor health.

#### Registry

Discover reusable nodes, actors, and subworkflows by owner, contract, capability, and version.

### 8.7 First visual slice

Do not wait for the complete drag-and-drop editor before proving alignment and runtime truth.

The first vertical slice includes a **read-only live graph Run view**:

- a small workflow rendered with the current AgentCulture graph components;
- an active path through intake, plan, build, test, and verify;
- a visible loop from verify back to build;
- a headspace code node showing the pinned operation and observed test evidence;
- node detail showing actor or runner, contract, owner, attempt, and ledger delta;
- live updates from the event stream.

The graphical editor comes later.

### 8.8 Accessibility

The UI must provide:

- complete keyboard navigation;
- a non-graph run timeline containing the same information;
- visible focus states;
- status labels and icons in addition to color;
- reduced-motion support;
- screen-reader names for nodes, edges, actors, ledger records, evidence, and statuses;
- zoom-independent readable detail panels.

---

## 9. Functional requirements

### 9.1 Workflow definitions

A workflow definition contains:

- metadata;
- an explicit owner;
- typed input and output contracts;
- immutable nodes and edges;
- execution limits;
- policy references;
- reusable component references;
- presentation metadata that never changes runtime semantics.

Drafts are mutable. Published versions are immutable and content-addressed.

A run always pins one published workflow digest.

### 9.2 Node kinds

#### MVP

- `agent` — invoke an external agent actor;
- `code` — submit a typed operation to headspace-cli's Docker runner;
- `action.http` — invoke a deterministic HTTP service;
- `decision` — select an output port using CEL;
- `approval` — create a bounded human task and wait;
- `wait` — resume at a durable time or external signal;
- `end` — produce the workflow result.

#### Later

- `action.container` — execute a pinned OCI image through an isolated runner;
- `transform.wasm` — execute a bounded WASI transform without network by default;
- `parallel` — create several child tokens;
- `join` — wait for all, any, or quorum;
- `foreach` — execute a bounded collection;
- `subworkflow` — invoke a pinned workflow version;
- `event` — wait for a correlated external event.

Arbitrary scripts never run inside the API, scheduler, or generic worker process. A code node describes an operation; a runner executes it.

### 9.3 Contracts

Every node declares:

- input schema;
- output schema per domain outcome;
- error schema;
- ledger records it may read;
- ledger record types and authority levels it may propose;
- success signals and required evidence;
- maximum inline payload size;
- artifact types when applicable.

Use JSON Schema Draft 2020-12.

At publish time, the compiler:

1. validates each schema;
2. bundles referenced schemas;
3. resolves every reference to a digest;
4. checks data bindings;
5. proves edge compatibility where safely decidable;
6. emits a warning when compatibility cannot be proven;
7. rejects known incompatibilities.

JSON Schema is a validation language, not a complete decidable subtyping system. Runtime validation remains mandatory even after static checks pass.

### 9.4 Ownership

Every compiled node contains:

- `ownerRef`;
- source repository or component reference;
- escalation contact;
- optional on-call service;
- data classification;
- policy set.

Authoring defaults may reduce repetition, but the normalized JSON representation must contain a resolved owner for every node.

Actor ownership and node ownership are separate. A platform team may own the reusable node while another team owns the external actor.

### 9.5 Actors

An actor is a registered identity and endpoint. Actor types include:

- agent;
- service;
- isolated code runner;
- human group.

The core engine does not branch on provider names. Provider and model details are telemetry metadata reported by the adapter.

An actor manifest declares:

- protocol version;
- endpoint or runner reference;
- authentication method;
- capabilities;
- supported sync/async behavior;
- heartbeat and cancellation support;
- owner;
- data and network policy.

Runner manifests use the same actor boundary but additionally declare isolation, observation, and evidence capabilities.

### 9.6 Agent state and memory

Culture Nodes owns:

- run state;
- node-run state;
- attempts;
- timers;
- bindings;
- work-ledger records and projections;
- artifacts and references;
- transition history.

The actor owns:

- conversation state;
- local scratch state;
- tool state;
- working directory;
- model session.

Memory is external. A workflow passes `memoryRef`, `workspaceRef`, or `continuationRef` explicitly.

Node input should normally contain a bounded ledger projection rather than the full historical conversation. The contract names the record types, authority levels, fields, and time or ancestry window it needs.

### 9.7 Transitions and loops

Edges originate from a named domain outcome.

Optional CEL conditions can further constrain an edge.

Loops are first-class but bounded by workflow policy:

- maximum total transitions;
- maximum visits per node;
- maximum wall-clock duration;
- maximum concurrent tokens;
- optional agent token or cost budget.

No loop may rely solely on an agent deciding when to stop.

### 9.8 Parallelism

The design supports token splitting and joining even if MVP execution remains sequential.

Join policies include:

- all;
- any;
- quorum;
- first_success.

Cancellation of losing branches is explicit and best-effort.

### 9.9 Human work

An approval node creates a human task containing:

- decision schema;
- requested approver role or group;
- deadline;
- exact context and artifact references;
- audit identity;
- allowed outcomes.

The workflow pauses without holding a worker or database transaction.

### 9.10 Reuse and versioning

Reusable nodes and workflows are addressed by immutable digest.

Human-friendly semantic versions can resolve to a digest during publication. Production runs never resolve mutable tags at execution time.

Initial sources:

- repository-local files;
- HTTPS manifests;
- Git references pinned to commit.

Later, package reusable definitions and schemas as signed OCI artifacts instead of inventing a proprietary registry protocol.

### 9.11 Devague integration

Devague is both a design source and an integration:

- its separation of model reasoning from authoritative JSON becomes a runtime rule;
- its proposed versus confirmed claim discipline becomes ledger authority;
- its scope, frame, challenge, plan, and delivery artifacts become typed ledger projections;
- its review operation becomes an atomic human-review transaction protected by ledger version or checksum;
- its deterministic CLI can run as a code node through headspace when a workflow needs the actual repository tool.

Culture Nodes should not copy Devague's implementation into the scheduler. It should define a ledger contract that Devague can produce and consume through a conformance adapter.

---

## 10. Agent-native work ledger

### 10.1 Purpose

The work ledger is the durable, machine-readable account of:

- what was requested;
- what is claimed;
- what remains uncertain;
- what work was proposed and authorized;
- what actor performed it;
- what result was claimed;
- what evidence was observed;
- what was reviewed, confirmed, rejected, or superseded.

It is not a transcript and not a log dump. It is structured project state.

The runtime event log answers **what the orchestrator did**. The work ledger answers **what the work currently means and what supports it**. The two are linked but not conflated.

### 10.2 Record types

MVP record types:

| Type | Purpose |
| --- | --- |
| `announcement` | A concise goal, scope, or intended outcome |
| `claim` | A proposition whose authority and provenance are explicit |
| `assumption` | A premise allowed temporarily but not confirmed |
| `question` | An unresolved item required or parked for later |
| `task` | A bounded unit of work with dependencies and acceptance conditions |
| `decision` | A selected option and the authority that selected it |
| `success_signal` | A mechanically inspectable condition for acceptance |
| `evidence` | An attributable observation with declared coverage |
| `result` | A node or task result linked to claims and evidence |
| `review` | A confirmation, rejection, or requested revision |

Additional domain-specific record types can be registered by schema, but cannot redefine core authority semantics.

### 10.3 Common envelope

~~~json
{
  "id": "ledger_01J...",
  "schema_version": "nodes.culture.dev/ledger/v1alpha1",
  "record_type": "claim",
  "run_id": "run_01J...",
  "node_run_id": "nr_01J...",
  "attempt_id": "att_01J...",
  "origin": {
    "kind": "agent",
    "actor_id": "actor_planner_v3"
  },
  "authority": "proposed",
  "subject_ref": "task_01J...",
  "data": {},
  "provenance_refs": [],
  "supersedes": null,
  "created_at": "2026-08-08T15:00:00Z",
  "content_digest": "sha256:..."
}
~~~

Ledger records are immutable. Corrections append a new record with `supersedes`.

### 10.4 Authority

Core authority values:

| Authority | Meaning |
| --- | --- |
| `proposed` | Suggested by an actor; not authoritative |
| `confirmed` | Explicitly accepted by an authorized human or policy gate |
| `observed` | Directly measured by an identified trusted runner or tool |
| `derived` | Deterministically computed from referenced confirmed or observed records |
| `rejected` | Explicitly rejected by an authorized reviewer or validator |
| `superseded` | Replaced by a later record |

Authority is constrained by producer:

- agents may create `proposed` records;
- humans may create `confirmed` or `rejected` review records within their authority;
- trusted runners may create `observed` evidence only for fields they directly measured;
- deterministic engine projections may create `derived` records;
- no actor may promote its own proposal merely by changing a field.

### 10.5 Provenance and honesty

Every claim and result must identify its origin and supporting provenance.

Evidence records contain:

- producer identity and revision;
- collection method;
- input snapshot digest;
- execution-environment digest;
- command or operation digest;
- observation time;
- covered scope;
- completeness;
- artifact references;
- structured measurements.

An empty field, missing observation, or incomplete scope remains explicit.

Examples:

- raw process output does not prove that tests passed;
- exit code is observed by the runner, while text printed by the process is process-reported content;
- `changed_paths.complete` is true only when the runner controlled and compared the entire relevant workspace;
- a redaction claim is scoped to the channels actually inspected;
- an agent saying “done” creates a completion claim, not verified evidence.

### 10.6 Tasks

A task contains:

- goal and announcement;
- owner and assigned actor or actor requirements;
- input ledger projection;
- dependencies;
- contract;
- permitted operations;
- success signals;
- resource budget;
- status;
- assurance state;
- result and evidence references.

Task execution status:

- proposed;
- ready;
- claimed;
- running;
- blocked;
- completed;
- failed;
- cancelled.

Assurance state is separate:

- unverified;
- evidence_attached;
- verified;
- rejected.

An actor may mark execution `completed` by producing a result. Only acceptance checks, a verifier, or an authorized review can make the assurance state `verified`.

### 10.7 Ledger delta contract

Each node declares:

- the ledger projection it may read;
- record types it may propose;
- fields it may set;
- maximum record and payload counts;
- whether a human review is required;
- evidence required for each outcome.

The actor returns a proposed ledger delta:

~~~json
{
  "outcome": "completed",
  "records": [
    {
      "record_type": "result",
      "authority": "proposed",
      "subject_ref": "task_01J...",
      "data": {}
    }
  ],
  "evidence_refs": []
}
~~~

The runtime validates permissions, schemas, references, and authority before appending any record.

### 10.8 Review transactions

Human review is explicit and transactional:

- a review request is non-authoritative until acted upon;
- a batch confirm/reject operation is all-or-nothing;
- the request contains the ledger version and frame checksum it reviewed;
- stale reviews are rejected instead of being applied to changed work;
- empty review sets still return valid machine-readable JSON;
- stdout and write behavior are part of the CLI/API contract.

### 10.9 Projections

Agents and UI views consume projections, not the entire ledger.

Standard projections:

- current scope;
- confirmed claims;
- open assumptions and questions;
- ready tasks;
- active tasks;
- verification queue;
- decision history;
- evidence for a task or result;
- delivery summary.

Markdown summaries are generated reflections of these JSON projections. Editing Markdown does not mutate authoritative state.

### 10.10 Mechanical acceptance

Success signals are executable assertions where possible.

Examples:

- required JSON schema validates;
- named test suite exited successfully in a pinned headspace;
- dependency count changed by the expected amount;
- behavioral parity fixtures produce identical outputs;
- changed-path set is within policy;
- artifact digest matches the result record;
- required claims are confirmed;
- no blocking question remains.

Targets are encoded as tests or validators from the first implementation commit, not merely stated in prose.

---

## 11. Definition and compilation model

### 11.1 Authoring example

~~~yaml
apiVersion: nodes.culture.dev/v1alpha1
kind: Workflow

metadata:
  name: deliver-change
  version: 1.0.0
  ownerRef: team/platform-ai

spec:
  entry: intake

  contract:
    input:
      schemaRef: ./contracts/change-request.schema.json
    output:
      schemaRef: ./contracts/delivery-result.schema.json

  limits:
    maxDuration: 2h
    maxTransitions: 32
    maxVisitsPerNode: 4
    maxParallelTokens: 8

  ledger:
    schemaVersion: nodes.culture.dev/ledger/v1alpha1
    maxRecordsPerNode: 100
    requireProvenance: true

  nodes:
    intake:
      kind: agent
      ownerRef: team/platform-ai
      uses: actor://company/intake@sha256:111111
      contract:
        input:
          schemaRef: ./contracts/change-request.schema.json
        outcomes:
          completed:
            schemaRef: ./contracts/intake-result.schema.json
      input:
        from: /run/input
      ledger:
        read: [current_scope]
        propose: [announcement, claim, assumption, question]
      policy:
        timeout: 5m
        retry:
          maxAttempts: 2
          backoff: exponential

    plan:
      kind: agent
      ownerRef: team/architecture
      uses: actor://company/planner@sha256:222222
      contract:
        input:
          schemaRef: ./contracts/intake-result.schema.json
        outcomes:
          completed:
            schemaRef: ./contracts/plan.schema.json
      input:
        from: /nodes/intake/output
      ledger:
        read: [current_scope, confirmed_claims, open_assumptions]
        propose: [task, decision, success_signal]
      policy:
        timeout: 15m
        retry:
          maxAttempts: 2

    build:
      kind: agent
      ownerRef: team/developer-experience
      uses: actor://company/developer@sha256:333333
      contract:
        input:
          schemaRef: ./contracts/build-request.schema.json
        outcomes:
          completed:
            schemaRef: ./contracts/change-set.schema.json
      input:
        bindings:
          request: /run/input
          readyTasks: /ledger/projections/ready_tasks
          priorVerification: /ledger/projections/verification_queue
      ledger:
        read: [current_scope, confirmed_claims, ready_tasks, decision_history]
        propose: [claim, result, evidence]
      policy:
        timeout: 45m
        retry:
          maxAttempts: 2

    test:
      kind: code
      ownerRef: team/developer-experience
      uses: runner://headspace/docker@sha256:555555
      operation:
        workspaceRef: /nodes/build/artifacts/workspace
        image: registry.example/python-test@sha256:666666
        argv: [python, -m, pytest, -q]
        network: none
      contract:
        outcomes:
          passed:
            schemaRef: ./contracts/test-result.schema.json
          failed:
            schemaRef: ./contracts/test-result.schema.json
      ledger:
        read: [active_tasks]
        observe: [evidence]
        propose: [result]
      acceptance:
        requires:
          - kind: process_exit
            equals: 0
          - kind: workspace_diff
            complete: true
      policy:
        timeout: 15m
        retry:
          maxAttempts: 1

    verify:
      kind: agent
      ownerRef: team/quality-platform
      uses: actor://company/verifier@sha256:444444
      contract:
        input:
          schemaRef: ./contracts/change-set.schema.json
        outcomes:
          passed:
            schemaRef: ./contracts/verification.schema.json
          changes_required:
            schemaRef: ./contracts/verification.schema.json
          blocked:
            schemaRef: ./contracts/blocker.schema.json
      input:
        bindings:
          changeSet: /nodes/build/output
          testEvidence: /nodes/test/evidence
          ledger: /ledger/projections/verification_queue
      ledger:
        read: [current_scope, active_tasks, verification_queue]
        propose: [claim, result, question]
      policy:
        timeout: 20m
        retry:
          maxAttempts: 2

    human-review:
      kind: approval
      ownerRef: team/platform-ai
      decisionSchemaRef: ./contracts/review-decision.schema.json
      approverRef: group/platform-ai-approvers
      deadline: 24h

    finish:
      kind: end
      ownerRef: team/platform-ai
      output:
        from: /nodes/verify/output

  edges:
    - from: intake.completed
      to: plan

    - from: plan.completed
      to: build

    - from: build.completed
      to: test

    - from: test.passed
      to: verify

    - from: test.failed
      to: build

    - from: verify.passed
      to: finish

    - from: verify.changes_required
      to: build

    - from: verify.blocked
      to: human-review

    - from: human-review.approved
      to: build

    - from: human-review.rejected
      to: finish
~~~

### 11.2 Input bindings

Use JSON Pointer for direct data movement.

This keeps simple mappings deterministic and statically inspectable. Do not invent a template language for field interpolation.

Use CEL for:

- edge predicates;
- bounded decisions;
- policy predicates;
- small deterministic transforms where explicitly allowed.

More complex transformation belongs in a typed transform or action node.

### 11.3 Compiler output

The compiler produces normalized JSON containing:

- expanded defaults;
- resolved owners;
- resolved actor and component digests;
- resolved runner and container-image digests;
- bundled schema digests;
- validated bindings;
- validated ledger read/write capabilities;
- compiled success-signal checks;
- compiled CEL programs;
- normalized edge order;
- execution limits;
- policy references;
- presentation metadata;
- a content digest.

Store both:

- the exact submitted source for human review;
- the normalized JSON intermediate representation for execution.

The runtime executes only the normalized representation.

### 11.4 Validation levels

#### Syntax

The file and API version are valid.

#### Structure

Entrypoint, node kinds, edges, owners, and policies are valid.

#### Graph

References exist, terminal paths are reachable, joins are valid, and illegal cycles are rejected.

#### Contract validation

Schemas, bindings, outcomes, and known compatibility are valid.

#### Ledger validation

Record schemas, projections, authority transitions, producer permissions, provenance requirements, and success signals are valid.

#### Policy

Actors, runners, images, networks, secrets, classifications, limits, and required approvals are allowed.

#### Deployment

All pinned components and actor references are resolvable in the target environment.

---

## 12. Runtime architecture

~~~mermaid
flowchart TD
    C["CLI / Web UI"] --> API["API + compiler"]
    API --> DB["PostgreSQL"]
    DB --> S["Scheduler / outbox"]
    S --> Q["Postgres or SQS signal"]
    Q --> W["Stateless workers"]
    W --> A["External actors"]
    W --> H["headspace runner"]
    W --> O["Artifact store"]
    W --> DB
~~~

### 12.1 Deployable processes

Ship one Go binary with process modes:

- `nodes serve` — API, event stream, embedded web assets;
- `nodes scheduler` — timers, outbox, and ready-work publication;
- `nodes worker` — claims and dispatches work;
- `nodes all` — all roles for local development;
- `nodes validate` — validate and compile definitions;
- `nodes run` — create and follow a run;
- `nodes inspect` — inspect a run or attempt.

One binary reduces packaging differences. Production still scales each role independently.

### 12.2 Authoritative state

PostgreSQL is authoritative for:

- definitions and immutable versions;
- runs and tokens;
- node runs and attempts;
- leases and timers;
- human tasks;
- artifacts and references;
- idempotency records;
- append-only events;
- transactional outbox.

SQS is not the source of truth. It signals that authoritative work is ready.

### 12.3 Queue abstraction

The queue driver exposes:

- publish work reference;
- receive work reference;
- acknowledge signal;
- retry or delay signal.

On every signal, the worker still performs a fenced claim against PostgreSQL.

This design means:

- duplicated SQS messages are harmless;
- a message received out of order cannot overwrite newer state;
- lost publication can be repaired from the outbox;
- local mode needs no queue product;
- switching queue drivers does not change workflow semantics.

### 12.4 Work claiming

A work record contains:

- work ID;
- node-run ID;
- state version;
- lease owner;
- lease expiry;
- fencing token;
- attempt number;
- available-at time.

Claiming is atomic. Reclaiming an expired lease increments the fencing token.

Every completion update must match:

- work ID;
- expected state;
- attempt;
- fencing token.

Late workers cannot commit over a newer attempt.

### 12.5 Transaction boundary

When a node attempt completes, one database transaction:

1. verifies the fencing token and current state;
2. validates the output contract;
3. validates the proposed ledger delta and producer authority;
4. appends accepted ledger records and evidence references;
5. records the attempt result;
6. completes the node run;
7. appends audit events;
8. calculates eligible edges;
9. creates the next token or node runs;
10. inserts outbox records;
11. commits.

External side effects occur outside this transaction and therefore require idempotency.

### 12.6 Async actors

Workers must not hold leases or goroutines for long-running agents.

An asynchronous invocation changes the attempt to `waiting_external` and releases worker capacity. A durable deadline and callback identity are stored. Heartbeats and completion events resume scheduling.

### 12.7 Timers

Waits, retries, deadlines, and lease recovery are durable rows, not in-memory timers.

The scheduler claims due timers in bounded batches and writes the resulting state change and outbox event transactionally.

---

## 13. Actor protocol

### 13.1 Invocation

~~~http
POST /v1/invocations
Idempotency-Key: <attempt-key>
Traceparent: <trace-context>
Authorization: Bearer <scoped-workload-token>
Content-Type: application/json
~~~

~~~json
{
  "protocol_version": "1.0",
  "run_id": "run_01J...",
  "token_id": "tok_01J...",
  "node_run_id": "nr_01J...",
  "attempt_id": "att_01J...",
  "attempt": 1,
  "workflow": {
    "name": "deliver-change",
    "version_digest": "sha256:..."
  },
  "node": {
    "id": "build",
    "contract_digest": "sha256:..."
  },
  "input": {},
  "artifact_refs": [],
  "context_refs": [],
  "deadline": "2026-08-08T15:00:00Z",
  "callback": {
    "url": "https://nodes.example/v1/attempts/att_01J/events",
    "token": "<short-lived-attempt-token>"
  }
}
~~~

### 13.2 Synchronous result

`200 OK` returns:

~~~json
{
  "outcome": "completed",
  "output": {},
  "ledger_delta": {
    "records": []
  },
  "artifact_refs": [],
  "continuation_ref": null,
  "usage": {
    "input_tokens": 0,
    "output_tokens": 0,
    "cost": null,
    "currency": null
  }
}
~~~

### 13.3 Asynchronous acceptance

`202 Accepted` returns:

~~~json
{
  "invocation_id": "external_123",
  "heartbeat_after_seconds": 30,
  "supports_cancellation": true
}
~~~

### 13.4 Callback events

Actors can send:

- accepted;
- heartbeat;
- progress;
- artifact;
- completed;
- failed;
- blocked.

Every actor event contains a stable event ID and monotonically increasing actor sequence. Repeated callbacks are idempotent.

Completion after cancellation or attempt replacement is recorded as a late diagnostic event but cannot commit workflow state.

### 13.5 Error classification

Adapters classify errors as:

- retryable transport failure;
- rate limited;
- actor unavailable;
- actor rejected input;
- authentication or policy failure;
- contract failure;
- execution failure;
- timeout;
- cancellation.

Only explicitly retryable categories use automatic retry policy.

### 13.6 Cancellation

Cancellation is durable in Culture Nodes and best-effort at the actor.

The adapter may call the actor's cancellation endpoint, but workflow state does not depend on an external process acknowledging cancellation.

### 13.7 Code execution with headspace-cli

headspace-cli is the first code-runner adapter and the reference pattern for trustworthy code nodes.

The boundary is:

~~~mermaid
flowchart LR
    O["Typed operation"] --> P["Enforced policy"]
    P --> H["Docker headspace"]
    H --> R["Result"]
    H --> E["Observed evidence"]
~~~

Policy must be enforced inside dispatch to the runner. A wrapper that checks policy and then hands an unrestricted shell to another process does not satisfy the boundary.

#### Operation contract

A code node submits:

~~~json
{
  "operation_id": "op_01J...",
  "runner": "headspace",
  "runner_revision": "sha256:...",
  "workspace": {
    "source_ref": "artifact://workspace/input",
    "source_digest": "sha256:...",
    "write_mode": "copy-on-write"
  },
  "container": {
    "image": "registry.example/test-runner@sha256:...",
    "user": "65532:65532",
    "read_only_root": true
  },
  "command": {
    "argv": ["python", "-m", "pytest", "-q"],
    "working_directory": "/workspace",
    "environment_refs": []
  },
  "policy": {
    "timeout_seconds": 900,
    "cpu": 2,
    "memory_mib": 2048,
    "pids": 256,
    "network": "none",
    "allowed_output_paths": ["/workspace"]
  },
  "evidence": {
    "snapshot_before": true,
    "snapshot_after": true,
    "capture_exit": true,
    "capture_logs": true
  }
}
~~~

The command is an argument array, never a shell string. If a shell is intentionally required, the operation declares that fact and policy can reject it.

#### Safe defaults

- network disabled;
- image pinned by digest;
- non-root user;
- read-only container root;
- copy-on-write workspace;
- no host Docker socket;
- no host home directory;
- no implicit environment variables;
- secrets absent unless individually granted;
- CPU, memory, process, disk, and time limits;
- bounded stdout and stderr with complete logs stored as artifacts;
- output paths explicitly allowed.

#### Structured result

headspace returns:

~~~json
{
  "operation_id": "op_01J...",
  "state": "completed",
  "exit": {
    "code": 0,
    "signal": null
  },
  "timing": {
    "started_at": "2026-08-08T15:00:00Z",
    "finished_at": "2026-08-08T15:01:12Z",
    "duration_ms": 72000
  },
  "environment": {
    "runner_revision": "sha256:...",
    "image_digest": "sha256:...",
    "input_digest": "sha256:...",
    "policy_digest": "sha256:..."
  },
  "changes": {
    "complete": true,
    "paths": [],
    "snapshot_digest": "sha256:...",
    "diff_artifact_ref": "artifact://diff/..."
  },
  "artifacts": {
    "stdout_ref": "artifact://logs/stdout",
    "stderr_ref": "artifact://logs/stderr",
    "output_workspace_ref": "artifact://workspace/output"
  }
}
~~~

#### Trust boundary

The runner may issue `observed` evidence for facts it directly measured:

- image digest;
- input digest;
- policy digest;
- exit code and signal;
- start and end time;
- resource usage;
- before/after snapshot and diff digest;
- files visible within the controlled workspace.

Text printed by the process remains process-reported content. A line saying “all tests passed” is not itself observed proof. A parser or test adapter may derive structured test evidence from a report artifact, with its parser revision and coverage recorded.

Docker provides isolation, not truth. Trust comes from the typed operation, enforced policy, immutable inputs, runner observation, evidence provenance, and independent validation.

#### Replayability

Every code result can emit a replay manifest containing:

- input artifact digest;
- image digest;
- runner revision;
- command argv;
- declared environment;
- policy digest;
- expected output contract.

Re-execution may still be nondeterministic when dependencies, clocks, randomness, or external resources are allowed. The ledger records those allowances rather than claiming hermetic behavior.

---

## 14. Persistence model

Core tables:

| Table | Purpose |
| --- | --- |
| `namespaces` | Installation and tenant boundary |
| `owners` | Human/team ownership references |
| `actors` | Actor identities and immutable revisions |
| `workflow_drafts` | Mutable authoring state |
| `workflow_versions` | Immutable source, normalized IR, and digest |
| `runs` | One workflow execution |
| `tokens` | Active and historical control tokens |
| `node_runs` | Logical node executions |
| `attempts` | Individual dispatch attempts |
| `runner_operations` | Code-runner requests, policy digests, and observed results |
| `work_items` | Ready work, leases, and fencing |
| `timers` | Durable waits, deadlines, and retry availability |
| `human_tasks` | Approval and human-input work |
| `ledger_records` | Immutable typed claims, tasks, decisions, results, and evidence |
| `ledger_reviews` | Atomic review transactions and stale-frame guards |
| `ledger_projection_versions` | Checkpoints for deterministic projections |
| `artifacts` | Metadata and object-store references |
| `events` | Append-only audit events |
| `outbox` | Transactional event and queue publication |
| `idempotency_keys` | External request and dispatch deduplication |

Every operational row contains `namespace_id` from the first migration, even if the first release exposes only a single installation namespace.

Do not use full event sourcing for the MVP.

- current-state tables are authoritative for orchestration;
- the append-only work ledger is authoritative for task, claim, decision, and evidence meaning;
- append-only runtime events are authoritative for audit and integrations;
- projections can be rebuilt from their authoritative source.

---

## 15. Events and observability

### 15.1 Event envelope

Emit CloudEvents-compatible JSON envelopes.

Example types:

- `dev.culture.nodes.run.created`;
- `dev.culture.nodes.token.entered`;
- `dev.culture.nodes.node-run.ready`;
- `dev.culture.nodes.attempt.started`;
- `dev.culture.nodes.actor.accepted`;
- `dev.culture.nodes.attempt.completed`;
- `dev.culture.nodes.ledger.record-appended`;
- `dev.culture.nodes.ledger.review-committed`;
- `dev.culture.nodes.runner.operation-completed`;
- `dev.culture.nodes.contract.rejected`;
- `dev.culture.nodes.token.transitioned`;
- `dev.culture.nodes.run.waiting`;
- `dev.culture.nodes.run.completed`.

Event data contains IDs and safe metadata. Large or sensitive content is referenced, not copied into every event.

### 15.2 OpenTelemetry model

Do not keep a single span open for a workflow that may wait for hours or days.

Use:

- one span per attempt;
- propagated trace context for synchronous work;
- span links across queues and asynchronous callbacks;
- stable `run_id` and `node_run_id` attributes for correlation;
- audit events for the durable history.

Use GenAI semantic conventions when an adapter reports compatible metadata, but do not make evolving provider telemetry part of the runtime contract.

### 15.3 Required telemetry

Traces and logs may use high-cardinality IDs.

Metrics must avoid run IDs and expose:

- ready-to-claim latency;
- dispatch latency;
- actor duration;
- node duration;
- retry count;
- contract rejection count;
- ledger validation and stale-review rejection count;
- headspace queue, start, and execution duration;
- code operations by outcome and bounded runner revision;
- lease expiry count;
- callback latency;
- active and waiting runs;
- ready work depth;
- scheduler lag;
- artifact bytes;
- reported agent tokens and cost by bounded dimensions.

Prompt, completion, tool arguments, and tool results are not captured by default. Content telemetry requires an explicit policy.

---

## 16. Security model

### 16.1 Authentication and identity

- Human access uses OIDC.
- Services use workload identity, mTLS, or signed short-lived tokens.
- Every actor invocation is attributable to workflow, version, run, token, node, attempt, actor, and owner.
- Every runner observation is attributable to workflow, node, operation, input digest, image digest, policy digest, runner revision, and owner.

### 16.2 Authorization

RBAC roles:

- viewer;
- author;
- publisher;
- operator;
- approver;
- namespace administrator.

Policy can additionally constrain:

- actor kinds;
- specific actor revisions;
- network destinations;
- secret references;
- data classifications;
- maximum limits;
- required approval nodes;
- allowed code runners.

### 16.3 Secrets

Definitions contain `secretRef` values only.

Prefer resolving secrets within the target actor or isolated runner using workload identity. The control plane should not materialize provider credentials merely to forward them.

### 16.4 Code isolation

The generic worker must never:

- mount the Docker socket;
- execute repository scripts directly;
- evaluate arbitrary JavaScript or shell;
- run untrusted plugins in-process.

Container code runs through an isolated runner on ECS, EKS, or another configured backend with a pinned image digest, resource limits, non-root user, scoped network, and scoped workload role.

The first implementation dispatches code through the headspace-cli adapter. The adapter is a separate deployment and security boundary from the generic worker.

Small pure transformations may later use WASI with no network or filesystem by default.

Runner evidence is accepted only for observation kinds declared by the runner manifest and allowed by policy. Container output cannot grant itself `observed` authority.

### 16.5 Network safety

HTTP actors use:

- explicit allowlists or registered endpoints;
- DNS and redirect revalidation;
- private-network policy;
- TLS verification;
- request and response size limits;
- deadlines;
- SSRF protection.

### 16.6 Audit and retention

Audit events are append-only at the application layer and exportable to an external retention system.

Retention is configured separately for:

- operational state;
- event history;
- logs and traces;
- artifacts;
- sensitive agent content.

---

## 17. Technology choices

### 17.1 Runtime language: Go

Choose Go for the control plane.

Why:

- strong fit for network services, schedulers, workers, and CLIs;
- cheap concurrency for many mostly waiting operations;
- predictable deployment as a small set of binaries;
- straightforward Linux and ARM64 builds;
- strong PostgreSQL, AWS, OpenTelemetry, and CEL ecosystems;
- lower idle overhead than a Node.js or Python control plane;
- simpler operational profile than Rust for the first implementation.

Do not require agent authors to use Go. The actor protocol is HTTP/JSON and language-neutral.

### 17.2 Why not the alternatives

| Language | Strength | Reason not chosen for the control plane |
| --- | --- | --- |
| Rust | Excellent efficiency and safety | Slower initial iteration and a higher contributor barrier for this product stage |
| TypeScript/Node.js | Fast product iteration and shared UI language | Better used in the UI; less aligned with the low-idle, many-worker control-plane target |
| Python | Excellent AI ecosystem and prototyping | Agents can remain Python; the durable scheduler should not inherit Python worker overhead |
| C#/.NET | Strong enterprise runtime and tooling | Viable, but Go better matches the single-binary, minimal-container, Kubernetes-native target |

### 17.3 Go implementation profile

Prefer a small dependency set:

- standard `net/http` server and middleware;
- `pgx` for PostgreSQL;
- generated typed SQL using `sqlc`;
- CEL-Go;
- a JSON Schema 2020-12 validator;
- OpenTelemetry Go;
- AWS SDK modules only in AWS-specific packages.
- a narrow headspace-cli client in the runner adapter package.

Keep core engine packages independent of AWS SDK types.

### 17.4 Web framework

Use:

- TypeScript;
- React;
- Vite;
- React Flow;
- the shared Culture/AgentCulture design tokens and graph components;
- Tailwind CSS, Radix primitives, and Motion only to the extent used or permitted by that shared system;
- ELK.js for optional automatic layout;
- TanStack Query for API state.

Serve the production SPA from the Go binary using embedded assets. Local development uses the Vite development server.

Keep all visual-system coupling behind one `web/src/culture-design/` boundary. Culture Nodes should be able to update to a newer AgentCulture standard without rewriting graph behavior.

Do not use Next.js. The product does not require server-rendered public pages, and a separate application server would complicate the one-deployment goal.

### 17.5 Live transport

Use Server-Sent Events for run updates because the first live interaction is server-to-client.

Add WebSockets only when collaborative editing or another true bidirectional stream becomes a requirement.

### 17.6 Durable execution dependency

Do not make Temporal, Kafka, Kubernetes, or Redis mandatory for the MVP.

Culture Nodes is itself defining workflow semantics. A small PostgreSQL state machine keeps local deployment understandable and preserves control over contracts, tokens, ownership, and visual state.

Keep engine interfaces narrow enough that a future execution backend can be evaluated by ADR if proven requirements exceed the PostgreSQL engine.

---

## 18. Repository structure

~~~text
culture-nodes/
├── cmd/
│   └── nodes/
├── internal/
│   ├── api/
│   ├── auth/
│   ├── compiler/
│   ├── contracts/
│   ├── ledger/
│   │   ├── records/
│   │   ├── projections/
│   │   ├── review/
│   │   └── validation/
│   ├── engine/
│   ├── scheduler/
│   ├── worker/
│   ├── actors/
│   ├── runners/
│   │   └── headspace/
│   ├── queue/
│   │   ├── postgres/
│   │   └── sqs/
│   ├── store/
│   │   └── postgres/
│   ├── artifacts/
│   │   ├── filesystem/
│   │   └── s3/
│   ├── events/
│   ├── policy/
│   └── telemetry/
├── api/
│   ├── openapi/
│   └── actor-protocol/
├── schemas/
│   ├── workflow/
│   ├── actor/
│   ├── ledger/
│   ├── runner/
│   ├── events/
│   └── examples/
├── migrations/
├── web/
│   ├── src/app/
│   ├── src/canvas/
│   ├── src/features/
│   ├── src/culture-design/
│   └── src/lib/
├── sdk/
│   ├── python/
│   ├── typescript/
│   └── dotnet/
├── examples/
│   ├── hello/
│   └── delivery-loop/
├── deploy/
│   ├── compose/
│   ├── helm/
│   └── aws/
├── docs/
│   ├── concepts/
│   ├── operations/
│   ├── security/
│   └── adr/
├── tests/
│   ├── conformance/
│   ├── fault/
│   ├── ledger/
│   ├── headspace/
│   └── load/
└── Makefile
~~~

Keep SDKs thin. Generate API types where possible and avoid putting engine behavior into SDKs.

---

## 19. Deployment profiles

### 19.1 Local

`docker compose up` starts:

- one `nodes all` container;
- PostgreSQL;
- one separately isolated headspace runner for the code-node example;
- optional local object storage only when an example needs artifacts.

The UI is embedded in the Go service. A separate UI container is unnecessary.

The Docker engine used by headspace is never mounted into the `nodes` container. The local profile uses a separate runner boundary and makes that reduced local isolation explicit.

`nodes run examples/hello/workflow.yaml` can also use a developer-mode embedded SQLite-free setup only if durable semantics remain identical; otherwise require PostgreSQL and keep one storage model.

Recommendation: require PostgreSQL from the beginning. Avoid implementing and testing two persistence semantics.

### 19.2 AWS

Default production profile:

- ALB in front of API tasks;
- ECS/Fargate services for API, scheduler, and generic workers;
- dedicated headspace runner capacity on isolated EC2 or EKS worker nodes for Docker code operations;
- RDS/Aurora PostgreSQL;
- SQS Standard queues;
- S3 for artifacts;
- Secrets Manager references;
- OIDC/IAM workload identity;
- OpenTelemetry collector to the chosen backend.

EKS is supported through deployment packaging and the external runner adapter, but should not be required.

Do not claim that a Docker-based headspace runs inside Fargate. A future ECS `RunTask` adapter may implement the same typed operation and evidence contract without being the Docker headspace adapter.

### 19.3 Process scaling

- API scales on request load.
- workers scale on ready-work latency and queue depth.
- scheduler begins as one active lease holder with standby instances.
- actor and code-runner capacity scale independently.

---

## 20. Reliability semantics

### 20.1 Guarantees

Culture Nodes guarantees:

- committed orchestration state is durable;
- a run pins an immutable definition;
- each state transition is fenced and idempotent;
- ready work is eventually republished from the outbox;
- an audit event records each committed transition;
- accepted ledger records are immutable and attributable;
- observed evidence is scoped to a declared producer and collection method;
- failed workers can be replaced after lease expiry.

### 20.2 Explicit non-guarantees

Culture Nodes does not guarantee:

- exactly-once external side effects;
- deterministic agent outputs;
- successful cancellation of an already-running external process;
- total ordering between unrelated parallel branches;
- static proof of every schema relationship;
- truth of assertions printed by code inside a container.

### 20.3 Idempotency

Every dispatch receives a stable attempt idempotency key.

Actors that perform side effects must either:

- honor the key;
- expose a reconciliation operation;
- declare themselves non-idempotent and require policy approval.

Retry policy is prohibited for a non-idempotent actor unless a compensating or reconciliation strategy is configured.

### 20.4 Recovery matrix

| Failure point | Recovery |
| --- | --- |
| Worker dies before dispatch | Lease expires; another worker claims |
| Worker dies after dispatch, before recording acceptance | Same attempt key is retried or reconciled |
| Actor callback is duplicated | Event ID and sequence deduplicate it |
| Actor callback arrives after a newer attempt | Record as late; fencing rejects state change |
| Headspace completion is delivered twice | Operation ID and attempt fencing deduplicate it |
| SQS signal is duplicated | PostgreSQL claim permits one current owner |
| SQS publication is missed | Outbox republishes |
| API restarts during a human wait | Durable task and timer remain |
| Scheduler restarts | Due timers are reclaimed |

---

## 21. Performance and quality gates

These are initial engineering targets, not public claims.

### 21.1 Resource budgets

- idle generic worker RSS target: no more than 64 MiB;
- idle API process RSS target: no more than 96 MiB;
- local all-in-one process RSS target: no more than 128 MiB, excluding PostgreSQL;
- no worker memory growth proportional to total historical runs;
- large payloads leave the database and event stream through artifact references.

### 21.2 Initial benchmark profile

On a documented 4-vCPU, 8-GiB Linux environment with PostgreSQL:

- store 10,000 non-terminal runs;
- sustain 250 committed node transitions per second for ten minutes;
- keep p95 ready-to-claim latency below one second;
- recover a killed worker within configured lease expiry plus five seconds;
- demonstrate duplicate queue delivery without duplicate committed transition;
- demonstrate 1,000 concurrently waiting external invocations without one goroutine per wait;
- append and project 100,000 ledger records with deterministic projection digests;
- run the headspace conformance fixture repeatedly with identical input, image, policy, and result-envelope structure.

Change targets only through a recorded ADR with benchmark evidence.

### 21.3 Definition quality

- identical normalized definitions produce identical digests;
- publishing rejects mutable production references;
- all example workflows pass schema and policy validation;
- every compiled node has an owner and pinned actor, runner, or component digest;
- every node's ledger read/write authority compiles to an explicit capability set;
- identical ledger inputs produce identical deterministic projections.

### 21.4 UI quality

- maintain smooth pan and zoom on a 500-node graph in the reference browser;
- aggregate active execution markers before update density harms readability;
- pass automated accessibility checks and manual keyboard review;
- visual regression tests cover every node status and zoom level;
- graph components are visually compared against the pinned AgentCulture design revision.

---

## 22. Testing strategy

### Unit tests

- state transition legality;
- JSON normalization and digest stability;
- schema bundling and validation;
- CEL compilation;
- retry and backoff calculation;
- edge selection;
- lease fencing;
- idempotency;
- ledger authority rules;
- ledger projection and supersession;
- stale review detection;
- headspace operation and evidence validation.

### Property tests

- no terminal node run becomes non-terminal;
- old fencing tokens never commit;
- event sequence is monotonic per aggregate;
- normalization is idempotent;
- duplicate callbacks do not change final state;
- an agent-origin record never becomes `confirmed` without an authorized review;
- process-reported text never becomes runner-observed evidence;
- superseded records never reappear in a current projection.

### Integration tests

- PostgreSQL work claiming with multiple workers;
- transactional outbox publication;
- sync and async actors;
- contract rejection;
- timers and human tasks;
- atomic ledger review transactions;
- Devague record round-trip fixtures;
- headspace operation, timeout, cancellation, snapshot, and evidence handling;
- S3 artifact references;
- SQS duplicate and reordered delivery.

### Fault-injection tests

Kill processes:

- before actor dispatch;
- after dispatch;
- before transaction commit;
- after commit but before signal acknowledgement;
- during callback handling;
- during timer processing;
- after a headspace operation starts but before its result is recorded;
- between ledger validation and transaction commit.

Every test asserts the final state and audit trail, not merely process exit.

### Conformance kit

Provide a language-neutral actor conformance suite that verifies:

- authentication;
- idempotency;
- synchronous result;
- asynchronous acceptance;
- heartbeat;
- cancellation;
- duplicate callback handling;
- contract failure behavior.

Provide a ledger conformance suite that verifies:

- record schemas and digests;
- origin and authority permissions;
- atomic review;
- stale-frame rejection;
- deterministic projections;
- supersession;
- evidence completeness.

Provide a headspace conformance suite that verifies:

- pinned image and runner revisions;
- argv execution without implicit shell;
- network-denied default;
- non-root and resource limits;
- input/output snapshot digests;
- exit and signal observation;
- bounded logs and artifact references;
- duplicate operation reconciliation;
- clear separation between observed facts and process-reported content.

---

## 23. Delivery plan

### Phase 0 — Contract and compiler

Deliver:

- terminology and schema;
- YAML/JSON parser;
- normalized JSON IR;
- digesting;
- JSON Schema contracts;
- ledger record schemas and authority model;
- deterministic ledger projections;
- Devague mapping fixtures;
- CEL conditions;
- owner and policy validation;
- `nodes validate`;
- delivery-loop example.

Exit criteria:

- the example compiles deterministically;
- deliberate ownership, graph, contract, ledger, evidence, and policy errors produce precise diagnostics;
- the same Devague fixture maps to the same ledger projection and digest;
- no runtime code is required to review the workflow definition.

### Phase 1 — Durable vertical slice

Deliver:

- PostgreSQL schema;
- sequential state machine with loops;
- sync and async HTTP actors;
- append-only work ledger and review transactions;
- headspace-cli Docker code node;
- retries, timeouts, cancellation;
- runtime event log and outbox;
- API and CLI run commands;
- read-only live graph Run and Ledger views aligned to AgentCulture;
- Docker Compose.

Exit criteria:

- intake → plan → build → test → verify runs;
- test executes through headspace and appends structured observed evidence;
- `changes_required` loops to build;
- process restart preserves the run;
- an asynchronous actor completes by callback;
- an agent completion claim remains unverified until success signals accept its evidence;
- the UI shows committed transitions and ledger changes live.

### Phase 2 — Multi-instance runtime

Deliver:

- leases and fencing;
- multiple workers;
- SQS signal driver;
- S3 artifacts;
- OpenTelemetry;
- OIDC and workload authentication;
- fault-injection and load suites;
- ECS/Fargate deployment.

Exit criteria:

- worker-kill and duplicate-delivery tests pass;
- benchmark gates are recorded;
- API and workers scale independently.

### Phase 3 — Authoring and reuse

Deliver:

- graphical editor;
- compiler diagnostics on canvas;
- ledger/provenance graph and review inbox;
- reusable node and subworkflow packages;
- immutable version publishing;
- approval inbox;
- policy administration;
- OCI artifact packaging and signatures.

### Phase 4 — Rich execution

Deliver:

- parallel, join, foreach, and subworkflow nodes;
- additional runner adapters;
- optional WASI transforms;
- advanced run comparison and replay;
- cost and budget enforcement;
- enterprise retention and audit export.

---

## 24. MVP acceptance criteria

The initial implementation issue is complete when:

- [ ] `apiVersion: nodes.culture.dev/v1alpha1` is documented and validated.
- [ ] YAML and JSON authoring compile to one normalized JSON representation.
- [ ] Published workflow versions are immutable and content-addressed.
- [ ] Every compiled node resolves an explicit owner.
- [ ] Agent, HTTP action, and code nodes use provider-neutral actor or runner boundaries.
- [ ] Input, output, and error payloads are runtime-validated.
- [ ] Ledger records are immutable, schema-valid, content-addressed, and attributable.
- [ ] Agents can propose but cannot self-confirm claims or evidence.
- [ ] Task execution state is separate from assurance state.
- [ ] Human review is atomic and rejects stale ledger versions.
- [ ] Markdown summaries are derived from JSON and are not authoritative.
- [ ] Domain outcomes are separate from technical failure.
- [ ] Conditional edges and a bounded loop work.
- [ ] A run pins an exact workflow digest.
- [ ] Run state survives API, scheduler, and worker restart.
- [ ] Long-running actors do not hold worker leases.
- [ ] Attempts carry idempotency keys and fencing tokens.
- [ ] Two workers cannot commit the same current transition.
- [ ] Duplicate actor callbacks are harmless.
- [ ] Every committed transition produces an append-only event.
- [ ] A headspace code node runs in a disposable Docker boundary with a pinned image and policy.
- [ ] The headspace result includes observed exit, environment, snapshot, diff, and artifact evidence.
- [ ] Process output cannot grant itself `observed` authority.
- [ ] The read-only graph Run view displays nodes, edges, actors/runners, owners, status, tasks, and evidence.
- [ ] The visual implementation uses the pinned current AgentCulture design revision.
- [ ] Reduced-motion and non-graph timeline views exist.
- [ ] `docker compose up` starts the complete local system.
- [ ] No provider-specific agent type leaks into the engine.
- [ ] No arbitrary code executes in the control-plane process.
- [ ] Idle-memory and transition-throughput benchmarks are published.
- [ ] The AWS production topology and threat model are documented.

---

## 25. Risks and mitigations

### Culture Nodes drifts from the shared visual standard

**Mitigation:** reuse or extract the current AgentCulture graph components and tokens, pin the source revision, and enforce visual regression tests.

### Building a durable engine becomes the whole project

**Mitigation:** deliberately small PostgreSQL state machine, sequential MVP, explicit non-goals, fault tests before feature breadth, and a future backend ADR boundary.

### JSON Schema compatibility is overpromised

**Mitigation:** distinguish proven incompatibility, proven compatibility, and unknown; always validate at runtime.

### Agents create unbounded loops or cost

**Mitigation:** engine-enforced transition, visit, duration, concurrency, token, and optional cost limits.

### External actors lose callbacks

**Mitigation:** actor idempotency, heartbeat/deadline, reconciliation hook, and durable attempt state.

### Ledger becomes an unstructured event dump

**Mitigation:** small registered record vocabulary, strict schemas, authority rules, bounded projections, supersession, and separate runtime events.

### Agents manufacture evidence

**Mitigation:** producer-scoped authority; agents propose, trusted runners observe only measured facts, and validators derive conclusions from referenced evidence.

### UI becomes a generic node editor

**Mitigation:** shared Culture graph components, progressive detail, owner/contract/ledger panels, provenance edges, and workflow-specific states.

### Code nodes compromise the control plane

**Mitigation:** no in-process scripts or control-plane Docker socket; separate headspace runner with pinned artifacts, enforced policy, observation scope, and workload identity.

### SQS and PostgreSQL disagree

**Mitigation:** PostgreSQL is authoritative; SQS contains disposable work references only.

### Metrics become unusable from high cardinality

**Mitigation:** IDs belong in events, logs, and traces; metrics use bounded labels.

---

## 26. Deliberate decisions and remaining questions

### Decided

- Product name: Culture Nodes.
- Product URL: `nodes.culture.dev`.
- Honeycomb, cell, and bee visuals and vocabulary are removed.
- The latest AgentCulture graph design is the visual standard.
- Backend: Go.
- UI: React/TypeScript/Vite/React Flow.
- Canonical state and definitions: JSON.
- Work state: append-only Devague-derived JSON ledger.
- Agents propose records; authorized humans confirm; trusted runners observe; deterministic validators derive.
- Human authoring: YAML or JSON.
- Runtime contracts: JSON Schema 2020-12.
- Conditions: CEL.
- Durable store: PostgreSQL.
- Local queue: PostgreSQL.
- AWS queue: SQS signal with PostgreSQL authority.
- Artifact store: S3-compatible.
- Actors remain external.
- Long-running actors complete asynchronously.
- Code runs through headspace-cli outside the control plane.
- Docker isolation is not treated as evidence of truth.
- The first UI is a live read-only graph Run and Ledger view, not the full editor.

### Questions to answer during Phase 0

1. Should the standalone CLI remain `nodes` or become a subcommand of an existing `culture` CLI?
2. Which exact AgentCulture source revision or shared package owns the current graph components?
3. Should the shared ledger schema remain in Culture Nodes initially or move to the existing `workledger` project after Devague round-trip conformance?
4. Should ownership references resolve from a repository file, Culture identity, or both?
5. Which OIDC provider and workload-identity profile should the reference AWS deployment demonstrate?

These questions affect integration details, not the core architecture.

---

## 27. Initial issue statement

Implement the Phase 0 and Phase 1 vertical slice of Culture Nodes:

> A versioned graph for intake, planning, building, testing, and verification is authored as YAML, compiled to canonical JSON, executed durably through external agents and a headspace code runner, recorded in an append-only work ledger, and rendered using the current AgentCulture graph system.

The slice is successful when the build agent proposes completion, headspace observes the exact test operation and evidence, acceptance logic verifies or rejects the task, the verifier can return `changes_required`, the graph visibly loops to build, the run survives process restart, and every claim and result remains attributable to an exact workflow version, node, actor or runner, contract, attempt, evidence set, and owner.

That is the smallest implementation that proves the product:

> **Every node has a contract. Every result has evidence.**

---

## 28. Primary technical references

- [Go](https://go.dev/)
- [React Flow custom nodes](https://reactflow.dev/learn/customization/custom-nodes)
- [React Flow performance guidance](https://reactflow.dev/learn/advanced-use/performance)
- [Common Expression Language](https://cel.dev/)
- [JSON Schema Draft 2020-12](https://json-schema.org/draft/2020-12)
- [PostgreSQL `SKIP LOCKED`](https://www.postgresql.org/docs/current/sql-select.html)
- [Amazon SQS at-least-once delivery](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/standard-queues-at-least-once-delivery.html)
- [CloudEvents](https://cloudevents.io/)
- [OpenTelemetry semantic conventions](https://opentelemetry.io/docs/specs/semconv/)
- [Amazon ECS task IAM roles](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/task-iam-roles.html)
- [AWS Fargate security considerations](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/fargate-security-considerations.html)
- [wazero](https://wazero.io/)
- [Culture.dev](https://culture.dev/)
- [Culture ecosystem](https://culture.dev/ecosystem/)
- [AgentCulture Devague](https://github.com/agentculture/devague)
- [AgentCulture headspace-cli](https://github.com/agentculture/headspace-cli)
