import { useEffect, useState } from "react";
import { ApiError, getLedger, getRun, getWorkflow, listNodeRuns } from "../api/client";
import type { LedgerRecord, RunView as RunViewPayload, Usage } from "../api/types";
import { parseWorkflowGraph, type WorkflowGraph } from "../domain/graph";

export interface RunData {
  view: RunViewPayload | null;
  graph: WorkflowGraph | null;
  ledger: LedgerRecord[];
  /**
   * Node-run-level usage (task t2/t5), keyed by node run id. `GET
   * /v1alpha1/runs/{id}`'s nested `node_runs` carry no `usage` field of
   * their own (only `GET /v1alpha1/node-runs`'s flat listing items do — see
   * api/openapi/openapi.yaml's NodeRunListItem vs NodeRun), so this is
   * filled from a second, best-effort fetch of that listing, matched back
   * onto this run by `run_id`. The listing has no `run_id` filter and is
   * paginated by recency across the whole namespace, so this is honestly a
   * best-effort join, not a guarantee — a node run absent from the map
   * simply has no entry here (never a fabricated one), and NodeDetailPanel
   * renders that absence as "usage data not available" rather than a zero.
   */
  usageByNodeRunId: Record<string, Usage>;
  loading: boolean;
  error: ApiError | null;
}

const asApiError = (cause: unknown): ApiError =>
  cause instanceof ApiError
    ? cause
    : new ApiError(0, String(cause), "check the browser console");

/**
 * Load everything the Run view needs: the run snapshot, the immutable
 * workflow version the run is pinned to (fetched by digest — a run always
 * renders the graph it actually executes, never "latest"), and the run's
 * ledger records for the node-detail delta.
 */
export function useRunData(runId: string | undefined): RunData {
  const [data, setData] = useState<RunData>({
    view: null,
    graph: null,
    ledger: [],
    usageByNodeRunId: {},
    loading: true,
    error: null,
  });

  useEffect(() => {
    if (!runId) return;
    const controller = new AbortController();
    const { signal } = controller;

    setData({
      view: null,
      graph: null,
      ledger: [],
      usageByNodeRunId: {},
      loading: true,
      error: null,
    });

    (async () => {
      try {
        const view = await getRun(runId, signal);
        const workflow = await getWorkflow(view.run.workflow_digest, signal);
        const graph = parseWorkflowGraph(workflow.normalized_ir);
        // The ledger is supporting detail, not the view's spine: a ledger
        // that cannot be read must not blank the graph.
        let ledger: LedgerRecord[] = [];
        try {
          ledger = (await getLedger(runId, signal)).items;
        } catch {
          ledger = [];
        }
        // Same non-fatal treatment for the node-run usage join: it enriches
        // NodeDetailPanel, it does not gate the graph rendering at all.
        let usageByNodeRunId: Record<string, Usage> = {};
        try {
          const page = await listNodeRuns(signal, { limit: 500 });
          usageByNodeRunId = Object.fromEntries(
            page.items
              .filter((item) => item.run_id === runId)
              .map((item) => [item.id, item.usage] as const),
          );
        } catch {
          usageByNodeRunId = {};
        }
        if (signal.aborted) return;
        setData({ view, graph, ledger, usageByNodeRunId, loading: false, error: null });
      } catch (cause) {
        if (signal.aborted) return;
        setData({
          view: null,
          graph: null,
          ledger: [],
          usageByNodeRunId: {},
          loading: false,
          error: asApiError(cause),
        });
      }
    })();

    return () => controller.abort();
  }, [runId]);

  return data;
}
