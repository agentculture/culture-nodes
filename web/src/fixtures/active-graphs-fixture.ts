import type { EventEnvelope, NodeRunListItem } from "../api/types";

/**
 * Fixture data for the Active Graphs sub-tab (task t31): the committed rows
 * and events that make `workflows-fixture.ts`'s one non-terminal run —
 * `run-deliver-v2-01J8XKWORKFLOWS02`, pinning deliver-change v2 — visibly
 * alive. Used by the vitest component tests and the Playwright e2e spec
 * (via e2e/fixtures/api.ts) alike.
 *
 * Honesty shape, deliberately mirrored from the spec's h14 wording:
 *   - exactly one workflow digest has active runs, so exactly one graph
 *     renders with a halo (hello-world's only run is completed; the orphan
 *     run matches no published version — neither may appear);
 *   - one node-run row is non-terminal (`build`, running), so exactly one
 *     node carries live presence;
 *   - the event history holds one committed event on the known run (a
 *     visible pulse) and one naming a run the view never loaded (a no-op):
 *     events_total = 2, pulses_total = 1.
 */

export const ACTIVE_RUN_ID = "run-deliver-v2-01J8XKWORKFLOWS02";
export const ACTIVE_NODE_ID = "build";
export const UNKNOWN_RUN_ID = "run-ghost-never-loaded";

const EMPTY_USAGE = {
  input_tokens: 0,
  output_tokens: 0,
  cached_input_tokens: 0,
  reasoning_tokens: 0,
  attempts_reported: 0,
  attempts_not_reported: 1,
};

/** `GET /v1alpha1/node-runs` rows: one live, one long-finished. */
export const ACTIVE_NODE_RUNS: NodeRunListItem[] = [
  {
    id: "nr-active-build",
    run_id: ACTIVE_RUN_ID,
    node_id: ACTIVE_NODE_ID,
    state: "running",
    created_at: "2026-08-09T09:46:00Z",
    updated_at: "2026-08-09T09:56:00Z",
    usage: EMPTY_USAGE,
  },
  {
    id: "nr-active-intake",
    run_id: ACTIVE_RUN_ID,
    node_id: "intake",
    state: "completed",
    outcome: "completed",
    created_at: "2026-08-09T09:45:00Z",
    updated_at: "2026-08-09T09:45:30Z",
    completed_at: "2026-08-09T09:45:30Z",
    usage: EMPTY_USAGE,
  },
];

export interface ActiveFixtureEvent {
  /** The SSE `id:` field — the events table's ULID primary key. */
  id: string;
  envelope: EventEnvelope;
}

function activeEvent(
  id: string,
  type: string,
  runId: string,
  data: Record<string, unknown>,
): ActiveFixtureEvent {
  return {
    id,
    envelope: {
      id,
      source: "nodes",
      specversion: "1.0",
      type: `dev.culture.nodes.${type}`,
      subject: runId,
      time: "2026-08-09T09:57:00Z",
      datacontenttype: "application/json",
      data: { run_id: runId, ...data },
    },
  };
}

/**
 * The committed cross-run history the fixture stream replays, in id order:
 * one pulse-producing event on the loaded run, then one event naming a run
 * the view never fetched — which must be a no-op (h14).
 */
export const ACTIVE_EVENTS: ActiveFixtureEvent[] = [
  activeEvent("01ACTIVE0000000000000001", "attempt.started", ACTIVE_RUN_ID, {
    node_id: ACTIVE_NODE_ID,
    node_run_id: "nr-active-build",
  }),
  activeEvent("01ACTIVE0000000000000002", "attempt.started", UNKNOWN_RUN_ID, {
    node_id: "somewhere",
  }),
];

export const ACTIVE_EVENTS_TOTAL = ACTIVE_EVENTS.length;
export const ACTIVE_PULSES_TOTAL = 1;
export const ACTIVE_LAST_EVENT_ID = ACTIVE_EVENTS[ACTIVE_EVENTS.length - 1].id;

/**
 * Serialize fixture events as the cross-run SSE body, exactly as
 * writeCrossRunSSEEvent frames them (same framing as mesh-fixture.ts's
 * helper — duplicated here so this fixture stays importable on its own).
 */
export function activeEventsAsSse(events: ActiveFixtureEvent[]): string {
  return events
    .map(
      (item) =>
        `id: ${item.id}\nevent: ${item.envelope.type}\ndata: ${JSON.stringify(item.envelope)}\n\n`,
    )
    .join("");
}
