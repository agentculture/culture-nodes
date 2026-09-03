import { useCallback, useEffect, useMemo, useRef, useState } from "react";
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
import { useReducedMotion } from "../hooks/useReducedMotion";
import {
  type SharedEvent,
  type SharedEventType,
} from "../hooks/useSharedEvents";
import { useSnapshotReconcile } from "../hooks/useSnapshotReconcile";

/**
 * The Active Graphs sub-view of Design (task t31, moved out of the retired
 * `routes/NodeGraphs.tsx` by task t8, otherwise unchanged).
 *
 * It lives in its own module rather than inside `routes/Design.tsx` for one
 * reason: Design now hosts three panels — the gallery (every published
 * graph, run or no run), the node-definition catalog, and this — and a
 * single file holding all three would run past the 1000-line hard limit
 * `tests/lint/filelength_test.go` enforces. The split is along the seam the
 * panels already had: this one owns the live cross-run stream, the other two
 * own committed rows read once.
 */

const toApiError = (cause: unknown): ApiError =>
  cause instanceof ApiError
    ? cause
    : new ApiError(0, String(cause), "check the browser console");

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
export function ActiveGraphsPanel() {
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
        resolveSnapshot();
        setAgentState({ status: "ready", run: null });
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setVersions([]);
        setRuns([]);
        setNodeRuns([]);
        resolveSnapshot();
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
  const { status, lastEventId, resolveSnapshot } = useSnapshotReconcile(
    ACTIVE_GRAPH_EVENT_TYPES,
    applyEvent,
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
    <div id="design-active-panel">
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

export default ActiveGraphsPanel;
