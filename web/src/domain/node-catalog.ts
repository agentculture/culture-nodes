import type { WorkflowIRNode, WorkflowVersion } from "../api/types";
import { parseWorkflowGraph, type WorkflowGraph } from "./graph";
import { groupWorkflowVersions } from "./workflows";

/**
 * Cross-workflow node/graph catalog (task t29): a PURE, client-side domain
 * parser that derives the "Nodes" and "Node Graphs" sub-tab data (GitHub
 * issue #56) from an array of already-published `WorkflowVersion`s — no
 * routes, no components, no new API surface. Every export here is a
 * deterministic function over data the API already serves; nothing is
 * fetched or rendered from this module.
 *
 * Both catalogs are derived from the *latest* version of each `workflow_key`
 * (via `groupWorkflowVersions`, imported and reused as-is from
 * `domain/workflows.ts` rather than reimplemented): an older, superseded
 * version's nodes are not distinct catalog entries in their own right — they
 * are what the latest version replaced. `groupWorkflowVersions` already
 * orders groups alphabetically by `workflowKey` and each group's versions
 * newest-first, so `group.versions[0]` is always the latest.
 */

// ---------------------------------------------------------------------------
// Node definitions catalog
// ---------------------------------------------------------------------------

/** One occurrence of a node definition: the workflow version and node id it appears as. */
export interface NodeDefinitionOccurrence {
  workflowKey: string;
  version: number;
  nodeId: string;
}

export interface NodeDefinition {
  /** Stable identity key — see `definitionRef` below for how it's built. */
  id: string;
  kind: string;
  /** The actor/runner/approver ref that backs this definition's identity, when the kind has one. */
  ref?: string;
  /** Every (workflow, node id) this definition appears as, across all latest workflow versions. */
  occurrences: NodeDefinitionOccurrence[];
}

/**
 * Node-definition identity: kind + "what it uses/runs" — the one
 * `WorkflowIRNode` field that names a concrete external dependency for the
 * kinds that have one:
 *
 *  - **agent** nodes: the actor ref (`uses`, e.g.
 *    `actor://company/intake@sha256:111111`)
 *  - **code** nodes: the runner ref (`uses` again, e.g.
 *    `runner://headspace/docker@sha256:555555` — `uses` is the one field
 *    the IR overloads for both actor and runner identity, see api/types.ts)
 *  - **approval** nodes: the approver ref (`approverRef`) — the closest
 *    thing a human-gated node has to "what it runs"
 *  - every other kind (`decision`, `wait`, `end`, `action.http`,
 *    `subworkflow`) has no such field on `WorkflowIRNode` today, so identity
 *    falls back to the kind alone — every "wait" node in the catalog is one
 *    definition, honestly reflecting that the IR carries nothing further to
 *    distinguish them (never a synthetic, made-up identity).
 *
 * Two nodes with the same kind + ref are the same *definition* even across
 * workflows — e.g. two workflows both dispatching to
 * `actor://company/intake@sha256:111111` collapse into one entry with two
 * occurrences. That collapsing is also what makes the cross-workflow
 * linkage below possible: it is a query over this same identity, not a
 * separate derivation.
 */
function definitionRef(node: WorkflowIRNode): string | undefined {
  if (node.uses) return node.uses;
  if (node.kind === "approval" && node.approverRef) return node.approverRef;
  return undefined;
}

function definitionId(kind: string, ref: string | undefined): string {
  return ref ? `${kind}:${ref}` : kind;
}

function compareOccurrence(
  a: NodeDefinitionOccurrence,
  b: NodeDefinitionOccurrence,
): number {
  return (
    a.workflowKey.localeCompare(b.workflowKey) ||
    a.version - b.version ||
    a.nodeId.localeCompare(b.nodeId)
  );
}

/**
 * Distinct node definitions across every latest published workflow version,
 * sorted deterministically by `id` (definitions) then by workflow/version/
 * node id (each definition's occurrences) — same input, same output order,
 * regardless of the input array's order or the `Object.keys` iteration
 * order of any one workflow's `spec.nodes` map.
 */
export function deriveNodeDefinitions(
  versions: WorkflowVersion[],
): NodeDefinition[] {
  const byId = new Map<string, NodeDefinition>();

  for (const group of groupWorkflowVersions(versions)) {
    const latest = group.versions[0];
    if (!latest) continue;
    const graph = parseWorkflowGraph(latest.normalized_ir);

    for (const node of graph.nodes) {
      const ref = definitionRef(node.raw);
      const id = definitionId(node.kind, ref);
      let definition = byId.get(id);
      if (!definition) {
        definition = { id, kind: node.kind, ref, occurrences: [] };
        byId.set(id, definition);
      }
      definition.occurrences.push({
        workflowKey: group.workflowKey,
        version: latest.version,
        nodeId: node.id,
      });
    }
  }

  const definitions = [...byId.values()];
  for (const definition of definitions) {
    definition.occurrences.sort(compareOccurrence);
  }
  return definitions.sort((a, b) => a.id.localeCompare(b.id));
}

