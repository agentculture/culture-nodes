import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useParams } from "react-router-dom";
import {
  Background,
  Controls,
  MarkerType,
  ReactFlow,
  ReactFlowProvider,
  type BuiltInEdge,
  type ReactFlowInstance,
} from "@xyflow/react";
import { setAgentState } from "../agent-state/store";
import { DASHED, SOLID } from "../culture-design/edges";
import CategoryChip from "../components/CategoryChip";
import ErrorNotice from "../components/ErrorNotice";
import EventTimeline from "../components/EventTimeline";
import NodeDetailPanel from "../components/NodeDetailPanel";
import StatusChip from "../components/StatusChip";
import UsageSummary from "../components/UsageSummary";
import WorkflowNode, {
  LOOP_SOURCE_HANDLE,
  LOOP_TARGET_HANDLE,
  type WorkflowFlowNode,
} from "../components/WorkflowNode";
import { NODE_HEIGHT, NODE_WIDTH, useElkLayout } from "../hooks/useElkLayout";
import { useReducedMotion } from "../hooks/useReducedMotion";
import { useRunEvents } from "../hooks/useRunEvents";
import {
  applyEvent,
  executionFromRunView,
  idleExecution,
  type RunGraphState,
} from "../domain/run-state";
import { mergeUsage, runDisplayName } from "../domain/usage";
import { useRunData } from "./useRunData";

const NODE_TYPES = { workflow: WorkflowNode };
const PAN_STEP = 64;
const EMPTY_WALKED: ReadonlySet<string> = new Set<string>();

type ViewMode = "graph" | "timeline";

