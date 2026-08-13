import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import NodeGraphs from "./NodeGraphs";
import { ApiError, listRuns, listWorkflows } from "../api/client";
import {
  DELIVER_CHANGE_V2_DIGEST,
  HELLO_WORLD_DIGEST,
  WORKFLOW_VERSIONS,
  WORKFLOWS_RUNS,
} from "../fixtures/workflows-fixture";
import { WORKFLOW_DIGEST } from "../fixtures/run-fixture";
import { getAgentState, resetAgentState } from "../agent-state/store";
import { resetSharedEventsForTests } from "../hooks/useSharedEvents";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return { ...actual, listWorkflows: vi.fn(), listRuns: vi.fn() };
});

const mockListWorkflows = vi.mocked(listWorkflows);
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

function renderNodeGraphs(initialEntries: string[] = ["/graphs"]) {
  return render(
    <MemoryRouter initialEntries={initialEntries}>
      <NodeGraphs />
      <LocationProbe />
    </MemoryRouter>,
  );
}

function resolveFixture() {
  mockListWorkflows.mockResolvedValue({ items: WORKFLOW_VERSIONS });
  mockListRuns.mockResolvedValue({ items: WORKFLOWS_RUNS });
}

beforeEach(() => {
  mockListWorkflows.mockReset();
  mockListRuns.mockReset();
  resetAgentState();
});

describe("Node Graphs sub-tabs (task t28)", () => {
  it("defaults to the Nodes sub-tab when the URL carries no ?tab param", () => {
    renderNodeGraphs();
    expect(
      screen.getByRole("button", { name: "Nodes" }),
    ).toHaveAttribute("aria-pressed", "true");
    expect(
      screen.getByRole("button", { name: "Node Graphs" }),
    ).toHaveAttribute("aria-pressed", "false");
    expect(
      screen.getByRole("button", { name: "Active Graphs" }),
    ).toHaveAttribute("aria-pressed", "false");
    expect(document.getElementById("node-graphs-nodes-empty")).toBeInTheDocument();
  });

  it("selects the sub-tab named by ?tab= on first render", () => {
    renderNodeGraphs(["/graphs?tab=active"]);
    expect(
      screen.getByRole("button", { name: "Active Graphs" }),
    ).toHaveAttribute("aria-pressed", "true");
    expect(document.getElementById("node-graphs-active-empty")).toBeInTheDocument();
  });

  it("falls back to the Nodes sub-tab for an unrecognized ?tab= value", () => {
    renderNodeGraphs(["/graphs?tab=bogus"]);
    expect(
      screen.getByRole("button", { name: "Nodes" }),
    ).toHaveAttribute("aria-pressed", "true");
  });

  it("clicking a sub-tab button updates the URL's ?tab= param (bookmarkable)", async () => {
    const user = userEvent.setup();
    renderNodeGraphs();
    expect(screen.getByTestId("location-search")).toHaveTextContent("");

    await user.click(screen.getByRole("button", { name: "Active Graphs" }));
    expect(screen.getByTestId("location-search")).toHaveTextContent(
      "?tab=active",
    );

    await user.click(screen.getByRole("button", { name: "Nodes" }));
    // The default tab clears the param rather than writing "?tab=nodes".
    expect(screen.getByTestId("location-search")).toHaveTextContent("");
  });

  it("switching to the Node Graphs sub-tab fetches and renders the workflow cards", async () => {
    resolveFixture();
    const user = userEvent.setup();
    renderNodeGraphs();
    await user.click(screen.getByRole("button", { name: "Node Graphs" }));
    await screen.findByText("deliver-change");
    expect(mockListWorkflows).toHaveBeenCalledTimes(1);
  });
});

describe("Nodes sub-tab honest empty state (h14)", () => {
  it("renders an honest empty state, never fabricated node rows, and reports ready immediately", () => {
    renderNodeGraphs(["/graphs?tab=nodes"]);
    const empty = document.getElementById("node-graphs-nodes-empty");
    expect(empty).toBeInTheDocument();
    expect(empty).toHaveTextContent(/No node catalog yet/);
    expect(getAgentState().status).toBe("ready");
  });
});

describe("Active Graphs sub-tab honest empty state (h14)", () => {
  it("renders an honest empty state, never fabricated activity, and reports ready immediately", () => {
    renderNodeGraphs(["/graphs?tab=active"]);
    const empty = document.getElementById("node-graphs-active-empty");
    expect(empty).toBeInTheDocument();
    expect(empty).toHaveTextContent(/No active-graph view yet/);
    expect(getAgentState().status).toBe("ready");
  });
});

