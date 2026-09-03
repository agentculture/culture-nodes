import type {
  Actor,
  EventEnvelope,
  NodeRunListItem,
  Run,
} from "../api/types";

/**
 * The Mesh view's fixture slice (task t18): actors across the kind
 * vocabulary (including a duplicate revision, which the view must collapse
 * to one node), a mix of active and terminal runs, the node-runs rows that
 * attribute runs to actors, and a short committed-event history for the
 * cross-run SSE stream — ULID-shaped ids in generation order, because the
 * events table's own primary key is the stream's resume cursor
 * (internal/api/events.go, handleStreamEvents).
 */

export const MESH_ACTORS: Actor[] = [
  {
    id: "actor-thor-r1",
    actor_key: "codex-thor",
    revision: 1,
    kind: "agent",
    protocol: "http",
    endpoint_ref: "http://thor:8090",
    created_at: "2026-08-01T08:00:00Z",
  },
  {
    id: "actor-thor-r2",
    actor_key: "codex-thor",
    revision: 2,
    kind: "agent",
    protocol: "http",
    endpoint_ref: "http://thor:8091",
    created_at: "2026-08-08T08:00:00Z",
  },
  {
    id: "actor-orin",
    actor_key: "codex-orin",
    revision: 1,
    kind: "agent",
    protocol: "http",
    endpoint_ref: "http://orin:8090",
    created_at: "2026-08-01T09:00:00Z",
  },
  {
    id: "actor-ori",
    actor_key: "ori",
    revision: 1,
    kind: "human",
    protocol: "http",
    created_at: "2026-08-01T07:00:00Z",
  },
  {
    id: "actor-headspace",
    actor_key: "headspace-runner",
    revision: 1,
    kind: "runner",
    protocol: "http",
    endpoint_ref: "http://thor:9400",
    created_at: "2026-08-01T10:00:00Z",
  },
];

/** 5 rows -> 4 mesh nodes (codex-thor's two revisions collapse). */
export const MESH_ACTOR_NODE_COUNT = 4;

export const MESH_RUNS: Run[] = [
  {
    id: "run-mesh-alpha",
    workflow_digest: "sha256:mesh-wf",
    state: "running",
    name: "Review the adapters",
    category: "review",
    created_at: "2026-08-12T09:00:00Z",
    updated_at: "2026-08-12T09:30:00Z",
  },
  {
    id: "run-mesh-beta",
    workflow_digest: "sha256:mesh-wf",
    state: "waiting",
    display_hint: "audit the events surface",
    created_at: "2026-08-12T09:10:00Z",
    updated_at: "2026-08-12T09:20:00Z",
  },
  {
    id: "run-mesh-done",
    workflow_digest: "sha256:mesh-wf",
    state: "completed",
    name: "Yesterday's sweep",
    created_at: "2026-08-11T09:00:00Z",
    updated_at: "2026-08-11T10:00:00Z",
    completed_at: "2026-08-11T10:00:00Z",
  },
];

/** Terminal runs stay off the mesh: 3 fixture runs -> 2 satellites. */
export const MESH_ACTIVE_RUN_COUNT = 2;

const EMPTY_USAGE = {
  input_tokens: 0,
  output_tokens: 0,
  cached_input_tokens: 0,
  reasoning_tokens: 0,
  attempts_reported: 0,
  attempts_not_reported: 1,
};

/**
 * run-mesh-alpha attributes to codex-thor via the OLD revision's actor id —
 * the view must alias it onto the surviving node. run-mesh-beta has no
 * attempt yet (no actor_id), so it honestly links to the control plane.
 */
export const MESH_NODE_RUNS: NodeRunListItem[] = [
  {
    id: "nr-mesh-alpha-build",
    run_id: "run-mesh-alpha",
    node_id: "build",
    actor_id: "actor-thor-r1",
    state: "running",
    created_at: "2026-08-12T09:05:00Z",
    updated_at: "2026-08-12T09:30:00Z",
    usage: EMPTY_USAGE,
  },
  {
    id: "nr-mesh-beta-intake",
    run_id: "run-mesh-beta",
    node_id: "intake",
    state: "ready",
    created_at: "2026-08-12T09:10:00Z",
    updated_at: "2026-08-12T09:10:00Z",
    usage: EMPTY_USAGE,
  },
];

export interface MeshFixtureEvent {
  /** The SSE `id:` field — the events table's ULID primary key. */
  id: string;
  envelope: EventEnvelope;
}

function meshEvent(
  id: string,
  type: string,
  runId: string,
  data: Record<string, unknown>,
): MeshFixtureEvent {
  return {
    id,
    envelope: {
      id,
      source: "nodes",
      specversion: "1.0",
      type: `dev.culture.nodes.${type}`,
      subject: runId,
      time: "2026-08-12T09:31:00Z",
      datacontenttype: "application/json",
      data: { run_id: runId, ...data },
    },
  };
}

/**
 * The committed cross-run history the fixture stream replays, in id order:
 * two interior pulses, one run appearing, one run resolving. Counters a
 * spec can assert: events_total = 4, pulses_total = 3 (two pulses + one
 * resolution; the run.created is an appearance, not a particle).
 */
export const MESH_EVENTS: MeshFixtureEvent[] = [
  meshEvent(
    "01MESH000000000000000001",
    "ledger.record-appended",
    "run-mesh-alpha",
    { node_run_id: "nr-mesh-alpha-build", record_type: "claim" },
  ),
  meshEvent(
    "01MESH000000000000000002",
    "token.transitioned",
    "run-mesh-alpha",
    { from_node: "build", to_node: "verify", outcome: "completed" },
  ),
  meshEvent("01MESH000000000000000003", "run.created", "run-mesh-gamma", {
    workflow_key: "mesh-demo",
    workflow_digest: "sha256:mesh-wf",
    entry: "intake",
  }),
  meshEvent("01MESH000000000000000004", "run.completed", "run-mesh-beta", {
    node_run_id: "nr-mesh-beta-intake",
    end_node: "done",
    transitions: 3,
  }),
];

/** A deliberately large committed history proving `from=latest` is tail-only. */
export const MESH_HISTORICAL_EVENTS: MeshFixtureEvent[] = Array.from(
  { length: 1284 },
  (_, index) =>
    meshEvent(
      `01HISTORY${String(index).padStart(16, "0")}`,
      "ledger.record-appended",
      "run-mesh-alpha",
      { record_type: "claim" },
    ),
);

export const MESH_SNAPSHOT_EVENT = meshEvent(
  "01MESH000000000000000000",
  "stream.snapshot",
  "",
  {},
);

export const MESH_EVENTS_TOTAL = MESH_EVENTS.length;
export const MESH_PULSES_TOTAL = 3;
export const MESH_LAST_EVENT_ID = MESH_EVENTS[MESH_EVENTS.length - 1].id;

/**
 * Serialize fixture events as the cross-run SSE body, exactly as
 * writeCrossRunSSEEvent frames them:
 * `id: <ulid>\nevent: <type>\ndata: <envelope JSON>\n\n`.
 */
export function meshEventsAsSse(events: MeshFixtureEvent[]): string {
  return events
    .map(
      (item) =>
        `id: ${item.id}\nevent: ${item.envelope.type}\ndata: ${JSON.stringify(item.envelope)}\n\n`,
    )
    .join("");
}
