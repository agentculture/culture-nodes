/**
 * Fixture data for the Workflows view (`/workflows`, task t8): two published
 * workflow_keys — `deliver-change` (two versions, reusing run-fixture.ts's
 * `WORKFLOW_IR`/`WORKFLOW_DIGEST` for its v1 so the existing Run-view
 * fixtures still resolve when a workflow card's recent run is followed) and
 * a second, single-version `hello-world` — plus a spread of runs across
 * every digest, used by both the vitest route test and the Playwright e2e
 * spec via request interception (e2e/fixtures/api.ts).
 *
 * One run carries a digest that matches no published version
 * (`ORPHAN_DIGEST`) — the honesty condition the domain layer's test already
 * covers (a run with no matching workflow group renders nowhere), exercised
 * here end-to-end too: it must never show up under either workflow card.
 *
 * Plain TypeScript, like run-fixture.ts, so both the app's tsconfig and the
 * Playwright/node one compile it.
 */

import type { Run, WorkflowIR, WorkflowVersion } from "../api/types";
import { WORKFLOW_DIGEST, WORKFLOW_IR } from "./run-fixture";

export const DELIVER_CHANGE_V2_DIGEST =
  "sha256:7b1c4d9e2f5a8b3c6d0e9f2a5b8c1d4e7f0a3b6c9d2e5f8a1b4c7d0e3f6a9b2c";
export const HELLO_WORLD_DIGEST =
  "sha256:a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9";
export const ORPHAN_DIGEST =
  "sha256:0000000000000000000000000000000000000000000000000000000000ff";

const HELLO_WORLD_IR: WorkflowIR = {
  apiVersion: "nodes.culture.dev/v1alpha1",
  kind: "Workflow",
  metadata: {
    name: "hello-world",
    version: "1.0.0",
    ownerRef: "team/platform-ai",
  },
  spec: {
    entry: "greet",
    nodes: {
      greet: { kind: "agent", ownerRef: "team/platform-ai", outcomes: ["completed"] },
      finish: { kind: "end", ownerRef: "team/platform-ai", outcomes: [] },
    },
    edges: [{ from: "greet.completed", to: "finish" }],
  },
};

const t = (minute: number, second = 0) =>
  `2026-08-09T09:${String(minute).padStart(2, "0")}:${String(second).padStart(2, "0")}Z`;

/** `GET /v1alpha1/workflows`' flat, unsorted version list — the view groups it. */
export const WORKFLOW_VERSIONS: WorkflowVersion[] = [
  {
    id: "wfv-deliver-change-1",
    workflow_key: "deliver-change",
    version: 1,
    source_format: "yaml",
    source: "# see schemas/examples/deliver-change.workflow.json\n",
    normalized_ir: WORKFLOW_IR,
    digest: WORKFLOW_DIGEST,
    created_at: t(0),
  },
  {
    id: "wfv-deliver-change-2",
    workflow_key: "deliver-change",
    version: 2,
    source_format: "yaml",
    source: "# v2: adds the human-review deadline\n",
    normalized_ir: {
      ...WORKFLOW_IR,
      metadata: { ...WORKFLOW_IR.metadata, version: "1.1.0" },
    },
    digest: DELIVER_CHANGE_V2_DIGEST,
    created_at: t(40),
  },
  {
    id: "wfv-hello-world-1",
    workflow_key: "hello-world",
    version: 1,
    source_format: "yaml",
    source: "# a minimal single-node workflow\n",
    normalized_ir: HELLO_WORLD_IR,
    digest: HELLO_WORLD_DIGEST,
    created_at: t(5),
  },
];

