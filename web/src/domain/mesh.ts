import type { Actor, EventEnvelope, NodeRunListItem, Run, RunState } from "../api/types";
import { TERMINAL_PALETTE } from "../culture-design/palette";

/**
 * The live-mesh overview's pure half (task t18): graph assembly from the
 * actors + runs + node-runs read surfaces, event->pulse mapping for the
 * cross-run SSE stream, and the deterministic orbital layout. Everything
 * here is side-effect-free so vitest can pin it without a canvas.
 *
 * Honesty (h14): every node and edge below traces to a committed API row —
 * actors from `GET /v1alpha1/actors`, runs from `GET /v1alpha1/runs`,
 * actor<->run attribution from `GET /v1alpha1/node-runs` (each row's
 * `actor_id` is its most recent attempt's actors-table reference). Nothing
 * is invented; a run nobody has dispatched yet honestly links to the
 * control plane instead of guessing an actor.
 *
 * Why attribution comes from node-runs rather than the events stream: the
 * engine's committed events today (internal/engine/events.go) carry
 * `run_id` / `node_run_id` / `worker_id` but no actors-table reference —
 * `attempt.started` / `actor.accepted` are declared in internal/events but
 * never emitted. The node-runs listing's `actor_id` is the same fact from
 * the same database, read from the surface that actually has it (see task
 * t18's deviation note in the run report).
 */

/** The synthetic center node's id — the control plane itself. */
export const CONTROL_PLANE_ID = "control-plane";

/** Run states that count as "on the mesh right now" (openapi RunState). */
export const ACTIVE_MESH_RUN_STATES: ReadonlySet<RunState> = new Set([
  "created",
  "running",
  "waiting",
]);

/** How an actor row is drawn: the kind-differentiated glyph families. */
export type ActorGlyph = "agent" | "human" | "runner" | "other";

/**
 * actors.kind is free text in the schema (migrations/0001, `kind TEXT NOT
 * NULL`); today's register-actor.sh writes 'agent'. Map the values the
 * system vocabulary names onto glyph families and keep anything else
 * visible as "other" rather than dropping it.
 */
export function actorGlyph(kind: string): ActorGlyph {
  switch (kind) {
    case "agent":
      return "agent";
    case "human":
    case "team":
      return "human";
    case "runner":
      return "runner";
    default:
      return "other";
  }
}

/** Glyph family -> terminal palette hex + relative prominence + shape. */
export const ACTOR_GLYPH_STYLE: Record<
  ActorGlyph,
  { color: string; dim: number; shape: "circle" | "square" }
> = {
  agent: { color: TERMINAL_PALETTE.teal, dim: 1, shape: "circle" },
  human: { color: TERMINAL_PALETTE.neutral, dim: 0.7, shape: "circle" },
  runner: { color: TERMINAL_PALETTE.blue, dim: 0.85, shape: "square" },
  other: { color: TERMINAL_PALETTE.violet, dim: 0.8, shape: "circle" },
};

/** Run satellites: ember-warm while live; resolution recolors them. */
export const RUN_COLOR = TERMINAL_PALETTE.amber;
export const RUN_SETTLE_COLOR = TERMINAL_PALETTE.green;
export const RUN_FLARE_COLOR = TERMINAL_PALETTE.pink;

export interface MeshActorNode {
  id: string;
  actorKey: string;
  kind: string;
  glyph: ActorGlyph;
  revision: number;
}

export interface MeshRunNode {
  id: string;
  /** name > display_hint > id — the same precedence the run views use. */
  label: string;
  labelKind: "name" | "hint" | "id";
  category: string | null;
  state: string;
  /** The mesh node id this run's edge attaches to (actor or control plane). */
  attachedTo: string;
}

export interface MeshEdge {
  from: string;
  to: string;
}

export interface MeshGraph {
  actors: MeshActorNode[];
  runs: MeshRunNode[];
  edges: MeshEdge[];
}

/**
 * Actor rows are append-only: a capability/endpoint change is a new row
 * with the same actor_key and a higher revision (openapi Actor schema).
 * One mesh node per actor_key — the newest revision — with an alias map so
 * an attempt that referenced an older revision's id still attributes to
 * the surviving node.
 */
export function dedupeActors(actors: Actor[]): {
  actors: Actor[];
  aliases: Map<string, string>;
} {
  const newestByKey = new Map<string, Actor>();
  for (const actor of actors) {
    const seen = newestByKey.get(actor.actor_key);
    if (!seen || actor.revision > seen.revision) {
      newestByKey.set(actor.actor_key, actor);
    }
  }
  const aliases = new Map<string, string>();
  for (const actor of actors) {
    const kept = newestByKey.get(actor.actor_key);
    if (kept) aliases.set(actor.id, kept.id);
  }
  return { actors: [...newestByKey.values()], aliases };
}

