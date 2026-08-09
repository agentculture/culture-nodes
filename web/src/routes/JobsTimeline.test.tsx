import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import JobsTimeline from "./JobsTimeline";
import { ApiError, listNodeRuns } from "../api/client";
import {
  JOB_RUNS_CURSOR,
  JOB_RUNS_PAGE_1,
  JOB_RUNS_PAGE_2,
} from "../fixtures/node-runs-fixture";
import { getAgentState, resetAgentState } from "../agent-state/store";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return { ...actual, listNodeRuns: vi.fn() };
});

const mockListNodeRuns = vi.mocked(listNodeRuns);

function LocationProbe() {
  const location = useLocation();
  return <div data-testid="location-search">{location.search}</div>;
}

function renderJobs(initialEntries: string[] = ["/jobs"]) {
  return render(
    <MemoryRouter initialEntries={initialEntries}>
      <JobsTimeline />
      <LocationProbe />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mockListNodeRuns.mockReset();
  resetAgentState();
});

describe("JobsTimeline loading/empty/error", () => {
  it("shows a loading state before the first response resolves", () => {
    mockListNodeRuns.mockReturnValue(new Promise(() => {})); // never resolves
    renderJobs();
    expect(screen.getByText("Loading node runs…")).toBeInTheDocument();
  });

  it("shows the empty state when the API reports no node runs", async () => {
    mockListNodeRuns.mockResolvedValue({ items: [] });
    renderJobs();
    expect(await screen.findByText("No node runs in this range.")).toBeInTheDocument();
  });

  it("renders an error notice and stops loading when the request fails", async () => {
    mockListNodeRuns.mockRejectedValue(
      new ApiError(0, "cannot reach the control plane", "start `nodes serve`"),
    );
    renderJobs();
    await screen.findByText("error:", { exact: false });
    expect(
      screen.getByText("cannot reach the control plane", { exact: false }),
    ).toBeInTheDocument();
    expect(
      screen.getByText("start `nodes serve`", { exact: false }),
    ).toBeInTheDocument();
  });

  it("marks agent-state ready once the initial load settles, loading otherwise", async () => {
    mockListNodeRuns.mockResolvedValue({ items: JOB_RUNS_PAGE_1 });
    renderJobs();
    expect(getAgentState().status).toBe("loading");
    await screen.findByRole("table");
    expect(getAgentState().status).toBe("ready");
  });
});

describe("JobsTimeline data + table", () => {
  it("fetches with no time bound by default, and renders one row per node run", async () => {
    mockListNodeRuns.mockResolvedValue({ items: JOB_RUNS_PAGE_1 });
    renderJobs();
    await screen.findByRole("table");

    expect(mockListNodeRuns).toHaveBeenCalledTimes(1);
    const [, params] = mockListNodeRuns.mock.calls[0];
    expect(params).toEqual({ updated_since: undefined, updated_until: undefined });

    for (const item of JOB_RUNS_PAGE_1) {
      expect(screen.getByRole("link", { name: item.run_id })).toBeInTheDocument();
    }
  });

  it("renders the run column as a link into /runs/:id", async () => {
    mockListNodeRuns.mockResolvedValue({ items: JOB_RUNS_PAGE_1 });
    renderJobs();
    await screen.findByRole("table");
    const item = JOB_RUNS_PAGE_1[0];
    expect(screen.getByRole("link", { name: item.run_id })).toHaveAttribute(
      "href",
      `/runs/${item.run_id}`,
    );
  });
});

