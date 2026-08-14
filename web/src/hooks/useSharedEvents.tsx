import {
  useCallback,
  useEffect,
  useRef,
  useSyncExternalStore,
  type ReactNode,
} from "react";
import { meshEventsUrl } from "../api/client";
import type { EventEnvelope } from "../api/types";
import { SUBSCRIBED_EVENT_TYPES } from "./useRunEvents";

/**
 * Everything the cross-run stream can name in its `event:` field: the
 * per-run stream's vocabulary plus the engine types the per-run view never
 * needed to subscribe to (internal/engine/events.go declares them; the
 * cross-run endpoint forwards whatever the events table holds). EventSource
 * has no wildcard listener, so each is registered explicitly — an unknown
 * future type is simply not seen, never mis-rendered. This is the single
 * fixed vocabulary the shared connection listens for; a subscriber's own
 * `types` argument then filters which of those reach it.
 */
export const SHARED_EVENT_TYPES = [
  ...SUBSCRIBED_EVENT_TYPES,
  "dev.culture.nodes.node-run.failed",
  "dev.culture.nodes.attempt.retry-scheduled",
  "dev.culture.nodes.human-task.created",
  "dev.culture.nodes.human-task.decided",
] as const;

export type SharedEventType = (typeof SHARED_EVENT_TYPES)[number];

/**
 * The shared stream's honest connection vocabulary: `live` only while the
 * one underlying EventSource is actually open; anything else is
 * `reconnecting` — no subscriber ever sees a faked liveness (mirrors the
 * pre-t27 per-hook contract, t18 acceptance #2).
 */
export type SharedStreamStatus = "live" | "reconnecting";

export interface SharedEvent {
  /** The SSE `id:` field — the events table's ULID, the resume cursor. */
  id: string;
  envelope: EventEnvelope;
}

type Listener = (event: SharedEvent) => void;
type StatusListener = () => void;

interface StreamSnapshot {
  status: SharedStreamStatus;
  lastEventId: string | null;
}

const BASE_RECONNECT_DELAY_MS = 1000;
const MAX_RECONNECT_DELAY_MS = 15_000;

const SERVER_SNAPSHOT: StreamSnapshot = {
  status: "reconnecting",
  lastEventId: null,
};

/**
 * The one EventSource this whole app holds against the cross-run stream
 * (task t27, c48/h41): a module-level manager rather than React state, so
 * its lifetime is independent of which components happen to be mounted at
 * any moment. `subscribe` is reference-counted — the underlying connection
 * opens on the first subscriber and closes on the last one leaving, but
 * every subscribe/unsubscribe cycle in between reuses the same EventSource
 * (h41's "never reconnect" requirement). `SharedEventsProvider` below holds
 * one permanent reference for the app's lifetime so view-level mount/unmount
 * churn never drops the count to zero in the first place.
 *
 * Deliberately not React Context for the connection itself: routes/Mesh.tsx
 * (t28) must keep working, unmodified, with its existing tests that render
 * `<Mesh />` with no provider ancestor (see Mesh.test.tsx) — a module
 * singleton subscribes/connects the same way with or without
 * `SharedEventsProvider` mounted above it. The provider's only job is to
 * pin the connection open for the app's real lifetime; it is not required
 * for correctness, only for avoiding a reconnect during route transitions
 * when every view-level subscriber briefly reaches zero.
 */
class SharedEventsManager {
  private source: EventSource | null = null;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private attempts = 0;
  private lastId: string | null = null;
  private refCount = 0;
  private readonly listenersByType = new Map<SharedEventType, Set<Listener>>();
  private readonly statusListeners = new Set<StatusListener>();
  private snapshot: StreamSnapshot = SERVER_SNAPSHOT;

  private setSnapshot(next: StreamSnapshot) {
    this.snapshot = next;
    for (const listener of this.statusListeners) listener();
  }

  private handleMessage = (raw: MessageEvent<string>) => {
    let envelope: EventEnvelope;
    try {
      envelope = JSON.parse(raw.data) as EventEnvelope;
    } catch {
      return; // a frame we cannot parse is dropped, never guessed at
    }
    const id = raw.lastEventId || envelope.id;
    if (raw.lastEventId && raw.lastEventId !== this.lastId) {
      this.lastId = raw.lastEventId;
      this.setSnapshot({ status: this.snapshot.status, lastEventId: this.lastId });
    }
    // Routing key is the envelope's own `type` field, not the native SSE
    // frame name: the server always sets `event:` to the same value it puts
    // in the CloudEvents body (internal/api/events.go), so the two agree in
    // production, but the parsed body is the one guaranteed present —
    // `addEventListener("message", ...)`'s fallback path never populates a
    // native event name at all.
    const listeners = this.listenersByType.get(envelope.type as SharedEventType);
    if (!listeners || listeners.size === 0) return;
    const event: SharedEvent = { id, envelope };
    for (const listener of Array.from(listeners)) listener(event);
  };

