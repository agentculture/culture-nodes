import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { setAgentState } from "../agent-state/store";
import { ApiError, listNodeRuns, listRuns, listWorkflows } from "../api/client";
import type { NodeRunListItem, Run, WorkflowVersion } from "../api/types";
import ActiveGraphCanvas from "../components/ActiveGraphCanvas";
import ErrorNotice from "../components/ErrorNotice";
import {
  deriveActiveGraphs,
  needsPresenceRefresh,
  presenceEventAction,
} from "../domain/active-presence";
import { accentFor } from "../domain/graph";
import { deriveNodeDefinitions } from "../domain/node-catalog";
import { groupWorkflowVersions, withRecentRuns } from "../domain/workflows";
import { useReducedMotion } from "../hooks/useReducedMotion";
import {
  useSharedEvents,
  type SharedEvent,
  type SharedEventType,
} from "../hooks/useSharedEvents";

/**
 * The Node Graphs tab (task t28, issue #56): replaces the standalone
 * Workflows tab with three sub-tabs — Nodes, Node Graphs, Active Graphs —
 * using RunView's aria-pressed segmented-toggle pattern (RunView.tsx:347-367,
 * `.view-toggle` in styles/app.css:391-411, reused verbatim) instead of
 * inventing a new control.
 *
 * Sub-tab selection lives in the `?tab=` URL search param (the same
 * URL-param-state discipline `useTimeRange` established for `since`/`until`)
 * so a sub-tab is bookmarkable and agent-drivable without any client-side
 * component state an agent can't see from the URL alone.
 *
 * All three sub-tabs do real work now: Node Graphs is the exact
 * per-workflow-cards view the old Workflows tab rendered (task t8, moved
 * here unchanged by t28); Nodes renders the cross-workflow node-definition
 * catalog `domain/node-catalog.ts` (task t29) derives from published
 * workflow IRs; Active Graphs (task t31) renders every graph a
 * non-terminal run pins, live — halo and pulses driven by committed rows
 * and the shared SSE stream, per `domain/active-presence.ts`. The h14 rule
 * holds throughout: everything drawn traces to a committed API row, and
 * each sub-tab renders an honest empty state when there is nothing.
 */

type SubTab = "nodes" | "graphs" | "active";

const DEFAULT_TAB: SubTab = "nodes";

function parseTab(value: string | null): SubTab {
  if (value === "nodes" || value === "graphs" || value === "active") {
    return value;
  }
  return DEFAULT_TAB;
}

/**
 * The Node Graphs sub-tab: every published workflow, one card per
 * `workflow_key`, listing its versions/digests/owner and its most recent
 * runs. This is routes/Workflows.tsx's entire data-fetch and render body
 * (task t8), moved verbatim under the new sub-tab — the underlying API
 * surface (`GET /v1alpha1/workflows`, `GET /v1alpha1/runs`) did not change,
 * only where in the nav it lives.
 *
 * Mounted only while `tab === "graphs"` (a plain conditional render, not a
 * route), so switching away and back re-fetches on mount just like any
 * other route would — and switching to Nodes/Active never leaves a stale
 * error notice or loading state from this panel behind.
 */
