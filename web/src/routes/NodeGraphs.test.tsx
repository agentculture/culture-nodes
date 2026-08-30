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
  SWEEP_DOMINATED_RUNS,
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
import { WORKFLOW_DIGEST } from "../fixtures/run-fixture";
import { RECENT_RUNS_LIMIT } from "../domain/workflows";
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

/**
 * The mock server's `GET /v1alpha1/runs`, honoring `workflow_key` the way
 * the real endpoint does (task t7's filter): a keyed request answers only
 * that workflow's runs, an unkeyed one answers the whole window. The Node
 * Graphs sub-tab asks per key (task t8); the Active Graphs sub-tab asks
 * unkeyed, because "what is alive right now" is a cross-workflow question.
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
    renderNodeGraphs(["/graphs?tab=active"]);
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
  it("requests every published workflow version, then one runs page PER workflow_key", async () => {
    resolveFixture();
    renderNodeGraphs(["/graphs?tab=graphs"]);
    await screen.findByText("deliver-change");
    expect(mockListWorkflows).toHaveBeenCalledTimes(1);
    // One keyed request per published workflow_key — never one global
    // listing sliced client-side (task t8).
    expect(mockListRuns).toHaveBeenCalledTimes(2);
    expect(mockListRuns.mock.calls.map(([, params]) => params)).toEqual([
      { workflow_key: "deliver-change", sort: "updated_at", limit: RECENT_RUNS_LIMIT },
      { workflow_key: "hello-world", sort: "updated_at", limit: RECENT_RUNS_LIMIT },
    ]);
  });
});

/**
 * Task t8 / claim c8. The defect: each card's "recent runs" was filtered
 * out of ONE unfiltered `GET /v1alpha1/runs` page. In production the
 * pr-upkeep sweep mints a run every few minutes, so that page holds nothing
 * but sweep runs and every workflow card claimed "No runs yet" while having
 * hundreds of runs. The fixture below reproduces exactly that: the unkeyed
 * listing is 50 sweep runs, and no card's own run is anywhere in it.
 */
describe("Node Graphs recent runs survive a sweep-dominated global window (task t8)", () => {
  function resolveSweepDominated() {
    mockListWorkflows.mockResolvedValue({
      items: NODE_CATALOG_WORKFLOW_VERSIONS,
    });
    mockListRuns.mockImplementation((_signal, params) =>
      Promise.resolve({
        items: params?.workflow_key
          ? workflowsRunsFor(params.workflow_key)
          : SWEEP_DOMINATED_RUNS,
      }),
    );
    mockListNodeRuns.mockResolvedValue({ items: [] });
  }

  it("renders a workflow's own runs even though not one of them is in the unfiltered window", async () => {
    resolveSweepDominated();
    renderNodeGraphs(["/graphs?tab=graphs"]);
    const card = (await screen.findByText("deliver-change")).closest(
      ".workflow-card",
    ) as HTMLElement;
    const runLinks = within(card)
      .getAllByRole("link")
      .filter((link) => link.getAttribute("href")?.startsWith("/runs/"));
    expect(runLinks.map((link) => link.textContent)).toEqual([
      "run-deliver-v2-01J8XKWORKFLOWS02",
      "run-deliver-v1-01J8XKWORKFLOWS04",
    ]);
    expect(within(card).queryByText(/No runs yet/)).not.toBeInTheDocument();
  });

  it("never renders the empty state for any workflow that has runs", async () => {
    resolveSweepDominated();
    renderNodeGraphs(["/graphs?tab=graphs"]);
    await screen.findByText("deliver-change");
    for (const key of ["deliver-change", "hello-world"]) {
      const card = document.querySelector(
        `[data-workflow-key="${key}"]`,
      ) as HTMLElement;
      expect(within(card).queryByText(/No runs yet/)).not.toBeInTheDocument();
    }
  });

  it("still renders the honest empty state for a workflow that genuinely has none (h14)", async () => {
    resolveSweepDominated();
    renderNodeGraphs(["/graphs?tab=graphs"]);
    await screen.findByText("notify-team");
    const card = document.querySelector(
      '[data-workflow-key="notify-team"]',
    ) as HTMLElement;
    expect(within(card).getByText(/No runs yet/)).toBeInTheDocument();
    expect(
      within(card)
        .queryAllByRole("link")
        .filter((link) => link.getAttribute("href")?.startsWith("/runs/")),
    ).toHaveLength(0);
  });

  it("never shows another workflow's runs on a card — the sweep's runs appear nowhere", async () => {
    resolveSweepDominated();
    renderNodeGraphs(["/graphs?tab=graphs"]);
    await screen.findByText("deliver-change");
    expect(
      screen.queryByText(SWEEP_DOMINATED_RUNS[0].id),
    ).not.toBeInTheDocument();
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
