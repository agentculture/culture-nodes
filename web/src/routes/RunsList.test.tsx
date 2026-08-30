import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import RunsList from "./RunsList";
import { ApiError, listRuns } from "../api/client";
import { BOARD_RUNS } from "../fixtures/runs-board-fixture";
import { getAgentState, resetAgentState } from "../agent-state/store";
import { resetSharedEventsForTests } from "../hooks/useSharedEvents";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return { ...actual, listRuns: vi.fn() };
});

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

function LocationProbe() {
  const location = useLocation();
  return <div data-testid="location-search">{location.search}</div>;
}

function renderList(initialEntries: string[] = ["/runs"]) {
  return render(
    <MemoryRouter initialEntries={initialEntries}>
      <RunsList />
      <LocationProbe />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mockListRuns.mockReset();
  resetAgentState();
});

describe("RunsList loading/empty/error", () => {
  it("shows a loading state before the first response resolves", () => {
    mockListRuns.mockReturnValue(new Promise(() => {})); // never resolves
    renderList();
    expect(screen.getByText("Loading runs…")).toBeInTheDocument();
  });

  it("shows the empty state when the API reports no runs", async () => {
    mockListRuns.mockResolvedValue({ items: [] });
    renderList();
    expect(
      await screen.findByText(/No runs in this range\./),
    ).toBeInTheDocument();
  });

  it("renders an error notice and stops loading when the request fails", async () => {
    mockListRuns.mockRejectedValue(
      new ApiError(0, "cannot reach the control plane", "start `nodes serve`"),
    );
    renderList();
    await screen.findByText("error:", { exact: false });
    expect(
      screen.getByText("cannot reach the control plane", { exact: false }),
    ).toBeInTheDocument();
  });
});

describe("RunsList data + table", () => {

  it("collapses 50 consecutive failed runs of one workflow and shows its workflow key", async () => {
    const failedSweeps = Array.from({ length: 50 }, (_, index) => ({
      ...BOARD_RUNS[0],
      id: `sweep-${index + 1}`,
      state: "failed" as const,
      workflow_key: "pr-upkeep-sweep-cycle",
    }));
    mockListRuns.mockResolvedValue({ items: failedSweeps });
    renderList();

    const table = await screen.findByRole("table");
    expect(within(table).getAllByRole("row")).toHaveLength(2);
    expect(screen.getByText("pr-upkeep-sweep-cycle")).toBeInTheDocument();
    expect(screen.getByText("50")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "sweep-2" })).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: /expand 50 failed runs/i }));
    expect(screen.getByRole("link", { name: "sweep-2" })).toBeInTheDocument();
  });

  it("loads and appends the next page using next_cursor", async () => {
    mockListRuns
      .mockResolvedValueOnce({ items: [BOARD_RUNS[0]], next_cursor: "runs-page-2" })
      .mockResolvedValueOnce({ items: [BOARD_RUNS[1]] });
    renderList();
    await screen.findByRole("link", { name: BOARD_RUNS[0].id });

    await userEvent.click(screen.getByRole("button", { name: "Load more" }));

    expect(await screen.findByRole("link", { name: BOARD_RUNS[1].id })).toBeInTheDocument();
    expect(mockListRuns).toHaveBeenLastCalledWith(undefined, {
      sort: "updated_at",
      updated_since: undefined,
      updated_until: undefined,
      cursor: "runs-page-2",
    });
  });

  it("refetches through the API when the state filter changes", async () => {
    mockListRuns.mockResolvedValue({ items: BOARD_RUNS });
    renderList();
    await screen.findByRole("table");

    await userEvent.selectOptions(screen.getByRole("combobox", { name: "State" }), "failed");

    await waitFor(() => expect(mockListRuns).toHaveBeenCalledTimes(2));
    expect(mockListRuns.mock.calls[1][1]).toMatchObject({ state: "failed" });
  });

  it("sorts by updated_at, states the ordering contract, and links every run into /runs/:id", async () => {
    mockListRuns.mockResolvedValue({ items: BOARD_RUNS });
    renderList();
    await screen.findByRole("table");

    expect(mockListRuns).toHaveBeenCalledTimes(1);
    const [, params] = mockListRuns.mock.calls[0];
    expect(params).toEqual({
      sort: "updated_at",
      updated_since: undefined,
      updated_until: undefined,
    });

    expect(
      screen.getByText("Every run, newest first by last update."),
    ).toBeInTheDocument();

    for (const run of BOARD_RUNS) {
      expect(screen.getByRole("link", { name: run.id })).toHaveAttribute(
        "href",
        `/runs/${run.id}`,
      );
    }
  });

  it("renders an updated column so the ordering statement has visible backing", async () => {
    mockListRuns.mockResolvedValue({ items: BOARD_RUNS });
    renderList();
    await screen.findByRole("table");
    expect(
      screen.getByRole("columnheader", { name: "updated" }),
    ).toBeInTheDocument();
  });
});

