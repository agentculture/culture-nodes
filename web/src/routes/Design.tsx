import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
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
import { setAgentState } from "../agent-state/store";
import { ApiError, listRuns, listWorkflows } from "../api/client";
import type { WorkflowVersion } from "../api/types";
import ErrorNotice from "../components/ErrorNotice";
import CultureNode from "../culture-design/CultureNode";
import { DASHED } from "../culture-design/edges";
import { accentFor, type GraphNode, type WorkflowGraph } from "../domain/graph";
import { deriveNodeDefinitions } from "../domain/node-catalog";
import {
  graphFromPublishedIR,
  groupWorkflowVersions,
  RECENT_RUNS_LIMIT,
  selectGalleryVersion,
  storedSource,
  withRunsByWorkflowKey,
  type GallerySelection,
  type WorkflowGroup,
} from "../domain/workflows";
import { NODE_HEIGHT, NODE_WIDTH, useElkLayout } from "../hooks/useElkLayout";
import { useReducedMotion } from "../hooks/useReducedMotion";
import ActiveGraphsPanel from "./ActiveGraphs";
import {
  type SharedEventType,
} from "../hooks/useSharedEvents";
import { useSnapshotReconcile } from "../hooks/useSnapshotReconcile";

/**
 * Design (task t8, claims c24/c31/c36) — the view that makes every published
 * workflow readable as a graph, with no run required.
 *
 * It replaces the misnamed "Node Graphs" view, whose "Node Graphs" sub-tab
 * drew *cards* — a versions table and a recent-runs list per workflow_key —
 * and never a graph. Only Active Graphs drew one, and only for a workflow a
 * non-terminal run currently pins, so a workflow that was published and
 * never run had no graph anywhere in the app (spec s4). The gallery below
 * draws the selected version's graph straight from its published
 * `normalized_ir`, so "has a graph" now depends on being published and on
 * nothing else.
 *
 * Three sub-views, on the same `?tab=` URL-param discipline the retired view
 * established (bookmarkable, and legible to an agent from the URL alone):
 *
 *   - **Gallery** (default): the workflow index and the selected version's
 *     graph, plus that version's stored source, byte-identical.
 *   - **Nodes**: the cross-workflow node-definition catalog (task t29),
 *     carried over unchanged.
 *   - **Active graphs**: which of those graphs are alive right now (task
 *     t31), carried over unchanged into `routes/ActiveGraphs.tsx`.
 *
 * Honesty (h14/h4) holds throughout: every node, edge, version and run drawn
 * here traces to a committed API row, an empty gallery says so rather than
 * drawing a placeholder graph, and the source pane shows the operator's own
 * bytes rather than a re-serialization of the IR.
 */

type SubTab = "gallery" | "nodes" | "active";

const DEFAULT_TAB: SubTab = "gallery";

function parseTab(value: string | null): SubTab {
  if (value === "gallery" || value === "nodes" || value === "active") {
    return value;
  }
  return DEFAULT_TAB;
}

/**
 * Every run-lifecycle event that can change what the gallery's index says
 * about a workflow's runs — a stable module-level reference, as
 * useSharedEvents requires (issue #46). A new workflow *version* publish has
 * no committed-event counterpart in the shared stream's vocabulary today, so
 * this panel's auto-refresh honestly tracks run activity only; a version
 * published with no accompanying run stays until the next mount.
 */
const GALLERY_EVENT_TYPES = [
  "dev.culture.nodes.run.created",
  "dev.culture.nodes.run.waiting",
  "dev.culture.nodes.run.completed",
  "dev.culture.nodes.run.failed",
  "dev.culture.nodes.run.cancelled",
  "dev.culture.nodes.run.bounded",
] as const satisfies readonly SharedEventType[];

/** Mirrors the Mesh view's attribution-refresh discipline (Mesh.tsx). */
const REFRESH_DEBOUNCE_MS = 4000;

const toApiError = (cause: unknown): ApiError =>
  cause instanceof ApiError
    ? cause
    : new ApiError(0, String(cause), "check the browser console");

