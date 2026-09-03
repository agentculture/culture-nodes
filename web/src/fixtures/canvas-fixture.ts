import type { WorkflowValidation, WorkflowVersion } from "../api/types";

export const CANVAS_SOURCE = `apiVersion: nodes.culture.dev/v1alpha1
kind: Workflow
metadata:
  name: canvas-fixture
  version: 1.0.0
spec:
  entry: start
  nodes:
    start:
      kind: agent
      uses: actor://company/start@sha256:111111
    code:
      kind: code
      uses: runner://company/code@sha256:222222
    decision:
      kind: decision
    approval:
      kind: approval
      approverRef: role://release
    wait:
      kind: wait
    child:
      kind: subworkflow
    finish:
      kind: end
  edges:
    - from: start.completed
      to: finish
# comments live here
`;

export const CANVAS_DIGEST =
  "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";

export const CANVAS_VALIDATION: WorkflowValidation = {
  valid: true,
  digest: CANVAS_DIGEST,
  diagnostics: [],
};

export const CANVAS_INVALID_VALIDATION: WorkflowValidation = {
  valid: false,
  digest: "",
  diagnostics: [
    {
      level: "error",
      path: "/spec/nodes/start/uses",
      code: "actor-not-found",
      message: "Actor ref — byte-for-byte: café ☕",
      hint: "Pin a registered actor digest.",
    },
    {
      level: "error",
      path: "/spec/edges/0/to",
      code: "unknown-node",
      message: 'edge target "missing" is not declared',
      hint: "Choose a declared node.",
    },
  ],
};

export const CANVAS_VERSION: WorkflowVersion = {
  id: "wfv-canvas-1",
  workflow_key: "canvas-fixture",
  version: 1,
  source_format: "yaml",
  source: CANVAS_SOURCE,
  digest: CANVAS_DIGEST,
  created_at: "2026-09-03T08:00:00Z",
  normalized_ir: {
    apiVersion: "nodes.culture.dev/v1alpha1",
    kind: "Workflow",
    metadata: { name: "canvas-fixture", version: "1.0.0" },
    spec: {
      entry: "start",
      nodes: {
        start: { kind: "agent", uses: "actor://company/start@sha256:111111" },
        code: { kind: "code", uses: "runner://company/code@sha256:222222" },
        decision: { kind: "decision" },
        approval: { kind: "approval", approverRef: "role://release" },
        wait: { kind: "wait" },
        child: { kind: "subworkflow" },
        finish: { kind: "end" },
      },
      edges: [{ from: "start.completed", to: "finish" }],
    },
  },
};
