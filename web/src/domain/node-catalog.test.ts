import { describe, expect, it } from "vitest";
import type { WorkflowIR, WorkflowVersion } from "../api/types";
import {
  NODE_CATALOG_DEFINITION_COUNT,
  NODE_CATALOG_GRAPH_COUNT,
  NODE_CATALOG_LINK_COUNT,
  NODE_CATALOG_WORKFLOW_VERSIONS,
} from "../fixtures/workflows-fixture";
import {
  deriveCrossWorkflowLinks,
  deriveGraphCatalog,
  deriveNodeDefinitions,
} from "./node-catalog";

describe("deriveNodeDefinitions", () => {
  it("derives the fixture's deterministic definition count", () => {
    const definitions = deriveNodeDefinitions(NODE_CATALOG_WORKFLOW_VERSIONS);
    expect(definitions).toHaveLength(NODE_CATALOG_DEFINITION_COUNT);
  });

  it("returns the same output on repeated calls (no mutation, no ordering drift)", () => {
    const first = deriveNodeDefinitions(NODE_CATALOG_WORKFLOW_VERSIONS);
    const second = deriveNodeDefinitions(NODE_CATALOG_WORKFLOW_VERSIONS);
    expect(second).toEqual(first);
  });

  it("sorts definitions by id", () => {
    const definitions = deriveNodeDefinitions(NODE_CATALOG_WORKFLOW_VERSIONS);
    const ids = definitions.map((d) => d.id);
    expect(ids).toEqual([...ids].sort((a, b) => a.localeCompare(b)));
  });

  it("keys an agent node's identity by kind + actor ref (uses)", () => {
    const definitions = deriveNodeDefinitions(NODE_CATALOG_WORKFLOW_VERSIONS);
    const intake = definitions.find(
      (d) => d.id === "agent:actor://company/intake@sha256:111111",
    );
    expect(intake).toBeDefined();
    expect(intake?.kind).toBe("agent");
    expect(intake?.ref).toBe("actor://company/intake@sha256:111111");
  });

  it("keys a code node's identity by kind + runner ref (uses)", () => {
    const definitions = deriveNodeDefinitions(NODE_CATALOG_WORKFLOW_VERSIONS);
    const test = definitions.find(
      (d) => d.id === "code:runner://headspace/docker@sha256:555555",
    );
    expect(test).toBeDefined();
    expect(test?.kind).toBe("code");
    expect(test?.occurrences).toEqual([
      { workflowKey: "deliver-change", version: 2, nodeId: "test" },
    ]);
  });

  it("keys an approval node's identity by kind + approver ref", () => {
    const definitions = deriveNodeDefinitions(NODE_CATALOG_WORKFLOW_VERSIONS);
    const humanReview = definitions.find(
      (d) => d.id === "approval:group/platform-ai-approvers",
    );
    expect(humanReview).toBeDefined();
    expect(humanReview?.ref).toBe("group/platform-ai-approvers");
  });

  it("collapses no-ref nodes of the same kind across workflows into one definition", () => {
    const definitions = deriveNodeDefinitions(NODE_CATALOG_WORKFLOW_VERSIONS);
    const end = definitions.find((d) => d.id === "end");
    expect(end).toBeDefined();
    expect(end?.ref).toBeUndefined();
    // deliver-change, hello-world, and notify-team each have a bare
    // `finish` (kind: end) node — none of them carry a distinguishing ref.
    expect(
      end?.occurrences.map((o) => `${o.workflowKey}/${o.nodeId}`),
    ).toEqual([
      "deliver-change/finish",
      "hello-world/finish",
      "notify-team/finish",
    ]);
  });

  it("gives an agent node with no `uses` its own unbound definition, distinct from ref-backed agent definitions", () => {
    const definitions = deriveNodeDefinitions(NODE_CATALOG_WORKFLOW_VERSIONS);
    const unboundAgent = definitions.find((d) => d.id === "agent");
    expect(unboundAgent).toBeDefined();
    expect(unboundAgent?.ref).toBeUndefined();
    expect(unboundAgent?.occurrences).toEqual([
      { workflowKey: "hello-world", version: 1, nodeId: "greet" },
    ]);
  });

  it("only derives from the latest version of each workflow_key", () => {
    // deliver-change v1 and v2 share identical nodes/edges in the fixture
    // (only metadata.version differs) — asserting the definition count
    // stays fixed regardless proves this isn't double-counting both
    // versions' nodes.
    const definitions = deriveNodeDefinitions(NODE_CATALOG_WORKFLOW_VERSIONS);
    const intakeOccurrences = definitions.find(
      (d) => d.id === "agent:actor://company/intake@sha256:111111",
    )?.occurrences;
    expect(
      intakeOccurrences?.filter((o) => o.workflowKey === "deliver-change"),
    ).toHaveLength(1);
  });

  it("returns an empty list for no versions", () => {
    expect(deriveNodeDefinitions([])).toEqual([]);
  });
});

