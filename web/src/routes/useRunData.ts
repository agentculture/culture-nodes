import { useEffect, useState } from "react";
import { ApiError, getLedger, getRun, getWorkflow } from "../api/client";
import type { LedgerRecord, RunView as RunViewPayload } from "../api/types";
import { parseWorkflowGraph, type WorkflowGraph } from "../domain/graph";

export interface RunData {
  view: RunViewPayload | null;
  graph: WorkflowGraph | null;
  ledger: LedgerRecord[];
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
    loading: true,
    error: null,
  });

  useEffect(() => {
    if (!runId) return;
    const controller = new AbortController();
    const { signal } = controller;

    setData({ view: null, graph: null, ledger: [], loading: true, error: null });

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
        if (signal.aborted) return;
        setData({ view, graph, ledger, loading: false, error: null });
      } catch (cause) {
        if (signal.aborted) return;
        setData({
          view: null,
          graph: null,
          ledger: [],
          loading: false,
          error: asApiError(cause),
        });
      }
    })();

    return () => controller.abort();
  }, [runId]);

  return data;
}
