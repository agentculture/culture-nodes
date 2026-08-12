import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { ApiError, listRuns } from "../api/client";
import type { Run } from "../api/types";
import { setAgentState } from "../agent-state/store";
import CategoryChip from "../components/CategoryChip";
import ErrorNotice from "../components/ErrorNotice";
import TimeRangeFilter from "../components/TimeRangeFilter";
import { runDisplayName } from "../domain/usage";
import { useTimeRange } from "../hooks/useTimeRange";

/**
 * The run list — the entry point into the Run view (PRD §8.6 Operations, in
 * its smallest useful form: search runs comes later, listing them does not).
 *
 * The time-range filter is the Jobs view's control and state idiom verbatim
 * (issue #23): `since`/`until` ride the URL search params via useTimeRange
 * and go to the API as `updated_since`/`updated_until` — server-side
 * scoping, never a client-side re-slice of an already-fetched list. The
 * list sorts by `updated_at` to match its own ordering statement (and the
 * other two views).
 */
export function RunsList() {
  const { since, until, applyRange } = useTimeRange();
  const [runs, setRuns] = useState<Run[] | null>(null);
  const [error, setError] = useState<ApiError | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    setAgentState({ status: "loading", run: null });
    setError(null);
    // A range change must not keep rendering the previous range's rows
    // while the new request is in flight — the loading state is gated on
    // runs === null, so reset it (review finding on #27).
    setRuns(null);
    listRuns(controller.signal, {
      sort: "updated_at",
      updated_since: since,
      updated_until: until,
    })
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
  }, [since, until]);

  return (
    <section className="view-rail runs-list">
      <h1>Runs</h1>
      <p className="muted">Every run, newest first by last update.</p>

      <TimeRangeFilter since={since} until={until} onApply={applyRange} />

      {error ? <ErrorNotice error={error} /> : null}
      {runs === null ? (
        <p className="muted" id="runs-loading">
          Loading runs…
        </p>
      ) : runs.length === 0 ? (
        <p className="muted" id="runs-empty">
          No runs in this range. Create one with <code>nodes run create</code>
          , or widen the range.
        </p>
      ) : (
        <div className="table-scroll">
          <table className="ledger-table" id="runs-table">
            <thead>
              <tr>
                <th scope="col">run</th>
                <th scope="col">category</th>
                <th scope="col">state</th>
                <th scope="col">workflow digest</th>
                <th scope="col">created</th>
                <th scope="col">updated</th>
              </tr>
            </thead>
            <tbody>
              {runs.map((run) => {
                const display = runDisplayName(run);
                return (
                  <tr key={run.id} data-run-id={run.id}>
                    <th scope="row">
                      <Link to={`/runs/${run.id}`} title={run.id}>
                        {display.derived ? (
                          <span
                            className="run-name run-name--derived"
                            data-derived="true"
                            title={`derived guess, not a given name: "${display.text}"`}
                          >
                            {display.text}
                          </span>
                        ) : (
                          display.text
                        )}
                      </Link>
                    </th>
                    <td>
                      {run.category ? (
                        <CategoryChip category={run.category} />
                      ) : (
                        <span className="muted">—</span>
                      )}
                    </td>
                    <td data-run-state={run.state}>{run.state}</td>
                    <td>
                      <code title={run.workflow_digest}>
                        {run.workflow_digest.slice(0, 20)}…
                      </code>
                    </td>
                    <td>
                      <time dateTime={run.created_at}>{run.created_at}</time>
                    </td>
                    <td>
                      <time dateTime={run.updated_at}>{run.updated_at}</time>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

export default RunsList;
