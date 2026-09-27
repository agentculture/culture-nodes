import type { ComponentType } from "react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import * as client from "../api/client";
import {
  FOCUS_DISTANCE_0,
  FOCUS_DISTANCE_1,
  FOCUS_DISTANCE_2,
  focusAtDistance,
} from "../fixtures/declarations-fixture";
import Declarations from "./Declarations";

/**
 * Declarations route (task t22, #328, c24/h18): renders a single
 * declaration or a chain from the SAME focus payload, and the distance
 * control refetches at N=0/1/2, forwarding direction/link to the API —
 * where the server actually applies them (declgraph.go's BFS).
 *
 * React Flow is stubbed the same way ActiveGraphCanvas.test.tsx stubs it:
 * these tests assert this component's own contract (which declarations and
 * links it hands React Flow, and how it labels them), not React Flow's own
 * canvas internals. ELK is mocked to its synchronous fallback for the same
 * reason ActiveGraphCanvas.test.tsx mocks it — jsdom has no business
 * loading a 1.4 MB layout engine, and the graph's CONTENT (which nodes,
 * which edges, which distance) is what this route promises, not their
 * pixel position.
 */

vi.mock("@xyflow/react", () => ({
  ReactFlow: (props: {
    nodes: Array<{ id: string; type: string; data: Record<string, unknown> }>;
    edges: Array<{
      id: string;
      source: string;
      target: string;
      label?: string;
      className?: string;
    }>;
    nodeTypes: Record<string, ComponentType<{ id: string; data: unknown }>>;
    children?: React.ReactNode;
  }) => (
    <div data-testid="react-flow-stub">
      {props.nodes.map((node) => {
        const NodeType = props.nodeTypes[node.type];
        return <NodeType key={node.id} id={node.id} data={node.data} />;
      })}
      <ul data-testid="react-flow-edges">
        {props.edges.map((edge) => (
          <li
            key={edge.id}
            data-edge-id={edge.id}
            data-source={edge.source}
            data-target={edge.target}
            className={edge.className}
          >
            {edge.label}
          </li>
        ))}
      </ul>
      {props.children}
    </div>
  ),
  Background: () => null,
  Controls: () => null,
  Handle: () => null,
  Position: { Left: "left", Right: "right", Top: "top", Bottom: "bottom" },
  MarkerType: { ArrowClosed: "arrowclosed" },
}));

vi.mock("../hooks/useElkLayout", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../hooks/useElkLayout")>();
  return {
    ...actual,
    useElkLayout: () => ({ positions: {}, ready: false }),
  };
});