/**
 * run_id -> actor_id from the node-runs listing: each row's `actor_id` is
 * its most recent attempt's actor reference (openapi NodeRunListItem), so
 * per run the row with the latest `updated_at` that names an actor wins.
 */
export function runActorAttribution(
  items: NodeRunListItem[],
): Map<string, string> {
  const latest = new Map<string, NodeRunListItem>();
  for (const item of items) {
    if (!item.actor_id) continue;
    const seen = latest.get(item.run_id);
    if (!seen || item.updated_at > seen.updated_at) {
      latest.set(item.run_id, item);
    }
  }
  const out = new Map<string, string>();
  for (const [runId, item] of latest) {
    if (item.actor_id) out.set(runId, item.actor_id);
  }
  return out;
}

/** name > display_hint > id, and which of the three it actually was. */
export function runLabel(run: {
  id: string;
  name?: string;
  display_hint?: string;
}): { label: string; labelKind: "name" | "hint" | "id" } {
  if (run.name) return { label: run.name, labelKind: "name" };
  if (run.display_hint) return { label: run.display_hint, labelKind: "hint" };
  return { label: run.id, labelKind: "id" };
}

/**
 * Assemble the whole graph: deduped actors orbiting the control plane,
 * active runs as satellites of the actor executing them (or of the control
 * plane when no attempt has named one yet), one edge per node.
 */
export function assembleMeshGraph(
  actors: Actor[],
  runs: Run[],
  nodeRuns: NodeRunListItem[],
): MeshGraph {
  const { actors: deduped, aliases } = dedupeActors(actors);
  const attribution = runActorAttribution(nodeRuns);
  const actorNodes: MeshActorNode[] = deduped.map((actor) => ({
    id: actor.id,
    actorKey: actor.actor_key,
    kind: actor.kind,
    glyph: actorGlyph(actor.kind),
    revision: actor.revision,
  }));
  const actorIds = new Set(actorNodes.map((a) => a.id));

  const runNodes: MeshRunNode[] = runs
    .filter((run) => ACTIVE_MESH_RUN_STATES.has(run.state))
    .map((run) => {
      const attributed = attribution.get(run.id);
      const resolved = attributed ? (aliases.get(attributed) ?? attributed) : undefined;
      return {
        id: run.id,
        ...runLabel(run),
        category: run.category ?? null,
        state: run.state,
        attachedTo:
          resolved && actorIds.has(resolved) ? resolved : CONTROL_PLANE_ID,
      };
    });

  const edges: MeshEdge[] = [
    ...actorNodes.map((actor) => ({ from: actor.id, to: CONTROL_PLANE_ID })),
    ...runNodes.map((run) => ({ from: run.id, to: run.attachedTo })),
  ];
  return { actors: actorNodes, runs: runNodes, edges };
}

// ---------------------------------------------------------------------
// Event -> pulse mapping (the SSE stream's visible half)
// ---------------------------------------------------------------------

const TYPE_PREFIX = "dev.culture.nodes.";

/**
 * Committed events whose payloads flow *back* toward the control plane
 * (results, evidence, decisions); everything else flows outward (work
 * being dispatched into the mesh).
 */
const INBOUND_TYPES = new Set([
  "attempt.completed",
  "ledger.record-appended",
  "ledger.review-committed",
  "runner.operation-completed",
  "contract.rejected",
  "human-task.decided",
  "node-run.failed",
]);

export type MeshEventAction =
  | { kind: "run-added"; runId: string; label: string }
  | {
      kind: "run-resolved";
      runId: string;
      outcome: "completed" | "failed" | "cancelled";
    }
  | { kind: "pulse"; runId: string; direction: "outbound" | "inbound" }
  | { kind: "none" };

function eventRunId(envelope: EventEnvelope): string | null {
  const fromData = envelope.data?.run_id;
  if (typeof fromData === "string" && fromData !== "") return fromData;
  if (typeof envelope.subject === "string" && envelope.subject !== "") {
    return envelope.subject;
  }
  return null;
}

/**
 * One committed event -> one visible action. Every event that names a run
 * produces exactly one pulse-family action (particles correspond one-to-one
 * to committed events, h14); an event naming no run is honestly a no-op
 * rather than a decorative guess.
 */
export function meshEventAction(envelope: EventEnvelope): MeshEventAction {
  const runId = eventRunId(envelope);
  if (!runId) return { kind: "none" };
  const type = envelope.type.startsWith(TYPE_PREFIX)
    ? envelope.type.slice(TYPE_PREFIX.length)
    : envelope.type;
  switch (type) {
    case "run.created": {
      const key = envelope.data?.workflow_key;
      return {
        kind: "run-added",
        runId,
        label: typeof key === "string" && key !== "" ? key : runId,
      };
    }
    case "run.completed":
      return { kind: "run-resolved", runId, outcome: "completed" };
    case "run.failed":
    case "run.bounded":
      return { kind: "run-resolved", runId, outcome: "failed" };
    case "run.cancelled":
      return { kind: "run-resolved", runId, outcome: "cancelled" };
    default:
      return {
        kind: "pulse",
        runId,
        direction: INBOUND_TYPES.has(type) ? "inbound" : "outbound",
      };
  }
}

