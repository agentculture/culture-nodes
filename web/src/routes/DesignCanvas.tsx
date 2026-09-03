import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Background, Controls, Handle, MarkerType, Position, ReactFlow,
  ReactFlowProvider, applyEdgeChanges, useReactFlow,
  type BuiltInEdge, type Connection, type EdgeChange, type Node, type NodeChange, type NodeProps,
} from "@xyflow/react";
import { useSearchParams } from "react-router-dom";
import { ApiError, listWorkflows, publishWorkflow, validateWorkflow } from "../api/client";
import type { Diagnostic, WorkflowVersion } from "../api/types";
import ErrorNotice from "../components/ErrorNotice";
import SegmentedToggle from "../components/SegmentedToggle";
import CultureNode from "../culture-design/CultureNode";
import { parseWorkflowGraph, type GraphNode } from "../domain/graph";
import { deriveNodeDefinitions } from "../domain/node-catalog";
import { openWorkflowDocument, type WorkflowDocument } from "../domain/workflow-document";
import { parseWorkflowSourceForPreview } from "../domain/workflow-source";
import { useElkLayout } from "../hooks/useElkLayout";

const EMPTY_SOURCE = `apiVersion: nodes.culture.dev/v1alpha1
kind: Workflow
metadata:
  name: untitled
  version: 1.0.0
spec:
  entry: start
  nodes:
    start:
      kind: agent
  edges: []
`;
const KINDS = ["agent", "code", "decision", "approval", "wait", "subworkflow", "end"];
const DEBOUNCE_MS = 350;

type CanvasData = Record<string, unknown> & { graphNode: GraphNode; selected: boolean; diagnostics: Diagnostic[]; onSelect: (id: string) => void };
type CanvasNode = Node<CanvasData, "culture">;

function EditableNode({ data }: NodeProps<CanvasNode>) {
  return <><Handle type="target" position={Position.Left} isConnectable /><CultureNode node={data.graphNode} selected={data.selected} onOpen={data.onSelect} /><Handle type="source" position={Position.Right} isConnectable />{data.diagnostics.length ? <span className="canvas-node-diagnostic" aria-label={`Diagnostics for node ${data.graphNode.id}`}>{data.diagnostics.map((d) => <span key={`${d.path}:${d.code}`}>{d.message}</span>)}</span> : null}</>;
}
const NODE_TYPES = { culture: EditableNode };

function fieldsFor(kind: string): Array<{ label: string; key: string; shape?: "array" | "operation" | "until" }> {
  if (kind === "agent") return [{ label: "uses", key: "uses" }];
  if (kind === "code") return [{ label: "runner", key: "uses" }, { label: "operation", key: "operation", shape: "operation" }];
  if (kind === "decision") return [{ label: "rule", key: "rule" }, { label: "outcomes", key: "outcomes", shape: "array" }];
  if (kind === "approval") return [{ label: "authority", key: "approverRef" }, { label: "deadline", key: "deadline" }];
  if (kind === "wait") return [{ label: "signal", key: "until", shape: "until" }];
  if (kind === "subworkflow") return [{ label: "pinned digest", key: "uses" }];
  return [];
}

