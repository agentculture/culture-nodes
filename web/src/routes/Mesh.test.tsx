import { beforeEach, afterEach, describe, expect, it, vi } from "vitest";
import { act, render, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { ComponentType } from "react";
import { getAgentState, resetAgentState } from "../agent-state/store";
import type { Actor, NodeRunListItem, Run } from "../api/types";
import Mesh from "./Mesh";
import { SharedEventsProvider } from "../hooks/useSharedEvents";

/**
 * The Mesh route against a mocked client and a fake EventSource: jsdom has
 * no 2D canvas, so — exactly like webglass — these tests assert the
 * machine-readable mirror (agent-state counts, connection state, counters)
 * and the DOM contract (stable ids, data-motion), never pixels.
 */

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    listActors: vi.fn(),
    listRuns: vi.fn(),
    listNodeRuns: vi.fn(),
    getMesh: vi.fn(),
    listWorkflows: vi.fn(),
  };
});

vi.mock("@xyflow/react", () => ({
  ReactFlow: (props: { nodes: Array<{ id: string; type: string; data: Record<string, unknown> }>; nodeTypes: Record<string, ComponentType<{ data: unknown }>>; children?: React.ReactNode }) => <div data-testid="react-flow-stub">{props.nodes.map((node) => { const NodeType = props.nodeTypes[node.type]; return <NodeType key={node.id} data={node.data} />; })}{props.children}</div>,
  Background: () => null, Handle: () => null,
  Position: { Left: "left", Right: "right" }, MarkerType: { ArrowClosed: "arrowclosed" },
}));
vi.mock("../hooks/useElkLayout", () => ({ NODE_WIDTH: 224, NODE_HEIGHT: 84, useElkLayout: () => ({ positions: {}, ready: false }) }));

import { getMesh, listActors, listNodeRuns, listRuns, listWorkflows } from "../api/client";
import { MESH_PAYLOAD, MESH_WORKFLOWS } from "../fixtures/mesh-fixture";

const USAGE = {
  input_tokens: 0,
  output_tokens: 0,
  cached_input_tokens: 0,
  reasoning_tokens: 0,
  attempts_reported: 0,
  attempts_not_reported: 0,
};

const ACTORS: Actor[] = [
  {
    id: "a-thor-r1",
    actor_key: "codex-thor",
    revision: 1,
    kind: "agent",
    protocol: "http",
    created_at: "2026-08-10T00:00:00Z",
  },
  {
    id: "a-thor-r2",
    actor_key: "codex-thor",
    revision: 2,
    kind: "agent",
    protocol: "http",
    created_at: "2026-08-11T00:00:00Z",
  },
  {
    id: "a-orin",
    actor_key: "codex-orin",
    revision: 1,
    kind: "agent",
    protocol: "http",
    created_at: "2026-08-10T00:00:00Z",
  },
  {
    id: "a-ori",
    actor_key: "ori",
    revision: 1,
    kind: "human",
    protocol: "http",
    created_at: "2026-08-10T00:00:00Z",
  },
];

const RUNS: Run[] = [
  {
    id: "run-live",
    workflow_digest: "sha256:wf",
    state: "running",
    name: "Review adapters",
    created_at: "2026-08-12T00:00:00Z",
    updated_at: "2026-08-12T01:00:00Z",
  },
  {
    id: "run-old",
    workflow_digest: "sha256:wf",
    state: "completed",
    created_at: "2026-08-11T00:00:00Z",
    updated_at: "2026-08-11T02:00:00Z",
  },
];

const NODE_RUNS: NodeRunListItem[] = [
  {
    id: "nr-1",
    run_id: "run-live",
    node_id: "build",
    actor_id: "a-thor-r2",
    state: "running",
    created_at: "2026-08-12T00:00:00Z",
    updated_at: "2026-08-12T01:00:00Z",
    usage: USAGE,
  },
];

type Listener = (event: { data: string; lastEventId: string }) => void;

class FakeEventSource {
  static CONNECTING = 0 as const;
  static OPEN = 1 as const;
  static CLOSED = 2 as const;
  static instances: FakeEventSource[] = [];