describe("JobsTimeline time-range filter (server-side)", () => {
  it("loads an already-bookmarked since/until straight from the URL on first render", async () => {
    mockListNodeRuns.mockResolvedValue({ items: JOB_RUNS_PAGE_1 });
    renderJobs([
      "/jobs?since=2026-08-01T00%3A00%3A00.000Z&until=2026-08-02T00%3A00%3A00.000Z",
    ]);
    await screen.findByRole("table");

    expect(mockListNodeRuns).toHaveBeenCalledTimes(1);
    const [, params] = mockListNodeRuns.mock.calls[0];
    expect(params).toEqual({
      updated_since: "2026-08-01T00:00:00.000Z",
      updated_until: "2026-08-02T00:00:00.000Z",
    });
  });

  it("selecting a preset updates the URL search params and refetches with updated_since carrying that value", async () => {
    mockListNodeRuns.mockResolvedValue({ items: JOB_RUNS_PAGE_1 });
    const user = userEvent.setup();
    renderJobs();
    await screen.findByRole("table");
    expect(mockListNodeRuns).toHaveBeenCalledTimes(1);

    await user.click(screen.getByRole("button", { name: "Last hour" }));

    await waitFor(() => expect(mockListNodeRuns).toHaveBeenCalledTimes(2));
    const [, params] = mockListNodeRuns.mock.calls[1];
    expect(params?.updated_since).toBeTruthy();
    expect(params?.updated_until).toBeUndefined();
    const since = params?.updated_since as string;

    // The URL search params carry the exact same value the request used —
    // this is what makes the active range shareable/bookmarkable.
    expect(screen.getByTestId("location-search")).toHaveTextContent(
      `since=${encodeURIComponent(since)}`,
    );
  });

  it("applying a custom since/until updates the URL and the outgoing request with both bounds", async () => {
    mockListNodeRuns.mockResolvedValue({ items: JOB_RUNS_PAGE_1 });
    const user = userEvent.setup();
    renderJobs();
    await screen.findByRole("table");

    await user.click(screen.getByRole("button", { name: "Custom" }));
    await user.type(screen.getByLabelText("Since"), "2026-08-01T09:00");
    await user.type(screen.getByLabelText("Until"), "2026-08-02T09:00");
    await user.click(screen.getByRole("button", { name: "Apply" }));

    await waitFor(() => expect(mockListNodeRuns).toHaveBeenCalledTimes(2));
    const [, params] = mockListNodeRuns.mock.calls[1];
    const expectedSince = new Date("2026-08-01T09:00").toISOString();
    const expectedUntil = new Date("2026-08-02T09:00").toISOString();
    expect(params).toEqual({
      updated_since: expectedSince,
      updated_until: expectedUntil,
    });

    const search = screen.getByTestId("location-search").textContent ?? "";
    expect(search).toContain(`since=${encodeURIComponent(expectedSince)}`);
    expect(search).toContain(`until=${encodeURIComponent(expectedUntil)}`);
  });

  it("never filters client-side: a change in range always produces a new request, not a re-slice of the existing items", async () => {
    mockListNodeRuns.mockResolvedValue({ items: JOB_RUNS_PAGE_1 });
    const user = userEvent.setup();
    renderJobs();
    await screen.findByRole("table");
    mockListNodeRuns.mockClear();

    mockListNodeRuns.mockResolvedValue({ items: JOB_RUNS_PAGE_2 });
    await user.click(screen.getByRole("button", { name: "Last 24h" }));

    await waitFor(() => expect(mockListNodeRuns).toHaveBeenCalledTimes(1));
    // The table now reflects the *new* response's rows, proving the range
    // change drove a real fetch rather than filtering the old page in place.
    await screen.findByRole("link", { name: JOB_RUNS_PAGE_2[0].run_id });
    expect(
      screen.queryByRole("link", { name: JOB_RUNS_PAGE_1[0].run_id }),
    ).not.toBeInTheDocument();
  });
});

describe("JobsTimeline load more (cursor pagination)", () => {
  it("appends the next page and carries the cursor on the request", async () => {
    mockListNodeRuns.mockResolvedValueOnce({
      items: JOB_RUNS_PAGE_1,
      next_cursor: JOB_RUNS_CURSOR,
    });
    const user = userEvent.setup();
    renderJobs();
    await screen.findByRole("table");

    const loadMore = screen.getByRole("button", { name: "Load more" });
    mockListNodeRuns.mockResolvedValueOnce({ items: JOB_RUNS_PAGE_2 });
    await user.click(loadMore);

    await waitFor(() => expect(mockListNodeRuns).toHaveBeenCalledTimes(2));
    const [, params] = mockListNodeRuns.mock.calls[1];
    expect(params).toEqual({
      updated_since: undefined,
      updated_until: undefined,
      cursor: JOB_RUNS_CURSOR,
    });

    for (const item of [...JOB_RUNS_PAGE_1, ...JOB_RUNS_PAGE_2]) {
      expect(screen.getByRole("link", { name: item.run_id })).toBeInTheDocument();
    }
    // No further page: the button is gone rather than disabled forever.
    expect(
      screen.queryByRole("button", { name: /Load more/ }),
    ).not.toBeInTheDocument();
  });

  it("shows no Load more button when the first page has no next_cursor", async () => {
    mockListNodeRuns.mockResolvedValue({ items: JOB_RUNS_PAGE_1 });
    renderJobs();
    await screen.findByRole("table");
    expect(
      screen.queryByRole("button", { name: /Load more/ }),
    ).not.toBeInTheDocument();
  });
});
