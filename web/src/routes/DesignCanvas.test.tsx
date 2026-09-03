import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { listWorkflows, publishWorkflow, validateWorkflow } from "../api/client";
import {
  CANVAS_INVALID_VALIDATION,
  CANVAS_SOURCE,
  CANVAS_VALIDATION,
  CANVAS_VERSION,
} from "../fixtures/canvas-fixture";
import DesignCanvas from "./DesignCanvas";

vi.mock("../api/client", async (original) => ({
  ...(await original<typeof import("../api/client")>()),
  listWorkflows: vi.fn(), validateWorkflow: vi.fn(), publishWorkflow: vi.fn(),
}));
vi.mock("@xyflow/react", () => ({
  ReactFlowProvider: ({ children }: { children: React.ReactNode }) => <>{children}</>,
  ReactFlow: ({ nodes, edges }: { nodes: Array<{ id: string; data: { diagnostics?: Array<{ message: string }>; onSelect: (id: string) => void } }>; edges: Array<{ id: string; data?: { diagnostics?: Array<{ message: string }> } }> }) => <div>{nodes.map((n) => <button key={n.id} aria-label={`node ${n.id}`} onClick={() => n.data.onSelect(n.id)} data-diagnostics={n.data.diagnostics?.map((d) => d.message).join("|")}>{n.id}</button>)}{edges.map((e) => <button key={e.id} aria-label={`edge ${e.id}`} data-diagnostics={e.data?.diagnostics?.map((d) => d.message).join("|")}>{e.id}</button>)}</div>,
  Background: () => null, Controls: () => null, Handle: () => null,
  Position: { Left: "left", Right: "right" }, MarkerType: { ArrowClosed: "arrow" },
  applyNodeChanges: (_: unknown, nodes: unknown) => nodes,
  applyEdgeChanges: (_: unknown, edges: unknown) => edges,
  useReactFlow: () => ({ screenToFlowPosition: (point: { x: number; y: number }) => point }),
}));
vi.mock("../hooks/useElkLayout", () => ({ NODE_WIDTH: 224, NODE_HEIGHT: 84, useElkLayout: () => ({ positions: {}, ready: true }) }));

const validate = vi.mocked(validateWorkflow);
const publish = vi.mocked(publishWorkflow);
beforeEach(() => {
  vi.mocked(listWorkflows).mockResolvedValue({ items: [CANVAS_VERSION] });
  validate.mockResolvedValue(CANVAS_VALIDATION);
  publish.mockResolvedValue(CANVAS_VERSION);
});

function view() {
  return render(<MemoryRouter initialEntries={["/design/canvas"]}><DesignCanvas initialSource={CANVAS_SOURCE} /></MemoryRouter>);
}

describe("DesignCanvas", () => {
  it("derives its palette, adds with the keyboard, and exposes each kind's properties", async () => {
    view();
    const palette = await screen.findByRole("region", { name: "Node palette" });
    for (const kind of ["agent", "code", "decision", "approval", "wait", "subworkflow", "end"]) expect(within(palette).getByRole("button", { name: new RegExp(kind) })).toBeInTheDocument();
    fireEvent.keyDown(within(palette).getByRole("button", { name: /code/ }), { key: "Enter" });
    expect(await screen.findByLabelText("runner")).toBeInTheDocument();
    expect(screen.getByLabelText("operation")).toBeInTheDocument();

    const expected = new Map([
      ["start", ["uses"]],
      ["decision", ["rule", "outcomes"]],
      ["approval", ["authority", "deadline"]],
      ["wait", ["signal"]],
      ["child", ["pinned digest"]],
    ]);
    for (const [id, fields] of expected) {
      fireEvent.click(screen.getByLabelText(`node ${id}`));
      for (const field of fields) expect(screen.getByLabelText(field)).toBeInTheDocument();
    }
  });

  it("selects, connects, and deletes through keyboard-operable controls", async () => {
    view();
    await screen.findByRole("region", { name: "Node palette" });
    fireEvent.click(screen.getByLabelText("node start"));
    fireEvent.keyDown(screen.getByRole("button", { name: "Connect from selected" }), { key: "Enter" });
    fireEvent.click(screen.getByRole("button", { name: "Connect from selected" }));
    fireEvent.click(screen.getByLabelText("node decision"));
    fireEvent.click(screen.getByRole("button", { name: "Source" }));
    expect(screen.getByLabelText("Workflow source")).toHaveTextContent("from: start.completed");
    fireEvent.keyDown(screen.getByRole("button", { name: "Delete selected" }), { key: "Enter" });
    fireEvent.click(screen.getByRole("button", { name: "Delete selected" }));
    expect(screen.queryByLabelText("node decision")).not.toBeInTheDocument();
  });

  it("debounces validation, posts the live string, and pins byte-identical diagnostics by JSON path", async () => {
    validate.mockResolvedValue(CANVAS_INVALID_VALIDATION);
    view();
    await waitFor(() => expect(validate.mock.calls[0]?.[0]).toEqual({ format: "yaml", source: CANVAS_SOURCE }), { timeout: 1500 });
    expect(screen.getByText(CANVAS_INVALID_VALIDATION.diagnostics[0].message)).toHaveTextContent(CANVAS_INVALID_VALIDATION.diagnostics[0].message);
    expect(screen.getByLabelText("node start")).toHaveAttribute("data-diagnostics", CANVAS_INVALID_VALIDATION.diagnostics[0].message);
    expect(screen.getByLabelText(/edge start\.completed/)).toHaveAttribute("data-diagnostics", CANVAS_INVALID_VALIDATION.diagnostics[1].message);
  });

  it("publishes the exact live source and reports an existing digest with Download", async () => {
    view();
    await waitFor(() => expect(screen.getByRole("button", { name: "Publish" })).toBeEnabled(), { timeout: 1500 });
    fireEvent.click(screen.getByRole("button", { name: "Publish" }));
    await waitFor(() => expect(publish).toHaveBeenCalledWith({ format: "yaml", source: CANVAS_SOURCE }));
    expect(screen.getByText("no semantic change — this version already exists; your comments live in your file")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Download" })).toHaveAttribute("download");
  });
});
