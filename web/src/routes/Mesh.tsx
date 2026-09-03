import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Background, Handle, MarkerType, Position, ReactFlow, type BuiltInEdge, type Node, type NodeProps } from "@xyflow/react";
import { setAgentState } from "../agent-state/store";
import { ApiError, getMesh, listNodeRuns, listRuns, listWorkflows, type MeshPayload } from "../api/client";
import type { NodeRunListItem, Run, WorkflowVersion, WorkflowIRNode } from "../api/types";
import CultureNode from "../culture-design/CultureNode";
import ErrorNotice from "../components/ErrorNotice";
import { assembleMeshGraph, meshEventAction, needsAttributionRefresh, type MeshEventAction, type MeshNode } from "../domain/mesh";
import type { GraphNode, WorkflowGraph } from "../domain/graph";
import { NODE_HEIGHT, NODE_WIDTH, useElkLayout } from "../hooks/useElkLayout";
import type { ReactFlowInstance } from "@xyflow/react";
import { useMeshEvents, type MeshEvent } from "../hooks/useMeshEvents";
import { useReducedMotion } from "../hooks/useReducedMotion";

const RESOLVE_LINGER_MS = 3400;
const PAGE_LIMIT = 200;
const EMPTY_MESH: MeshPayload = { actors: [], machines: {}, version: "", workers: [] };

interface MeshFlowData extends Record<string, unknown> { item: MeshNode; selected: boolean; pulseCount: number }
type MeshFlowNode = Node<MeshFlowData, "mesh">;

function FlowCultureNode({ data }: NodeProps<MeshFlowNode>) {
  const raw: WorkflowIRNode = { kind: data.item.kind };
  const node: GraphNode = { id: data.item.label, kind: data.item.kind, ownerRef: data.item.sub, outcomes: [], raw, depth: 0 };
  return <><Handle type="target" position={Position.Left} isConnectable={false} /><CultureNode node={node} selected={data.selected} live={data.item.status === "answering"} pulseCount={data.pulseCount} />{data.item.status && data.item.status !== "answering" ? <p className="status-chip status-chip--unknown" title={data.item.error}>{statusWord(data.item.status)} · {data.item.error}</p> : null}<Handle type="source" position={Position.Right} isConnectable={false} /></>;
}
/** The spec's vocabulary for a probe that did not answer (c34/h24): a failed
 *  probe is "unknown", never a healthy word; a bridge with no capability
 *  surface says so; an actor nobody probes is "not probeable". */
function statusWord(status: string | undefined): string {
  if (status === "failed") return "unknown";
  if (status === "unsupported") return "no capability surface";
  if (status === "unobserved") return "not probeable";
  return status ?? "";
}

const NODE_TYPES = { mesh: FlowCultureNode };

