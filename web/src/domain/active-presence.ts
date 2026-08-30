import type {
  EventEnvelope,
  NodeRunListItem,
  NodeRunState,
  Run,
  RunState,
  WorkflowVersion,
} from "../api/types";
import { parseWorkflowGraph, type WorkflowGraph } from "./graph";
import { groupWorkflowVersions } from "./workflows";

/**
 * The Active Graphs sub-tab's pure half (task t31, spec claim c31 / honesty
 * condition h20): which published workflow graphs are alive right now, which
 * of their nodes hold active work, and how one committed cross-run event
 * maps onto a visible action. Everything here is a deterministic function
 * over data the API already serves — nothing is fetched or rendered from
 * this module, mirroring domain/mesh.ts's split for the Mesh view.
 *
 * Honesty (h14): a graph is "alive" only because a committed run row in a
 * non-terminal state pins one of its published digests; a node carries a
 * halo only because a committed node-run row in a non-terminal state names
 * it; a pulse renders only for an event naming a run this view has actually
 * loaded. No liveness is ever invented — an event naming no known run is a
 * no-op, never a decorative guess (see s35's challenge-pass note in
 * docs/specs/2026-08-13-economy-discord-graphs.md).
 */

/** Run states that count as "holding active tokens" (openapi RunState). */
export const ACTIVE_RUN_STATES: ReadonlySet<RunState> = new Set<RunState>([
  "created",
  "running",
  "waiting",
]);

/** Node-run states that count as "active work at this node" right now. */
export const ACTIVE_NODE_RUN_STATES: ReadonlySet<NodeRunState> =
  new Set<NodeRunState>(["ready", "leased", "running", "waiting_external"]);

/**
 * One workflow graph with live presence: the published version an active
 * run actually pins (never "the latest version" — a run executes the exact
 * digest it was created against), its parsed graph, and the committed rows
 * that make it alive.
 */
export interface ActiveGraphPresence {
  workflowKey: string;
  version: number;
  digest: string;
  graph: WorkflowGraph;
  /** Ids of the non-terminal runs pinning this digest, listing order kept. */
  runIds: string[];
  /**
   * Node ids (of nodes that exist in this graph) with at least one
   * non-terminal node run belonging to one of `runIds`, sorted for a
   * deterministic render order. A node-run row naming a node the graph
   * does not declare is dropped, never guessed at.
   */
  activeNodeIds: string[];
}

/**
 * Derive every alive graph: one entry per published digest with >= 1
 * non-terminal run. A run whose digest matches no published version renders
 * nowhere (the same orphan-run honesty rule domain/workflows.ts's
 * `withRunsByWorkflowKey` established). Entries are ordered by workflowKey
 * alphabetically, then newest version first — stable regardless of input
 * order.
 */
export function deriveActiveGraphs(
  versions: WorkflowVersion[],
  runs: Run[],
  nodeRuns: NodeRunListItem[],
): ActiveGraphPresence[] {
  const byDigest = new Map<
    string,
    { workflowKey: string; version: WorkflowVersion }
  >();
  for (const group of groupWorkflowVersions(versions)) {
    for (const version of group.versions) {
      byDigest.set(version.digest, { workflowKey: group.workflowKey, version });
    }
  }

  const activeRunsByDigest = new Map<string, Run[]>();
  for (const run of runs) {
    if (!ACTIVE_RUN_STATES.has(run.state)) continue;
    if (!byDigest.has(run.workflow_digest)) continue;
    const list = activeRunsByDigest.get(run.workflow_digest);
    if (list) list.push(run);
    else activeRunsByDigest.set(run.workflow_digest, [run]);
  }

  const entries: ActiveGraphPresence[] = [];
  for (const [digest, activeRuns] of activeRunsByDigest) {
    const found = byDigest.get(digest);
    if (!found) continue;
    const graph = parseWorkflowGraph(found.version.normalized_ir);
    const runIdSet = new Set(activeRuns.map((run) => run.id));
    const declaredNodes = new Set(graph.nodes.map((node) => node.id));
    const active = new Set<string>();
    for (const nodeRun of nodeRuns) {
      if (!runIdSet.has(nodeRun.run_id)) continue;
      if (!ACTIVE_NODE_RUN_STATES.has(nodeRun.state)) continue;
      if (!declaredNodes.has(nodeRun.node_id)) continue;
      active.add(nodeRun.node_id);
    }
    entries.push({
      workflowKey: found.workflowKey,
      version: found.version.version,
      digest,
      graph,
      runIds: activeRuns.map((run) => run.id),
      activeNodeIds: [...active].sort((a, b) => a.localeCompare(b)),
    });
  }

  return entries.sort(
    (a, b) =>
      a.workflowKey.localeCompare(b.workflowKey) || b.version - a.version,
  );
}

