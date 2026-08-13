import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import type { ComponentType } from "react";
import NodeGraphs from "./NodeGraphs";
import { ApiError, listNodeRuns, listRuns, listWorkflows } from "../api/client";
import {
  DELIVER_CHANGE_V2_DIGEST,
  HELLO_WORLD_DIGEST,
  NODE_CATALOG_DEFINITION_COUNT,
  NODE_CATALOG_WORKFLOW_VERSIONS,
  WORKFLOW_VERSIONS,
  WORKFLOWS_RUNS,
} from "../fixtures/workflows-fixture";
import {
  ACTIVE_NODE_ID,
  ACTIVE_NODE_RUNS,
  ACTIVE_RUN_ID,
  UNKNOWN_RUN_ID,
} from "../fixtures/active-graphs-fixture";
import { WORKFLOW_DIGEST } from "../fixtures/run-fixture";
import { getAgentState, resetAgentState } from "../agent-state/store";
import { resetSharedEventsForTests } from "../hooks/useSharedEvents";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    listWorkflows: vi.fn(),
    listRuns: vi.fn(),
    listNodeRuns: vi.fn(),
  };
});

// The Active Graphs canvas is exercised through its own contract in
// ActiveGraphCanvas.test.tsx; here React Flow is stubbed (jsdom renders the
// presentational layer, not a live canvas — the vitest setup convention),
// while still rendering every node through `nodeTypes` so presence markup
// stays assertable end to end.
vi.mock("@xyflow/react", () => ({
  ReactFlow: (props: {
    nodes: Array<{ id: string; type: string; data: Record<string, unknown> }>;
    nodeTypes: Record<string, ComponentType<{ id: string; data: unknown }>>;
    children?: React.ReactNode;
  }) => (
    <div data-testid="react-flow-stub">
      {props.nodes.map((node) => {
        const NodeType = props.nodeTypes[node.type];
        return <NodeType key={node.id} id={node.id} data={node.data} />;
      })}
      {props.children}
    </div>
  ),
  Background: () => null,
  Handle: () => null,
  Position: { Left: "left", Right: "right", Top: "top", Bottom: "bottom" },
  MarkerType: { ArrowClosed: "arrowclosed" },
}));

// ELK is a 1.4 MB bundle jsdom has no business executing; the fallback
// layout is deterministic and nothing here asserts positions.
vi.mock("../hooks/useElkLayout", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("../hooks/useElkLayout")>();
  return {
    ...actual,
    useElkLayout: () => ({ positions: {}, ready: false }),
  };
});

const mockListWorkflows = vi.mocked(listWorkflows);
const mockListRuns = vi.mocked(listRuns);
const mockListNodeRuns = vi.mocked(listNodeRuns);

type FakeListener = (event: { data: string; lastEventId: string }) => void;

/** The Mesh.test.tsx fake, for driving the shared cross-run stream. */
class FakeEventSource {
  static CONNECTING = 0 as const;
  static OPEN = 1 as const;
  static CLOSED = 2 as const;
  static instances: FakeEventSource[] = [];

  url: string;
  readyState = 0;
  onopen: (() => void) | null = null;
  onmessage: FakeListener | null = null;
  onerror: (() => void) | null = null;
  private listeners = new Map<string, FakeListener[]>();

  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, listener: FakeListener) {
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
      subject: data.run_id,
      time: "2026-08-13T00:00:00Z",
      datacontenttype: "application/json",
      data,
    };
    const event = { data: JSON.stringify(envelope), lastEventId: id };
    for (const listener of this.listeners.get(type) ?? []) listener(event);
  }
}

function mockMatchMedia(reduced: boolean) {
  window.matchMedia = vi.fn().mockImplementation((query: string) => ({
    matches: reduced && query.includes("prefers-reduced-motion"),
    media: query,
    onchange: null,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => false,
  }));
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
  mockListNodeRuns.mockResolvedValue({ items: ACTIVE_NODE_RUNS });
}