function DesignCanvasInner({ initialSource }: { initialSource?: string }) {
  const [params] = useSearchParams();
  const [source, setSource] = useState(initialSource ?? params.get("source") ?? EMPTY_SOURCE);
  const documentRef = useRef<WorkflowDocument | null>(null);
  const [versions, setVersions] = useState<WorkflowVersion[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [connectFrom, setConnectFrom] = useState<string | null>(null);
  const [diagnostics, setDiagnostics] = useState<Diagnostic[]>([]);
  const [valid, setValid] = useState(false);
  const [validatedDigest, setValidatedDigest] = useState("");
  const [error, setError] = useState<ApiError | null>(null);
  const [notice, setNotice] = useState("");
  const [pane, setPane] = useState<"properties" | "source">("properties");
  // Positions the person dragged to (or dropped at) win over ELK for the life
  // of the page; everything else lands where ELK puts it, like every canvas.
  const [placed, setPlaced] = useState<Record<string, { x: number; y: number }>>({});
  const previousSource = useRef(source);
  const flow = useReactFlow();

  useEffect(() => { const opened = openWorkflowDocument(source, "yaml"); documentRef.current = opened.ok ? opened.doc : null; }, []);
  useEffect(() => { const controller = new AbortController(); listWorkflows(controller.signal).then((r) => setVersions(r.items)).catch((e) => setError(e)); return () => controller.abort(); }, []);
  useEffect(() => {
    const controller = new AbortController();
    const timer = window.setTimeout(() => validateWorkflow({ format: "yaml", source }, controller.signal).then((result) => { setDiagnostics(result.diagnostics); setValid(result.valid); setValidatedDigest(result.digest); }).catch((cause) => { if (!controller.signal.aborted) setError(cause instanceof ApiError ? cause : new ApiError(0, String(cause), "check the browser console")); }), DEBOUNCE_MS);
    return () => { window.clearTimeout(timer); controller.abort(); };
  }, [source]);

  const graph = useMemo(() => { const ir = parseWorkflowSourceForPreview(source, "yaml"); return ir ? parseWorkflowGraph(ir) : null; }, [source]);
  const { positions: elkPositions, ready: layoutReady } = useElkLayout(graph);
  const nodeDiagnostics = (id: string) => diagnostics.filter((d) => d.path.startsWith(`/spec/nodes/${id}/`) || d.path === `/spec/nodes/${id}`);
  const edgeDiagnostics = (index: number) => diagnostics.filter((d) => d.path.startsWith(`/spec/edges/${index}/`) || d.path === `/spec/edges/${index}`);
  const commit = useCallback((mutation: (doc: WorkflowDocument) => { ok: boolean; reason?: string }) => {
    const doc = documentRef.current; if (!doc) return;
    const result = mutation(doc); if (!result.ok) { setError(new ApiError(0, result.reason ?? "edit refused", "edit the source document")); return; }
    const next = doc.toString(); previousSource.current = source; setSource(next); setNotice("");
  }, [source]);
  const uniqueId = (kind: string) => { const ids = new Set(graph?.nodes.map((n) => n.id)); let id = kind; let i = 2; while (ids.has(id)) id = `${kind}-${i++}`; return id; };
  const addKind = (kind: string, at?: { x: number; y: number }) => { const id = uniqueId(kind); commit((doc) => doc.addNode(id, { kind })); if (at) setPlaced((prev) => ({ ...prev, [id]: { x: Math.round(at.x), y: Math.round(at.y) } })); setSelected(id); setPane("properties"); };
  const selectNode = (id: string) => { if (connectFrom && connectFrom !== id) { commit((doc) => doc.addEdge({ from: `${connectFrom}.completed`, to: id })); setConnectFrom(null); } setSelected(id); };
  const nodes: CanvasNode[] = (graph?.nodes ?? []).map((node, index) => ({ id: node.id, type: "culture", position: placed[node.id] ?? elkPositions[node.id] ?? { x: 60 + (index % 3) * 260, y: 50 + Math.floor(index / 3) * 150 }, data: { graphNode: node, selected: selected === node.id, diagnostics: nodeDiagnostics(node.id), onSelect: selectNode } }));
  const edges: BuiltInEdge[] = (graph?.edges ?? []).map((edge, index) => ({ id: edge.id, source: edge.source, target: edge.target, label: edge.outcome, markerEnd: { type: MarkerType.ArrowClosed }, data: { diagnostics: edgeDiagnostics(index) } }));
  const connect = (connection: Connection) => { if (connection.source && connection.target) commit((doc) => doc.addEdge({ from: `${connection.source}.completed`, to: connection.target })); };
  const removeSelected = () => { if (!selected) return; commit((doc) => doc.removeNode(selected)); setSelected(null); };
  const selectedNode = graph?.nodes.find((n) => n.id === selected);
  const changeProp = (key: string, value: string, shape?: "array" | "operation" | "until") => commit((doc) => doc.setNodeProp(selected!, key, shape === "array" ? value.split(",").map((v) => v.trim()).filter(Boolean) : shape === "operation" ? { image: value } : shape === "until" ? { signal: value } : value));
  const publish = async () => { setError(null); try { const existed = versions.some((v) => v.digest === validatedDigest); await publishWorkflow({ format: "yaml", source }); setNotice(existed ? "no semantic change — this version already exists; your comments live in your file" : `Published ${validatedDigest}`); } catch (cause) { setError(cause instanceof ApiError ? cause : new ApiError(0, String(cause), "check the browser console")); } };
  const definitions = deriveNodeDefinitions(versions);
  const paletteKinds = [...new Set([...definitions.map((d) => d.kind), ...KINDS])];
  const downloadHref = `data:text/yaml;charset=utf-8,${encodeURIComponent(source)}`;

  return <section className="view-rail design-canvas-view"><header><h1>Design canvas</h1><div><button type="button" onClick={removeSelected} disabled={!selected}>Delete selected</button><button type="button" onClick={() => selected && setConnectFrom(selected)} disabled={!selected}>Connect from selected</button><button type="button" onClick={publish} disabled={!valid}>Publish</button></div></header>{error ? <ErrorNotice error={error} /> : null}{notice ? <p role="status">{notice} <a href={downloadHref} download="workflow.yaml">Download</a></p> : null}
    <div className="design-canvas-layout"><aside aria-label="Node palette" role="region"><h2>Palette</h2>{paletteKinds.map((kind) => <button key={kind} type="button" draggable onDragStart={(e) => e.dataTransfer.setData("application/x-node-kind", kind)} onClick={() => addKind(kind)} onKeyDown={(e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); addKind(kind); } }} aria-label={`Add ${kind}`}>{kind}</button>)}</aside>
      <div role="application" aria-label="Workflow canvas" className="canvas-surface design-canvas__canvas" style={{ height: 520 }} onDragOver={(e) => e.preventDefault()} onDrop={(e) => { e.preventDefault(); const kind = e.dataTransfer.getData("application/x-node-kind"); if (kind) addKind(kind, flow.screenToFlowPosition({ x: e.clientX, y: e.clientY })); }} data-layout-ready={layoutReady}><ReactFlow nodes={nodes} edges={edges} nodeTypes={NODE_TYPES} onConnect={connect} onNodesChange={(changes: NodeChange<CanvasNode>[]) => { for (const change of changes) if (change.type === "position" && change.position) { const at = change.position; setPlaced((prev) => ({ ...prev, [change.id]: { x: Math.round(at.x), y: Math.round(at.y) } })); } }} onEdgesChange={(changes: EdgeChange<BuiltInEdge>[]) => applyEdgeChanges(changes, edges)} nodesConnectable deleteKeyCode={null} fitView fitViewOptions={{ padding: 0.12, maxZoom: 1 }}><Background /><Controls /></ReactFlow></div>
      <aside aria-label="Inspector"><SegmentedToggle label="Inspector pane"><button type="button" aria-pressed={pane === "properties"} onClick={() => setPane("properties")}>Properties</button><button type="button" aria-pressed={pane === "source"} onClick={() => setPane("source")}>Source</button></SegmentedToggle>{pane === "source" ? <pre aria-label="Workflow source" className="culture-source">{sourceLines(previousSource.current, source).map((line, i) => <span key={i} className={line.inserted ? "culture-source__ins" : undefined}>{line.text}{i < source.split("\n").length - 1 ? "\n" : ""}</span>)}</pre> : selectedNode ? <form aria-label={`${selectedNode.kind} properties`} onSubmit={(e) => e.preventDefault()}><h2>{selectedNode.id}</h2>{fieldsFor(selectedNode.kind).map((field) => <label key={field.key}>{field.label}<input aria-label={field.label} onChange={(e) => changeProp(field.key, e.target.value, field.shape)} /></label>)}</form> : <p>Select a node.</p>}</aside>
    </div><p className="design-canvas__status" aria-label="Canvas status" data-valid={valid}>{statusLine(valid, diagnostics.length, graph, validatedDigest)}</p><section aria-label="Diagnostics">{diagnostics.map((d) => <p key={`${d.path}:${d.code}`} data-diagnostic-path={d.path}>{d.message}</p>)}</section></section>;
}

