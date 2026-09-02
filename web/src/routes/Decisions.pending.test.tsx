import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import Decisions from "./Decisions";
import { getLedger, getRun, getWhoami, listHumanTasks, listPendingDecisions } from "../api/client";
import type { HumanTask } from "../api/types";
import { WHOAMI_ACTOR_ID, WHOAMI_BOUND, WHOAMI_UNBOUND } from "../fixtures/whoami-fixture";
import { resetWhoamiForTests } from "../hooks/useWhoami";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    getLedger: vi.fn(),
    getRun: vi.fn(),
    getWhoami: vi.fn(),
    listHumanTasks: vi.fn(),
    listPendingDecisions: vi.fn(),
  };
});

const mockListHumanTasks = vi.mocked(listHumanTasks);
const mockListPendingDecisions = vi.mocked(listPendingDecisions);
const mockGetLedger = vi.mocked(getLedger);
const mockGetRun = vi.mocked(getRun);
const mockGetWhoami = vi.mocked(getWhoami);

function task(index: number, overrides: Partial<HumanTask> = {}): HumanTask {
  return {
    id: `task-${String(index).padStart(3, "0")}`,
    run_id: `run-${Math.floor(index / 3)}`,
    kind: "approval",
    status: "pending",
    request: {
      allowed_outcomes: ["approved", "expired", "rejected"],
      decision_schema_ref: "schema://pr-upkeep/merge-decision/v1",
    },
    created_at: "2026-08-30T10:00:00Z",
    ...overrides,
  };
}

async function renderPending() {
  render(
    <MemoryRouter initialEntries={["/decisions"]}>
      <Decisions />
    </MemoryRouter>,
  );
  await userEvent.click(screen.getByRole("button", { name: "Pending" }));
}

beforeEach(() => {
  resetWhoamiForTests();
  mockGetWhoami.mockReset();
  mockGetWhoami.mockResolvedValue(WHOAMI_BOUND);
  mockListHumanTasks.mockReset();
  mockListPendingDecisions.mockReset();
  mockListPendingDecisions.mockResolvedValue({ items: [], record_count: 0 });
  mockGetLedger.mockResolvedValue({ items: [], ledger_version: 12 });
  mockGetRun.mockResolvedValue({ run: { id: "run", workflow_digest: "sha256:x", state: "waiting", created_at: "", updated_at: "" }, tokens: [], node_runs: [] });
});

afterEach(() => vi.unstubAllGlobals());

describe("Decisions pending tab", () => {
  it("follows backend cursors so tasks beyond the first backend page are reachable", async () => {
    const first = Array.from({ length: 500 }, (_, index) => task(index));
    mockListHumanTasks
      .mockResolvedValueOnce({ items: first, next_cursor: "page-2" })
      .mockResolvedValueOnce({ items: [task(500)] });
    await renderPending();

    await waitFor(() => expect(mockListHumanTasks).toHaveBeenCalledTimes(2));
    expect(mockListHumanTasks).toHaveBeenNthCalledWith(2, expect.any(AbortSignal), {
      status: "pending", limit: 500, cursor: "page-2",
    });
    for (let page = 0; page < 20; page++) {
      await userEvent.click(screen.getByRole("button", { name: "Next page" }));
    }
    expect(screen.getByText("task-500")).toBeInTheDocument();
  });

  it("paginates a 200-item queue into eight pages of 25", async () => {
    mockListHumanTasks.mockResolvedValue({
      items: Array.from({ length: 200 }, (_, index) => task(index)),
    });
    await renderPending();

    expect(await screen.findByText("Page 1 of 8")).toBeInTheDocument();
    expect(document.querySelectorAll("[data-human-task-id]")).toHaveLength(25);
    expect(screen.getAllByRole("button", { name: "approved" })).toHaveLength(25);

    await userEvent.click(screen.getByRole("button", { name: "Next page" }));
    expect(screen.getByText("Page 2 of 8")).toBeInTheDocument();
    expect(screen.getByText("task-025")).toBeInTheDocument();
    expect(screen.queryByText("task-000")).not.toBeInTheDocument();
  });

  it("posts the matching option as the signed-in actor, with no Authorization header, and removes the item", async () => {
    mockListHumanTasks.mockResolvedValue({ items: [task(0)] });
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ human_task_id: "task-000", outcome: "approved" }), {
        status: 200,
        headers: { "content-type": "application/json" },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const user = userEvent.setup();
    await renderPending();
    await screen.findByText("task-000");
    await waitFor(() => expect(screen.getByRole("button", { name: "approved" })).toBeEnabled());
    await user.click(screen.getByRole("button", { name: "approved" }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/v1alpha1/human-tasks/task-000/decision");
    expect(Object.keys(init.headers).map((key: string) => key.toLowerCase())).not.toContain("authorization");
    expect(JSON.parse(init.body)).toEqual({
      outcome: "approved",
      decider_actor_id: WHOAMI_ACTOR_ID,
      response: { outcome: "approved" },
      expected_ledger_version: 12,
    });
    expect(screen.queryByText("task-000")).not.toBeInTheDocument();
  });

  it("keeps every outcome disabled for an unbound login and lets nothing leave the browser", async () => {
    mockGetWhoami.mockResolvedValue(WHOAMI_UNBOUND);
    mockListHumanTasks.mockResolvedValue({ items: [task(0)] });
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    await renderPending();
    await screen.findByText("task-000");
    await waitFor(() => expect(mockGetWhoami).toHaveBeenCalled());
    const approve = screen.getByRole("button", { name: "approved" });
    expect(approve).toBeDisabled();
    await userEvent.click(approve);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("clamps the page after deciding the only item on the last page", async () => {
    mockListHumanTasks.mockResolvedValue({
      items: Array.from({ length: 26 }, (_, index) => task(index)),
    });
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ human_task_id: "task-025", outcome: "approved" }), {
        status: 200,
        headers: { "content-type": "application/json" },
      }),
    ));
    const user = userEvent.setup();
    await renderPending();
    await screen.findByText("Page 1 of 2");
    await user.click(screen.getByRole("button", { name: "Next page" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "approved" })).toBeEnabled());
    await user.click(screen.getByRole("button", { name: "approved" }));

    expect(await screen.findByText("Page 1 of 1")).toBeInTheDocument();
    expect(screen.queryByText("Page 2 of 1")).not.toBeInTheDocument();
    expect(screen.getByText("task-000")).toBeInTheDocument();
  });

  // `expired` is declared on every approval task (the compiler implies it)
  // and is never a button: it is the outcome the control plane records when
  // it reads a fact, and DecideHumanTask refuses it from a decider (#265).
  it.each([
    ["approval", ["approved", "expired", "rejected"], ["approved", "rejected"]],
    ["trigger_remint_exhausted", [], []],
    ["approval", ["expired"], []],
  ])("renders only selectable outcomes for %s", async (kind, outcomes, selectable) => {
    mockListHumanTasks.mockResolvedValue({
      items: [task(0, { kind, request: { allowed_outcomes: outcomes } })],
    });
    await renderPending();
    const card = await screen.findByTestId("pending-task-task-000");
    const buttons = within(card).queryAllByRole("button");
    expect(buttons.map((button) => button.textContent)).toEqual(selectable);
    if (outcomes.length === 0) {
      expect(within(card).getByText("needs an outcome set")).toBeInTheDocument();
    } else if (selectable.length === 0) {
      expect(within(card).getByText("no outcome a person may select")).toBeInTheDocument();
    }
  });
});


