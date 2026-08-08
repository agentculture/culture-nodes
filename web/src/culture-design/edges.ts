// culture-design/edges.ts
//
// Edge-style constants encoding agentculture/org's sitewide diagram
// semantic — pinned commit b4d939ba0aa354a5ae53065319a773e0013de698,
// cited from site-astro/src/components/CliRuntimeStackDiagram.astro:
//
//   .connector.solid          { stroke: var(--accent-strong); stroke-width: 2.4; }
//   .connector.solid.emphasis { stroke-width: 3; }
//   .connector.dotted         { stroke: var(--ink-soft); stroke-width: 2;
//                                stroke-dasharray: 2 7; }
//   .layer-box.replaceable    { stroke-dasharray: 9 7; }
//
// The diagram's own figcaption states the convention directly: "Solid
// lines show operational command and state paths ... Dotted lines are the
// optional runtime event feed and named-intent path." `.layer-box.replaceable`
// draws "optional / replaceable" surfaces (the agent harness, the physical
// robot) with the same 9-7 dashed stroke the site uses elsewhere for
// not-yet-fixed, swappable state — reused below as the DASHED edge style.
//
// culture-nodes maps these three visual conventions onto the ledger
// authority model from docs/initial-design/culture-nodes-prd-spec.md:
// agents may only create `proposed` records; humans confirm/reject;
// trusted runners create `observed` evidence; deterministic validators
// create `derived` records; no actor promotes its own proposal. Edge
// style follows that same authority line — SOLID is reserved for state a
// human/runner/validator actually put on the record; DASHED marks
// something an agent proposed that nobody has confirmed yet; DOTTED is
// reserved for a soft/reference link carrying no ledger authority at all
// (org's own usage: the agent<->runtime paths that bypass the CLI's
// control-plane authority).

export type EdgeStyleName = "SOLID" | "DASHED" | "DOTTED";

export interface EdgeStyle {
  name: EdgeStyleName;
  /** SVG stroke — a CSS custom property reference, resolved by whatever
   *  stylesheet/tree consumes it against tokens.css. */
  stroke: string;
  strokeWidth: number;
  /** SVG stroke-dasharray, or undefined for a solid line. */
  strokeDasharray?: string;
  /** What this style means on a Culture Nodes graph edge. */
  meaning: string;
}

/** Confirmed / active — CliRuntimeStackDiagram.astro `.connector.solid`. */
export const SOLID: EdgeStyle = {
  name: "SOLID",
  stroke: "var(--accent-strong)",
  strokeWidth: 2.4,
  meaning: "confirmed / active",
};

/**
 * Proposed — org's `.layer-box.replaceable` dashed-border convention
 * (stroke-dasharray: 9 7), repurposed here for edges.
 */
export const DASHED: EdgeStyle = {
  name: "DASHED",
  stroke: "var(--ink-soft)",
  strokeWidth: 2,
  strokeDasharray: "9 7",
  meaning: "proposed",
};

/** Reference / soft link — CliRuntimeStackDiagram.astro `.connector.dotted`. */
export const DOTTED: EdgeStyle = {
  name: "DOTTED",
  stroke: "var(--ink-soft)",
  strokeWidth: 2,
  strokeDasharray: "2 7",
  meaning: "reference / soft link",
};

export const EDGE_STYLES: Record<EdgeStyleName, EdgeStyle> = {
  SOLID,
  DASHED,
  DOTTED,
};

/**
 * Ledger authority states from the PRD's authority model
 * (docs/initial-design/culture-nodes-prd-spec.md §"Ledger authority
 * model").
 */
export type LedgerAuthority =
  | "proposed"
  | "confirmed"
  | "observed"
  | "derived";

/**
 * ledger authority -> edge style. Only `proposed` (an agent's own,
 * unconfirmed claim) renders DASHED; every authority a human confirmed, or
 * a trusted runner/validator recorded directly, renders SOLID.
 */
export const LEDGER_AUTHORITY_EDGE_STYLE: Record<LedgerAuthority, EdgeStyle> =
  {
    proposed: DASHED,
    confirmed: SOLID,
    observed: SOLID,
    derived: SOLID,
  };