describe("RunsList name, derived hint, and category (task t5)", () => {
  it("shows the run's given name as the link text, not marked as derived", async () => {
    mockListRuns.mockResolvedValue({
      items: [{ ...BOARD_RUNS[0], name: "nightly regression sweep" }],
    });
    renderList();
    const link = await screen.findByRole("link", {
      name: "nightly regression sweep",
    });
    expect(link).toHaveAttribute("href", `/runs/${BOARD_RUNS[0].id}`);
    expect(screen.queryByText(BOARD_RUNS[0].id)).not.toBeInTheDocument();
  });

  it("falls back to display_hint, visibly marked as a derived guess", async () => {
    mockListRuns.mockResolvedValue({
      items: [
        {
          ...BOARD_RUNS[0],
          display_hint: "add the ledger projection endpoint",
        },
      ],
    });
    renderList();
    await screen.findByRole("table");
    const hint = screen.getByText("add the ledger projection endpoint");
    expect(hint).toHaveAttribute("data-derived", "true");
    expect(hint.className).toContain("run-name--derived");
  });

  it("falls back to the run id when neither name nor hint is given", async () => {
    mockListRuns.mockResolvedValue({ items: [BOARD_RUNS[0]] });
    renderList();
    const link = await screen.findByRole("link", { name: BOARD_RUNS[0].id });
    expect(link.querySelector(".run-name--derived")).toBeNull();
  });

  it("renders the category as a chip, and an em dash when the run has none", async () => {
    mockListRuns.mockResolvedValue({
      items: [
        { ...BOARD_RUNS[0], category: "ci" },
        BOARD_RUNS[1],
      ],
    });
    renderList();
    await screen.findByRole("table");
    const chip = screen.getByText("ci");
    expect(chip.className).toContain("category-chip");
    const rows = screen.getAllByRole("row");
    expect(within(rows[2]).getByText("—")).toBeInTheDocument();
  });
});

