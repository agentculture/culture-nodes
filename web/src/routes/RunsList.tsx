import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { ApiError, listRuns } from "../api/client";
import type { Run } from "../api/types";
import type { RunState } from "../api/types";
import { setAgentState } from "../agent-state/store";
import CategoryChip from "../components/CategoryChip";
import ErrorNotice from "../components/ErrorNotice";
import RunStateChip from "../components/RunStateChip";
import TimeRangeFilter from "../components/TimeRangeFilter";
import { formatRelativeTime } from "../domain/run-board";
import { runDisplayName } from "../domain/usage";
import { useSharedEvents, type SharedEventType } from "../hooks/useSharedEvents";
import { useTimeRange } from "../hooks/useTimeRange";

/**
 * Every run-lifecycle event that can change a row this table renders (its
 * `state` column) or its place in the `updated_at` ordering — a stable
 * module-level reference, as useSharedEvents requires (issue #46).
 */
const RUNS_LIST_EVENT_TYPES = [
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
 * The run list — the entry point into the Run view (PRD §8.6 Operations, in
 * its smallest useful form: search runs comes later, listing them does not).
 *
 * The time-range filter is the Jobs view's control and state idiom verbatim
 * (issue #23): `since`/`until` ride the URL search params via useTimeRange
 * and go to the API as `updated_since`/`updated_until` — server-side
 * scoping, never a client-side re-slice of an already-fetched list. The
 * list sorts by `updated_at` to match its own ordering statement (and the
 * other two views).
 *
 * Auto-refresh (issue #46, task t30): a run-lifecycle event on the shared
 * cross-run stream schedules a debounced background refetch via
 * `reloadKey`. The reload effect below is deliberately a *second* effect
 * from the initial-load/range-change one: a range change is a deliberate
 * "start over" (resets to the loading state — the `runs === null` gate
 * below, review finding on #27), but an SSE-triggered refresh must never
 * regress `runs` to null or agent-state back to "loading" — stale-while-
 * revalidate holds the rendered table until the fresh rows arrive.
 *
 * State and time render exactly as the Board does (task t27): `RunStateChip`
 * rather than a bare word, and `formatRelativeTime` with the raw RFC3339
 * instant kept on `title`/`dateTime`. The same run in two views was reading as
 * two different things; relative time is what a reader scans by, and the exact
 * instant is one hover away rather than gone.
 */
export function RunsList() {
  const { since, until, applyRange } = useTimeRange();
  const [runs, setRuns] = useState<Run[] | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [reloadKey, setReloadKey] = useState(0);
  const [stateFilter, setStateFilter] = useState<RunState | "">("");
  const [nextCursor, setNextCursor] = useState<string | undefined>();
  const [loadingMore, setLoadingMore] = useState(false);
  const [expandedGroups, setExpandedGroups] = useState<Set<string>>(new Set());
  const reloadTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const lastReload = useRef(0);
  const filterKey = JSON.stringify([since, until, stateFilter]);
  const currentFilterKey = useRef(filterKey);
  currentFilterKey.current = filterKey;

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

  useSharedEvents(RUNS_LIST_EVENT_TYPES, scheduleReload);

  useEffect(() => {
    const controller = new AbortController();
    setAgentState({ status: "loading", run: null });
    setError(null);
    setLoadingMore(false);
    // A range change must not keep rendering the previous range's rows
    // while the new request is in flight — the loading state is gated on
    // runs === null, so reset it (review finding on #27).
    setRuns(null);
    listRuns(controller.signal, {
      sort: "updated_at",
      updated_since: since,
      updated_until: until,
      ...(stateFilter ? { state: stateFilter } : {}),
    })
      .then((list) => {
        if (controller.signal.aborted) return;
        setRuns(list.items);
        setNextCursor(list.next_cursor);
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
  }, [since, until, stateFilter]);

  // The SSE-triggered background refresh (issue #46): fires only after the
  // initial load (reloadKey === 0 is that first render, already handled
  // above). Never nulls `runs`, never touches agent-state — a failed
  // refresh keeps the last honest rows and reports the error alongside.
  useEffect(() => {
    if (reloadKey === 0) return;
    const controller = new AbortController();
    listRuns(controller.signal, {
      sort: "updated_at",
      updated_since: since,
      updated_until: until,
      ...(stateFilter ? { state: stateFilter } : {}),
    })
      .then((list) => {
        if (controller.signal.aborted) return;
        setRuns(list.items);
        setNextCursor(list.next_cursor);
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

  const loadMore = useCallback(() => {
    if (!nextCursor) return;
    const requestFilterKey = filterKey;
    setLoadingMore(true);
    listRuns(undefined, {
      sort: "updated_at",
      updated_since: since,
      updated_until: until,
      ...(stateFilter ? { state: stateFilter } : {}),
      cursor: nextCursor,
    })
      .then((page) => {
        if (currentFilterKey.current !== requestFilterKey) return;
        setRuns((current) => [...(current ?? []), ...page.items]);
        setNextCursor(page.next_cursor);
      })
      .catch((cause: unknown) => {
        if (currentFilterKey.current !== requestFilterKey) return;
        setError(cause instanceof ApiError ? cause : new ApiError(0, String(cause), "check the browser console"));
      })
      .finally(() => {
        if (currentFilterKey.current === requestFilterKey) setLoadingMore(false);
      });
  }, [nextCursor, since, until, stateFilter, filterKey]);

  const groupedRuns = runs === null ? null : runs.reduce<Array<{ key: string; runs: Run[] }>>((groups, run) => {
    const previous = groups[groups.length - 1];
    const workflow = run.workflow_key ?? run.workflow_digest;
    if (run.state === "failed" && previous?.runs[0].state === "failed" && (previous.runs[0].workflow_key ?? previous.runs[0].workflow_digest) === workflow) {
      previous.runs.push(run);
    } else {
      groups.push({ key: `${run.id}:${workflow}`, runs: [run] });
    }
    return groups;
  }, []);

  return (
    <section className="view-rail runs-list">
      <h1>Runs</h1>
      <p className="muted">Every run, newest first by last update.</p>

      <TimeRangeFilter since={since} until={until} onApply={applyRange} />
      <label className="runs-list__state-filter">
        State
        <select value={stateFilter} onChange={(event) => setStateFilter(event.target.value as RunState | "")}>
          <option value="">All states</option>
          {(["created", "running", "waiting", "completed", "failed", "cancelled"] as RunState[]).map((state) => <option key={state} value={state}>{state}</option>)}
        </select>
      </label>

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
        <>
          <div className="table-scroll">
          <table className="ledger-table" id="runs-table">
            <thead>
              <tr>
                <th scope="col">run</th>
                <th scope="col">category</th>
                <th scope="col">state</th>
                <th scope="col">workflow key</th>
                <th scope="col">workflow digest</th>
                <th scope="col">created</th>
                <th scope="col">updated</th>
              </tr>
            </thead>
            <tbody>
              {groupedRuns?.flatMap((group) => {
                const collapsed = group.runs.length > 1 && !expandedGroups.has(group.key);
                const visibleRuns = collapsed ? group.runs.slice(0, 1) : group.runs;
                return visibleRuns.map((run, index) => {
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
                    <td data-run-state={run.state}>
                      <RunStateChip state={run.state} />
                      {index === 0 && group.runs.length > 1 ? (
                        <button type="button" className="runs-list__count-badge" aria-label={`${collapsed ? "Expand" : "Collapse"} ${group.runs.length} failed runs`} onClick={() => setExpandedGroups((current) => {
                          const next = new Set(current);
                          if (next.has(group.key)) next.delete(group.key); else next.add(group.key);
                          return next;
                        })}>{group.runs.length}</button>
                      ) : null}
                    </td>
                    <td>{run.workflow_key ? <code>{run.workflow_key}</code> : <span className="muted">unknown</span>}</td>
                    <td>
                      <code title={run.workflow_digest}>
                        {run.workflow_digest.slice(0, 20)}…
                      </code>
                    </td>
                    <td>
                      <time dateTime={run.created_at} title={run.created_at}>
                        {formatRelativeTime(run.created_at)}
                      </time>
                    </td>
                    <td>
                      <time dateTime={run.updated_at} title={run.updated_at}>
                        {formatRelativeTime(run.updated_at)}
                      </time>
                    </td>
                  </tr>
                );
              })})}
            </tbody>
          </table>
          </div>
          {nextCursor ? <button type="button" className="jobs-timeline__load-more" onClick={loadMore} disabled={loadingMore}>{loadingMore ? "Loading…" : "Load more"}</button> : null}
        </>
      )}
    </section>
  );
}

export default RunsList;
