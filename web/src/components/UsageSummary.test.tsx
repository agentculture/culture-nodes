import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import type { Usage } from "../api/types";
import UsageSummary from "./UsageSummary";

describe("UsageSummary", () => {
  it("renders token totals as the primary figure", () => {
    const usage: Usage = {
      input_tokens: 12300,
      output_tokens: 4100,
      cached_input_tokens: 0,
      reasoning_tokens: 0,
      attempts_reported: 2,
      attempts_not_reported: 0,
    };
    render(<UsageSummary usage={usage} id="usage" />);
    expect(screen.getByText("12.3k in / 4.1k out")).toBeInTheDocument();
    expect(screen.getByText("12.3k in / 4.1k out").className).toContain(
      "usage-summary__tokens",
    );
  });

  it("renders cost as secondary only when the API actually reported one", () => {
    const noCost: Usage = {
      input_tokens: 10,
      output_tokens: 5,
      cached_input_tokens: 0,
      reasoning_tokens: 0,
      attempts_reported: 1,
      attempts_not_reported: 0,
    };
    const { container, rerender } = render(<UsageSummary usage={noCost} />);
    expect(container.querySelector(".usage-summary__cost")).toBeNull();

    const withCost: Usage = { ...noCost, cost: 1.5, currency: "USD" };
    rerender(<UsageSummary usage={withCost} />);
    expect(screen.getByText("1.50 USD")).toBeInTheDocument();
  });

  it("never invents a currency when the API reported a cost without one", () => {
    const usage: Usage = {
      input_tokens: 10,
      output_tokens: 5,
      cached_input_tokens: 0,
      reasoning_tokens: 0,
      cost: 3,
      attempts_reported: 1,
      attempts_not_reported: 0,
    };
    render(<UsageSummary usage={usage} />);
    expect(screen.getByText("3.00")).toBeInTheDocument();
    expect(screen.queryByText(/USD|EUR|\$/)).not.toBeInTheDocument();
  });

  it("renders one line per cost_by_currency entry, never summed together", () => {
    const usage: Usage = {
      input_tokens: 10,
      output_tokens: 5,
      cached_input_tokens: 0,
      reasoning_tokens: 0,
      cost_by_currency: [
        { currency: "USD", cost: 1.5 },
        { currency: "EUR", cost: 2 },
      ],
      attempts_reported: 2,
      attempts_not_reported: 0,
    };
    render(<UsageSummary usage={usage} />);
    expect(screen.getByText("1.50 USD, 2.00 EUR")).toBeInTheDocument();
  });

  it("renders an explicit not-reported state, and NOT '0 tokens', when attempts_reported is 0", () => {
    // This is the honesty-condition unit test t5's acceptance criteria calls
    // out by name: absent usage must never render as a zero.
    const usage: Usage = {
      input_tokens: 0,
      output_tokens: 0,
      cached_input_tokens: 0,
      reasoning_tokens: 0,
      attempts_reported: 0,
      attempts_not_reported: 3,
    };
    render(<UsageSummary usage={usage} id="usage" />);
    expect(screen.getByText("usage not reported")).toBeInTheDocument();
    expect(document.getElementById("usage")).toHaveAttribute(
      "data-usage-reported",
      "false",
    );
    expect(screen.queryByText(/0 in \/ 0 out/)).not.toBeInTheDocument();
    expect(screen.queryByText(/0 tokens/)).not.toBeInTheDocument();
  });

  it("still renders a genuinely reported zero as a zero, distinct from not-reported", () => {
    const usage: Usage = {
      input_tokens: 0,
      output_tokens: 0,
      cached_input_tokens: 0,
      reasoning_tokens: 0,
      attempts_reported: 1,
      attempts_not_reported: 0,
    };
    render(<UsageSummary usage={usage} id="usage" />);
    expect(screen.getByText("0 in / 0 out")).toBeInTheDocument();
    expect(document.getElementById("usage")).toHaveAttribute(
      "data-usage-reported",
      "true",
    );
  });

  it("notes how many attempts did not report usage, alongside a reported figure", () => {
    const usage: Usage = {
      input_tokens: 100,
      output_tokens: 50,
      cached_input_tokens: 0,
      reasoning_tokens: 0,
      attempts_reported: 1,
      attempts_not_reported: 2,
    };
    render(<UsageSummary usage={usage} />);
    expect(screen.getByText(/2 attempts not reported/)).toBeInTheDocument();
  });

  it("shows attempts_reported explicitly, alongside the existing not-reported note (task t11)", () => {
    const usage: Usage = {
      input_tokens: 5200,
      output_tokens: 1800,
      cached_input_tokens: 0,
      reasoning_tokens: 0,
      cost: 0.52,
      currency: "USD",
      attempts_reported: 1,
      attempts_not_reported: 1,
    };
    render(<UsageSummary usage={usage} id="usage" />);
    expect(screen.getByText("1 attempt reported")).toBeInTheDocument();
    expect(screen.getByText(/1 attempt not reported/)).toBeInTheDocument();
    expect(document.getElementById("usage")).toHaveAttribute(
      "data-attempts-reported",
      "1",
    );
    expect(document.getElementById("usage")).toHaveAttribute(
      "data-attempts-not-reported",
      "1",
    );
  });

  it("pluralizes the reported-attempts count", () => {
    const usage: Usage = {
      input_tokens: 100,
      output_tokens: 50,
      cached_input_tokens: 0,
      reasoning_tokens: 0,
      attempts_reported: 3,
      attempts_not_reported: 0,
    };
    render(<UsageSummary usage={usage} />);
    expect(screen.getByText("3 attempts reported")).toBeInTheDocument();
  });

  it("carries attempts_reported/not_reported as data attributes even in the not-reported branch", () => {
    const usage: Usage = {
      input_tokens: 0,
      output_tokens: 0,
      cached_input_tokens: 0,
      reasoning_tokens: 0,
      attempts_reported: 0,
      attempts_not_reported: 2,
    };
    render(<UsageSummary usage={usage} id="usage" />);
    // The visible copy stays exactly "usage not reported" (asserted
    // elsewhere byte-for-byte) — the counts ride as attributes instead.
    expect(screen.getByText("usage not reported")).toBeInTheDocument();
    expect(document.getElementById("usage")).toHaveAttribute(
      "data-attempts-reported",
      "0",
    );
    expect(document.getElementById("usage")).toHaveAttribute(
      "data-attempts-not-reported",
      "2",
    );
  });

  it("drops the explicit attempts-reported line in compact mode", () => {
    const usage: Usage = {
      input_tokens: 100,
      output_tokens: 50,
      cached_input_tokens: 0,
      reasoning_tokens: 0,
      attempts_reported: 1,
      attempts_not_reported: 0,
    };
    render(<UsageSummary usage={usage} compact />);
    expect(screen.queryByText(/attempt.*reported/)).not.toBeInTheDocument();
  });

  it("compact mode drops the partial-attempts note and shortens the not-reported label", () => {
    const usage: Usage = {
      input_tokens: 0,
      output_tokens: 0,
      cached_input_tokens: 0,
      reasoning_tokens: 0,
      attempts_reported: 0,
      attempts_not_reported: 1,
    };
    render(<UsageSummary usage={usage} compact />);
    expect(screen.getByText("not reported")).toBeInTheDocument();
    expect(screen.queryByText("usage not reported")).not.toBeInTheDocument();
  });
});
