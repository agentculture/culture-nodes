import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import Statistics from "./Statistics";
import { ApiError, listNodeRuns, listRuns } from "../api/client";
import { getAgentState, resetAgentState } from "../agent-state/store";
import { resetSharedEventsForTests } from "../hooks/useSharedEvents";
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

/** A minimal fake of the shared cross-run EventSource (mirrors Mesh.test.tsx). */
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  url: string;
  readyState = 0;
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: string; lastEventId: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  private listeners = new Map<
    string,
    Array<(event: { data: string; lastEventId: string }) => void>
  >();

  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }

  addEventListener(
    type: string,
    listener: (event: { data: string; lastEventId: string }) => void,
  ) {
    const list = this.listeners.get(type) ?? [];
    list.push(listener);
    this.listeners.set(type, list);
  }

  close() {
    this.readyState = 2;
  }

  open() {
    this.readyState = 1;
    this.onopen?.();
  }

  emit(type: string, data: Record<string, unknown>, id: string) {
    const envelope = {
      id,
      source: "nodes",
      specversion: "1.0",
      type,
      time: "2026-08-13T00:00:00Z",
      datacontenttype: "application/json",
      data,
    };
    const event = { data: JSON.stringify(envelope), lastEventId: id };
    for (const listener of this.listeners.get(type) ?? []) listener(event);
  }
}

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

describe("Statistics cache-ratio tile (task t2, ADR 0009)", () => {
  beforeEach(() => mockTwoPages());

  it("renders the window's cache hit rate computed from cached/input across every reporting run", async () => {
    renderStatistics();
    await screen.findByRole("table");

    // Fixture total: cached 1000 (200 from nr-stat-a1 + 800 from
    // nr-stat-b1) over the whole 8500-token prompt those runs consumed
    // (input 7500 + the 1000 cache reads reported beside it) = 11.8%
    // (task t8: cache reads are NOT inside input_tokens, so 1000/7500
    // would overstate the hit rate — and on production data the same
    // division read 588%).
    const cacheTile = document.getElementById("stat-tile-cache-ratio")!;
    expect(within(cacheTile).getByText(/11\.8% cached/)).toBeInTheDocument();
  });

  it("renders an honest not-computable state, never a fabricated 0%, when no node runs are in the window", async () => {
    mockListNodeRuns.mockReset();
    mockListNodeRuns.mockResolvedValue({ items: [] });
    renderStatistics();
    await screen.findByText("No node runs in this range.");
    // The stat tile itself doesn't render at all in the empty state (the
    // whole stat-tiles block is gated on stats.totalRuns > 0) — this test
    // pins that the empty state short-circuits before any tile, cache-ratio
    // included, could render a fabricated figure.
    expect(document.getElementById("stat-tile-cache-ratio")).toBeNull();
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

    // The table appearing and the agent-state registration are two different
    // effects. Waiting on the first says nothing about the second, so reading
    // the store straight after findByRole is a race that only loses on a
    // slower machine -- which is why this passed locally and failed only in
    // CI. Wait for the thing actually being asserted, the way
    // LedgerView.test.tsx already does for getAgentState().status.
    await waitFor(() => expect(getAgentState().statistics).toBeDefined());

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

describe("Statistics auto-refresh (issue #46, task t30)", () => {
  beforeEach(() => {
    resetSharedEventsForTests();
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
  });

  afterEach(() => {
    resetSharedEventsForTests();
    vi.unstubAllGlobals();
  });

  it("refetches on a usage-affecting event, staying stale-while-revalidate: no loading regression, no nulled table", async () => {
    mockListNodeRuns.mockResolvedValueOnce({ items: STATS_NODE_RUNS_PAGE_1 });
    renderStatistics();
    await screen.findByRole("table");
    await waitFor(() => expect(getAgentState().status).toBe("ready"));
    expect(
      document.getElementById("statistics-denominator"),
    ).toHaveTextContent("2 runs in this window");

    const source = FakeEventSource.instances[0];
    act(() => source.open());

    let resolveReload: ((value: { items: typeof STATS_NODE_RUNS_PAGE_1 }) => void) | undefined;
    mockListNodeRuns.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveReload = resolve;
        }),
    );

    act(() => {
      source.emit("dev.culture.nodes.attempt.completed", { run_id: STATS_RUN_A }, "01EVT1");
    });

    await waitFor(() => expect(mockListNodeRuns).toHaveBeenCalledTimes(2));

    // The reload fetch is in flight — the original stats and agent-state
    // must still be exactly as they were (stale-while-revalidate).
    expect(screen.getByRole("table")).toBeInTheDocument();
    expect(screen.queryByText("Loading statistics…")).not.toBeInTheDocument();
    expect(getAgentState().status).toBe("ready");
    expect(
      document.getElementById("statistics-denominator"),
    ).toHaveTextContent("2 runs in this window");

    await act(async () => {
      resolveReload?.({ items: [STATS_NODE_RUNS_PAGE_2[0]] });
    });

    await waitFor(() =>
      expect(
        document.getElementById("statistics-denominator"),
      ).toHaveTextContent("1 run in this window"),
    );
    expect(getAgentState().status).toBe("ready");
  });

  it("debounces a burst of simultaneous events into a single refetch", async () => {
    mockListNodeRuns.mockResolvedValueOnce({ items: STATS_NODE_RUNS_PAGE_1 });
    renderStatistics();
    await screen.findByRole("table");

    const source = FakeEventSource.instances[0];
    act(() => source.open());
    mockListNodeRuns.mockClear();
    mockListNodeRuns.mockResolvedValue({ items: STATS_NODE_RUNS_PAGE_1 });

    act(() => {
      source.emit("dev.culture.nodes.attempt.completed", { run_id: "a" }, "01EVT1");
      source.emit("dev.culture.nodes.attempt.completed", { run_id: "b" }, "01EVT2");
      source.emit("dev.culture.nodes.node-run.failed", { run_id: "c" }, "01EVT3");
    });

    await waitFor(() => expect(mockListNodeRuns).toHaveBeenCalledTimes(1));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(mockListNodeRuns).toHaveBeenCalledTimes(1);
  });

  it("ignores an event type this view did not subscribe to", async () => {
    mockListNodeRuns.mockResolvedValueOnce({ items: STATS_NODE_RUNS_PAGE_1 });
    renderStatistics();
    await screen.findByRole("table");

    const source = FakeEventSource.instances[0];
    act(() => source.open());
    mockListNodeRuns.mockClear();

    act(() => {
      source.emit("dev.culture.nodes.run.created", { run_id: "a" }, "01EVT1");
    });
    await new Promise((resolve) => setTimeout(resolve, 20));

    expect(mockListNodeRuns).not.toHaveBeenCalled();
  });
});
