import { describe, expect, it } from "vitest";
import type { Usage } from "../api/types";
import {
  formatCost,
  formatCostByCurrency,
  formatTokenCount,
  formatUsageTokens,
  mergeUsage,
  runDisplayName,
} from "./usage";

describe("formatTokenCount", () => {
  it("renders small counts verbatim", () => {
    expect(formatTokenCount(0)).toBe("0");
    expect(formatTokenCount(999)).toBe("999");
  });

  it("renders thousands with one decimal and a k suffix", () => {
    expect(formatTokenCount(12345)).toBe("12.3k");
    expect(formatTokenCount(4100)).toBe("4.1k");
    expect(formatTokenCount(1000)).toBe("1k");
  });

  it("renders millions with an m suffix", () => {
    expect(formatTokenCount(2_500_000)).toBe("2.5m");
  });
});

describe("formatUsageTokens", () => {
  it("formats input/output as the primary, token-first figure", () => {
    const usage: Usage = {
      input_tokens: 12300,
      output_tokens: 4100,
      attempts_reported: 3,
      attempts_not_reported: 0,
    };
    expect(formatUsageTokens(usage)).toBe("12.3k in / 4.1k out");
  });
});

describe("formatCost / formatCostByCurrency", () => {
  it("never invents a currency the caller did not pass", () => {
    // honesty condition h27: no currency reported means no currency shown.
    expect(formatCost(12.3)).toBe("12.30");
    expect(formatCost(12.3)).not.toContain("$");
    expect(formatCost(12.3)).not.toMatch(/[A-Z]{3}/);
  });

  it("appends the currency only when one was actually reported", () => {
    expect(formatCost(12.3, "USD")).toBe("12.30 USD");
  });

  it("formats every cost_by_currency entry independently, never summed", () => {
    expect(
      formatCostByCurrency([
        { currency: "USD", cost: 12.3 },
        { currency: "EUR", cost: 3 },
      ]),
    ).toEqual(["12.30 USD", "3.00 EUR"]);
  });
});

describe("mergeUsage", () => {
  it("sums tokens and attempt counts across entries", () => {
    const merged = mergeUsage([
      { input_tokens: 100, output_tokens: 50, attempts_reported: 1, attempts_not_reported: 0 },
      { input_tokens: 200, output_tokens: 75, attempts_reported: 1, attempts_not_reported: 1 },
    ]);
    expect(merged.input_tokens).toBe(300);
    expect(merged.output_tokens).toBe(125);
    expect(merged.attempts_reported).toBe(2);
    expect(merged.attempts_not_reported).toBe(1);
  });

  it("collapses to a single cost/currency when every entry agreed", () => {
    const merged = mergeUsage([
      { input_tokens: 1, output_tokens: 1, cost: 1, currency: "USD", attempts_reported: 1, attempts_not_reported: 0 },
      { input_tokens: 1, output_tokens: 1, cost: 2, currency: "USD", attempts_reported: 1, attempts_not_reported: 0 },
    ]);
    expect(merged.cost).toBe(3);
    expect(merged.currency).toBe("USD");
    expect(merged.cost_by_currency).toBeUndefined();
  });

  it("falls back to cost_by_currency when entries disagree on currency", () => {
    const merged = mergeUsage([
      { input_tokens: 1, output_tokens: 1, cost: 1, currency: "USD", attempts_reported: 1, attempts_not_reported: 0 },
      { input_tokens: 1, output_tokens: 1, cost: 2, currency: "EUR", attempts_reported: 1, attempts_not_reported: 0 },
    ]);
    expect(merged.cost).toBeUndefined();
    expect(merged.cost_by_currency).toEqual(
      expect.arrayContaining([
        { currency: "USD", cost: 1 },
        { currency: "EUR", cost: 2 },
      ]),
    );
  });

  it("never fabricates a cost when no entry reported one", () => {
    const merged = mergeUsage([
      { input_tokens: 1, output_tokens: 1, attempts_reported: 1, attempts_not_reported: 0 },
    ]);
    expect(merged.cost).toBeUndefined();
    expect(merged.cost_by_currency).toBeUndefined();
  });

  it("returns a present-but-empty rollup for an empty entry list", () => {
    const merged = mergeUsage([]);
    expect(merged).toEqual({
      input_tokens: 0,
      output_tokens: 0,
      attempts_reported: 0,
      attempts_not_reported: 0,
    });
  });
});

describe("runDisplayName", () => {
  it("prefers the operator-given name", () => {
    expect(
      runDisplayName({ id: "run-1", name: "nightly build", display_hint: "hint" }),
    ).toEqual({ text: "nightly build", derived: false });
  });

  it("falls back to display_hint, marked as derived", () => {
    expect(
      runDisplayName({ id: "run-1", display_hint: "add the ledger endpoint" }),
    ).toEqual({ text: "add the ledger endpoint", derived: true });
  });

  it("falls back to the run id when neither is present, never marked as derived", () => {
    expect(runDisplayName({ id: "run-1" })).toEqual({
      text: "run-1",
      derived: false,
    });
  });
});
