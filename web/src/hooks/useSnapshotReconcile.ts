import { useCallback, useEffect, useRef, useState } from "react";
import {
  useSharedEvents,
  type SharedEvent,
  type SharedEventType,
} from "./useSharedEvents";

/**
 * Subscribe before a view starts its initial REST read, queueing committed
 * events until that read settles. Event ids are remembered so an SSE retry
 * cannot apply the same committed row twice.
 */
export function useSnapshotReconcile(
  types: readonly SharedEventType[],
  applyEvent: (event: SharedEvent) => void,
) {
  const applyRef = useRef(applyEvent);
  applyRef.current = applyEvent;
  const reconciling = useRef(true);
  const [restResolved, setRestResolved] = useState(false);
  const pending = useRef(new Map<string, SharedEvent>());
  const applied = useRef(new Set<string>());

  const onEvent = useCallback((event: SharedEvent) => {
    if (applied.current.has(event.id) || pending.current.has(event.id)) return;
    if (reconciling.current) {
      pending.current.set(event.id, event);
      return;
    }
    applied.current.add(event.id);
    applyRef.current(event);
  }, []);

  const stream = useSharedEvents(types, onEvent);

  const resolveSnapshot = useCallback(() => {
    setRestResolved(true);
  }, []);

  useEffect(() => {
    if (!restResolved || !reconciling.current) return;
    reconciling.current = false;
    for (const event of pending.current.values()) {
      if (applied.current.has(event.id)) continue;
      applied.current.add(event.id);
      applyRef.current(event);
    }
    pending.current.clear();
  }, [restResolved]);

  return { ...stream, resolveSnapshot };
}
