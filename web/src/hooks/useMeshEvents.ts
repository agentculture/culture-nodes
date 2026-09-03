import type { EventEnvelope } from "../api/types";
import {
  SHARED_EVENT_TYPES,
  type SharedStreamStatus,
} from "./useSharedEvents";
import { useSnapshotReconcile } from "./useSnapshotReconcile";

/**
 * Everything the cross-run stream can name in its `event:` field. Kept as
 * an alias of the shared connection's fixed vocabulary (useSharedEvents.ts)
 * — this hook used to own that list and drive its own EventSource; task t27
 * moved the connection itself to one app-wide manager (c48/h41) that Mesh's
 * hook, and every other cross-run consumer, now subscribes to instead of
 * opening a second connection.
 */
export const MESH_EVENT_TYPES = SHARED_EVENT_TYPES;

/**
 * The mesh stream's honest connection vocabulary: `live` only while the
 * shared EventSource is actually open; anything else is `reconnecting` —
 * the view never fakes liveness (t18 acceptance #2).
 */
export type MeshStreamStatus = SharedStreamStatus;

export interface MeshEvent {
  /** The SSE `id:` field — the events table's ULID, the resume cursor. */
  id: string;
  envelope: EventEnvelope;
}

export interface MeshEventsResult {
  status: MeshStreamStatus;
  lastEventId: string | null;
  snapshotId: string | null;
  resolveSnapshot: () => void;
}

/**
 * Subscribe to the app-wide shared cross-run stream (task t27) for the
 * Mesh view's event vocabulary, delivering each frame to `onEvent` as it
 * arrives instead of accumulating an ever-growing array (this stream never
 * terminates; a mesh view holds counters and a bounded particle pool, not
 * history).
 *
 * External contract unchanged from pre-t27: same exports, same shape,
 * same status/lastEventId semantics — routes/Mesh.tsx needs no changes.
 * What changed underneath is the connection itself: this hook no longer
 * owns an EventSource — useSharedEvents.ts's module-level manager does,
 * shared with every other view in the app (c48/h41).
 */
export function useMeshEvents(
  onEvent: (event: MeshEvent) => void,
): MeshEventsResult {
  return useSnapshotReconcile(MESH_EVENT_TYPES, onEvent);
}