afterEach(() => {
  vi.restoreAllMocks();
});

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/declarations/:name" element={<Declarations />} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("Declarations", () => {
  it("N=0: renders a single declaration with no links", async () => {
    const focusSpy = vi
      .spyOn(client, "getDeclarationFocus")
      .mockResolvedValue(FOCUS_DISTANCE_0);

    renderAt("/declarations/build?distance=0");

    expect(await screen.findByText("build", { selector: ".decl-node__name" })).toBeInTheDocument();
    expect(screen.queryByText("test", { selector: ".decl-node__name" })).not.toBeInTheDocument();
    expect(screen.getByTestId("react-flow-edges").children).toHaveLength(0);
    expect(await screen.findByTestId("decl-summary")).toHaveTextContent(
      "single declaration · distance 0 · direction both · link both",
    );
    expect(focusSpy).toHaveBeenCalledWith(
      "build",
      { distance: 0, direction: "both", link: "both" },
      expect.anything(),
    );
  });

  it("N=1: renders the center plus its direct neighbours and their links", async () => {
    vi.spyOn(client, "getDeclarationFocus").mockResolvedValue(FOCUS_DISTANCE_1);

    renderAt("/declarations/build?distance=1");

    for (const name of ["build", "test", "publish"]) {
      expect(
        await screen.findByText(name, { selector: ".decl-node__name" }),
      ).toBeInTheDocument();
    }
    expect(screen.queryByText("notify", { selector: ".decl-node__name" })).not.toBeInTheDocument();
    const edgeItems = screen.getByTestId("react-flow-edges").children;
    expect(edgeItems).toHaveLength(2);
    expect(await screen.findByTestId("decl-summary")).toHaveTextContent(
      "chain graph · distance 1",
    );
  });

  it("N=2: renders the full chain, marking must links distinct from can links", async () => {
    vi.spyOn(client, "getDeclarationFocus").mockResolvedValue(FOCUS_DISTANCE_2);

    renderAt("/declarations/build?distance=2");

    for (const name of ["build", "test", "publish", "notify"]) {
      expect(
        await screen.findByText(name, { selector: ".decl-node__name" }),
      ).toBeInTheDocument();
    }
    const edgeList = await screen.findByTestId("react-flow-edges");
    expect(edgeList.children).toHaveLength(3);
    expect(edgeList.querySelectorAll(".decl-edge--must")).toHaveLength(2);
    expect(edgeList.querySelectorAll(".decl-edge--can")).toHaveLength(1);

    // Distance rings render every ring the server returned, grouped.
    expect(await screen.findByText("distance 2", { selector: ".decl-rings__label" })).toBeInTheDocument();
  });

  it("marks the center node distinctly from its neighbours", async () => {
    vi.spyOn(client, "getDeclarationFocus").mockResolvedValue(FOCUS_DISTANCE_1);
    renderAt("/declarations/build?distance=1");

    const buildNode = await screen.findByText("build", { selector: ".decl-node__name" });
    expect(buildNode.closest(".decl-node")).toHaveAttribute("data-center", "true");
    const testNode = await screen.findByText("test", { selector: ".decl-node__name" });
    expect(testNode.closest(".decl-node")).toHaveAttribute("data-center", "false");
  });

  it("changing the distance control refetches focus at the new N", async () => {
    const focusSpy = vi
      .spyOn(client, "getDeclarationFocus")
      .mockImplementation((_name, params) =>
        Promise.resolve(
          focusAtDistance(((params?.distance ?? 0) as 0 | 1 | 2)),
        ),
      );

    renderAt("/declarations/build?distance=0");
    await screen.findByText("build", { selector: ".decl-node__name" });

    fireEvent.change(screen.getByLabelText("Distance"), { target: { value: "2" } });

    await waitFor(() =>
      expect(focusSpy).toHaveBeenLastCalledWith(
        "build",
        { distance: 2, direction: "both", link: "both" },
        expect.anything(),
      ),
    );
    expect(
      await screen.findByText("notify", { selector: ".decl-node__name" }),
    ).toBeInTheDocument();
  });

  it("changing direction/link forwards them to the focus API", async () => {
    const focusSpy = vi
      .spyOn(client, "getDeclarationFocus")
      .mockResolvedValue(FOCUS_DISTANCE_1);

    renderAt("/declarations/build?distance=1");
    await screen.findByText("build", { selector: ".decl-node__name" });

    fireEvent.change(screen.getByLabelText("Direction"), { target: { value: "up" } });
    await waitFor(() =>
      expect(focusSpy).toHaveBeenLastCalledWith(
        "build",
        { distance: 1, direction: "up", link: "both" },
        expect.anything(),
      ),
    );

    fireEvent.change(screen.getByLabelText("Link"), { target: { value: "must" } });
    await waitFor(() =>
      expect(focusSpy).toHaveBeenLastCalledWith(
        "build",
        { distance: 1, direction: "up", link: "must" },
        expect.anything(),
      ),
    );
  });

  it("renders an error notice when the focus request fails", async () => {
    vi.spyOn(client, "getDeclarationFocus").mockRejectedValue(
      new client.ApiError(404, "no declaration named \"ghost\"", "check the declaration name"),
    );

    renderAt("/declarations/ghost");

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "no declaration named \"ghost\"",
    );
  });
});
