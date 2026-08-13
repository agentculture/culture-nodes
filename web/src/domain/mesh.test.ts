import { describe, expect, it } from "vitest";
import type { Actor, EventEnvelope, NodeRunListItem, Run } from "../api/types";
import {
  ACTIVE_MESH_RUN_STATES,
  CONTROL_PLANE_ID,
  actorGlyph,
  assembleMeshGraph,
  dedupeActors,
  hash01,
  layoutMesh,
  meshEventAction,
  needsAttributionRefresh,
  runActorAttribution,
  runLabel,
} from "./mesh";

const USAGE = {
  input_tokens: 0,
  output_tokens: 0,
  cached_input_tokens: 0,
  reasoning_tokens: 0,
  attempts_reported: 0,
  attempts_not_reported: 0,
};

function actor(id: string, key: string, revision = 1, kind = "agent"): Actor {
  return {
    id,
    actor_key: key,
    revision,
    kind,
    protocol: "http",
    created_at: "2026-08-12T00:00:00Z",
  };
}

function run(id: string, state: Run["state"], extra: Partial<Run> = {}): Run {
  return {
    id,
    workflow_digest: "sha256:wf",
    state,
    created_at: "2026-08-12T00:00:00Z",
    updated_at: "2026-08-12T01:00:00Z",
    ...extra,
  };
}

function nodeRunItem(
  runId: string,
  actorId: string | undefined,
  updatedAt: string,
): NodeRunListItem {
  return {
    id: `nr-${runId}-${updatedAt}`,
    run_id: runId,
    node_id: "build",
    actor_id: actorId,
    state: "running",
    created_at: "2026-08-12T00:00:00Z",
    updated_at: updatedAt,
    usage: USAGE,
  };
}

function envelope(type: string, data: Record<string, unknown>): EventEnvelope {
  return {
    id: "01AAAA",
    source: "nodes",
    specversion: "1.0",
    type: `dev.culture.nodes.${type}`,
    subject: typeof data.run_id === "string" ? (data.run_id as string) : undefined,
    time: "2026-08-12T02:00:00Z",
    datacontenttype: "application/json",
    data,
  };
}

describe("actorGlyph", () => {
  it("maps the system vocabulary and keeps unknown kinds visible as other", () => {
    expect(actorGlyph("agent")).toBe("agent");
    expect(actorGlyph("human")).toBe("human");
    expect(actorGlyph("team")).toBe("human");
    expect(actorGlyph("runner")).toBe("runner");
    expect(actorGlyph("something-new")).toBe("other");
  });
});

describe("dedupeActors", () => {
  it("keeps one node per actor_key — the newest revision — and aliases old ids", () => {
    const rows = [
      actor("a-thor-r1", "codex-thor", 1),
      actor("a-thor-r2", "codex-thor", 2),
      actor("a-orin", "codex-orin", 1),
    ];
    const { actors, aliases } = dedupeActors(rows);
    expect(actors.map((a) => a.id).sort()).toEqual(["a-orin", "a-thor-r2"]);
    expect(aliases.get("a-thor-r1")).toBe("a-thor-r2");
    expect(aliases.get("a-thor-r2")).toBe("a-thor-r2");
  });
});

describe("runActorAttribution", () => {
  it("attributes each run to its most recently updated actor-naming row", () => {
    const items = [
      nodeRunItem("run-1", "a-old", "2026-08-12T01:00:00Z"),
      nodeRunItem("run-1", "a-new", "2026-08-12T02:00:00Z"),
      nodeRunItem("run-2", undefined, "2026-08-12T03:00:00Z"),
    ];
    const map = runActorAttribution(items);
    expect(map.get("run-1")).toBe("a-new");
    expect(map.has("run-2")).toBe(false);
  });
});

describe("runLabel", () => {
  it("prefers name over display_hint over id, and says which it used", () => {
    expect(runLabel({ id: "r", name: "Ship it", display_hint: "h" })).toEqual({
      label: "Ship it",
      labelKind: "name",
    });
    expect(runLabel({ id: "r", display_hint: "review the diff" })).toEqual({
      label: "review the diff",
      labelKind: "hint",
    });
    expect(runLabel({ id: "r" })).toEqual({ label: "r", labelKind: "id" });
  });
});

describe("assembleMeshGraph", () => {
  const actors = [
    actor("a-thor-r1", "codex-thor", 1),
    actor("a-thor-r2", "codex-thor", 2),
    actor("a-ori", "ori", 1, "human"),
  ];

  it("shows only active runs, attributed through actor-revision aliases", () => {
    const runs = [
      run("run-live", "running"),
      run("run-waiting", "waiting"),
      run("run-done", "completed"),
    ];
    // run-live's newest attempt names the OLD thor revision's id; the edge
    // must land on the surviving node.
    const nodeRuns = [nodeRunItem("run-live", "a-thor-r1", "2026-08-12T02:00:00Z")];
    const graph = assembleMeshGraph(actors, runs, nodeRuns);

    expect(graph.actors.map((a) => a.id).sort()).toEqual(["a-ori", "a-thor-r2"]);
    expect(graph.runs.map((r) => r.id).sort()).toEqual(["run-live", "run-waiting"]);
    const live = graph.runs.find((r) => r.id === "run-live")!;
    expect(live.attachedTo).toBe("a-thor-r2");
    const waiting = graph.runs.find((r) => r.id === "run-waiting")!;
    expect(waiting.attachedTo).toBe(CONTROL_PLANE_ID);
  });

  it("emits exactly one edge per actor and per run", () => {
    const graph = assembleMeshGraph(actors, [run("run-live", "running")], []);
    // 2 deduped actors -> center, 1 run -> center (unattributed).
    expect(graph.edges).toHaveLength(3);
    expect(
      graph.edges.filter((e) => e.to === CONTROL_PLANE_ID),
    ).toHaveLength(3);
  });

  it("covers every RunState the enum declares in its active set decision", () => {
    for (const state of [
      "created",
      "running",
      "waiting",
    ] as const) {
      expect(ACTIVE_MESH_RUN_STATES.has(state)).toBe(true);
    }
    for (const state of ["completed", "failed", "cancelled"] as const) {
      expect(ACTIVE_MESH_RUN_STATES.has(state)).toBe(false);
    }
  });
});

