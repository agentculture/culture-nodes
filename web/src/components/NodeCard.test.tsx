import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import NodeCard, { bandForZoom, type DetailBand } from "../culture-design/CultureNode";
import { parseWorkflowGraph } from "../domain/graph";
import {
  executionFromRunView,
  idleExecution,
  type NodeExecState,
  type NodeExecution,
} from "../domain/run-state";
import { RUN_VIEW, WORKFLOW_IR } from "../fixtures/run-fixture";

const graph = parseWorkflowGraph(WORKFLOW_IR);
const executions = executionFromRunView(RUN_VIEW);
const nodeOf = (id: string) => {
  const node = graph.nodes.find((candidate) => candidate.id === id);
  if (!node) throw new Error(`fixture has no node ${id}`);
  return node;
};

function renderCard(
  nodeId: string,
  overrides: {
    band?: DetailBand;
    execution?: Partial<NodeExecution>;
    reducedMotion?: boolean;
    selected?: boolean;
    onOpen?: (id: string) => void;
    hasEvidence?: boolean;
  } = {},
) {
  const base = executions[nodeId] ?? idleExecution(nodeId);
  return render(
    <NodeCard
      node={nodeOf(nodeId)}
      execution={{ ...base, ...overrides.execution }}
      band={overrides.band ?? "medium"}
      selected={overrides.selected ?? false}
      reducedMotion={overrides.reducedMotion ?? false}
      onOpen={overrides.onOpen ?? (() => {})}
      hasEvidence={overrides.hasEvidence}
    />,
  );
}

describe("bandForZoom", () => {
  it("selects the PRD §8.5 detail bands by zoom", () => {
    expect(bandForZoom(0.3)).toBe("far");
    expect(bandForZoom(0.8)).toBe("medium");
    expect(bandForZoom(1.4)).toBe("close");
  });
});