beforeEach(() => {
  mockListWorkflows.mockReset();
  mockListRuns.mockReset();
  mockListNodeRuns.mockReset();
  // Every panel now fetches on mount, so give each mock an honest default
  // ("nothing published/running yet"); tests override what they exercise.
  mockListWorkflows.mockResolvedValue({ items: [] });
  mockListRuns.mockResolvedValue({ items: [] });
  mockListNodeRuns.mockResolvedValue({ items: [] });
  resetAgentState();
  resetSharedEventsForTests();
  FakeEventSource.instances = [];
  vi.stubGlobal("EventSource", FakeEventSource);
  mockMatchMedia(false);
});

afterEach(() => {
  resetSharedEventsForTests();
  vi.unstubAllGlobals();
});

describe("Node Graphs sub-tabs (task t28)", () => {
  it("defaults to the Nodes sub-tab when the URL carries no ?tab param", async () => {
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
    expect(
      document.getElementById("node-graphs-nodes-panel"),
    ).toBeInTheDocument();
    await waitFor(() => expect(getAgentState().status).toBe("ready"));
  });

  it("selects the sub-tab named by ?tab= on first render", async () => {
    renderNodeGraphs(["/graphs?tab=active"]);
    expect(
      screen.getByRole("button", { name: "Active Graphs" }),
    ).toHaveAttribute("aria-pressed", "true");
    expect(
      document.getElementById("node-graphs-active-panel"),
    ).toBeInTheDocument();
    await waitFor(() => expect(getAgentState().status).toBe("ready"));
  });

  it("falls back to the Nodes sub-tab for an unrecognized ?tab= value", async () => {
    renderNodeGraphs(["/graphs?tab=bogus"]);
    expect(
      screen.getByRole("button", { name: "Nodes" }),
    ).toHaveAttribute("aria-pressed", "true");
    await waitFor(() => expect(getAgentState().status).toBe("ready"));
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
    await waitFor(() => expect(getAgentState().status).toBe("ready"));
  });

  it("switching to the Node Graphs sub-tab fetches and renders the workflow cards", async () => {
    resolveFixture();
    const user = userEvent.setup();
    renderNodeGraphs();
    await user.click(screen.getByRole("button", { name: "Node Graphs" }));
    await screen.findByText("deliver-change");
    // Twice: once by the default Nodes sub-tab's catalog on mount (task
    // t31), once by the workflow-cards panel after the switch.
    expect(mockListWorkflows).toHaveBeenCalledTimes(2);
  });
});

describe("Nodes sub-tab: the node-definition catalog (tasks t29+t31, c20)", () => {
  it("renders one card per distinct definition derived from published IRs, nothing else", async () => {
    mockListWorkflows.mockResolvedValue({
      items: NODE_CATALOG_WORKFLOW_VERSIONS,
    });
    renderNodeGraphs(["/graphs?tab=nodes"]);
    await waitFor(() => expect(getAgentState().status).toBe("ready"));
    expect(document.querySelectorAll(".node-def-card")).toHaveLength(
      NODE_CATALOG_DEFINITION_COUNT,
    );
  });

  it("shows kind, ref, and every cross-workflow occurrence on a shared-actor definition", async () => {
    mockListWorkflows.mockResolvedValue({
      items: NODE_CATALOG_WORKFLOW_VERSIONS,
    });
    renderNodeGraphs(["/graphs?tab=nodes"]);
    await waitFor(() => expect(getAgentState().status).toBe("ready"));
    const card = document.querySelector(
      '[data-definition-id="agent:actor://company/intake@sha256:111111"]',
    ) as HTMLElement;
    expect(card).toBeInTheDocument();
    expect(card).toHaveAttribute("data-node-kind", "agent");
    expect(within(card).getByText("agent")).toBeInTheDocument();
    expect(
      within(card).getByText("actor://company/intake@sha256:111111"),
    ).toBeInTheDocument();
    // deliver-change's intake node + notify-team's notify node — the
    // fixture's one deliberate cross-workflow coincidence.
    expect(
      card.querySelector('[data-occurrence="deliver-change@v2:intake"]'),
    ).toBeInTheDocument();
    expect(
      card.querySelector('[data-occurrence="notify-team@v1:notify"]'),
    ).toBeInTheDocument();
    expect(within(card).getByText("2 occurrences")).toBeInTheDocument();
  });

  it("says honestly when a definition has no ref instead of inventing an identity", async () => {
    mockListWorkflows.mockResolvedValue({
      items: NODE_CATALOG_WORKFLOW_VERSIONS,
    });
    renderNodeGraphs(["/graphs?tab=nodes"]);
    await waitFor(() => expect(getAgentState().status).toBe("ready"));
    const endCard = document.querySelector(
      '[data-definition-id="end"]',
    ) as HTMLElement;
    expect(
      within(endCard).getByText(/identity is the kind alone/),
    ).toBeInTheDocument();
  });

  it("renders an honest empty state when no workflow has been published (h14)", async () => {
    renderNodeGraphs(["/graphs?tab=nodes"]);
    expect(
      await screen.findByText(/No node definitions yet/),
    ).toBeInTheDocument();
    expect(document.querySelectorAll(".node-def-card")).toHaveLength(0);
    expect(getAgentState().status).toBe("ready");
  });

  it("renders an error notice when the workflows request fails, and still reports ready", async () => {
    mockListWorkflows.mockRejectedValue(
      new ApiError(0, "cannot reach the control plane", "start `nodes serve`"),
    );
    renderNodeGraphs(["/graphs?tab=nodes"]);
    await screen.findByText("error:", { exact: false });
    expect(getAgentState().status).toBe("ready");
  });
});