/**
 * Events that can change which actor a run attributes to — a node run
 * became claimable or an attempt finished — and therefore justify a
 * (debounced) refetch of the node-runs listing.
 */
export function needsAttributionRefresh(eventType: string): boolean {
  return (
    eventType === `${TYPE_PREFIX}node-run.ready` ||
    eventType === `${TYPE_PREFIX}attempt.completed`
  );
}

// ---------------------------------------------------------------------
// Deterministic layout
// ---------------------------------------------------------------------

export interface MeshPosition {
  x: number;
  y: number;
  /** Depth cue in [0.82, 1.18]: scales drift amplitude, glow, parallax. */
  z: number;
}

/** FNV-1a — a stable per-id hash so layout jitter is deterministic. */
export function hashId(id: string): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < id.length; i++) {
    h ^= id.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return h >>> 0;
}

/** hash -> [0, 1), stable for a given id. */
export function hash01(id: string, salt = 0): number {
  return ((hashId(id) ^ Math.imul(salt + 1, 0x9e3779b9)) >>> 0) / 0x100000000;
}

/**
 * Lay the graph out: control plane centered, actors evenly on an ellipse
 * ring (deterministically jittered per id so the ring never reads as a
 * clock face), attributed runs orbiting their actor on the side facing
 * away from the center, unattributed runs on an inner ring. Pure function
 * of (graph, W, H) — the same inputs always produce the same layout, so a
 * resize recomputes rather than re-randomizes (the MeshIsland lesson).
 */
export function layoutMesh(
  graph: MeshGraph,
  width: number,
  height: number,
): Map<string, MeshPosition> {
  const out = new Map<string, MeshPosition>();
  const cx = width / 2;
  const cy = height / 2;
  out.set(CONTROL_PLANE_ID, { x: cx, y: cy, z: 1 });

  // A contained ring: capped so an ultra-wide canvas doesn't stretch the
  // constellation into disconnected corners (the operator's display is
  // wide; the mesh should read as one organism at its center).
  const rx = Math.max(120, Math.min(width * 0.3, 620, width / 2 - 90));
  const ry = Math.max(90, Math.min(height * 0.36, height / 2 - 70));

  const n = graph.actors.length;
  graph.actors.forEach((actor, i) => {
    const jitter = (hash01(actor.id) - 0.5) * ((Math.PI * 2) / Math.max(n, 4)) * 0.4;
    const angle = -Math.PI / 2 + (i * Math.PI * 2) / Math.max(n, 1) + jitter;
    const wobble = 0.92 + hash01(actor.id, 1) * 0.14;
    out.set(actor.id, {
      x: cx + Math.cos(angle) * rx * wobble,
      y: cy + Math.sin(angle) * ry * wobble,
      z: 0.82 + hash01(actor.id, 2) * 0.36,
    });
  });

  // Attributed runs orbit their actor; siblings fan out around it.
  const siblings = new Map<string, MeshRunNode[]>();
  for (const run of graph.runs) {
    const list = siblings.get(run.attachedTo) ?? [];
    list.push(run);
    siblings.set(run.attachedTo, list);
  }
  for (const [anchorId, runs] of siblings) {
    const anchor = out.get(anchorId) ?? out.get(CONTROL_PLANE_ID)!;
    const isCenter = anchorId === CONTROL_PLANE_ID;
    // Away-from-center direction for actor anchors; a full inner ring for
    // control-plane orphans.
    const baseAngle = isCenter
      ? -Math.PI / 2
      : Math.atan2(anchor.y - cy, anchor.x - cx);
    const orbit = isCenter ? Math.min(rx, ry) * 0.48 : 58;
    runs.forEach((run, i) => {
      const spread = isCenter
        ? (i * Math.PI * 2) / Math.max(runs.length, 3) +
          hash01(run.id) * 0.5
        : (i - (runs.length - 1) / 2) * 0.7 + (hash01(run.id) - 0.5) * 0.3;
      const angle = baseAngle + spread;
      const wobble = 0.9 + hash01(run.id, 1) * 0.2;
      out.set(run.id, {
        x: anchor.x + Math.cos(angle) * orbit * wobble,
        y: anchor.y + Math.sin(angle) * orbit * wobble,
        z: 0.82 + hash01(run.id, 2) * 0.36,
      });
    });
  }
  return out;
}
