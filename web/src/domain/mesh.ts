import type { EventEnvelope, NodeRunListItem, Run, RunState, WorkflowVersion } from "../api/types";
import type { MeshPayload } from "../api/client";

export const CONTROL_PLANE_ID = "control-plane";
export const ACTIVE_MESH_RUN_STATES: ReadonlySet<RunState> = new Set(["created", "running", "waiting"]);

export type MeshNodeKind = "machine" | "control-plane" | "bridge" | "human" | "workflow" | "run";
export type MeshTrace = { surface: "mesh" | "runs" | "node-runs" | "workflows"; row: string };

export interface MeshNode {
  id: string;
  kind: MeshNodeKind;
  label: string;
  sub: string;
  trace: MeshTrace;
  status?: "answering" | "unobserved" | "unsupported" | "failed";
  error?: string;
  versionCount?: number;
  actorKey?: string;
  workflowDigest?: string;
}

export type MeshEdgeRelation = "actor-machine" | "run-actor" | "run-workflow" | "actor-workflow";
export interface MeshEdge { id: string; source: string; target: string; relation: MeshEdgeRelation }
export interface MeshGraph {
  name: string;
  entry: string;
  nodes: MeshNode[];
  edges: MeshEdge[];
  machineCount: number;
  actorCount: number;
  runCount: number;
  probeFailures: number;
  unattributedActors: number;
}

