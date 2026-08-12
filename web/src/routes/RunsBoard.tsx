import { useEffect, useState } from "react";
import { setAgentState } from "../agent-state/store";
import { ApiError, listRuns } from "../api/client";
import type { Run } from "../api/types";
import ErrorNotice from "../components/ErrorNotice";
import RunCard from "../components/RunCard";
import TimeRangeFilter from "../components/TimeRangeFilter";
import { groupRunsByState, RUN_STATE_COLUMNS } from "../domain/run-board";
import { useReducedMotion } from "../hooks/useReducedMotion";
import { useTimeRange } from "../hooks/useTimeRange";

/**
 * The runs board (PRD §8.6 Operations): every run as a card, grouped into
 * one column per `Run.state` (openapi.yaml's `RunState` enum — `created,
 * running, waiting, completed, failed, cancelled`). It renders committed API
 * state only: `GET /v1alpha1/runs` sorted by `updated_at` (task t11), the
 * same one-shot fetch idiom RunsList.tsx uses (AbortController + agent-state
 * loading/ready), just with the board's own params instead of the run
 * list's defaults.
 *
 * The time-range filter is the Jobs view's control and state idiom verbatim
 * (issue #23): `since`/`until` ride the URL search params via useTimeRange
 * and go to the API as `updated_since`/`updated_until` — server-side
 * scoping, never a client-side re-slice of an already-fetched list.
 *
 * A run waiting on an approval node reports `state: "waiting"` exactly like
 * any other external wait — the list endpoint carries no node-run detail —
 * so it appears here under "waiting" with everything else that is, never in
 * a column of its own (see groupRunsByState in domain/run-board.ts).
 */
export function RunsBoard() {
  const { since, until, applyRange } = useTimeRange();
  const [runs, setRuns] = useState<Run[] | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const reducedMotion = useReducedMotion();

  useEffect(() => {
    const controller = new AbortController();
    setAgentState({ status: "loading", run: null });
    setError(null);
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
        // As in RunsList: "ready" means the initial load finished, including
        // finishing it badly — the error renders alongside, not instead.
        setAgentState({ status: "ready", run: null });
      });
    return () => controller.abort();
  }, [since, until]);

  const grouped = runs ? groupRunsByState(runs) : null;

  return (
    <section className="view-rail runs-board">
      <h1>Board</h1>
      <p className="muted">
        Every run, one column per state, newest first by last update.
      </p>

      <TimeRangeFilter since={since} until={until} onApply={applyRange} />

      {error ? <ErrorNotice error={error} /> : null}
      {runs === null ? (
        <p className="muted" id="runs-board-loading">
          Loading runs…
        </p>
      ) : runs.length === 0 ? (
        <p className="muted" id="runs-board-empty">
          No runs in this range. Create one with <code>nodes run create</code>
          , or widen the range.
        </p>
      ) : (
        <div className="runs-board__columns" id="runs-board-columns">
          {RUN_STATE_COLUMNS.map((state) => {
            const column = grouped?.[state] ?? [];
            return (
              <div
                key={state}
                className="runs-board__column"
                data-column-state={state}
              >
                <h2 className="runs-board__column-head">
                  {state}{" "}
                  <span className="runs-board__count">{column.length}</span>
                </h2>
                {column.length === 0 ? (
                  <p className="muted runs-board__column-empty">No runs</p>
                ) : (
                  <ul className="runs-board__cards">
                    {column.map((run) => (
                      <li key={run.id}>
                        <RunCard run={run} reducedMotion={reducedMotion} />
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            );
          })}
        </div>
      )}
    </section>
  );
}

export default RunsBoard;
