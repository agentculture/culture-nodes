# web-ui-lift — the demo page is the acceptance reference

`culture-nodes-lifted.html` is a fixture-only, browsable demo of the lifted
web UI, designed before the build (frame `web-ui-lift`, spec
`docs/specs/2026-09-03-web-ui-lift.md`). It carries no real rows and says so in
its header. It is the reference the built app is judged against: its bottom bar
lists six acceptance checks, and every one of them has an executable twin in the
real app's test suite (task t17, `web/e2e/walkthrough.spec.ts`).

Open it from the repo (`open docs/demos/web-ui-lift/culture-nodes-lifted.html`)
or from the published artifact the operator keeps.

## The six walkthrough checks and their executable twins

| Check on the demo's bar | Spec id | Proved in the real app by | Fixture |
| --- | --- | --- | --- |
| No history replay on arrival: events since load stays 0, replayed: 0 | c30 / h20 | `web/e2e/mesh.spec.ts` asserts `#agent-state` `mesh.events_total == 0` after load; `internal/api/events_test.go` asserts a `?from=latest` connect streams nothing at or before the snapshot marker | `web/src/fixtures/mesh-fixture.ts` with 1,284 historical events |
| Idle workflows have a graph: nightly-regression (0 runs) draws its full graph from Design | c31 / h21 | `web/e2e/design.spec.ts` asserts a graph with the expected node count for each key on a fixture with zero runs | `web/src/fixtures/workflows-fixture.ts` |
| One node component everywhere: the same node draws Mesh, a run, the gallery and the canvas | c32 / h22 | `tests/lint/culturenode_test.go` pins that all five views import `web/src/culture-design/CultureNode.tsx` and no second node card exists | — |
| Canvas publish equals CLI publish: add a node, connect, validate, publish; the two digests are identical | c33 / h23 | `web/e2e/canvas.spec.ts` publishes the same source through the canvas and through the CLI path and asserts identical digests; diagnostics are byte-identical to the validate response | `web/src/fixtures/canvas-fixture.ts` |
| Mesh draws real relationships: actor→machine, actor→workflow, run→actor, run→workflow; a failed probe renders unknown | c34 / h24 | `web/e2e/mesh.spec.ts` asserts the four edge kinds from rows, the unknown machine with its probe error text, one workflow node per key with a version count, and the two-machine key-count difference | `web/src/fixtures/mesh-fixture.ts` |
| Fluid, no layout jump; one segmented toggle; pinch and drag work | c26 / h6 | `web/e2e/no-jump.spec.ts` asserts no node position changes after first paint on RunView and Design; the `/decisions` strip renders as `SegmentedToggle`; `web/e2e/screenshots.spec.ts` captures mesh and active-graph shots for the operator's review on #270 | existing fixtures |

The walkthrough spec names each test by its spec id (for example
`c30 no history replay`) so the delivery summary can say, per announced outcome,
which step proves it. An outcome without a passing step is reported as not
delivered.

## The six operator asks and where each is met

| Ask (2026-09-03) | Requirement | Task |
| --- | --- | --- |
| The mesh must not replay history on return | c2, c37 | t1, t2 |
| Graphs must be visible for any workflow, not only running ones | c24 | t8 |
| Workflows must be composable with the mouse, not only as text | c28 (gated by c27, c29) | t12, t13, t14, t15 |
| The whole site must feel more intuitive and fluid | c25, c26 | t9, t10 |
| The graph style should be the mesh node style | c22 | t3 |
| The mesh must convey information about its nodes | c23, c40, c41 | t5, t6, t7 |

## Before-state, reproducible on main as of 2026-09-03

Each sentence of the spec's before-state cites a scope entry; a reviewer can
reproduce each on `main` at commit `f9d50e9`:

- s1 — `internal/api/events.go` treats a cursor-less connect as "from the
  beginning of this namespace's event log"; `web/src/hooks/useSharedEvents.tsx`
  connects with no cursor on every page load; `web/src/routes/Mesh.tsx` animates
  every replayed lifecycle event with a 3.4 s linger.
- s2 — `web/src/domain/mesh.ts` emits only actor→control-plane and run→actor
  edges; `layoutMesh` places actors on a ring.
- s3 — `MeshCanvas.tsx` (Canvas-2D) and `ActiveGraphCanvas.tsx` /
  `NodeCard.tsx` (React Flow + DOM) share tokens but no node component.
- s4 — `web/src/routes/NodeGraphs.tsx`: the "Node Graphs" sub-tab renders cards,
  never a graph; only Active Graphs draws one, and only for non-terminal runs.
- s5 — `web/src/routes/AuthorWorkflow.tsx` is a textarea with a read-only
  preview; every handle in `WorkflowNode.tsx` has `isConnectable={false}`.
- s6 — `docs/decisions/issue-12-remaining-web-ux-scope.md` recommended won't-do
  for on-canvas mutation, with an explicit reopen rule.
- s7 — `web/src/components/Header.tsx` offers twelve destinations; three are
  projections of one dataset; `/workflows/new` is not in the nav.
- s8 — issues #270, #226 and #12 hold the prior UX findings; the screenshot pass
  has no mesh or active-graph coverage.
- s9 — PRD §8.4 and §8.8 and the h14 comments bind honesty and accessibility.

## What the demo deliberately does not claim

It is not a rendering of production: machine names, revisions, runs and the
1,284-event history are fixtures. The canvas's "YAML kept" behaviour is a text
model of the real `workflow-document` module (task t14), and its publish digest
is a local SHA-256 over a canonical form, standing in for the compiler's.