  url: string;
  readyState = 0;
  onopen: (() => void) | null = null;
  onmessage: Listener | null = null;
  onerror: (() => void) | null = null;
  private listeners = new Map<string, Listener[]>();

  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, listener: Listener) {
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

  /** The server's `stream.snapshot` control frame: a bare body, no envelope. */
  emitSnapshot(id: string) {
    const event = { data: JSON.stringify({ snapshot_id: id }), lastEventId: id };
    for (const listener of this.listeners.get("stream.snapshot") ?? []) listener(event);
  }

  emit(type: string, data: Record<string, unknown>, id: string) {
    const envelope = {
      id,
      source: "nodes",
      specversion: "1.0",
      type,
      subject: data.run_id,
      time: "2026-08-12T02:00:00Z",
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

async function renderMesh() {
  const view = render(
    <MemoryRouter initialEntries={["/mesh"]}>
      <Mesh />
    </MemoryRouter>,
  );
  await waitFor(() => expect(getAgentState().status).toBe("ready"));
  await waitFor(() => expect(getAgentState().mesh).toBeTruthy());
  return view;
}

beforeEach(() => {
  resetAgentState();
  FakeEventSource.instances = [];
  vi.stubGlobal("EventSource", FakeEventSource);
  // jsdom has no 2D context; the canvas guards a null ctx (as CI's webglass
  // job does for the real bundle). Silence jsdom's not-implemented noise.
  vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockReturnValue(null);
  mockMatchMedia(false);
  vi.mocked(listActors).mockResolvedValue({ items: ACTORS });
  vi.mocked(listRuns).mockResolvedValue({ items: RUNS });
  vi.mocked(listNodeRuns).mockResolvedValue({ items: NODE_RUNS });
  vi.mocked(getMesh).mockResolvedValue(MESH_PAYLOAD);
  vi.mocked(listWorkflows).mockResolvedValue({ items: MESH_WORKFLOWS });
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("Mesh route", () => {
  it("reconciles an event committed while its REST snapshot is in flight exactly once", async () => {
    let resolveRuns!: (value: { items: Run[] }) => void;
    vi.mocked(listRuns).mockReturnValueOnce(
      new Promise((resolve) => {
        resolveRuns = resolve;
      }),
    );

    render(<SharedEventsProvider>{null}</SharedEventsProvider>);
    const source = FakeEventSource.instances[0];
    act(() => source.open());

    render(
      <MemoryRouter initialEntries={["/mesh"]}>
        <Mesh />
      </MemoryRouter>,
    );
    act(() => {
      source.emitSnapshot("01SNAPSHOT");
      source.emit(
        "dev.culture.nodes.run.created",
        { run_id: "run-raced", workflow_key: "mesh-demo" },
        "01RACED",
      );
      source.emit(
        "dev.culture.nodes.run.created",
        { run_id: "run-raced", workflow_key: "mesh-demo" },
        "01RACED",
      );
      resolveRuns({ items: RUNS });
    });

    await waitFor(() => expect(getAgentState().status).toBe("ready"));
    await waitFor(() => expect(getAgentState().mesh?.run_count).toBe(2));
    expect(getAgentState().mesh?.events_total).toBe(1);
  });

  it("assembles the graph from actors + active runs and mirrors it in agent-state", async () => {
    await renderMesh();
    const mesh = getAgentState().mesh!;
    // 4 actor rows collapse to 3 actors (one per actor_key); only the
    // active run counts; one edge per actor + one per run.
    expect(mesh.actor_count).toBe(5);
    expect(mesh.machine_count).toBe(3);
    expect(mesh.run_count).toBe(1);
    expect(mesh.edge_count).toBeGreaterThan(0);
    expect(mesh.reduced_motion).toBe(false);
    expect(document.querySelector("#mesh-canvas")).toBeTruthy();
    expect(
      document.querySelector("#mesh-canvas")?.getAttribute("data-motion"),
    ).toBe("animated");
  });

  it("reports the stream honestly: reconnecting until open, live after", async () => {
    await renderMesh();
    const indicator = document.querySelector("#mesh-connection")!;
    expect(indicator.getAttribute("data-state")).toBe("reconnecting");
    expect(indicator.textContent).toContain("reconnecting");

    act(() => FakeEventSource.instances[0].open());
    await waitFor(() =>
      expect(indicator.getAttribute("data-state")).toBe("live"),
    );
    expect(getAgentState().mesh!.connection).toBe("live");
  });

  it("turns a committed event into a counted pulse with its resume cursor", async () => {
    await renderMesh();
    const source = FakeEventSource.instances[0];
    act(() => {
      source.open();
      source.emit(
        "dev.culture.nodes.ledger.record-appended",
        { run_id: "run-live" },
        "01EVENT1",
      );
    });
    await waitFor(() => expect(getAgentState().mesh!.events_total).toBe(1));
    const mesh = getAgentState().mesh!;
    expect(mesh.pulses_total).toBe(1);
    expect(mesh.last_event_id).toBe("01EVENT1");
  });

  it("makes a run appear on run.created and counts its lifecycle resolution", async () => {
    await renderMesh();
    const source = FakeEventSource.instances[0];
    act(() => {
      source.open();
      source.emit(
        "dev.culture.nodes.run.created",
        { run_id: "run-new", workflow_key: "mesh-demo" },
        "01EVENT2",
      );
    });
    await waitFor(() => expect(getAgentState().mesh!.run_count).toBe(2));
    // run.created is an appearance, not a particle.
    expect(getAgentState().mesh!.pulses_total).toBe(0);

    act(() =>
      source.emit(
        "dev.culture.nodes.run.completed",
        { run_id: "run-new" },
        "01EVENT3",
      ),
    );
    await waitFor(() => expect(getAgentState().mesh!.pulses_total).toBe(1));
    // The resolved run lingers for its settle animation rather than
    // vanishing the moment the event lands.
    expect(getAgentState().mesh!.run_count).toBe(2);
  });

  it("renders one dignified static frame under prefers-reduced-motion", async () => {
    mockMatchMedia(true);
    await renderMesh();
    expect(
      document.querySelector("#mesh-canvas")?.getAttribute("data-motion"),
    ).toBe("static");
    expect(getAgentState().mesh!.reduced_motion).toBe(true);
  });

  it("labels an unsupported actor as having no capability surface", async () => {
    vi.mocked(getMesh).mockResolvedValueOnce({
      ...MESH_PAYLOAD,
      actors: [{ id: "actor-human-ops-r1", actor_key: "company/human-ops", machine: null, bridge: { observed_at: "now", class: "unsupported", reason: "GET capabilities: 404 Not Found", error: "GET capabilities: 404 Not Found" } }],
      machines: {},
    });
    const view = await renderMesh();
    expect(view.container.textContent).toContain("no capability surface · GET capabilities: 404 Not Found");
    expect(getAgentState().mesh?.probe_failures).toBe(0);
  });

  it("drops the mesh block from agent-state on unmount", async () => {
    const view = await renderMesh();
    view.unmount();
    expect(getAgentState().mesh).toBeUndefined();
  });
});
