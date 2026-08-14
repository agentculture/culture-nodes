import type { CurrencyCost, Run, Usage } from "../api/types";

/**
 * Token-first cost rendering (task t5, honesty condition h27 / frame claim
 * c35): every helper here treats token counts as the primary figure and
 * currency cost as secondary, reported-only detail. None of them ever
 * invents a currency the API did not send — there is no default currency,
 * no symbol substitution, no pricing table. If the API sent no `currency`,
 * nothing here prints one.
 */

/** `12345` -> `"12.3k"`, `1234567` -> `"1.2m"`, anything under 1000 verbatim. */
export function formatTokenCount(count: number): string {
  const abs = Math.abs(count);
  if (abs < 1000) return String(count);
  if (abs < 1_000_000) {
    return `${trimTrailingZero((count / 1000).toFixed(1))}k`;
  }
  return `${trimTrailingZero((count / 1_000_000).toFixed(1))}m`;
}

function trimTrailingZero(value: string): string {
  return value.endsWith(".0") ? value.slice(0, -2) : value;
}

/** `"12.3k in / 4.1k out"` — the primary, always-safe-to-show figure. */
export function formatUsageTokens(usage: Usage): string {
  return `${formatTokenCount(usage.input_tokens)} in / ${formatTokenCount(usage.output_tokens)} out`;
}

/**
 * `cached_input_tokens / input_tokens`, computed only when `input_tokens >
 * 0` (task t2, ADR 0009) — mirrors the server's `usageOut` exactly: never a
 * fabricated 0/0 ratio when nothing in scope reported any input tokens.
 * `undefined` in that case, matching `Usage.cache_ratio`'s own optionality
 * on the wire (this is the client-side computation `mergeUsage` uses so a
 * merged rollup's ratio is recomputed from its own summed fields rather
 * than averaged from its inputs' individual ratios, which would be wrong).
 */
export function cacheRatio(usage: Pick<Usage, "input_tokens" | "cached_input_tokens">): number | undefined {
  return usage.input_tokens > 0 ? usage.cached_input_tokens / usage.input_tokens : undefined;
}

/** `"13.3% cached"` — the cache-ratio stat tile's rendering, honest about absence. */
export function formatCacheRatio(ratio: number): string {
  return `${(ratio * 100).toFixed(1)}% cached`;
}

/**
 * A single reported cost as secondary text — never a symbol invented for a
 * currency the API did not send (h27). `"12.34"` when no currency was
 * reported alongside the cost, `"12.34 USD"` when one was.
 */
export function formatCost(cost: number, currency?: string): string {
  const amount = cost.toFixed(2);
  return currency ? `${amount} ${currency}` : amount;
}

/** One line per `CurrencyCost` entry, in the order the API listed them. */
export function formatCostByCurrency(entries: CurrencyCost[]): string[] {
  return entries.map((entry) => formatCost(entry.cost, entry.currency));
}

/**
 * Merge several node-run-level Usage rollups (e.g. every visit of a looped
 * node) into one, client-side, using the same rules the server's §13.2
 * aggregation uses: sum tokens and attempt counts; sum cost per currency
 * actually seen, and only collapse to a single `cost`/`currency` pair when
 * every contributing entry agreed on one currency (an entry with no
 * currency at all merges under the empty-string bucket, its own group).
 *
 * `cached_input_tokens`/`reasoning_tokens` (task t2, ADR 0009) sum exactly
 * the way `input_tokens`/`output_tokens` do — plain addition, no second
 * "how many entries reported cache telemetry" tracking, mirroring the
 * server rollup's own choice not to invent a second sentinel (an entry
 * whose cache fields are honestly 0 because its backend reports none
 * contributes 0, indistinguishable from a true 0% cache turn — the same
 * ambiguity the server-side sum accepts). `cache_ratio` is NOT summed or
 * averaged from the inputs' own ratios (that would be wrong — a weighted
 * quantity is not a mean of ratios); it is recomputed from the merged
 * sums via `cacheRatio`, honoring the same input_tokens > 0 gate.
 */
export function mergeUsage(entries: Usage[]): Usage {
  let inputTokens = 0;
  let outputTokens = 0;
  let cachedInputTokens = 0;
  let reasoningTokens = 0;
  let attemptsReported = 0;
  let attemptsNotReported = 0;
  const costByCurrency = new Map<string, number>();

  for (const usage of entries) {
    inputTokens += usage.input_tokens;
    outputTokens += usage.output_tokens;
    cachedInputTokens += usage.cached_input_tokens;
    reasoningTokens += usage.reasoning_tokens;
    attemptsReported += usage.attempts_reported;
    attemptsNotReported += usage.attempts_not_reported;

    if (usage.cost_by_currency) {
      for (const entry of usage.cost_by_currency) {
        const key = entry.currency ?? "";
        costByCurrency.set(key, (costByCurrency.get(key) ?? 0) + entry.cost);
      }
    } else if (usage.cost !== undefined) {
      const key = usage.currency ?? "";
      costByCurrency.set(key, (costByCurrency.get(key) ?? 0) + usage.cost);
    }
  }

  const merged: Usage = {
    input_tokens: inputTokens,
    output_tokens: outputTokens,
    cached_input_tokens: cachedInputTokens,
    reasoning_tokens: reasoningTokens,
    attempts_reported: attemptsReported,
    attempts_not_reported: attemptsNotReported,
  };
  const ratio = cacheRatio(merged);
  if (ratio !== undefined) merged.cache_ratio = ratio;

  if (costByCurrency.size === 1) {
    const [[currency, cost]] = costByCurrency.entries();
    merged.cost = cost;
    if (currency) merged.currency = currency;
  } else if (costByCurrency.size > 1) {
    merged.cost_by_currency = [...costByCurrency.entries()].map(
      ([currency, cost]) => (currency ? { currency, cost } : { cost }),
    );
  }

  return merged;
}

export interface RunDisplayName {
  /** The text to render as the run's title. */
  text: string;
  /**
   * `true` only for a `display_hint` — a derived guess, never something an
   * operator actually said. A renderer MUST mark this visibly distinct from
   * a given `name` (muted style + a title attribute, per task t5) and never
   * present it as if it were the given name.
   */
  derived: boolean;
}

/**
 * `name` when the operator gave one, else `display_hint` marked as derived,
 * else the run id — task t5's fallback chain, in one place so every view
 * (RunsList, RunCard, JobsTable, RunView) resolves it identically.
 */
export function runDisplayName(
  run: Pick<Run, "id" | "name" | "display_hint">,
): RunDisplayName {
  if (run.name) return { text: run.name, derived: false };
  if (run.display_hint) return { text: run.display_hint, derived: true };
  return { text: run.id, derived: false };
}
