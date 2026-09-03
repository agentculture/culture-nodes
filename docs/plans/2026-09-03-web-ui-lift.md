# Build Plan — web-ui-lift

slug: `web-ui-lift` · status: `exported` · from frame: `web-ui-lift`

> Culture Nodes' web UI got a lift: one graph language across mesh, workflows and runs; the mesh draws the relationships the data already holds instead of a hub-and-spoke; every published workflow is readable as a graph, not only running ones; the mesh no longer replays history on arrival; and a workflow can be composed on canvas.

## Tasks

### t1 — Server: tail-only stream mode with a snapshot marker

- instruction: internal/api/events.go: parse from=latest in the cross-run resume reader; on that mode SELECT max(id) FROM events WHERE `namespace_id`=$1, write one SSE frame (id: that max id, event: stream.snapshot, data: {"`snapshot_id`": ...}), then poll from that id. Add the frame to api/openapi/openapi.yaml streamEvents. Tests in internal/api/`events_test.go` only. No web files.
- covers: c2, h1, h25, c30, h20
- acceptance:
  - GET /v1alpha1/events?from=latest sends event: stream.snapshot as its first frame carrying the namespace's current max events-table id, then streams strictly after that id; the cursor-less default is unchanged (`events_test.go`)
  - A test seeds 50 terminal runs and asserts none of their lifecycle events is streamed on a ?from=latest connect
  - A test commits a run.created between the marker and a subsequent GET /runs read and asserts the event is delivered exactly once after the marker

### t2 — Web: the one shared stream opens tail-only and views reconcile on the snapshot marker

- instruction: Files: web/src/hooks/useSharedEvents.tsx (manager: from=latest, snapshotId in snapshot, handle event stream.snapshot), web/src/api/client.ts meshEventsUrl(latest), web/src/routes/Mesh.tsx + Inbox/Decisions/RunsList/RunsBoard/JobsTimeline/Statistics/NodeGraphs (buffer-until-REST helper in hooks/useSnapshotReconcile.ts), web/src/fixtures/mesh-fixture.ts (snapshot marker), tests/lint/`eventsource_test.go`. Do not touch MeshCanvas.tsx or domain/mesh.ts (t6 owns them).
- depends on: t1
- covers: c37, h29, c30, h20
- acceptance:
  - useSharedEvents.tsx constructs its EventSource with ?from=latest, stores the stream.snapshot id, and exposes it via the manager snapshot; a lint test asserts exactly one EventSource construction site under web/src
  - Mesh.tsx and every other shared-stream view fetch REST state after subscribing and buffer streamed events until the response resolves, applying them idempotently by id; a vitest mounts Mesh after the stream has been open, commits a run.created between fetch and subscribe, and asserts the run renders once
  - web/e2e/mesh.spec.ts: with a fixture stream carrying 1,284 historical events, #agent-state mesh.`events_total` is 0 after load and increments by exactly one per committed event afterwards

### t3 — The one Culture node component on React Flow, in the mesh idiom

- instruction: Port the presence-node CSS from ActiveGraphCanvas.tsx (halo keyframes, pulse rings, motion gating) into culture-design/CultureNode.tsx + culture-design/node.css; NodeCard's kind-specific content and bandForZoom move into it. Keep the terminal ground for every canvas (.canvas surface class). Files: web/src/culture-design/CultureNode.tsx, node.css, components/WorkflowNode.tsx, components/ActiveGraphCanvas.tsx (adapter only), components/NodeCard.tsx (remove), styles/app.css (node rules move out), tests/lint/`culturenode_test.go`. Reference: docs/demos/web-ui-lift/culture-nodes-lifted.html .cn styles.
- covers: c22, c32, h22
- acceptance:
  - web/src/culture-design/CultureNode.tsx renders kind-coloured glowing core, halo, label, sub-line, optional state chip and one-shot pulse ring from tokens only; WorkflowNode.tsx and ActiveGraphNode are thin adapters over it; RunView, ActiveGraphCanvas and AuthorWorkflow preview render through it
  - Zoom bands far/medium/close still change what a node shows (NodeCard.test.tsx ported); prefers-reduced-motion renders one static frame (data-motion=static, no keyframes)
  - tests/lint/`culturenode_test.go` pins that RunView, ActiveGraphCanvas, Design gallery, authoring preview and Mesh import web/src/culture-design/CultureNode.tsx and that no second node-card component exists (NodeCard.tsx deleted or reduced to the adapter)

