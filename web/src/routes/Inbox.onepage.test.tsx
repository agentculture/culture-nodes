import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import Inbox from "./Inbox";
import {
  getLedger,
  getWhoami,
  listHumanTasks,
  listPendingDecisions,
  listReviewedRecords,
} from "../api/client";
import type { HumanTask } from "../api/types";
import {
  DECIDED_TASK,
  DECISION_RESULT,
  INBOX_NOW,
  LEDGER_VERSION,
  PENDING_TASK,
  WAITING_TASK,
} from "../fixtures/human-tasks-fixture";
import {
  NOTED_DECIDED_TASK,
  REPORT_TASK,
  REVIEWED_RECORD,
} from "../fixtures/one-place-to-decide-fixture";
import { PENDING_DECISIONS } from "../fixtures/pending-decisions-fixture";
import { WHOAMI_ACTOR_ID, WHOAMI_BOUND } from "../fixtures/whoami-fixture";
import { resetAgentState } from "../agent-state/store";
import { resetWhoamiForTests } from "../hooks/useWhoami";

/**
 * Task t46 (owner decision d19): ONE place to decide. Human tasks and
 * proposed ledger records share one page with four tabs; every task
 * decision takes an optional note; an agent report that arrives as a JSON
 * string reads as a report; and t47's blocked-agent outcomes render with
 * readable labels. The decision POST runs the real client against a stubbed
 * `fetch`, as in Inbox.test.tsx — the body sent is the acceptance.
 */
vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    listHumanTasks: vi.fn(),
    listPendingDecisions: vi.fn(),
    listReviewedRecords: vi.fn(),
    getLedger: vi.fn(),
    getWhoami: vi.fn(),
  };
});

const mockListHumanTasks = vi.mocked(listHumanTasks);
const mockListPendingDecisions = vi.mocked(listPendingDecisions);
const mockListReviewedRecords = vi.mocked(listReviewedRecords);
const mockGetLedger = vi.mocked(getLedger);
const mockGetWhoami = vi.mocked(getWhoami);

function resolveTasks(pending: HumanTask[], decided: HumanTask[] = []) {
  mockListHumanTasks.mockImplementation(async (_signal, params) =>
    params?.status === "pending"
      ? { items: pending }
      : params?.status === "decided"
        ? { items: decided }
        : { items: [] },
  );
}

function renderInbox(entry = "/inbox") {
  return render(
    <MemoryRouter initialEntries={[entry]}>
      <Inbox now={INBOX_NOW} />
    </MemoryRouter>,
  );
}