  private connect = () => {
    if (typeof EventSource === "undefined") return;
    this.setSnapshot({ status: "reconnecting", lastEventId: this.lastId });
    const source = new EventSource(meshEventsUrl(this.lastId ?? undefined));
    this.source = source;
    source.onopen = () => {
      if (this.source !== source) return;
      this.attempts = 0;
      this.setSnapshot({ status: "live", lastEventId: this.lastId });
    };
    for (const type of SHARED_EVENT_TYPES) {
      source.addEventListener(type, this.handleMessage as EventListener);
    }
    source.onmessage = this.handleMessage;
    source.onerror = () => {
      if (this.source !== source) return; // a stale handler from a torn-down connection
      // readyState CONNECTING means the browser is retrying on its own (and
      // will send Last-Event-ID itself); report honestly but let it work.
      if (source.readyState === EventSource.CLOSED) {
        this.attempts += 1;
        this.setSnapshot({ status: "reconnecting", lastEventId: this.lastId });
        const delay = Math.min(
          BASE_RECONNECT_DELAY_MS * 2 ** Math.min(this.attempts - 1, 4),
          MAX_RECONNECT_DELAY_MS,
        );
        this.timer = setTimeout(this.connect, delay);
      } else {
        this.setSnapshot({ status: "reconnecting", lastEventId: this.lastId });
      }
    };
  };

  private ensureConnected() {
    if (this.source || this.timer) return;
    this.attempts = 0;
    this.connect();
  }

  private teardown() {
    if (this.timer) clearTimeout(this.timer);
    this.timer = undefined;
    this.source?.close();
    this.source = null;
    this.setSnapshot({ status: "reconnecting", lastEventId: this.lastId });
  }

  /**
   * Register `onEvent` for exactly the named types (EventSource has no
   * wildcard, so a subscriber's interest is always an explicit list) and
   * return an unsubscribe function. The underlying connection opens lazily
   * on the first subscriber of the app and closes only when the last one —
   * across every subscriber, not just this call's types — leaves.
   */
  subscribe(types: readonly SharedEventType[], onEvent: Listener): () => void {
    for (const type of types) {
      let set = this.listenersByType.get(type);
      if (!set) {
        set = new Set();
        this.listenersByType.set(type, set);
      }
      set.add(onEvent);
    }
    this.refCount += 1;
    this.ensureConnected();

    let disposed = false;
    return () => {
      if (disposed) return;
      disposed = true;
      for (const type of types) {
        this.listenersByType.get(type)?.delete(onEvent);
      }
      this.refCount -= 1;
      if (this.refCount <= 0) this.teardown();
    };
  }

  subscribeStatus(listener: StatusListener): () => void {
    this.statusListeners.add(listener);
    return () => this.statusListeners.delete(listener);
  }

  getSnapshot = (): StreamSnapshot => this.snapshot;

  /** Test seam: drop back to a clean, disconnected, zero-subscriber state. */
  reset(): void {
    if (this.timer) clearTimeout(this.timer);
    this.timer = undefined;
    this.source?.close();
    this.source = null;
    this.attempts = 0;
    this.lastId = null;
    this.refCount = 0;
    this.listenersByType.clear();
    this.statusListeners.clear();
    this.snapshot = SERVER_SNAPSHOT;
  }
}

const manager = new SharedEventsManager();

/**
 * Test seam (mirrors `resetAgentState` in agent-state/store.ts): tear down
 * whatever connection/subscribers a previous test left behind so the next
 * test starts from zero. Production code never calls this — the manager's
 * whole point is to persist for the app's real lifetime.
 */
export function resetSharedEventsForTests(): void {
  manager.reset();
}

/**
 * Subscribe to the shared cross-run stream for exactly the given event
 * types. `types` should be a stable (module-level `as const`) reference —
 * it is only read on mount/unmount, mirroring the pre-t27 hooks' single
 * fixed subscription list; changing its contents at runtime is not a
 * supported pattern (no view needs it today).
 */
export function useSharedEvents(
  types: readonly SharedEventType[],
  onEvent: (event: SharedEvent) => void,
): { status: SharedStreamStatus; lastEventId: string | null } {
  const onEventRef = useRef(onEvent);
  onEventRef.current = onEvent;

  useEffect(() => {
    const forward: Listener = (event) => onEventRef.current(event);
    return manager.subscribe(types, forward);
    // `types` is documented above as a stable reference; re-subscribing on
    // every render would defeat the "never reconnect" contract by thrashing
    // the manager's ref count.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [types]);

  return useSyncExternalStore(
    useCallback((onStoreChange) => manager.subscribeStatus(onStoreChange), []),
    manager.getSnapshot,
    () => SERVER_SNAPSHOT,
  );
}

/**
 * Mounted once in main.tsx wrapping the whole app. Holds one permanent
 * reference on the shared manager for the app's real lifetime, so route
 * transitions — where every view-level `useSharedEvents` subscriber can
 * briefly reach zero between one view's unmount and the next view's mount —
 * never tear the connection down and reopen it. Not required for the
 * "at most one connection" guarantee (the manager enforces that on its
 * own); required only for "subscribe/detach cycles never reconnect".
 */
export function SharedEventsProvider({ children }: { children: ReactNode }) {
  useEffect(() => manager.subscribe([], () => {}), []);
  return <>{children}</>;
}
