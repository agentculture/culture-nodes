import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { ApiError, listRuns } from "../api/client";
import type { Run } from "../api/types";
import { setAgentState } from "../agent-state/store";
import ErrorNotice from "../components/ErrorNotice";

/**
 * The run list — the entry point into the Run view (PRD §8.6 Operations, in
 * its smallest useful form: search runs comes later, listing them does not).
 */
export function RunsList() {
  const [runs, setRuns] = useState<Run[] | null>(null);
  const [error, setError] = useState<ApiError | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    setAgentState({ status: "loading", run: null });
    listRuns(controller.signal)
      .then((list) => {
        if (controller.signal.aborted) return;
        setRuns(list.items);
        setAgentState({ status: "ready", run: null });
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setRuns([]);
        setError(
          cause instanceof ApiError
            ? cause
            : new ApiError(0, String(cause), "check the browser console"),
        );
        // "ready" means the view finished its initial load — including
        // finishing it badly. An agent reading agent-state needs to know the
        // page has settled; the error is reported alongside, not instead.
        setAgentState({ status: "ready", run: null });
      });
    return () => controller.abort();
  }, []);

  return (
    <section className="container runs-list">
      <h1>Runs</h1>
      {error ? <ErrorNotice error={error} /> : null}
      {runs === null ? (
        <p className="muted" id="runs-loading">
          Loading runs…
        </p>
      ) : runs.length === 0 ? (
        <p className="muted" id="runs-empty">
          No runs yet. Create one with <code>nodes run create</code>.
        </p>
      ) : (
        <table className="ledger-table" id="runs-table">
          <thead>
            <tr>
              <th scope="col">run</th>
              <th scope="col">state</th>
              <th scope="col">workflow digest</th>
              <th scope="col">created</th>
            </tr>
          </thead>
          <tbody>
            {runs.map((run) => (
              <tr key={run.id} data-run-id={run.id}>
                <th scope="row">
                  <Link to={`/runs/${run.id}`}>{run.id}</Link>
                </th>
                <td data-run-state={run.state}>{run.state}</td>
                <td>
                  <code title={run.workflow_digest}>
                    {run.workflow_digest.slice(0, 20)}…
                  </code>
                </td>
                <td>
                  <time dateTime={run.created_at}>{run.created_at}</time>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

export default RunsList;