/**
 * Extra fixture data for the cross-workflow node catalog (task t29,
 * domain/node-catalog.ts). `WORKFLOW_VERSIONS`'s two workflow_keys
 * (`deliver-change`, `hello-world`) share no actor/runner ref, so there is
 * nothing in the base fixture to exercise cross-workflow linkage derivation
 * against. This adds a third workflow, `notify-team`, whose single node
 * deliberately reuses `deliver-change`'s `intake` node actor ref
 * (`actor://company/intake@sha256:111111`, see run-fixture.ts's
 * `WORKFLOW_IR`) — the fixture's one intentional cross-workflow coincidence.
 *
 * Kept as an *additive* export (`NODE_CATALOG_WORKFLOW_VERSIONS =
 * WORKFLOW_VERSIONS + this`) rather than folding into `WORKFLOW_VERSIONS`
 * itself, because `web/e2e/workflows.spec.ts:164` pins
 * `expect(WORKFLOW_VERSIONS).toHaveLength(3)` and `web/src/routes/
 * Workflows.test.tsx` renders `WORKFLOW_VERSIONS` directly — growing that
 * array would break both.
 */
export const NOTIFY_TEAM_DIGEST =
  "sha256:9f8e7d6c5b4a39281706f5e4d3c2b1a0f9e8d7c6b5a4938271605f4e3d2c1b0a";

const NOTIFY_TEAM_IR: WorkflowIR = {
  apiVersion: "nodes.culture.dev/v1alpha1",
  kind: "Workflow",
  metadata: {
    name: "notify-team",
    version: "1.0.0",
    ownerRef: "team/platform-ai",
  },
  spec: {
    entry: "notify",
    nodes: {
      notify: {
        kind: "agent",
        ownerRef: "team/platform-ai",
        // Same actor ref as deliver-change's `intake` node — the deliberate
        // cross-workflow coincidence this fixture exists to exercise.
        uses: "actor://company/intake@sha256:111111",
        outcomes: ["completed"],
      },
      finish: { kind: "end", ownerRef: "team/platform-ai", outcomes: [] },
    },
    edges: [{ from: "notify.completed", to: "finish" }],
  },
};

export const NOTIFY_TEAM_VERSION: WorkflowVersion = {
  id: "wfv-notify-team-1",
  workflow_key: "notify-team",
  version: 1,
  source_format: "yaml",
  source: "# a single-node workflow sharing deliver-change's intake actor\n",
  normalized_ir: NOTIFY_TEAM_IR,
  digest: NOTIFY_TEAM_DIGEST,
  created_at: "2026-08-09T09:07:00Z",
};

/** `GET /v1alpha1/runs?sort=updated_at`, newest first — as the view requests it. */
export const WORKFLOWS_RUNS: Run[] = [
  {
    id: "run-hello-01J8XKWORKFLOWS0001",
    workflow_digest: HELLO_WORLD_DIGEST,
    state: "completed",
    created_at: t(50),
    updated_at: t(58),
    completed_at: t(58),
  },
  {
    id: "run-deliver-v2-01J8XKWORKFLOWS02",
    workflow_digest: DELIVER_CHANGE_V2_DIGEST,
    state: "running",
    created_at: t(45),
    updated_at: t(56),
  },
  {
    id: "run-orphan-01J8XKWORKFLOWS0003",
    workflow_digest: ORPHAN_DIGEST,
    state: "failed",
    created_at: t(20),
    updated_at: t(44),
    completed_at: t(44),
  },
  {
    id: "run-deliver-v1-01J8XKWORKFLOWS04",
    workflow_digest: WORKFLOW_DIGEST,
    state: "completed",
    created_at: t(1),
    updated_at: t(10),
    completed_at: t(10),
  },
];

/**
 * Which published `workflow_key` each fixture digest belongs to — the join
 * `GET /v1alpha1/runs` itself makes server-side (`runs` JOIN
 * `workflow_versions`, internal/api/queries.go). `ORPHAN_DIGEST` is
 * deliberately absent: it belongs to no published version, so no
 * workflow_key query can ever return that run.
 */
const WORKFLOW_KEY_BY_DIGEST: Record<string, string> = {
  [WORKFLOW_DIGEST]: "deliver-change",
  [DELIVER_CHANGE_V2_DIGEST]: "deliver-change",
  [HELLO_WORLD_DIGEST]: "hello-world",
};

