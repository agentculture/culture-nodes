import { describe, expect, it } from "vitest";
import { RUN_EVENTS, RUN_VIEW } from "../fixtures/run-fixture";
import type { NodeRunState } from "../api/types";
import {
  applyEvent,
  executionFromRunView,
  NODE_STATE_ICON,
  NODE_STATE_LABEL,
  nodeRunStateToExecState,
  shortEventType,
  type RunGraphState,
} from "./run-state";

const fold = (): RunGraphState =>
  RUN_EVENTS.reduce(applyEvent, {
    nodes: executionFromRunView(RUN_VIEW),
    walkedEdges: new Set<string>(),
  });

describe("executionFromRunView", () => {
  const nodes = executionFromRunView(RUN_VIEW);

  it("derives a presentation state per node from its node runs", () => {
    expect(nodes.intake.state).toBe("completed");
    expect(nodes.verify.state).toBe("completed");
    expect(nodes.build.state).toBe("active");
  });

  it("collapses a node's repeat visits into one record, keeping every attempt", () => {
    expect(nodes.build.nodeRuns).toHaveLength(2);
    expect(nodes.build.attempts.map((a) => a.id)).toEqual([
      "att-build-1",
      "att-build-2",
    ]);
    expect(nodes.build.visits).toBe(2);
  });

  it("reports no execution for a node no token reached", () => {
    expect(nodes.finish).toBeUndefined();
  });

  it("distinguishes policy_denied from a plain failure", () => {
    const denied = executionFromRunView({
      ...RUN_VIEW,
      node_runs: [
        {
          id: "nr-x",
          node_id: "build",
          state: "failed",
          visit_count: 1,
          created_at: "2026-08-09T09:00:00Z",
          attempts: [
            {
              id: "att-x",
              node_run_id: "nr-x",
              attempt_number: 1,
              status: "policy_denied",
              started_at: "2026-08-09T09:00:00Z",
            },
          ],
        },
      ],
    });
    expect(denied.build.state).toBe("policy_denied");
  });
});

describe("applyEvent", () => {
  it("marks an edge walked only once a token has transitioned it", () => {
    const state = fold();
    expect(state.walkedEdges.has("intake.completed->plan")).toBe(true);
    // The loop the run actually took.
    expect(state.walkedEdges.has("verify.changes_required->build")).toBe(true);
    // A declared but untaken path stays unwalked — and therefore dashed.
    expect(state.walkedEdges.has("verify.blocked->human-review")).toBe(false);
    expect(state.walkedEdges.has("test.failed->build")).toBe(false);
  });

  it("leaves the folded node states agreeing with the snapshot", () => {
    const state = fold();
    expect(state.nodes.intake.state).toBe("completed");
    expect(state.nodes.test.state).toBe("completed");
    expect(state.nodes.build.state).toBe("active");
  });

  it("keeps the attempt records the snapshot supplied", () => {
    const state = fold();
    expect(state.nodes.build.attempts).toHaveLength(2);
  });

  it("does not mutate the state it was given", () => {
    const before: RunGraphState = { nodes: {}, walkedEdges: new Set() };
    applyEvent(before, RUN_EVENTS[4]);
    expect(before.nodes).toEqual({});
    expect(before.walkedEdges.size).toBe(0);
  });

  it("ignores an event type it does not model", () => {
    const before: RunGraphState = { nodes: {}, walkedEdges: new Set() };
    const after = applyEvent(before, {
      sequence: "99",
      envelope: {
        id: "x",
        source: "nodes",
        specversion: "1.0",
        type: "dev.culture.nodes.something.new",
        time: "2026-08-09T09:00:00Z",
        datacontenttype: "application/json",
        data: { node_id: "build" },
      },
    });
    expect(after.nodes).toEqual({});
  });
});

describe("state vocabulary", () => {
  it("gives every state both a word and a glyph, so colour is never alone", () => {
    for (const key of Object.keys(NODE_STATE_LABEL) as (keyof typeof NODE_STATE_LABEL)[]) {
      expect(NODE_STATE_LABEL[key]).toBeTruthy();
      expect(NODE_STATE_ICON[key]).toBeTruthy();
    }
  });

  it("shortens event types for display", () => {
    expect(shortEventType("dev.culture.nodes.token.transitioned")).toBe(
      "token.transitioned",
    );
  });
});

describe("nodeRunStateToExecState", () => {
  it("maps every NodeRunState value onto a renderable NodeExecState", () => {
    const cases: [NodeRunState, string][] = [
      ["ready", "ready"],
      ["leased", "active"],
      ["running", "active"],
      ["waiting_external", "waiting"],
      ["completed", "completed"],
      ["failed", "failed"],
      ["cancelled", "cancelled"],
    ];
    for (const [input, expected] of cases) {
      expect(nodeRunStateToExecState(input)).toBe(expected);
    }
  });

  it("has an icon and label for the result of every mapping (StatusChip never renders blank)", () => {
    const inputs: NodeRunState[] = [
      "ready",
      "leased",
      "running",
      "waiting_external",
      "completed",
      "failed",
      "cancelled",
    ];
    for (const input of inputs) {
      const mapped = nodeRunStateToExecState(input);
      expect(NODE_STATE_LABEL[mapped]).toBeTruthy();
      expect(NODE_STATE_ICON[mapped]).toBeTruthy();
    }
  });
});
