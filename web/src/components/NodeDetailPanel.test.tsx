import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import NodeDetailPanel from "./NodeDetailPanel";
import type { Attempt, LedgerRecord, Usage } from "../api/types";
import { parseWorkflowGraph } from "../domain/graph";
import {
  executionFromRunView,
  idleExecution,
  type NodeExecution,
} from "../domain/run-state";
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

  it("renders per-attempt model and token attribution without hiding explicit unknowns", () => {
    const attempt: Attempt = {
      id: "att-attributed",
      node_run_id: "nr-attributed",
      attempt_number: 1,
      status: "succeeded",
      started_at: "2026-08-15T10:00:00Z",
      completed_at: "2026-08-15T10:01:00Z",
      usage: {
        input_tokens: 120,
        output_tokens: 45,
        cached_input_tokens: 20,
        reasoning_tokens: 8,
        usage_model: "unknown:colleague-backend-cannot-report",
      },
      termination_reason: "end_turn",
      continuation_ref: "session://opaque",
    };
    render(
      <NodeDetailPanel
        node={graph.nodes.find((n) => n.id === "build")!}
        execution={{
          nodeId: "build",
          // NodeExecState is the node-scoped vocabulary ("completed"), not the
          // attempt-scoped one ("succeeded") -- see RunStateChip's note on the
          // three separate vocabularies.
          state: "completed",
          nodeRuns: [],
          attempts: [attempt],
          visits: 1,
        }}
        ledger={[]}
        onClose={vi.fn()}
      />,
    );
    expect(
      screen.getByText("unknown:colleague-backend-cannot-report"),
    ).toBeInTheDocument();
    expect(screen.getByText("120 in / 45 out")).toBeInTheDocument();
    // The separator lives inside the same text node (` · 8 reasoning`), so an
    // exact-string match would fail on the bullet rather than on the number.
    expect(screen.getByText(/8 reasoning/)).toBeInTheDocument();
  });

  describe("preserve branch (task t26)", () => {
    // Synthetic executions built directly (bypassing RUN_VIEW, which has no
    // failed attempt in its fixture) — NodeExecution is a plain interface,
    // and this keeps the assertion focused on preserve rendering alone.
    function failedAttempt(overrides: Partial<Attempt> = {}): Attempt {
      return {
        id: "att-preserve-1",
        node_run_id: "nr-preserve",
        attempt_number: 1,
        status: "failed",
        started_at: "2026-08-13T10:00:00Z",
        completed_at: "2026-08-13T10:04:00Z",
        ...overrides,
      };
    }

    function executionWith(attempt: Attempt): NodeExecution {
      return {
        nodeId: "build",
        state: "failed",
        nodeRuns: [],
        attempts: [attempt],
        visits: 1,
      };
    }

    it("shows no preserve branch for an attempt that reported none", () => {
      const { container } = render(
        <NodeDetailPanel
          node={graph.nodes.find((n) => n.id === "build")!}
          execution={executionWith(failedAttempt())}
          ledger={[]}
          onClose={vi.fn()}
        />,
      );
      const row = container.querySelector('[data-attempt-id="att-preserve-1"]')!;
      expect(row).toHaveTextContent("—");
      expect(row.querySelector("[data-preserve-status]")).toBeNull();
    });

    it("renders a pushed branch as pushed, distinguishable from local-only", () => {
      render(
        <NodeDetailPanel
          node={graph.nodes.find((n) => n.id === "build")!}
          execution={executionWith(
            failedAttempt({
              preserve_branch: "preserve/run-01J-att-01K",
              preserve_pushed: true,
              preserve_remote: "origin",
            }),
          )}
          ledger={[]}
          onClose={vi.fn()}
        />,
      );
      const status = screen.getByText(/pushed to origin/);
      expect(status.classList.contains("detail-panel__preserve-status")).toBe(
        true,
      );
      expect(
        status.closest("[data-preserve-status]"),
      ).toHaveAttribute("data-preserve-status", "pushed");
      expect(screen.getByText("preserve/run-01J-att-01K")).toBeInTheDocument();
    });

    it("links a pushed branch when a forge URL template is configured", () => {
      render(
        <NodeDetailPanel
          node={graph.nodes.find((n) => n.id === "build")!}
          execution={executionWith(
            failedAttempt({
              preserve_branch: "preserve/run-01J-att-01K",
              preserve_pushed: true,
              preserve_remote: "origin",
            }),
          )}
          ledger={[]}
          onClose={vi.fn()}
        />,
      );
      // Whether a link renders depends on VITE_PRESERVE_BRANCH_URL_TEMPLATE
      // at build time (see NodeDetailPanel's module-level constant) — this
      // suite runs with none configured, so the branch renders as plain
      // text, never a link. See preserve.test.ts for the link-construction
      // logic itself, exercised directly with a template argument.
      const cell = screen
        .getByText("preserve/run-01J-att-01K")
        .closest("td")!;
      expect(cell.querySelector("a")).toBeNull();
    });

    it("renders a local-only branch as local-only, never as a link", () => {
      render(
        <NodeDetailPanel
          node={graph.nodes.find((n) => n.id === "build")!}
          execution={executionWith(
            failedAttempt({
              preserve_branch: "preserve/run-01J-att-01L",
              preserve_pushed: false,
              preserve_remote: "origin",
            }),
          )}
          ledger={[]}
          onClose={vi.fn()}
        />,
      );
      const status = screen.getByText(/local-only/);
      expect(
        status.closest("[data-preserve-status]"),
      ).toHaveAttribute("data-preserve-status", "local-only");
      const cell = screen
        .getByText("preserve/run-01J-att-01L")
        .closest("td")!;
      expect(cell.querySelector("a")).toBeNull();
    });
  });

  // Task t11 (ADR 0012): a node run whose deadline expired and whose actor
  // session reported back afterwards carries TWO attempt records, and neither
  // is deleted. The panel has to say which is the correction and which is the
  // history it replaced — otherwise an operator reads one dispatch as two.
  describe("late-callback supersession (task t11)", () => {
    function reconciledExecution(): NodeExecution {
      return {
        nodeId: "build",
        state: "failed",
        nodeRuns: [],
        attempts: [
          {
            id: "att-timed-out",
            node_run_id: "nr-deadline",
            attempt_number: 1,
            status: "timed_out",
            started_at: "2026-08-15T10:00:00Z",
            completed_at: "2026-08-15T10:30:00Z",
          },
          {
            id: "att-correction",
            node_run_id: "nr-deadline",
            attempt_number: 2,
            status: "failed",
            started_at: "2026-08-15T10:00:00Z",
            completed_at: "2026-08-15T10:31:00Z",
            supersedes: "att-timed-out",
            usage: {
              input_tokens: 4321,
              output_tokens: 1234,
              usage_model: "claude-opus-5[1m]",
            },
          },
        ],
        visits: 1,
      };
    }

    it("labels the correction and the record it supersedes", () => {
      const { container } = render(
        <NodeDetailPanel
          node={graph.nodes.find((n) => n.id === "build")!}
          execution={reconciledExecution()}
          ledger={[]}
          onClose={vi.fn()}
        />,
      );

      const timedOut = container.querySelector('[data-attempt-id="att-timed-out"]')!;
      const correction = container.querySelector(
        '[data-attempt-id="att-correction"]',
      )!;
      expect(timedOut).toHaveAttribute("data-attempt-record", "superseded");
      expect(timedOut).toHaveTextContent("superseded");
      expect(correction).toHaveAttribute("data-attempt-record", "correction");
      expect(correction).toHaveTextContent("corrects #1");

      // The whole point of the correction: what the session actually spent is
      // readable, on the row that is current.
      expect(correction).toHaveTextContent("4321 in / 1234 out");
    });

    it("leaves an ordinary dispatch unlabelled", () => {
      const { container } = render(
        <NodeDetailPanel
          node={graph.nodes.find((n) => n.id === "build")!}
          execution={{
            nodeId: "build",
            state: "failed",
            nodeRuns: [],
            attempts: [
              {
                id: "att-plain",
                node_run_id: "nr-plain",
                attempt_number: 1,
                status: "failed",
                started_at: "2026-08-15T10:00:00Z",
              },
            ],
            visits: 1,
          }}
          ledger={[]}
          onClose={vi.fn()}
        />,
      );
      const row = container.querySelector('[data-attempt-id="att-plain"]')!;
      expect(row).toHaveAttribute("data-attempt-record", "dispatch");
      expect(row).not.toHaveTextContent("corrects");
      expect(row).not.toHaveTextContent("superseded");
    });
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
