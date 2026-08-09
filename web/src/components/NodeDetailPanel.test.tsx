import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import NodeDetailPanel from "./NodeDetailPanel";
import { parseWorkflowGraph } from "../domain/graph";
import { executionFromRunView, idleExecution } from "../domain/run-state";
import { LEDGER_RECORDS, RUN_VIEW, WORKFLOW_IR } from "../fixtures/run-fixture";

const graph = parseWorkflowGraph(WORKFLOW_IR);
const executions = executionFromRunView(RUN_VIEW);

function renderPanel(nodeId: string, onClose = vi.fn()) {
  const node = graph.nodes.find((candidate) => candidate.id === nodeId);
  if (!node) throw new Error(`fixture has no node ${nodeId}`);
  const result = render(
    <NodeDetailPanel
      node={node}
      execution={executions[nodeId] ?? idleExecution(nodeId)}
      ledger={LEDGER_RECORDS}
      onClose={onClose}
    />,
  );
  return { ...result, onClose };
}

describe("NodeDetailPanel", () => {
  it("names the node, its kind, owner and executor", () => {
    renderPanel("test");
    expect(
      screen.getByRole("region", { name: "Node detail: test" }),
    ).toBeInTheDocument();
    expect(screen.getByText("code")).toBeInTheDocument();
    expect(screen.getByText("team/developer-experience")).toBeInTheDocument();
    // Once in the facts list, once as the attempt's actor.
    expect(screen.getAllByText("runner://headspace/docker")).toHaveLength(2);
  });

  it("shows the contract digest the node is pinned to", () => {
    renderPanel("intake");
    expect(screen.getByText("sha256:aaa1")).toBeInTheDocument();
  });

  it("lists every attempt with its status and timing", () => {
    renderPanel("build");
    const table = screen.getByRole("table");
    expect(table).toHaveAttribute("id", "node-detail-attempts");
    expect(screen.getByText("succeeded")).toBeInTheDocument();
    expect(screen.getByText("dispatched")).toBeInTheDocument();
  });

  it("shows the ledger delta for this node run only", () => {
    const { container } = renderPanel("intake");
    const delta = container.querySelector("#node-detail-ledger");
    expect(delta?.querySelectorAll("li")).toHaveLength(3);
    // The test node's evidence belongs to a different node run.
    expect(delta?.textContent).not.toContain("sha256:4444");
  });

  it("surfaces observed evidence and its provenance", () => {
    const { container } = renderPanel("test");
    const evidence = container.querySelector("#node-detail-evidence");
    expect(evidence?.textContent).toContain(
      "artifact://workspace/pytest-report.xml",
    );
    expect(evidence?.textContent).toContain("att-test-1");
    expect(
      evidence?.querySelector('.authority-chip[data-authority="observed"]'),
    ).toBeInTheDocument();
  });

  it("says plainly when a node run appended nothing", () => {
    renderPanel("plan");
    expect(
      screen.getByText("This node run has appended no ledger records."),
    ).toBeInTheDocument();
  });

  it("takes focus when it opens", () => {
    renderPanel("verify");
    expect(screen.getByRole("region", { name: "Node detail: verify" })).toHaveFocus();
  });

  it("closes on Escape", async () => {
    const user = userEvent.setup();
    const { onClose } = renderPanel("verify");
    await user.keyboard("{Escape}");
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("closes from its keyboard-reachable close button", async () => {
    const user = userEvent.setup();
    const { onClose } = renderPanel("verify");
    await user.tab();
    expect(screen.getByRole("button", { name: "Close" })).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
