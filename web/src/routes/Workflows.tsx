import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { setAgentState } from "../agent-state/store";
import { ApiError, listRuns, listWorkflows } from "../api/client";
import type { Run, WorkflowVersion } from "../api/types";
import ErrorNotice from "../components/ErrorNotice";
import { groupWorkflowVersions, withRecentRuns } from "../domain/workflows";

/**
 * The Workflows view (task t8): every published workflow, one card per
 * `workflow_key`, listing its versions/digests/owner and its most recent
 * runs — using only the two documented endpoints this task is scoped to,
 * `GET /v1alpha1/workflows` (task t8) and `GET /v1alpha1/workflows/{digest}`
 * (published earlier; the list already carries everything a card needs, so
 * this view never has to follow up per-digest).
 *
 * "Recent runs" is not a server capability — `GET /v1alpha1/runs` carries no
 * `workflow_key`, only `workflow_digest` (components.schemas.Run) — so this
 * view fetches the run list once (sorted `updated_at`, the same "newest
 * first" contract every other list view here uses) and associates each run
 * to its workflow client-side, purely by matching digest against the
 * versions that workflow_key has published (domain/workflows.ts). No new
 * endpoint, no server-side filter parameter: zero API surface change.
 *
 * Both requests race in one `Promise.all`; agent-state only reports "ready"
 * once both have settled, same "loading means still-in-flight, ready means
 * settled — including settled badly" convention RunsList/RunsBoard/Jobs use.
 */
export function Workflows() {
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
    <section className="view-rail workflows-view">
      <div className="workflows-view__head">
        <div>
          <h1>Workflows</h1>
          <p className="muted">
            Every published workflow, its versions and digests, and its most
            recent runs.
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
    </section>
  );
}

export default Workflows;