// ---------------------------------------------------------------------
// Event -> presence action mapping (the SSE stream's visible half)
// ---------------------------------------------------------------------

const TYPE_PREFIX = "dev.culture.nodes.";

export type PresenceAction =
  /** A committed event on a known run: render a visible pulse (h14 —
   *  one pulse per committed event, never more, never invented). `nodeId`
   *  is the node the event's own payload names, or null when the payload
   *  carries no node — a graph-level pulse, never a guessed node. */
  | { kind: "pulse"; runId: string; nodeId: string | null }
  /** A known run reached a terminal state: presence must drop it. */
  | { kind: "run-resolved"; runId: string; state: "completed" | "failed" | "cancelled" }
  /** A run this view has not loaded yet was created: refetch committed
   *  rows rather than inventing a placeholder graph. */
  | { kind: "run-appeared"; runId: string }
  /** An event naming no run, or naming a run this view does not know:
   *  honestly a no-op (h14). */
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
 * The node a committed event's own payload names, if any: `node_id` on
 * node-run/attempt/token events (internal/engine/events.go), `to_node` on
 * token.transitioned (the node the token arrived at), `end_node` on
 * run.completed. Anything else is honestly null — never a guess.
 */
function eventNodeId(envelope: EventEnvelope): string | null {
  const data = envelope.data ?? {};
  for (const key of ["node_id", "to_node", "end_node"]) {
    const value = data[key];
    if (typeof value === "string" && value !== "") return value;
  }
  return null;
}

/**
 * One committed event -> one presence action. `knownRunIds` is the set of
 * run ids this view actually fetched from the API: an event naming any
 * other run is a no-op — with the single exception of `run.created`, which
 * maps to a refetch request (`run-appeared`) so the committed row itself
 * can arrive; the event alone never renders anything.
 */
export function presenceEventAction(
  envelope: EventEnvelope,
  knownRunIds: ReadonlySet<string>,
): PresenceAction {
  const runId = eventRunId(envelope);
  if (!runId) return { kind: "none" };
  const type = envelope.type.startsWith(TYPE_PREFIX)
    ? envelope.type.slice(TYPE_PREFIX.length)
    : envelope.type;
  if (type === "run.created") {
    return knownRunIds.has(runId)
      ? { kind: "none" }
      : { kind: "run-appeared", runId };
  }
  if (!knownRunIds.has(runId)) return { kind: "none" };
  switch (type) {
    case "run.completed":
      return { kind: "run-resolved", runId, state: "completed" };
    case "run.failed":
    case "run.bounded":
      return { kind: "run-resolved", runId, state: "failed" };
    case "run.cancelled":
      return { kind: "run-resolved", runId, state: "cancelled" };
    default:
      return { kind: "pulse", runId, nodeId: eventNodeId(envelope) };
  }
}

/**
 * Events that can change which nodes hold active work — a node run became
 * claimable, an attempt started/finished, a token moved — and therefore
 * justify a (debounced) refetch of the node-runs listing so
 * `activeNodeIds` stays a readout of committed rows rather than drifting.
 */
export function needsPresenceRefresh(eventType: string): boolean {
  return (
    eventType === `${TYPE_PREFIX}node-run.ready` ||
    eventType === `${TYPE_PREFIX}node-run.failed` ||
    eventType === `${TYPE_PREFIX}attempt.started` ||
    eventType === `${TYPE_PREFIX}attempt.completed` ||
    eventType === `${TYPE_PREFIX}token.transitioned` ||
    eventType === `${TYPE_PREFIX}token.entered`
  );
}
