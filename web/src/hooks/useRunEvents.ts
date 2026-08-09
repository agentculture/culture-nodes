import { useEffect, useRef, useState } from "react";
import { runEventsUrl } from "../api/client";
import type { EventEnvelope, RunEvent } from "../api/types";
import { TERMINAL_EVENT_TYPES } from "../domain/run-state";

/**
 * Every event type the stream names in its `event:` field. EventSource has
 * no wildcard listener — an event with `event: <type>` is dispatched under
 * that name and never reaches `onmessage` — so each one is subscribed
 * explicitly. Unknown types are simply not rendered; the timeline says so
 * rather than pretending the stream was empty.
 */
export const SUBSCRIBED_EVENT_TYPES = [
  "dev.culture.nodes.run.created",
  "dev.culture.nodes.token.entered",
  "dev.culture.nodes.node-run.ready",
  "dev.culture.nodes.attempt.started",
  "dev.culture.nodes.actor.accepted",
  "dev.culture.nodes.attempt.completed",
  "dev.culture.nodes.ledger.record-appended",
  "dev.culture.nodes.ledger.review-committed",
  "dev.culture.nodes.runner.operation-completed",
  "dev.culture.nodes.contract.rejected",
  "dev.culture.nodes.token.transitioned",
  "dev.culture.nodes.run.waiting",
  "dev.culture.nodes.run.completed",
  "dev.culture.nodes.run.failed",
  "dev.culture.nodes.run.cancelled",
  "dev.culture.nodes.run.bounded",
] as const;

export type StreamStatus = "connecting" | "open" | "closed" | "error";

export interface RunEventsResult {
  events: RunEvent[];
  status: StreamStatus;
  /** The last per-run sequence received — the resume point. */
  lastEventId: string | null;
}

const RECONNECT_DELAY_MS = 1000;
const MAX_RECONNECTS = 5;

/**
 * Subscribe to a run's committed event stream.
 *
 * Resume: the browser replays `Last-Event-ID` automatically on its own
 * reconnect, which the API honours. When the *server* closes the stream
 * before a terminal event (a relay restart, a proxy timeout), the browser
 * treats it as a clean end and stops — so this hook reopens explicitly with
 * `?from=<lastEventId>`, the header-less form of the same resume point
 * (openapi.yaml, streamRunEvents). No event is replayed twice and none is
 * skipped.
 */
export function useRunEvents(runId: string | undefined): RunEventsResult {
  const [events, setEvents] = useState<RunEvent[]>([]);
  const [status, setStatus] = useState<StreamStatus>("connecting");
  const lastIdRef = useRef<string | null>(null);
  const [lastEventId, setLastEventId] = useState<string | null>(null);

  useEffect(() => {
    if (!runId) return;
    if (typeof EventSource === "undefined") {
      setStatus("error");
      return;
    }

    lastIdRef.current = null;
    setEvents([]);
    setLastEventId(null);

    let source: EventSource | null = null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let reconnects = 0;
    let terminal = false;
    let disposed = false;

    const handle = (raw: MessageEvent<string>) => {
      let envelope: EventEnvelope;
      try {
        envelope = JSON.parse(raw.data) as EventEnvelope;
      } catch {
        return; // a frame we cannot parse is dropped, never guessed at
      }
      if (raw.lastEventId) {
        lastIdRef.current = raw.lastEventId;
        setLastEventId(raw.lastEventId);
      }
      setEvents((prev) => [
        ...prev,
        { sequence: raw.lastEventId || String(prev.length + 1), envelope },
      ]);
      if (TERMINAL_EVENT_TYPES.has(envelope.type)) {
        terminal = true;
        setStatus("closed");
        source?.close();
      }
    };

    const connect = () => {
      if (disposed) return;
      setStatus("connecting");
      source = new EventSource(runEventsUrl(runId, lastIdRef.current ?? undefined));
      source.onopen = () => {
        reconnects = 0;
        setStatus("open");
      };
      for (const type of SUBSCRIBED_EVENT_TYPES) {
        source.addEventListener(type, handle as EventListener);
      }
      source.onmessage = handle;
      source.onerror = () => {
        if (disposed || terminal) return;
        // readyState CONNECTING means the browser is retrying on its own
        // (and will send Last-Event-ID); only a CLOSED source needs us.
        if (source && source.readyState === EventSource.CLOSED) {
          if (reconnects >= MAX_RECONNECTS) {
            setStatus("error");
            return;
          }
          reconnects += 1;
          setStatus("error");
          timer = setTimeout(connect, RECONNECT_DELAY_MS);
        }
      };
    };

    connect();

    return () => {
      disposed = true;
      if (timer) clearTimeout(timer);
      source?.close();
    };
  }, [runId]);

  return { events, status, lastEventId };
}
