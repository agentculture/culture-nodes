import { useEffect, useMemo, useState } from "react";
import type { ELK as ElkInstance } from "elkjs/lib/elk-api";
import type { WorkflowGraph } from "../domain/graph";

// Kept in step with `.node-card`'s width/height in styles/app.css: React Flow
// needs the box size before the DOM exists, so the two cannot be derived from
// each other.
export const NODE_WIDTH = 224;
export const NODE_HEIGHT = 128;

export type NodePositions = Record<string, { x: number; y: number }>;

// elk.bundled.js is ~1.4 MB — a third of the app if it is bundled into the
// entry chunk. It is imported dynamically instead, which costs nothing
// visually because fallbackLayout already draws the graph while ELK loads.
let elkPromise: Promise<ElkInstance> | null = null;

function getElk(): Promise<ElkInstance> {
  elkPromise ??= import("elkjs/lib/elk.bundled.js").then(
    (module) => new module.default(),
  );
  return elkPromise;
}

const LAYOUT_OPTIONS: Record<string, string> = {
  "elk.algorithm": "layered",
  "elk.direction": "RIGHT",
  "elk.layered.spacing.nodeNodeBetweenLayers": "72",
  "elk.spacing.nodeNode": "48",
  "elk.layered.cycleBreaking.strategy": "DEPTH_FIRST",
  "elk.layered.nodePlacement.strategy": "NETWORK_SIMPLEX",
  "elk.edgeRouting": "POLYLINE",
};

/**
 * A deterministic layered layout used before ELK answers, and instead of it
 * if ELK ever fails. Depth comes from the breadth-first walk in
 * domain/graph.ts, so this is the same left-to-right reading order — just
 * without ELK's crossing minimisation.
 */
export function fallbackLayout(graph: WorkflowGraph): NodePositions {
  const perDepth = new Map<number, number>();
  const positions: NodePositions = {};
  for (const node of graph.nodes) {
    const row = perDepth.get(node.depth) ?? 0;
    perDepth.set(node.depth, row + 1);
    positions[node.id] = {
      x: node.depth * (NODE_WIDTH + 72),
      y: row * (NODE_HEIGHT + 48),
    };
  }
  return positions;
}

/**
 * Lay the workflow out with ELK's `layered` algorithm.
 *
 * ELK runs on the main thread here (elk.bundled.js), which is fine for the
 * graph sizes the PRD's first slice targets and keeps the bundle free of a
 * worker-URL build step. The fallback layout renders immediately so the
 * canvas is never blank while ELK works — and stays put if ELK throws.
 */
export function useElkLayout(graph: WorkflowGraph | null): {
  positions: NodePositions;
  ready: boolean;
} {
  const seed = useMemo(
    () => (graph ? fallbackLayout(graph) : ({} as NodePositions)),
    [graph],
  );
  const [positions, setPositions] = useState<NodePositions>(seed);
  const [ready, setReady] = useState(false);

  useEffect(() => {
    setPositions(seed);
    setReady(false);
    if (!graph) return;

    let cancelled = false;
    getElk()
      .then((elk) =>
        elk.layout({
          id: "root",
          layoutOptions: LAYOUT_OPTIONS,
          children: graph.nodes.map((node) => ({
            id: node.id,
            width: NODE_WIDTH,
            height: NODE_HEIGHT,
          })),
          edges: graph.edges.map((edge) => ({
            id: edge.id,
            sources: [edge.source],
            targets: [edge.target],
          })),
        }),
      )
      .then((laid) => {
        if (cancelled) return;
        const next: NodePositions = {};
        for (const child of laid.children ?? []) {
          next[child.id] = { x: child.x ?? 0, y: child.y ?? 0 };
        }
        setPositions(Object.keys(next).length > 0 ? next : seed);
        setReady(true);
      })
      .catch(() => {
        if (cancelled) return;
        // Keep the fallback positions — an unlaid-out graph is still a
        // readable graph, and a layout engine failure must not blank the view.
        setReady(true);
      });

    return () => {
      cancelled = true;
    };
  }, [graph, seed]);

  return { positions, ready };
}
