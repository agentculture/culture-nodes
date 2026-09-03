import { useCallback, useEffect, useRef, useState } from "react";
import { setAgentState } from "../agent-state/store";
import { ApiError, listNodeRuns, listRuns } from "../api/client";
import type { NodeRunListItem, Run } from "../api/types";
import ErrorNotice from "../components/ErrorNotice";
import JobsTable from "../components/JobsTable";
import TimeRangeFilter from "../components/TimeRangeFilter";
import type { SharedEventType } from "../hooks/useSharedEvents";
import { useSnapshotReconcile } from "../hooks/useSnapshotReconcile";
import { useTimeRange } from "../hooks/useTimeRange";

/**
 * Every node-run/attempt lifecycle event that can change a row this view
 * renders — a stable module-level reference, as useSharedEvents requires
 * (issue #46).
 */
const JOBS_EVENT_TYPES = [
  "dev.culture.nodes.node-run.ready",
  "dev.culture.nodes.attempt.started",
  "dev.culture.nodes.actor.accepted",
  "dev.culture.nodes.attempt.completed",
  "dev.culture.nodes.node-run.failed",
  "dev.culture.nodes.attempt.retry-scheduled",
  "dev.culture.nodes.runner.operation-completed",
  "dev.culture.nodes.contract.rejected",
] as const satisfies readonly SharedEventType[];

/** Mirrors the Mesh view's attribution-refresh discipline (Mesh.tsx). */
const REFRESH_DEBOUNCE_MS = 4000;

/**
 * The jobs timeline (task t15): every node run across every run, newest
 * first — the cross-run counterpart to the per-run node list, reading
 * `GET /v1alpha1/node-runs` (task t11) the same way RunsBoard.tsx reads
 * `GET /v1alpha1/runs`.
 *
 * The time-range filter is server-side by construction: `since`/`until`
 * live in the URL search params (via useTimeRange — the same hook Board and
 * Runs use, issue #23 — so the active range is shareable/bookmarkable) and
 * are passed straight through as `updated_since`/`updated_until` on every
 * fetch — there is no client-side filtering of an already-fetched list
 * anywhere in this file. Changing the range resets pagination and refetches
 * page one; "Load more" replays the same range with the last page's
 * `next_cursor`.
 *
 * `runsById` (task t5) is a second, best-effort lookup: `GET
 * /v1alpha1/node-runs` carries no run name/category of its own (only
 * `run_id`), so this view separately fetches `GET /v1alpha1/runs` for the
 * same since/until window and joins by id, purely to let JobsTable show a
 * name/category chip next to the run link. It is deliberately non-blocking
 * and non-fatal — a failed or incomplete lookup just leaves rows showing
 * the bare run id, exactly as before this task, never an error state of its
 * own.
 */
