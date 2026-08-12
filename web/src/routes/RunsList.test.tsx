import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import RunsList from "./RunsList";
import { ApiError, listRuns } from "../api/client";
import { BOARD_RUNS } from "../fixtures/runs-board-fixture";
import { resetAgentState } from "../agent-state/store";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return { ...actual, listRuns: vi.fn() };
});

const mockListRuns = vi.mocked(listRuns);

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