/**
 * One gallery load: the published versions, grouped by `workflow_key`, each
 * group's recent runs fetched with that key's own
 * `GET /v1alpha1/runs?workflow_key=…` request (the filter task t7 added).
 *
 * The per-key requests go out together (`Promise.all`), so the load costs one
 * round trip for the workflows plus one more for all of the keys. This is NOT
 * one unfiltered listing sliced client-side: `GET /v1alpha1/runs` answers at
 * most one page, and the pr-upkeep sweep mints a run every few minutes, so in
 * production that page held nothing but sweep runs and every workflow read as
 * "never run" while having hundreds of runs (task t8, claim c8). Asking per
 * key is what makes "never run" mean what it says.
 *
 * Any one request rejecting rejects the whole load — a workflow rendered as
 * "never run" because its request failed would be that same lie back again.
 */
async function loadGroups(signal: AbortSignal): Promise<WorkflowGroup[]> {
  const workflowList = await listWorkflows(signal);
  const groups = groupWorkflowVersions(workflowList.items);
  const runLists = await Promise.all(
    groups.map((group) =>
      listRuns(signal, {
        workflow_key: group.workflowKey,
        sort: "updated_at",
        limit: RECENT_RUNS_LIMIT,
      }),
    ),
  );
  return withRunsByWorkflowKey(
    groups,
    new Map(groups.map((group, i) => [group.workflowKey, runLists[i].items])),
  );
}

/* ---------------------------------------------------------------- canvas */

interface GalleryNodeData extends Record<string, unknown> {
  node: GraphNode;
  inspected: boolean;
  motion: "animated" | "static";
}

type GalleryFlowNode = Node<GalleryNodeData, "published">;

/** Loop edges attach below the row — the WorkflowNode.tsx convention. */
const LOOP_SOURCE_HANDLE = "loop-out";
const LOOP_TARGET_HANDLE = "loop-in";

/**
 * The gallery's node: the one Culture node component (task t3), with no
 * execution passed at all.
 *
 * That absence is the point. A published version has no run, so there is no
 * node state, no attempt, no actor that ran anything — and a card that showed
 * a state chip here would be inventing one. `CultureNode` without an
 * `execution` renders exactly the identity the IR carries: id, kind and the
 * kind's colour, plus the owner ref when the node names one.
 */
