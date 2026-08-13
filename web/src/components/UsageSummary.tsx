import type { Usage } from "../api/types";
import {
  formatCost,
  formatCostByCurrency,
  formatUsageTokens,
} from "../domain/usage";

export interface UsageSummaryProps {
  usage: Usage;
  /** A tighter one-line rendering for table cells (JobsTable). */
  compact?: boolean;
  id?: string;
}

/**
 * The one place every view (RunView, NodeDetailPanel, JobsTable) renders a
 * §13.2 usage/cost rollup — token totals as the primary figure, currency
 * cost as secondary detail only when the API actually reported one, and an
 * explicit "not reported" state instead of ever presenting absent usage as
 * a zero (task t5, honesty condition h27 / frame claim c35: "no code path
 * derives currency from tokens" — this component never invents a currency
 * the `Usage` object did not carry).
 *
 * `data-usage-reported` is the stable hook a test (or an agent) uses to
 * distinguish "genuinely zero" from "nothing was reported at all" without
 * parsing rendered text. `data-attempts-reported` / `data-attempts-not-reported`
 * (task t11) carry the same two counts explicitly, on both branches below,
 * so per-attempt cost coverage is machine-readable even when the visible
 * copy stays terse (e.g. the "usage not reported" branch, whose wording is
 * asserted verbatim elsewhere and is not touched here).
 */
export function UsageSummary({ usage, compact = false, id }: UsageSummaryProps) {
  if (usage.attempts_reported === 0) {
    return (
      <span
        id={id}
        className="usage-summary usage-summary--not-reported"
        data-usage-reported="false"
        data-attempts-reported={usage.attempts_reported}
        data-attempts-not-reported={usage.attempts_not_reported}
      >
        {compact ? "not reported" : "usage not reported"}
      </span>
    );
  }

  const costLines = usage.cost_by_currency
    ? formatCostByCurrency(usage.cost_by_currency)
    : usage.cost !== undefined
      ? [formatCost(usage.cost, usage.currency)]
      : [];

  return (
    <span
      id={id}
      className={`usage-summary${compact ? " usage-summary--compact" : ""}`}
      data-usage-reported="true"
      data-attempts-reported={usage.attempts_reported}
      data-attempts-not-reported={usage.attempts_not_reported}
    >
      <span className="usage-summary__tokens">{formatUsageTokens(usage)}</span>
      {costLines.length > 0 ? (
        <span className="usage-summary__cost">{costLines.join(", ")}</span>
      ) : null}
      {/* Explicit attempt-coverage line (task t11 acceptance #2): how many
          attempts contributed the figures above, spelled out rather than
          left implicit — additive to the `__partial` not-reported note
          below, never a replacement for its exact wording. */}
      {!compact ? (
        <span className="usage-summary__attempts muted">
          {usage.attempts_reported} attempt
          {usage.attempts_reported === 1 ? "" : "s"} reported
        </span>
      ) : null}
      {!compact && usage.attempts_not_reported > 0 ? (
        <span className="usage-summary__partial muted">
          ({usage.attempts_not_reported} attempt
          {usage.attempts_not_reported === 1 ? "" : "s"} not reported)
        </span>
      ) : null}
    </span>
  );
}

export default UsageSummary;
