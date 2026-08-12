import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import Statistics from "./Statistics";
import { ApiError, listNodeRuns, listRuns } from "../api/client";
import { getAgentState, resetAgentState } from "../agent-state/store";
import {
  STATS_CURSOR,
  STATS_NODE_RUNS_PAGE_1,
  STATS_NODE_RUNS_PAGE_2,
  STATS_RUNS,
  STATS_RUN_A,
  STATS_RUN_C,
  STATS_RUN_E,
} from "../fixtures/statistics-fixture";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return { ...actual, listNodeRuns: vi.fn(), listRuns: vi.fn() };
});

const mockListNodeRuns = vi.mocked(listNodeRuns);
const mockListRuns = vi.mocked(listRuns);

function renderStatistics(initialEntries: string[] = ["/stats"]) {
  return render(
    <MemoryRouter initialEntries={initialEntries}>
      <Statistics />
    </MemoryRouter>,
  );
}

/** Answers exactly the two-page sequence the fixture describes, keyed off `cursor`. */
function mockTwoPages() {
  mockListNodeRuns.mockImplementation((_signal, params) => {
    if (params?.cursor === STATS_CURSOR) {
      return Promise.resolve({ items: STATS_NODE_RUNS_PAGE_2 });
    }
    return Promise.resolve({
      items: STATS_NODE_RUNS_PAGE_1,
      next_cursor: STATS_CURSOR,
    });
  });
}

beforeEach(() => {
  mockListNodeRuns.mockReset();
  mockListRuns.mockReset();
  mockListRuns.mockResolvedValue({ items: STATS_RUNS });
  resetAgentState();
});

describe("Statistics loading/empty/error", () => {
  it("shows a loading state before the first response resolves", () => {
    mockListNodeRuns.mockReturnValue(new Promise(() => {}));
    mockListRuns.mockReturnValue(new Promise(() => {}));
    renderStatistics();
    expect(screen.getByText("Loading statistics…")).toBeInTheDocument();
  });

  it("shows the empty state when the window has no node runs", async () => {
    mockListNodeRuns.mockResolvedValue({ items: [] });
    renderStatistics();
    expect(
      await screen.findByText("No node runs in this range."),
    ).toBeInTheDocument();
  });

  it("renders an error notice and still marks agent-state ready when the fetch fails", async () => {
    mockListNodeRuns.mockRejectedValue(
      new ApiError(0, "cannot reach the control plane", "start `nodes serve`"),
    );
    renderStatistics();
    await screen.findByText("error:", { exact: false });
    expect(getAgentState().status).toBe("ready");
  });
});

describe("Statistics pagination", () => {
  it("walks every page of node-runs before computing the aggregate, not just the first", async () => {
    mockTwoPages();
    renderStatistics();
    await screen.findByRole("table");

    expect(mockListNodeRuns).toHaveBeenCalledTimes(2);
    const [, secondCallParams] = mockListNodeRuns.mock.calls[1];
    expect(secondCallParams?.cursor).toBe(STATS_CURSOR);

    // Total tokens must reflect BOTH pages (7500 input across 4 reporting
    // runs), proving the aggregate isn't computed from page 1 alone.
    expect(screen.getByText(/7\.5k in/)).toBeInTheDocument();
  });
});

describe("Statistics denominator (honesty condition h9)", () => {
  beforeEach(() => mockTwoPages());

  it("states total, reported, and excluded run counts, with excluded visible (not a footnote)", async () => {
    renderStatistics();
    await screen.findByRole("table");

    const denominator = document.getElementById("statistics-denominator")!;
    expect(denominator).toHaveTextContent("5 runs in this window");
    expect(denominator).toHaveTextContent("4 reported usage");
    const excluded = document.getElementById("statistics-excluded");
    expect(excluded).toHaveTextContent("1 excluded (usage never reported)");
  });

  it("never folds the excluded run's zero usage into the token average", async () => {
    renderStatistics();
    await screen.findByRole("table");
    // avg input 1875 (=7500/4 reporting runs) -> "1.9k in", NOT 1500 (which
    // would result from dividing by 5 and folding run-e in as a zero).
    const avgTile = document.getElementById("stat-tile-avg-tokens")!;
    expect(within(avgTile).getByText(/1\.9k in/)).toBeInTheDocument();
  });
});

