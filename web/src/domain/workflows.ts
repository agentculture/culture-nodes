import type { Run, WorkflowVersion } from "../api/types";
import { parseWorkflowGraph, type WorkflowGraph } from "./graph";
import { parseWorkflowSourceForPreview } from "./workflow-source";

/**
 * One published workflow, as the Workflows view (task t8) renders it: every
 * version the API has for a `workflow_key`, newest first, plus the recent
 * runs pinned to any of those versions' digests.
 *
 * `owner` is read from the *latest* version's `normalized_ir.metadata.ownerRef`
 * (PRD §11.3's workflow-level owner) — there is no separate owner field on
 * the wire (api/openapi/openapi.yaml's WorkflowVersion schema), so this is
 * the one place the API actually carries it. An older version with no
 * `ownerRef`, or a document that never set one, renders as `undefined` here
 * rather than inventing a placeholder — the view's job is to say "unowned"
 * honestly, not to guess.
 */
export interface WorkflowGroup {
  workflowKey: string;
  owner?: string;
  /** Every published version of this workflow_key, newest version first. */
  versions: WorkflowVersion[];
  /** Populated by withRunsByWorkflowKey; empty until then. */
  recentRuns: Run[];
}

/**
 * Buckets `GET /v1alpha1/workflows`' flat version list by `workflow_key`,
 * each bucket's versions sorted newest-version-first, buckets themselves
 * ordered alphabetically by key for a stable render order. Grouping is
 * purely a client-side reshaping of already-committed data — the list
 * endpoint reports every version of every published workflow in one flat
 * array (no server-side grouping), so this is the only place the
 * one-workflow-many-versions structure exists.
 */
export function groupWorkflowVersions(
  versions: WorkflowVersion[],
): WorkflowGroup[] {
  const byKey = new Map<string, WorkflowVersion[]>();
  for (const v of versions) {
    const bucket = byKey.get(v.workflow_key);
    if (bucket) {
      bucket.push(v);
    } else {
      byKey.set(v.workflow_key, [v]);
    }
  }

  const groups: WorkflowGroup[] = [];
  for (const [workflowKey, bucket] of byKey) {
    const sorted = [...bucket].sort((a, b) => b.version - a.version);
    groups.push({
      workflowKey,
      owner: sorted[0]?.normalized_ir?.metadata?.ownerRef,
      versions: sorted,
      recentRuns: [],
    });
  }
  return groups.sort((a, b) => a.workflowKey.localeCompare(b.workflowKey));
}

/** How many recent runs each workflow card shows, absent an explicit limit. */
export const RECENT_RUNS_LIMIT = 5;

/**
 * Attaches each group's recent runs from a per-`workflow_key` answer —
 * `GET /v1alpha1/runs?workflow_key=<key>` per group (the filter task t7
 * added), keyed by the group's own `workflowKey`.
 *
 * Task t8 replaced a single unfiltered listing filtered client-side by
 * digest. That was wrong for the reason every "filter the global list"
 * shortcut is wrong: `GET /v1alpha1/runs` answers at most one page (50 by
 * default), so as soon as one high-frequency workflow — the pr-upkeep sweep
 * — fills that page, every OTHER workflow's runs fall off it and each card
 * rendered "No runs yet" for a workflow with hundreds of runs. Asking the
 * server per key means a card's list can only be empty when that workflow
 * genuinely has no runs.
 *
 * A group whose key the map has no entry for stays empty rather than
 * falling back to anything: a request that never answered is not evidence
 * of no runs, and the caller is responsible for not rendering a card whose
 * runs it failed to fetch as authoritative.
 *
 * Each list is expected already sorted newest-first (the caller requests
 * `sort=updated_at`, as every other list view here does); this only
 * truncates, it never re-sorts, so it stays a straight readout of committed
 * API state.
 */
export function withRunsByWorkflowKey(
  groups: WorkflowGroup[],
  runsByKey: ReadonlyMap<string, Run[]>,
  limit: number = RECENT_RUNS_LIMIT,
): WorkflowGroup[] {
  return groups.map((group) => ({
    ...group,
    recentRuns: (runsByKey.get(group.workflowKey) ?? []).slice(0, limit),
  }));
}

