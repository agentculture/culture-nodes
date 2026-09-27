import { useEffect, useMemo, useRef, useState } from "react";
import { useParams, useSearchParams } from "react-router-dom";
import {
  Background,
  Controls,
  Handle,
  MarkerType,
  Position,
  ReactFlow,
  type BuiltInEdge,
  type Node,
  type NodeProps,
  type ReactFlowInstance,
} from "@xyflow/react";
import { setAgentState } from "../agent-state/store";
import { ApiError, getDeclarationFocus } from "../api/client";
import type {
  DeclarationFocus,
  DeclarationFocusDirection,
  DeclarationFocusLinkFilter,
} from "../api/types";
import ErrorNotice from "../components/ErrorNotice";
import {
  edgeStyleForLinkKind,
  focusToGraph,
  ringsOf,
} from "../domain/declgraph";
import type { GraphNode } from "../domain/graph";
import { NODE_HEIGHT, NODE_WIDTH, useElkLayout } from "../hooks/useElkLayout";

/**
 * Declarations (task t22, #328; spec c24/h18): one declaration, or a
 * declaration chain, rendered as a graph from
 * `GET /v1alpha1/declarations/{name}/focus` — the SAME response shape at
 * every distance (c24), which is why there is one route for "a single
 * declaration" and "a chain graph" rather than two. `{name}` is read
 * straight off the URL and handed to the focus API unchanged: the server
 * resolves it as a declaration name first, then as a declaration alias
 * (chain) name (task t21b) — this view never needs to know which one it
 * got.
 *
 * The distance/direction/link controls live in the URL's search params
 * (`?distance=&direction=&link=`), the same shareable-state idiom
 * `useTimeRange.ts` uses for the Runs views — changing a control refetches
 * the focus at the new query, which is where distance-ring expansion and
 * direction/link filtering actually happen: `internal/api/declgraph.go`'s
 * BFS applies both server-side (openapi.yaml's getDeclarationFocus
 * parameters), so there is no broader payload for this route to filter
 * further client-side.
 *
 * Layout reuses `hooks/useElkLayout.ts` — the same ELK hook
 * `ActiveGraphCanvas`/`RunView` already lay workflow graphs out with — via
 * `domain/declgraph.ts`'s `focusToGraph`, which maps the focus payload onto
 * the existing `WorkflowGraph` shape instead of adding a second layout
 * path. must/can links render as SOLID/DASHED
 * (`domain/declgraph.ts#edgeStyleForLinkKind`), the same dashed/solid
 * vocabulary `AuthorityChip` uses for ledger authority, so the "which of
 * these edges are optional" question reads the same way everywhere in the
 * app.
 */

const DIRECTIONS: readonly DeclarationFocusDirection[] = ["both", "up", "down"];
const LINK_FILTERS: readonly DeclarationFocusLinkFilter[] = ["both", "must", "can"];

function toApiError(cause: unknown): ApiError {
  return cause instanceof ApiError
    ? cause
    : new ApiError(0, String(cause), "check the browser console");
}

function parseDistance(raw: string | null): number {
  const n = Number(raw);
  return Number.isFinite(n) && n >= 0 ? Math.trunc(n) : 0;
}

function parseEnum<T extends string>(
  raw: string | null,
  allowed: readonly T[],
  fallback: T,
): T {
  return (allowed as readonly string[]).includes(raw ?? "") ? (raw as T) : fallback;
}

interface DeclNodeData extends Record<string, unknown> {
  node: GraphNode;
  center: boolean;
}

type DeclFlowNode = Node<DeclNodeData, "declaration">;

/** One declaration card: name, action/trigger kind, and its distance from the center. */
function DeclarationNode({ data }: NodeProps<DeclFlowNode>) {
  return (
    <div
      className={`decl-node${data.center ? " decl-node--center" : ""}`}
      data-declaration={data.node.id}
      data-distance={data.node.depth}
      data-center={data.center ? "true" : "false"}
    >
      <Handle type="target" position={Position.Left} isConnectable={false} />
      <p className="decl-node__name">{data.node.id}</p>
      <p className="decl-node__kind muted">{data.node.kind}</p>
      <p className="decl-node__distance muted">distance {data.node.depth}</p>
      <Handle type="source" position={Position.Right} isConnectable={false} />
    </div>
  );
}

const NODE_TYPES = { declaration: DeclarationNode };

