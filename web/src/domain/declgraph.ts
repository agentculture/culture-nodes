import type {
  DeclarationFocus,
  DeclarationFocusDeclaration,
  DeclarationFocusLink,
  DeclarationLinkKind,
} from "../api/types";
import { DASHED, SOLID, type EdgeStyle } from "../culture-design/edges";
import type { GraphEdge, GraphNode, WorkflowGraph } from "./graph";

/**
 * declgraph.ts (task t22, #328, spec c24/h18): the pure mapping from a
 * `DeclarationFocus` API response to what the Declarations route renders —
 * an ELK-layoutable graph and a distance-ring grouping. Kept here, tested
 * without a DOM, exactly the way `domain/graph.ts` separates workflow-IR
 * parsing from `ActiveGraphCanvas`'s rendering; the route component only
 * fetches and hands this module's output to `useElkLayout`/`ReactFlow`.
 *
 * `DeclarationFocus` already IS the one payload shape for both a single
 * declaration (distance 0, no links) and a chain/focus graph (distance >
 * 0) — c24's point — so there is exactly one mapping function, not a
 * "single" and a "chain" variant.
 */

/**
 * `focusToGraph` reuses `domain/graph.ts`'s `WorkflowGraph`/`GraphNode`
 * shape rather than inventing a parallel one, so the existing
 * `hooks/useElkLayout.ts` (the ELK layout hook every other graph view in
 * this app uses) lays this graph out unmodified — no new layout code, no
 * new npm dependency. `GraphNode.raw` is typed `WorkflowIRNode`, whose
 * only required field is `kind` (api/types.ts), so a declaration's
 * `action_kind` is a fully valid (if minimal) `raw` value; nothing here
 * pretends a declaration IS a workflow node beyond the fields ELK and the
 * canvas actually read (id, kind, depth for layout; outcomes/raw are
 * unused by declarations and carried only to satisfy the shared type).
 *
 * `distance` (server-computed hop count from the center, c62/c63) becomes
 * `depth` directly: ELK's `layered`/`RIGHT` algorithm draws distance 0 in
 * the leftmost column and each further ring one column to its right — the
 * same "distance ring" the route's rings list renders as text.
 */
export function focusToGraph(focus: DeclarationFocus): WorkflowGraph {
  const nodes: GraphNode[] = focus.declarations.map((d) => ({
    id: d.name,
    kind: d.action_kind,
    outcomes: [],
    raw: { kind: d.action_kind },
    depth: d.distance,
  }));

  const present = new Set(focus.declarations.map((d) => d.name));
  const edges: GraphEdge[] = focus.links
    // The server only ever returns links between two declarations BOTH
    // present in this focus (declgraph.go's handleDeclarationFocus), so
    // this filter is defensive, not load-bearing -- but a pure mapping
    // function should never assume an edge's endpoints exist rather than
    // checking.
    .filter((l) => present.has(l.from) && present.has(l.to))
    .map((l) => ({
      id: declarationEdgeId(l),
      source: l.from,
      target: l.to,
      outcome: l.kind,
      // A declaration link has no run-time "walked this loop" concept
      // (that is workflow-graph vocabulary) -- always false here.
      loop: false,
    }));

  return {
    name: focus.center,
    entry: focus.center,
    nodes,
    edges,
  };
}

/** Stable key for one focus link, safe to use as a React/ELK edge id. */
export function declarationEdgeId(link: DeclarationFocusLink): string {
  return `${link.from}--${link.kind}-->${link.to}`;
}

/**
 * must/can -> the same dashed/solid vocabulary `culture-design/edges.ts`
 * already carries for ledger authority (`AuthorityChip`,
 * `LEDGER_AUTHORITY_EDGE_STYLE`): a `must` link is a hard ordering
 * requirement -- SOLID, the style reserved for state actually on the
 * record -- and a `can` link is an optional/soft ordering -- DASHED, the
 * "proposed / not load-bearing" style. This is the one place that mapping
 * is decided; the route only reads the result.
 */
export function edgeStyleForLinkKind(kind: DeclarationLinkKind): EdgeStyle {
  return kind === "must" ? SOLID : DASHED;
}

/** One distance ring: every declaration the server placed at that hop count. */
export interface DeclarationDistanceRing {
  distance: number;
  declarations: DeclarationFocusDeclaration[];
}

/**
 * Groups `focus.declarations` by their server-computed `distance`, sorted
 * innermost ring first and, within a ring, by name -- a deterministic
 * reading order for the distance-ring control's summary list. Distance 0
 * always holds at least the center declaration (or, for an alias/chain
 * center, every declaration the alias resolves to directly -- t21b);
 * higher rings can be empty of declarations reachable at that hop count,
 * in which case they are simply absent rather than rendered empty.
 */
export function ringsOf(focus: DeclarationFocus): DeclarationDistanceRing[] {
  const byDistance = new Map<number, DeclarationFocusDeclaration[]>();
  for (const d of focus.declarations) {
    const ring = byDistance.get(d.distance);
    if (ring) ring.push(d);
    else byDistance.set(d.distance, [d]);
  }
  return Array.from(byDistance.entries())
    .sort(([a], [b]) => a - b)
    .map(([distance, declarations]) => ({
      distance,
      declarations: [...declarations].sort((a, b) =>
        a.name.localeCompare(b.name),
      ),
    }));
}
