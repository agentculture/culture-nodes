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
