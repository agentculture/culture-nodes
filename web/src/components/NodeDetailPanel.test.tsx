import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import NodeDetailPanel from "./NodeDetailPanel";
import type { Usage } from "../api/types";
import { parseWorkflowGraph } from "../domain/graph";
import { executionFromRunView, idleExecution } from "../domain/run-state";
import { LEDGER_RECORDS, RUN_VIEW, WORKFLOW_IR } from "../fixtures/run-fixture";

const graph = parseWorkflowGraph(WORKFLOW_IR);
const executions = executionFromRunView(RUN_VIEW);

function renderPanel(nodeId: string, onClose = vi.fn(), usage?: Usage) {
  const node = graph.nodes.find((candidate) => candidate.id === nodeId);
  if (!node) throw new Error(`fixture has no node ${nodeId}`);
  const result = render(
    <NodeDetailPanel
      node={node}
      execution={executions[nodeId] ?? idleExecution(nodeId)}
      ledger={LEDGER_RECORDS}
      onClose={onClose}
      usage={usage}
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

  describe("usage (task t5)", () => {
    it("says plainly when usage data was not found for this node run", () => {
      renderPanel("verify");
      expect(
        screen.getByText("Usage data is not available for this node run."),
      ).toBeInTheDocument();
      expect(document.getElementById("node-detail-usage")).toBeNull();
    });

    it("shows token totals as the primary figure when usage is present", () => {
      const usage: Usage = {
        input_tokens: 12300,
        output_tokens: 4100,
        cost: 0.42,
        currency: "USD",
        attempts_reported: 1,
        attempts_not_reported: 0,
      };
      renderPanel("verify", vi.fn(), usage);
      const summary = document.getElementById("node-detail-usage");
      expect(summary).not.toBeNull();
      expect(summary).toHaveTextContent("12.3k in / 4.1k out");
      expect(summary).toHaveTextContent("0.42 USD");
    });

    it("renders the not-reported state, never '0 tokens', when attempts_reported is 0", () => {
      const usage: Usage = {
        input_tokens: 0,
        output_tokens: 0,
        attempts_reported: 0,
        attempts_not_reported: 1,
      };
      renderPanel("verify", vi.fn(), usage);
      expect(
        document.getElementById("node-detail-usage"),
      ).toHaveAttribute("data-usage-reported", "false");
      expect(screen.getByText("usage not reported")).toBeInTheDocument();
      expect(screen.queryByText(/0 in \/ 0 out/)).not.toBeInTheDocument();
    });
  });
});
