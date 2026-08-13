import { useEffect, useRef, useState } from "react";
import { meshEventsUrl } from "../api/client";
import type { EventEnvelope } from "../api/types";
import { SUBSCRIBED_EVENT_TYPES } from "./useRunEvents";

/**
 * Everything the cross-run stream can name in its `event:` field: the
 * per-run stream's vocabulary plus the engine types the per-run view never
 * needed to subscribe to (internal/engine/events.go declares them; the
 * cross-run endpoint forwards whatever the events table holds). EventSource
 * has no wildcard listener, so each is subscribed explicitly — an unknown
 * future type is simply not seen, never mis-rendered.
 */
export const MESH_EVENT_TYPES = [
  ...SUBSCRIBED_EVENT_TYPES,
  "dev.culture.nodes.node-run.failed",
  "dev.culture.nodes.attempt.retry-scheduled",
  "dev.culture.nodes.human-task.created",
  "dev.culture.nodes.human-task.decided",
] as const;

/**
 * The mesh stream's honest connection vocabulary: `live` only while the
 * EventSource is actually open; anything else is `reconnecting` — the view
 * never fakes liveness (t18 acceptance #2).
 */
export type MeshStreamStatus = "live" | "reconnecting";

export interface MeshEvent {
  /** The SSE `id:` field — the events table's ULID, the resume cursor. */
  id: string;
  envelope: EventEnvelope;
}

export interface MeshEventsResult {
  status: MeshStreamStatus;
  lastEventId: string | null;
}

const BASE_RECONNECT_DELAY_MS = 1000;
const MAX_RECONNECT_DELAY_MS = 15_000;

/**
 * Subscribe to `GET /v1alpha1/events` (task t17) — the cross-run committed
 * event stream — delivering each frame to `onEvent` as it arrives instead
 * of accumulating an ever-growing array (this stream never terminates; a
 * mesh view holds counters and a bounded particle pool, not history).
 *
 * Resume follows useRunEvents' discipline: the browser replays
 * `Last-Event-ID` on its own reconnects; when the server closes the stream
 * outright this hook reopens with `?from=<last id>`. Unlike the per-run
 * hook there is no terminal event and no retry cap — a live overview keeps
 * quietly retrying with capped backoff, and the honest `reconnecting`
 * status is the user-visible truth in the meantime.
 */
export function useMeshEvents(
  onEvent: (event: MeshEvent) => void,
): MeshEventsResult {
  const [status, setStatus] = useState<MeshStreamStatus>("reconnecting");
  const [lastEventId, setLastEventId] = useState<string | null>(null);
  const onEventRef = useRef(onEvent);
  onEventRef.current = onEvent;

  useEffect(() => {
    if (typeof EventSource === "undefined") {
      setStatus("reconnecting");
      return;
    }

    let source: EventSource | null = null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let attempts = 0;
    let disposed = false;
    let lastId: string | null = null;

    const handle = (raw: MessageEvent<string>) => {
      let envelope: EventEnvelope;
      try {
        envelope = JSON.parse(raw.data) as EventEnvelope;
      } catch {
        return; // a frame we cannot parse is dropped, never guessed at
      }
      if (raw.lastEventId) {
        lastId = raw.lastEventId;
        setLastEventId(raw.lastEventId);
      }
      onEventRef.current({ id: raw.lastEventId || envelope.id, envelope });
    };

    const connect = () => {
      if (disposed) return;
      setStatus("reconnecting");
      source = new EventSource(meshEventsUrl(lastId ?? undefined));
      source.onopen = () => {
        attempts = 0;
        setStatus("live");
      };
      for (const type of MESH_EVENT_TYPES) {
        source.addEventListener(type, handle as EventListener);
      }
      source.onmessage = handle;
      source.onerror = () => {
        if (disposed) return;
        // CONNECTING = the browser is retrying on its own (and will send
        // Last-Event-ID itself); report honestly but let it work.
        if (source && source.readyState === EventSource.CLOSED) {
          attempts += 1;
          setStatus("reconnecting");
          const delay = Math.min(
            BASE_RECONNECT_DELAY_MS * 2 ** Math.min(attempts - 1, 4),
            MAX_RECONNECT_DELAY_MS,
          );
          timer = setTimeout(connect, delay);
        } else {
          setStatus("reconnecting");
        }
      };
    };

    connect();

    return () => {
      disposed = true;
      if (timer) clearTimeout(timer);
      source?.close();
    };
  }, []);

  return { status, lastEventId };
}
