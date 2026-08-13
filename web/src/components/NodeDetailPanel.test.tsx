import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import NodeDetailPanel from "./NodeDetailPanel";
import type { LedgerRecord, Usage } from "../api/types";
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

  describe("structured workspace evidence (task t11)", () => {
    it("renders changed files, snapshot digest, and artifact refs for a workspace-snapshot evidence record", () => {
      const { container } = renderPanel("build");
      const evidence = container.querySelector("#node-detail-evidence");
      expect(evidence).not.toBeNull();

      const changedPaths = evidence!.querySelector(
        '[data-evidence-changed-paths="true"]',
      );
      expect(changedPaths).not.toBeNull();
      expect(changedPaths?.textContent).toContain("internal/worker/hooks.go");
      expect(changedPaths?.textContent).toContain("internal/runners/dispatch.go");
      expect(changedPaths?.querySelectorAll("li")).toHaveLength(2);

      const digest = evidence!.querySelector(
        '[data-evidence-snapshot-digest="true"]',
      );
      expect(digest?.textContent).toBe(`sha256:${"c".repeat(64)}`);

      const refs = evidence!.querySelector('[data-evidence-artifact-refs="true"]');
      expect(refs?.textContent).toContain("artifact://diff/att-build-1");
      expect(refs?.querySelector("a")).toHaveAttribute(
        "href",
        "artifact://diff/att-build-1",
      );
    });

    it("keeps rendering an evidence record with an unrecognized payload shape exactly as before — no regression", () => {
      const { container } = renderPanel("test");
      const evidence = container.querySelector("#node-detail-evidence");
      // The test node's evidence (process_exit/workspace_diff) is not
      // workspace-snapshot shaped: the generic line still renders (asserted
      // by the pre-existing "surfaces observed evidence" test above), and
      // no structured block is added for it.
      expect(
        evidence?.querySelector('[data-workspace-evidence="true"]'),
      ).toBeNull();
    });

    it("adds no structured block to any evidence record when a node run has none at all", () => {
      renderPanel("plan");
      expect(document.getElementById("node-detail-evidence")).toBeNull();
      expect(
        screen.getByText("No observed evidence is attached to this node run."),
      ).toBeInTheDocument();
    });
  });

  describe("success signals (task t18)", () => {
    function signalRecord(id: string, mechanical: boolean): LedgerRecord {
      return {
        id,
        schema_version: "nodes.culture.dev/ledger/v1alpha1",
        record_type: "success_signal",
        run_id: "run_01ABC",
        node_run_id: "nr-test",
        attempt_id: "att-test-1",
        origin: { kind: "agent", actor_id: "actor://company/intake" },
        authority: "proposed",
        data: {
          statement: mechanical
            ? "the test process exits 0"
            : "the change reads well to a reviewer",
          check: { kind: "process_exit", equals: 0 },
          mechanical,
        },
        provenance_refs: [],
        created_at: "2026-08-13T10:23:26Z",
        content_digest: `sha256:${id.padEnd(40, "0")}`,
      };
    }

    function renderWithSignals() {
      const node = graph.nodes.find((candidate) => candidate.id === "test");
      if (!node) throw new Error("fixture has no node test");
      return render(
        <NodeDetailPanel
          node={node}
          execution={executions.test}
          ledger={[
            ...LEDGER_RECORDS,
            signalRecord("lr-sig-mech", true),
            signalRecord("lr-sig-prose", false),
          ]}
          onClose={vi.fn()}
        />,
      );
    }

    it("marks a mechanical:false signal as not machine-checkable in the ledger delta", () => {
      const { container } = renderWithSignals();
      const prose = container.querySelector('[data-record-id="lr-sig-prose"]');
      expect(prose?.textContent).toContain("success_signal");
      expect(
        prose?.querySelector(
          '[data-signal-checkability="not-machine-checkable"]',
        ),
      ).toHaveTextContent("not machine-checkable");
    });

    it("adds no such mark to a mechanical:true signal — its verdict is the derived evaluation record", () => {
      const { container } = renderWithSignals();
      const mech = container.querySelector('[data-record-id="lr-sig-mech"]');
      expect(mech?.textContent).toContain("success_signal");
      expect(
        mech?.querySelector("[data-signal-checkability]"),
      ).toBeNull();
    });
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
        cached_input_tokens: 0,
        reasoning_tokens: 0,
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
        cached_input_tokens: 0,
        reasoning_tokens: 0,
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
