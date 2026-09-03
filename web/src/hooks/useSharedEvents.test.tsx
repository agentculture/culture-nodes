import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render } from "@testing-library/react";
import { useEffect } from "react";
import {
  resetSharedEventsForTests,
  SharedEventsProvider,
  useSharedEvents,
  type SharedEvent,
  type SharedEventType,
  type SharedStreamStatus,
} from "./useSharedEvents";

/**
 * The app-wide shared EventSource manager (task t27, spec claim c48 /
 * honesty condition h41): exactly one connection for the whole app no
 * matter how many views subscribe, and subscribe/detach cycles never
 * reconnect the underlying stream.
 */

const NOOP = () => {};

class FakeEventSource {
  static CONNECTING = 0 as const;
  static OPEN = 1 as const;
  static CLOSED = 2 as const;
  static instances: FakeEventSource[] = [];

  url: string;
  readyState: 0 | 1 | 2 = 0;
  onopen: (() => void) | null = null;
  onmessage:
    | ((event: { data: string; lastEventId: string; type: string }) => void)
    | null = null;
  onerror: (() => void) | null = null;
  closeCalls = 0;
  private listeners = new Map<
    string,
    Array<(event: { data: string; lastEventId: string; type: string }) => void>
  >();

  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }

  addEventListener(
    type: string,
    listener: (event: { data: string; lastEventId: string; type: string }) => void,
  ) {
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

  /** Simulates the browser giving up and closing the connection. */
  fail() {
    this.readyState = FakeEventSource.CLOSED;
    this.onerror?.();
  }

  /**
   * The `?from=latest` boundary marker exactly as internal/api/events.go
   * frames it: native event name `stream.snapshot`, body `{"snapshot_id"}`,
   * and no CloudEvents envelope at all.
   */
  emitSnapshot(id: string) {
    const event = {
      data: JSON.stringify({ snapshot_id: id }),
      lastEventId: id,
      type: "stream.snapshot",
    };
    for (const listener of this.listeners.get("stream.snapshot") ?? []) listener(event);
  }

  emit(type: string, data: Record<string, unknown>, id: string) {
    const envelope = {
      id,
      source: "nodes",
      specversion: "1.0",
      type,
      time: "2026-08-13T00:00:00Z",
      datacontenttype: "application/json",
      data,
    };
    const event = { data: JSON.stringify(envelope), lastEventId: id, type };
    for (const listener of this.listeners.get(type) ?? []) listener(event);
  }
}

beforeEach(() => {
  resetSharedEventsForTests();
  FakeEventSource.instances = [];
  vi.stubGlobal("EventSource", FakeEventSource);
});