/**
 * The stored document, exactly as it was published (task t8, claim c36).
 *
 * `WorkflowVersion` carries BOTH halves of a published version: the
 * compiler's `normalized_ir` and the operator's own `source` bytes with the
 * `source_format` they were submitted in (api/openapi/openapi.yaml's
 * WorkflowVersion; the list endpoint selects the `source` column too, see
 * internal/api/queries.go's `workflowVersionColumns`). This accessor exists
 * to make it impossible to reach for the wrong one by accident: the Design
 * view's source pane shows THESE bytes, never a re-serialization of the IR.
 *
 * A round trip through the IR would lose every comment, every blank line and
 * the author's key order — which is exactly the difference h28 asks a test to
 * prove, so the readout has to come from here.
 */
export function storedSource(version: WorkflowVersion): {
  source: string;
  format: "yaml" | "json";
} {
  return { source: version.source, format: version.source_format };
}

/**
 * The graph the Design gallery draws: the published, normalized IR parsed by
 * the one parser every canvas in this app uses (`domain/graph.ts`). No run is
 * involved anywhere in this path — that is claim c31: a workflow published
 * and never run still has a graph, because the graph is a property of the
 * published version, not of any execution of it.
 */
export function graphFromPublishedIR(version: WorkflowVersion): WorkflowGraph {
  return parseWorkflowGraph(version.normalized_ir);
}

/**
 * The same version's graph, derived from its STORED SOURCE instead — the
 * path an editor takes when it opens a published version (c36).
 *
 * `null` when the bytes do not parse into a workflow shape at all. A source
 * the server accepted always will; a UI still may not throw on a document it
 * did not itself validate (see `parseWorkflowSourceForPreview`).
 */
export function graphFromStoredSource(
  version: WorkflowVersion,
): WorkflowGraph | null {
  const { source, format } = storedSource(version);
  const ir = parseWorkflowSourceForPreview(source, format);
  return ir ? parseWorkflowGraph(ir) : null;
}

/**
 * A graph's identity as two sorted sets — `id:kind` per node, and each edge's
 * `source.outcome->target` key.
 *
 * This is what h28's "identical node and edge sets" means operationally.
 * Sorted and stringified deliberately: the two parses reach their nodes in
 * different orders (an IR's key order is the compiler's, a document's is the
 * author's), and a comparison that cared about order would fail on a
 * difference nobody can see on screen. Layout, depth and the raw node object
 * are all left out for the same reason — they are how the graph is drawn,
 * not what it is.
 */
export function graphTopology(graph: WorkflowGraph): {
  nodes: string[];
  edges: string[];
} {
  return {
    nodes: graph.nodes.map((node) => `${node.id}:${node.kind}`).sort(),
    edges: graph.edges.map((edge) => edge.id).sort(),
  };
}

/** One gallery selection: which workflow, and which of its versions. */
export interface GallerySelection {
  group: WorkflowGroup;
  version: WorkflowVersion;
}

/**
 * Resolve the `?workflow=`/`?version=` pair the Design gallery keeps in the
 * URL against what is actually published (task t8).
 *
 * Selection lives in the URL rather than in component state for the reason
 * every other view here does it (`useTimeRange`, the sub-tab param): a graph
 * an agent or a colleague cannot link to is a graph they have to be talked
 * through. That means the params are *reader-supplied* and may name anything
 * at all, so both halves fall back rather than render nothing: an unknown
 * key lands on the first published workflow, an unknown version on that
 * workflow's newest one. `null` only when nothing is published — the one
 * case where an empty gallery is the honest answer (h14).
 */
export function selectGalleryVersion(
  groups: WorkflowGroup[],
  workflowKey: string | null,
  version: number | null,
): GallerySelection | null {
  if (groups.length === 0) return null;
  const group =
    groups.find((candidate) => candidate.workflowKey === workflowKey) ??
    groups[0];
  const selected =
    group.versions.find((candidate) => candidate.version === version) ??
    group.versions[0];
  if (!selected) return null;
  return { group, version: selected };
}