function NodeGraphsPanel() {
  const [versions, setVersions] = useState<WorkflowVersion[] | null>(null);
  const [runs, setRuns] = useState<Run[] | null>(null);
  const [error, setError] = useState<ApiError | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    setAgentState({ status: "loading", run: null });
    setError(null);
    setVersions(null);
    setRuns(null);

    const toApiError = (cause: unknown): ApiError =>
      cause instanceof ApiError
        ? cause
        : new ApiError(0, String(cause), "check the browser console");

    Promise.all([
      listWorkflows(controller.signal),
      listRuns(controller.signal, { sort: "updated_at" }),
    ])
      .then(([workflowList, runList]) => {
        if (controller.signal.aborted) return;
        setVersions(workflowList.items);
        setRuns(runList.items);
        setAgentState({ status: "ready", run: null });
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setVersions([]);
        setRuns([]);
        setError(toApiError(cause));
        // "ready" means the initial load finished, including finishing it
        // badly — the error renders alongside, not instead (same convention
        // as every other list view here).
        setAgentState({ status: "ready", run: null });
      });
    return () => controller.abort();
  }, []);

  const groups =
    versions !== null && runs !== null
      ? withRecentRuns(groupWorkflowVersions(versions), runs)
      : null;

  return (
    <div id="node-graphs-graphs-panel">
      {error ? <ErrorNotice error={error} /> : null}
      {groups === null ? (
        <p className="muted" id="workflows-loading">
          Loading workflows…
        </p>
      ) : groups.length === 0 ? (
        <p className="muted" id="workflows-empty">
          No workflows published yet. Publish one with{" "}
          <code>nodes workflow publish</code>.
        </p>
      ) : (
        <ul className="workflows-list" id="workflows-list">
          {groups.map((group) => (
            <li
              key={group.workflowKey}
              className="workflow-card"
              data-workflow-key={group.workflowKey}
            >
              <div className="workflow-card__head">
                <h2>{group.workflowKey}</h2>
                <span className="workflow-card__owner" data-workflow-owner>
                  {group.owner ?? "unowned"}
                </span>
              </div>

              <div className="table-scroll">
                <table className="ledger-table workflow-card__versions">
                  <caption>{group.versions.length} version(s)</caption>
                  <thead>
                    <tr>
                      <th scope="col">version</th>
                      <th scope="col">digest</th>
                      <th scope="col">published</th>
                    </tr>
                  </thead>
                  <tbody>
                    {group.versions.map((version) => (
                      <tr
                        key={version.digest}
                        data-workflow-digest={version.digest}
                      >
                        <td>{version.version}</td>
                        <td>
                          <code title={version.digest}>
                            {version.digest.slice(0, 20)}…
                          </code>
                        </td>
                        <td>
                          <time dateTime={version.created_at}>
                            {version.created_at}
                          </time>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>

              <div className="workflow-card__runs">
                <h3>Recent runs</h3>
                {group.recentRuns.length === 0 ? (
                  <p className="muted">No runs yet.</p>
                ) : (
                  <ul className="workflow-card__run-list">
                    {group.recentRuns.map((run) => (
                      <li key={run.id} data-run-id={run.id}>
                        <Link to={`/runs/${run.id}`}>{run.id}</Link>{" "}
                        <span data-run-state={run.state}>{run.state}</span>{" "}
                        <time dateTime={run.updated_at}>
                          {run.updated_at}
                        </time>
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

const toApiError = (cause: unknown): ApiError =>
  cause instanceof ApiError
    ? cause
    : new ApiError(0, String(cause), "check the browser console");

/**
 * The Nodes sub-tab (task t29's parser, rendered by task t31): every
 * distinct node definition across the latest version of each published
 * workflow — `deriveNodeDefinitions` over `GET /v1alpha1/workflows`, the
 * same fetch the workflow-cards panel makes (c20: only published-IR-derived
 * data, nothing invented). One card per definition: kind (word + the
 * NODE_KIND_PALETTE identity color, never color alone), the actor/runner/
 * approver ref backing its identity when the kind has one, and every
 * (workflow, node id) occurrence.
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
    <div id="node-graphs-nodes-panel">
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

/**
 * Every cross-run event type that can move Active Graphs presence: the
 * pulse family (dispatch/attempt/token/ledger activity), the run lifecycle
 * (created/terminal), and run.waiting. Module-level `as const`-style
 * constant because useSharedEvents requires a stable reference.
 */
const ACTIVE_GRAPH_EVENT_TYPES: readonly SharedEventType[] = [
  "dev.culture.nodes.run.created",
  "dev.culture.nodes.token.entered",
  "dev.culture.nodes.node-run.ready",
  "dev.culture.nodes.attempt.started",
  "dev.culture.nodes.actor.accepted",
  "dev.culture.nodes.attempt.completed",
  "dev.culture.nodes.node-run.failed",
  "dev.culture.nodes.attempt.retry-scheduled",
  "dev.culture.nodes.contract.rejected",
  "dev.culture.nodes.token.transitioned",
  "dev.culture.nodes.ledger.record-appended",
  "dev.culture.nodes.runner.operation-completed",
  "dev.culture.nodes.run.waiting",
  "dev.culture.nodes.run.completed",
  "dev.culture.nodes.run.failed",
  "dev.culture.nodes.run.cancelled",
  "dev.culture.nodes.run.bounded",
];

/** Minimum gap between presence (runs + node-runs) refetches. */
const PRESENCE_REFRESH_MS = 4000;
/** How many node-run rows the presence join reads (newest first). */
const PRESENCE_NODE_RUNS_LIMIT = 200;

const EMPTY_PULSES: Record<string, number> = {};

/**
 * The Active Graphs sub-tab (task t31, c31/h20): every workflow graph a
 * non-terminal run currently pins, rendered live. Liveness derivation is
 * `domain/active-presence.ts` — a pure readout of committed rows — and
 * aliveness on screen is driven by the initial fetch (workflows + runs +
 * node-runs) plus the shared cross-run SSE stream (task t27's one
 * connection): each committed event on a known run becomes exactly one
 * visible pulse; an event naming no known run is a no-op (h14); a
 * `run.created` for an unseen run triggers a debounced refetch of the
 * committed rows rather than a rendered placeholder.
 */
function ActiveGraphsPanel() {
  const [versions, setVersions] = useState<WorkflowVersion[] | null>(null);
  const [runs, setRuns] = useState<Run[] | null>(null);
  const [nodeRuns, setNodeRuns] = useState<NodeRunListItem[] | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [eventsTotal, setEventsTotal] = useState(0);
  const [pulsesTotal, setPulsesTotal] = useState(0);
  /** digest -> nodeId -> committed-event pulse count. */
  const [pulses, setPulses] = useState<Record<string, Record<string, number>>>(
    {},
  );
  const reducedMotion = useReducedMotion();

  const refreshTimer = useRef<ReturnType<typeof setTimeout> | undefined>(
    undefined,
  );
  const lastRefresh = useRef(0);

  useEffect(() => {
    const controller = new AbortController();
    setAgentState({ status: "loading", run: null });
    setError(null);
    setVersions(null);
    setRuns(null);
    setNodeRuns(null);
    Promise.all([
      listWorkflows(controller.signal),
      listRuns(controller.signal, { sort: "updated_at" }),
      listNodeRuns(controller.signal, { limit: PRESENCE_NODE_RUNS_LIMIT }),
    ])
      .then(([workflowList, runList, nodeRunList]) => {
        if (controller.signal.aborted) return;
        setVersions(workflowList.items);
        setRuns(runList.items);
        setNodeRuns(nodeRunList.items);
        setAgentState({ status: "ready", run: null });
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setVersions([]);
        setRuns([]);
        setNodeRuns([]);
        setError(toApiError(cause));
        // "ready" means the initial load finished, including finishing it
        // badly — the error renders alongside (the app-wide convention).
        setAgentState({ status: "ready", run: null });
      });
    return () => controller.abort();
  }, []);

  // Debounced presence refresh: at most one runs+node-runs refetch per
  // PRESENCE_REFRESH_MS, triggered by events that can change which runs
  // exist or which nodes hold work (the Mesh view's attribution pattern).
  const refreshPresence = useCallback(() => {
    if (refreshTimer.current) return;
    const since = Date.now() - lastRefresh.current;
    const wait = Math.max(0, PRESENCE_REFRESH_MS - since);
    refreshTimer.current = setTimeout(() => {
      refreshTimer.current = undefined;
      lastRefresh.current = Date.now();
      Promise.all([
        listRuns(undefined, { sort: "updated_at" }),
        listNodeRuns(undefined, { limit: PRESENCE_NODE_RUNS_LIMIT }),
      ])
        .then(([runList, nodeRunList]) => {
          setRuns(runList.items);
          setNodeRuns(nodeRunList.items);
        })
        .catch(() => {
          /* a failed refresh keeps the last honest presence */
        });
    }, wait);
  }, []);

  useEffect(
    () => () => {
      if (refreshTimer.current) clearTimeout(refreshTimer.current);
    },
    [],
  );

  const knownRunIds = useMemo(
    () => new Set((runs ?? []).map((run) => run.id)),
    [runs],
  );
  const runById = useMemo(
    () => new Map((runs ?? []).map((run) => [run.id, run])),
    [runs],
  );

  const applyEvent = (event: SharedEvent) => {
    setEventsTotal((n) => n + 1);
    const action = presenceEventAction(event.envelope, knownRunIds);
    if (action.kind === "none") return;
    if (action.kind === "run-appeared") {
      // Never render from the event alone — fetch the committed row.
      refreshPresence();
      return;
    }
    if (action.kind === "run-resolved") {
      // The terminal state is itself a committed fact; presence derivation
      // drops the run (and its graph, if it was the last one) from here.
      setRuns((prev) =>
        prev === null
          ? prev
          : prev.map((run) =>
              run.id === action.runId ? { ...run, state: action.state } : run,
            ),
      );
      return;
    }
    // A visible pulse: exactly one per committed event on a known run.
    setPulsesTotal((n) => n + 1);
    const run = runById.get(action.runId);
    if (run && action.nodeId !== null) {
      const digest = run.workflow_digest;
      const nodeId = action.nodeId;
      setPulses((prev) => ({
        ...prev,
        [digest]: {
          ...(prev[digest] ?? {}),
          [nodeId]: (prev[digest]?.[nodeId] ?? 0) + 1,
        },
      }));
    }
    if (needsPresenceRefresh(event.envelope.type)) refreshPresence();
  };

  // The stream opens the moment the view mounts, concurrently with the
  // initial workflows+runs+node-runs fetch — so committed events can (and in
  // practice routinely do) arrive before `runs` exists. Adjudicating them
  // against an empty known-run set would drop them as "unknown run", which
  // would be a lie: the run is known, we just had not read it yet. Hold them
  // instead, and replay once the committed rows land — every held event is
  // then judged against real rows, so events_total and pulses_total stay
  // consistent with each other and with h14 (still exactly one pulse per
  // committed event, and still none for an event naming a genuinely
  // unknown run).
  const pendingEvents = useRef<SharedEvent[]>([]);
  const applyEventRef = useRef(applyEvent);
  applyEventRef.current = applyEvent;

  const onEvent = (event: SharedEvent) => {
    if (runs === null) {
      pendingEvents.current.push(event);
      return;
    }
    applyEvent(event);
  };

  useEffect(() => {
    if (runs === null || pendingEvents.current.length === 0) return;
    const queued = pendingEvents.current;
    pendingEvents.current = [];
    for (const event of queued) applyEventRef.current(event);
  }, [runs]);

  const { status, lastEventId } = useSharedEvents(
    ACTIVE_GRAPH_EVENT_TYPES,
    onEvent,
  );

  const presence = useMemo(
    () =>
      versions !== null && runs !== null && nodeRuns !== null
        ? deriveActiveGraphs(versions, runs, nodeRuns)
        : null,
    [versions, runs, nodeRuns],
  );

  // The machine-readable mirror: the halos and pulses are visual claims
  // webglass cannot read, so every fact they assert lands in #agent-state
  // (the Mesh.tsx:177-194 convention). Published once real data exists —
  // while loading, the pixels claim nothing and neither does the mirror.
  useEffect(() => {
    if (presence === null) return;
    setAgentState({
      active_graphs: {
        graph_count: presence.length,
        active_run_count: presence.reduce(
          (total, entry) => total + entry.runIds.length,
          0,
        ),
        active_node_count: presence.reduce(
          (total, entry) => total + entry.activeNodeIds.length,
          0,
        ),
        connection: status,
        last_event_id: lastEventId,
        events_total: eventsTotal,
        pulses_total: pulsesTotal,
        reduced_motion: reducedMotion,
      },
    });
  }, [presence, status, lastEventId, eventsTotal, pulsesTotal, reducedMotion]);

  // Leaving the view drops the block from #agent-state (undefined keys are
  // omitted by JSON.stringify — the mesh/authoring/statistics convention).
  useEffect(() => () => setAgentState({ active_graphs: undefined }), []);

  return (
    <div id="node-graphs-active-panel">
      <p
        id="active-graphs-connection"
        className="mesh-connection"
        data-state={status}
        role="status"
      >
        <span className="mesh-connection__dot" aria-hidden="true" />
        {status === "live" ? "live" : "reconnecting"}
      </p>
      {error ? <ErrorNotice error={error} /> : null}
      {presence === null ? (
        <p className="muted" id="active-graphs-loading">
          Loading active graphs…
        </p>
      ) : presence.length === 0 ? (
        <p className="muted" id="active-graphs-empty">
          No graphs alive right now. A workflow appears here the moment a run
          of it holds active tokens — start one with{" "}
          <code>nodes run start</code>.
        </p>
      ) : (
        <div className="active-graphs" id="active-graphs-list">
          {presence.map((entry) => (
            <ActiveGraphCanvas
              key={entry.digest}
              presence={entry}
              reducedMotion={reducedMotion}
              pulses={pulses[entry.digest] ?? EMPTY_PULSES}
            />
          ))}
        </div>
      )}
    </div>
  );
}

export function NodeGraphs() {
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
    <section className="view-rail node-graphs-view">
      <div className="node-graphs-view__head">
        <div>
          <h1>Node Graphs</h1>
          <p className="muted">
            Node definitions, the graphs strict node-to-node handovers and
            events wire them into, and which of those graphs are alive right
            now.
          </p>
        </div>
        <Link
          to="/workflows/new"
          id="new-workflow-link"
          className="author-workflow__button author-workflow__button--primary"
        >
          New workflow
        </Link>
      </div>

      <div
        id="node-graphs-toggle"
        className="view-toggle"
        role="group"
        aria-label="Node Graphs sub-tab"
      >
        <button
          type="button"
          id="node-graphs-toggle-nodes"
          aria-pressed={tab === "nodes"}
          onClick={() => setTab("nodes")}
        >
          Nodes
        </button>
        <button
          type="button"
          id="node-graphs-toggle-graphs"
          aria-pressed={tab === "graphs"}
          onClick={() => setTab("graphs")}
        >
          Node Graphs
        </button>
        <button
          type="button"
          id="node-graphs-toggle-active"
          aria-pressed={tab === "active"}
          onClick={() => setTab("active")}
        >
          Active Graphs
        </button>
      </div>

      {tab === "nodes" ? <NodesPanel /> : null}
      {tab === "graphs" ? <NodeGraphsPanel /> : null}
      {tab === "active" ? <ActiveGraphsPanel /> : null}
    </section>
  );
}

export default NodeGraphs;