describe("NodeCard progressive detail", () => {
  it("shows only topology at distant zoom", () => {
    renderCard("intake", { band: "far" });
    expect(screen.getByText("intake")).toBeInTheDocument();
    expect(screen.queryByText("agent")).not.toBeInTheDocument();
    expect(screen.queryByText("completed")).not.toBeInTheDocument();
  });

  it("still surfaces a failure at distant zoom, because §8.5 says failures read from far out", () => {
    renderCard("build", {
      band: "far",
      execution: { state: "failed" as NodeExecState },
    });
    expect(screen.getByText("failed")).toBeInTheDocument();
  });

  it("shows name, kind, state and owner at medium zoom", () => {
    renderCard("intake", { band: "medium" });
    expect(screen.getByText("intake")).toBeInTheDocument();
    expect(screen.getByText("agent")).toBeInTheDocument();
    expect(screen.getByText("completed")).toBeInTheDocument();
    expect(screen.getByText("team/platform-ai")).toBeInTheDocument();
  });

  it("adds attempt count and actor at close zoom", () => {
    renderCard("build", { band: "close" });
    expect(screen.getByText("attempts")).toBeInTheDocument();
    expect(screen.getByText(/^2 \(/)).toBeInTheDocument();
    expect(screen.getByText("actor")).toBeInTheDocument();
    expect(screen.getByText("actor://company/developer")).toBeInTheDocument();
  });

  it("labels a code node's executor a runner, not an actor", () => {
    renderCard("test", { band: "close" });
    expect(screen.getByText("runner")).toBeInTheDocument();
    expect(screen.getByText("image")).toBeInTheDocument();
  });

  it("shows the visit count once a node has been entered more than once", () => {
    renderCard("build");
    expect(screen.getByText("visit 2")).toBeInTheDocument();
  });
});

describe("NodeCard execution-state overlays", () => {
  const cases: Array<[NodeExecState, string]> = [
    ["ready", "ready"],
    ["active", "running"],
    ["waiting", "waiting"],
    ["completed", "completed"],
    ["failed", "failed"],
    ["policy_denied", "policy denied"],
    ["cancelled", "cancelled"],
  ];

  it.each(cases)("renders %s with an icon and a word, never colour alone", (state, label) => {
    const { container } = renderCard("build", { execution: { state } });
    const card = container.querySelector<HTMLElement>(".node-card");
    expect(card?.dataset.nodeState).toBe(state);
    expect(screen.getByText(label)).toBeInTheDocument();
    // The glyph rides alongside the word and is hidden from assistive tech.
    const icon = container.querySelector(".status-chip__icon");
    expect(icon).toHaveAttribute("aria-hidden", "true");
    expect(icon?.textContent).toBeTruthy();
  });

  it("badges a waiting node rather than relying on the border alone", () => {
    renderCard("human-review", { execution: { state: "waiting" } });
    expect(screen.getByText("awaiting signal")).toBeInTheDocument();
  });
});

describe("NodeCard evidence marker (task t11)", () => {
  it("carries data-node-evidence regardless of band, so a selector doesn't depend on zoom", () => {
    const { container } = renderCard("build", { band: "far", hasEvidence: true });
    expect(
      container.querySelector('.node-card[data-node-evidence="true"]'),
    ).toBeInTheDocument();
  });

  it("defaults to false when the caller does not know (never a fabricated claim)", () => {
    const { container } = renderCard("build", { band: "medium" });
    expect(
      container.querySelector('.node-card[data-node-evidence="false"]'),
    ).toBeInTheDocument();
    expect(screen.queryByText("evidence")).not.toBeInTheDocument();
  });

  it("shows a visible evidence badge at medium/close zoom when hasEvidence is true", () => {
    renderCard("build", { band: "medium", hasEvidence: true });
    expect(screen.getByText("evidence")).toBeInTheDocument();
  });

  it("does not show the visible badge at far zoom — that band renders topology/failure only", () => {
    const { container } = renderCard("build", { band: "far", hasEvidence: true });
    expect(container.querySelector(".node-card__badge--evidence")).toBeNull();
  });
});

describe("NodeCard reduced motion", () => {
  it("pulses an active attempt when motion is allowed", () => {
    const { container } = renderCard("build", { reducedMotion: false });
    expect(container.querySelector(".culture-node__pulse")).toBeInTheDocument();
    expect(screen.queryByText("attempt in flight")).not.toBeInTheDocument();
  });

  it("replaces the pulse with a badge when motion is reduced", () => {
    const { container } = renderCard("build", { reducedMotion: true });
    expect(container.querySelector(".culture-node__pulse")).not.toBeInTheDocument();
    expect(screen.getByText("attempt in flight")).toBeInTheDocument();
  });

  it("never pulses a node that is not running", () => {
    const { container } = renderCard("intake", { reducedMotion: false });
    expect(container.querySelector(".culture-node__pulse")).not.toBeInTheDocument();
  });
});

describe("NodeCard accessibility", () => {
  it("exposes each node as a button named node <name>, <kind>, <state>", () => {
    renderCard("intake");
    expect(
      screen.getByRole("button", { name: "node intake, agent, completed" }),
    ).toBeInTheDocument();
  });

  it("is in the tab order and opens on Enter", async () => {
    const onOpen = vi.fn();
    const user = userEvent.setup();
    renderCard("verify", { onOpen });
    await user.tab();
    const card = screen.getByRole("button", {
      name: "node verify, agent, completed",
    });
    expect(card).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(onOpen).toHaveBeenCalledWith("verify");
  });

  it("opens on Space as well, the other button activation key", async () => {
    const onOpen = vi.fn();
    const user = userEvent.setup();
    renderCard("verify", { onOpen });
    await user.tab();
    await user.keyboard(" ");
    expect(onOpen).toHaveBeenCalledWith("verify");
  });

  it("reports selection with aria-pressed", () => {
    renderCard("intake", { selected: true });
    expect(
      screen.getByRole("button", { name: /node intake/ }),
    ).toHaveAttribute("aria-pressed", "true");
  });

  it("carries a data-node-id an agent can select on", () => {
    const { container } = renderCard("intake");
    expect(container.querySelectorAll('[data-node-id="intake"]')).toHaveLength(1);
  });
});
