import { describe, expect, it } from "vitest";
import type { MeshPayload } from "../api/client";
import type { NodeRunListItem, Run, WorkflowVersion } from "../api/types";
import { assembleMeshGraph, meshEventAction } from "./mesh";
const mesh: MeshPayload = { version: "v1", workers: [{ worker_id: "w1", hostname: "thor", revision: "a", actor_keys: ["company/dev"], last_seen: "now" }, { worker_id: "w2", hostname: "orin", revision: "b", actor_keys: ["company/review", "human/ori"], last_seen: "now" }], machines: { thor: { actors: ["company/dev"] }, orin: { actors: ["company/review"] } }, actors: [{ id: "actor-dev-row", actor_key: "company/dev", machine: "thor", bridge: { observed_at: "now", class: "failed", error: "probe timed out", failure_count: 1 } }, { id: "actor-review-row", actor_key: "company/review", machine: "orin", bridge: { observed_at: "now", deployment: {} } }, { id: "actor-ori-row", actor_key: "human/ori", machine: null, bridge: { observed_at: "now", class: "unsupported", reason: "GET capabilities: 404 Not Found", error: "GET capabilities: 404 Not Found" } }] };
const workflow = (digest: string, version: number): WorkflowVersion => ({ id: `wv${version}`, workflow_key: "ship", version, digest, source: "", source_format: "yaml", created_at: "now", normalized_ir: { spec: { entry: "work", nodes: { work: { kind: "agent", uses: "actor://company/dev@sha256:old" }, approve: { kind: "approval", approverRef: "actor://human/ori" } }, edges: [] } } });
const run: Run = { id: "r1", workflow_digest: "d3", state: "running", created_at: "now", updated_at: "now" };
const EMPTY_USAGE = { input_tokens: 0, output_tokens: 0, cached_input_tokens: 0, reasoning_tokens: 0, attempts_reported: 0, attempts_not_reported: 0 };
/** The form the API actually returns: the actors-table row id (attempts.actor_id). */
const nr: NodeRunListItem = { id: "nr1", run_id: "r1", node_id: "work", actor_id: "actor-dev-row", state: "running", created_at: "now", updated_at: "now", usage: EMPTY_USAGE };
const nrWith = (actorId: string): NodeRunListItem => ({ ...nr, actor_id: actorId });
describe("assembleMeshGraph", () => {
  it("builds typed nodes, groups versions, and emits only recorded relations", () => { const graph = assembleMeshGraph(mesh, [run], [nr], [workflow("d1", 1), workflow("d2", 2), workflow("d3", 3)]); expect(graph.nodes.map((n) => n.kind)).toEqual(expect.arrayContaining(["machine", "control-plane", "bridge", "human", "workflow", "run"])); expect(graph.nodes.find((n) => n.kind === "workflow")?.versionCount).toBe(3); expect(graph.edges.map((e) => e.relation).sort()).toEqual(["actor-machine", "actor-machine", "actor-workflow", "run-actor", "run-workflow"].sort()); });
  it("counts only failed probes and keeps unsupported actors unattributed", () => { const graph = assembleMeshGraph(mesh, [], [], []); expect(graph.nodes.find((n) => n.actorKey === "company/dev")).toMatchObject({ status: "failed", error: "probe timed out" }); expect(graph.nodes.find((n) => n.actorKey === "human/ori")).toMatchObject({ status: "unsupported", sub: "unattributed" }); expect(graph).toMatchObject({ machineCount: 2, probeFailures: 1, unattributedActors: 1 }); expect(graph.nodes.find((n) => n.label === "thor")?.sub).not.toBe(graph.nodes.find((n) => n.label === "orin")?.sub); });
});
/**
 * A node run's actor reference is not one shape: the API reports
 * attempts.actor_id (the actors-table row id), while a workflow-authored
 * reference may arrive bare or digest-pinned. Every one of them must resolve
 * to the same mesh actor — keying on actor_key alone dropped the edge for the
 * only form production actually emits (PR #292 review).
 */
it.each([
  ["the actors-table row id", "actor-dev-row"],
  ["a bare actor key", "company/dev"],
  ["a URI reference", "actor://company/dev"],
  ["a digest-pinned URI reference", "actor://company/dev@sha256:abc123"],
])("resolves run attribution given %s", (_label, actorId) => {
  const graph = assembleMeshGraph(mesh, [run], [nrWith(actorId)], [workflow("d3", 3)]);
  const edge = graph.edges.find((e) => e.relation === "run-actor");
  expect(edge).toMatchObject({ source: "run:r1", target: "actor:company/dev" });
});

it("emits no run-actor edge for an actor the mesh does not hold", () => {
  const graph = assembleMeshGraph(mesh, [run], [nrWith("actor://nobody/here")], [workflow("d3", 3)]);
  expect(graph.edges.some((e) => e.relation === "run-actor")).toBe(false);
});

it("maps one committed event to one relevant run pulse", () => { expect(meshEventAction({ id: "e", source: "x", specversion: "1", type: "dev.culture.nodes.ledger.record-appended", subject: "r1", time: "now", datacontenttype: "application/json", data: { run_id: "r1" } })).toEqual({ kind: "pulse", runId: "r1", direction: "inbound" }); });
