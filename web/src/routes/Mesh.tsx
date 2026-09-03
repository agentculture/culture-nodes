import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { setAgentState } from "../agent-state/store";
import { ApiError, listActors, listNodeRuns, listRuns } from "../api/client";
import type { Actor, NodeRunListItem, Run } from "../api/types";
import ErrorNotice from "../components/ErrorNotice";
import MeshCanvas from "../components/MeshCanvas";
import type { MeshActionBus } from "../components/MeshCanvas";
import {
  assembleMeshGraph,
  meshEventAction,
  needsAttributionRefresh,
} from "../domain/mesh";
import type { MeshEventAction } from "../domain/mesh";
import { useMeshEvents } from "../hooks/useMeshEvents";
import type { MeshEvent } from "../hooks/useMeshEvents";
import { useReducedMotion } from "../hooks/useReducedMotion";

/**
 * The live-mesh overview (task t18): the control plane at the center,
 * registered actors (`GET /v1alpha1/actors`, task t15) orbiting as glowing
 * nodes, active runs as satellites of the actor executing them, and every
 * committed event from the cross-run stream (`GET /v1alpha1/events`, task
 * t17) arriving as a visible pulse along the relevant edge.
 *
 * Honesty (h14): everything on the canvas is live, committed state — no
 * canned data, no decorative traffic. Actor<->run edges come from the
 * node-runs listing's `actor_id` (the most recent attempt's actors-table
 * reference), because the committed events themselves carry no actor
 * reference today — see domain/mesh.ts. That listing is refetched
 * (debounced) when the stream reports node-run/attempt activity, so a
 * fresh dispatch pulls its run from the control plane to its actor within
 * one refresh. The connection indicator is honest too: `live` only while
 * the SSE stream is actually open, `reconnecting` otherwise — never faked.
 */

/** How long a resolved run lingers so its settle/fade can finish. */
const RESOLVE_LINGER_MS = 3400;
/** Minimum gap between attribution (node-runs) refetches. */
const ATTRIBUTION_REFRESH_MS = 4000;
/** How many node-run rows the attribution join reads (newest first). */
const ATTRIBUTION_PAGE_LIMIT = 200;

