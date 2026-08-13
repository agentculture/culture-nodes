import { useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";
import {
  Background,
  Handle,
  MarkerType,
  Position,
  ReactFlow,
  type BuiltInEdge,
  type Node,
  type NodeProps,
  type ReactFlowInstance,
} from "@xyflow/react";
import { DASHED } from "../culture-design/edges";
import type { ActiveGraphPresence } from "../domain/active-presence";
import { accentFor, type GraphNode } from "../domain/graph";
import { NODE_HEIGHT, NODE_WIDTH, useElkLayout } from "../hooks/useElkLayout";
import { useAnimationGate } from "../hooks/useAnimationGate";

/**
 * ActiveGraphCanvas — one alive workflow graph on the Active Graphs sub-tab
 * (task t31, c31/h20): the published graph an active run actually pins,
 * rendered with the RunView React Flow + ELK stack, overlaid with LIVE
 * presence. The Mesh craft, translated to the CSS-animated DOM substrate
 * (MeshCanvas.tsx:16-44 is the reference):
 *
 *   - the breathing halo is a CSS keyframe animation (the pre-rendered-glow
 *     equivalent: the browser composites one declared animation instead of
 *     us building gradients per frame), stepped timing where a pulse decays
 *     (the quantized-alpha idiom);
 *   - pulses are one-shot ring elements keyed by a monotonically increasing
 *     per-node counter — each rendered ring corresponds to exactly one
 *     committed event (h14), the pool being React's keyed reconciliation;
 *   - IntersectionObserver + visibilitychange gate every animation via
 *     `data-motion="paused"` (useAnimationGate);
 *   - prefers-reduced-motion renders ONE static frame: `data-motion="static"`
 *     never animates, and liveness stays fully readable as text — the
 *     "N active run(s)" line and each live node's "active" badge — because
 *     status is never color (or motion) alone.
 *
 * Everything drawn traces to committed API state: the graph from a
 * published workflow version, the halo from non-terminal run rows, node
 * presence from non-terminal node-run rows, pulses one-to-one with
 * committed cross-run events delivered by the shared stream.
 */

/** Loop edges attach below the row — same convention as WorkflowNode.tsx. */
const LOOP_SOURCE_HANDLE = "loop-out";
const LOOP_TARGET_HANDLE = "loop-in";

interface ActiveNodeData extends Record<string, unknown> {
  node: GraphNode;
  /** A non-terminal node run of an active run names this node (h20). */
  live: boolean;
  /** Committed events pulsed at this node so far — keys the one-shot ring. */
  pulseCount: number;
  inspected: boolean;
}

type ActiveFlowNode = Node<ActiveNodeData, "presence">;

/**
 * The compact presence card: kind-colored identity dot + name + kind word,
 * an explicit "active" badge while live (word, not color alone), and the
 * pulse ring. Deliberately free of React Flow state (no useStore) so it
 * renders and asserts without a live canvas.
 */
function ActiveGraphNode({ data }: NodeProps<ActiveFlowNode>) {
  const accent = accentFor(data.node.kind);
  const classes = [
    "active-node",
    data.live ? "is-live" : "",
    data.inspected ? "is-inspected" : "",
  ]
    .filter(Boolean)
    .join(" ");
  return (
    <>
      <Handle type="target" position={Position.Left} isConnectable={false} />
      <div
        className={classes}
        data-node-id={data.node.id}
        data-node-live={data.live ? "true" : "false"}
        style={{ ["--node-accent" as string]: accent }}
      >
        <div className="active-node__head">
          <span className="active-node__dot" aria-hidden="true" />
          <span className="active-node__name">{data.node.id}</span>
        </div>
        <div className="active-node__meta">
          <span className="active-node__kind">{data.node.kind}</span>
          {data.live ? (
            <span className="active-node__badge">
              <span aria-hidden="true">●</span> active
            </span>
          ) : null}
        </div>
        {data.pulseCount > 0 ? (
          // Keyed by the counter: each committed event remounts the ring,
          // restarting its one-shot animation — one visible pulse per event.
          <span
            key={data.pulseCount}
            className="active-node__pulse"
            data-pulse-count={data.pulseCount}
            aria-hidden="true"
          />
        ) : null}
      </div>
      <Handle type="source" position={Position.Right} isConnectable={false} />
      <Handle
        type="source"
        id={LOOP_SOURCE_HANDLE}
        position={Position.Bottom}
        isConnectable={false}
      />
      <Handle
        type="target"
        id={LOOP_TARGET_HANDLE}
        position={Position.Bottom}
        isConnectable={false}
      />
    </>
  );
}

const NODE_TYPES = { presence: ActiveGraphNode };

export interface ActiveGraphCanvasProps {
  presence: ActiveGraphPresence;
  reducedMotion: boolean;
  /** nodeId -> committed-event pulse count for THIS graph's nodes. */
  pulses: Record<string, number>;
}

/** A DOM-id-safe slug for one graph: workflow key + pinned version. */
export function activeGraphDomId(presence: ActiveGraphPresence): string {
  return `active-graph-${presence.workflowKey}-v${presence.version}`;
}

export function ActiveGraphCanvas({
  presence,
  reducedMotion,
  pulses,
}: ActiveGraphCanvasProps) {
  const wrapRef = useRef<HTMLElement | null>(null);
  const instanceRef = useRef<ReactFlowInstance<
    ActiveFlowNode,
    BuiltInEdge
  > | null>(null);
  const animate = useAnimationGate(wrapRef);
  const [inspectedId, setInspectedId] = useState<string | null>(null);

  const { graph, runIds, activeNodeIds } = presence;
  const alive = runIds.length > 0;
  const activeSet = useMemo(() => new Set(activeNodeIds), [activeNodeIds]);
  const domId = activeGraphDomId(presence);

  const { positions, ready: layoutReady } = useElkLayout(graph);

  // fitView runs once on mount, against the fallback layout — refit when
  // ELK's real layout lands (the RunView.tsx:146-152 lesson).
  useEffect(() => {
    if (!layoutReady) return;
    const frame = requestAnimationFrame(() => {
      instanceRef.current?.fitView({ padding: 0.1, maxZoom: 1 });
    });
    return () => cancelAnimationFrame(frame);
  }, [layoutReady, positions]);

  const flowNodes: ActiveFlowNode[] = useMemo(
    () =>
      graph.nodes.map((node) => ({
        id: node.id,
        type: "presence" as const,
        position: positions[node.id] ?? { x: 0, y: 0 },
        width: NODE_WIDTH,
        height: NODE_HEIGHT,
        draggable: false,
        selectable: false,
        connectable: false,
        data: {
          node,
          live: activeSet.has(node.id),
          pulseCount: pulses[node.id] ?? 0,
          inspected: inspectedId === node.id,
        },
      })),
    // Motion mode deliberately absent: a node card carries no motion state
    // of its own — the section's `data-motion` attribute gates every
    // animation from above, so flipping it re-creates no node objects.
    [graph, positions, activeSet, pulses, inspectedId],
  );

  const flowEdges: BuiltInEdge[] = useMemo(
    () =>
      graph.edges.map((edge) => ({
        id: edge.id,
        source: edge.source,
        target: edge.target,
        sourceHandle: edge.loop ? LOOP_SOURCE_HANDLE : undefined,
        targetHandle: edge.loop ? LOOP_TARGET_HANDLE : undefined,
        type: "smoothstep" as const,
        pathOptions: { borderRadius: 14, offset: edge.loop ? 28 : 12 },
        label: edge.outcome,
        className: ["flow-edge", "is-unwalked", edge.loop ? "is-loop" : ""]
          .filter(Boolean)
          .join(" "),
        // DASHED throughout: this canvas shows the paths the graph
        // *proposes* (culture-design/edges.ts vocabulary) — which edges a
        // particular token walked is the per-run view's story, not this
        // overview's, and pretending otherwise would be invented state.
        style: {
          stroke: DASHED.stroke,
          strokeWidth: DASHED.strokeWidth,
          strokeDasharray: DASHED.strokeDasharray,
        },
        markerEnd: { type: MarkerType.ArrowClosed, width: 14, height: 14 },
        ariaLabel: `edge ${edge.source} ${edge.outcome} to ${edge.target}${edge.loop ? ", loop" : ""}`,
      })),
    [graph],
  );

  // Keyboard inspection without a pointer (the MeshCanvas.tsx:833-847
  // precedent): Left/Right cycles the breadth-first node order, Escape
  // clears; the readout below the canvas is a role="status" live region.
  const onKeyDown = (event: React.KeyboardEvent<HTMLDivElement>) => {
    if (event.key === "Escape") {
      setInspectedId(null);
      return;
    }
    if (event.key !== "ArrowRight" && event.key !== "ArrowLeft") return;
    event.preventDefault();
    if (graph.nodes.length === 0) return;
    const idx = graph.nodes.findIndex((node) => node.id === inspectedId);
    const delta = event.key === "ArrowRight" ? 1 : -1;
    // From "nothing inspected" (idx -1), Right lands on the first node and
    // Left on the last — both walks start at the natural end.
    const base = idx === -1 && delta === -1 ? 0 : idx;
    const next =
      graph.nodes[(base + delta + graph.nodes.length) % graph.nodes.length];
    setInspectedId(next.id);
  };

  const inspected = inspectedId
    ? graph.nodes.find((node) => node.id === inspectedId)
    : undefined;

  const motion = reducedMotion ? "static" : animate ? "animated" : "paused";

  return (
    <section
      ref={wrapRef}
      id={domId}
      className={`active-graph${alive ? " is-alive" : ""}`}
      data-workflow-key={presence.workflowKey}
      data-workflow-digest={presence.digest}
      data-alive={alive ? "true" : "false"}
      data-motion={motion}
      aria-label={`Live graph of workflow ${presence.workflowKey} version ${presence.version}, ${runIds.length} active run(s)`}
    >
      <header className="active-graph__head">
        <div>
          <h2 className="active-graph__title">
            {presence.workflowKey}{" "}
            <span className="active-graph__version">v{presence.version}</span>
          </h2>
          <p className="active-graph__digest muted">
            digest <code title={presence.digest}>{presence.digest.slice(0, 20)}…</code>
          </p>
        </div>
        <div className="active-graph__presence">
          <p className="active-graph__count" data-active-run-count={runIds.length}>
            <span className="active-graph__live-dot" aria-hidden="true" />
            {runIds.length} active {runIds.length === 1 ? "run" : "runs"}
          </p>
          <ul className="active-graph__runs" aria-label="Active runs">
            {runIds.map((runId) => (
              <li key={runId} data-run-id={runId}>
                <Link to={`/runs/${runId}`}>{runId}</Link>
              </li>
            ))}
          </ul>
        </div>
      </header>

      <div
        // A stable id per graph: React Flow's own wrapper also carries
        // role="application" (the RunView.tsx:373-379 nesting), so an
        // agent or e2e assertion needs an unambiguous handle on *this*
        // element rather than a role lookup that matches both.
        id={`${domId}-canvas`}
        className="active-graph__canvas"
        tabIndex={0}
        role="application"
        aria-label={`Workflow graph for ${presence.workflowKey}. Left and right arrow keys inspect each node; Escape clears the inspection.`}
        onKeyDown={onKeyDown}
      >
        <ReactFlow
          nodes={flowNodes}
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
          panOnScroll
          minZoom={0.3}
          maxZoom={1.5}
          fitView
          fitViewOptions={{ padding: 0.1, maxZoom: 1 }}
          proOptions={{ hideAttribution: true }}
        >
          <Background gap={28} size={1} />
        </ReactFlow>
      </div>

      <p
        className="active-graph__inspect"
        id={`${domId}-inspect`}
        role="status"
      >
        {inspected
          ? `${inspected.id} · ${inspected.kind} · ${
              activeSet.has(inspected.id) ? "active" : "no active work"
            }`
          : "Focus the graph and use the arrow keys to inspect nodes."}
      </p>
    </section>
  );
}

export default ActiveGraphCanvas;
