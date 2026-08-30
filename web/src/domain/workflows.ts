import type { Run, WorkflowVersion } from "../api/types";

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