export function Mesh() {
  const [payload, setPayload] = useState<MeshPayload>(EMPTY_MESH);
  const [runs, setRuns] = useState<Run[]>([]);
  const [nodeRuns, setNodeRuns] = useState<NodeRunListItem[]>([]);
  const [workflows, setWorkflows] = useState<WorkflowVersion[]>([]);
  const [error, setError] = useState<ApiError | null>(null);
  const [eventsTotal, setEventsTotal] = useState(0);
  const [pulsesTotal, setPulsesTotal] = useState(0);
  const [pulseByRun, setPulseByRun] = useState<Record<string, number>>({});
  const [inspectedId, setInspectedId] = useState<string | null>(null);
  const removalTimers = useRef(new Map<string, ReturnType<typeof setTimeout>>());
  const reducedMotion = useReducedMotion();

  useEffect(() => {
    const controller = new AbortController(); setAgentState({ status: "loading", run: null });
    Promise.all([getMesh(controller.signal), listRuns(controller.signal, { sort: "updated_at" }), listNodeRuns(controller.signal, { limit: PAGE_LIMIT }), listWorkflows(controller.signal)])
      .then(([mesh, runList, nrList, workflowList]) => { if (controller.signal.aborted) return; setPayload(mesh); setRuns(runList.items); setNodeRuns(nrList.items); setWorkflows(workflowList.items); resolveSnapshot(); setAgentState({ status: "ready", run: null }); })
      .catch((cause: unknown) => { if (controller.signal.aborted) return; setError(cause instanceof ApiError ? cause : new ApiError(0, String(cause), "check the browser console")); resolveSnapshot(); setAgentState({ status: "ready", run: null }); });
    return () => controller.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const applyAction = useCallback((action: MeshEventAction, occurredAt: string) => {
    if (action.kind === "run-added") setRuns((prev) => prev.some((run) => run.id === action.runId) ? prev : [...prev, { id: action.runId, workflow_digest: "", state: "created", created_at: occurredAt, updated_at: occurredAt, display_hint: action.label }]);
    if (action.kind === "run-resolved" && !removalTimers.current.has(action.runId)) {
      removalTimers.current.set(action.runId, setTimeout(() => { removalTimers.current.delete(action.runId); setRuns((prev) => prev.filter((run) => run.id !== action.runId)); }, RESOLVE_LINGER_MS));
    }
    if (action.kind === "pulse" || action.kind === "run-resolved") { setPulsesTotal((n) => n + 1); setPulseByRun((counts) => ({ ...counts, [action.runId]: (counts[action.runId] ?? 0) + 1 })); }
  }, []);
  const onEvent = useCallback((event: MeshEvent) => { setEventsTotal((n) => n + 1); applyAction(meshEventAction(event.envelope), event.envelope.time); if (needsAttributionRefresh(event.envelope.type)) listNodeRuns(undefined, { limit: PAGE_LIMIT }).then((x) => setNodeRuns(x.items)).catch(() => {}); }, [applyAction]);
  const { status, lastEventId, resolveSnapshot } = useMeshEvents(onEvent);

  const graph = useMemo(() => assembleMeshGraph(payload, runs, nodeRuns, workflows), [payload, runs, nodeRuns, workflows]);
  const layoutGraph: WorkflowGraph = useMemo(() => ({ name: graph.name, entry: graph.entry, nodes: graph.nodes.map((n, i) => ({ id: n.id, kind: n.kind, ownerRef: n.sub, outcomes: [], raw: { kind: n.kind }, depth: n.kind === "machine" || n.kind === "control-plane" ? 0 : n.kind === "bridge" || n.kind === "human" ? 1 : n.kind === "workflow" ? 2 : 3 + i * 0 })), edges: graph.edges.map((e) => ({ ...e, outcome: e.relation, loop: false })) }), [graph]);
  const { positions, ready: layoutReady } = useElkLayout(layoutGraph);
  const instanceRef = useRef<ReactFlowInstance<MeshFlowNode, BuiltInEdge> | null>(null);
  // React Flow's own fitView runs once, on mount, against the fallback
  // layout; refit when ELK's positions land so the whole mesh is in view.
  useEffect(() => {
    if (!layoutReady) return;
    const frame = requestAnimationFrame(() => { instanceRef.current?.fitView({ padding: 0.1, maxZoom: 1 }); });
    return () => cancelAnimationFrame(frame);
  }, [layoutReady, positions]);
  const nodes: MeshFlowNode[] = graph.nodes.map((item) => ({ id: item.id, type: "mesh", position: positions[item.id] ?? { x: 0, y: 0 }, width: NODE_WIDTH, height: NODE_HEIGHT, draggable: false, data: { item, selected: item.id === inspectedId, pulseCount: item.kind === "run" ? pulseByRun[item.id.slice(4)] ?? 0 : 0 } }));
  const edges: BuiltInEdge[] = graph.edges.map((edge) => { const runId = edge.source.startsWith("run:") ? edge.source.slice(4) : ""; const hasActorEdge = graph.edges.some((candidate) => candidate.source === edge.source && candidate.relation === "run-actor"); const relevant = edge.relation === "run-actor" || (edge.relation === "run-workflow" && !hasActorEdge); const pulse = relevant ? pulseByRun[runId] ?? 0 : 0; return { id: edge.id, source: edge.source, target: edge.target, label: edge.relation, className: `mesh-edge mesh-edge--${edge.relation}`, animated: pulse > 0 && !reducedMotion, markerEnd: { type: MarkerType.ArrowClosed, width: 14, height: 14 }, data: { pulse } }; });

  useEffect(() => { setAgentState({ mesh: { machine_count: graph.machineCount, actor_count: graph.actorCount, run_count: graph.runCount, edge_count: graph.edges.length, probe_failures: graph.probeFailures, unattributed_actors: graph.unattributedActors, connection: status, last_event_id: lastEventId, events_total: eventsTotal, pulses_total: pulsesTotal, reduced_motion: reducedMotion } }); }, [graph, status, lastEventId, eventsTotal, pulsesTotal, reducedMotion]);
  useEffect(() => () => { setAgentState({ mesh: undefined }); for (const timer of removalTimers.current.values()) clearTimeout(timer); }, []);

  const inspected = graph.nodes.find((node) => node.id === inspectedId);
  const onKeyDown = (event: React.KeyboardEvent<HTMLDivElement>) => { if (event.key === "Escape") return setInspectedId(null); if (event.key !== "ArrowRight" && event.key !== "ArrowLeft") return; event.preventDefault(); const index = graph.nodes.findIndex((node) => node.id === inspectedId); const delta = event.key === "ArrowRight" ? 1 : -1; setInspectedId(graph.nodes[(index + delta + graph.nodes.length) % graph.nodes.length]?.id ?? null); };
  return <section className="view-rail mesh-view"><header className="mesh-view__head"><div><h1>Mesh</h1><p className="muted">Machines, bridges, people, published workflows and active runs, joined only by committed relationships.</p></div><p id="mesh-connection" className="mesh-connection" data-state={status} role="status"><span className="mesh-connection__dot" aria-hidden="true" />{status === "live" ? "live" : "reconnecting"}</p></header>{error ? <ErrorNotice error={error} /> : null}
    <div id="mesh-canvas" className="mesh-canvas canvas-surface layout-canvas" data-motion={reducedMotion ? "static" : "animated"} data-layout-ready={layoutReady} tabIndex={0} onKeyDown={onKeyDown} style={{ height: 620 }}>
      <ReactFlow nodes={nodes} edges={edges} nodeTypes={NODE_TYPES} fitView fitViewOptions={{ padding: 0.1, maxZoom: 1 }} onInit={(instance) => { instanceRef.current = instance; }} nodesFocusable onNodeMouseEnter={(_, node) => setInspectedId(node.id)} onNodeClick={(_, node) => setInspectedId(node.id)}><Background /></ReactFlow>
    </div>
    <aside id="mesh-tooltip" className="mesh-tooltip" hidden={!inspected} role="status">{inspected ? <><span className="mesh-tooltip__label">{inspected.label}</span><span className="mesh-tooltip__sub">{inspected.kind} · traces to {inspected.trace.surface}: {inspected.trace.row}{inspected.status && inspected.status !== "answering" ? ` · ${statusWord(inspected.status)} · ${inspected.error}` : ""}</span></> : null}</aside>
    <ul className="mesh-legend" aria-label="Mesh legend">{["machine", "control plane / bridge", "human", "workflow (by key)", "active run"].map((x) => <li key={x}>{x}</li>)}</ul>
    {graph.actorCount === 0 && !error ? <p className="muted" id="mesh-empty">No actors registered yet.</p> : null}
  </section>;
}
export default Mesh;
