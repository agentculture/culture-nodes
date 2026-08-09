import type { RunEvent } from "../api/types";
import { shortEventType } from "../domain/run-state";

export interface EventTimelineProps {
  events: RunEvent[];
  /** Highlight the rows that touch this node, when one is selected. */
  selectedNodeId?: string | null;
  onSelectNode?: (nodeId: string) => void;
}

function nodeOf(event: RunEvent): string | undefined {
  const data = event.envelope.data ?? {};
  for (const key of ["node_id", "to_node", "end_node", "from_node"]) {
    const value = data[key];
    if (typeof value === "string" && value !== "") return value;
  }
  return undefined;
}

function timeOf(event: RunEvent): string {
  const raw = event.envelope.time;
  const parsed = new Date(raw);
  if (Number.isNaN(parsed.getTime())) return raw;
  return parsed.toISOString().replace("T", " ").replace(/\.\d+Z$/, "Z");
}

/**
 * The non-graph run projection PRD §8.8 requires: "a non-graph run timeline
 * containing the same information". Every committed event the stream
 * delivered, in order, with its type, the node it touched, and its time —
 * readable, linearizable, and reachable with a keyboard alone.
 */
export function EventTimeline({
  events,
  selectedNodeId,
  onSelectNode,
}: EventTimelineProps) {
  if (events.length === 0) {
    return (
      <p id="event-timeline-empty" className="muted">
        No committed events yet. The timeline fills as the run emits them.
      </p>
    );
  }

  return (
    <ol id="event-timeline" className="timeline" aria-label="Run event timeline">
      {events.map((event) => {
        const node = nodeOf(event);
        const type = shortEventType(event.envelope.type);
        const isSelected = Boolean(node && node === selectedNodeId);
        return (
          <li
            key={`${event.sequence}-${event.envelope.id}`}
            className={`timeline__item${isSelected ? " is-selected" : ""}`}
            data-event-type={event.envelope.type}
          >
            <span className="timeline__seq" aria-label={`sequence ${event.sequence}`}>
              {event.sequence}
            </span>
            <time className="timeline__time" dateTime={event.envelope.time}>
              {timeOf(event)}
            </time>
            <span className="timeline__type">{type}</span>
            {node ? (
              onSelectNode ? (
                <button
                  type="button"
                  className="timeline__node"
                  onClick={() => onSelectNode(node)}
                  aria-label={`open detail for node ${node}`}
                >
                  {node}
                </button>
              ) : (
                <span className="timeline__node">{node}</span>
              )
            ) : (
              <span className="timeline__node muted">—</span>
            )}
          </li>
        );
      })}
    </ol>
  );
}

export default EventTimeline;