/** Lines of `next`, each flagged when it does not appear in `previous` — the
 *  demo's inserted-line highlight, computed on text alone so comments and
 *  order are compared exactly as the author wrote them. */
function sourceLines(previous: string, next: string): Array<{ text: string; inserted: boolean }> {
  const seen = new Map<string, number>();
  for (const line of previous.split("\n")) seen.set(line, (seen.get(line) ?? 0) + 1);
  return next.split("\n").map((text) => {
    const left = seen.get(text) ?? 0;
    if (left > 0) { seen.set(text, left - 1); return { text, inserted: false }; }
    return { text, inserted: previous !== next };
  });
}

function statusLine(valid: boolean, diagnosticCount: number, graph: { nodes: unknown[]; edges: unknown[] } | null, digest: string): string {
  if (!graph) return "source does not parse yet";
  if (valid) return `valid · ${graph.nodes.length} nodes · ${graph.edges.length} edges${digest ? ` · digest ${digest}` : ""}`;
  return `${diagnosticCount} diagnostic${diagnosticCount === 1 ? "" : "s"} — see below`;
}

/** The canvas needs React Flow's provider for the drop-point conversion, so
 *  the exported component always renders inside one. */
export function DesignCanvas(props: { initialSource?: string }) { return <ReactFlowProvider><DesignCanvasInner {...props} /></ReactFlowProvider>; }

export default function DesignCanvasRoute(props: { initialSource?: string }) { return <DesignCanvas {...props} />; }