describe("Statistics tokens/cost aggregation", () => {
  beforeEach(() => mockTwoPages());

  it("computes average and median tokens/cost per run, grouped by run_id first", async () => {
    renderStatistics();
    await screen.findByRole("table");

    const medianTile = document.getElementById("stat-tile-median-tokens")!;
    // median input 1750 -> "1.8k in"
    expect(within(medianTile).getByText(/1\.8k in/)).toBeInTheDocument();

    const costTile = document.getElementById("stat-tile-avg-cost")!;
    expect(within(costTile)).toBeTruthy();
    expect(costTile).toHaveTextContent("3.75");
    expect(costTile).toHaveTextContent("3.50");
    expect(costTile).toHaveTextContent("USD");
  });

  it("proves per-run grouping: run-a's two node runs are merged before averaging", async () => {
    renderStatistics();
    await screen.findByRole("table");
    // The fixture's run-a is split into nr-stat-a1/a2 (400+600=1000 input).
    // If the view averaged over node runs instead of runs, there would be 6
    // contributing rows instead of 4 and the numbers above would differ —
    // this test pins the run_id used as the grouping key exists and only
    // appears once in the aggregate by checking the reported-run count.
    const denominator = document.getElementById("statistics-denominator")!;
    expect(denominator).toHaveTextContent("4 reported usage");
    void STATS_RUN_A;
  });
});

describe("Statistics category breakdown", () => {
  beforeEach(() => mockTwoPages());

  it("renders one row per category, an uncategorized bucket, totals and averages honoring the same denominator rules", async () => {
    renderStatistics();
    await screen.findByRole("table", { name: /categor/i });

    const table = document.getElementById("category-stats-table")!;
    const ciRow = within(table).getByRole("row", {
      name: /ci/i,
    });
    expect(ciRow).toHaveTextContent("3"); // total runs
    expect(within(ciRow).getByText("2")).toBeInTheDocument(); // reported

    const uncategorizedRow = table.querySelector(
      '[data-category="uncategorized"]',
    )?.closest("tr");
    expect(uncategorizedRow).toBeTruthy();
    expect(uncategorizedRow).toHaveTextContent("Uncategorized");
  });
});

describe("Statistics agent-state registration", () => {
  beforeEach(() => mockTwoPages());

  it("registers totals and the denominator in #agent-state", async () => {
    renderStatistics();
    await screen.findByRole("table");

    const statistics = getAgentState().statistics;
    expect(statistics).toMatchObject({
      total_runs: 5,
      reported_runs: 4,
      excluded_runs: 1,
      total_input_tokens: 7500,
      total_output_tokens: 3750,
      avg_input_tokens: 1875,
      median_input_tokens: 1750,
      cost_currency: "USD",
      avg_cost: 3.75,
      median_cost: 3.5,
    });
    expect(statistics?.category_count).toBeGreaterThan(0);
  });
});

describe("Statistics time filter", () => {
  it("passes since/until straight through as updated_since/updated_until on every page request", async () => {
    mockListNodeRuns.mockResolvedValue({ items: STATS_NODE_RUNS_PAGE_1 });
    renderStatistics([
      "/stats?since=2026-08-01T00%3A00%3A00.000Z&until=2026-08-02T00%3A00%3A00.000Z",
    ]);
    await screen.findByRole("table");

    const [, params] = mockListNodeRuns.mock.calls[0];
    expect(params).toMatchObject({
      updated_since: "2026-08-01T00:00:00.000Z",
      updated_until: "2026-08-02T00:00:00.000Z",
    });
  });

  it("refetches and recomputes the aggregate when the range changes", async () => {
    // Page 1 (unfiltered): run-a + run-b -> 2 runs. Selecting "Last hour"
    // re-requests and this fixture answers with run-c alone -> 1 run —
    // the aggregate must move, not silently keep showing the old totals.
    mockListNodeRuns.mockResolvedValueOnce({ items: STATS_NODE_RUNS_PAGE_1 });
    mockListNodeRuns.mockResolvedValueOnce({
      items: [STATS_NODE_RUNS_PAGE_2[0]], // run-c only
    });
    const user = userEvent.setup();
    renderStatistics();
    await screen.findByRole("table");
    expect(
      document.getElementById("statistics-denominator"),
    ).toHaveTextContent("2 runs in this window");

    await user.click(screen.getByRole("button", { name: "Last hour" }));
    await waitFor(() => expect(mockListNodeRuns).toHaveBeenCalledTimes(2));

    await waitFor(() =>
      expect(
        document.getElementById("statistics-denominator"),
      ).toHaveTextContent("1 run in this window"),
    );
    void STATS_RUN_C;
    void STATS_RUN_E;
  });
});
