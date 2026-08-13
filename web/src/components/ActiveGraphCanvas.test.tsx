import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { ComponentType } from "react";
import ActiveGraphCanvas from "./ActiveGraphCanvas";
import {
  deriveActiveGraphs,
  type ActiveGraphPresence,
} from "../domain/active-presence";
import {
  WORKFLOW_VERSIONS,
  WORKFLOWS_RUNS,
} from "../fixtures/workflows-fixture";
import {
  ACTIVE_NODE_ID,
  ACTIVE_NODE_RUNS,
  ACTIVE_RUN_ID,
} from "../fixtures/active-graphs-fixture";

/**
 * ActiveGraphCanvas (task t31): the halo renders only from committed-run
 * presence, node liveness is word+color (never color alone), every pulse
 * element is keyed to a committed-event counter, reduced motion renders a
 * static frame, and the graph is inspectable from the keyboard alone.
 *
 * React Flow itself is stubbed: these tests assert this component's own
 * contract (classes, data attributes, text, keyboard handling), not React
 * Flow's canvas internals — the same presentational-layer discipline the
 * vitest setup file documents. The stub still renders every node through
 * `nodeTypes`, so the per-node presence markup is asserted for real.
 */

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

// ELK is a 1.4 MB bundle jsdom has no business executing — the fallback
// layout is deterministic and the canvas contract is position-independent.
vi.mock("../hooks/useElkLayout", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("../hooks/useElkLayout")>();
  return {
    ...actual,
    useElkLayout: () => ({ positions: {}, ready: false }),
  };
});

const PRESENCE: ActiveGraphPresence = deriveActiveGraphs(
  WORKFLOW_VERSIONS,
  WORKFLOWS_RUNS,
  ACTIVE_NODE_RUNS,
)[0];

function renderCanvas(overrides?: {
  presence?: ActiveGraphPresence;
  reducedMotion?: boolean;
  pulses?: Record<string, number>;
}) {
  return render(
    <MemoryRouter>
      <ActiveGraphCanvas
        presence={overrides?.presence ?? PRESENCE}
        reducedMotion={overrides?.reducedMotion ?? false}
        pulses={overrides?.pulses ?? {}}
      />
    </MemoryRouter>,
  );
}

describe("halo from committed presence (h20/h14)", () => {
  it("marks the graph alive only because non-terminal runs pin it, with the fact in words too", () => {
    renderCanvas();
    const section = document.getElementById(
      "active-graph-deliver-change-v2",
    ) as HTMLElement;
    expect(section).toBeInTheDocument();
    expect(section.classList.contains("is-alive")).toBe(true);
    expect(section).toHaveAttribute("data-alive", "true");
    // Never color/motion alone: the count line says it in words.
    expect(section).toHaveTextContent("1 active run");
  });

  it("renders no halo for a presence without active runs", () => {
    renderCanvas({ presence: { ...PRESENCE, runIds: [], activeNodeIds: [] } });
    const section = document.getElementById(
      "active-graph-deliver-change-v2",
    ) as HTMLElement;
    expect(section.classList.contains("is-alive")).toBe(false);
    expect(section).toHaveAttribute("data-alive", "false");
  });

  it("links every active run to its Run view", () => {
    renderCanvas();
    const link = screen.getByRole("link", { name: ACTIVE_RUN_ID });
    expect(link).toHaveAttribute("href", `/runs/${ACTIVE_RUN_ID}`);
  });
});

describe("node presence (h20: only committed node-run rows glow)", () => {
  it("marks exactly the nodes with non-terminal node runs live, with an explicit word badge", () => {
    renderCanvas();
    const live = document.querySelector(
      `[data-node-id="${ACTIVE_NODE_ID}"]`,
    ) as HTMLElement;
    expect(live).toHaveAttribute("data-node-live", "true");
    expect(live).toHaveTextContent("active");

    const idle = document.querySelector(
      '[data-node-id="intake"]',
    ) as HTMLElement;
    expect(idle).toHaveAttribute("data-node-live", "false");
    expect(idle).not.toHaveTextContent("active");
  });

  it("renders every node of the pinned graph, none invented, none dropped", () => {
    renderCanvas();
    const ids = [...document.querySelectorAll("[data-node-id]")].map((el) =>
      el.getAttribute("data-node-id"),
    );
    expect(ids.sort()).toEqual(
      PRESENCE.graph.nodes.map((node) => node.id).sort(),
    );
  });
});

describe("pulses (h14: one ring per committed event)", () => {
  it("renders no pulse ring before any committed event arrived", () => {
    renderCanvas();
    expect(document.querySelector(".active-node__pulse")).toBeNull();
  });

  it("renders a ring keyed by the committed-event counter for the pulsed node only", () => {
    renderCanvas({ pulses: { [ACTIVE_NODE_ID]: 2 } });
    const rings = document.querySelectorAll(".active-node__pulse");
    expect(rings).toHaveLength(1);
    expect(rings[0]).toHaveAttribute("data-pulse-count", "2");
    expect(
      rings[0].closest(`[data-node-id="${ACTIVE_NODE_ID}"]`),
    ).not.toBeNull();
  });
});

describe("motion modes", () => {
  it("renders one static frame under prefers-reduced-motion", () => {
    renderCanvas({ reducedMotion: true });
    const section = document.getElementById(
      "active-graph-deliver-change-v2",
    ) as HTMLElement;
    expect(section).toHaveAttribute("data-motion", "static");
    // The static frame still states liveness in words.
    expect(section).toHaveTextContent("1 active run");
  });

  it("animates when motion is allowed and the canvas is on screen", () => {
    renderCanvas();
    expect(
      document.getElementById("active-graph-deliver-change-v2"),
    ).toHaveAttribute("data-motion", "animated");
  });
});

describe("keyboard inspection (MeshCanvas precedent, no pointer required)", () => {
  it("cycles nodes with the arrow keys and reads the inspection out as text", async () => {
    const user = userEvent.setup();
    renderCanvas();
    const canvas = screen.getByRole("application");
    canvas.focus();

    await user.keyboard("{ArrowRight}");
    const readout = document.getElementById(
      "active-graph-deliver-change-v2-inspect",
    ) as HTMLElement;
    // First node in the breadth-first order is the entry node.
    expect(readout).toHaveTextContent(
      `${PRESENCE.graph.entry} · agent · no active work`,
    );

    // Walk forward until the live node and check the active wording.
    const liveIndex = PRESENCE.graph.nodes.findIndex(
      (node) => node.id === ACTIVE_NODE_ID,
    );
    for (let i = 0; i < liveIndex; i += 1) {
      await user.keyboard("{ArrowRight}");
    }
    expect(readout).toHaveTextContent(`${ACTIVE_NODE_ID} · agent · active`);

    // The inspected node is visibly marked on the canvas too.
    expect(
      document.querySelector(`[data-node-id="${ACTIVE_NODE_ID}"]`),
    ).toHaveClass("is-inspected");
  });

  it("wraps backwards from the first node and clears on Escape", async () => {
    const user = userEvent.setup();
    renderCanvas();
    screen.getByRole("application").focus();

    await user.keyboard("{ArrowLeft}");
    const readout = document.getElementById(
      "active-graph-deliver-change-v2-inspect",
    ) as HTMLElement;
    const last = PRESENCE.graph.nodes[PRESENCE.graph.nodes.length - 1];
    expect(readout).toHaveTextContent(last.id);

    await user.keyboard("{Escape}");
    expect(readout).toHaveTextContent(
      "Focus the graph and use the arrow keys to inspect nodes.",
    );
  });
});
