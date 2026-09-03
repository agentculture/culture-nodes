import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import type { ComponentType } from "react";
import Design from "./Design";
import { ApiError, listNodeRuns, listRuns, listWorkflows } from "../api/client";
import {
  DELIVER_CHANGE_V1_SOURCE,
  DELIVER_CHANGE_V2_DIGEST,
  DESIGN_GRAPH_SIZES,
  HELLO_WORLD_DIGEST,
  HELLO_WORLD_SOURCE,
  NODE_CATALOG_DEFINITION_COUNT,
  NODE_CATALOG_WORKFLOW_VERSIONS,
  SWEEP_DOMINATED_RUNS,
  WORKFLOW_VERSIONS,
  WORKFLOWS_RUNS,
  workflowsRunsFor,
} from "../fixtures/workflows-fixture";
import { ACTIVE_NODE_RUNS } from "../fixtures/active-graphs-fixture";
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

describe("Design sub-views (task t8, replacing task t28's tab shell)", () => {
  it("defaults to the Gallery when the URL carries no ?tab param", async () => {
    resolveFixture();
    renderDesign();
    expect(screen.getByRole("button", { name: "Gallery" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(screen.getByRole("button", { name: "Nodes" })).toHaveAttribute(
      "aria-pressed",
      "false",
    );
    expect(
      screen.getByRole("button", { name: "Active graphs" }),
    ).toHaveAttribute("aria-pressed", "false");
    expect(document.getElementById("design-gallery-panel")).toBeInTheDocument();
    await waitFor(() => expect(getAgentState().status).toBe("ready"));
  });

  it("selects the sub-view named by ?tab= on first render", async () => {
    renderDesign(["/design?tab=active"]);
    expect(
      screen.getByRole("button", { name: "Active graphs" }),
    ).toHaveAttribute("aria-pressed", "true");
    expect(document.getElementById("design-active-panel")).toBeInTheDocument();
    await waitFor(() => expect(getAgentState().status).toBe("ready"));
  });

  it("keeps the node-definition catalog reachable at ?tab=nodes (t8 acceptance 3)", async () => {
    mockListWorkflows.mockResolvedValue({
      items: NODE_CATALOG_WORKFLOW_VERSIONS,
    });
    renderDesign(["/design?tab=nodes"]);
    await waitFor(() => expect(getAgentState().status).toBe("ready"));
    expect(document.getElementById("design-nodes-panel")).toBeInTheDocument();
    expect(document.querySelectorAll(".node-def-card")).toHaveLength(
      NODE_CATALOG_DEFINITION_COUNT,
    );
  });

  it("falls back to the Gallery for an unrecognized ?tab= value", async () => {
    resolveFixture();
    renderDesign(["/design?tab=bogus"]);
    expect(screen.getByRole("button", { name: "Gallery" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    await waitFor(() => expect(getAgentState().status).toBe("ready"));
  });

  it("clicking a sub-view button updates the URL's ?tab= param (bookmarkable)", async () => {
    resolveFixture();
    const user = userEvent.setup();
    renderDesign();
    await waitFor(() => expect(getAgentState().status).toBe("ready"));

    await user.click(screen.getByRole("button", { name: "Active graphs" }));
    expect(screen.getByTestId("location-search")).toHaveTextContent(
      "?tab=active",
    );

    await user.click(screen.getByRole("button", { name: "Gallery" }));
    // The default sub-view clears the param rather than writing "?tab=gallery".
    expect(screen.getByTestId("location-search")).not.toHaveTextContent("tab=");
  });
});

/**
 * The gallery itself (task t8, claims c24/c31): every published workflow is
 * readable as a graph, and NOT ONE of these tests involves a run.
 */
describe("Design gallery: every published version as a graph", () => {
  function resolveCatalog(runs: typeof WORKFLOWS_RUNS | null = null) {
    mockListWorkflows.mockResolvedValue({
      items: NODE_CATALOG_WORKFLOW_VERSIONS,
    });
    mockListRuns.mockImplementation((_signal, params) =>
      Promise.resolve({
        items:
          runs === null
            ? params?.workflow_key
              ? workflowsRunsFor(params.workflow_key)
              : WORKFLOWS_RUNS
            : runs,
      }),
    );
  }

  it("lists one entry per published workflow_key with its version count and owner", async () => {
    resolveCatalog();
    renderDesign();
    await screen.findByText("deliver-change");
    const index = document.getElementById("design-workflow-list")!;
    expect(index.querySelectorAll("[data-workflow-key]")).toHaveLength(3);
    const deliver = index.querySelector(
      '[data-workflow-key="deliver-change"]',
    ) as HTMLElement;
    expect(deliver).toHaveTextContent("2 versions");
    expect(deliver).toHaveTextContent("team/platform-ai");
  });

  it("draws the selected version's graph from normalized_ir — the expected node and edge count for every key (c31/h21)", async () => {
    resolveCatalog();
    const user = userEvent.setup();
    renderDesign();
    await screen.findByText("deliver-change");

    for (const [key, size] of Object.entries(DESIGN_GRAPH_SIZES)) {
      await user.click(
        document.querySelector(
          `#design-workflow-list [data-workflow-key="${key}"]`,
        ) as HTMLElement,
      );
      const canvas = document.getElementById("design-graph")!;
      await waitFor(() =>
        expect(canvas).toHaveAttribute("data-workflow-key", key),
      );
      expect(canvas).toHaveAttribute("data-node-count", String(size.nodes));
      expect(canvas).toHaveAttribute("data-edge-count", String(size.edges));
      // The nodes are really drawn, through the one Culture node component.
      expect(canvas.querySelectorAll("[data-node-id]")).toHaveLength(
        size.nodes,
      );
    }
  });

  it("draws every graph with ZERO runs in the namespace — the whole point of the view (c31)", async () => {
    resolveCatalog([]);
    const user = userEvent.setup();
    renderDesign();
    await screen.findByText("notify-team");

    for (const [key, size] of Object.entries(DESIGN_GRAPH_SIZES)) {
      await user.click(
        document.querySelector(
          `#design-workflow-list [data-workflow-key="${key}"]`,
        ) as HTMLElement,
      );
      await waitFor(() =>
        expect(document.getElementById("design-graph")).toHaveAttribute(
          "data-workflow-key",
          key,
        ),
      );
      expect(document.getElementById("design-graph")).toHaveAttribute(
        "data-node-count",
        String(size.nodes),
      );
      // Said in words too, not only by the absence of a run list.
      expect(document.getElementById("design-no-runs")).toBeInTheDocument();
    }
    expect(getAgentState().design!.run_count).toBe(0);
  });

  it("keeps the selection in the URL, so a graph is a link and not a click path", async () => {
    resolveCatalog();
    const user = userEvent.setup();
    renderDesign();
    await screen.findByText("hello-world");

    await user.click(
      document.querySelector(
        '#design-workflow-list [data-workflow-key="hello-world"]',
      ) as HTMLElement,
    );
    expect(screen.getByTestId("location-search")).toHaveTextContent(
      "workflow=hello-world",
    );
  });

  it("renders the version the URL names, and lists every version of that key newest first", async () => {
    resolveCatalog();
    renderDesign(["/design?workflow=deliver-change&version=1"]);
    await screen.findByText("deliver-change");
    expect(document.getElementById("design-graph")).toHaveAttribute(
      "data-workflow-digest",
      WORKFLOW_DIGEST,
    );
    const versions = document
      .getElementById("design-version-list")!
      .querySelectorAll("[data-workflow-digest]");
    expect(
      Array.from(versions).map((el) => el.getAttribute("data-workflow-digest")),
    ).toEqual([DELIVER_CHANGE_V2_DIGEST, WORKFLOW_DIGEST]);
  });

  it("switching version redraws the same key at the other digest", async () => {
    resolveCatalog();
    const user = userEvent.setup();
    renderDesign(["/design?workflow=deliver-change"]);
    await screen.findByText("deliver-change");
    expect(document.getElementById("design-graph")).toHaveAttribute(
      "data-workflow-digest",
      DELIVER_CHANGE_V2_DIGEST,
    );
    await user.click(
      document.querySelector(
        `#design-version-list [data-workflow-digest="${WORKFLOW_DIGEST}"]`,
      ) as HTMLElement,
    );
    await waitFor(() =>
      expect(document.getElementById("design-graph")).toHaveAttribute(
        "data-workflow-digest",
        WORKFLOW_DIGEST,
      ),
    );
  });

  it("falls back to the newest version of the first workflow when the URL names neither", async () => {
    resolveCatalog();
    renderDesign();
    await screen.findByText("deliver-change");
    expect(document.getElementById("design-graph")).toHaveAttribute(
      "data-workflow-digest",
      DELIVER_CHANGE_V2_DIGEST,
    );
  });

  it("shows the stored source byte-identical, and only when asked (c36/h28)", async () => {
    resolveCatalog();
    const user = userEvent.setup();
    renderDesign(["/design?workflow=deliver-change&version=1"]);
    await screen.findByText("deliver-change");
    expect(document.getElementById("design-source")).toBeNull();

    await user.click(screen.getByRole("button", { name: "Open source" }));
    const pane = document.getElementById("design-source")!;
    // textContent, not a normalized render: every byte, in order.
    expect(pane.textContent).toBe(DELIVER_CHANGE_V1_SOURCE);
    expect(pane).toHaveAttribute("data-source-format", "yaml");
    expect(pane).toHaveAttribute(
      "data-source-bytes",
      String(DELIVER_CHANGE_V1_SOURCE.length),
    );
    // A comment the IR cannot carry: proof this is the source, not a
    // re-serialization of normalized_ir.
    expect(pane.textContent).toContain("# the loop");
  });

  it("publishes the machine-readable mirror of what the canvas is claiming", async () => {
    resolveCatalog();
    renderDesign(["/design?workflow=hello-world"]);
    await screen.findByText("hello-world");
    await waitFor(() =>
      expect(getAgentState().design?.workflow_key).toBe("hello-world"),
    );
    expect(getAgentState().design).toEqual({
      workflow_count: 3,
      workflow_key: "hello-world",
      version: 1,
      digest: HELLO_WORLD_DIGEST,
      node_count: DESIGN_GRAPH_SIZES["hello-world"].nodes,
      edge_count: DESIGN_GRAPH_SIZES["hello-world"].edges,
      source_bytes: HELLO_WORLD_SOURCE.length,
      source_open: false,
      run_count: 1,
    });
  });

  it("drops the design block from agent-state when leaving the gallery", async () => {
    resolveCatalog();
    const user = userEvent.setup();
    renderDesign();
    await screen.findByText("deliver-change");
    await user.click(screen.getByRole("button", { name: "Nodes" }));
    await waitFor(() => expect(getAgentState().design).toBeUndefined());
  });

  it("lists the selected workflow's recent runs, and never another workflow's or an unmatched digest's", async () => {
    resolveCatalog();
    renderDesign(["/design?workflow=deliver-change"]);
    await screen.findByText("deliver-change");
    const runs = document.querySelectorAll(".design-gallery__run-list a");
    expect(Array.from(runs).map((a) => a.textContent)).toEqual([
      "run-deliver-v2-01J8XKWORKFLOWS02",
      "run-deliver-v1-01J8XKWORKFLOWS04",
    ]);
    expect(runs[0]).toHaveAttribute(
      "href",
      "/runs/run-deliver-v2-01J8XKWORKFLOWS02",
    );
    expect(
      screen.queryByText("run-orphan-01J8XKWORKFLOWS0003"),
    ).not.toBeInTheDocument();
  });
});

describe("Design gallery loading/empty/error", () => {
  it("shows a loading state before both requests resolve", () => {
    mockListWorkflows.mockReturnValue(new Promise(() => {}));
    mockListRuns.mockReturnValue(new Promise(() => {}));
    renderDesign();
    expect(screen.getByText("Loading workflows…")).toBeInTheDocument();
    expect(document.getElementById("design-graph")).toBeNull();
  });

  it("says nothing is published rather than drawing an empty graph (h14)", async () => {
    mockListWorkflows.mockResolvedValue({ items: [] });
    mockListRuns.mockResolvedValue({ items: [] });
    renderDesign();
    expect(
      await screen.findByText(/No workflows published yet\./),
    ).toBeInTheDocument();
    expect(document.getElementById("design-graph")).toBeNull();
    expect(getAgentState().design?.workflow_count).toBe(0);
  });

  it("renders an error notice and stops loading when the workflows request fails", async () => {
    mockListWorkflows.mockRejectedValue(
      new ApiError(0, "cannot reach the control plane", "start `nodes serve`"),
    );
    mockListRuns.mockResolvedValue({ items: [] });
    renderDesign();
    await screen.findByText("error:", { exact: false });
    expect(
      screen.getByText("cannot reach the control plane", { exact: false }),
    ).toBeInTheDocument();
    expect(getAgentState().status).toBe("ready");
  });

  it("renders an error notice when the runs request fails, even if workflows succeeded", async () => {
    mockListWorkflows.mockResolvedValue({ items: WORKFLOW_VERSIONS });
    mockListRuns.mockRejectedValue(
      new ApiError(0, "cannot reach the control plane", "start `nodes serve`"),
    );
    renderDesign();
    await screen.findByText("error:", { exact: false });
  });
});

describe("Design gallery data fetch", () => {
  it("requests every published workflow version, then one runs page PER workflow_key", async () => {
    resolveFixture();
    renderDesign();
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
 * Task t8 / claim c8, carried over from the retired workflow-cards panel:
 * each workflow's runs were filtered out of ONE unfiltered
 * `GET /v1alpha1/runs` page. In production the pr-upkeep sweep mints a run
 * every few minutes, so that page holds nothing but sweep runs and every
 * workflow read as "never run" while having hundreds. The fixture below
 * reproduces exactly that: the unkeyed listing is 50 sweep runs, and no
 * workflow's own run is anywhere in it.
 */
describe("Gallery run counts survive a sweep-dominated global window (task t8)", () => {
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
  }

  it("shows a workflow's own runs even though not one of them is in the unfiltered window", async () => {
    resolveSweepDominated();
    renderDesign(["/design?workflow=deliver-change"]);
    await screen.findByText("deliver-change");
    const runs = document.querySelectorAll(".design-gallery__run-list a");
    expect(Array.from(runs).map((a) => a.textContent)).toEqual([
      "run-deliver-v2-01J8XKWORKFLOWS02",
      "run-deliver-v1-01J8XKWORKFLOWS04",
    ]);
    expect(document.getElementById("design-no-runs")).toBeNull();
    expect(
      screen.queryByText(SWEEP_DOMINATED_RUNS[0].id),
    ).not.toBeInTheDocument();
  });

  it("still says 'never run' for a workflow that genuinely has no runs (h14)", async () => {
    resolveSweepDominated();
    renderDesign(["/design?workflow=notify-team"]);
    await screen.findByText("notify-team");
    const entry = document.querySelector(
      '#design-workflow-list [data-workflow-key="notify-team"]',
    ) as HTMLElement;
    expect(within(entry).getByText("never run")).toBeInTheDocument();
    expect(document.getElementById("design-no-runs")).toBeInTheDocument();
    // …and it still has a graph. That is claim c31 in one assertion.
    expect(document.getElementById("design-graph")).toHaveAttribute(
      "data-node-count",
      String(DESIGN_GRAPH_SIZES["notify-team"].nodes),
    );
  });
});

describe("Design authoring entry point", () => {
  it("links to /workflows/new from every sub-view", async () => {
    renderDesign(["/design?tab=active"]);
    expect(screen.getByRole("link", { name: "New workflow" })).toHaveAttribute(
      "href",
      "/workflows/new",
    );
    await waitFor(() => expect(getAgentState().status).toBe("ready"));
  });
});

describe("Nodes sub-view: the node-definition catalog (tasks t29+t31, c20)", () => {
  it("renders one card per distinct definition derived from published IRs, nothing else", async () => {
    mockListWorkflows.mockResolvedValue({
      items: NODE_CATALOG_WORKFLOW_VERSIONS,
    });
    renderDesign(["/design?tab=nodes"]);
    await waitFor(() => expect(getAgentState().status).toBe("ready"));
    expect(document.querySelectorAll(".node-def-card")).toHaveLength(
      NODE_CATALOG_DEFINITION_COUNT,
    );
  });

  it("shows kind, ref, and every cross-workflow occurrence on a shared-actor definition", async () => {
    mockListWorkflows.mockResolvedValue({
      items: NODE_CATALOG_WORKFLOW_VERSIONS,
    });
    renderDesign(["/design?tab=nodes"]);
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
    renderDesign(["/design?tab=nodes"]);
    await waitFor(() => expect(getAgentState().status).toBe("ready"));
    const endCard = document.querySelector(
      '[data-definition-id="end"]',
    ) as HTMLElement;
    expect(
      within(endCard).getByText(/identity is the kind alone/),
    ).toBeInTheDocument();
  });

  it("renders an honest empty state when no workflow has been published (h14)", async () => {
    renderDesign(["/design?tab=nodes"]);
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
    renderDesign(["/design?tab=nodes"]);
    await screen.findByText("error:", { exact: false });
    expect(getAgentState().status).toBe("ready");
  });
});

describe("Gallery auto-refresh (issue #46, task t30)", () => {
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
    renderDesign(["/design"]);
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
    renderDesign(["/design"]);
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
    renderDesign(["/design"]);
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

  it("detaches the subscription when switching away from the gallery (no reload after leaving)", async () => {
    resolveFixture();
    const { rerender } = renderDesign(["/design"]);
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