describe("deriveGraphCatalog", () => {
  it("derives the fixture's deterministic graph count — one per workflow_key", () => {
    const catalog = deriveGraphCatalog(NODE_CATALOG_WORKFLOW_VERSIONS);
    expect(catalog).toHaveLength(NODE_CATALOG_GRAPH_COUNT);
  });

  it("orders entries alphabetically by workflow_key", () => {
    const catalog = deriveGraphCatalog(NODE_CATALOG_WORKFLOW_VERSIONS);
    expect(catalog.map((e) => e.workflowKey)).toEqual([
      "deliver-change",
      "hello-world",
      "notify-team",
    ]);
  });

  it("uses the latest version's digest and number, not the oldest", () => {
    const catalog = deriveGraphCatalog(NODE_CATALOG_WORKFLOW_VERSIONS);
    const deliver = catalog.find((e) => e.workflowKey === "deliver-change")!;
    expect(deliver.version).toBe(2);
  });

  it("reports node/edge counts and the entry node from the parsed graph", () => {
    const catalog = deriveGraphCatalog(NODE_CATALOG_WORKFLOW_VERSIONS);
    const deliver = catalog.find((e) => e.workflowKey === "deliver-change")!;
    expect(deliver.entry).toBe("intake");
    expect(deliver.nodeCount).toBe(7);
    expect(deliver.edgeCount).toBe(10);

    const notify = catalog.find((e) => e.workflowKey === "notify-team")!;
    expect(notify.entry).toBe("notify");
    expect(notify.nodeCount).toBe(2);
    expect(notify.edgeCount).toBe(1);
  });

  it("flags a workflow with a loop edge (verify.changes_required -> build) as hasLoop", () => {
    const catalog = deriveGraphCatalog(NODE_CATALOG_WORKFLOW_VERSIONS);
    const deliver = catalog.find((e) => e.workflowKey === "deliver-change")!;
    expect(deliver.hasLoop).toBe(true);
  });

  it("does not flag a loop-free workflow", () => {
    const catalog = deriveGraphCatalog(NODE_CATALOG_WORKFLOW_VERSIONS);
    const hello = catalog.find((e) => e.workflowKey === "hello-world")!;
    expect(hello.hasLoop).toBe(false);
    const notify = catalog.find((e) => e.workflowKey === "notify-team")!;
    expect(notify.hasLoop).toBe(false);
  });

  it("returns an empty list for no versions", () => {
    expect(deriveGraphCatalog([])).toEqual([]);
  });
});

describe("deriveCrossWorkflowLinks", () => {
  it("derives the fixture's deterministic link count", () => {
    const links = deriveCrossWorkflowLinks(NODE_CATALOG_WORKFLOW_VERSIONS);
    expect(links).toHaveLength(NODE_CATALOG_LINK_COUNT);
  });

  it("links deliver-change's intake node and notify-team's notify node via the shared actor ref", () => {
    const links = deriveCrossWorkflowLinks(NODE_CATALOG_WORKFLOW_VERSIONS);
    const link = links[0];
    expect(link.ref).toBe("actor://company/intake@sha256:111111");
    expect(link.kind).toBe("agent");
    expect(
      link.occurrences.map((o) => `${o.workflowKey}/${o.nodeId}`),
    ).toEqual(["deliver-change/intake", "notify-team/notify"]);
  });

  it("excludes definitions that appear in only one workflow", () => {
    const links = deriveCrossWorkflowLinks(NODE_CATALOG_WORKFLOW_VERSIONS);
    // plan/build/verify/test each appear in exactly one workflow_key
    // (deliver-change) — none of them should surface as a link.
    const refs = links.map((l) => l.ref);
    expect(refs).not.toContain("actor://company/planner@sha256:222222");
    expect(refs).not.toContain("actor://company/developer@sha256:333333");
    expect(refs).not.toContain("actor://company/verifier@sha256:444444");
    expect(refs).not.toContain("runner://headspace/docker@sha256:555555");
  });

  it("excludes no-ref definitions (kind alone is never a link, however many workflows share it)", () => {
    const links = deriveCrossWorkflowLinks(NODE_CATALOG_WORKFLOW_VERSIONS);
    // The "end" definition spans deliver-change and hello-world (two
    // workflows) but has no ref — it must never appear as a link.
    expect(links.some((l) => l.kind === "end")).toBe(false);
  });

  it("excludes approval-node approver refs even if shared across workflows", () => {
    const sharedApprover = "group/platform-ai-approvers";
    const versions: WorkflowVersion[] = [
      ...NODE_CATALOG_WORKFLOW_VERSIONS,
      approvalWorkflowVersion("second-approval-flow", sharedApprover),
    ];
    const links = deriveCrossWorkflowLinks(versions);
    expect(links.some((l) => l.ref === sharedApprover)).toBe(false);
  });

  it("returns an empty list for no versions", () => {
    expect(deriveCrossWorkflowLinks([])).toEqual([]);
  });
});

/** A minimal one-node, approval-kind workflow version, for the approver-exclusion test above. */
function approvalWorkflowVersion(
  workflowKey: string,
  approverRef: string,
): WorkflowVersion {
  const ir: WorkflowIR = {
    metadata: { name: workflowKey, version: "1.0.0" },
    spec: {
      entry: "review",
      nodes: {
        review: {
          kind: "approval",
          approverRef,
          outcomes: ["approved", "rejected"],
        },
      },
      edges: [],
    },
  };
  return {
    id: `wfv-${workflowKey}-1`,
    workflow_key: workflowKey,
    version: 1,
    source_format: "yaml",
    source: "# fixture\n",
    normalized_ir: ir,
    digest: `sha256:${workflowKey}`,
    created_at: "2026-08-09T09:08:00Z",
  };
}
