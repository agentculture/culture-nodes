import { describe, expect, it } from "vitest";
import { DASHED, SOLID } from "../culture-design/edges";
import {
  BUILD,
  FOCUS_DISTANCE_0,
  FOCUS_DISTANCE_1,
  FOCUS_DISTANCE_2,
  NOTIFY,
  PUBLISH,
  TEST,
} from "../fixtures/declarations-fixture";
import {
  declarationEdgeId,
  edgeStyleForLinkKind,
  focusToGraph,
  ringsOf,
} from "./declgraph";

describe("focusToGraph", () => {
  it("renders a single declaration (distance 0) as a one-node, no-edge graph", () => {
    const graph = focusToGraph(FOCUS_DISTANCE_0);
    expect(graph.name).toBe("build");
    expect(graph.entry).toBe("build");
    expect(graph.nodes).toHaveLength(1);
    expect(graph.nodes[0]).toMatchObject({ id: "build", depth: 0, kind: BUILD.action_kind });
    expect(graph.edges).toEqual([]);
  });

  it("renders distance 1: the center plus its direct neighbours and their links", () => {
    const graph = focusToGraph(FOCUS_DISTANCE_1);
    expect(graph.nodes.map((n) => n.id).sort()).toEqual(["build", "publish", "test"]);
    expect(graph.nodes.find((n) => n.id === "test")).toMatchObject({ depth: 1 });
    expect(graph.nodes.find((n) => n.id === "publish")).toMatchObject({ depth: 1 });
    // `publish -> notify` is out of ring at distance 1 (notify is distance
    // 2) and must not appear as a dangling edge.
    expect(graph.edges.map((e) => e.id).sort()).toEqual(
      [
        declarationEdgeId({ from: "test", to: "build", kind: "must" }),
        declarationEdgeId({ from: "build", to: "publish", kind: "can" }),
      ].sort(),
    );
  });

  it("renders distance 2: every declaration and every link between present declarations", () => {
    const graph = focusToGraph(FOCUS_DISTANCE_2);
    expect(graph.nodes.map((n) => n.id).sort()).toEqual(["build", "notify", "publish", "test"]);
    expect(graph.nodes.find((n) => n.id === "notify")).toMatchObject({ depth: 2 });
    expect(graph.edges).toHaveLength(3);
    expect(graph.edges.map((e) => `${e.source}->${e.target}:${e.outcome}`).sort()).toEqual(
      ["build->publish:can", "publish->notify:must", "test->build:must"].sort(),
    );
    // Depth carries the server's hop count straight through -- the ELK
    // layer this node wants, per useElkLayout's LAYOUT_OPTIONS -- never
    // re-derived from the edges here.
    for (const edge of graph.edges) {
      expect(edge.loop).toBe(false);
    }
  });

  it("drops a link whose endpoint is not present in this focus (defensive, matches the server's own invariant)", () => {
    const graph = focusToGraph({
      ...FOCUS_DISTANCE_0,
      links: [{ from: "build", to: "ghost", kind: "must" }],
    });
    expect(graph.edges).toEqual([]);
  });
});

describe("ringsOf", () => {
  it("groups distance 0 into a single ring", () => {
    expect(ringsOf(FOCUS_DISTANCE_0)).toEqual([
      { distance: 0, declarations: [BUILD] },
    ]);
  });

  it("groups distance 1 into two rings, sorted by distance then name", () => {
    expect(ringsOf(FOCUS_DISTANCE_1)).toEqual([
      { distance: 0, declarations: [BUILD] },
      { distance: 1, declarations: [PUBLISH, TEST] },
    ]);
  });

  it("groups distance 2 into three rings", () => {
    const rings = ringsOf(FOCUS_DISTANCE_2);
    expect(rings.map((r) => r.distance)).toEqual([0, 1, 2]);
    expect(rings[2]).toEqual({ distance: 2, declarations: [NOTIFY] });
  });
});

describe("edgeStyleForLinkKind", () => {
  it("renders a must link SOLID -- a hard ordering requirement actually on the record", () => {
    expect(edgeStyleForLinkKind("must")).toBe(SOLID);
  });

  it("renders a can link DASHED -- an optional/soft ordering", () => {
    expect(edgeStyleForLinkKind("can")).toBe(DASHED);
  });

  it("distinguishes the two styles (never the same stroke for both link kinds)", () => {
    expect(edgeStyleForLinkKind("must")).not.toBe(edgeStyleForLinkKind("can"));
  });
});

describe("declarationEdgeId", () => {
  it("is stable and unique per (from, kind, to)", () => {
    expect(declarationEdgeId({ from: "a", to: "b", kind: "must" })).toBe("a--must-->b");
    expect(declarationEdgeId({ from: "a", to: "b", kind: "can" })).not.toBe(
      declarationEdgeId({ from: "a", to: "b", kind: "must" }),
    );
  });
});
