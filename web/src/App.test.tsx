import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import App, { titleForPath } from "./App";
import { resetAgentState } from "./agent-state/store";
import { resetSharedEventsForTests } from "./hooks/useSharedEvents";

/**
 * Route-specific document titles (task t27). Before this every view shared
 * one static title, so a reader with eight tabs open could not tell the Runs
 * list from a run, or one run from another.
 */

vi.mock("./api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api/client")>();
  const pending = () => new Promise(() => {});
  return {
    ...actual,
    listRuns: vi.fn(pending),
    listNodeRuns: vi.fn(pending),
    listWorkflows: vi.fn(pending),
    listHumanTasks: vi.fn(pending),
    listPendingDecisions: vi.fn(pending),
    listActors: vi.fn(pending),
    listPlanImports: vi.fn(pending),
    getVersion: vi.fn(pending),
    getWhoami: vi.fn(pending),
    getTicket: vi.fn(pending),
    getRun: vi.fn(pending),
    getLedger: vi.fn(pending),
    getWorkflow: vi.fn(pending),
  };
});

class NoopEventSource {
  close() {}
  addEventListener() {}
}

beforeEach(() => {
  resetAgentState();
  resetSharedEventsForTests();
  vi.stubGlobal("EventSource", NoopEventSource);
  document.title = "";
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("titleForPath", () => {
  it.each([
    ["/runs", "Runs · Culture Nodes"],
    ["/board", "Board · Culture Nodes"],
    ["/jobs", "Jobs · Culture Nodes"],
    ["/inbox", "Inbox · Culture Nodes"],
    ["/decisions", "Decisions · Culture Nodes"],
    ["/mesh", "Mesh · Culture Nodes"],
    ["/stats", "Statistics · Culture Nodes"],
    ["/graphs", "Node Graphs · Culture Nodes"],
    ["/plan", "Plan · Culture Nodes"],
    ["/workflows/new", "New workflow · Culture Nodes"],
    ["/workflows/generate", "Generate workflow · Culture Nodes"],
  ])("names %s as %s", (path, expected) => {
    expect(titleForPath(path)).toBe(expected);
  });

  it("names the subject of a detail page, not just its kind", () => {
    expect(titleForPath("/runs/01M0RUN")).toBe("Run 01M0RUN · Culture Nodes");
    expect(titleForPath("/runs/01M0RUN/ledger")).toBe(
      "Ledger 01M0RUN · Culture Nodes",
    );
    expect(titleForPath("/tickets/SCRUM-6")).toBe(
      "Ticket SCRUM-6 · Culture Nodes",
    );
    expect(titleForPath("/plan/economy-discord-graphs")).toBe(
      "Plan economy-discord-graphs · Culture Nodes",
    );
  });

  it("decodes a percent-encoded subject rather than showing the escape", () => {
    expect(titleForPath("/tickets/A%2FB")).toBe("Ticket A/B · Culture Nodes");
  });

  it("prefers the longest matching prefix, so a sub-route is not titled as its parent", () => {
    expect(titleForPath("/workflows/new")).toBe("New workflow · Culture Nodes");
    expect(titleForPath("/graphs?tab=active")).not.toBe("Culture Nodes");
  });

  it("names the app alone at the root and says Not found off the map", () => {
    expect(titleForPath("/")).toBe("Culture Nodes");
    expect(titleForPath("/nowhere")).toBe("Not found · Culture Nodes");
  });
});

describe("RouteWatcher wiring", () => {
  it("sets document.title from the route the app rendered", async () => {
    render(
      <MemoryRouter initialEntries={["/decisions"]}>
        <App />
      </MemoryRouter>,
    );
    await waitFor(() =>
      expect(document.title).toBe("Decisions · Culture Nodes"),
    );
  });

  it("sets a run's own title on a run detail route", async () => {
    render(
      <MemoryRouter initialEntries={["/runs/01M0RUNTITLE"]}>
        <App />
      </MemoryRouter>,
    );
    await waitFor(() =>
      expect(document.title).toBe("Run 01M0RUNTITLE · Culture Nodes"),
    );
  });
});