afterEach(() => {
  resetSharedEventsForTests();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

/** A minimal subscriber component reporting every status change it sees. */
function Subscriber(props: {
  types: readonly SharedEventType[];
  onEvent?: (event: SharedEvent) => void;
  report: (
    status: SharedStreamStatus,
    lastEventId: string | null,
    snapshotId: string | null,
  ) => void;
}) {
  const { status, lastEventId, snapshotId } = useSharedEvents(
    props.types,
    props.onEvent ?? NOOP,
  );
  useEffect(() => {
    props.report(status, lastEventId, snapshotId);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [status, lastEventId, snapshotId]);
  return null;
}

const RUN_CREATED = "dev.culture.nodes.run.created" as SharedEventType;
const RUN_COMPLETED = "dev.culture.nodes.run.completed" as SharedEventType;

describe("useSharedEvents (task t27, c48/h41)", () => {
  it("opens exactly one EventSource for two simultaneous subscribers", () => {
    render(
      <>
        <Subscriber types={[RUN_CREATED]} report={NOOP} />
        <Subscriber types={[RUN_COMPLETED]} report={NOOP} />
      </>,
    );
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(FakeEventSource.instances[0].url).toBe("/v1alpha1/events?from=latest");
  });

  it("stores the stream.snapshot id in the manager snapshot without delivering it", () => {
    const snapshots: Array<string | null> = [];
    const events: SharedEvent[] = [];
    render(
      <Subscriber
        types={[RUN_CREATED]}
        onEvent={(event) => events.push(event)}
        report={(_status, _lastEventId, snapshotId) => snapshots.push(snapshotId)}
      />,
    );

    act(() => FakeEventSource.instances[0].emitSnapshot("01SNAPSHOT"));

    expect(snapshots.at(-1)).toBe("01SNAPSHOT");
    expect(events).toHaveLength(0);
  });

  it("resumes from the snapshot boundary after the connection closes, never from latest again", () => {
    vi.useFakeTimers();
    try {
      const view = render(<Subscriber types={[RUN_CREATED]} report={NOOP} />);
      act(() => FakeEventSource.instances[0].open());
      act(() => FakeEventSource.instances[0].emitSnapshot("01SNAPSHOT"));
      act(() => FakeEventSource.instances[0].fail());
      act(() => vi.advanceTimersByTime(1000)); // the base reconnect delay

      expect(FakeEventSource.instances).toHaveLength(2);
      expect(FakeEventSource.instances[1].url).toBe("/v1alpha1/events?from=01SNAPSHOT");
      view.unmount();
    } finally {
      vi.useRealTimers();
    }
  });

  it("reports the stream honestly: reconnecting until open, live after", () => {
    const statuses: SharedStreamStatus[] = [];
    render(<Subscriber types={[RUN_CREATED]} report={(s) => statuses.push(s)} />);
    expect(statuses[0]).toBe("reconnecting");

    act(() => FakeEventSource.instances[0].open());
    expect(statuses[statuses.length - 1]).toBe("live");
  });

  it("keeps the same connection alive when a second subscriber joins and the first detaches (never reconnects)", () => {
    const first = render(<Subscriber types={[RUN_CREATED]} report={NOOP} />);
    act(() => FakeEventSource.instances[0].open());
    expect(FakeEventSource.instances).toHaveLength(1);

    // A second view subscribes while the first is still mounted.
    const second = render(<Subscriber types={[RUN_COMPLETED]} report={NOOP} />);
    expect(FakeEventSource.instances).toHaveLength(1);

    // The first view's subscription detaches; the connection must not
    // reconnect because the second subscriber is still holding it open.
    first.unmount();
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(FakeEventSource.instances[0].closeCalls).toBe(0);
    expect(FakeEventSource.instances[0].readyState).toBe(FakeEventSource.OPEN);

    second.unmount();
  });

  it("delivers only the subscribed event type to each subscriber", () => {
    const createdEvents: SharedEvent[] = [];
    const completedEvents: SharedEvent[] = [];
    render(
      <>
        <Subscriber
          types={[RUN_CREATED]}
          onEvent={(e) => createdEvents.push(e)}
          report={NOOP}
        />
        <Subscriber
          types={[RUN_COMPLETED]}
          onEvent={(e) => completedEvents.push(e)}
          report={NOOP}
        />
      </>,
    );
    const source = FakeEventSource.instances[0];
    act(() => {
      source.open();
      source.emit(RUN_CREATED, { run_id: "run-1" }, "01EVENT1");
    });
    expect(createdEvents).toHaveLength(1);
    expect(completedEvents).toHaveLength(0);
    expect(createdEvents[0].id).toBe("01EVENT1");

    act(() => source.emit(RUN_COMPLETED, { run_id: "run-1" }, "01EVENT2"));
    expect(createdEvents).toHaveLength(1);
    expect(completedEvents).toHaveLength(1);
  });

  it("tears the connection down once every subscriber has detached, and opens a fresh one for a later subscriber", () => {
    const view = render(<Subscriber types={[RUN_CREATED]} report={NOOP} />);
    act(() => FakeEventSource.instances[0].open());
    view.unmount();

    expect(FakeEventSource.instances[0].closeCalls).toBe(1);

    render(<Subscriber types={[RUN_CREATED]} report={NOOP} />);
    expect(FakeEventSource.instances).toHaveLength(2);
  });

  it("SharedEventsProvider pins the connection open across a gap where no view subscribes", () => {
    render(<SharedEventsProvider>{null}</SharedEventsProvider>);
    expect(FakeEventSource.instances).toHaveLength(1);
    act(() => FakeEventSource.instances[0].open());

    // A view mounts and fully detaches again while the provider stays put.
    const view = render(<Subscriber types={[RUN_CREATED]} report={NOOP} />);
    view.unmount();

    // Still the same, still-open connection — never reconnected.
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(FakeEventSource.instances[0].closeCalls).toBe(0);
    expect(FakeEventSource.instances[0].readyState).toBe(FakeEventSource.OPEN);
  });

  it("backs off reconnects from 1s, doubling to a 15s cap", () => {
    vi.useFakeTimers();
    try {
      render(<Subscriber types={[RUN_CREATED]} report={NOOP} />);
      expect(FakeEventSource.instances).toHaveLength(1);

      const expectedDelays = [1000, 2000, 4000, 8000, 15000, 15000];
      for (const delay of expectedDelays) {
        const before = FakeEventSource.instances.length;
        act(() => {
          FakeEventSource.instances[FakeEventSource.instances.length - 1].fail();
        });
        expect(FakeEventSource.instances).toHaveLength(before); // not yet
        act(() => {
          vi.advanceTimersByTime(delay - 1);
        });
        expect(FakeEventSource.instances).toHaveLength(before);
        act(() => {
          vi.advanceTimersByTime(1);
        });
        expect(FakeEventSource.instances).toHaveLength(before + 1);
      }
    } finally {
      vi.useRealTimers();
    }
  });

  it("carries the resume cursor forward across a reconnect", () => {
    vi.useFakeTimers();
    try {
      const view = render(<Subscriber types={[RUN_CREATED]} report={NOOP} />);
      const first = FakeEventSource.instances[0];
      act(() => {
        first.open();
        first.emit(RUN_CREATED, { run_id: "run-1" }, "01EVENT9");
      });
      act(() => first.fail());
      act(() => vi.advanceTimersByTime(1000)); // the base reconnect delay
      // Second connection should resume from the last id seen.
      expect(FakeEventSource.instances).toHaveLength(2);
      expect(FakeEventSource.instances[1].url).toContain("from=01EVENT9");
      view.unmount();
    } finally {
      vi.useRealTimers();
    }
  });
});

/**
 * Task t3 (login-from-anywhere cycle): the cross-run stream now writes an
 * SSE comment line every 25 s while idle (internal/api/events.go). A
 * browser's EventSource never dispatches comment frames, so the manager has
 * nothing to filter; these tests pin the two properties the keepalive
 * protects at the manager boundary — a body-less frame is inert, and a
 * proxy dropping an idle connection is recovered from with the resume
 * cursor intact.
 */
describe("useSharedEvents keepalive tolerance and forced drop (task t3)", () => {
  it("drops a body-less frame without touching listeners or the resume cursor", () => {
    const seen: SharedEvent[] = [];
    const statuses: Array<[SharedStreamStatus, string | null]> = [];
    render(
      <Subscriber
        types={[RUN_CREATED]}
        onEvent={(e) => seen.push(e)}
        report={(s, id) => statuses.push([s, id])}
      />,
    );
    const source = FakeEventSource.instances[0];
    act(() => source.open());
    act(() => source.emit(RUN_CREATED, { run_id: "run-1" }, "01EVENT1"));
    expect(seen).toHaveLength(1);
    expect(statuses[statuses.length - 1]).toEqual(["live", "01EVENT1"]);

    // The nearest thing to a comment line the manager could ever be handed.
    act(() => source.onmessage?.({ data: "", lastEventId: "", type: "message" }));
    act(() => source.onmessage?.({ data: ": keepalive", lastEventId: "", type: "message" }));

    expect(seen).toHaveLength(1);
    expect(statuses[statuses.length - 1]).toEqual(["live", "01EVENT1"]);

    act(() => source.emit(RUN_CREATED, { run_id: "run-2" }, "01EVENT2"));
    expect(seen.map((e) => e.id)).toEqual(["01EVENT1", "01EVENT2"]);
  });

  it("reopens a dropped idle connection from the last real event id", () => {
    vi.useFakeTimers();
    try {
      const seen: SharedEvent[] = [];
      const statuses: SharedStreamStatus[] = [];
      render(
        <Subscriber
          types={[RUN_CREATED]}
          onEvent={(e) => seen.push(e)}
          report={(s) => statuses.push(s)}
        />,
      );
      const first = FakeEventSource.instances[0];
      act(() => first.open());
      act(() => first.emit(RUN_CREATED, { run_id: "run-1" }, "01EVENT5"));
      expect(first.url).toContain("from=latest");

      act(() => first.fail());
      expect(statuses[statuses.length - 1]).toBe("reconnecting");
      expect(FakeEventSource.instances).toHaveLength(1); // 1 s backoff first

      act(() => {
        vi.advanceTimersByTime(1000);
      });
      expect(FakeEventSource.instances).toHaveLength(2);
      const second = FakeEventSource.instances[1];
      expect(second.url).toContain("from=01EVENT5");

      act(() => second.open());
      expect(statuses[statuses.length - 1]).toBe("live");
      act(() => second.emit(RUN_CREATED, { run_id: "run-2" }, "01EVENT6"));
      expect(seen.map((e) => e.id)).toEqual(["01EVENT5", "01EVENT6"]);
    } finally {
      vi.useRealTimers();
    }
  });
});
