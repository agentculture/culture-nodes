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
 * parsing rendered text.
 */
export function UsageSummary({ usage, compact = false, id }: UsageSummaryProps) {
  if (usage.attempts_reported === 0) {
    return (
      <span
        id={id}
        className="usage-summary usage-summary--not-reported"
        data-usage-reported="false"
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
    >
      <span className="usage-summary__tokens">{formatUsageTokens(usage)}</span>
      {costLines.length > 0 ? (
        <span className="usage-summary__cost">{costLines.join(", ")}</span>
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