export function Declarations() {
  const { name } = useParams<{ name: string }>();
  const [searchParams, setSearchParams] = useSearchParams();
  const distance = parseDistance(searchParams.get("distance"));
  const direction = parseEnum(searchParams.get("direction"), DIRECTIONS, "both");
  const link = parseEnum(searchParams.get("link"), LINK_FILTERS, "both");

  const [focus, setFocus] = useState<DeclarationFocus | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const instanceRef = useRef<ReactFlowInstance<DeclFlowNode, BuiltInEdge> | null>(
    null,
  );

  useEffect(() => {
    if (!name) return;
    const controller = new AbortController();
    setAgentState({ status: "loading" });
    setError(null);
    getDeclarationFocus(name, { distance, direction, link }, controller.signal)
      .then((payload) => {
        if (controller.signal.aborted) return;
        setFocus(payload);
        setAgentState({ status: "ready" });
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setFocus(null);
        setError(toApiError(cause));
        setAgentState({ status: "ready" });
      });
    return () => controller.abort();
  }, [name, distance, direction, link]);

  const graph = useMemo(() => (focus ? focusToGraph(focus) : null), [focus]);
  const rings = useMemo(() => (focus ? ringsOf(focus) : []), [focus]);
  const { positions, ready: layoutReady } = useElkLayout(graph);

  useEffect(() => {
    if (!layoutReady) return;
    const frame = requestAnimationFrame(() => {
      instanceRef.current?.fitView({ padding: 0.15, maxZoom: 1 });
    });
    return () => cancelAnimationFrame(frame);
  }, [layoutReady, positions]);

  const setParam = (key: string, value: string) => {
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev);
      if (value) next.set(key, value);
      else next.delete(key);
      return next;
    });
  };

  const flowNodes: DeclFlowNode[] = useMemo(
    () =>
      graph
        ? graph.nodes.map((node) => ({
            id: node.id,
            type: "declaration" as const,
            position: positions[node.id] ?? { x: 0, y: 0 },
            width: NODE_WIDTH,
            height: NODE_HEIGHT,
            draggable: false,
            connectable: false,
            selectable: false,
            data: { node, center: node.id === focus?.center },
          }))
        : [],
    [graph, positions, focus],
  );

  const flowEdges: BuiltInEdge[] = useMemo(
    () =>
      graph
        ? graph.edges.map((edge) => {
            const style = edgeStyleForLinkKind(
              edge.outcome as "must" | "can",
            );
            return {
              id: edge.id,
              source: edge.source,
              target: edge.target,
              label: edge.outcome,
              className: `flow-edge decl-edge decl-edge--${edge.outcome}`,
              style: {
                stroke: style.stroke,
                strokeWidth: style.strokeWidth,
                strokeDasharray: style.strokeDasharray,
              },
              markerEnd: { type: MarkerType.ArrowClosed, width: 14, height: 14 },
              ariaLabel: `${edge.source} ${edge.outcome} ${edge.target}`,
            };
          })
        : [],
    [graph],
  );

  if (!name) {
    return (
      <section className="view-rail">
        <h1>Declarations</h1>
        <p className="muted">No declaration or chain name given.</p>
      </section>
    );
  }

  return (
    <section className="view-rail decl-view" data-declaration-center={name}>
      <header>
        <h1>{name}</h1>
        {focus ? (
          <p className="muted" data-testid="decl-summary">
            {focus.distance === 0 ? "single declaration" : "chain graph"} ·
            distance {focus.distance} · direction {focus.direction} · link{" "}
            {focus.link}
          </p>
        ) : null}
      </header>

      <form
        className="decl-controls"
        aria-label="Focus controls"
        onSubmit={(event) => event.preventDefault()}
      >
        <label htmlFor="decl-distance">
          Distance
          <input
            id="decl-distance"
            type="number"
            min={0}
            step={1}
            value={distance}
            onChange={(event) =>
              setParam(
                "distance",
                String(Math.max(0, Math.trunc(Number(event.target.value) || 0))),
              )
            }
          />
        </label>
        <label htmlFor="decl-direction">
          Direction
          <select
            id="decl-direction"
            value={direction}
            onChange={(event) => setParam("direction", event.target.value)}
          >
            {DIRECTIONS.map((d) => (
              <option key={d} value={d}>
                {d}
              </option>
            ))}
          </select>
        </label>
        <label htmlFor="decl-link">
          Link
          <select
            id="decl-link"
            value={link}
            onChange={(event) => setParam("link", event.target.value)}
          >
            {LINK_FILTERS.map((l) => (
              <option key={l} value={l}>
                {l}
              </option>
            ))}
          </select>
        </label>
      </form>

      {error ? <ErrorNotice error={error} /> : null}

      {focus ? (
        <>
          <ul className="decl-rings" aria-label="Distance rings">
            {rings.map((ring) => (
              <li key={ring.distance} data-distance={ring.distance}>
                <span className="decl-rings__label">distance {ring.distance}</span>
                {ring.declarations.map((d) => (
                  <span key={d.name} className="decl-rings__item">
                    {d.name}
                  </span>
                ))}
              </li>
            ))}
          </ul>

          <div
            className="decl-canvas canvas-surface"
            role="application"
            aria-label={`Declaration graph for ${name}`}
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
              elementsSelectable={false}
              panOnScroll
              minZoom={0.3}
              maxZoom={1.5}
              fitView
              fitViewOptions={{ padding: 0.15, maxZoom: 1 }}
              proOptions={{ hideAttribution: true }}
            >
              <Background gap={28} size={1} />
              <Controls showInteractive={false} />
            </ReactFlow>
          </div>
        </>
      ) : null}
    </section>
  );
}

export default Declarations;
