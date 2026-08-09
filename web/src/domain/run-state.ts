import type {
  Attempt,
  NodeRun,
  NodeRunState,
  RunEvent,
  RunView,
} from "../api/types";

/**
 * The execution states the canvas overlays onto a node (PRD §8.4). These are
 * *presentation* states derived from node-run/attempt records — they are not
 * a second state machine, and nothing here ever invents a state the control
 * plane did not report.
 *
 * `policy_denied` is broken out of `failed` deliberately: §8.4 requires
 * "waiting, blocked, failed, and policy-denied states are distinct".
 */
export type NodeExecState =
  | "idle"
  | "ready"
  | "active"
  | "waiting"
  | "completed"
  | "failed"
  | "policy_denied"
  | "cancelled";

export interface NodeExecution {
  nodeId: string;
  state: NodeExecState;
  /** Node runs for this node, oldest first. More than one means a loop. */
  nodeRuns: NodeRun[];
  /** Every attempt across every node run, oldest first. */
  attempts: Attempt[];
  /** The actor/runner that ran the most recent attempt, when known. */
  actorId?: string;
  /** The outcome the most recent completed node run reported. */
  outcome?: string;
  /** How many times a token has entered this node. */
  visits: number;
}

export interface RunGraphState {
  nodes: Record<string, NodeExecution>;
  /** Edge ids (`source.outcome->target`) a token has actually traversed. */
  walkedEdges: Set<string>;
}

const IDLE: NodeExecution = {
  nodeId: "",
  state: "idle",
  nodeRuns: [],
  attempts: [],
  visits: 0,
};

/** An empty execution record for a node no token has reached yet. */
export function idleExecution(nodeId: string): NodeExecution {
  return { ...IDLE, nodeId };
}

function byCreatedAt(a: { created_at: string }, b: { created_at: string }) {
  return a.created_at < b.created_at ? -1 : a.created_at > b.created_at ? 1 : 0;
}

function stateFor(nodeRun: NodeRun, attempts: Attempt[]): NodeExecState {
  switch (nodeRun.state) {
    case "ready":
      return "ready";
    case "leased":
    case "running":
      return "active";
    case "waiting_external":
      return "waiting";
    case "completed":
      return "completed";
    case "cancelled":
      return "cancelled";
    case "failed": {
      const last = attempts[attempts.length - 1];
      return last?.status === "policy_denied" ? "policy_denied" : "failed";
    }
    default:
      return "idle";
  }
}

/**
 * Map a `NodeRunListItem`'s raw `NodeRunState` (the cross-run `GET
 * /v1alpha1/node-runs` listing, task t11/t15) onto the same `NodeExecState`
 * vocabulary `StatusChip` already renders everywhere else. Unlike `stateFor`
 * above, a list item carries no attempts, so a `failed` row cannot be split
 * into `failed` vs `policy_denied` here — that distinction needs an
 * attempt's status, which this flat listing does not nest. Every other
 * value maps straight across; `leased` and `running` both read as `active`,
 * exactly as `stateFor` treats them.
 */
export function nodeRunStateToExecState(state: NodeRunState): NodeExecState {
  switch (state) {
    case "ready":
      return "ready";
    case "leased":
    case "running":
      return "active";
    case "waiting_external":
      return "waiting";
    case "completed":
      return "completed";
    case "failed":
      return "failed";
    case "cancelled":
      return "cancelled";
    default:
      return "idle";
  }
}

/**
 * Fold a RunView into per-node execution records. The RunView is a snapshot
 * (`GET /v1alpha1/runs/{id}`); the event stream then advances it.
 */
export function executionFromRunView(view: RunView): Record<string, NodeExecution> {
  const byNode = new Map<string, NodeRun[]>();
  for (const nodeRun of [...view.node_runs].sort(byCreatedAt)) {
    const list = byNode.get(nodeRun.node_id);
    if (list) list.push(nodeRun);
    else byNode.set(nodeRun.node_id, [nodeRun]);
  }

  const out: Record<string, NodeExecution> = {};
  for (const [nodeId, nodeRuns] of byNode) {
    const attempts = nodeRuns
      .flatMap((nodeRun) => nodeRun.attempts ?? [])
      .sort((a, b) =>
        a.started_at < b.started_at ? -1 : a.started_at > b.started_at ? 1 : 0,
      );
    const current = nodeRuns[nodeRuns.length - 1];
    const lastAttempt = attempts[attempts.length - 1];
    out[nodeId] = {
      nodeId,
      state: stateFor(current, attempts),
      nodeRuns,
      attempts,
      actorId: lastAttempt?.actor_id,
      outcome: current.outcome,
      visits: current.visit_count || nodeRuns.length,
    };
  }
  return out;
}

