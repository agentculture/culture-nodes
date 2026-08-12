/**
 * Fixture data for the authoring slice (`/workflows/new`, task t9): a
 * two-node valid document, an invalid one (an edge to an undeclared node),
 * and the `WorkflowValidation`/`WorkflowVersion` payloads the fixture API
 * (`POST /v1alpha1/workflows/validate`, `POST /v1alpha1/workflows`) answers
 * with — used by both the vitest component test and the Playwright e2e spec
 * via request interception (e2e/fixtures/api.ts).
 *
 * The valid source's `spec.entry`/`spec.nodes`/`spec.edges` shape is exactly
 * what `domain/workflow-source.ts`'s client-side preview parse expects — see
 * that module's docstring for why the authored document and the normalized
 * IR share the same topology.
 */

import type { WorkflowValidation, WorkflowVersion } from "../api/types";

export const AUTHORING_DIGEST =
  "sha256:9f3e2d1c4b5a69788695041e2f3c4b5a69788695041e2f3c4b5a69788695041";

export const VALID_YAML_SOURCE = `apiVersion: nodes.culture.dev/v1alpha1
kind: Workflow

metadata:
  name: hand-authored
  version: 1.0.0
  ownerRef: team/platform-ai

spec:
  entry: greet
  nodes:
    greet:
      kind: agent
      ownerRef: team/platform-ai
      outcomes: [completed]
    finish:
      kind: end
      ownerRef: team/platform-ai
  edges:
    - from: greet.completed
      to: finish
`;

export const INVALID_YAML_SOURCE = `apiVersion: nodes.culture.dev/v1alpha1
kind: Workflow

metadata:
  name: broken

spec:
  entry: greet
  nodes:
    greet:
      kind: agent
  edges:
    - from: greet.completed
      to: does-not-exist
`;

export const VALID_VALIDATION: WorkflowValidation = {
  valid: true,
  digest: AUTHORING_DIGEST,
  diagnostics: [],
};

export const INVALID_VALIDATION: WorkflowValidation = {
  valid: false,
  digest: "",
  diagnostics: [
    {
      level: "error",
      path: "/spec/edges/0/to",
      code: "unknown-node",
      message: 'edge target "does-not-exist" is not a declared node',
      hint: 'declare a node named "does-not-exist" or point the edge at an existing one',
    },
  ],
};

export const PUBLISHED_VERSION: WorkflowVersion = {
  id: "wfv-hand-authored-1",
  workflow_key: "hand-authored",
  version: 1,
  source_format: "yaml",
  source: VALID_YAML_SOURCE,
  normalized_ir: {
    apiVersion: "nodes.culture.dev/v1alpha1",
    kind: "Workflow",
    metadata: {
      name: "hand-authored",
      version: "1.0.0",
      ownerRef: "team/platform-ai",
    },
    spec: {
      entry: "greet",
      nodes: {
        greet: {
          kind: "agent",
          ownerRef: "team/platform-ai",
          outcomes: ["completed"],
        },
        finish: { kind: "end", ownerRef: "team/platform-ai" },
      },
      edges: [{ from: "greet.completed", to: "finish" }],
    },
  },
  digest: AUTHORING_DIGEST,
  created_at: "2026-08-12T09:00:00Z",
};
