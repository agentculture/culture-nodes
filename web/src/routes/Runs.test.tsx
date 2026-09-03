import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import Runs, { parseRunsView } from "./Runs";
import { listNodeRuns, listRuns } from "../api/client";
import { BOARD_RUNS } from "../fixtures/runs-board-fixture";
import { JOB_RUNS_PAGE_1 } from "../fixtures/node-runs-fixture";
import { resetAgentState } from "../agent-state/store";
import { resetSharedEventsForTests } from "../hooks/useSharedEvents";

/**
 * One page, three projections (task t9). List, board and jobs were three of
 * the twelve nav destinations reading one dataset; they are `?view=` on
 * `/runs` now, so the projection is bookmarkable and `/board` and `/jobs`
 * have somewhere to redirect to (App.test.tsx walks those).
 */

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return { ...actual, listRuns: vi.fn(), listNodeRuns: vi.fn() };
});

const mockListRuns = vi.mocked(listRuns);
const mockListNodeRuns = vi.mocked(listNodeRuns);

class NoopEventSource {
  close() {}
  addEventListener() {}
}

function LocationProbe() {
  const location = useLocation();
  return <span data-testid="location">{location.search}</span>;
}

function renderRuns(initialEntries: string[] = ["/runs"]) {
  return render(
    <MemoryRouter initialEntries={initialEntries}>
      <Runs />
      <LocationProbe />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mockListRuns.mockReset();
  mockListNodeRuns.mockReset();
  mockListRuns.mockResolvedValue({ items: BOARD_RUNS });
  mockListNodeRuns.mockResolvedValue({ items: JOB_RUNS_PAGE_1 });
  resetAgentState();
  resetSharedEventsForTests();
  vi.stubGlobal("EventSource", NoopEventSource);
});

describe("parseRunsView", () => {
  it.each([
    ["list", "list"],
    ["board", "board"],
    ["jobs", "jobs"],
  ])("reads ?view=%s as the %s projection", (raw, expected) => {
    expect(parseRunsView(raw)).toBe(expected);
  });

  it("falls back to the run table for an absent or unknown view rather than erroring", () => {
    expect(parseRunsView(null)).toBe("list");
    expect(parseRunsView("")).toBe("list");
    expect(parseRunsView("kanban")).toBe("list");
  });
});

describe("Runs projections", () => {
  it("renders one Runs heading for the page, not one per projection", async () => {
    renderRuns();
    const headings = await screen.findAllByRole("heading", {
      name: "Runs",
      level: 1,
    });
    expect(headings).toHaveLength(1);
  });

  it("shows the run table by default", async () => {
    renderRuns();
    await waitFor(() =>
      expect(document.getElementById("runs-table")).toBeInTheDocument(),
    );
    expect(document.getElementById("runs-board-columns")).toBeNull();
  });

  it("shows the board for ?view=board, and only the board", async () => {
    renderRuns(["/runs?view=board"]);
    await waitFor(() =>
      expect(document.getElementById("runs-board-columns")).toBeInTheDocument(),
    );
    expect(document.getElementById("runs-table")).toBeNull();
    expect(document.getElementById("jobs-table")).toBeNull();
  });

  it("shows the jobs timeline for ?view=jobs, and only it", async () => {
    renderRuns(["/runs?view=jobs"]);
    await waitFor(() =>
      expect(document.getElementById("jobs-table")).toBeInTheDocument(),
    );
    expect(document.getElementById("runs-table")).toBeNull();
    expect(document.getElementById("runs-board-columns")).toBeNull();
  });
});

describe("Runs projection toggle", () => {
  it("marks the current projection pressed and the other two not", async () => {
    renderRuns(["/runs?view=jobs"]);
    const toggle = document.getElementById("runs-toggle");
    expect(toggle).toHaveAttribute("role", "group");
    expect(document.getElementById("runs-toggle-jobs")).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    for (const key of ["list", "board"]) {
      expect(document.getElementById(`runs-toggle-${key}`)).toHaveAttribute(
        "aria-pressed",
        "false",
      );
    }
  });

  it("puts the chosen projection in the URL, so it can be bookmarked and shared", async () => {
    const user = userEvent.setup();
    renderRuns();
    await user.click(screen.getByRole("button", { name: "Board" }));
    expect(screen.getByTestId("location")).toHaveTextContent("view=board");
    await waitFor(() =>
      expect(document.getElementById("runs-board-columns")).toBeInTheDocument(),
    );
  });

  it("writes the default projection as the absence of ?view, not as a second URL for one page", async () => {
    const user = userEvent.setup();
    renderRuns(["/runs?view=board"]);
    await user.click(screen.getByRole("button", { name: "List" }));
    expect(screen.getByTestId("location").textContent).toBe("");
  });

  it("keeps the bookmarked time range when the projection changes", async () => {
    const user = userEvent.setup();
    renderRuns([
      "/runs?since=2026-08-01T00%3A00%3A00.000Z&until=2026-08-02T00%3A00%3A00.000Z",
    ]);
    await user.click(screen.getByRole("button", { name: "Jobs" }));
    await waitFor(() =>
      expect(document.getElementById("jobs-table")).toBeInTheDocument(),
    );
    const search = screen.getByTestId("location").textContent ?? "";
    expect(search).toContain("view=jobs");
    expect(search).toContain("since=2026-08-01T00%3A00%3A00.000Z");
    expect(search).toContain("until=2026-08-02T00%3A00%3A00.000Z");
  });
});
