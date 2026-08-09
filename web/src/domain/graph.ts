import type { WorkflowIR, WorkflowIRNode } from "../api/types";
import {
  NODE_KIND_PALETTE,
  TERMINAL_PALETTE,
  type NodeKind,
  type PaletteColor,
} from "../culture-design/palette";

/** A workflow node, flattened out of the IR's `spec.nodes` map. */
export interface GraphNode {
  id: string;
  kind: string;
  ownerRef?: string;
  uses?: string;
  outcomes: string[];
  raw: WorkflowIRNode;
  /** Depth from the entry node — the ELK layer this node wants. */
  depth: number;
}

export interface GraphEdge {
  /** Stable key: `${source}.${outcome}->${target}`. */
  id: string;
  source: string;
  target: string;
  outcome: string;
  when?: string;
  /**
   * True when this edge returns the token to a node already reached on the
   * way here — a loop. `verify.changes_required -> build` is the PRD §8.7
   * slice's named example, and the one the Run view must render as visibly
   * distinct once walked.
   */
  loop: boolean;
}

export interface WorkflowGraph {
  name: string;
  version?: string;
  ownerRef?: string;
  entry: string;
  nodes: GraphNode[];
  edges: GraphEdge[];
}

/** Split an edge's `"<node>.<outcome>"` source into its two halves. */
export function splitEdgeSource(from: string): {
  node: string;
  outcome: string;
} {
  const dot = from.lastIndexOf(".");
  if (dot <= 0) return { node: from, outcome: "" };
  return { node: from.slice(0, dot), outcome: from.slice(dot + 1) };
}

/**
 * Parse a normalized workflow IR into the node/edge lists the canvas draws.
 *
 * Node order is breadth-first from the entry node, which is also the DOM
 * order React Flow renders them in — so tabbing through the canvas walks
 * the workflow the way it executes (PRD §8.8 "complete keyboard
 * navigation"), not the way a JSON object happened to be keyed.
 */
export function parseWorkflowGraph(ir: WorkflowIR): WorkflowGraph {
  const spec = ir.spec;
  const nodeIds = Object.keys(spec.nodes ?? {});
  const entry = spec.entry;

  const adjacency = new Map<string, string[]>();
  for (const edge of spec.edges ?? []) {
    const { node } = splitEdgeSource(edge.from);
    const list = adjacency.get(node);
    if (list) list.push(edge.to);
    else adjacency.set(node, [edge.to]);
  }

  // Breadth-first from the entry: order + depth in one walk.
  const depth = new Map<string, number>();
  const order: string[] = [];
  if (entry && spec.nodes?.[entry]) {
    const queue: string[] = [entry];
    depth.set(entry, 0);
    while (queue.length > 0) {
      const id = queue.shift() as string;
      order.push(id);
      for (const next of adjacency.get(id) ?? []) {
        if (depth.has(next) || !spec.nodes[next]) continue;
        depth.set(next, (depth.get(id) ?? 0) + 1);
        queue.push(next);
      }
    }
  }
  // Anything unreachable from the entry still renders — the compiler rejects
  // unreachable nodes, but a UI that silently drops data is worse than one
  // that shows it.
  for (const id of nodeIds) {
    if (!depth.has(id)) {
      depth.set(id, 0);
      order.push(id);
    }
  }

  const nodes: GraphNode[] = order.map((id) => {
    const raw = spec.nodes[id];
    return {
      id,
      kind: raw.kind,
      ownerRef: raw.ownerRef,
      uses: raw.uses,
      outcomes: raw.outcomes ?? Object.keys(raw.contract?.outcomes ?? {}),
      raw,
      depth: depth.get(id) ?? 0,
    };
  });

  const edges: GraphEdge[] = (spec.edges ?? []).map((edge) => {
    const { node, outcome } = splitEdgeSource(edge.from);
    const sourceDepth = depth.get(node) ?? 0;
    const targetDepth = depth.get(edge.to) ?? 0;
    return {
      id: `${node}.${outcome}->${edge.to}`,
      source: node,
      target: edge.to,
      outcome,
      when: edge.when,
      loop: targetDepth <= sourceDepth,
    };
  });

  return {
    name: ir.metadata?.name ?? "workflow",
    version: ir.metadata?.version,
    ownerRef: ir.metadata?.ownerRef,
    entry,
    nodes,
    edges,
  };
}

const KNOWN_KINDS = new Set(Object.keys(NODE_KIND_PALETTE));

/** node kind -> culture-design palette color, with a neutral fallback. */
export function paletteColorFor(kind: string): PaletteColor {
  return KNOWN_KINDS.has(kind)
    ? NODE_KIND_PALETTE[kind as NodeKind]
    : "neutral";
}

/** node kind -> resolved hex from the org terminal palette. */
export function accentFor(kind: string): string {
  return TERMINAL_PALETTE[paletteColorFor(kind)];
}