describe("Active Graphs sub-tab: live presence (task t31, c31/h20)", () => {
  async function renderActive() {
    const view = renderNodeGraphs(["/graphs?tab=active"]);
    await waitFor(() => expect(getAgentState().status).toBe("ready"));
    await waitFor(() =>
      expect(getAgentState().active_graphs).toBeTruthy(),
    );
    return view;
  }

  it("renders a haloed graph only for the workflow whose runs hold active tokens", async () => {
    resolveFixture();
    await renderActive();
    const graph = document.getElementById(
      "active-graph-deliver-change-v2",
    ) as HTMLElement;
    expect(graph).toBeInTheDocument();
    expect(graph.classList.contains("is-alive")).toBe(true);
    expect(graph).toHaveAttribute("data-alive", "true");
    expect(graph).toHaveAttribute(
      "data-workflow-digest",
      DELIVER_CHANGE_V2_DIGEST,
    );
    // hello-world's only run is completed; the orphan digest matches no
    // published version — neither renders a graph (h14/h20).
    expect(document.querySelectorAll(".active-graph")).toHaveLength(1);
    expect(screen.queryByText("hello-world")).not.toBeInTheDocument();
    expect(
      screen.queryByText("run-orphan-01J8XKWORKFLOWS0003"),
    ).not.toBeInTheDocument();
  });

  it("marks exactly the nodes with non-terminal node runs as live", async () => {
    resolveFixture();
    await renderActive();
    expect(
      document.querySelector(`[data-node-id="${ACTIVE_NODE_ID}"]`),
    ).toHaveAttribute("data-node-live", "true");
    expect(
      document.querySelector('[data-node-id="intake"]'),
    ).toHaveAttribute("data-node-live", "false");
  });

  it("renders an honest empty state when no run is active (h14)", async () => {
    mockListWorkflows.mockResolvedValue({ items: WORKFLOW_VERSIONS });
    mockListRuns.mockResolvedValue({
      items: WORKFLOWS_RUNS.filter((run) => run.state !== "running"),
    });
    renderNodeGraphs(["/graphs?tab=active"]);
    expect(
      await screen.findByText(/No graphs alive right now/),
    ).toBeInTheDocument();
    expect(document.querySelectorAll(".active-graph")).toHaveLength(0);
    expect(getAgentState().active_graphs?.graph_count).toBe(0);
  });

  it("publishes the complete machine-readable mirror block", async () => {
    resolveFixture();
    await renderActive();
    expect(getAgentState().active_graphs).toEqual({
      graph_count: 1,
      active_run_count: 1,
      active_node_count: 1,
      connection: "reconnecting",
      last_event_id: null,
      events_total: 0,
      pulses_total: 0,
      reduced_motion: false,
    });
  });

  it("reports the stream honestly: reconnecting until open, live after", async () => {
    resolveFixture();
    await renderActive();
    const indicator = document.getElementById("active-graphs-connection")!;
    expect(indicator.getAttribute("data-state")).toBe("reconnecting");
    expect(indicator.textContent).toContain("reconnecting");
    act(() => FakeEventSource.instances[0].open());
    await waitFor(() =>
      expect(indicator.getAttribute("data-state")).toBe("live"),
    );
    expect(getAgentState().active_graphs!.connection).toBe("live");
  });

  it("turns a committed event on a known run into exactly one visible pulse (h14)", async () => {
    resolveFixture();
    await renderActive();
    const source = FakeEventSource.instances[0];
    act(() => {
      source.open();
      source.emit(
        "dev.culture.nodes.attempt.started",
        { run_id: ACTIVE_RUN_ID, node_id: ACTIVE_NODE_ID },
        "01EVENT1",
      );
    });
    await waitFor(() =>
      expect(getAgentState().active_graphs!.events_total).toBe(1),
    );
    expect(getAgentState().active_graphs!.pulses_total).toBe(1);
    expect(getAgentState().active_graphs!.last_event_id).toBe("01EVENT1");
    const ring = document.querySelector(".active-node__pulse");
    expect(ring).toHaveAttribute("data-pulse-count", "1");
    expect(
      ring?.closest(`[data-node-id="${ACTIVE_NODE_ID}"]`),
    ).not.toBeNull();
  });

  it("treats an event naming no known run as a no-op — counted, never rendered (h14)", async () => {
    resolveFixture();
    await renderActive();
    const source = FakeEventSource.instances[0];
    act(() => {
      source.open();
      source.emit(
        "dev.culture.nodes.attempt.started",
        { run_id: UNKNOWN_RUN_ID, node_id: "somewhere" },
        "01EVENT2",
      );
    });
    await waitFor(() =>
      expect(getAgentState().active_graphs!.events_total).toBe(1),
    );
    expect(getAgentState().active_graphs!.pulses_total).toBe(0);
    expect(document.querySelector(".active-node__pulse")).toBeNull();
  });

  it("drops a graph the moment its last run resolves through a committed terminal event", async () => {
    resolveFixture();
    await renderActive();
    const source = FakeEventSource.instances[0];
    act(() => {
      source.open();
      source.emit(
        "dev.culture.nodes.run.completed",
        { run_id: ACTIVE_RUN_ID },
        "01EVENT3",
      );
    });
    await waitFor(() =>
      expect(getAgentState().active_graphs!.graph_count).toBe(0),
    );
    expect(
      await screen.findByText(/No graphs alive right now/),
    ).toBeInTheDocument();
  });

  it("renders one static frame under prefers-reduced-motion", async () => {
    mockMatchMedia(true);
    resolveFixture();
    await renderActive();
    expect(
      document
        .getElementById("active-graph-deliver-change-v2")
        ?.getAttribute("data-motion"),
    ).toBe("static");
    expect(getAgentState().active_graphs!.reduced_motion).toBe(true);
  });

  it("drops the active_graphs block from agent-state when leaving the sub-tab", async () => {
    resolveFixture();
    const user = userEvent.setup();
    await renderActive();
    await user.click(screen.getByRole("button", { name: "Nodes" }));
    await waitFor(() =>
      expect(getAgentState().active_graphs).toBeUndefined(),
    );
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
  it("links to /workflows/new from every sub-tab", async () => {
    renderNodeGraphs(["/graphs?tab=active"]);
    expect(
      screen.getByRole("link", { name: "New workflow" }),
    ).toHaveAttribute("href", "/workflows/new");
    await waitFor(() => expect(getAgentState().status).toBe("ready"));
  });
});
