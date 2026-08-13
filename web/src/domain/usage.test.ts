import { describe, expect, it } from "vitest";
import type { Usage } from "../api/types";
import {
  cacheRatio,
  formatCacheRatio,
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
      cached_input_tokens: 0,
      reasoning_tokens: 0,
      attempts_reported: 3,
      attempts_not_reported: 0,
    };
    expect(formatUsageTokens(usage)).toBe("12.3k in / 4.1k out");
  });
});

describe("cacheRatio / formatCacheRatio", () => {
  it("computes cached/input only when input_tokens > 0", () => {
    expect(cacheRatio({ input_tokens: 200, cached_input_tokens: 50 })).toBe(0.25);
  });

  it("never fabricates a 0/0 ratio when input_tokens is 0", () => {
    expect(cacheRatio({ input_tokens: 0, cached_input_tokens: 0 })).toBeUndefined();
  });

  it("formats a ratio as a percentage with the word 'cached'", () => {
    expect(formatCacheRatio(0.1333)).toBe("13.3% cached");
    expect(formatCacheRatio(0)).toBe("0.0% cached");
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
      { input_tokens: 100, output_tokens: 50, cached_input_tokens: 0, reasoning_tokens: 0, attempts_reported: 1, attempts_not_reported: 0 },
      { input_tokens: 200, output_tokens: 75, cached_input_tokens: 0, reasoning_tokens: 0, attempts_reported: 1, attempts_not_reported: 1 },
    ]);
    expect(merged.input_tokens).toBe(300);
    expect(merged.output_tokens).toBe(125);
    expect(merged.attempts_reported).toBe(2);
    expect(merged.attempts_not_reported).toBe(1);
  });

  it("collapses to a single cost/currency when every entry agreed", () => {
    const merged = mergeUsage([
      { input_tokens: 1, output_tokens: 1, cached_input_tokens: 0, reasoning_tokens: 0, cost: 1, currency: "USD", attempts_reported: 1, attempts_not_reported: 0 },
      { input_tokens: 1, output_tokens: 1, cached_input_tokens: 0, reasoning_tokens: 0, cost: 2, currency: "USD", attempts_reported: 1, attempts_not_reported: 0 },
    ]);
    expect(merged.cost).toBe(3);
    expect(merged.currency).toBe("USD");
    expect(merged.cost_by_currency).toBeUndefined();
  });

  it("falls back to cost_by_currency when entries disagree on currency", () => {
    const merged = mergeUsage([
      { input_tokens: 1, output_tokens: 1, cached_input_tokens: 0, reasoning_tokens: 0, cost: 1, currency: "USD", attempts_reported: 1, attempts_not_reported: 0 },
      { input_tokens: 1, output_tokens: 1, cached_input_tokens: 0, reasoning_tokens: 0, cost: 2, currency: "EUR", attempts_reported: 1, attempts_not_reported: 0 },
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
      { input_tokens: 1, output_tokens: 1, cached_input_tokens: 0, reasoning_tokens: 0, attempts_reported: 1, attempts_not_reported: 0 },
    ]);
    expect(merged.cost).toBeUndefined();
    expect(merged.cost_by_currency).toBeUndefined();
  });

  it("returns a present-but-empty rollup for an empty entry list", () => {
    const merged = mergeUsage([]);
    expect(merged).toEqual({
      input_tokens: 0,
      output_tokens: 0,
      cached_input_tokens: 0,
      reasoning_tokens: 0,
      attempts_reported: 0,
      attempts_not_reported: 0,
    });
    // input_tokens is 0, so cache_ratio must never be a fabricated 0/0 —
    // it must be entirely absent from the merged object.
    expect(merged.cache_ratio).toBeUndefined();
    expect("cache_ratio" in merged).toBe(false);
  });

  // task t2/ADR 0009: the client-side mirror of the server's rollup rules
  // for the extended usage fields.
  it("sums cached_input_tokens and reasoning_tokens across entries, mirroring the server rollup", () => {
    const merged = mergeUsage([
      { input_tokens: 400, output_tokens: 200, cached_input_tokens: 200, reasoning_tokens: 50, attempts_reported: 1, attempts_not_reported: 0 },
      { input_tokens: 2000, output_tokens: 1000, cached_input_tokens: 800, reasoning_tokens: 100, attempts_reported: 1, attempts_not_reported: 0 },
    ]);
    expect(merged.cached_input_tokens).toBe(1000);
    expect(merged.reasoning_tokens).toBe(150);
  });

  it("an entry with tokens but no cache telemetry at all contributes nothing to the cached sum, never a fabricated zero standing in for 'unmeasurable'", () => {
    const merged = mergeUsage([
      { input_tokens: 100, output_tokens: 50, cached_input_tokens: 60, reasoning_tokens: 10, attempts_reported: 1, attempts_not_reported: 0 },
      // A backend whose contract exposes no cache telemetry at all reports
      // input/output tokens with cached_input_tokens/reasoning_tokens
      // genuinely 0 (per the wire contract they're always-present sums,
      // never omitted) -- contributing 0 to the merge is exactly right.
      { input_tokens: 50, output_tokens: 20, cached_input_tokens: 0, reasoning_tokens: 0, attempts_reported: 1, attempts_not_reported: 0 },
    ]);
    expect(merged.input_tokens).toBe(150);
    expect(merged.cached_input_tokens).toBe(60);
  });

  it("recomputes cache_ratio from the merged sums rather than averaging the inputs' own ratios", () => {
    const merged = mergeUsage([
      // ratio 0.5 on its own
      { input_tokens: 100, output_tokens: 0, cached_input_tokens: 50, reasoning_tokens: 0, attempts_reported: 1, attempts_not_reported: 0 },
      // ratio 0 on its own
      { input_tokens: 900, output_tokens: 0, cached_input_tokens: 0, reasoning_tokens: 0, attempts_reported: 1, attempts_not_reported: 0 },
    ]);
    // A naive average of 0.5 and 0 would be 0.25; the honest weighted
    // figure over the merged sums is 50/1000 = 0.05.
    expect(merged.cache_ratio).toBeCloseTo(0.05);
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