function stubDecisionFetch(body: unknown = DECISION_RESULT) {
  const fetchMock = vi.fn().mockResolvedValue(
    new Response(JSON.stringify(body), {
      status: 200,
      headers: { "content-type": "application/json" },
    }),
  );
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

const tabButton = (name: string) =>
  screen.getByRole("button", { name: new RegExp(`^${name}`) });
const countOf = (name: string) =>
  tabButton(name).querySelector(".inbox-tab__count")?.textContent;
const cardFor = (id: string) =>
  document.querySelector(`[data-human-task-id="${id}"]`) as HTMLElement;

beforeEach(() => {
  mockListHumanTasks.mockReset();
  mockListPendingDecisions.mockReset();
  mockListPendingDecisions.mockResolvedValue({ items: [], record_count: 0 });
  mockListReviewedRecords.mockReset();
  mockListReviewedRecords.mockResolvedValue({ items: [] });
  mockGetLedger.mockReset();
  mockGetLedger.mockResolvedValue({ items: [], ledger_version: LEDGER_VERSION });
  mockGetWhoami.mockReset();
  mockGetWhoami.mockResolvedValue(WHOAMI_BOUND);
  resetWhoamiForTests();
  resetAgentState();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("one place to decide: tabs (task t46)", () => {
  it("has four tabs, To act first, with a count on each — To review counts proposed records", async () => {
    resolveTasks([PENDING_TASK, WAITING_TASK], [DECIDED_TASK]);
    mockListPendingDecisions.mockResolvedValue(PENDING_DECISIONS);
    mockListReviewedRecords.mockResolvedValue({ items: [REVIEWED_RECORD] });
    renderInbox();
    await screen.findByText(PENDING_TASK.id);

    const names = Array.from(
      document.querySelectorAll("#inbox-tabs button"),
      (button) => button.id,
    );
    expect(names).toEqual([
      "inbox-tab-act",
      "inbox-tab-review",
      "inbox-tab-waiting",
      "inbox-tab-decided",
    ]);
    expect(tabButton("To act")).toHaveAttribute("aria-pressed", "true");
    expect(countOf("To act")).toBe("1");
    expect(countOf("To review")).toBe(String(PENDING_DECISIONS.record_count));
    expect(countOf("Waiting")).toBe("1");
    // One decided task and one committed review.
    expect(countOf("Decided")).toBe("2");
  });

  it("keeps t44's ?tab=open deep link on To act, and opens To review from ?tab=review", async () => {
    resolveTasks([PENDING_TASK]);
    mockListPendingDecisions.mockResolvedValue(PENDING_DECISIONS);
    const { unmount } = renderInbox("/inbox?tab=open");
    await screen.findByText(PENDING_TASK.id);
    expect(tabButton("To act")).toHaveAttribute("aria-pressed", "true");
    unmount();

    renderInbox("/inbox?tab=review");
    await screen.findByText(PENDING_DECISIONS.items[0].run_id);
    expect(tabButton("To review")).toHaveAttribute("aria-pressed", "true");
    expect(screen.queryByText(PENDING_TASK.id)).toBeNull();
    // The review form is the Decisions page's, rationale and all.
    expect(screen.getAllByLabelText(/Why \(recorded on the decision\)/)).toHaveLength(
      PENDING_DECISIONS.items.length,
    );
  });

  it("shows the review half's error without blanking the tasks", async () => {
    resolveTasks([PENDING_TASK]);
    mockListReviewedRecords.mockRejectedValue(new Error("404 reviewed-records"));
    renderInbox();
    expect(await screen.findByText(PENDING_TASK.id)).toBeInTheDocument();
    expect(await screen.findByText(/404 reviewed-records/)).toBeInTheDocument();
  });
});

describe("one place to decide: optional note on every task decision (task t46)", () => {
  it("posts the trimmed note with the decision", async () => {
    resolveTasks([PENDING_TASK]);
    const fetchMock = stubDecisionFetch({ ...DECISION_RESULT, note: "checked the diff" });
    const user = userEvent.setup();
    renderInbox();
    await screen.findByText(PENDING_TASK.id);
    const card = cardFor(PENDING_TASK.id);
    const approve = within(card).getByRole("button", { name: "approved" });
    await waitFor(() => expect(approve).toBeEnabled());

    await user.type(within(card).getByLabelText(/^Note \(optional/), "  checked the diff  ");
    await user.click(approve);

    await screen.findByText(/decision recorded/i);
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(JSON.parse(init.body as string)).toEqual({
      outcome: "approved",
      decider_actor_id: WHOAMI_ACTOR_ID,
      response: { outcome: "approved" },
      expected_ledger_version: LEDGER_VERSION,
      note: "checked the diff",
    });
    expect(screen.getByText(/with your note/)).toBeInTheDocument();
  });

  it("sends no note key when none was written", async () => {
    resolveTasks([PENDING_TASK]);
    const fetchMock = stubDecisionFetch();
    const user = userEvent.setup();
    renderInbox();
    await screen.findByText(PENDING_TASK.id);
    const card = cardFor(PENDING_TASK.id);
    await user.type(within(card).getByLabelText(/^Note \(optional/), "   ");
    const approve = within(card).getByRole("button", { name: "approved" });
    await waitFor(() => expect(approve).toBeEnabled());
    await user.click(approve);

    await screen.findByText(/decision recorded/i);
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(JSON.parse(init.body as string)).not.toHaveProperty("note");
  });

  it("shows a decided task's note, and committed reviews with their rationale, newest first", async () => {
    resolveTasks([], [DECIDED_TASK, NOTED_DECIDED_TASK]);
    mockListReviewedRecords.mockResolvedValue({ items: [REVIEWED_RECORD] });
    renderInbox("/inbox?tab=decided");
    await screen.findByText(NOTED_DECIDED_TASK.id);

    const list = document.getElementById("inbox-decided") as HTMLElement;
    const order = Array.from(list.children, (card) =>
      card.getAttribute("data-human-task-id") ?? card.getAttribute("data-reviewed-record-id"),
    );
    expect(order).toEqual([
      REVIEWED_RECORD.review_record_id,
      NOTED_DECIDED_TASK.id,
      DECIDED_TASK.id,
    ]);

    const noted = within(cardFor(NOTED_DECIDED_TASK.id));
    expect(noted.getByText(NOTED_DECIDED_TASK.note!)).toHaveClass("inbox-card__note");
    expect(noted.getByText("approved")).toBeInTheDocument();

    const review = list.querySelector(
      `[data-reviewed-record-id="${REVIEWED_RECORD.review_record_id}"]`,
    ) as HTMLElement;
    expect(within(review).getByText(REVIEWED_RECORD.rationale!)).toBeInTheDocument();
    expect(within(review).getByText("reject")).toBeInTheDocument();
    expect(review.querySelector('[data-authority="rejected"]')).not.toBeNull();
    // The decided record rendered as the review form rendered it: prose.
    const statement = (REVIEWED_RECORD.record.data as { statement: string }).statement;
    expect(within(review).getByText(statement)).toHaveClass("decisions-record__statement");
    expect(within(list).queryAllByRole("button")).toHaveLength(0);
  });
});

describe("one place to decide: readable agent report (task t46)", () => {
  it("reads a JSON-string agent_report key by key and keeps the workspace under the audit", async () => {
    resolveTasks([REPORT_TASK]);
    renderInbox();
    await screen.findByText(REPORT_TASK.id);
    const card = cardFor(REPORT_TASK.id);
    const scoped = within(card);
    const audit = card.querySelector("details.inbox-card__audit-details") as HTMLElement;
    const outsideAudit = (text: string | RegExp) =>
      scoped.getAllByText(text).filter((node) => !audit.contains(node));

    expect(
      outsideAudit("No usable GitHub credential was granted to the worker."),
    ).toHaveLength(1);
    expect(outsideAudit(/Refresh\/reissue the GITHUB_TOKEN_WORKER PAT/)).toHaveLength(1);
    expect(outsideAudit(/I could not edit the PR\./)).toHaveLength(1);
    expect(outsideAudit("gh auth status: not logged in")).toHaveLength(1);
    expect(outsideAudit("none — the PR was not edited")).toHaveLength(1);
    expect(outsideAudit("Jira: SCRUM-16")).toHaveLength(1);
    const prLink = scoped.getByRole("link", { name: `agentculture/culture-nodes#${329}` });
    expect(prLink).toHaveAttribute("href", "https://github.com/agentculture/culture-nodes/pull/329");

    // The workspace measurement and an unknown key: in the audit, nowhere else.
    for (const key of ["workspace_measured", "new_future_key"]) {
      const hits = scoped.getAllByText(new RegExp(key));
      for (const hit of hits) expect(audit.contains(hit)).toBe(true);
    }
    expect(within(audit).getByText(/files_changed/)).toBeInTheDocument();
    // Never raw JSON as the main content.
    const main = Array.from(card.querySelectorAll("pre, dd, p")).filter(
      (node) => !audit.contains(node),
    );
    for (const node of main) expect(node.textContent).not.toContain('{"');
  });
});

describe("one place to decide: blocked-agent outcomes (task t46, for t47)", () => {
  it("renders retry / abandon / acknowledged as Retry, Abandon, Acknowledge with a hint each, posting the raw outcome", async () => {
    resolveTasks([REPORT_TASK]);
    const fetchMock = stubDecisionFetch({ ...DECISION_RESULT, outcome: "retry" });
    const user = userEvent.setup();
    renderInbox();
    await screen.findByText(REPORT_TASK.id);
    const card = cardFor(REPORT_TASK.id);
    const outcomes = within(
      card.querySelector(".inbox-card__outcomes") as HTMLElement,
    );

    expect(outcomes.getAllByRole("button").map((b) => b.textContent)).toEqual([
      "Retry",
      "Abandon",
      "Acknowledge",
    ]);
    expect(outcomes.getByRole("button", { name: "Retry" })).toHaveAccessibleDescription(
      /try again/,
    );
    expect(outcomes.getByRole("button", { name: "Abandon" })).toHaveAccessibleDescription(
      /not tried again/,
    );
    expect(
      outcomes.getByRole("button", { name: "Acknowledge" }),
    ).toHaveAccessibleDescription(/read this/);

    const retry = outcomes.getByRole("button", { name: "Retry" });
    await waitFor(() => expect(retry).toBeEnabled());
    await user.click(retry);
    await screen.findByText(/decision recorded/i);
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(JSON.parse(init.body as string).outcome).toBe("retry");
  });

  it("still renders an outcome it has no label for, under its own name", async () => {
    resolveTasks([
      { ...REPORT_TASK, allowed_outcomes: ["escalate", "retry"] },
    ]);
    renderInbox();
    await screen.findByText(REPORT_TASK.id);
    const outcomes = within(
      cardFor(REPORT_TASK.id).querySelector(".inbox-card__outcomes") as HTMLElement,
    );
    expect(outcomes.getAllByRole("button").map((b) => b.textContent)).toEqual([
      "escalate",
      "Retry",
    ]);
    expect(outcomes.getByRole("button", { name: "escalate" })).not.toHaveAttribute(
      "aria-describedby",
    );
  });
});
