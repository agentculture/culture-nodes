import { useCallback } from "react";
import { useSearchParams } from "react-router-dom";
import type { TimeRangeValue } from "../components/TimeRangeFilter";

/**
 * The one time-range state idiom, shared by Jobs, Board and Runs (issue
 * #23): the active range lives in the `since`/`until` URL search params —
 * shareable and bookmarkable — and this hook is the single mapping between
 * those params and the `TimeRangeFilter` control. Each view passes the
 * bounds straight through to its own list call as
 * `updated_since`/`updated_until`; nothing here (or in the views) filters
 * an already-fetched list client-side.
 */
export function useTimeRange() {
  const [searchParams, setSearchParams] = useSearchParams();
  const since = searchParams.get("since") ?? undefined;
  const until = searchParams.get("until") ?? undefined;

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

  return { since, until, applyRange };
}
