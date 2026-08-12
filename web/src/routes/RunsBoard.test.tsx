import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import RunsBoard from "./RunsBoard";
import { ApiError, listRuns } from "../api/client";
import { BOARD_RUNS } from "../fixtures/runs-board-fixture";
import { resetAgentState, getAgentState } from "../agent-state/store";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return { ...actual, listRuns: vi.fn() };
});

const mockListRuns = vi.mocked(listRuns);

function LocationProbe() {
  const location = useLocation();
  return <div data-testid="location-search">{location.search}</div>;
}

function renderBoard(initialEntries: string[] = ["/board"]) {
  return render(
    <MemoryRouter initialEntries={initialEntries}>
      <RunsBoard />
      <LocationProbe />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mockListRuns.mockReset();
  resetAgentState();
});

describe("RunsBoard loading/empty/error", () => {
  it("shows a loading state before the first response resolves", () => {
    mockListRuns.mockReturnValue(new Promise(() => {})); // never resolves
    renderBoard();
    expect(screen.getByText("Loading runs…")).toBeInTheDocument();
  });

  it("shows the empty state when the API reports no runs", async () => {
    mockListRuns.mockResolvedValue({ items: [] });
    renderBoard();
    expect(
      await screen.findByText(/No runs in this range\./),
    ).toBeInTheDocument();
  });

  it("renders an error notice and stops loading when the request fails", async () => {
    mockListRuns.mockRejectedValue(
      new ApiError(0, "cannot reach the control plane", "start `nodes serve`"),
    );
    renderBoard();
    await screen.findByText("error:", { exact: false });
    expect(
      screen.getByText("cannot reach the control plane", { exact: false }),
    ).toBeInTheDocument();
    expect(
      screen.getByText("start `nodes serve`", { exact: false }),
    ).toBeInTheDocument();
  });
});

describe("RunsBoard data fetch", () => {
  it("asks the API for runs sorted by updated_at (t11 params), unbounded by default", async () => {
    mockListRuns.mockResolvedValue({ items: BOARD_RUNS });
    renderBoard();
    await screen.findByRole("heading", { name: /^created/, level: 2 });
    expect(mockListRuns).toHaveBeenCalledTimes(1);
    const [, params] = mockListRuns.mock.calls[0];
    expect(params).toEqual({
      sort: "updated_at",
      updated_since: undefined,
      updated_until: undefined,
    });
  });

  it("marks agent-state ready once the initial load settles, loading otherwise", async () => {
    mockListRuns.mockResolvedValue({ items: BOARD_RUNS });
    renderBoard();
    expect(getAgentState().status).toBe("loading");
    await screen.findByRole("heading", { name: /^created/, level: 2 });
    expect(getAgentState().status).toBe("ready");
  });
});