describe("Decisions pending tab record rendering (task t27, task t12)", () => {
  it("shows the claim's statement as prose and sends the decider to the ticket page", async () => {
    const statement = "Ran the suite twice.\nBoth greens are CI's, not mine.";
    mockListHumanTasks.mockResolvedValue({ items: [] });
    mockListPendingDecisions.mockResolvedValue({
      record_count: 1,
      items: [
        {
          run_id: "run-t27",
          ledger_version: 9,
          records: [
            {
              id: "rec-t27-0001",
              record_type: "claim",
              origin_kind: "agent",
              origin_actor_id: "codex-orin",
              created_at: "2026-08-30T10:00:00Z",
              data: { kind: "completion", statement },
            },
          ],
        },
      ],
    });
    mockGetRun.mockResolvedValue({
      run: {
        id: "run-t27",
        workflow_digest: "sha256:x",
        state: "waiting",
        created_at: "",
        updated_at: "",
        input: { ticket_key: "SCRUM-42" },
      },
      tokens: [],
      node_runs: [],
    });
    await renderPending();

    await screen.findByText("rec-t27-0001");
    const prose = document.querySelector(".decisions-record__statement")!;
    expect(prose.textContent).toBe(statement);

    // Task t12: the inert checkbox — one that selected a record into a verdict
    // no form on this tab could submit — is gone, and the group links to the
    // page that CAN take the decision (decision c33: replaced, not retired).
    expect(screen.queryByRole("checkbox")).toBeNull();
    expect(
      await screen.findByRole("link", {
        name: "Decide these claims on ticket SCRUM-42",
      }),
    ).toHaveAttribute("href", "/tickets/SCRUM-42");
  });

  it("says plainly when a claim's run names no ticket, instead of linking nowhere", async () => {
    mockListHumanTasks.mockResolvedValue({ items: [] });
    mockListPendingDecisions.mockResolvedValue({
      record_count: 1,
      items: [
        {
          run_id: "run-no-ticket",
          ledger_version: 3,
          records: [
            {
              id: "rec-no-ticket-1",
              record_type: "claim",
              origin_kind: "agent",
              origin_actor_id: "codex-orin",
              created_at: "2026-08-30T10:00:00Z",
              data: { kind: "completion", statement: "Nothing to change." },
            },
          ],
        },
      ],
    });
    await renderPending();

    await screen.findByText("rec-no-ticket-1");
    expect(
      screen.getByText(/No ticket is recorded for this run/),
    ).toBeInTheDocument();
    expect(screen.queryByRole("checkbox")).toBeNull();
  });
});
