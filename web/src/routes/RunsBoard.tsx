import { useCallback, useEffect, useRef, useState } from "react";
import { setAgentState } from "../agent-state/store";
import { ApiError, listRuns } from "../api/client";
import type { Run } from "../api/types";
import ErrorNotice from "../components/ErrorNotice";
import RunCard from "../components/RunCard";
import TimeRangeFilter from "../components/TimeRangeFilter";
import { groupRunsByState, RUN_STATE_COLUMNS } from "../domain/run-board";
import { useReducedMotion } from "../hooks/useReducedMotion";
import type { SharedEventType } from "../hooks/useSharedEvents";
import { useSnapshotReconcile } from "../hooks/useSnapshotReconcile";
import { useTimeRange } from "../hooks/useTimeRange";

/**
 * Every run-lifecycle event that can move a card between columns — same
 * vocabulary as RunsList (this view groups the identical `GET
 * /v1alpha1/runs` rows by state instead of listing them flat), a stable
 * module-level reference as useSharedEvents requires (issue #46).
 */
const RUNS_BOARD_EVENT_TYPES = [
  "dev.culture.nodes.run.created",
  "dev.culture.nodes.run.waiting",
  "dev.culture.nodes.run.completed",
  "dev.culture.nodes.run.failed",
  "dev.culture.nodes.run.cancelled",
  "dev.culture.nodes.run.bounded",
] as const satisfies readonly SharedEventType[];

/** Mirrors the Mesh view's attribution-refresh discipline (Mesh.tsx). */
const REFRESH_DEBOUNCE_MS = 4000;

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
 *
 * Auto-refresh (issue #46, task t30): same debounced reloadKey idiom as
 * RunsList — a run-lifecycle event schedules a background refetch that
 * never nulls the rendered columns nor regresses agent-state to "loading"
 * (that only happens on the initial load / an explicit range change).
 */
export function RunsBoard() {
  const { since, until, applyRange } = useTimeRange();
  const [runs, setRuns] = useState<Run[] | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [reloadKey, setReloadKey] = useState(0);
  const reloadTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const lastReload = useRef(0);
  const reducedMotion = useReducedMotion();

  const scheduleReload = useCallback(() => {
    if (reloadTimer.current) return;
    const elapsed = Date.now() - lastReload.current;
    const wait = Math.max(0, REFRESH_DEBOUNCE_MS - elapsed);
    reloadTimer.current = setTimeout(() => {
      reloadTimer.current = undefined;
      lastReload.current = Date.now();
      setReloadKey((key) => key + 1);
    }, wait);
  }, []);

  useEffect(
    () => () => {
      if (reloadTimer.current) clearTimeout(reloadTimer.current);
    },
    [],
  );

  const { resolveSnapshot } = useSnapshotReconcile(
    RUNS_BOARD_EVENT_TYPES,
    scheduleReload,
  );

  useEffect(() => {
    const controller = new AbortController();
    setAgentState({ status: "loading", run: null });
    setError(null);
    // See RunsList: a range change resets to the loading state instead of
    // rendering the previous range's columns while the new fetch runs.
    setRuns(null);
    listRuns(controller.signal, {
      sort: "updated_at",
      updated_since: since,
      updated_until: until,
    })
      .then((list) => {
        if (controller.signal.aborted) return;
        setRuns(list.items);
        resolveSnapshot();
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
        resolveSnapshot();
        // As in RunsList: "ready" means the initial load finished, including
        // finishing it badly — the error renders alongside, not instead.
        setAgentState({ status: "ready", run: null });
      });
    return () => controller.abort();
  }, [since, until, resolveSnapshot]);

  // The SSE-triggered background refresh (issue #46): skips the very first
  // render (reloadKey === 0, already handled above), never nulls `runs`,
  // never touches agent-state — a failed refresh keeps the last honest
  // columns and reports the error alongside.
  useEffect(() => {
    if (reloadKey === 0) return;
    const controller = new AbortController();
    listRuns(controller.signal, {
      sort: "updated_at",
      updated_since: since,
      updated_until: until,
    })
      .then((list) => {
        if (controller.signal.aborted) return;
        setRuns(list.items);
        setError(null);
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setError(
          cause instanceof ApiError
            ? cause
            : new ApiError(0, String(cause), "check the browser console"),
        );
      });
    return () => controller.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [reloadKey]);

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
