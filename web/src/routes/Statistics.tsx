import { useCallback, useEffect, useRef, useState } from "react";
import { setAgentState } from "../agent-state/store";
import { ApiError, listNodeRuns, listRuns } from "../api/client";
import type { NodeRunListItem, Run } from "../api/types";
import CategoryChip from "../components/CategoryChip";
import ErrorNotice from "../components/ErrorNotice";
import TimeRangeFilter from "../components/TimeRangeFilter";
import UsageSummary from "../components/UsageSummary";
import { computeCategoryStats, computeRunStats, groupUsageByRun } from "../domain/stats";
import { formatCacheRatio, formatCost, formatTokenCount } from "../domain/usage";
import { useSharedEvents, type SharedEventType } from "../hooks/useSharedEvents";
import { useTimeRange } from "../hooks/useTimeRange";

/**
 * The events that can move usage totals: an attempt reporting usage on
 * completion, or on failure (issue #46/c2's fabricated-zeros fix records a
 * failed attempt's real usage too, once it ships). A stable module-level
 * reference, as useSharedEvents requires.
 */
const STATISTICS_EVENT_TYPES = [
  "dev.culture.nodes.attempt.completed",
  "dev.culture.nodes.node-run.failed",
] as const satisfies readonly SharedEventType[];

/** Mirrors the Mesh view's attribution-refresh discipline (Mesh.tsx). */
const REFRESH_DEBOUNCE_MS = 4000;

/**
 * The Statistics view (task t6): jobs cost/usage totals, and average/median
 * PER RUN, over the same server-side time-range idiom Board/Runs/Jobs use
 * (useTimeRange + TimeRangeFilter -> `updated_since`/`updated_until` on the
 * request, never a client-side filter of an already-fetched list).
 *
 * `GET /v1alpha1/runs` deliberately carries no per-item usage (api/types.ts's
 * `Run.usage` doc), so this view reads `GET /v1alpha1/node-runs` (task t11)
 * instead — the same endpoint JobsTimeline reads — and, unlike
 * JobsTimeline's "Load more" pagination, walks every page for the window
 * before computing a total: a statistic computed from only the first page
 * would silently under-report whenever a window has more node runs than one
 * page holds.
 *
 * Frame honesty condition h9/c36: the view always states how many runs are
 * in the window, how many reported usage, and how many are excluded — a run
 * whose usage was never reported at all (`attempts_reported: 0` on every
 * node run under it) is counted and shown as excluded, never folded into
 * the average as a zero. All of the actual arithmetic — grouping node-run
 * usage by run_id, the never-sum-across-currencies rule — lives in
 * domain/stats.ts and domain/usage.ts (task t5); this component only wires
 * fetch -> compute -> render.
 */