const str = (value: unknown): string | undefined =>
  typeof value === "string" && value !== "" ? value : undefined;

/**
 * Advance a graph state by one committed event.
 *
 * Only committed runtime events drive overlays (PRD §8.4) — this function is
 * the single place that translates one into a visual change, and it returns
 * a new object rather than mutating so React re-renders predictably.
 */
export function applyEvent(
  prev: RunGraphState,
  event: RunEvent,
): RunGraphState {
  const { type, data } = event.envelope;
  const nodeId = str(data.node_id);
  const nodes = { ...prev.nodes };
  const walkedEdges = prev.walkedEdges;

  const touch = (id: string, patch: Partial<NodeExecution>) => {
    const base = nodes[id] ?? idleExecution(id);
    nodes[id] = { ...base, ...patch };
  };

  switch (type) {
    case "dev.culture.nodes.node-run.ready":
    case "dev.culture.nodes.token.entered": {
      if (nodeId) {
        const visit = typeof data.visit === "number" ? data.visit : undefined;
        touch(nodeId, {
          state: "ready",
          visits: visit ?? (nodes[nodeId]?.visits ?? 0) + 1,
        });
      }
      break;
    }
    case "dev.culture.nodes.attempt.started":
    case "dev.culture.nodes.actor.accepted": {
      if (nodeId) {
        touch(nodeId, { state: "active", actorId: str(data.actor_id) });
      }
      break;
    }
    case "dev.culture.nodes.attempt.completed": {
      if (nodeId) {
        const tech = str(data.tech_status);
        const state: NodeExecState =
          tech === "succeeded"
            ? "completed"
            : tech === "policy_denied"
              ? "policy_denied"
              : tech === "cancelled"
                ? "cancelled"
                : "failed";
        touch(nodeId, {
          state,
          outcome: str(data.outcome),
          actorId: str(data.actor_id) ?? nodes[nodeId]?.actorId,
        });
      }
      break;
    }
    case "dev.culture.nodes.contract.rejected": {
      if (nodeId) touch(nodeId, { state: "failed" });
      break;
    }
    case "dev.culture.nodes.token.transitioned": {
      const from = str(data.from_node);
      const to = str(data.to_node);
      const outcome = str(data.outcome) ?? "";
      if (from && to) {
        const next = new Set(walkedEdges);
        next.add(`${from}.${outcome}->${to}`);
        touch(from, { state: nodes[from]?.state ?? "completed" });
        touch(to, {
          state: "ready",
          visits:
            typeof data.visit === "number"
              ? data.visit
              : (nodes[to]?.visits ?? 0) + 1,
        });
        return { nodes, walkedEdges: next };
      }
      break;
    }
    case "dev.culture.nodes.run.waiting": {
      if (nodeId) touch(nodeId, { state: "waiting" });
      break;
    }
    case "dev.culture.nodes.run.completed": {
      const endNode = str(data.end_node);
      if (endNode) touch(endNode, { state: "completed" });
      break;
    }
    default:
      break;
  }

  return { nodes, walkedEdges };
}

/** Human-facing label for an execution state — never color alone (§8.8). */
export const NODE_STATE_LABEL: Record<NodeExecState, string> = {
  idle: "not reached",
  ready: "ready",
  active: "running",
  waiting: "waiting",
  completed: "completed",
  failed: "failed",
  policy_denied: "policy denied",
  cancelled: "cancelled",
};

/**
 * A text glyph per state. Paired with the label in every chip so status
 * survives both monochrome rendering and color-vision differences (§8.8:
 * "status labels and icons in addition to color").
 */
export const NODE_STATE_ICON: Record<NodeExecState, string> = {
  idle: "·",
  ready: "○",
  active: "◐",
  waiting: "⏸",
  completed: "✔",
  failed: "✕",
  policy_denied: "⛔",
  cancelled: "⊘",
};

/** The terminal run event types — the stream closes after one of these. */
export const TERMINAL_EVENT_TYPES = new Set([
  "dev.culture.nodes.run.completed",
  "dev.culture.nodes.run.failed",
  "dev.culture.nodes.run.cancelled",
  "dev.culture.nodes.run.bounded",
]);

/** Strip the `dev.culture.nodes.` prefix for display. */
export function shortEventType(type: string): string {
  return type.replace(/^dev\.culture\.nodes\./, "");
}
