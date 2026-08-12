import type { NodeRunListItem, Usage } from "../api/types";
import { mergeUsage } from "./usage";

/**
 * The Statistics view's aggregation (task t6, frame claims c13/c36, honesty
 * condition h9/h28): jobs cost/usage totals, and average/median PER RUN,
 * over a listed time window.
 *
 * `GET /v1alpha1/node-runs` (task t11) is the only endpoint that carries
 * per-item usage over an arbitrary window — `GET /v1alpha1/runs` deliberately
 * does not (api/types.ts's `Run.usage` doc). So every function here works
 * from a flat list of `NodeRunListItem`s and groups by `run_id` FIRST,
 * before any average/median is computed — the plan's acceptance wording is
 * explicit that the denominator and the central-tendency stats are per RUN,
 * not per node run. mergeUsage (domain/usage.ts, task t5) does the actual
 * summation so this module never re-implements the "never sum across
 * currencies" rule (h27/c35) — it only reuses it.
 */

/** run_id -> that run's own §13.2 usage rollup, merged from every node run
 * belonging to it in the listed window. */
export type RunUsageMap = Record<string, Usage>;

export function groupUsageByRun(items: NodeRunListItem[]): RunUsageMap {
  const byRun = new Map<string, Usage[]>();
  for (const item of items) {
    const list = byRun.get(item.run_id);
    if (list) list.push(item.usage);
    else byRun.set(item.run_id, [item.usage]);
  }
  const result: RunUsageMap = {};
  for (const [runId, usages] of byRun) {
    result[runId] = mergeUsage(usages);
  }
  return result;
}

function mean(values: number[]): number | null {
  if (values.length === 0) return null;
  return values.reduce((sum, value) => sum + value, 0) / values.length;
}

function median(values: number[]): number | null {
  if (values.length === 0) return null;
  const sorted = [...values].sort((a, b) => a - b);
  const mid = Math.floor(sorted.length / 2);
  return sorted.length % 2 !== 0
    ? sorted[mid]
    : (sorted[mid - 1] + sorted[mid]) / 2;
}

/**
 * A cost currency is only usable for average/median when `mergeUsage`
 * collapsed every reporting run's cost to one `cost`/`currency` pair — the
 * same signal domain/usage.ts's `mergeUsage` already computes (its
 * `cost_by_currency` array is set instead whenever more than one currency
 * was seen). Reusing that signal means this module has no code path of its
 * own that could derive or invent a currency (frame claim c35).
 */
function costsForSingleCurrency(usages: Usage[]): number[] {
  return usages
    .filter((usage) => usage.cost !== undefined)
    .map((usage) => usage.cost as number);
}

export interface RunStats {
  /** Every distinct run_id present in the listed node-run window. */
  totalRuns: number;
  /** Runs with at least one reported attempt (`attempts_reported > 0`). */
  reportedRuns: number;
  /** Runs whose merged usage is entirely unreported — never folded in as a zero (h9/h28). */
  excludedRuns: number;
  /** The §13.2 rollup merged across every reporting run. */
  usage: Usage;
  avgInputTokens: number | null;
  medianInputTokens: number | null;
  avgOutputTokens: number | null;
  medianOutputTokens: number | null;
  /** The single currency covering every cost-reporting run, or `null` when
   * costs were reported in more than one currency (no average is computed
   * then) or none were reported at all. An empty string is a valid,
   * distinct case: cost was reported with no currency at all. */
  costCurrency: string | null;
  avgCost: number | null;
  medianCost: number | null;
}

export function computeRunStats(runUsage: RunUsageMap): RunStats {
  const allUsages = Object.values(runUsage);
  const reported = allUsages.filter((usage) => usage.attempts_reported > 0);
  const overall = mergeUsage(allUsages);

  const inputTokens = reported.map((usage) => usage.input_tokens);
  const outputTokens = reported.map((usage) => usage.output_tokens);

  let costCurrency: string | null = null;
  let avgCost: number | null = null;
  let medianCost: number | null = null;
  if (overall.cost !== undefined) {
    costCurrency = overall.currency ?? "";
    const costs = costsForSingleCurrency(reported);
    avgCost = mean(costs);
    medianCost = median(costs);
  }

  return {
    totalRuns: allUsages.length,
    reportedRuns: reported.length,
    excludedRuns: allUsages.length - reported.length,
    usage: overall,
    avgInputTokens: mean(inputTokens),
    medianInputTokens: median(inputTokens),
    avgOutputTokens: mean(outputTokens),
    medianOutputTokens: median(outputTokens),
    costCurrency,
    avgCost,
    medianCost,
  };
}

export interface CategoryStats {
  /** The run's flat category tag, or `""` for the uncategorized bucket. */
  category: string;
  /** Display label — `"Uncategorized"` for the `""` bucket. */
  label: string;
  totalRuns: number;
  reportedRuns: number;
  excludedRuns: number;
  usage: Usage;
  avgInputTokens: number | null;
  avgOutputTokens: number | null;
  costCurrency: string | null;
  avgCost: number | null;
}

export const UNCATEGORIZED_LABEL = "Uncategorized";

/**
 * `categoryByRun` is a best-effort join (task t5's pattern, same as
 * JobsTable's `runsById`): `GET /v1alpha1/node-runs` carries no category of
 * its own, only `run_id`, so a run this map has no entry for lands in the
 * uncategorized bucket honestly — the view does not know its category,
 * which is the same fact "no category was set" reports.
 */
export function computeCategoryStats(
  runUsage: RunUsageMap,
  categoryByRun: Record<string, string | undefined>,
): CategoryStats[] {
  const runIdsByCategory = new Map<string, string[]>();
  for (const runId of Object.keys(runUsage)) {
    const category = categoryByRun[runId] ?? "";
    const list = runIdsByCategory.get(category);
    if (list) list.push(runId);
    else runIdsByCategory.set(category, [runId]);
  }

  const rows: CategoryStats[] = [];
  for (const [category, runIds] of runIdsByCategory) {
    const usages = runIds.map((runId) => runUsage[runId]);
    const reported = usages.filter((usage) => usage.attempts_reported > 0);
    const overall = mergeUsage(usages);

    let costCurrency: string | null = null;
    let avgCost: number | null = null;
    if (overall.cost !== undefined) {
      costCurrency = overall.currency ?? "";
      avgCost = mean(costsForSingleCurrency(reported));
    }

    rows.push({
      category,
      label: category || UNCATEGORIZED_LABEL,
      totalRuns: runIds.length,
      reportedRuns: reported.length,
      excludedRuns: runIds.length - reported.length,
      usage: overall,
      avgInputTokens: mean(reported.map((usage) => usage.input_tokens)),
      avgOutputTokens: mean(reported.map((usage) => usage.output_tokens)),
      costCurrency,
      avgCost,
    });
  }

  // Named categories alphabetically, uncategorized always last — the same
  // "labeled bucket, not a footnote" treatment the acceptance criteria asks
  // for applies to ordering too: it is a real row, just always the last one.
  rows.sort((a, b) => {
    if (a.category === "" && b.category === "") return 0;
    if (a.category === "") return 1;
    if (b.category === "") return -1;
    return a.category.localeCompare(b.category);
  });
  return rows;
}
