import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import App, { titleForPath } from "./App";
import { ApiError, getWhoami } from "./api/client";
import { resetAgentState } from "./agent-state/store";
import { resetSharedEventsForTests } from "./hooks/useSharedEvents";
import { resetWhoamiForTests } from "./hooks/useWhoami";
import { WHOAMI_BOUND } from "./fixtures/whoami-fixture";

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
  resetWhoamiForTests();
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

/**
 * What `/` resolves to (task t17). Only a 401 — "no Access identity reached
 * the control plane" — is a fact about who is here; every other whoami
 * failure is a fact about the control plane, and bouncing the reader to the
 * run table on it both contradicts IdentityGate's rule and restarts the load
 * that `#agent-state` had already settled.
 */
describe("Landing", () => {
  it("keeps the landing page when whoami fails for any reason but a 401", async () => {
    vi.mocked(getWhoami).mockRejectedValueOnce(
      new ApiError(500, "Internal Server Error", "check the control plane"),
    );
    render(
      <MemoryRouter initialEntries={["/"]}>
        <App />
      </MemoryRouter>,
    );
    expect(
      await screen.findByRole("heading", { name: /waiting on a person/i }),
    ).toBeInTheDocument();
    await waitFor(() => expect(document.title).toBe("Culture Nodes"));
  });

  it("sends a reader with no Access identity to the run table", async () => {
    vi.mocked(getWhoami).mockRejectedValueOnce(
      new ApiError(401, "Unauthorized", "sign in through Cloudflare Access"),
    );
    render(
      <MemoryRouter initialEntries={["/"]}>
        <App />
      </MemoryRouter>,
    );
    await waitFor(() => expect(document.title).toBe("Runs · Culture Nodes"));
  });

  it("keeps a signed-in person on the landing page", async () => {
    vi.mocked(getWhoami).mockResolvedValueOnce(WHOAMI_BOUND);
    render(
      <MemoryRouter initialEntries={["/"]}>
        <App />
      </MemoryRouter>,
    );
    expect(
      await screen.findByRole("heading", { name: /waiting on a person/i }),
    ).toBeInTheDocument();
  });
});