/**
 * What `GET /v1alpha1/runs?workflow_key=<key>` answers for this fixture
 * (task t8, using the filter task t7 added) — the per-card query the Node
 * Graphs sub-tab now makes, one per published workflow_key, instead of
 * filtering one global listing client-side.
 *
 * `notify-team` (NODE_CATALOG_WORKFLOW_VERSIONS) has no runs at all, so it
 * answers `[]` — the fixture's honest "No runs yet" workflow.
 */
export function workflowsRunsFor(workflowKey: string): Run[] {
  return WORKFLOWS_RUNS.filter(
    (run) => WORKFLOW_KEY_BY_DIGEST[run.workflow_digest] === workflowKey,
  );
}

/**
 * The unfiltered `GET /v1alpha1/runs` window as production actually looks
 * (task t8, claim c8): a single high-frequency workflow — the pr-upkeep
 * sweep, minting a run every few minutes — fills the default 50-row page,
 * so NOT ONE run of any other workflow appears in it. Filling each card's
 * "recent runs" from this list is exactly how every workflow came to say
 * "No runs yet" while having hundreds of runs.
 *
 * The Active Graphs sub-tab still reads the unfiltered listing (it asks
 * "what is alive right now" across all workflows, not per card), so this
 * stays a separate export rather than replacing WORKFLOWS_RUNS.
 */
export const SWEEP_DIGEST =
  "sha256:5eee9100000000000000000000000000000000000000000000000000000000ab";

export const SWEEP_DOMINATED_RUNS: Run[] = Array.from(
  { length: 50 },
  (_, i): Run => ({
    id: `run-sweep-01J8XKSWEEP${String(i).padStart(6, "0")}`,
    workflow_digest: SWEEP_DIGEST,
    state: "completed",
    created_at: t(59, 59 - i),
    updated_at: t(59, 59 - i),
    completed_at: t(59, 59 - i),
  }),
);

/**
 * The version list `domain/node-catalog.test.ts` derives its catalog from:
 * `WORKFLOW_VERSIONS` (deliver-change x2 + hello-world) plus `notify-team`.
 * Three distinct workflow_keys once grouped by latest version.
 */
export const NODE_CATALOG_WORKFLOW_VERSIONS: WorkflowVersion[] = [
  ...WORKFLOW_VERSIONS,
  NOTIFY_TEAM_VERSION,
];

/**
 * Deterministic counts for `NODE_CATALOG_WORKFLOW_VERSIONS`, computed by
 * hand from the latest version of each workflow_key (deliver-change v2,
 * hello-world v1, notify-team v1) — asserted against by name in
 * `domain/node-catalog.test.ts` rather than as inline literals, per the e2e
 * `MESH_ACTOR_NODE_COUNT` convention (web/e2e/mesh.spec.ts).
 *
 * Node-definition identity is kind + `uses`/`approverRef` (see
 * `domain/node-catalog.ts`'s doc comment). deliver-change's latest version
 * contributes 7 distinct definitions (intake/plan/build/verify — agent,
 * each with a distinct actor ref; test — code; human-review — approval;
 * finish — end, no ref). hello-world's `greet` node is an agent with no
 * `uses`, so it mints one more definition ("unbound agent"); hello-world's
 * `finish` node collapses into deliver-change's existing "end" definition
 * (same kind, no ref, so no ref to distinguish them). notify-team's single
 * node reuses deliver-change's `intake` actor ref outright, so it adds an
 * *occurrence* to an existing definition rather than a new one.
 */
export const NODE_CATALOG_DEFINITION_COUNT = 8;

/** One graph-catalog entry per distinct workflow_key. */
export const NODE_CATALOG_GRAPH_COUNT = 3;

/**
 * One cross-workflow link: deliver-change's `intake` node and notify-team's
 * `notify` node both use `actor://company/intake@sha256:111111`.
 */
export const NODE_CATALOG_LINK_COUNT = 1;