describe("RunsBoard state columns", () => {
  it("renders one column per RunState (openapi.yaml's created/running/waiting/completed/failed/cancelled), never an invented label like 'queued'", async () => {
    mockListRuns.mockResolvedValue({ items: BOARD_RUNS });
    renderBoard();
    await screen.findByRole("heading", { name: /^created/, level: 2 });

    for (const state of [
      "created",
      "running",
      "waiting",
      "completed",
      "failed",
      "cancelled",
    ]) {
      expect(
        screen.getByRole("heading", { name: new RegExp(`^${state}`) }),
      ).toBeInTheDocument();
    }
    expect(screen.queryByText(/queued/i)).not.toBeInTheDocument();
  });

  it("places each run's card under the column matching its own committed state", async () => {
    mockListRuns.mockResolvedValue({ items: BOARD_RUNS });
    const { container } = renderBoard();
    await screen.findByRole("heading", { name: /^created/, level: 2 });

    for (const run of BOARD_RUNS) {
      const column = container.querySelector<HTMLElement>(
        `[data-column-state="${run.state}"]`,
      );
      expect(column).not.toBeNull();
      expect(
        within(column as HTMLElement).getByText(`${run.id.slice(0, 20)}…`),
      ).toBeInTheDocument();
    }
  });

  it("puts the approval-paused run under waiting, alongside any other wait — never a column of its own", async () => {
    mockListRuns.mockResolvedValue({ items: BOARD_RUNS });
    const { container } = renderBoard();
    await screen.findByRole("heading", { name: /^created/, level: 2 });

    const waitingColumn = container.querySelector<HTMLElement>(
      '[data-column-state="waiting"]',
    ) as HTMLElement;
    const approvalPaused = BOARD_RUNS.find((run) =>
      run.id.startsWith("run-waiting-approval"),
    )!;
    expect(
      within(waitingColumn).getByText(
        `${approvalPaused.id.slice(0, 20)}…`,
      ),
    ).toBeInTheDocument();
    // No sixth, "approval" column was invented alongside the six RunState ones.
    expect(container.querySelectorAll("[data-column-state]")).toHaveLength(6);
  });

  it("shows a column's own empty state when a state has no runs, rather than hiding the column", async () => {
    mockListRuns.mockResolvedValue({
      items: BOARD_RUNS.filter((run) => run.state !== "cancelled"),
    });
    const { container } = renderBoard();
    await screen.findByRole("heading", { name: /^created/, level: 2 });
    const cancelledColumn = container.querySelector<HTMLElement>(
      '[data-column-state="cancelled"]',
    ) as HTMLElement;
    expect(cancelledColumn).not.toBeNull();
    expect(within(cancelledColumn).getByText(/No runs/)).toBeInTheDocument();
  });

  it("every card is a link into the existing /runs/:id RunView", async () => {
    mockListRuns.mockResolvedValue({ items: BOARD_RUNS });
    renderBoard();
    await screen.findByRole("heading", { name: /^created/, level: 2 });
    for (const run of BOARD_RUNS) {
      const link = screen.getByRole("link", {
        name: new RegExp(`^run ${run.id}, ${run.state}`),
      });
      expect(link).toHaveAttribute("href", `/runs/${run.id}`);
    }
  });
});

describe("RunsBoard time-range filter (server-side, issue #23)", () => {
  it("states the shared ordering contract and renders the shared filter control", async () => {
    mockListRuns.mockResolvedValue({ items: BOARD_RUNS });
    renderBoard();
    await screen.findByRole("heading", { name: /^created/, level: 2 });
    expect(
      screen.getByText(/newest first by last update/),
    ).toBeInTheDocument();
    expect(screen.getByRole("group", { name: "Time range" })).toBeInTheDocument();
  });

  it("loads an already-bookmarked since/until straight from the URL on first render", async () => {
    mockListRuns.mockResolvedValue({ items: BOARD_RUNS });
    renderBoard([
      "/board?since=2026-08-01T00%3A00%3A00.000Z&until=2026-08-02T00%3A00%3A00.000Z",
    ]);
    await screen.findByRole("heading", { name: /^created/, level: 2 });

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
    renderBoard();
    await screen.findByRole("heading", { name: /^created/, level: 2 });
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
    renderBoard();
    await screen.findByRole("heading", { name: /^created/, level: 2 });
    mockListRuns.mockClear();

    const kept = BOARD_RUNS.filter((run) => run.state === "running");
    mockListRuns.mockResolvedValue({ items: kept });
    await user.click(screen.getByRole("button", { name: "Last 24h" }));

    await waitFor(() => expect(mockListRuns).toHaveBeenCalledTimes(1));
    // The board now reflects the *new* response's runs, proving the range
    // change drove a real fetch rather than filtering the old list in place.
    const createdRun = BOARD_RUNS.find((run) => run.state === "created")!;
    await waitFor(() =>
      expect(
        screen.queryByText(`${createdRun.id.slice(0, 20)}…`),
      ).not.toBeInTheDocument(),
    );
    expect(
      screen.getByText(`${kept[0].id.slice(0, 20)}…`),
    ).toBeInTheDocument();
  });
});
