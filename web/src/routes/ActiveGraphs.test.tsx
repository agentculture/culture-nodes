import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import type { ComponentType } from "react";
import Design from "./Design";
import { listNodeRuns, listRuns, listWorkflows } from "../api/client";
import {
  DELIVER_CHANGE_V2_DIGEST,
  WORKFLOW_VERSIONS,
  WORKFLOWS_RUNS,
  workflowsRunsFor,
} from "../fixtures/workflows-fixture";
import {
  ACTIVE_NODE_ID,
  ACTIVE_NODE_RUNS,
  ACTIVE_RUN_ID,
  UNKNOWN_RUN_ID,
} from "../fixtures/active-graphs-fixture";
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

function renderDesign(initialEntries: string[] = ["/design"]) {
  return render(
    <MemoryRouter initialEntries={initialEntries}>
      <Design />
      <LocationProbe />
    </MemoryRouter>,
  );
}

/**
 * The mock server's `GET /v1alpha1/runs`, honoring `workflow_key` the way
 * the real endpoint does (task t7's filter): a keyed request answers only
 * that workflow's runs, an unkeyed one answers the whole window. The
 * gallery asks per key (task t8); the Active graphs sub-view asks unkeyed,
 * because "what is alive right now" is a cross-workflow question.
 */
function runsFor(
  _signal?: AbortSignal,
  params?: { workflow_key?: string },
): Promise<{ items: typeof WORKFLOWS_RUNS }> {
  return Promise.resolve({
    items: params?.workflow_key
      ? workflowsRunsFor(params.workflow_key)
      : WORKFLOWS_RUNS,
  });
}

function resolveFixture() {
  mockListWorkflows.mockResolvedValue({ items: WORKFLOW_VERSIONS });
  mockListRuns.mockImplementation(runsFor);
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

/**
 * The Active graphs sub-view of Design (task t31's panel, moved into
 * `routes/ActiveGraphs.tsx` and re-homed under `/design?tab=active` by task
 * t8). These are t31's own tests, unchanged apart from the route and the
 * panel id — the behaviour they pin did not move, only the file did.
 */
describe("Active graphs sub-view: live presence (task t31, c31/h20)", () => {
  async function renderActive() {
    const view = renderDesign(["/design?tab=active"]);
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
    renderDesign(["/design?tab=active"]);
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

  it("holds an event that arrives before the committed rows land, then judges it against them", async () => {
    // The stream opens concurrently with the initial fetch, so a committed
    // event can beat the runs listing. Adjudicating it against an empty
    // known-run set would drop it as "unknown run" — a lie, and a source of
    // nondeterministic pulse counts. It must be held and replayed.
    mockListWorkflows.mockResolvedValue({ items: WORKFLOW_VERSIONS });
    mockListNodeRuns.mockResolvedValue({ items: ACTIVE_NODE_RUNS });
    let releaseRuns!: (value: Awaited<ReturnType<typeof listRuns>>) => void;
    mockListRuns.mockReturnValue(
      new Promise<Awaited<ReturnType<typeof listRuns>>>((resolve) => {
        releaseRuns = resolve;
      }),
    );
    renderDesign(["/design?tab=active"]);
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    act(() => {
      FakeEventSource.instances[0].open();
      FakeEventSource.instances[0].emit(
        "dev.culture.nodes.attempt.started",
        { run_id: ACTIVE_RUN_ID, node_id: ACTIVE_NODE_ID },
        "01EVENTQ",
      );
    });
    // Nothing is claimed while the rows are still in flight.
    expect(getAgentState().active_graphs).toBeFalsy();

    await act(async () => {
      releaseRuns({ items: WORKFLOWS_RUNS });
    });
    await waitFor(() =>
      expect(getAgentState().active_graphs?.pulses_total).toBe(1),
    );
    expect(getAgentState().active_graphs!.events_total).toBe(1);
    expect(document.querySelector(".active-node__pulse")).toHaveAttribute(
      "data-pulse-count",
      "1",
    );
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