describe("Node Graphs sub-tab loading/empty/error", () => {
  it("shows a loading state before both requests resolve", () => {
    mockListWorkflows.mockReturnValue(new Promise(() => {}));
    mockListRuns.mockReturnValue(new Promise(() => {}));
    renderNodeGraphs(["/graphs?tab=graphs"]);
    expect(screen.getByText("Loading workflows…")).toBeInTheDocument();
  });

  it("shows the empty state when no workflow has been published", async () => {
    mockListWorkflows.mockResolvedValue({ items: [] });
    mockListRuns.mockResolvedValue({ items: [] });
    renderNodeGraphs(["/graphs?tab=graphs"]);
    expect(
      await screen.findByText(/No workflows published yet\./),
    ).toBeInTheDocument();
  });

  it("renders an error notice and stops loading when the workflows request fails", async () => {
    mockListWorkflows.mockRejectedValue(
      new ApiError(0, "cannot reach the control plane", "start `nodes serve`"),
    );
    mockListRuns.mockResolvedValue({ items: [] });
    renderNodeGraphs(["/graphs?tab=graphs"]);
    await screen.findByText("error:", { exact: false });
    expect(
      screen.getByText("cannot reach the control plane", { exact: false }),
    ).toBeInTheDocument();
  });

  it("renders an error notice when the runs request fails, even if workflows succeeded", async () => {
    mockListWorkflows.mockResolvedValue({ items: WORKFLOW_VERSIONS });
    mockListRuns.mockRejectedValue(
      new ApiError(0, "cannot reach the control plane", "start `nodes serve`"),
    );
    renderNodeGraphs(["/graphs?tab=graphs"]);
    await screen.findByText("error:", { exact: false });
  });

  it("marks agent-state ready once both requests settle, loading otherwise", async () => {
    resolveFixture();
    renderNodeGraphs(["/graphs?tab=graphs"]);
    expect(getAgentState().status).toBe("loading");
    await screen.findByText("deliver-change");
    expect(getAgentState().status).toBe("ready");
  });
});

describe("Node Graphs sub-tab data fetch", () => {
  it("requests every published workflow version and every run sorted by updated_at", async () => {
    resolveFixture();
    renderNodeGraphs(["/graphs?tab=graphs"]);
    await screen.findByText("deliver-change");
    expect(mockListWorkflows).toHaveBeenCalledTimes(1);
    expect(mockListRuns).toHaveBeenCalledTimes(1);
    const [, runParams] = mockListRuns.mock.calls[0];
    expect(runParams).toEqual({ sort: "updated_at" });
  });
});

describe("Node Graphs sub-tab grouping and rendering", () => {
  it("renders one card per workflow_key, not one per version", async () => {
    resolveFixture();
    renderNodeGraphs(["/graphs?tab=graphs"]);
    await screen.findByText("deliver-change");
    expect(
      document.querySelectorAll('[data-workflow-key="deliver-change"]'),
    ).toHaveLength(1);
    expect(
      document.querySelectorAll('[data-workflow-key="hello-world"]'),
    ).toHaveLength(1);
  });

  it("lists every version of a workflow with its own digest, newest version first", async () => {
    resolveFixture();
    renderNodeGraphs(["/graphs?tab=graphs"]);
    await screen.findByText("deliver-change");
    const card = document.querySelector(
      '[data-workflow-key="deliver-change"]',
    ) as HTMLElement;
    const rows = within(card).getAllByRole("row").slice(1); // drop header row
    expect(rows).toHaveLength(2);
    expect(within(card).getByText(String(2))).toBeInTheDocument();
    expect(
      card.querySelector(`[data-workflow-digest="${DELIVER_CHANGE_V2_DIGEST}"]`),
    ).toBeInTheDocument();
    expect(
      card.querySelector(`[data-workflow-digest="${WORKFLOW_DIGEST}"]`),
    ).toBeInTheDocument();
  });

  it("shows the owner from the latest version's metadata", async () => {
    resolveFixture();
    renderNodeGraphs(["/graphs?tab=graphs"]);
    await screen.findByText("deliver-change");
    const card = document.querySelector(
      '[data-workflow-key="deliver-change"]',
    ) as HTMLElement;
    expect(within(card).getByText("team/platform-ai")).toBeInTheDocument();
  });

  it("lists each workflow's recent runs across all of its versions, newest first, never a run belonging to another workflow", async () => {
    resolveFixture();
    renderNodeGraphs(["/graphs?tab=graphs"]);
    await screen.findByText("deliver-change");
    const card = document.querySelector(
      '[data-workflow-key="deliver-change"]',
    ) as HTMLElement;
    const runLinks = within(card)
      .getAllByRole("link")
      .filter((link) => link.getAttribute("href")?.startsWith("/runs/"));
    expect(runLinks.map((link) => link.textContent)).toEqual([
      "run-deliver-v2-01J8XKWORKFLOWS02",
      "run-deliver-v1-01J8XKWORKFLOWS04",
    ]);
  });

  it("never renders a run whose digest matches no published version", async () => {
    resolveFixture();
    renderNodeGraphs(["/graphs?tab=graphs"]);
    await screen.findByText("deliver-change");
    expect(
      screen.queryByText("run-orphan-01J8XKWORKFLOWS0003"),
    ).not.toBeInTheDocument();
  });

  it("shows a workflow's own empty state when it has no recent runs", async () => {
    mockListWorkflows.mockResolvedValue({ items: WORKFLOW_VERSIONS });
    mockListRuns.mockResolvedValue({ items: [] });
    renderNodeGraphs(["/graphs?tab=graphs"]);
    await screen.findByText("hello-world");
    const card = document.querySelector(
      '[data-workflow-key="hello-world"]',
    ) as HTMLElement;
    expect(within(card).getByText(/No runs yet/)).toBeInTheDocument();
  });

  it("every recent-run row is a real link into the existing Run view", async () => {
    resolveFixture();
    renderNodeGraphs(["/graphs?tab=graphs"]);
    await screen.findByText("hello-world");
    const helloRun = WORKFLOWS_RUNS.find(
      (run) => run.workflow_digest === HELLO_WORLD_DIGEST,
    )!;
    const link = screen.getByRole("link", { name: new RegExp(helloRun.id) });
    expect(link).toHaveAttribute("href", `/runs/${helloRun.id}`);
  });
});