export function Statistics() {
  const { since, until, applyRange } = useTimeRange();

  const [nodeRuns, setNodeRuns] = useState<NodeRunListItem[] | null>(null);
  const [runsById, setRunsById] = useState<Record<string, Run>>({});
  const [error, setError] = useState<ApiError | null>(null);
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

  useSharedEvents(STATISTICS_EVENT_TYPES, scheduleReload);

  // Walks every page for the window (see the class doc above) — pulled out
  // to a stable callback so both the initial load and the SSE-triggered
  // refresh below share the exact same bounded-pagination walk.
  const fetchAllNodeRuns = useCallback(
    async (signal: AbortSignal): Promise<NodeRunListItem[]> => {
      const items: NodeRunListItem[] = [];
      let cursor: string | undefined;
      // Bounded rather than an unconditional while(true): a pathological
      // server that never stops returning next_cursor cannot hang this view
      // forever. 1000 pages is far past any realistic window.
      for (let page = 0; page < 1000; page++) {
        const result = await listNodeRuns(signal, {
          updated_since: since,
          updated_until: until,
          cursor,
        });
        items.push(...result.items);
        if (!result.next_cursor) break;
        cursor = result.next_cursor;
      }
      return items;
    },
    [since, until],
  );

  useEffect(() => {
    const controller = new AbortController();
    setAgentState({ status: "loading", run: null });
    setError(null);
    setNodeRuns(null);

    const toApiError = (cause: unknown): ApiError =>
      cause instanceof ApiError
        ? cause
        : new ApiError(0, String(cause), "check the browser console");

    fetchAllNodeRuns(controller.signal)
      .then((items) => {
        if (controller.signal.aborted) return;
        setNodeRuns(items);
        setAgentState({ status: "ready", run: null });
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setNodeRuns([]);
        setError(toApiError(cause));
        setAgentState({ status: "ready", run: null });
      });
    return () => controller.abort();
  }, [fetchAllNodeRuns]);

  // The category join (task t5 pattern, same as JobsTimeline): a separate,
  // non-blocking fetch of GET /v1alpha1/runs for the same window. Its
  // failure is swallowed — runs it cannot name just land in the
  // uncategorized bucket, honestly reporting "category unknown" rather than
  // gating the whole view on a second endpoint.
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
  // render (reloadKey === 0, already handled by the effect above). Walks
  // the same bounded pagination and refreshes the category join alongside
  // it — never nulls `nodeRuns`, never touches agent-state.
  useEffect(() => {
    if (reloadKey === 0) return;
    const controller = new AbortController();
    Promise.all([
      fetchAllNodeRuns(controller.signal),
      listRuns(controller.signal, { updated_since: since, updated_until: until }),
    ])
      .then(([items, list]) => {
        if (controller.signal.aborted) return;
        setNodeRuns(items);
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

  const runUsage = nodeRuns !== null ? groupUsageByRun(nodeRuns) : null;
  const stats = runUsage !== null ? computeRunStats(runUsage) : null;
  const categoryByRun: Record<string, string | undefined> = Object.fromEntries(
    Object.entries(runsById).map(([id, run]) => [id, run.category]),
  );
  const categories =
    runUsage !== null ? computeCategoryStats(runUsage, categoryByRun) : null;

  useEffect(() => {
    if (!stats || !categories) return;
    setAgentState({
      statistics: {
        total_runs: stats.totalRuns,
        reported_runs: stats.reportedRuns,
        excluded_runs: stats.excludedRuns,
        total_input_tokens: stats.usage.input_tokens,
        total_output_tokens: stats.usage.output_tokens,
        avg_input_tokens: stats.avgInputTokens,
        median_input_tokens: stats.medianInputTokens,
        avg_output_tokens: stats.avgOutputTokens,
        median_output_tokens: stats.medianOutputTokens,
        cost_currency: stats.costCurrency,
        avg_cost: stats.avgCost,
        median_cost: stats.medianCost,
        category_count: categories.length,
      },
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [stats, categories]);

  return (
    <section className="view-rail statistics-view" id="statistics-view">
      <h1>Statistics</h1>
      <p className="muted">
        Jobs cost and usage totals over the listed window, aggregated per run.
      </p>

      <TimeRangeFilter since={since} until={until} onApply={applyRange} />

      {error ? <ErrorNotice error={error} /> : null}

      {stats === null ? (
        <p className="muted" id="statistics-loading">
          Loading statistics…
        </p>
      ) : stats.totalRuns === 0 ? (
        <p className="muted" id="statistics-empty">
          No node runs in this range.
        </p>
      ) : (
        <>
          <p className="statistics-denominator" id="statistics-denominator">
            <strong data-stat="total-runs">{stats.totalRuns}</strong> run
            {stats.totalRuns === 1 ? "" : "s"} in this window —{" "}
            <span data-stat="reported-runs">{stats.reportedRuns}</span> reported
            usage,{" "}
            <span
              className="statistics-denominator__excluded"
              id="statistics-excluded"
              data-stat="excluded-runs"
            >
              {stats.excludedRuns} excluded (usage never reported)
            </span>
            .
          </p>

          <div className="stat-tiles" id="stat-tiles">
            <div className="stat-tile" id="stat-tile-tokens" data-stat-tile="tokens">
              <span className="stat-tile__label">Total tokens</span>
              <UsageSummary usage={stats.usage} id="stat-total-usage" />
            </div>

            <div className="stat-tile" id="stat-tile-avg-tokens" data-stat-tile="avg-tokens">
              <span className="stat-tile__label">Average tokens / run</span>
              {stats.avgInputTokens === null ? (
                <span className="muted" data-usage-reported="false">
                  no reporting runs
                </span>
              ) : (
                <span className="stat-tile__value" data-stat="avg-tokens">
                  {formatTokenCount(Math.round(stats.avgInputTokens))} in /{" "}
                  {formatTokenCount(Math.round(stats.avgOutputTokens ?? 0))} out
                </span>
              )}
            </div>

            <div
              className="stat-tile"
              id="stat-tile-median-tokens"
              data-stat-tile="median-tokens"
            >
              <span className="stat-tile__label">Median tokens / run</span>
              {stats.medianInputTokens === null ? (
                <span className="muted" data-usage-reported="false">
                  no reporting runs
                </span>
              ) : (
                <span className="stat-tile__value" data-stat="median-tokens">
                  {formatTokenCount(Math.round(stats.medianInputTokens))} in /{" "}
                  {formatTokenCount(Math.round(stats.medianOutputTokens ?? 0))} out
                </span>
              )}
            </div>

            <div className="stat-tile" id="stat-tile-cache-ratio" data-stat-tile="cache-ratio">
              <span className="stat-tile__label">Cache hit rate</span>
              {stats.usage.cache_ratio === undefined ? (
                <span className="muted" data-usage-reported="false">
                  not computable (no input tokens reported)
                </span>
              ) : (
                <span className="stat-tile__value" data-stat="cache-ratio">
                  {formatCacheRatio(stats.usage.cache_ratio)}
                </span>
              )}
            </div>

            <div className="stat-tile" id="stat-tile-avg-cost" data-stat-tile="avg-cost">
              <span className="stat-tile__label">Average / median cost / run</span>
              {stats.avgCost === null ? (
                <span className="muted" data-usage-reported="false">
                  {stats.usage.cost_by_currency
                    ? "mixed currencies — no single average"
                    : "not reported"}
                </span>
              ) : (
                <span className="stat-tile__value" data-stat="avg-cost">
                  {formatCost(stats.avgCost, stats.costCurrency || undefined)} avg /{" "}
                  {formatCost(stats.medianCost ?? 0, stats.costCurrency || undefined)} median
                </span>
              )}
            </div>
          </div>

          <h2>By category</h2>
          <div className="table-scroll">
            <table
              className="ledger-table category-stats-table"
              id="category-stats-table"
            >
              <caption>
                {categories?.length ?? 0} categor
                {(categories?.length ?? 0) === 1 ? "y" : "ies"} (uncategorized
                runs bucketed separately, not dropped)
              </caption>
              <thead>
                <tr>
                  <th scope="col">category</th>
                  <th scope="col">runs</th>
                  <th scope="col">reported</th>
                  <th scope="col">excluded</th>
                  <th scope="col">total usage</th>
                  <th scope="col">avg tokens / run</th>
                  <th scope="col">avg cost / run</th>
                </tr>
              </thead>
              <tbody>
                {categories?.map((row) => (
                  <tr
                    key={row.category || "uncategorized"}
                    data-category={row.category || "uncategorized"}
                  >
                    <th scope="row">
                      {row.category ? (
                        <CategoryChip category={row.category} />
                      ) : (
                        <span
                          className="category-chip category-chip--uncategorized"
                          data-category="uncategorized"
                        >
                          Uncategorized
                        </span>
                      )}
                    </th>
                    <td data-stat="category-total-runs">{row.totalRuns}</td>
                    <td data-stat="category-reported-runs">{row.reportedRuns}</td>
                    <td data-stat="category-excluded-runs">{row.excludedRuns}</td>
                    <td>
                      <UsageSummary usage={row.usage} compact />
                    </td>
                    <td data-stat="category-avg-tokens">
                      {row.avgInputTokens === null ? (
                        <span className="muted">—</span>
                      ) : (
                        `${formatTokenCount(Math.round(row.avgInputTokens))} in / ${formatTokenCount(
                          Math.round(row.avgOutputTokens ?? 0),
                        )} out`
                      )}
                    </td>
                    <td data-stat="category-avg-cost">
                      {row.avgCost === null ? (
                        <span className="muted">—</span>
                      ) : (
                        formatCost(row.avgCost, row.costCurrency || undefined)
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </section>
  );
}

export default Statistics;