### t4 — Design-token lint guard: no colour, font or radius literal outside culture-design

- instruction: Go test scanning web/src/\*\*/\*.{css,tsx,ts} with regexes; allowlist web/src/culture-design/; exempt list must be empty by the end of wave A. File-disjoint from every other task.
- covers: c18, h18
- acceptance:
  - tests/lint/`webtokens_test.go` fails on any #rgb/#rrggbb, rgba(, font-family name or rem radius literal in web/src outside web/src/culture-design/ (a negative fixture proves it fires); the current tree passes after t3/t6/t7 land or the guard lists the files it exempts with a reason

### t5 — Worker presence: additive table written by the worker loop, names only

- instruction: Files: migrations/ (new), internal/store/postgres/workerpresence.go (+`_test`), internal/worker/presence.go (+`_test`) called from the poll loop with os.Hostname() and the configured `NODES_ACTOR_`\* key names, cmd/nodes/worker.go wiring. No API handler here (t5b).
- covers: c19, h19
- acceptance:
  - migrations/`00NN_worker_presence`.sql adds `worker_presence`(`worker_id`, hostname, revision, `actor_keys` text\[\], `last_seen`) additively; nodes worker and nodes all upsert a row on every poll; a worker test asserts a row within one poll interval
  - A store test rejects an `actor_keys` entry that looks like a token value (contains ':' or is longer than 64 chars) and asserts only key NAMES are stored; api/openapi.yaml has no secret-bearing field in the mesh payload

### t6 — GET /v1alpha1/mesh: timed collector, probe cache with `observed_at`, machines keyed on self-reported hostname

- instruction: Files: internal/api/mesh.go (+`_test`), internal/mesh/collector.go (+`_test`; timer, semaphore, http timeout, cache map with `observed_at`/error), cmd/nodes/serve.go wiring, api/openapi/openapi.yaml Mesh schema. Read-only routes under principalMiddleware like GET /actors. Do not read actors.`endpoint_ref`.
- depends on: t5
- covers: c23, h3, h26, c40, h32
- acceptance:
  - GET /v1alpha1/mesh returns actors (deduped), machines keyed by capabilities.preflight.host.hostname, the control plane's version, each bridge's cached deployment block with `observed_at` and error text, and worker presence rows; it never probes inside the request (a test with a never-answering bridge returns within normal latency with that machine unknown and its error text)
  - Payload is byte-identical when every actors.`endpoint_ref` is NULL; two actors share a machine only when they reported the same hostname; an actor with no reported hostname returns machine null
  - Probe failures are counted per target in a log line or metric; the collector has a per-probe timeout and bounded concurrency (config, tested)

### t7 — Mesh rebuilt on React Flow from real relationships; MeshCanvas deleted

- instruction: Files: web/src/domain/mesh.ts (rewrite: assemble typed graph from GET /mesh + runs + node-runs + workflows), web/src/routes/Mesh.tsx (React Flow + useElkLayout, terminal ground, inspect panel listing 'traces to' rows), web/src/fixtures/mesh-fixture.ts, web/e2e/mesh.spec.ts, api client getMesh. Delete MeshCanvas.tsx and its test. Reference: the Mesh view of docs/demos/web-ui-lift/culture-nodes-lifted.html.
- depends on: t3, t6, t2
- covers: c23, h3, c34, h24, c41, h33, c15, h17, h2
- acceptance:
  - routes/Mesh.tsx draws typed CultureNodes (machine, control plane, bridge/actor, human, workflow-by-key, active run) with edges only from the mesh payload: actor→machine, run→actor (node-run attribution), run→workflow (`workflow_digest`), actor→workflow (IR uses refs); web/src/components/MeshCanvas.tsx is deleted
  - Mesh e2e fixture: a failed bridge probe renders unknown with the probe's error text (never green); three versions of one `workflow_key` render one workflow node with count 3; two machines declaring different actor-key sets on one namespace render visibly different (the #224 fixture); an actor with machine null renders unattributed
  - \#agent-state mesh block keeps `actor_count`/`run_count`/`edge_count`/connection/`events_total` and adds `machine_count`/`probe_failures`/`unattributed_actors`; committed events still pulse one-to-one along the relevant edge; hover and keyboard inspection name the row each node traces to

### t8 — Design gallery: every published version as a graph, open its stored source

