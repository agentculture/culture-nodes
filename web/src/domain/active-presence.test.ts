import { describe, expect, it } from "vitest";
import type { EventEnvelope, NodeRunListItem, Run } from "../api/types";
import {
  ACTIVE_NODE_RUN_STATES,
  ACTIVE_RUN_STATES,
  deriveActiveGraphs,
  needsPresenceRefresh,
  presenceEventAction,
} from "./active-presence";
import {
  DELIVER_CHANGE_V2_DIGEST,
  ORPHAN_DIGEST,
  WORKFLOW_VERSIONS,
  WORKFLOWS_RUNS,
} from "../fixtures/workflows-fixture";
import { WORKFLOW_DIGEST } from "../fixtures/run-fixture";
import {
  ACTIVE_NODE_ID,
  ACTIVE_NODE_RUNS,
  ACTIVE_RUN_ID,
  UNKNOWN_RUN_ID,
} from "../fixtures/active-graphs-fixture";

/**
 * The Active Graphs derivation (task t31, c31/h20): liveness is a readout
 * of committed rows — non-terminal runs pin graphs, non-terminal node runs
 * pin nodes — and the event mapping renders exactly one pulse per committed
 * event on a known run, a no-op for anything else (h14).
 */

const envelope = (
  type: string,
  runId: string | undefined,
  data: Record<string, unknown> = {},
): EventEnvelope => ({
  id: "01TESTEVENT000000000000001",
  source: "nodes",
  specversion: "1.0",
  type: `dev.culture.nodes.${type}`,
  subject: runId,
  time: "2026-08-13T00:00:00Z",
  datacontenttype: "application/json",
  data: runId ? { run_id: runId, ...data } : { ...data },
});

const KNOWN = new Set(WORKFLOWS_RUNS.map((run) => run.id));

describe("deriveActiveGraphs", () => {
  it("renders one graph per digest with a non-terminal run, and nothing for terminal or orphan runs", () => {
    const graphs = deriveActiveGraphs(
      WORKFLOW_VERSIONS,
      WORKFLOWS_RUNS,
      ACTIVE_NODE_RUNS,
    );
    // Only run-deliver-v2 is non-terminal (running); hello-world's run is
    // completed and the orphan run's digest matches no published version.
    expect(graphs).toHaveLength(1);
    expect(graphs[0].workflowKey).toBe("deliver-change");
    expect(graphs[0].version).toBe(2);
    expect(graphs[0].digest).toBe(DELIVER_CHANGE_V2_DIGEST);
    expect(graphs[0].runIds).toEqual([ACTIVE_RUN_ID]);
  });

  it("uses the graph of the digest the active run actually pins, not the latest version", () => {
    const runOnV1: Run = {
      id: "run-on-old-version",
      workflow_digest: WORKFLOW_DIGEST, // deliver-change v1
      state: "waiting",
      created_at: "2026-08-09T09:00:00Z",
      updated_at: "2026-08-09T09:01:00Z",
    };
    const graphs = deriveActiveGraphs(WORKFLOW_VERSIONS, [runOnV1], []);
    expect(graphs).toHaveLength(1);
    expect(graphs[0].version).toBe(1);
    expect(graphs[0].digest).toBe(WORKFLOW_DIGEST);
  });

  it("splits two active digests of the same workflow_key into two graphs, newest version first", () => {
    const runs: Run[] = [
      {
        id: "run-a",
        workflow_digest: WORKFLOW_DIGEST,
        state: "running",
        created_at: "2026-08-09T09:00:00Z",
        updated_at: "2026-08-09T09:01:00Z",
      },
      {
        id: "run-b",
        workflow_digest: DELIVER_CHANGE_V2_DIGEST,
        state: "created",
        created_at: "2026-08-09T09:02:00Z",
        updated_at: "2026-08-09T09:03:00Z",
      },
    ];
    const graphs = deriveActiveGraphs(WORKFLOW_VERSIONS, runs, []);
    expect(graphs.map((g) => [g.workflowKey, g.version])).toEqual([
      ["deliver-change", 2],
      ["deliver-change", 1],
    ]);
  });

  it("marks a node active only when a non-terminal node run of an active run names it", () => {
    const graphs = deriveActiveGraphs(
      WORKFLOW_VERSIONS,
      WORKFLOWS_RUNS,
      ACTIVE_NODE_RUNS,
    );
    // nr-active-build is running -> active; nr-active-intake completed -> not.
    expect(graphs[0].activeNodeIds).toEqual([ACTIVE_NODE_ID]);
  });

  it("drops node runs naming nodes the graph does not declare, or runs it does not know", () => {
    const nodeRuns: NodeRunListItem[] = [
      {
        ...ACTIVE_NODE_RUNS[0],
        id: "nr-ghost-node",
        node_id: "not-a-declared-node",
      },
      {
        ...ACTIVE_NODE_RUNS[0],
        id: "nr-other-run",
        run_id: "run-hello-01J8XKWORKFLOWS0001", // terminal run, not active
      },
    ];
    const graphs = deriveActiveGraphs(
      WORKFLOW_VERSIONS,
      WORKFLOWS_RUNS,
      nodeRuns,
    );
    expect(graphs[0].activeNodeIds).toEqual([]);
  });

  it("returns no graphs at all when every run is terminal", () => {
    const terminalOnly = WORKFLOWS_RUNS.filter(
      (run) => !ACTIVE_RUN_STATES.has(run.state),
    );
    expect(
      deriveActiveGraphs(WORKFLOW_VERSIONS, terminalOnly, ACTIVE_NODE_RUNS),
    ).toEqual([]);
  });

  it("never renders a graph for a run whose digest matches no published version", () => {
    const orphanActive: Run = {
      id: "run-orphan-live",
      workflow_digest: ORPHAN_DIGEST,
      state: "running",
      created_at: "2026-08-09T09:00:00Z",
      updated_at: "2026-08-09T09:01:00Z",
    };
    expect(deriveActiveGraphs(WORKFLOW_VERSIONS, [orphanActive], [])).toEqual(
      [],
    );
  });

  it("pins the state vocabularies to the openapi enums", () => {
    expect([...ACTIVE_RUN_STATES].sort()).toEqual([
      "created",
      "running",
      "waiting",
    ]);
    expect([...ACTIVE_NODE_RUN_STATES].sort()).toEqual([
      "leased",
      "ready",
      "running",
      "waiting_external",
    ]);
  });
});

