import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import Decisions from "./Decisions";
import { getLedger, getRun, listHumanTasks, listPendingDecisions } from "../api/client";
import type { HumanTask } from "../api/types";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    getLedger: vi.fn(),
    getRun: vi.fn(),
    listHumanTasks: vi.fn(),
    listPendingDecisions: vi.fn(),
  };
});

const mockListHumanTasks = vi.mocked(listHumanTasks);
const mockListPendingDecisions = vi.mocked(listPendingDecisions);
const mockGetLedger = vi.mocked(getLedger);
const mockGetRun = vi.mocked(getRun);

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
  window.localStorage.clear();
  window.sessionStorage.clear();
  mockListHumanTasks.mockReset();
  mockListPendingDecisions.mockReset();
  mockListPendingDecisions.mockResolvedValue({ items: [], record_count: 0 });
  mockGetLedger.mockResolvedValue({ items: [], ledger_version: 12 });
  mockGetRun.mockResolvedValue({ run: { id: "run", workflow_digest: "sha256:x", state: "waiting", created_at: "", updated_at: "" }, tokens: [], node_runs: [] });
});

afterEach(() => vi.unstubAllGlobals());

describe("Decisions pending tab", () => {
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

  it("posts the matching option under the held token and removes the item", async () => {
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
    await user.type(screen.getByLabelText("Decision token"), "held-secret");
    await user.click(screen.getByRole("button", { name: "Hold token" }));
    await user.type(screen.getByLabelText("Decider actor id"), "actor-human-ori");
    await user.click(screen.getByRole("button", { name: "approved" }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/v1alpha1/human-tasks/task-000/decision");
    expect(init.headers.authorization).toBe("Bearer held-secret");
    expect(JSON.parse(init.body)).toEqual({
      outcome: "approved",
      decider_actor_id: "actor-human-ori",
      response: { outcome: "approved" },
      expected_ledger_version: 12,
    });
    expect(screen.queryByText("task-000")).not.toBeInTheDocument();
    expect(window.localStorage.getItem("nodes.human-decision-actor-id")).toBe("actor-human-ori");
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
    await user.type(screen.getByLabelText("Decision token"), "held-secret");
    await user.click(screen.getByRole("button", { name: "Hold token" }));
    await user.type(screen.getByLabelText("Decider actor id"), "actor-human-ori");
    await user.click(screen.getByRole("button", { name: "approved" }));

    expect(await screen.findByText("Page 1 of 1")).toBeInTheDocument();
    expect(screen.queryByText("Page 2 of 1")).not.toBeInTheDocument();
    expect(screen.getByText("task-000")).toBeInTheDocument();
  });

  it.each([
    ["approval", ["approved", "expired", "rejected"]],
    ["trigger_remint_exhausted", []],
  ])("renders only accepted outcomes for %s", async (kind, outcomes) => {
    mockListHumanTasks.mockResolvedValue({
      items: [task(0, { kind, request: { allowed_outcomes: outcomes } })],
    });
    await renderPending();
    const card = await screen.findByTestId("pending-task-task-000");
    expect(within(card).queryAllByRole("button")).toHaveLength(outcomes.length);
    if (outcomes.length === 0) {
      expect(within(card).getByText("needs an outcome set")).toBeInTheDocument();
    }
  });
});


describe("Decisions pending tab record rendering (task t27)", () => {
  it("labels each claim checkbox with the record it selects and shows the statement as prose", async () => {
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
    await renderPending();

    expect(
      await screen.findByRole("checkbox", {
        name: "include this record in the verdict (rec-t27-0001)",
      }),
    ).toBeInTheDocument();
    const prose = document.querySelector(".decisions-record__statement")!;
    expect(prose.textContent).toBe(statement);
  });
});
