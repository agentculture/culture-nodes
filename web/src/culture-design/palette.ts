// culture-design/palette.ts
//
// The terminal 7-color categorical palette from agentculture/org — pinned
// commit b4d939ba0aa354a5ae53065319a773e0013de698,
// site-astro/src/components/ColleagueTerminal.astro. That component uses
// these eight swatches (seven categorical hues + a shared neutral
// fallback) as a per-capture-category accent — a top border stripe, a
// status dot, and a text tag — so a category reads as visually distinct
// before a single word of its label is parsed. DevagueTerminal.astro and
// LobesTerminal.astro reuse the same set as siblings in the same family.
//
// culture-nodes repurposes this categorical set for node-kind identity on
// the workflow canvas: one color per node kind, same design intent.

export type PaletteColor =
  | "teal"
  | "blue"
  | "amber"
  | "violet"
  | "pink"
  | "green"
  | "yellow"
  | "neutral";

/**
 * The eight terminal swatches, verbatim hex values from org's
 * ColleagueTerminal.astro `.accent-*` / `.dot` / `.accent-tag` rules.
 */
export const TERMINAL_PALETTE: Record<PaletteColor, string> = {
  teal: "#7fdcc9",
  blue: "#7fb3f2",
  amber: "#f2b774",
  violet: "#b49cf2",
  pink: "#f2789a",
  green: "#9fd6a3",
  yellow: "#e6cd7a",
  neutral: "#a9b0cf",
};

/**
 * Node kinds recognized by the Culture Nodes workflow graph
 * (docs/initial-design/culture-nodes-prd-spec.md).
 */
export type NodeKind =
  | "agent"
  | "code"
  | "decision"
  | "approval"
  | "wait"
  | "end"
  | "action.http"
  | "subworkflow";

/** node kind -> palette color. */
export const NODE_KIND_PALETTE: Record<NodeKind, PaletteColor> = {
  agent: "teal",
  code: "blue",
  decision: "violet",
  approval: "amber",
  wait: "neutral",
  end: "green",
  "action.http": "pink",
  subworkflow: "yellow",
};

/** node kind -> resolved hex, for consumers that don't want the indirection. */
export const NODE_KIND_HEX: Record<NodeKind, string> = Object.fromEntries(
  (Object.keys(NODE_KIND_PALETTE) as NodeKind[]).map((kind) => [
    kind,
    TERMINAL_PALETTE[NODE_KIND_PALETTE[kind]],
  ]),
) as Record<NodeKind, string>;

// ---------------------------------------------------------------------
// Terminal ground colors — the fixed dark backdrop ColleagueTerminal.astro
// (and its DevagueTerminal / LobesTerminal siblings) render the palette
// against. Unlike tokens.css's --surface/--ink (which follow the page
// theme), these are deliberately theme-INVARIANT — ColleagueTerminal.astro's
// own comment: "the ground itself never changes with the theme". Reused
// here wherever a Culture Nodes surface (e.g. a run-log / evidence panel)
// wants that same fixed dark terminal ground rather than the page's
// --surface token.
// ---------------------------------------------------------------------
export const TERMINAL_GROUND = {
  /** .term background */
  background: "#10142b",
  /** .term border / border-top-color default (neutral accent) */
  border: "rgba(233, 236, 248, 0.12)",
  /** .term-head border-bottom */
  borderSoft: "rgba(233, 236, 248, 0.1)",
  /** .title color */
  ink: "#e9ecf8",
  /** .term-head / .muted-ctx color */
  inkSoft: "#a9b0cf",
  /** .muted-source color */
  inkFaint: "#8790b8",
  /** pre color — terminal body text */
  body: "#c7cde8",
} as const;