function RunViewInner() {
  const { id: runId } = useParams<{ id: string }>();
  const { view, graph, ledger, usageByNodeRunId, loading, error } =
    useRunData(runId);
  const { events, status: streamStatus } = useRunEvents(runId);
  const reducedMotion = useReducedMotion();

  const [selected, setSelected] = useState<string | null>(null);
  const [mode, setMode] = useState<ViewMode>("graph");
  const instanceRef = useRef<ReactFlowInstance<WorkflowFlowNode, BuiltInEdge> | null>(
    null,
  );
  const openerRef = useRef<HTMLElement | null>(null);

  /**
   * Snapshot first, then every committed event folded on top in order. The
   * snapshot carries the records events do not (attempts, node runs); the
   * events carry what the snapshot cannot (which edges a token actually
   * walked). Neither invents state the control plane did not report.
   */
  const graphState: RunGraphState = useMemo(() => {
    const base: RunGraphState = {
      nodes: view ? executionFromRunView(view) : {},
      walkedEdges: new Set<string>(),
    };
    return events.reduce(applyEvent, base);
  }, [view, events]);

  const walkedEdges = graphState.walkedEdges ?? EMPTY_WALKED;

  const openNode = useCallback((nodeId: string) => {
    openerRef.current =
      document.activeElement instanceof HTMLElement
        ? document.activeElement
        : null;
    setSelected(nodeId);
  }, []);

  const closeDetail = useCallback(() => {
    setSelected(null);
    // Focus goes back where it came from — an Escape that strands focus on
    // <body> makes a keyboard user restart their walk (PRD §8.8).
    const opener = openerRef.current;
    if (opener?.isConnected) opener.focus();
    openerRef.current = null;
  }, []);

  const flowNodes: WorkflowFlowNode[] = useMemo(() => {
    if (!graph) return [];
    return graph.nodes.map((node) => ({
      id: node.id,
      type: "workflow" as const,
      position: { x: 0, y: 0 },
      width: NODE_WIDTH,
      height: NODE_HEIGHT,
      draggable: false,
      selectable: false,
      connectable: false,
      data: {
        node,
        execution: graphState.nodes[node.id] ?? idleExecution(node.id),
        isSelected: selected === node.id,
        reducedMotion,
        onOpen: openNode,
      },
    }));
  }, [graph, graphState, selected, reducedMotion, openNode]);

  const { positions, ready: layoutReady } = useElkLayout(graph);

  // React Flow's `fitView` runs once, on mount — against the fallback layout
  // that renders while ELK is still working. Fit again when the real layout
  // lands (and when the canvas is remounted by the view toggle), or the graph
  // opens framed on whatever the seed positions happened to be.
  useEffect(() => {
    if (!layoutReady || mode !== "graph") return;
    const frame = requestAnimationFrame(() => {
      instanceRef.current?.fitView({ padding: 0.08, maxZoom: 1 });
    });
    return () => cancelAnimationFrame(frame);
  }, [layoutReady, positions, mode]);

  const positionedNodes = useMemo(
    () =>
      flowNodes.map((node) => ({
        ...node,
        position: positions[node.id] ?? { x: 0, y: 0 },
      })),
    [flowNodes, positions],
  );

  const flowEdges: BuiltInEdge[] = useMemo(() => {
    if (!graph) return [];
    return graph.edges.map((edge) => {
      const walked = walkedEdges.has(edge.id);
      // culture-design/edges.ts is the whole vocabulary: DASHED until a token
      // has actually walked the edge (a path the graph merely proposes),
      // SOLID once one has (a transition on the record).
      const style = walked ? SOLID : DASHED;
      return {
        id: edge.id,
        source: edge.source,
        target: edge.target,
        sourceHandle: edge.loop ? LOOP_SOURCE_HANDLE : undefined,
        targetHandle: edge.loop ? LOOP_TARGET_HANDLE : undefined,
        type: "smoothstep",
        pathOptions: { borderRadius: 14, offset: edge.loop ? 28 : 12 },
        label: edge.outcome,
        className: [
          "flow-edge",
          walked ? "is-walked" : "is-unwalked",
          edge.loop ? "is-loop" : "",
        ]
          .filter(Boolean)
          .join(" "),
        style: {
          // A walked loop is the one edge that must not read as just another
          // line — §8.7 calls out verify -> build explicitly. Both the stroke
          // and the width are set inline because React Flow puts `style`
          // straight onto the path element, where a stylesheet cannot reach.
          stroke: edge.loop && walked ? "var(--accent)" : style.stroke,
          strokeWidth: edge.loop && walked ? 3.4 : style.strokeWidth,
          strokeDasharray: style.strokeDasharray,
        },
        markerEnd: { type: MarkerType.ArrowClosed, width: 14, height: 14 },
        ariaLabel: `edge ${edge.source} ${edge.outcome} to ${edge.target}, ${
          walked ? "walked" : "not yet walked"
        }${edge.loop ? ", loop" : ""}`,
        data: { walked, loop: edge.loop },
      } satisfies BuiltInEdge;
    });
  }, [graph, walkedEdges]);

  // Keep the machine-readable mirror in step with what is on screen.
  useEffect(() => {
    if (!runId) return;
    const nodeStates: Record<string, string> = {};
    for (const node of graph?.nodes ?? []) {
      nodeStates[node.id] = (graphState.nodes[node.id] ?? idleExecution(node.id))
        .state;
    }
    const usage = view?.run.usage;
    setAgentState({
      status: loading ? "loading" : "ready",
      run: {
        id: runId,
        state: view?.run.state ?? "unknown",
        node_states: nodeStates,
        selected,
        name: view?.run.name ?? null,
        display_hint: view?.run.display_hint ?? null,
        category: view?.run.category ?? null,
        usage: usage
          ? {
              input_tokens: usage.input_tokens,
              output_tokens: usage.output_tokens,
              cost: usage.cost ?? null,
              currency: usage.currency ?? null,
              reported: usage.attempts_reported > 0,
            }
          : null,
      },
    });
  }, [runId, graph, graphState, view, loading, selected]);

  const onCanvasKeyDown = (event: React.KeyboardEvent<HTMLDivElement>) => {
    if (!event.key.startsWith("Arrow")) return;
    const instance = instanceRef.current;
    if (!instance) return;
    event.preventDefault();
    const viewport = instance.getViewport();
    const dx =
      event.key === "ArrowLeft"
        ? PAN_STEP
        : event.key === "ArrowRight"
          ? -PAN_STEP
          : 0;
    const dy =
      event.key === "ArrowUp" ? PAN_STEP : event.key === "ArrowDown" ? -PAN_STEP : 0;
    instance.setViewport({
      x: viewport.x + dx,
      y: viewport.y + dy,
      zoom: viewport.zoom,
    });
  };

  const selectedNode = graph?.nodes.find((node) => node.id === selected);

  // Merged across every node run of the selected node (a loop revisits it
  // more than once) — undefined, not a fabricated Usage, when the
  // best-effort node-runs join found no matching entries at all (see
  // useRunData.ts's usageByNodeRunId doc comment).
  const selectedNodeUsageEntries = selectedNode
    ? (graphState.nodes[selectedNode.id]?.nodeRuns ?? [])
        .map((nodeRun) => usageByNodeRunId[nodeRun.id])
        .filter((usage): usage is NonNullable<typeof usage> => usage !== undefined)
    : [];
  const selectedNodeUsage =
    selectedNodeUsageEntries.length > 0
      ? mergeUsage(selectedNodeUsageEntries)
      : undefined;

  const runDisplay = view ? runDisplayName(view.run) : null;

  if (error) {
    return (
      <section className="view-rail">
        <h1>Run</h1>
        <ErrorNotice error={error} />
        <p>
          <Link to="/runs">Back to runs</Link>
        </p>
      </section>
    );
  }

  return (
    <section className="run-view view-rail">
      <div className="run-view__head">
        <div>
          <h1 className="run-view__title">
            {graph?.name ?? "Run"}{" "}
            <span className="run-view__id">{runId}</span>
          </h1>
          {view && runDisplay ? (
            <p className="run-view__run-name" id="run-view-name">
              <span
                className={`run-name${runDisplay.derived ? " run-name--derived" : ""}`}
                data-derived={runDisplay.derived ? "true" : "false"}
                title={
                  runDisplay.derived
                    ? `derived guess, not a given name: "${runDisplay.text}"`
                    : undefined
                }
              >
                {runDisplay.text}
              </span>
              {view.run.category ? (
                <CategoryChip category={view.run.category} />
              ) : null}
            </p>
          ) : null}
          <p className="run-view__sub muted">
            {graph?.ownerRef ? <>owner {graph.ownerRef} · </> : null}
            workflow digest{" "}
            <code>{view?.run.workflow_digest.slice(0, 24) ?? "—"}…</code>
          </p>
          {view?.run.usage ? (
            <p className="run-view__usage">
              <UsageSummary usage={view.run.usage} id="run-usage-summary" />
            </p>
          ) : null}
        </div>
        <div className="run-view__state">
          <span
            id="run-state-chip"
            className="run-state-chip"
            data-run-state={view?.run.state ?? "unknown"}
          >
            <span aria-hidden="true">◆</span> {view?.run.state ?? "loading"}
          </span>
          <span
            id="stream-status"
            className="stream-status"
            data-stream-status={streamStatus}
          >
            stream: {streamStatus}
          </span>
          <Link id="ledger-link" to={`/runs/${runId}/ledger`}>
            Ledger
          </Link>
        </div>
      </div>

      <div
        id="view-toggle"
        className="view-toggle"
        role="group"
        aria-label="Run view mode"
      >
        <button
          type="button"
          id="view-toggle-graph"
          aria-pressed={mode === "graph"}
          onClick={() => setMode("graph")}
        >
          Graph
        </button>
        <button
          type="button"
          id="view-toggle-timeline"
          aria-pressed={mode === "timeline"}
          onClick={() => setMode("timeline")}
        >
          Timeline
        </button>
      </div>

      <div className="run-view__body">
        <div className="run-view__primary">
          {mode === "graph" ? (
            <div
              id="run-canvas"
              className="run-canvas"
              onKeyDown={onCanvasKeyDown}
              aria-label={`Workflow graph for run ${runId}. Tab moves between nodes; Enter opens a node's detail; arrow keys pan.`}
              role="application"
            >
              {loading ? (
                <p className="muted" id="run-loading">
                  Loading run…
                </p>
              ) : (
                <ReactFlow
                  nodes={positionedNodes}
                  edges={flowEdges}
                  nodeTypes={NODE_TYPES}
                  onInit={(instance) => {
                    instanceRef.current = instance;
                  }}
                  nodesDraggable={false}
                  nodesConnectable={false}
                  nodesFocusable={false}
                  edgesFocusable={false}
                  elementsSelectable={false}
                  // Required, not decorative: React Flow gives a node
                  // `pointer-events: none` unless it is selectable, draggable
                  // or has a mouse handler — without this, the cards would be
                  // keyboard-reachable but unclickable.
                  onNodeClick={(_, node) => openNode(node.id)}
                  panOnScroll
                  minZoom={0.3}
                  maxZoom={2}
                  fitView
                  fitViewOptions={{ padding: 0.12, maxZoom: 1 }}
                  proOptions={{ hideAttribution: true }}
                >
                  <Background gap={28} size={1} />
                  <Controls showInteractive={false} />
                </ReactFlow>
              )}
            </div>
          ) : (
            <div id="run-node-list" className="node-list">
              <h2>Nodes</h2>
              <table className="ledger-table">
                <thead>
                  <tr>
                    <th scope="col">node</th>
                    <th scope="col">kind</th>
                    <th scope="col">owner</th>
                    <th scope="col">state</th>
                    <th scope="col">visits</th>
                    <th scope="col">attempts</th>
                  </tr>
                </thead>
                <tbody>
                  {(graph?.nodes ?? []).map((node) => {
                    const execution =
                      graphState.nodes[node.id] ?? idleExecution(node.id);
                    return (
                      <tr key={node.id} data-list-node-id={node.id}>
                        <th scope="row">
                          <button
                            type="button"
                            className="link-button"
                            onClick={() => openNode(node.id)}
                          >
                            {node.id}
                          </button>
                        </th>
                        <td>{node.kind}</td>
                        <td>{node.ownerRef ?? "—"}</td>
                        <td>
                          <StatusChip state={execution.state} />
                        </td>
                        <td>{execution.visits}</td>
                        <td>{execution.attempts.length}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}

          <section className="run-view__timeline" aria-labelledby="timeline-heading">
            <h2 id="timeline-heading">Event timeline</h2>
            <EventTimeline
              events={events}
              selectedNodeId={selected}
              onSelectNode={openNode}
            />
          </section>
        </div>

        {selectedNode ? (
          <NodeDetailPanel
            node={selectedNode}
            execution={
              graphState.nodes[selectedNode.id] ?? idleExecution(selectedNode.id)
            }
            ledger={ledger}
            usage={selectedNodeUsage}
            onClose={closeDetail}
          />
        ) : null}
      </div>
    </section>
  );
}

export function RunView() {
  return (
    <ReactFlowProvider>
      <RunViewInner />
    </ReactFlowProvider>
  );
}

export default RunView;