export function Mesh() {
  const [actors, setActors] = useState<Actor[]>([]);
  const [runs, setRuns] = useState<Run[]>([]);
  const [nodeRuns, setNodeRuns] = useState<NodeRunListItem[]>([]);
  const [error, setError] = useState<ApiError | null>(null);
  const [eventsTotal, setEventsTotal] = useState(0);
  const [pulsesTotal, setPulsesTotal] = useState(0);
  const reducedMotion = useReducedMotion();

  const busRef = useRef<MeshActionBus>({ listener: null });
  const removalTimers = useRef(new Map<string, ReturnType<typeof setTimeout>>());
  const lastAttributionFetch = useRef(0);
  const attributionTimer = useRef<ReturnType<typeof setTimeout> | undefined>(
    undefined,
  );

  // Initial load: the three read surfaces, in parallel.
  useEffect(() => {
    const controller = new AbortController();
    setAgentState({ status: "loading", run: null });
    setError(null);
    Promise.all([
      listActors(controller.signal),
      listRuns(controller.signal, { sort: "updated_at" }),
      listNodeRuns(controller.signal, { limit: ATTRIBUTION_PAGE_LIMIT }),
    ])
      .then(([actorList, runList, nodeRunList]) => {
        if (controller.signal.aborted) return;
        setActors(actorList.items);
        // The SSE stream starts alongside this fetch and may already have
        // added a run.created placeholder — keep any run the listing does
        // not know yet rather than overwriting it away (replay race).
        setRuns((prev) => {
          const fetched = runList.items;
          const known = new Set(fetched.map((run) => run.id));
          return [...fetched, ...prev.filter((run) => !known.has(run.id))];
        });
        setNodeRuns(nodeRunList.items);
        resolveSnapshot();
        setAgentState({ status: "ready", run: null });
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setError(
          cause instanceof ApiError
            ? cause
            : new ApiError(0, String(cause), "check the browser console"),
        );
        resolveSnapshot();
        // "ready" means the initial load finished — including finishing it
        // badly; the error renders alongside (the app-wide convention).
        setAgentState({ status: "ready", run: null });
      });
    return () => controller.abort();
    // resolveSnapshot is stable for this mount.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Debounced attribution refresh: at most one node-runs refetch per
  // ATTRIBUTION_REFRESH_MS, triggered by node-run/attempt events.
  const refreshAttribution = useCallback(() => {
    if (attributionTimer.current) return;
    const since = Date.now() - lastAttributionFetch.current;
    const wait = Math.max(0, ATTRIBUTION_REFRESH_MS - since);
    attributionTimer.current = setTimeout(() => {
      attributionTimer.current = undefined;
      lastAttributionFetch.current = Date.now();
      listNodeRuns(undefined, { limit: ATTRIBUTION_PAGE_LIMIT })
        .then((list) => setNodeRuns(list.items))
        .catch(() => {
          /* a failed refresh keeps the last honest attribution */
        });
    }, wait);
  }, []);

  useEffect(
    () => () => {
      if (attributionTimer.current) clearTimeout(attributionTimer.current);
      for (const timer of removalTimers.current.values()) clearTimeout(timer);
    },
    [],
  );

  const applyAction = useCallback(
    (action: MeshEventAction, occurredAt: string) => {
      if (action.kind === "run-added") {
        setRuns((prev) => {
          if (prev.some((run) => run.id === action.runId)) return prev;
          // The run.created payload carries the workflow key, not the run's
          // operator-given name — surface it as a derived hint until the
          // debounced list refresh brings the real record.
          const placeholder: Run = {
            id: action.runId,
            workflow_digest: "",
            state: "created",
            created_at: occurredAt,
            updated_at: occurredAt,
            display_hint: action.label,
          };
          return [...prev, placeholder];
        });
      } else if (action.kind === "run-resolved") {
        // Let the canvas play the settle/flare before the node leaves the
        // graph; the run's own state stays whatever the API last said.
        if (!removalTimers.current.has(action.runId)) {
          const timer = setTimeout(() => {
            removalTimers.current.delete(action.runId);
            setRuns((prev) => prev.filter((run) => run.id !== action.runId));
          }, RESOLVE_LINGER_MS);
          removalTimers.current.set(action.runId, timer);
        }
      }
      if (action.kind === "pulse" || action.kind === "run-resolved") {
        setPulsesTotal((n) => n + 1);
      }
      busRef.current.listener?.(action);
    },
    [],
  );

  const onEvent = useCallback(
    (event: MeshEvent) => {
      setEventsTotal((n) => n + 1);
      applyAction(meshEventAction(event.envelope), event.envelope.time);
      if (needsAttributionRefresh(event.envelope.type)) refreshAttribution();
    },
    [applyAction, refreshAttribution],
  );

  const { status, lastEventId, resolveSnapshot } = useMeshEvents(onEvent);

  const graph = useMemo(
    () => assembleMeshGraph(actors, runs, nodeRuns),
    [actors, runs, nodeRuns],
  );

  // The machine-readable mirror: a canvas is untestable by webglass, so
  // every fact the pixels claim is asserted here instead (acceptance #5).
  useEffect(() => {
    setAgentState({
      mesh: {
        actor_count: graph.actors.length,
        run_count: graph.runs.length,
        edge_count: graph.edges.length,
        connection: status,
        last_event_id: lastEventId,
        events_total: eventsTotal,
        pulses_total: pulsesTotal,
        reduced_motion: reducedMotion,
      },
    });
  }, [graph, status, lastEventId, eventsTotal, pulsesTotal, reducedMotion]);

  // Leaving the view drops the mesh block from #agent-state (undefined keys
  // are omitted by JSON.stringify — the authoring/statistics convention).
  useEffect(() => () => setAgentState({ mesh: undefined }), []);

  return (
    <section className="view-rail mesh-view">
      <header className="mesh-view__head">
        <div>
          <h1>Mesh</h1>
          <p className="muted">
            The org, breathing: actors around the control plane, active runs
            beside the actor executing them. Every pulse is a committed
            event.
          </p>
        </div>
        <p
          id="mesh-connection"
          className="mesh-connection"
          data-state={status}
          role="status"
        >
          <span className="mesh-connection__dot" aria-hidden="true" />
          {status === "live" ? "live" : "reconnecting"}
        </p>
      </header>

      {error ? <ErrorNotice error={error} /> : null}

      <MeshCanvas
        graph={graph}
        reducedMotion={reducedMotion}
        bus={busRef.current}
      />

      <ul className="mesh-legend" aria-label="Mesh legend">
        <li>
          <span className="mesh-legend__glyph mesh-legend__glyph--agent" />
          agent
        </li>
        <li>
          <span className="mesh-legend__glyph mesh-legend__glyph--human" />
          human
        </li>
        <li>
          <span className="mesh-legend__glyph mesh-legend__glyph--runner" />
          runner
        </li>
        <li>
          <span className="mesh-legend__glyph mesh-legend__glyph--run" />
          active run
        </li>
        <li className="mesh-legend__note">
          particles = committed events · hover or use arrow keys to inspect
        </li>
      </ul>

      {graph.actors.length === 0 && !error ? (
        <p className="muted" id="mesh-empty">
          No actors registered yet. Register one with{" "}
          <code>deploy/prod/register-actor.sh</code> and it appears here.
        </p>
      ) : null}
    </section>
  );
}

export default Mesh;
