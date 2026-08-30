import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ReactNode } from "react";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import RunView from "./RunView";
import { useRunData, type RunData } from "./useRunData";
import { useRunEvents } from "../hooks/useRunEvents";
import type { StreamStatus } from "../hooks/useRunEvents";
import { parseWorkflowGraph } from "../domain/graph";
import { RUN_ID, RUN_VIEW, WORKFLOW_IR } from "../fixtures/run-fixture";
import { resetAgentState } from "../agent-state/store";

/**
 * The Run view's head: the parts task t27 touched (the run id printed once,
 * and the `stream:` chip explaining itself).
 *
 * React Flow is stubbed and the two data hooks are mocked, the same
 * presentational-layer discipline ActiveGraphCanvas.test.tsx follows — these
 * assertions are about what the head renders from a given run, not about
 * canvas internals or fetch plumbing.
 */

vi.mock("@xyflow/react", () => ({
  ReactFlow: ({ children }: { children?: ReactNode }) => (
    <div data-testid="react-flow-stub">{children}</div>
  ),
  ReactFlowProvider: ({ children }: { children?: ReactNode }) => <>{children}</>,
  Background: () => null,
  Controls: () => null,
  MarkerType: { ArrowClosed: "arrowclosed" },
}));

vi.mock("./useRunData", () => ({ useRunData: vi.fn() }));
vi.mock("../hooks/useRunEvents", () => ({ useRunEvents: vi.fn() }));

const mockUseRunData = vi.mocked(useRunData) as unknown as {
  mockReturnValue: (value: RunData) => void;
};
const mockUseRunEvents = vi.mocked(useRunEvents) as unknown as {
  mockReturnValue: (value: {
    events: [];
    status: StreamStatus;
    lastEventId: string | null;
  }) => void;
};

const GRAPH = parseWorkflowGraph(WORKFLOW_IR);

function runData(overrides: Partial<RunData> = {}): RunData {
  return {
    view: RUN_VIEW,
    graph: GRAPH,
    ledger: [],
    usageByNodeRunId: {},
    loading: false,
    error: null,
    ...overrides,
  };
}

function renderRunView() {
  return render(
    <MemoryRouter initialEntries={[`/runs/${RUN_ID}`]}>
      <Routes>
        <Route path="/runs/:id" element={<RunView />} />
      </Routes>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  resetAgentState();
  mockUseRunEvents.mockReturnValue({
    events: [],
    status: "open",
    lastEventId: null,
  });
  mockUseRunData.mockReturnValue(runData());
});

afterEach(() => {
  vi.clearAllMocks();
});

describe("RunView run identity (task t27)", () => {
  it("prints a named run's name alongside the id, not instead of it", async () => {
    renderRunView();
    await waitFor(() =>
      expect(
        screen.getByText(RUN_VIEW.run.name as string),
      ).toBeInTheDocument(),
    );
    expect(screen.getAllByText(RUN_ID)).toHaveLength(1);
  });

  it("prints the run id exactly once when the run has no name and no display hint", async () => {
    mockUseRunData.mockReturnValue(
      runData({
        view: {
          ...RUN_VIEW,
          run: { ...RUN_VIEW.run, name: undefined, display_hint: undefined },
        },
      }),
    );
    renderRunView();

    // Before t27 the id rendered twice: once as the title's subject and once
    // as `runDisplayName`'s fallback in the name line under it, which read as
    // two facts about the run rather than one.
    await waitFor(() => expect(screen.getAllByText(RUN_ID)).toHaveLength(1));
    expect(document.getElementById("run-view-name")).toBeNull();
  });

  it("still shows a derived display hint, which is not the id", async () => {
    mockUseRunData.mockReturnValue(
      runData({
        view: {
          ...RUN_VIEW,
          run: {
            ...RUN_VIEW.run,
            name: undefined,
            display_hint: "add the ledger projection endpoint",
          },
        },
      }),
    );
    renderRunView();

    const name = await screen.findByText("add the ledger projection endpoint");
    expect(name).toHaveAttribute("data-derived", "true");
    expect(screen.getAllByText(RUN_ID)).toHaveLength(1);
  });
});

describe("RunView stream status chip (task t27)", () => {
  it("explains a closed stream: the page is stale, the run is not broken", async () => {
    mockUseRunEvents.mockReturnValue({
      events: [],
      status: "closed",
      lastEventId: null,
    });
    renderRunView();

    const chip = document.getElementById("stream-status");
    await waitFor(() => expect(chip).toHaveAttribute("data-stream-status", "closed"));
    expect(chip?.getAttribute("title")).toMatch(/live event stream has ended/);
    expect(chip?.getAttribute("title")).toMatch(/run itself is unaffected/);
    // The explanation is also in the accessible name, not tooltip-only: a
    // `title` is unreachable by keyboard and by most screen readers.
    expect(chip?.textContent).toMatch(/live event stream has ended/);
  });

  it("explains an open stream as live", async () => {
    renderRunView();
    const chip = document.getElementById("stream-status");
    await waitFor(() => expect(chip).toHaveAttribute("data-stream-status", "open"));
    expect(chip?.getAttribute("title")).toMatch(/live/);
  });

  it("has an explanation for every stream status the hook can report", async () => {
    for (const status of ["connecting", "open", "closed", "error"] as const) {
      mockUseRunEvents.mockReturnValue({ events: [], status, lastEventId: null });
      const { unmount } = renderRunView();
      const chip = document.getElementById("stream-status");
      await waitFor(() =>
        expect(chip).toHaveAttribute("data-stream-status", status),
      );
      expect(chip?.getAttribute("title") ?? "").not.toBe("");
      unmount();
    }
  });
});