// ---------------------------------------------------------------------------
// Graph catalog
// ---------------------------------------------------------------------------

export interface GraphCatalogEntry {
  workflowKey: string;
  version: number;
  digest: string;
  graph: WorkflowGraph;
  nodeCount: number;
  edgeCount: number;
  entry: string;
  /** True when the parsed graph contains at least one loop edge (graph.ts's `loop` flag). */
  hasLoop: boolean;
}

/**
 * Per-workflow-key graph groupings: the latest version of each `workflow_key`,
 * parsed via `parseWorkflowGraph` (imported unmodified from `domain/graph.ts`
 * — this module never reimplements graph parsing). Ordered by `workflowKey`
 * alphabetically, the same stable order `groupWorkflowVersions` already
 * produces.
 */
export function deriveGraphCatalog(
  versions: WorkflowVersion[],
): GraphCatalogEntry[] {
  const entries: GraphCatalogEntry[] = [];
  for (const group of groupWorkflowVersions(versions)) {
    const latest = group.versions[0];
    if (!latest) continue;
    const graph = parseWorkflowGraph(latest.normalized_ir);
    entries.push({
      workflowKey: group.workflowKey,
      version: latest.version,
      digest: latest.digest,
      graph,
      nodeCount: graph.nodes.length,
      edgeCount: graph.edges.length,
      entry: graph.entry,
      hasLoop: graph.edges.some((edge) => edge.loop),
    });
  }
  return entries;
}

// ---------------------------------------------------------------------------
// Cross-workflow linkage
// ---------------------------------------------------------------------------

export interface CrossWorkflowLink {
  /** The shared actor/runner ref (never an approver ref — see below). */
  ref: string;
  kind: string;
  /** Every occurrence of this ref, spanning 2+ distinct workflow_keys. */
  occurrences: NodeDefinitionOccurrence[];
}

/**
 * Cross-workflow linkage approximates issue #56's "handovers" between
 * workflows: nodes in *different* workflows that share the same
 * actor/runner identity (the same `uses` ref, via `deriveNodeDefinitions`'s
 * identity grouping) are surfaced here as a simple adjacency — one entry per
 * shared ref, carrying every workflow/node occurrence of it.
 *
 * This is deliberately NOT the same thing as issue #56's node-announced /
 * node-consumed EVENT handovers ("events each node announces and
 * consumes, creating complex node graph"). `WorkflowIRNode` (api/types.ts)
 * has no event-emission or event-subscription schema today — no field
 * anywhere names an event a node announces or a node subscribes to consume.
 * A same-actor-or-runner coincidence is the only cross-workflow signal this
 * parser can honestly derive from the published IR as it exists now. This
 * is exactly the gap claim c20 and its seed `s15`
 * (docs/specs/2026-08-13-economy-discord-graphs.md) name: "#56's catalog
 * half needs a data-source decision" and "node announced/consumed events
 * have no schema" — c20 only commits to deriving "the graph catalog"
 * client-side from workflow IRs, not to the event-handover half. Real
 * event-schema handovers are a follow-up once that schema exists (see the
 * spec's `q1`-adjacent open questions on split/join and event emission,
 * c30/c31).
 *
 * Only ref-backed definitions (agent/code) can link workflows this way —
 * `approverRef`-only definitions (approval nodes) are excluded, because an
 * approver group is an organizational unit, not an execution handover; two
 * workflows both gated by the same approver group are not "the same node
 * definition handing off work" the way two workflows dispatching to the
 * same actor are.
 */
export function deriveCrossWorkflowLinks(
  versions: WorkflowVersion[],
): CrossWorkflowLink[] {
  const links: CrossWorkflowLink[] = [];
  for (const definition of deriveNodeDefinitions(versions)) {
    if (!definition.ref) continue;
    if (definition.kind === "approval") continue;
    const distinctWorkflows = new Set(
      definition.occurrences.map((occurrence) => occurrence.workflowKey),
    );
    if (distinctWorkflows.size < 2) continue;
    links.push({
      ref: definition.ref,
      kind: definition.kind,
      occurrences: definition.occurrences,
    });
  }
  return links.sort((a, b) => a.ref.localeCompare(b.ref));
}