- instruction: Files: web/src/routes/Design.tsx (replaces NodeGraphs.tsx's NodeGraphsPanel; keep NodesPanel), web/src/domain/workflows.ts (source accessors), web/e2e/design.spec.ts, web/src/fixtures/workflows-fixture.ts, App.tsx redirects for /graphs and /workflows only (t8 owns the rest of App.tsx). Reference: Design view of docs/demos/web-ui-lift/culture-nodes-lifted.html.
- depends on: t3
- covers: c24, h4, c31, h21, c36, h28
- acceptance:
  - routes/Design.tsx lists workflow keys with versions from GET /workflows and draws the selected version's graph from `normalized_ir` through parseWorkflowGraph and CultureNode; web/e2e/design.spec.ts on a fixture with published workflows and zero runs asserts a graph with the expected node count for each key
  - Open in canvas / Open source shows the stored source byte-identical; gallery graph and source-parsed graph for the same digest have identical node and edge sets (vitest)
  - /graphs and /workflows redirect to /design; the Nodes sub-view (node-definition catalog) survives under /design?tab=nodes

### t9 — Navigation on the PRD §8.6 spine: eight links, one Runs page, redirects

- instruction: Files: web/src/components/Header.tsx (+test), web/src/App.tsx (+App.test.tsx route walk), web/src/routes/Runs.tsx (composes RunsList/RunsBoard/JobsTimeline bodies as projections), e2e specs that hardcode /board or /jobs updated to follow redirects. Do not restyle the toggle (t9 owns SegmentedToggle).
- depends on: t8
- covers: c25, h5
- acceptance:
  - Header.tsx renders exactly eight primary links — Your work, Inbox, Decisions | Design, Runs, Mesh, Ledger-and-plan, Statistics — asserted by name in Header.test.tsx
  - /runs hosts list, board and jobs behind ?view=list|board|jobs with a segmented toggle; /board and /jobs redirect; /design/new and /design/generate host the authoring pages; /workflows/new and /workflows/generate redirect; a route test walks every path today's App.tsx resolves and asserts each resolves directly or by redirect; `ROUTE_TITLES` updated

### t10 — Fluidity: no layout jump, one SegmentedToggle, mesh + active-graph screenshots

- instruction: Files: web/src/hooks/useElkLayout.ts, web/src/components/SegmentedToggle.tsx, small edits in RunView.tsx/Design.tsx/Runs.tsx/Decisions.tsx swapping the control, styles/app.css (.view-toggle rules), web/e2e/screenshots.spec.ts, web/e2e/no-jump.spec.ts.
- depends on: t7, t8, t9
- covers: c26, h6
- acceptance:
  - useElkLayout exposes ready; every canvas renders at opacity 0 until positions land then fades (skipped under reduced motion); a Playwright check asserts no node position changes after first paint on RunView and Design
  - components/SegmentedToggle.tsx replaces .view-toggle in RunView, Design, Runs and Decisions; the /decisions tab strip renders as it (the unstyled top-left buttons are gone); inbox/ticket/home e2e specs pass unchanged
  - web/e2e/screenshots.spec.ts captures mesh.png, mesh-dark.png and active-graphs.png from fixtures; `NODES_SHOTS` documentation updated

### t11 — Acceptance reference: the demo page committed with a walkthrough map

- instruction: The HTML is already copied into docs/demos/web-ui-lift/. Write the README (markdownlint clean; no angle-bracket placeholders). Nothing else in the tree changes.
- covers: c11, h13, c14, h16
- acceptance:
  - docs/demos/web-ui-lift/culture-nodes-lifted.html is tracked and docs/demos/web-ui-lift/README.md maps each of its six walkthrough checks (c30, c31, c32, c33, c34, fluid) to the e2e spec and fixture that proves it in the real app, and maps each of the six operator asks to the requirement and task that meets it
  - The README's before-state section cites scope entries s1–s9 by file so a reviewer can reproduce each on main as of 2026-09-03

### t12 — Spike: yaml Document round-trip baseline and validate proof, decision record

- instruction: Probe already showed flowCollectionPadding changes 4/82 lines by default. Try yaml Document toString options (flowCollectionPadding:false, lineWidth:0, indentSeq, etc.) until byte-identical; record what remains. Time-box: one session.
- depends on: t8
- covers: c27, h7
- acceptance:
  - docs/decisions/YYYY-MM-DD-design-canvas-spike.md records elapsed time, the exact stringify options that make the no-edit round-trip byte-identical on deploy/compose/testdata/smoke.workflow.yaml and the frame's own fixtures (or the residual difference classes if none do), the diff of one addIn node + one edge, and the POST /workflows/validate result; it ends with a go/no-go line the operator fills in
  - The spike runs in a throwaway worktree; no code from it merges except the decision record

### t13 — Reopen the issue-12 won't-do: decision record, Record issue, Feature issue

- instruction: Docs and issues only. Link both issues from the editor PR body.
- depends on: t12
- covers: c29, h9, c7, h12
- acceptance:
  - docs/decisions/YYYY-MM-DD-reopen-graphical-editor.md cites the 2026-08-15 brief verbatim, supersedes it, and cites the spike record; a Record issue points at it (scripts/open-issue.sh --type Record) and a Feature issue names the user task 'compose a workflow on canvas and publish it' with acceptance = c33; issue #12 is closed or re-scoped by that record

### t14 — workflow-document: surgical mutations on the author's YAML/JSON document

- instruction: Pure module + vitest golden files under web/src/domain/`__golden__`/; no UI. Reference behaviour: addNodeToSource/addEdgeToSource/setPropInSource in docs/demos/web-ui-lift/culture-nodes-lifted.html (text-surgical, comments kept).
- depends on: t12
- covers: c28, h10
- acceptance:
  - web/src/domain/workflow-document.ts wraps yaml.parseDocument with the spike's stringify options and exposes addNode, addEdge, setNodeProp, removeNode, removeEdge and toString; a commented fixture with a blank line, an anchor and a deliberate key order round-trips byte-identical through open → no edit → toString (golden-file test)
  - addNode + addEdge change only the inserted lines (golden diff test); a document whose mutation site holds a merge key is refused with a reason, never re-serialized; JSON-format sources take the same API

### t15 — Design canvas: palette, connect, properties, diagnostics on nodes, verbatim Publish

- instruction: Files: web/src/routes/DesignCanvas.tsx (+test), web/e2e/canvas.spec.ts, web/src/fixtures/canvas-fixture.ts, App.tsx route /design/canvas. Uses the workflow-document module, CultureNode, SegmentedToggle. Reference: the editor in docs/demos/web-ui-lift/culture-nodes-lifted.html. Digest equality via the fixture API's deterministic digest.
- depends on: t14, t13, t10
- covers: c28, h8, c33, h23, c39, h31
- acceptance:
  - routes/DesignCanvas.tsx: palette of kinds from deriveNodeDefinitions, drag or keyboard to add, connect via handles (isConnectable on), select/delete, per-kind property panel (agent: uses; code: runner/operation; decision: rule/outcomes; approval: authority/deadline; wait: signal; subworkflow: pinned digest), source pane showing the live document; every mouse action has a keyboard equivalent exercised by e2e
  - Validate (debounced) posts the document string to POST /workflows/validate and attaches each diagnostic to its node or edge by JSON path; the diagnostic text shown is byte-identical to the response
  - Publish ships the document string verbatim to the existing POST /workflows; an e2e publishes the same source via the canvas and via nodes workflow publish and asserts identical digests; when the digest already exists the canvas shows 'no semantic change — this version already exists; your comments live in your file' and offers Download (q4); a viewer-role principal gets the same refusal as on the text page; no new mutating route in openapi.yaml

### t16 — Operator review: before/after screenshots posted to #270, h12-style

- instruction: Human task for the operator; the agent captures both shot sets and drafts the comment body, the operator signs it.
- depends on: t10
- covers: c12, h14, h6, h24
- acceptance:
  - `NODES_SHOTS` before (main) and after (branch) directories captured; the operator posts the review comment on #270 naming each page and whether it reads less intimidating; the comment exists before the wave-A PR merges

### t17 — Walkthrough e2e: replay the demo's six checks against the built app

- instruction: One Playwright spec built on the existing fixtures; names like 'c30 no history replay'. It is the executable form of docs/demos/web-ui-lift/README.md.
- depends on: t15, t7
- covers: c1, h11, c13, h15, c30, c31, c32, c33, c34
- acceptance:
  - web/e2e/walkthrough.spec.ts performs, in order, the six checks the demo's acceptance walkthrough lists — mesh `events_total` 0 after load with 1,284 historical events; nightly-regression (0 runs) draws its graph from Design; the same CultureNode module renders Mesh, a run, the gallery and the canvas (DOM class + import guard); canvas add+connect+validate+publish yields the CLI digest; mesh shows actor→machine, run→actor, run→workflow edges and an unknown probe; no node position changes after first paint — each mapped to its spec id in the test name
  - The delivery summary names, per announced outcome, the walkthrough step that proves it; an outcome without a passing step is reported as not delivered

### t18 — Live test: the wave-A build on the deployed control plane, against real rows

- instruction: Operator lane (needs the Access cookie and a deploy). Do not stop or restart any bridge to provoke an unknown probe — use a bridge that is already down or the reachy host; if none, record 'not observable live'. Count the deploy as a hand-turn on #283 or its successor.
- depends on: t10, t11
- covers: c30, h20, c34, h24, c12, h14
- acceptance:
  - After deploy/prod/deploy.sh ships wave A to thor, the six walkthrough checks are run by hand on nodes.culture.dev (Access session) against the real namespace and recorded in docs/audits/YYYY-MM-DD-web-ui-lift-live-a.md with a screenshot per check: mesh `events_total` is 0 after a fresh load and 1 after one real committed event; spark, thor and orin appear as machines with their real revision and install mode; the #224-style key-count difference is visible or its absence is stated; a bridge that does not answer renders unknown with its error text
  - Every divergence between the live page and docs/demos/web-ui-lift/culture-nodes-lifted.html is listed in the audit with the spec id it touches; the audit ends with the operator's verdict line

### t19 — Validate-delivery pass: the agent runs the plan's behavioral tests and files evidence and deltas

- instruction: Agent task (Claude, operator lane). Use the vendored validate-delivery skill; input = docs/plans/2026-09-03-web-ui-lift.md + docs/demos/web-ui-lift/README.md map + the live audit.
- depends on: t17, t18
- covers: c1, h11, c13, h15, c11, h13
- acceptance:
  - /validate-delivery is run by the agent on the merged waves: every task's acceptance criteria are executed (vitest, Go tests, Playwright walkthrough, the live audit) and each result is filed on the plan as evidence or as a behavioral delta (added, amended, removed) through the devague CLI, never inside it; a failing or partial outcome is filed as such
  - The pass produces the list of divergences from the demo reference that the fixes cycle (next task) must close, each with the walkthrough check and spec id it affects

### t20 — Fixes cycle: close every divergence from the demo reference, then re-run the walkthrough and the live checks

- instruction: Bounded: at most two fix rounds; anything left after the second round is filed as a follow-up issue with its walkthrough check named, and reported as not delivered in the summary.
- depends on: t19
- covers: c26, h6, c22, h2, c28, h8
- acceptance:
  - Each divergence the validate-delivery pass listed is either fixed on the branch (with a test that would have caught it) or recorded as an approved deviation via /deviate — none is silently dropped; the walkthrough e2e (t17) and the live audit (t18, re-run as docs/audits/YYYY-MM-DD-web-ui-lift-live-b.md including wave B's canvas) are green afterwards
  - The operator reviews the fixed pages against the demo side by side and posts the verdict on #270; the delivery summary cites that comment

## Risks

- [unknown_nonblocking] Mesh layout algorithm: ELK layered (bundled, needs cycle breaking on a non-DAG mesh) versus a force layout (d3-force is a new web dependency). t7 decides and records; parked v4 on the frame. (task t7)
- [unknown_nonblocking] Server tasks t1, t5, t6 need a Postgres-backed test run; a codex sandbox cannot open sockets (#119), so they run on a lane with a database or the operator merges them with local tests. (task t6)
- [unknown_nonblocking] yaml Document stringify options may not reach byte-identical on every fixture; if a residual class remains, q4 already covers comment-only republish but the spike must say so before the workflow-document task starts. (task t12)
- [unknown_nonblocking] The bridge write path is unproven (#18); web tasks routed to codex actors may need the operator to land the patch by hand — counted as hand-turns.
- [unknown_nonblocking] Wave A: t8 (design gallery) and t9 (nav) both edit App.tsx; t8 keeps its edit to the two Navigate lines so t9's merge stays trivial. (task t9)
- [unknown_nonblocking] t18 and t20's live checks depend on an operator Access session, a deploy to thor, and real fleet state; if a bridge is never down the unknown-probe check is recorded as not observable live rather than faked. (task t18)