describe("Node Graphs authoring entry point (task t28)", () => {
  it("links to /workflows/new from every sub-tab", () => {
    renderNodeGraphs(["/graphs?tab=active"]);
    expect(
      screen.getByRole("link", { name: "New workflow" }),
    ).toHaveAttribute("href", "/workflows/new");
  });
});

describe("Node Graphs panel auto-refresh (issue #46, task t30)", () => {
  beforeEach(() => {
    resetSharedEventsForTests();
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
  });

  afterEach(() => {
    resetSharedEventsForTests();
    vi.unstubAllGlobals();
  });

  it("refetches on a run-lifecycle event, staying stale-while-revalidate: no loading regression, no nulled cards", async () => {
    resolveFixture();
    renderNodeGraphs(["/graphs?tab=graphs"]);
    await screen.findByText("hello-world");
    await waitFor(() => expect(getAgentState().status).toBe("ready"));

    const source = FakeEventSource.instances[0];
    act(() => source.open());

    let resolveWorkflows: ((value: { items: typeof WORKFLOW_VERSIONS }) => void) | undefined;
    mockListWorkflows.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveWorkflows = resolve;
        }),
    );

    act(() => {
      source.emit("dev.culture.nodes.run.completed", { run_id: "some-run" }, "01EVT1");
    });

    await waitFor(() => expect(mockListWorkflows).toHaveBeenCalledTimes(2));

    // The reload fetch is in flight — the original cards and agent-state
    // must still be exactly as they were (stale-while-revalidate).
    expect(screen.getByText("hello-world")).toBeInTheDocument();
    expect(screen.queryByText("Loading workflows…")).not.toBeInTheDocument();
    expect(getAgentState().status).toBe("ready");

    await act(async () => {
      resolveWorkflows?.({ items: WORKFLOW_VERSIONS });
    });
    expect(getAgentState().status).toBe("ready");
  });

  it("debounces a burst of simultaneous events into a single refetch", async () => {
    resolveFixture();
    renderNodeGraphs(["/graphs?tab=graphs"]);
    await screen.findByText("hello-world");

    const source = FakeEventSource.instances[0];
    act(() => source.open());
    mockListWorkflows.mockClear();
    mockListWorkflows.mockResolvedValue({ items: WORKFLOW_VERSIONS });

    act(() => {
      source.emit("dev.culture.nodes.run.completed", { run_id: "a" }, "01EVT1");
      source.emit("dev.culture.nodes.run.failed", { run_id: "b" }, "01EVT2");
      source.emit("dev.culture.nodes.run.cancelled", { run_id: "c" }, "01EVT3");
    });

    await waitFor(() => expect(mockListWorkflows).toHaveBeenCalledTimes(1));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(mockListWorkflows).toHaveBeenCalledTimes(1);
  });

  it("ignores an event type this view did not subscribe to", async () => {
    resolveFixture();
    renderNodeGraphs(["/graphs?tab=graphs"]);
    await screen.findByText("hello-world");

    const source = FakeEventSource.instances[0];
    act(() => source.open());
    mockListWorkflows.mockClear();

    act(() => {
      source.emit("dev.culture.nodes.attempt.started", { run_id: "a" }, "01EVT1");
    });
    await new Promise((resolve) => setTimeout(resolve, 20));

    expect(mockListWorkflows).not.toHaveBeenCalled();
  });

  it("detaches the subscription when switching away from the Node Graphs sub-tab (no reload after leaving)", async () => {
    resolveFixture();
    const { rerender } = renderNodeGraphs(["/graphs?tab=graphs"]);
    await screen.findByText("hello-world");

    const source = FakeEventSource.instances[0];
    act(() => source.open());

    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Nodes" }));
    expect(screen.queryByText("hello-world")).not.toBeInTheDocument();

    mockListWorkflows.mockClear();
    act(() => {
      source.emit("dev.culture.nodes.run.completed", { run_id: "a" }, "01EVT1");
    });
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(mockListWorkflows).not.toHaveBeenCalled();
    void rerender;
  });
});