describe("presenceEventAction (h14: every pulse traces to a committed event on a known run)", () => {
  it("maps an attempt event on a known run to a pulse naming the event's own node", () => {
    expect(
      presenceEventAction(
        envelope("attempt.started", ACTIVE_RUN_ID, { node_id: "build" }),
        KNOWN,
      ),
    ).toEqual({ kind: "pulse", runId: ACTIVE_RUN_ID, nodeId: "build" });
  });

  it("reads token.transitioned's destination node", () => {
    expect(
      presenceEventAction(
        envelope("token.transitioned", ACTIVE_RUN_ID, {
          from_node: "build",
          to_node: "test",
          outcome: "completed",
        }),
        KNOWN,
      ),
    ).toEqual({ kind: "pulse", runId: ACTIVE_RUN_ID, nodeId: "test" });
  });

  it("pulses with a null node when the payload names none — never a guessed node", () => {
    expect(
      presenceEventAction(envelope("run.waiting", ACTIVE_RUN_ID), KNOWN),
    ).toEqual({ kind: "pulse", runId: ACTIVE_RUN_ID, nodeId: null });
  });

  it("is a no-op for an event naming a run the view never loaded", () => {
    expect(
      presenceEventAction(
        envelope("attempt.started", UNKNOWN_RUN_ID, { node_id: "build" }),
        KNOWN,
      ),
    ).toEqual({ kind: "none" });
  });

  it("is a no-op for an event naming no run at all", () => {
    expect(
      presenceEventAction(envelope("attempt.started", undefined), KNOWN),
    ).toEqual({ kind: "none" });
  });

  it("maps run.created for an unknown run to a refetch request, never a rendered placeholder", () => {
    expect(
      presenceEventAction(
        envelope("run.created", "run-brand-new", { workflow_key: "x" }),
        KNOWN,
      ),
    ).toEqual({ kind: "run-appeared", runId: "run-brand-new" });
  });

  it("maps terminal run events on a known run to run-resolved with the committed state", () => {
    expect(
      presenceEventAction(envelope("run.completed", ACTIVE_RUN_ID), KNOWN),
    ).toEqual({ kind: "run-resolved", runId: ACTIVE_RUN_ID, state: "completed" });
    expect(
      presenceEventAction(envelope("run.failed", ACTIVE_RUN_ID), KNOWN),
    ).toEqual({ kind: "run-resolved", runId: ACTIVE_RUN_ID, state: "failed" });
    expect(
      presenceEventAction(envelope("run.bounded", ACTIVE_RUN_ID), KNOWN),
    ).toEqual({ kind: "run-resolved", runId: ACTIVE_RUN_ID, state: "failed" });
    expect(
      presenceEventAction(envelope("run.cancelled", ACTIVE_RUN_ID), KNOWN),
    ).toEqual({
      kind: "run-resolved",
      runId: ACTIVE_RUN_ID,
      state: "cancelled",
    });
  });
});

describe("needsPresenceRefresh", () => {
  it("asks for a refetch only on events that can change which nodes hold work", () => {
    for (const type of [
      "node-run.ready",
      "node-run.failed",
      "attempt.started",
      "attempt.completed",
      "token.transitioned",
      "token.entered",
    ]) {
      expect(needsPresenceRefresh(`dev.culture.nodes.${type}`)).toBe(true);
    }
    expect(needsPresenceRefresh("dev.culture.nodes.run.created")).toBe(false);
    expect(
      needsPresenceRefresh("dev.culture.nodes.ledger.record-appended"),
    ).toBe(false);
  });
});
