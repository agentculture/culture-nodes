// --- Declarations (task t22, #328) -----------------------------------------
//
// Mirrors api/openapi/openapi.yaml's DeclarationVersion / DeclarationShow /
// DeclarationFocus* / DeclarationAliasDetail schemas
// (internal/api/declarations.go, internal/api/declgraph.go). The focus
// route's direction/link filters are applied SERVER-SIDE (openapi.yaml's
// getDeclarationFocus parameters) — there is no unfiltered payload for the
// client to narrow further.

export type DeclarationLinkKind = "must" | "can";
export type DeclarationFocusDirection = "both" | "up" | "down";
export type DeclarationFocusLinkFilter = "both" | DeclarationLinkKind;

/** One published declaration version (components.schemas.DeclarationVersion). */
export interface DeclarationVersion {
  id: string;
  declaration_id: string;
  name: string;
  version: number;
  digest: string;
  author: string;
  created_at: string;
  warnings: string[];
}

export interface DeclarationLink {
  to: string;
  kind: DeclarationLinkKind;
}

/** `GET /v1alpha1/declarations/{name}`'s payload. */
export interface DeclarationShow extends DeclarationVersion {
  active: boolean;
  active_version_id?: string;
  links: DeclarationLink[];
}

/**
 * `GET /v1alpha1/declaration-aliases/{name}`'s payload (task t21b, #328):
 * a chain's parent, direct member declarations, and direct child aliases —
 * `declarations`/`aliases` are direct only, never recursive.
 */
export interface DeclarationAliasDetail {
  id: string;
  name: string;
  parent?: string;
  declarations: string[];
  aliases: string[];
}

/**
 * One declaration in a DeclarationFocus payload, carrying its distance in
 * hops from the center (0 for the center itself).
 */
export interface DeclarationFocusDeclaration {
  name: string;
  declaration_id: string;
  version: number;
  digest: string;
  distance: number;
  trigger_kind: string;
  action_kind: string;
  start_node: string;
  landing_node: string;
}

/**
 * A drawn start/landing waiting-state node (spec c63): never counted as a
 * hop by the server's BFS, included only so a graph can be drawn.
 */
export interface DeclarationFocusNode {
  declaration: string;
  role: "start" | "landing";
  name: string;
  deadline: string;
}

/** A must/can edge between two declarations both present in this focus. */
export interface DeclarationFocusLink {
  from: string;
  to: string;
  kind: DeclarationLinkKind;
}

/**
 * `GET /v1alpha1/declarations/{name}/focus`'s payload (task t20/t22, #328,
 * spec c24/c25/c62/c63): the SAME shape serves the single-declaration view
 * (distance 0) and the chain/graph view (distance > 0) — `{name}` also
 * resolves as a declaration alias (chain) name (task t21b).
 */
export interface DeclarationFocus {
  center: string;
  distance: number;
  direction: DeclarationFocusDirection;
  link: DeclarationFocusLinkFilter;
  declarations: DeclarationFocusDeclaration[];
  nodes: DeclarationFocusNode[];
  links: DeclarationFocusLink[];
}