describe("RunsList time-range filter (server-side, issue #23)", () => {
  it("treats empty or unparseable since/until URL params as absent instead of forwarding them", async () => {
    mockListRuns.mockResolvedValue({ items: BOARD_RUNS });
    renderList(["/runs?since=&until=not-a-timestamp"]);
    await screen.findByRole("table");

    const [, params] = mockListRuns.mock.calls[0];
    expect(params).toEqual({
      sort: "updated_at",
      updated_since: undefined,
      updated_until: undefined,
    });
  });

  it("returns to the loading state on a range change instead of showing the previous range's rows", async () => {
    mockListRuns.mockResolvedValueOnce({ items: BOARD_RUNS });
    const user = userEvent.setup();
    renderList();
    await screen.findByRole("table");

    mockListRuns.mockReturnValue(new Promise(() => {})); // never resolves
    await user.click(screen.getByRole("button", { name: "Last hour" }));

    await waitFor(() =>
      expect(screen.queryByRole("table")).not.toBeInTheDocument(),
    );
    expect(screen.getByText("Loading runs…")).toBeInTheDocument();
  });

  it("loads an already-bookmarked since/until straight from the URL on first render", async () => {
    mockListRuns.mockResolvedValue({ items: BOARD_RUNS });
    renderList([
      "/runs?since=2026-08-01T00%3A00%3A00.000Z&until=2026-08-02T00%3A00%3A00.000Z",
    ]);
    await screen.findByRole("table");

    expect(mockListRuns).toHaveBeenCalledTimes(1);
    const [, params] = mockListRuns.mock.calls[0];
    expect(params).toEqual({
      sort: "updated_at",
      updated_since: "2026-08-01T00:00:00.000Z",
      updated_until: "2026-08-02T00:00:00.000Z",
    });
  });

  it("selecting a preset updates the URL search params and refetches with updated_since carrying that value", async () => {
    mockListRuns.mockResolvedValue({ items: BOARD_RUNS });
    const user = userEvent.setup();
    renderList();
    await screen.findByRole("table");
    expect(mockListRuns).toHaveBeenCalledTimes(1);

    await user.click(screen.getByRole("button", { name: "Last hour" }));

    await waitFor(() => expect(mockListRuns).toHaveBeenCalledTimes(2));
    const [, params] = mockListRuns.mock.calls[1];
    expect(params?.updated_since).toBeTruthy();
    expect(params?.updated_until).toBeUndefined();
    expect(params?.sort).toBe("updated_at");
    const since = params?.updated_since as string;

    // The URL carries the exact value the request used — same
    // shareable/bookmarkable contract as the Jobs view.
    expect(screen.getByTestId("location-search")).toHaveTextContent(
      `since=${encodeURIComponent(since)}`,
    );
  });

  it("never filters client-side: a change in range always produces a new request, not a re-slice of the existing items", async () => {
    mockListRuns.mockResolvedValue({ items: BOARD_RUNS });
    const user = userEvent.setup();
    renderList();
    await screen.findByRole("table");
    mockListRuns.mockClear();

    const kept = BOARD_RUNS.slice(0, 1);
    mockListRuns.mockResolvedValue({ items: kept });
    await user.click(screen.getByRole("button", { name: "Last 24h" }));

    await waitFor(() => expect(mockListRuns).toHaveBeenCalledTimes(1));
    await screen.findByRole("link", { name: kept[0].id });
    expect(
      screen.queryByRole("link", { name: BOARD_RUNS[1].id }),
    ).not.toBeInTheDocument();
  });
});

describe("RunsList auto-refresh (issue #46, task t30)", () => {
  beforeEach(() => {
    resetSharedEventsForTests();
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
  });

  afterEach(() => {
    resetSharedEventsForTests();
    vi.unstubAllGlobals();
  });

  it("refetches on a run-lifecycle event, staying stale-while-revalidate: no loading regression, no nulled table", async () => {
    mockListRuns.mockResolvedValueOnce({ items: BOARD_RUNS });
    renderList();
    await waitFor(() => expect(screen.getByRole("table")).toBeInTheDocument());
    await waitFor(() => expect(getAgentState().status).toBe("ready"));

    const source = FakeEventSource.instances[0];
    act(() => source.open());

    // The reload fetch's own promise stays pending until this test resolves
    // it, so the assertions below observe the view mid-refetch.
    let resolveReload: ((value: { items: typeof BOARD_RUNS }) => void) | undefined;
    mockListRuns.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveReload = resolve;
        }),
    );

    act(() => {
      source.emit("dev.culture.nodes.run.completed", { run_id: BOARD_RUNS[0].id }, "01EVT1");
    });

    // The debounced reload fires (the first reload after mount schedules
    // with no delay, same as Mesh's attribution refresh); wait for the
    // second `listRuns` call to actually start.
    await waitFor(() => expect(mockListRuns).toHaveBeenCalledTimes(2));

    // The reload fetch is now in flight — the original rows and agent-state
    // must still be exactly as they were (stale-while-revalidate).
    expect(screen.getByRole("table")).toBeInTheDocument();
    expect(screen.queryByText("Loading runs…")).not.toBeInTheDocument();
    expect(getAgentState().status).toBe("ready");
    for (const run of BOARD_RUNS) {
      expect(screen.getByRole("link", { name: run.id })).toBeInTheDocument();
    }

    const updated = [{ ...BOARD_RUNS[0], state: "completed" as const }];
    await act(async () => {
      resolveReload?.({ items: updated });
    });

    await waitFor(() =>
      expect(
        screen.queryByRole("link", { name: BOARD_RUNS[1].id }),
      ).not.toBeInTheDocument(),
    );
    expect(screen.getByRole("link", { name: updated[0].id })).toBeInTheDocument();
    expect(getAgentState().status).toBe("ready");
  });

  it("debounces a burst of simultaneous events into a single refetch", async () => {
    mockListRuns.mockResolvedValueOnce({ items: BOARD_RUNS });
    renderList();
    await waitFor(() => expect(screen.getByRole("table")).toBeInTheDocument());

    const source = FakeEventSource.instances[0];
    act(() => source.open());
    mockListRuns.mockClear();
    mockListRuns.mockResolvedValue({ items: BOARD_RUNS });

    act(() => {
      source.emit("dev.culture.nodes.run.completed", { run_id: "a" }, "01EVT1");
      source.emit("dev.culture.nodes.run.failed", { run_id: "b" }, "01EVT2");
      source.emit("dev.culture.nodes.run.cancelled", { run_id: "c" }, "01EVT3");
    });

    await waitFor(() => expect(mockListRuns).toHaveBeenCalledTimes(1));
    // Give any accidental second refetch a chance to have started too.
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(mockListRuns).toHaveBeenCalledTimes(1);
  });

  it("ignores an event type this view did not subscribe to", async () => {
    mockListRuns.mockResolvedValueOnce({ items: BOARD_RUNS });
    renderList();
    await waitFor(() => expect(screen.getByRole("table")).toBeInTheDocument());

    const source = FakeEventSource.instances[0];
    act(() => source.open());
    mockListRuns.mockClear();

    act(() => {
      source.emit("dev.culture.nodes.attempt.started", { run_id: "a" }, "01EVT1");
    });
    await new Promise((resolve) => setTimeout(resolve, 20));

    expect(mockListRuns).not.toHaveBeenCalled();
  });
});


