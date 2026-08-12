import { describe, expect, it } from "vitest";
import type { NodeRunListItem, Usage } from "../api/types";
import {
  computeCategoryStats,
  computeRunStats,
  groupUsageByRun,
  UNCATEGORIZED_LABEL,
} from "./stats";

const NOT_REPORTED: Usage = {
  input_tokens: 0,
  output_tokens: 0,
  attempts_reported: 0,
  attempts_not_reported: 1,
};

function reported(
  input: number,
  output: number,
  cost?: number,
  currency = "USD",
): Usage {
  return cost === undefined
    ? { input_tokens: input, output_tokens: output, attempts_reported: 1, attempts_not_reported: 0 }
    : {
        input_tokens: input,
        output_tokens: output,
        cost,
        currency,
        attempts_reported: 1,
        attempts_not_reported: 0,
      };
}

function nodeRun(
  id: string,
  runId: string,
  usage: Usage,
): NodeRunListItem {
  return {
    id,
    run_id: runId,
    node_id: "node",
    state: "completed",
    created_at: "2026-08-10T10:00:00Z",
    updated_at: "2026-08-10T10:05:00Z",
    usage,
  };
}

describe("groupUsageByRun", () => {
  it("merges multiple node runs of the same run into one usage rollup", () => {
    const items = [
      nodeRun("nr-1", "run-a", reported(400, 200, 0.8)),
      nodeRun("nr-2", "run-a", reported(600, 300, 1.2)),
      nodeRun("nr-3", "run-b", reported(2000, 1000, 4)),
    ];
    const byRun = groupUsageByRun(items);
    expect(Object.keys(byRun).sort()).toEqual(["run-a", "run-b"]);
    expect(byRun["run-a"]).toMatchObject({
      input_tokens: 1000,
      output_tokens: 500,
      cost: 2,
      currency: "USD",
    });
    expect(byRun["run-b"]).toMatchObject({
      input_tokens: 2000,
      output_tokens: 1000,
      cost: 4,
      currency: "USD",
    });
  });

  it("keeps a run with only not-reported node runs distinctly at attempts_reported 0", () => {
    const items = [nodeRun("nr-1", "run-c", NOT_REPORTED)];
    const byRun = groupUsageByRun(items);
    expect(byRun["run-c"].attempts_reported).toBe(0);
  });
});

describe("computeRunStats", () => {
  // run-a: 1000/500 tokens, $2 | run-b: 2000/1000, $4 | run-c: 3000/1500, $6
  // | run-d: 1500/750, $3 | run-e: not reported at all.
  const runUsage = {
    "run-a": reported(1000, 500, 2),
    "run-b": reported(2000, 1000, 4),
    "run-c": reported(3000, 1500, 6),
    "run-d": reported(1500, 750, 3),
    "run-e": NOT_REPORTED,
  };

  it("states the denominator: total, reported, and excluded runs, never folding excluded in as zero", () => {
    const stats = computeRunStats(runUsage);
    expect(stats.totalRuns).toBe(5);
    expect(stats.reportedRuns).toBe(4);
    expect(stats.excludedRuns).toBe(1);
  });

  it("computes average and median tokens per run, over reporting runs only", () => {
    const stats = computeRunStats(runUsage);
    expect(stats.avgInputTokens).toBe(1875); // (1000+2000+3000+1500)/4
    expect(stats.medianInputTokens).toBe(1750); // sorted [1000,1500,2000,3000] -> (1500+2000)/2
    expect(stats.avgOutputTokens).toBe(937.5);
    expect(stats.medianOutputTokens).toBe(875);
  });

  it("computes average and median cost when a single currency covers every reporting run", () => {
    const stats = computeRunStats(runUsage);
    expect(stats.costCurrency).toBe("USD");
    expect(stats.avgCost).toBe(3.75); // (2+4+6+3)/4
    expect(stats.medianCost).toBe(3.5); // sorted [2,3,4,6] -> (3+4)/2
    expect(stats.usage.cost).toBe(15);
    expect(stats.usage.currency).toBe("USD");
  });

  it("reports totalRuns 0 and every stat null/empty for an empty window, never NaN", () => {
    const stats = computeRunStats({});
    expect(stats.totalRuns).toBe(0);
    expect(stats.reportedRuns).toBe(0);
    expect(stats.excludedRuns).toBe(0);
    expect(stats.avgInputTokens).toBeNull();
    expect(stats.medianInputTokens).toBeNull();
    expect(stats.costCurrency).toBeNull();
    expect(stats.avgCost).toBeNull();
  });

  it("never computes an average/median cost when more than one currency was reported (no code path derives currency from tokens)", () => {
    const mixed = {
      "run-x": reported(100, 50, 1, "USD"),
      "run-y": reported(100, 50, 1, "EUR"),
    };
    const stats = computeRunStats(mixed);
    expect(stats.costCurrency).toBeNull();
    expect(stats.avgCost).toBeNull();
    expect(stats.medianCost).toBeNull();
    // Total cost per currency is still available, just never summed together.
    expect(stats.usage.cost_by_currency).toEqual(
      expect.arrayContaining([
        { currency: "USD", cost: 1 },
        { currency: "EUR", cost: 1 },
      ]),
    );
    // Token stats are unaffected by the currency split.
    expect(stats.avgInputTokens).toBe(100);
  });

  it("a run with entirely unreported usage never contributes a zero to the token average", () => {
    const withZeroLikeExcluded = {
      "run-a": reported(1000, 500, 2),
      "run-e": NOT_REPORTED,
    };
    const stats = computeRunStats(withZeroLikeExcluded);
    // If the excluded run were folded in as 0 the average would be 500, not 1000.
    expect(stats.avgInputTokens).toBe(1000);
  });
});

describe("computeCategoryStats", () => {
  const runUsage = {
    "run-a": reported(1000, 500, 2),
    "run-b": reported(2000, 1000, 4),
    "run-c": reported(3000, 1500, 6),
    "run-d": reported(1500, 750, 3),
    "run-e": NOT_REPORTED,
  };
  const categoryByRun: Record<string, string | undefined> = {
    "run-a": "ci",
    "run-b": "ci",
    "run-c": "review",
    "run-e": "ci",
    // run-d intentionally absent -> uncategorized bucket
  };

  it("buckets by category, with a distinct labeled uncategorized bucket sorted last", () => {
    const rows = computeCategoryStats(runUsage, categoryByRun);
    expect(rows.map((row) => row.category)).toEqual(["ci", "review", ""]);
    const uncategorized = rows.find((row) => row.category === "");
    expect(uncategorized?.label).toBe(UNCATEGORIZED_LABEL);
    expect(uncategorized?.totalRuns).toBe(1);
  });

  it("carries the same denominator and total/average rules into each category row", () => {
    const rows = computeCategoryStats(runUsage, categoryByRun);
    const ci = rows.find((row) => row.category === "ci")!;
    expect(ci.totalRuns).toBe(3);
    expect(ci.reportedRuns).toBe(2);
    expect(ci.excludedRuns).toBe(1);
    expect(ci.usage.input_tokens).toBe(3000); // 1000 + 2000, run-e excluded
    expect(ci.avgInputTokens).toBe(1500);
    expect(ci.costCurrency).toBe("USD");
    expect(ci.avgCost).toBe(3);

    const review = rows.find((row) => row.category === "review")!;
    expect(review.totalRuns).toBe(1);
    expect(review.reportedRuns).toBe(1);
    expect(review.usage.input_tokens).toBe(3000);
    expect(review.avgInputTokens).toBe(3000);
  });
});
