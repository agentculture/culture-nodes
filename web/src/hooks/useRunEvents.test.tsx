import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { useRunEvents } from "./useRunEvents";

/**
 * Task t3 (login-from-anywhere cycle): the per-run stream now carries an
 * SSE comment line every 25 s while idle (internal/api/events.go). A
 * browser's EventSource never dispatches comment frames at all, so the hook
 * has nothing to filter — what these tests prove is that the two things the
 * keepalive is there to protect actually hold at the hook boundary:
 *
 *   1. a frame with no usable body (the closest thing to a comment the
 *      hook could ever be handed) is dropped without disturbing the events
 *      already collected or the resume cursor; and
 *   2. when a proxy still drops an idle connection, the hook reopens the
 *      stream itself, resuming from the last real event id via `?from=`.
 */

type Frame = { data: string; lastEventId: string; type: string };
type Listener = (event: Frame) => void;

class FakeEventSource {
  static CONNECTING = 0 as const;
  static OPEN = 1 as const;
  static CLOSED = 2 as const;
  static instances: FakeEventSource[] = [];

  url: string;
  readyState: 0 | 1 | 2 = 0;
  onopen: (() => void) | null = null;
  onmessage: Listener | null = null;
  onerror: (() => void) | null = null;
  closeCalls = 0;
  private listeners = new Map<string, Listener[]>();

  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, listener: Listener) {
    const list = this.listeners.get(type) ?? [];
    list.push(listener);
    this.listeners.set(type, list);
  }

  close() {
    this.readyState = FakeEventSource.CLOSED;
    this.closeCalls += 1;
  }

  open() {
    this.readyState = FakeEventSource.OPEN;
    this.onopen?.();
  }

  /** The browser giving up on the connection (a proxy dropped it). */
  fail() {
    this.readyState = FakeEventSource.CLOSED;
    this.onerror?.();
  }

  emit(type: string, data: Record<string, unknown>, id: string) {
    const envelope = {
      id: `evt-${id}`,
      source: "nodes",
      specversion: "1.0",
      type,
      subject: "run-1",
      time: "2026-09-01T00:00:00Z",
      datacontenttype: "application/json",
      data,
    };
    const frame: Frame = { data: JSON.stringify(envelope), lastEventId: id, type };
    for (const listener of this.listeners.get(type) ?? []) listener(frame);
  }
}

const TOKEN_ENTERED = "dev.culture.nodes.token.entered";

function latest(): FakeEventSource {
  return FakeEventSource.instances[FakeEventSource.instances.length - 1];
}

beforeEach(() => {
  FakeEventSource.instances = [];
  vi.stubGlobal("EventSource", FakeEventSource);
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("useRunEvents keepalive tolerance and reconnect (task t3)", () => {
  it("drops a body-less frame without touching collected events or the resume cursor", () => {
    const { result } = renderHook(() => useRunEvents("run-1"));
    const source = latest();
    act(() => source.open());
    act(() => source.emit(TOKEN_ENTERED, { node: "start" }, "1"));
    expect(result.current.events).toHaveLength(1);
    expect(result.current.lastEventId).toBe("1");

    // The nearest thing to a comment line the hook could ever see: a
    // default "message" frame with no parsable body and no id.
    act(() => source.onmessage?.({ data: "", lastEventId: "", type: "message" }));
    act(() => source.onmessage?.({ data: ": keepalive", lastEventId: "", type: "message" }));

    expect(result.current.events).toHaveLength(1);
    expect(result.current.lastEventId).toBe("1");
    expect(result.current.status).toBe("open");

    // And the stream is still fully usable afterwards.
    act(() => source.emit(TOKEN_ENTERED, { node: "next" }, "2"));
    expect(result.current.events).toHaveLength(2);
    expect(result.current.lastEventId).toBe("2");
  });

  it("reopens from the last real event id after the connection is dropped while idle", () => {
    vi.useFakeTimers();
    const { result } = renderHook(() => useRunEvents("run-1"));
    const first = latest();
    act(() => first.open());
    act(() => first.emit(TOKEN_ENTERED, { node: "start" }, "7"));
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(first.url).not.toContain("from=");

    act(() => first.fail());
    expect(result.current.status).toBe("error");
    expect(FakeEventSource.instances).toHaveLength(1); // not yet — 1 s backoff

    act(() => {
      vi.advanceTimersByTime(1000);
    });
    expect(FakeEventSource.instances).toHaveLength(2);
    const second = latest();
    expect(second).not.toBe(first);
    expect(second.url).toContain("/runs/run-1/events?from=7");
    expect(result.current.status).toBe("connecting");

    act(() => second.open());
    expect(result.current.status).toBe("open");
    // Nothing was replayed or lost across the drop.
    expect(result.current.events).toHaveLength(1);
    expect(result.current.lastEventId).toBe("7");

    act(() => second.emit(TOKEN_ENTERED, { node: "after" }, "8"));
    expect(result.current.events.map((e) => e.sequence)).toEqual(["7", "8"]);
  });

  it("leaves a browser-driven retry (readyState CONNECTING) alone rather than double-connecting", () => {
    vi.useFakeTimers();
    renderHook(() => useRunEvents("run-1"));
    const first = latest();
    act(() => first.open());

    first.readyState = FakeEventSource.CONNECTING;
    act(() => first.onerror?.());
    act(() => {
      vi.advanceTimersByTime(5000);
    });
    expect(FakeEventSource.instances).toHaveLength(1);
  });
});
