import { useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { setAgentState } from "../agent-state/store";
import { ApiError, listRuns, listWorkflows } from "../api/client";
import type { Run, WorkflowVersion } from "../api/types";
import ErrorNotice from "../components/ErrorNotice";
import { groupWorkflowVersions, withRecentRuns } from "../domain/workflows";

/**
 * The Node Graphs tab (task t28, issue #56): replaces the standalone
 * Workflows tab with three sub-tabs — Nodes, Node Graphs, Active Graphs —
 * using RunView's aria-pressed segmented-toggle pattern (RunView.tsx:347-367,
 * `.view-toggle` in styles/app.css:391-411, reused verbatim) instead of
 * inventing a new control.
 *
 * Sub-tab selection lives in the `?tab=` URL search param (the same
 * URL-param-state discipline `useTimeRange` established for `since`/`until`)
 * so a sub-tab is bookmarkable and agent-drivable without any client-side
 * component state an agent can't see from the URL alone.
 *
 * This wave only ships the *shell*: the Nodes sub-tab (task t29, a
 * client-side catalog parsed from published workflow IRs) and the Active
 * Graphs sub-tab (task t31, the SSE-driven aliveness halo) both land later.
 * Until then they render honest, accessible empty states that say what will
 * appear there — never canned or fabricated rows (design honesty rule h14:
 * everything drawn must trace to a committed API row, and an empty state
 * that pretends otherwise is exactly the kind of decorative traffic h14
 * forbids). Only the Node Graphs sub-tab does real work this wave: it is
 * the exact per-workflow-cards view the old Workflows tab rendered, moved
 * here unchanged (domain/workflows.ts is untouched — task t29's territory).
 */

type SubTab = "nodes" | "graphs" | "active";

const DEFAULT_TAB: SubTab = "nodes";

function parseTab(value: string | null): SubTab {
  if (value === "nodes" || value === "graphs" || value === "active") {
    return value;
  }
  return DEFAULT_TAB;
}

/**
 * The Node Graphs sub-tab: every published workflow, one card per
 * `workflow_key`, listing its versions/digests/owner and its most recent
 * runs. This is routes/Workflows.tsx's entire data-fetch and render body
 * (task t8), moved verbatim under the new sub-tab — the underlying API
 * surface (`GET /v1alpha1/workflows`, `GET /v1alpha1/runs`) did not change,
 * only where in the nav it lives.
 *
 * Mounted only while `tab === "graphs"` (a plain conditional render, not a
 * route), so switching away and back re-fetches on mount just like any
 * other route would — and switching to Nodes/Active never leaves a stale
 * error notice or loading state from this panel behind.
 */
function NodeGraphsPanel() {
  const [versions, setVersions] = useState<WorkflowVersion[] | null>(null);
  const [runs, setRuns] = useState<Run[] | null>(null);
  const [error, setError] = useState<ApiError | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    setAgentState({ status: "loading", run: null });
    setError(null);
    setVersions(null);
    setRuns(null);

    const toApiError = (cause: unknown): ApiError =>
      cause instanceof ApiError
        ? cause
        : new ApiError(0, String(cause), "check the browser console");

    Promise.all([
      listWorkflows(controller.signal),
      listRuns(controller.signal, { sort: "updated_at" }),
    ])
      .then(([workflowList, runList]) => {
        if (controller.signal.aborted) return;
        setVersions(workflowList.items);
        setRuns(runList.items);
        setAgentState({ status: "ready", run: null });
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setVersions([]);
        setRuns([]);
        setError(toApiError(cause));
        // "ready" means the initial load finished, including finishing it
        // badly — the error renders alongside, not instead (same convention
        // as every other list view here).
        setAgentState({ status: "ready", run: null });
      });
    return () => controller.abort();
  }, []);

  const groups =
    versions !== null && runs !== null
      ? withRecentRuns(groupWorkflowVersions(versions), runs)
      : null;

  return (
    <div id="node-graphs-graphs-panel">
      {error ? <ErrorNotice error={error} /> : null}
      {groups === null ? (
        <p className="muted" id="workflows-loading">
          Loading workflows…
        </p>
      ) : groups.length === 0 ? (
        <p className="muted" id="workflows-empty">
          No workflows published yet. Publish one with{" "}
          <code>nodes workflow publish</code>.
        </p>
      ) : (
        <ul className="workflows-list" id="workflows-list">
          {groups.map((group) => (
            <li
              key={group.workflowKey}
              className="workflow-card"
              data-workflow-key={group.workflowKey}
            >
              <div className="workflow-card__head">
                <h2>{group.workflowKey}</h2>
                <span className="workflow-card__owner" data-workflow-owner>
                  {group.owner ?? "unowned"}
                </span>
              </div>

              <div className="table-scroll">
                <table className="ledger-table workflow-card__versions">
                  <caption>{group.versions.length} version(s)</caption>
                  <thead>
                    <tr>
                      <th scope="col">version</th>
                      <th scope="col">digest</th>
                      <th scope="col">published</th>
                    </tr>
                  </thead>
                  <tbody>
                    {group.versions.map((version) => (
                      <tr
                        key={version.digest}
                        data-workflow-digest={version.digest}
                      >
                        <td>{version.version}</td>
                        <td>
                          <code title={version.digest}>
                            {version.digest.slice(0, 20)}…
                          </code>
                        </td>
                        <td>
                          <time dateTime={version.created_at}>
                            {version.created_at}
                          </time>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>

              <div className="workflow-card__runs">
                <h3>Recent runs</h3>
                {group.recentRuns.length === 0 ? (
                  <p className="muted">No runs yet.</p>
                ) : (
                  <ul className="workflow-card__run-list">
                    {group.recentRuns.map((run) => (
                      <li key={run.id} data-run-id={run.id}>
                        <Link to={`/runs/${run.id}`}>{run.id}</Link>{" "}
                        <span data-run-state={run.state}>{run.state}</span>{" "}
                        <time dateTime={run.updated_at}>
                          {run.updated_at}
                        </time>
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/**
 * The Nodes sub-tab: task t29's cross-workflow node-definition catalog has
 * not landed yet — there is no data source to render honestly, so this is
 * an accessible empty state rather than a placeholder that implies data
 * exists. Trivially "ready" the instant it mounts: there is nothing to
 * fetch, so there is nothing to wait for.
 */
function NodesPanel() {
  useEffect(() => {
    setAgentState({ status: "ready", run: null });
  }, []);

  return (
    <div id="node-graphs-nodes-empty" className="node-graphs-empty">
      <p className="muted">
        No node catalog yet. This sub-tab will list every distinct node
        definition — one entry per node kind, not per workflow — derived
        client-side from published workflow IRs, once the catalog parser
        ships.
      </p>
    </div>
  );
}

/**
 * The Active Graphs sub-tab: task t31's SSE-driven aliveness halo has not
 * landed yet — same honesty stance as NodesPanel, no fabricated rows or
 * decorative "activity" ahead of the real thing.
 */
function ActiveGraphsPanel() {
  useEffect(() => {
    setAgentState({ status: "ready", run: null });
  }, []);

  return (
    <div id="node-graphs-active-empty" className="node-graphs-empty">
      <p className="muted">
        No active-graph view yet. This sub-tab will show which node graphs
        currently hold active tokens, with a breathing indicator driven by
        real run events — no graph will ever be marked alive without a
        committed event behind it.
      </p>
    </div>
  );
}

export function NodeGraphs() {
  const [searchParams, setSearchParams] = useSearchParams();
  const tab = parseTab(searchParams.get("tab"));

  const setTab = (next: SubTab) => {
    setSearchParams((prev) => {
      const params = new URLSearchParams(prev);
      if (next === DEFAULT_TAB) params.delete("tab");
      else params.set("tab", next);
      return params;
    });
  };

  return (
    <section className="view-rail node-graphs-view">
      <div className="node-graphs-view__head">
        <div>
          <h1>Node Graphs</h1>
          <p className="muted">
            Node definitions, the graphs strict node-to-node handovers and
            events wire them into, and which of those graphs are alive right
            now.
          </p>
        </div>
        <Link
          to="/workflows/new"
          id="new-workflow-link"
          className="author-workflow__button author-workflow__button--primary"
        >
          New workflow
        </Link>
      </div>

      <div
        id="node-graphs-toggle"
        className="view-toggle"
        role="group"
        aria-label="Node Graphs sub-tab"
      >
        <button
          type="button"
          id="node-graphs-toggle-nodes"
          aria-pressed={tab === "nodes"}
          onClick={() => setTab("nodes")}
        >
          Nodes
        </button>
        <button
          type="button"
          id="node-graphs-toggle-graphs"
          aria-pressed={tab === "graphs"}
          onClick={() => setTab("graphs")}
        >
          Node Graphs
        </button>
        <button
          type="button"
          id="node-graphs-toggle-active"
          aria-pressed={tab === "active"}
          onClick={() => setTab("active")}
        >
          Active Graphs
        </button>
      </div>

      {tab === "nodes" ? <NodesPanel /> : null}
      {tab === "graphs" ? <NodeGraphsPanel /> : null}
      {tab === "active" ? <ActiveGraphsPanel /> : null}
    </section>
  );
}

export default NodeGraphs;