describe("RunsList state chip and humanised time (task t27)", () => {
  // Relative time is read off the real clock, so the one test that asserts a
  // rendered phrase pins the clock and puts it back — a leaked system time
  // would make every later test in this file time-dependent.
  afterEach(() => {
    vi.useRealTimers();
  });

  it("renders state as the same chip the Board and Jobs use, not a bare word", async () => {
    mockListRuns.mockResolvedValue({ items: [BOARD_RUNS[1]] });
    renderList();
    await screen.findByRole("table");

    const row = document.querySelector(`[data-run-id="${BOARD_RUNS[1].id}"]`)!;
    const chip = row.querySelector(".status-chip")!;
    expect(chip).toHaveAttribute("data-run-state", "running");
    // Icon AND word, never colour alone (PRD §8.8) — the same contract
    // RunStateChip.test.tsx pins for the component itself.
    expect(chip.querySelector(".status-chip__label")).toHaveTextContent("running");
    expect(chip.querySelector(".status-chip__icon")).not.toBeNull();
  });

  it("renders timestamps relative, keeping the exact instant on title and dateTime", async () => {
    const run = {
      ...BOARD_RUNS[1],
      created_at: "2026-08-30T10:00:00Z",
      updated_at: "2026-08-30T11:00:00Z",
    };
    mockListRuns.mockResolvedValue({ items: [run] });
    vi.useFakeTimers({
      now: new Date("2026-08-30T12:00:00Z"),
      shouldAdvanceTime: true,
    });
    renderList();
    await screen.findByRole("table");

    const times = document.querySelectorAll(
      `[data-run-id="${run.id}"] time`,
    ) as NodeListOf<HTMLTimeElement>;
    expect(times).toHaveLength(2);
    expect(times[0]).toHaveTextContent("2 hours ago");
    expect(times[0]).toHaveAttribute("title", run.created_at);
    expect(times[0]).toHaveAttribute("dateTime", run.created_at);
    expect(times[1]).toHaveTextContent("1 hour ago");
    expect(times[1]).toHaveAttribute("title", run.updated_at);
  });

  it("still shows the collapse badge next to the chip on a grouped failure run", async () => {
    const failures = Array.from({ length: 3 }, (_, index) => ({
      ...BOARD_RUNS[0],
      id: `sweep-${index + 1}`,
      state: "failed" as const,
      workflow_key: "pr-upkeep-sweep-cycle",
    }));
    mockListRuns.mockResolvedValue({ items: failures });
    renderList();
    await screen.findByRole("table");

    expect(
      screen.getByRole("button", { name: /expand 3 failed runs/i }),
    ).toBeInTheDocument();
    expect(
      document.querySelector('[data-run-id="sweep-1"] .status-chip'),
    ).toHaveAttribute("data-run-state", "failed");
  });
});