function refKey(ref: string | undefined): string | null {
  if (!ref) return null;
  return ref.replace(/^actor:\/\//, "").replace(/@sha256:.*$/, "");
}

function runActorAttribution(items: NodeRunListItem[]): Map<string, string> {
  const latest = new Map<string, NodeRunListItem>();
  for (const item of items) {
    if (!item.actor_id) continue;
    const prior = latest.get(item.run_id);
    if (!prior || item.updated_at > prior.updated_at) latest.set(item.run_id, item);
  }
  return new Map([...latest].map(([runId, row]) => [runId, row.actor_id!]));
}

/** Assemble typed nodes and only relationships explicitly held by API rows. */
export function assembleMeshGraph(
  mesh: MeshPayload,
  runs: Run[],
  nodeRuns: NodeRunListItem[],
  workflows: WorkflowVersion[],
): MeshGraph {
  const nodes: MeshNode[] = [{
    id: CONTROL_PLANE_ID, kind: "control-plane", label: "control plane",
    sub: mesh.version, trace: { surface: "mesh", row: "version" },
  }];
  const edges: MeshEdge[] = [];

  const workersByHost = new Map(mesh.workers.map((row) => [row.hostname, row]));
  for (const [hostname, machine] of Object.entries(mesh.machines).sort()) {
    const worker = workersByHost.get(hostname);
    nodes.push({
      id: `machine:${hostname}`, kind: "machine", label: hostname,
      sub: worker ? `${worker.actor_keys.length} actor keys · ${worker.revision}` : `${machine.actors.length} actors`,
      trace: { surface: "mesh", row: `machines.${hostname}${worker ? `; workers.${worker.worker_id}` : ""}` },
    });
  }

  const approvers = new Set<string>();
  for (const workflow of workflows) {
    for (const node of Object.values(workflow.normalized_ir.spec.nodes)) {
      const approver = refKey(node.approverRef);
      if (approver) approvers.add(approver);
    }
  }
  const actorIds = new Map<string, string>();
  let probeFailures = 0;
  let unattributedActors = 0;
  for (const actor of mesh.actors) {
    const id = `actor:${actor.actor_key}`;
    actorIds.set(actor.actor_key, id);
    const isHuman = approvers.has(actor.actor_key) || actor.actor_key.startsWith("human/") || actor.actor_key.startsWith("human-");
    if (actor.bridge.class === "failed" || (!actor.bridge.class && actor.bridge.error)) probeFailures++;
    if (actor.machine === null) unattributedActors++;
    nodes.push({
      id, kind: isHuman ? "human" : "bridge", label: actor.actor_key,
      sub: actor.machine ?? "unattributed",
      trace: { surface: "mesh", row: `actors[actor_key=${actor.actor_key}]` },
      status: actor.bridge.class ?? (actor.bridge.error ? "failed" : isHuman ? undefined : "answering"),
      error: actor.bridge.error,
      actorKey: actor.actor_key,
    });
    if (actor.machine !== null && mesh.machines[actor.machine]) {
      edges.push({ id: `actor-machine:${actor.actor_key}:${actor.machine}`, source: id, target: `machine:${actor.machine}`, relation: "actor-machine" });
    }
  }

  const workflowByDigest = new Map<string, string>();
  const versionsByKey = new Map<string, WorkflowVersion[]>();
  for (const workflow of workflows) {
    const list = versionsByKey.get(workflow.workflow_key) ?? [];
    list.push(workflow); versionsByKey.set(workflow.workflow_key, list);
    workflowByDigest.set(workflow.digest, workflow.workflow_key);
  }
  for (const [key, versions] of versionsByKey) {
    const id = `workflow:${key}`;
    const newest = [...versions].sort((a, b) => b.version - a.version)[0];
    nodes.push({ id, kind: "workflow", label: key, sub: `${versions.length} version${versions.length === 1 ? "" : "s"}`, versionCount: versions.length, workflowDigest: newest.digest, trace: { surface: "workflows", row: `workflow_key=${key}` } });
    const refs = new Set<string>();
    for (const version of versions) for (const node of Object.values(version.normalized_ir.spec.nodes)) {
      const keyRef = refKey(node.uses); if (keyRef) refs.add(keyRef);
    }
    for (const keyRef of refs) {
      const actorId = actorIds.get(keyRef); if (!actorId) continue;
      edges.push({ id: `actor-workflow:${keyRef}:${key}`, source: actorId, target: id, relation: "actor-workflow" });
    }
  }

  const attribution = runActorAttribution(nodeRuns);
  const activeRuns = runs.filter((run) => ACTIVE_MESH_RUN_STATES.has(run.state));
  for (const run of activeRuns) {
    const id = `run:${run.id}`;
    nodes.push({ id, kind: "run", label: run.name ?? run.display_hint ?? run.id, sub: run.state, trace: { surface: "runs", row: `runs/${run.id}` }, workflowDigest: run.workflow_digest });
    const actorId = attribution.get(run.id);
    if (actorId) {
      const actorKey = mesh.actors.find((a) => a.actor_key === actorId)?.actor_key ?? actorId;
      const target = actorIds.get(actorKey);
      if (target) edges.push({ id: `run-actor:${run.id}:${actorKey}`, source: id, target, relation: "run-actor" });
    }
    const workflowKey = workflowByDigest.get(run.workflow_digest);
    if (workflowKey) edges.push({ id: `run-workflow:${run.id}:${workflowKey}`, source: id, target: `workflow:${workflowKey}`, relation: "run-workflow" });
  }
  return { name: "mesh", entry: CONTROL_PLANE_ID, nodes, edges, machineCount: Object.keys(mesh.machines).length, actorCount: mesh.actors.length, runCount: activeRuns.length, probeFailures, unattributedActors };
}

const TYPE_PREFIX = "dev.culture.nodes.";
const INBOUND_TYPES = new Set(["attempt.completed", "ledger.record-appended", "ledger.review-committed", "runner.operation-completed", "contract.rejected", "human-task.decided", "node-run.failed"]);
export type MeshEventAction = { kind: "run-added"; runId: string; label: string } | { kind: "run-resolved"; runId: string; outcome: "completed" | "failed" | "cancelled" } | { kind: "pulse"; runId: string; direction: "outbound" | "inbound" } | { kind: "none" };
function eventRunId(envelope: EventEnvelope): string | null { const id = envelope.data?.run_id; return typeof id === "string" && id ? id : typeof envelope.subject === "string" && envelope.subject ? envelope.subject : null; }
export function meshEventAction(envelope: EventEnvelope): MeshEventAction {
  const runId = eventRunId(envelope); if (!runId) return { kind: "none" };
  const type = envelope.type.startsWith(TYPE_PREFIX) ? envelope.type.slice(TYPE_PREFIX.length) : envelope.type;
  if (type === "run.created") { const key = envelope.data?.workflow_key; return { kind: "run-added", runId, label: typeof key === "string" && key ? key : runId }; }
  if (type === "run.completed") return { kind: "run-resolved", runId, outcome: "completed" };
  if (type === "run.failed" || type === "run.bounded") return { kind: "run-resolved", runId, outcome: "failed" };
  if (type === "run.cancelled") return { kind: "run-resolved", runId, outcome: "cancelled" };
  return { kind: "pulse", runId, direction: INBOUND_TYPES.has(type) ? "inbound" : "outbound" };
}
export function needsAttributionRefresh(type: string): boolean { return type === `${TYPE_PREFIX}node-run.ready` || type === `${TYPE_PREFIX}attempt.completed`; }
