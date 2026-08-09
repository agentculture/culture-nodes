import { describe, expect, it } from "vitest";
import { WORKFLOW_IR } from "../fixtures/run-fixture";
import { accentFor, paletteColorFor, parseWorkflowGraph, splitEdgeSource } from "./graph";
import { NODE_KIND_PALETTE, TERMINAL_PALETTE } from "../culture-design/palette";

describe("splitEdgeSource", () => {
  it("splits node and outcome on the last dot", () => {
    expect(splitEdgeSource("verify.changes_required")).toEqual({
      node: "verify",
      outcome: "changes_required",
    });
  });

  it("treats a bare node id as having no outcome", () => {
    expect(splitEdgeSource("finish")).toEqual({ node: "finish", outcome: "" });
  });
});

describe("parseWorkflowGraph", () => {
  const graph = parseWorkflowGraph(WORKFLOW_IR);

  it("orders nodes breadth-first from the entry, so tab order follows execution", () => {
    expect(graph.nodes.map((node) => node.id)).toEqual([
      "intake",
      "plan",
      "build",
      "test",
      "verify",
      "finish",
      "human-review",
    ]);
  });

  it("carries kind, owner and outcomes through from the IR", () => {
    const test = graph.nodes.find((node) => node.id === "test");
    expect(test?.kind).toBe("code");
    expect(test?.ownerRef).toBe("team/developer-experience");
    expect(test?.outcomes).toEqual(["passed", "failed"]);
  });

  it("keys edges by source.outcome->target", () => {
    expect(graph.edges.map((edge) => edge.id)).toContain(
      "verify.changes_required->build",
    );
  });

  it("flags the changes_required return path as a loop", () => {
    const loop = graph.edges.find(
      (edge) => edge.id === "verify.changes_required->build",
    );
    expect(loop?.loop).toBe(true);
  });

  it("does not flag forward edges as loops", () => {
    const forward = graph.edges.find(
      (edge) => edge.id === "build.completed->test",
    );
    expect(forward?.loop).toBe(false);
  });
});

describe("palette mapping", () => {
  it("maps every workflow node kind onto a culture-design swatch", () => {
    const graph = parseWorkflowGraph(WORKFLOW_IR);
    for (const node of graph.nodes) {
      expect(NODE_KIND_PALETTE).toHaveProperty(node.kind);
      expect(accentFor(node.kind)).toBe(
        TERMINAL_PALETTE[NODE_KIND_PALETTE[node.kind as never]],
      );
    }
  });

  it("falls back to the neutral swatch for an unknown kind", () => {
    expect(paletteColorFor("something-new")).toBe("neutral");
    expect(accentFor("something-new")).toBe(TERMINAL_PALETTE.neutral);
  });
});
