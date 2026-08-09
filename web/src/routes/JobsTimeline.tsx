import { useCallback, useEffect, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { setAgentState } from "../agent-state/store";
import { ApiError, listNodeRuns } from "../api/client";
import type { NodeRunListItem } from "../api/types";
import ErrorNotice from "../components/ErrorNotice";
import JobsTable from "../components/JobsTable";
import TimeRangeFilter, {
  type TimeRangeValue,
} from "../components/TimeRangeFilter";

/**
 * The jobs timeline (task t15): every node run across every run, newest
 * first — the cross-run counterpart to the per-run node list, reading
 * `GET /v1alpha1/node-runs` (task t11) the same way RunsBoard.tsx reads
 * `GET /v1alpha1/runs`.
 *
 * The time-range filter is server-side by construction: `since`/`until`
 * live in this component's state (mirrored to the `since`/`until` URL
 * search params, so the active range is shareable/bookmarkable) and are
 * passed straight through as `updated_since`/`updated_until` on every
 * fetch — there is no client-side filtering of an already-fetched list
 * anywhere in this file. Changing the range resets pagination and refetches
 * page one; "Load more" replays the same range with the last page's
 * `next_cursor`.
 */
export function JobsTimeline() {
  const [searchParams, setSearchParams] = useSearchParams();
  const since = searchParams.get("since") ?? undefined;
  const until = searchParams.get("until") ?? undefined;

  const [items, setItems] = useState<NodeRunListItem[] | null>(null);
  const [nextCursor, setNextCursor] = useState<string | undefined>(undefined);
  const [error, setError] = useState<ApiError | null>(null);
  const [loadingMore, setLoadingMore] = useState(false);

  // Page one: refetched whenever the range changes. Pagination state resets
  // with it — a new range means a new result set, not more of the old one.
  useEffect(() => {
    const controller = new AbortController();
    setAgentState({ status: "loading" });
    setError(null);
    setNextCursor(undefined);
    listNodeRuns(controller.signal, {
      updated_since: since,
      updated_until: until,
    })
      .then((page) => {
        if (controller.signal.aborted) return;
        setItems(page.items);
        setNextCursor(page.next_cursor);
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
        // "ready" means the initial load finished, including finishing it
        // badly — same convention RunsBoard/RunsList use.
        setAgentState({ status: "ready" });
      });
    return () => controller.abort();
  }, [since, until]);

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

  const applyRange = useCallback(
    (range: TimeRangeValue) => {
      setSearchParams((prev) => {
        const next = new URLSearchParams(prev);
        if (range.since) next.set("since", range.since);
        else next.delete("since");
        if (range.until) next.set("until", range.until);
        else next.delete("until");
        return next;
      });
    },
    [setSearchParams],
  );

  return (
    <section className="container jobs-timeline">
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
          <JobsTable
            id="jobs-table"
            items={items}
            caption={`${items.length} node run(s), newest first`}
          />
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