export function JobsTimeline() {
  const { since, until, applyRange } = useTimeRange();

  const [items, setItems] = useState<NodeRunListItem[] | null>(null);
  const [nextCursor, setNextCursor] = useState<string | undefined>(undefined);
  const [error, setError] = useState<ApiError | null>(null);
  const [loadingMore, setLoadingMore] = useState(false);
  const [runsById, setRunsById] = useState<Record<string, Run>>({});
  const [reloadKey, setReloadKey] = useState(0);
  const reloadTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const lastReload = useRef(0);

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
    JOBS_EVENT_TYPES,
    scheduleReload,
  );

  // Page one: refetched whenever the range changes. Pagination state resets
  // with it — a new range means a new result set, not more of the old one.
  useEffect(() => {
    const controller = new AbortController();
    setAgentState({ status: "loading" });
    setError(null);
    setNextCursor(undefined);
    // Same reset idiom as RunsList/RunsBoard: never render the previous
    // range's rows while the new range is loading.
    setItems(null);
    listNodeRuns(controller.signal, {
      updated_since: since,
      updated_until: until,
    })
      .then((page) => {
        if (controller.signal.aborted) return;
        setItems(page.items);
        setNextCursor(page.next_cursor);
        resolveSnapshot();
        setAgentState({ status: "ready" });
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setItems([]);
        setError(
          cause instanceof ApiError
            ? cause
            : new ApiError(0, String(cause), "check the browser console"),
        );
        resolveSnapshot();
        // "ready" means the initial load finished, including finishing it
        // badly — same convention RunsBoard/RunsList use.
        setAgentState({ status: "ready" });
      });
    return () => controller.abort();
  }, [since, until, resolveSnapshot]);

  // The name/category lookup (task t5): a separate, non-blocking fetch of
  // GET /v1alpha1/runs for the same window. It does not gate `status` —
  // an agent/test only needs to know the node-run rows themselves loaded —
  // and its own failure is swallowed: rows just fall back to the bare run
  // id, same as if this effect did not exist.
  useEffect(() => {
    const controller = new AbortController();
    listRuns(controller.signal, { updated_since: since, updated_until: until })
      .then((list) => {
        if (controller.signal.aborted) return;
        setRunsById(Object.fromEntries(list.items.map((run) => [run.id, run])));
      })
      .catch(() => {
        if (controller.signal.aborted) return;
        setRunsById({});
      });
    return () => controller.abort();
  }, [since, until]);

  // The SSE-triggered background refresh (issue #46): skips the very first
  // render (reloadKey === 0, already handled by the two effects above).
  // Re-fetches page one for the current range and the name/category join
  // alongside it — never nulls `items`, never touches agent-state. A
  // refresh deliberately replaces `nextCursor` with the fresh page's own,
  // which means any additional pages a user had "Load more"-d in are
  // dropped in favor of an honest re-synced page one, rather than trying to
  // merge server-side inserts into an already-paginated view.
  useEffect(() => {
    if (reloadKey === 0) return;
    const controller = new AbortController();
    Promise.all([
      listNodeRuns(controller.signal, { updated_since: since, updated_until: until }),
      listRuns(controller.signal, { updated_since: since, updated_until: until }),
    ])
      .then(([page, list]) => {
        if (controller.signal.aborted) return;
        setItems(page.items);
        setNextCursor(page.next_cursor);
        setRunsById(Object.fromEntries(list.items.map((run) => [run.id, run])));
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
    setLoadingMore(true);
    listNodeRuns(undefined, {
      updated_since: since,
      updated_until: until,
      cursor: nextCursor,
    })
      .then((page) => {
        setItems((prev) => [...(prev ?? []), ...page.items]);
        setNextCursor(page.next_cursor);
      })
      .catch((cause: unknown) => {
        setError(
          cause instanceof ApiError
            ? cause
            : new ApiError(0, String(cause), "check the browser console"),
        );
      })
      .finally(() => setLoadingMore(false));
  }, [nextCursor, since, until]);

  return (
    <section className="view-rail jobs-timeline">
      <h1>Jobs</h1>
      <p className="muted">
        Every node run across every run, newest first by last update.
      </p>

      <TimeRangeFilter since={since} until={until} onApply={applyRange} />

      {error ? <ErrorNotice error={error} /> : null}

      {items === null ? (
        <p className="muted" id="jobs-loading">
          Loading node runs…
        </p>
      ) : items.length === 0 ? (
        <p className="muted" id="jobs-empty">
          No node runs in this range.
        </p>
      ) : (
        <>
          <div className="table-scroll">
            <JobsTable
              id="jobs-table"
              items={items}
              runsById={runsById}
              caption={`${items.length} node run(s), newest first`}
            />
          </div>
          {nextCursor ? (
            <button
              type="button"
              id="jobs-load-more"
              className="jobs-timeline__load-more"
              onClick={loadMore}
              disabled={loadingMore}
            >
              {loadingMore ? "Loading…" : "Load more"}
            </button>
          ) : null}
        </>
      )}
    </section>
  );
}

export default JobsTimeline;