function GalleryNode({ data }: NodeProps<GalleryFlowNode>) {
  return (
    <>
      <Handle type="target" position={Position.Left} isConnectable={false} />
      <CultureNode
        node={data.node}
        selected={data.inspected}
        motion={data.motion}
      />
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

const NODE_TYPES = { published: GalleryNode };

/**
 * One published version, drawn. The RunView/ActiveGraphCanvas stack — React
 * Flow over an ELK layered layout, dashed edges labelled with the outcome
 * that walks them — with the live half removed: no halo, no pulses, no
 * walked/unwalked distinction, because none of those facts exists without a
 * run and this canvas may not imply one.
 */
function GalleryCanvas({
  graph,
  digest,
  workflowKey,
}: {
  graph: WorkflowGraph;
  digest: string;
  workflowKey: string;
}) {
  const instanceRef = useRef<ReactFlowInstance<
    GalleryFlowNode,
    BuiltInEdge
  > | null>(null);
  const [inspectedId, setInspectedId] = useState<string | null>(null);
  const reducedMotion = useReducedMotion();
  const motion = reducedMotion ? "static" : "animated";
  const { positions, ready: layoutReady } = useElkLayout(graph);

  // The selection belongs to the version on screen: switching workflow or
  // version must not leave the previous graph's node id inspected.
  useEffect(() => setInspectedId(null), [digest]);

  // fitView runs once on mount against the fallback layout — refit when ELK's
  // real layout lands (the RunView.tsx:146-152 lesson).
  useEffect(() => {
    if (!layoutReady) return;
    const frame = requestAnimationFrame(() => {
      instanceRef.current?.fitView({ padding: 0.1, maxZoom: 1 });
    });
    return () => cancelAnimationFrame(frame);
  }, [layoutReady, positions]);

  const flowNodes: GalleryFlowNode[] = useMemo(
    () =>
      graph.nodes.map((node) => ({
        id: node.id,
        type: "published" as const,
        position: positions[node.id] ?? { x: 0, y: 0 },
        width: NODE_WIDTH,
        height: NODE_HEIGHT,
        draggable: false,
        selectable: false,
        connectable: false,
        data: { node, inspected: inspectedId === node.id, motion },
      })),
    [graph, positions, inspectedId, motion],
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
        // DASHED throughout: these are the paths the graph *proposes*
        // (culture-design/edges.ts vocabulary). Which edge a token walked is
        // a run's story, and this view has no run.
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

  // Keyboard inspection without a pointer (the ActiveGraphCanvas precedent):
  // Left/Right cycles the breadth-first node order, Escape clears; the
  // readout below the canvas is a role="status" live region.
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
    const base = idx === -1 && delta === -1 ? 0 : idx;
    const next =
      graph.nodes[(base + delta + graph.nodes.length) % graph.nodes.length];
    setInspectedId(next.id);
  };

  const inspected = inspectedId
    ? graph.nodes.find((node) => node.id === inspectedId)
    : undefined;

  return (
    <>
      <div
        id="design-graph"
        className="design-gallery__canvas canvas-surface"
        data-workflow-key={workflowKey}
        data-workflow-digest={digest}
        data-node-count={graph.nodes.length}
        data-edge-count={graph.edges.length}
        tabIndex={0}
        role="application"
        aria-label={`Graph of workflow ${workflowKey}. Left and right arrow keys inspect each node; Escape clears the inspection.`}
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
      <p className="design-gallery__inspect" id="design-graph-inspect" role="status">
        {inspected
          ? `${inspected.id} · ${inspected.kind}${
              inspected.uses ? ` · ${inspected.uses}` : ""
            }`
          : "Focus the graph and use the arrow keys to inspect nodes."}
      </p>
    </>
  );
}

/* --------------------------------------------------------------- gallery */

/**
 * The stored source, verbatim (claim c36 / honesty condition h28).
 *
 * `WorkflowVersion.source` is the document the operator published, byte for
 * byte — comments, blank lines and key order intact. It is rendered inside a
 * `<pre>` as text, never re-parsed and re-emitted, so what a reader copies
 * out of this pane is what `nodes workflow publish` would hash. Task t15's
 * canvas opens the same bytes; until it exists, this pane is where "open the
 * stored source" happens.
 */
function SourcePane({ version }: { version: WorkflowVersion }) {
  const { source, format } = storedSource(version);
  return (
    <pre
      id="design-source"
      className="design-gallery__source"
      data-source-format={format}
      data-source-bytes={source.length}
      data-workflow-digest={version.digest}
      aria-label={`Stored ${format} source of ${version.workflow_key} version ${version.version}`}
    >
      {source}
    </pre>
  );
}

function GalleryPanel() {
  const [searchParams, setSearchParams] = useSearchParams();
  const [groups, setGroups] = useState<WorkflowGroup[] | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [sourceOpen, setSourceOpen] = useState(false);
  const [reloadKey, setReloadKey] = useState(0);
  const reloadTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const lastReload = useRef(0);

  const scheduleReload = useCallback(() => {
    if (reloadTimer.current) return;
    const elapsed = Date.now() - lastReload.current;
    const wait = Math.max(0, REFRESH_DEBOUNCE_MS - elapsed);
    reloadTimer.current = setTimeout(() => {
      reloadTimer.current = undefined;
      lastReload.current = Date.now();
      setReloadKey((key) => key + 1);
    }, wait);
  }, []);

  useEffect(
    () => () => {
      if (reloadTimer.current) clearTimeout(reloadTimer.current);
    },
    [],
  );

  const { resolveSnapshot } = useSnapshotReconcile(GALLERY_EVENT_TYPES, scheduleReload);

  useEffect(() => {
    const controller = new AbortController();
    setAgentState({ status: "loading", run: null });
    setError(null);
    setGroups(null);

    loadGroups(controller.signal)
      .then((loaded) => {
        if (controller.signal.aborted) return;
        setGroups(loaded);
        resolveSnapshot();
        setAgentState({ status: "ready", run: null });
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setGroups([]);
        resolveSnapshot();
        setError(toApiError(cause));
        // "ready" means the initial load finished, including finishing it
        // badly — the error renders alongside, not instead (the convention
        // every list view here follows).
        setAgentState({ status: "ready", run: null });
      });
    return () => controller.abort();
  }, []);

  // The SSE-triggered background refresh (issue #46): skips the first render,
  // never nulls the rendered gallery, never regresses agent-state to loading.
  useEffect(() => {
    if (reloadKey === 0) return;
    const controller = new AbortController();
    loadGroups(controller.signal)
      .then((loaded) => {
        if (controller.signal.aborted) return;
        setGroups(loaded);
        setError(null);
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setError(toApiError(cause));
      });
    return () => controller.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [reloadKey]);

  const versionParam = Number(searchParams.get("version"));
  const selection: GallerySelection | null = useMemo(
    () =>
      groups === null
        ? null
        : selectGalleryVersion(
            groups,
            searchParams.get("workflow"),
            Number.isFinite(versionParam) && versionParam > 0
              ? versionParam
              : null,
          ),
    [groups, searchParams, versionParam],
  );

  const graph = useMemo(
    () => (selection ? graphFromPublishedIR(selection.version) : null),
    [selection],
  );

  const select = (workflowKey: string, version?: number) => {
    setSearchParams((prev) => {
      const params = new URLSearchParams(prev);
      params.set("workflow", workflowKey);
      // Choosing a workflow means "show me this workflow", not "show me its
      // version N" — a version param left over from the previous selection
      // would silently pick a version of a different graph.
      if (version === undefined) params.delete("version");
      else params.set("version", String(version));
      return params;
    });
  };

  // The machine-readable mirror: a canvas claims things in pixels that
  // webglass cannot read, so what it claims is stated here too (the
  // Mesh.tsx:177-194 convention). Published only once real rows exist —
  // while loading, the pixels claim nothing and neither does the mirror.
  useEffect(() => {
    if (groups === null) return;
    const source = selection ? storedSource(selection.version).source : null;
    setAgentState({
      design: {
        workflow_count: groups.length,
        workflow_key: selection?.group.workflowKey ?? null,
        version: selection?.version.version ?? null,
        digest: selection?.version.digest ?? null,
        node_count: graph?.nodes.length ?? 0,
        edge_count: graph?.edges.length ?? 0,
        source_bytes: source === null ? 0 : source.length,
        source_open: sourceOpen,
        run_count: selection?.group.recentRuns.length ?? 0,
      },
    });
  }, [groups, selection, graph, sourceOpen]);

  // Leaving the sub-view drops the block (undefined keys are omitted by
  // JSON.stringify — the mesh/authoring/statistics convention).
  useEffect(() => () => setAgentState({ design: undefined }), []);

  if (groups === null) {
    return (
      <div id="design-gallery-panel">
        {error ? <ErrorNotice error={error} /> : null}
        <p className="muted" id="design-loading">
          Loading workflows…
        </p>
      </div>
    );
  }

  if (groups.length === 0) {
    return (
      <div id="design-gallery-panel">
        {error ? <ErrorNotice error={error} /> : null}
        <p className="muted" id="design-empty">
          No workflows published yet. Publish one with{" "}
          <code>nodes workflow publish</code>.
        </p>
      </div>
    );
  }

  return (
    <div id="design-gallery-panel" className="design-gallery">
      {error ? <ErrorNotice error={error} /> : null}
      <div
        className="design-gallery__index"
        id="design-workflow-list"
        role="group"
        aria-label="Published workflows"
      >
        {groups.map((group) => {
          const current = group.workflowKey === selection?.group.workflowKey;
          const runs = group.recentRuns.length;
          return (
            <button
              type="button"
              key={group.workflowKey}
              className="design-gallery__key"
              data-workflow-key={group.workflowKey}
              aria-pressed={current}
              onClick={() => select(group.workflowKey)}
            >
              <span className="design-gallery__name">{group.workflowKey}</span>
              <span className="design-gallery__sub muted">
                {group.versions.length}{" "}
                {group.versions.length === 1 ? "version" : "versions"} ·{" "}
                {group.owner ?? "unowned"} ·{" "}
                <span data-run-count={runs}>
                  {/* A workflow that has never run still has a graph — that
                      is the whole point of this view (c31). Say so as a
                      fact, not as an absence. */}
                  {runs === 0 ? "never run" : `${runs} recent`}
                </span>
              </span>
            </button>
          );
        })}
      </div>

      {selection && graph ? (
        <div className="design-gallery__main">
          <div
            className="design-gallery__versions view-toggle"
            id="design-version-list"
            role="group"
            aria-label={`Versions of ${selection.group.workflowKey}`}
          >
            {selection.group.versions.map((version) => (
              <button
                type="button"
                key={version.digest}
                data-workflow-digest={version.digest}
                data-workflow-version={version.version}
                aria-pressed={version.digest === selection.version.digest}
                onClick={() =>
                  select(selection.group.workflowKey, version.version)
                }
              >
                v{version.version}
              </button>
            ))}
          </div>

          <GalleryCanvas
            graph={graph}
            digest={selection.version.digest}
            workflowKey={selection.group.workflowKey}
          />

          <div className="design-gallery__meta" id="design-meta">
            <span>
              {graph.nodes.length}{" "}
              {graph.nodes.length === 1 ? "node" : "nodes"} ·{" "}
              {graph.edges.length}{" "}
              {graph.edges.length === 1 ? "edge" : "edges"}
            </span>
            <span>owner {selection.group.owner ?? "unowned"}</span>
            <span>
              digest{" "}
              <code title={selection.version.digest}>
                {selection.version.digest.slice(0, 20)}…
              </code>
            </span>
            <span>
              published{" "}
              <time dateTime={selection.version.created_at}>
                {selection.version.created_at}
              </time>
            </span>
          </div>

          <div className="design-gallery__actions">
            <button
              type="button"
              id="design-source-toggle"
              className="author-workflow__button"
              aria-expanded={sourceOpen}
              aria-controls="design-source"
              onClick={() => setSourceOpen((open) => !open)}
            >
              {sourceOpen ? "Hide source" : "Open source"}
            </button>
          </div>
          {sourceOpen ? <SourcePane version={selection.version} /> : null}

          <div className="design-gallery__runs">
            <h2>Recent runs</h2>
            {selection.group.recentRuns.length === 0 ? (
              <p className="muted" id="design-no-runs">
                No runs yet — the graph above is drawn from the published
                version, not from any run of it.
              </p>
            ) : (
              <ul className="design-gallery__run-list">
                {selection.group.recentRuns.map((run) => (
                  <li key={run.id} data-run-id={run.id}>
                    <Link to={`/runs/${run.id}`}>{run.id}</Link>{" "}
                    <span data-run-state={run.state}>{run.state}</span>{" "}
                    <time dateTime={run.updated_at}>{run.updated_at}</time>
                  </li>
                ))}
              </ul>
            )}
          </div>
        </div>
      ) : null}
    </div>
  );
}

/* ----------------------------------------------------------------- nodes */

/**
 * The Nodes sub-view (task t29's parser, first rendered by task t31, moved
 * here by t8): every distinct node definition across the latest version of
 * each published workflow — `deriveNodeDefinitions` over
 * `GET /v1alpha1/workflows`, the same fetch the gallery makes (c20: only
 * published-IR-derived data, nothing invented). One card per definition:
 * kind (word + the NODE_KIND_PALETTE identity colour, never colour alone),
 * the actor/runner/approver ref backing its identity when the kind has one,
 * and every (workflow, node id) occurrence.
 */
function NodesPanel() {
  const [versions, setVersions] = useState<WorkflowVersion[] | null>(null);
  const [error, setError] = useState<ApiError | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    setAgentState({ status: "loading", run: null });
    setError(null);
    setVersions(null);
    listWorkflows(controller.signal)
      .then((list) => {
        if (controller.signal.aborted) return;
        setVersions(list.items);
        setAgentState({ status: "ready", run: null });
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setVersions([]);
        setError(toApiError(cause));
        setAgentState({ status: "ready", run: null });
      });
    return () => controller.abort();
  }, []);

  const definitions =
    versions !== null ? deriveNodeDefinitions(versions) : null;

  return (
    <div id="design-nodes-panel">
      {error ? <ErrorNotice error={error} /> : null}
      {definitions === null ? (
        <p className="muted" id="node-defs-loading">
          Loading node definitions…
        </p>
      ) : definitions.length === 0 ? (
        <p className="muted" id="node-defs-empty">
          No node definitions yet. They derive from published workflow IRs —
          publish a workflow with <code>nodes workflow publish</code> and its
          nodes appear here.
        </p>
      ) : (
        <ul className="node-defs" id="node-defs-list">
          {definitions.map((definition) => (
            <li
              key={definition.id}
              className="node-def-card"
              data-definition-id={definition.id}
              data-node-kind={definition.kind}
              style={{
                ["--node-accent" as string]: accentFor(definition.kind),
              }}
            >
              <div className="node-def-card__head">
                <span className="node-def-card__dot" aria-hidden="true" />
                <span className="node-def-card__kind">{definition.kind}</span>
                <span className="node-def-card__count">
                  {definition.occurrences.length}{" "}
                  {definition.occurrences.length === 1
                    ? "occurrence"
                    : "occurrences"}
                </span>
              </div>
              {definition.ref ? (
                <code className="node-def-card__ref" title={definition.ref}>
                  {definition.ref}
                </code>
              ) : (
                // The IR carries nothing further to distinguish these
                // definitions (see domain/node-catalog.ts) — say so rather
                // than inventing a synthetic identity.
                <p className="node-def-card__ref node-def-card__ref--none muted">
                  no external ref — identity is the kind alone
                </p>
              )}
              <ul className="node-def-card__occurrences">
                {definition.occurrences.map((occurrence) => (
                  <li
                    key={`${occurrence.workflowKey}:${occurrence.version}:${occurrence.nodeId}`}
                    data-occurrence={`${occurrence.workflowKey}@v${occurrence.version}:${occurrence.nodeId}`}
                  >
                    <span className="node-def-card__node">
                      {occurrence.nodeId}
                    </span>{" "}
                    in {occurrence.workflowKey}{" "}
                    <span className="muted">v{occurrence.version}</span>
                  </li>
                ))}
              </ul>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/* ------------------------------------------------------------------ view */

export function Design() {
  const [searchParams, setSearchParams] = useSearchParams();
  const tab = parseTab(searchParams.get("tab"));

  const setTab = (next: SubTab) => {
    setSearchParams((prev) => {
      const params = new URLSearchParams(prev);
      if (next === DEFAULT_TAB) params.delete("tab");
      else params.set("tab", next);
      return params;
    });
  };

  return (
    <section className="view-rail design-view">
      <div className="design-view__head">
        <div>
          <h1>Design</h1>
          <p className="muted">
            Every published workflow, readable as a graph — no run required.
            Select a version to see it drawn with the same node the Mesh and
            the run pages use, and to read the source it was published from.
          </p>
        </div>
        <Link
          to="/design/new"
          id="new-workflow-link"
          className="author-workflow__button author-workflow__button--primary"
        >
          New workflow
        </Link>
      </div>

      <div
        id="design-toggle"
        className="view-toggle"
        role="group"
        aria-label="Design sub-view"
      >
        <button
          type="button"
          id="design-toggle-gallery"
          aria-pressed={tab === "gallery"}
          onClick={() => setTab("gallery")}
        >
          Gallery
        </button>
        <button
          type="button"
          id="design-toggle-nodes"
          aria-pressed={tab === "nodes"}
          onClick={() => setTab("nodes")}
        >
          Nodes
        </button>
        <button
          type="button"
          id="design-toggle-active"
          aria-pressed={tab === "active"}
          onClick={() => setTab("active")}
        >
          Active graphs
        </button>
      </div>

      {tab === "gallery" ? <GalleryPanel /> : null}
      {tab === "nodes" ? <NodesPanel /> : null}
      {tab === "active" ? <ActiveGraphsPanel /> : null}
    </section>
  );
}

export default Design;