describe("meshEventAction", () => {
  it("maps run.created to run-added with the workflow key as label", () => {
    expect(
      meshEventAction(envelope("run.created", { run_id: "r1", workflow_key: "ship" })),
    ).toEqual({ kind: "run-added", runId: "r1", label: "ship" });
  });

  it("maps run terminal events to run-resolved outcomes", () => {
    expect(meshEventAction(envelope("run.completed", { run_id: "r1" }))).toEqual({
      kind: "run-resolved",
      runId: "r1",
      outcome: "completed",
    });
    expect(meshEventAction(envelope("run.failed", { run_id: "r1" }))).toEqual({
      kind: "run-resolved",
      runId: "r1",
      outcome: "failed",
    });
    expect(meshEventAction(envelope("run.bounded", { run_id: "r1" }))).toEqual({
      kind: "run-resolved",
      runId: "r1",
      outcome: "failed",
    });
    expect(meshEventAction(envelope("run.cancelled", { run_id: "r1" }))).toEqual({
      kind: "run-resolved",
      runId: "r1",
      outcome: "cancelled",
    });
  });

  it("maps interior events to directional pulses", () => {
    expect(meshEventAction(envelope("node-run.ready", { run_id: "r1" }))).toEqual({
      kind: "pulse",
      runId: "r1",
      direction: "outbound",
    });
    expect(
      meshEventAction(envelope("ledger.record-appended", { run_id: "r1" })),
    ).toEqual({ kind: "pulse", runId: "r1", direction: "inbound" });
    expect(
      meshEventAction(envelope("attempt.completed", { run_id: "r1" })),
    ).toEqual({ kind: "pulse", runId: "r1", direction: "inbound" });
  });

  it("falls back to the envelope subject for the run id", () => {
    const env = envelope("token.transitioned", {});
    env.subject = "r-from-subject";
    expect(meshEventAction(env)).toEqual({
      kind: "pulse",
      runId: "r-from-subject",
      direction: "outbound",
    });
  });

  it("is honestly a no-op for an event naming no run", () => {
    const env = envelope("token.transitioned", {});
    env.subject = undefined;
    expect(meshEventAction(env)).toEqual({ kind: "none" });
  });
});

describe("needsAttributionRefresh", () => {
  it("asks for a refetch only on node-run/attempt activity", () => {
    expect(needsAttributionRefresh("dev.culture.nodes.node-run.ready")).toBe(true);
    expect(needsAttributionRefresh("dev.culture.nodes.attempt.completed")).toBe(true);
    expect(needsAttributionRefresh("dev.culture.nodes.run.created")).toBe(false);
    expect(
      needsAttributionRefresh("dev.culture.nodes.ledger.record-appended"),
    ).toBe(false);
  });
});

describe("layoutMesh", () => {
  const graph = assembleMeshGraph(
    [
      actor("a-1", "one"),
      actor("a-2", "two"),
      actor("a-3", "three", 1, "runner"),
    ],
    [run("r-1", "running"), run("r-2", "waiting")],
    [nodeRunItem("r-1", "a-1", "2026-08-12T02:00:00Z")],
  );

  it("is deterministic: the same inputs always produce the same layout", () => {
    const first = layoutMesh(graph, 1200, 620);
    const second = layoutMesh(graph, 1200, 620);
    expect([...second.entries()]).toEqual([...first.entries()]);
  });

  it("positions every node inside the canvas, center in the middle", () => {
    const width = 1200;
    const height = 620;
    const positions = layoutMesh(graph, width, height);
    expect(positions.get(CONTROL_PLANE_ID)).toEqual({
      x: width / 2,
      y: height / 2,
      z: 1,
    });
    expect(positions.size).toBe(1 + 3 + 2);
    for (const pos of positions.values()) {
      expect(pos.x).toBeGreaterThan(0);
      expect(pos.x).toBeLessThan(width);
      expect(pos.y).toBeGreaterThan(0);
      expect(pos.y).toBeLessThan(height);
      expect(pos.z).toBeGreaterThanOrEqual(0.82);
      expect(pos.z).toBeLessThanOrEqual(1.18);
    }
  });

  it("keeps an attributed run's satellite near its actor", () => {
    const positions = layoutMesh(graph, 1200, 620);
    const actorPos = positions.get("a-1")!;
    const runPos = positions.get("r-1")!;
    const d = Math.hypot(actorPos.x - runPos.x, actorPos.y - runPos.y);
    expect(d).toBeGreaterThan(20);
    expect(d).toBeLessThan(90);
  });

  it("hash01 is stable and in [0, 1)", () => {
    expect(hash01("abc")).toBe(hash01("abc"));
    expect(hash01("abc")).not.toBe(hash01("abd"));
    for (const id of ["a", "b", "run-123", CONTROL_PLANE_ID]) {
      expect(hash01(id)).toBeGreaterThanOrEqual(0);
      expect(hash01(id)).toBeLessThan(1);
    }
  });
});
