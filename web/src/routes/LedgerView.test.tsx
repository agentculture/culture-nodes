import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import LedgerView from "./LedgerView";
import { ApiError, getLedger, getProjection } from "../api/client";
import { LEDGER, RUN_ID } from "../fixtures/run-fixture";
import { getAgentState, resetAgentState } from "../agent-state/store";
import { resetSharedEventsForTests } from "../hooks/useSharedEvents";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return { ...actual, getLedger: vi.fn(), getProjection: vi.fn() };
});

const mockGetLedger = vi.mocked(getLedger);
const mockGetProjection = vi.mocked(getProjection);

/** A minimal fake of the shared cross-run EventSource (mirrors Mesh.test.tsx). */
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  url: string;
  readyState = 0;
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: string; lastEventId: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  private listeners = new Map<
    string,
    Array<(event: { data: string; lastEventId: string }) => void>
  >();

  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }

  addEventListener(
    type: string,
    listener: (event: { data: string; lastEventId: string }) => void,
  ) {
    const list = this.listeners.get(type) ?? [];
    list.push(listener);
    this.listeners.set(type, list);
  }

  close() {
    this.readyState = 2;
  }

  open() {
    this.readyState = 1;
    this.onopen?.();
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
    const event = { data: JSON.stringify(envelope), lastEventId: id };
    for (const listener of this.listeners.get(type) ?? []) listener(event);
  }
}

function renderLedger(runId: string = RUN_ID) {
  return render(
    <MemoryRouter initialEntries={[`/runs/${runId}/ledger`]}>
      <Routes>
        <Route path="/runs/:id/ledger" element={<LedgerView />} />
      </Routes>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mockGetLedger.mockReset();
  mockGetProjection.mockReset();
  resetAgentState();
});

describe("LedgerView loading/empty/error", () => {
  it("shows the ledger version and record rows once the fetch resolves", async () => {
    mockGetLedger.mockResolvedValue(LEDGER);
    renderLedger();
    await waitFor(() => expect(getAgentState().status).toBe("ready"));
    expect(document.getElementById("ledger-version")).toHaveTextContent("6");
    expect(
      document.querySelectorAll("#ledger-table tbody tr"),
    ).toHaveLength(LEDGER.items.length);
  });

  it("renders an error notice and still marks agent-state ready when the fetch fails", async () => {
    mockGetLedger.mockRejectedValue(
      new ApiError(0, "cannot reach the control plane", "start `nodes serve`"),
    );
    renderLedger();
    await screen.findByText("error:", { exact: false });
    expect(getAgentState().status).toBe("ready");
  });
});

describe("LedgerView auto-refresh (issue #46, task t30)", () => {
  beforeEach(() => {
    resetSharedEventsForTests();
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
  });

  afterEach(() => {
    resetSharedEventsForTests();
    vi.unstubAllGlobals();
  });

  it("refetches on a ledger event for this run, staying stale-while-revalidate: no loading regression, no nulled table", async () => {
    mockGetLedger.mockResolvedValueOnce(LEDGER);
    renderLedger();
    await waitFor(() => expect(getAgentState().status).toBe("ready"));
    expect(document.getElementById("ledger-version")).toHaveTextContent("6");

    const source = FakeEventSource.instances[0];
    act(() => source.open());

    let resolveReload: ((value: typeof LEDGER) => void) | undefined;
    mockGetLedger.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveReload = resolve;
        }),
    );

    act(() => {
      source.emit(
        "dev.culture.nodes.ledger.record-appended",
        { run_id: RUN_ID },
        "01EVT1",
      );
    });

    await waitFor(() => expect(mockGetLedger).toHaveBeenCalledTimes(2));

    // The reload fetch is in flight — the original table and agent-state
    // must still be exactly as they were (stale-while-revalidate).
    expect(
      document.querySelectorAll("#ledger-table tbody tr"),
    ).toHaveLength(LEDGER.items.length);
    expect(getAgentState().status).toBe("ready");

    const updated = { items: LEDGER.items.slice(0, 1), ledger_version: 7 };
    await act(async () => {
      resolveReload?.(updated);
    });

    await waitFor(() =>
      expect(document.getElementById("ledger-version")).toHaveTextContent("7"),
    );
    expect(
      document.querySelectorAll("#ledger-table tbody tr"),
    ).toHaveLength(1);
    expect(getAgentState().status).toBe("ready");
  });

  it("ignores a ledger event for a different run", async () => {
    mockGetLedger.mockResolvedValueOnce(LEDGER);
    renderLedger();
    await waitFor(() => expect(getAgentState().status).toBe("ready"));

    const source = FakeEventSource.instances[0];
    act(() => source.open());
    mockGetLedger.mockClear();

    act(() => {
      source.emit(
        "dev.culture.nodes.ledger.record-appended",
        { run_id: "some-other-run" },
        "01EVT1",
      );
    });
    await new Promise((resolve) => setTimeout(resolve, 20));

    expect(mockGetLedger).not.toHaveBeenCalled();
  });

  it("ignores an event type this view did not subscribe to", async () => {
    mockGetLedger.mockResolvedValueOnce(LEDGER);
    renderLedger();
    await waitFor(() => expect(getAgentState().status).toBe("ready"));

    const source = FakeEventSource.instances[0];
    act(() => source.open());
    mockGetLedger.mockClear();

    act(() => {
      source.emit("dev.culture.nodes.attempt.completed", { run_id: RUN_ID }, "01EVT1");
    });
    await new Promise((resolve) => setTimeout(resolve, 20));

    expect(mockGetLedger).not.toHaveBeenCalled();
  });

  it("also refreshes the active projection alongside the raw ledger on reload", async () => {
    mockGetLedger.mockResolvedValue(LEDGER);
    const projection = {
      kind: "confirmed_claims",
      subject: RUN_ID,
      items: LEDGER.items.filter((record) => record.authority === "confirmed"),
      digest: "sha256:projection-confirmed",
    };
    mockGetProjection.mockResolvedValueOnce(projection);
    const user = userEvent.setup();
    renderLedger();
    await waitFor(() => expect(getAgentState().status).toBe("ready"));

    await user.selectOptions(
      screen.getByLabelText("Projection"),
      "confirmed_claims",
    );
    await waitFor(() =>
      expect(document.getElementById("projection-table")).toBeInTheDocument(),
    );
    expect(mockGetProjection).toHaveBeenCalledTimes(1);

    const source = FakeEventSource.instances[0];
    act(() => source.open());
    mockGetProjection.mockResolvedValueOnce({ ...projection, digest: "sha256:projection-confirmed-2" });

    act(() => {
      source.emit(
        "dev.culture.nodes.ledger.review-committed",
        { run_id: RUN_ID },
        "01EVT1",
      );
    });

    await waitFor(() => expect(mockGetProjection).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect(document.getElementById("projection-digest")).toHaveTextContent(
        "sha256:projection-confir",
      ),
    );
  });
});
