import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import Decisions from "./Decisions";
import { ApiError, getWhoami, listPendingDecisions } from "../api/client";
import {
  CLAIM_LEDGER_VERSION,
  CLAIM_RUN_ID,
  COMMIT_RESULT,
  PENDING_DECISIONS,
  PENDING_RUN,
  REVIEW_REQUEST,
} from "../fixtures/pending-decisions-fixture";
import {
  WHOAMI_ACTOR_ID,
  WHOAMI_BOUND,
  WHOAMI_EMAIL,
  WHOAMI_UNBOUND,
} from "../fixtures/whoami-fixture";
import { resetAgentState } from "../agent-state/store";
import { resetWhoamiForTests } from "../hooks/useWhoami";

/**
 * The Inbox test's stub pattern: the READS (the list, whoami) are
 * module-mocked, and the two mutating calls are NOT — they run the real client
 * helpers against a stubbed global `fetch`, because the acceptance for t30 is
 * what the browser actually sends (the bodies — and, since task t9, that no
 * Authorization header goes with them).
 */
vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return { ...actual, listPendingDecisions: vi.fn(), getWhoami: vi.fn() };
});

const mockListPendingDecisions = vi.mocked(listPendingDecisions);
const mockGetWhoami = vi.mocked(getWhoami);

function headerNames(init: { headers: Record<string, string> }): string[] {
  return Object.keys(init.headers).map((key) => key.toLowerCase());
}

function renderDecisions() {
  return render(
    <MemoryRouter initialEntries={["/decisions"]}>
      <Decisions />
    </MemoryRouter>,
  );
}

/** Both POSTs answered in order: create review, then commit it. */
function stubDecisionFetch() {
  const fetchMock = vi
    .fn()
    .mockResolvedValueOnce(
      new Response(JSON.stringify(REVIEW_REQUEST), {
        status: 201,
        headers: { "content-type": "application/json" },
      }),
    )
    .mockResolvedValueOnce(
      new Response(JSON.stringify(COMMIT_RESULT), {
        status: 200,
        headers: { "content-type": "application/json" },
      }),
    );
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

/** One record's verdict radio, by the label a decider reads. */
function verdictRadio(card: HTMLElement, recordId: string, name: string) {
  return within(
    within(card).getByRole("group", { name: `Verdict for ${recordId}` }),
  ).getByRole("radio", { name });
}

async function findRunCard(runId = CLAIM_RUN_ID) {
  await screen.findByText(runId);
  return document.querySelector(`[data-run-id="${runId}"]`) as HTMLElement;
}

beforeEach(() => {
  mockListPendingDecisions.mockReset();
  mockGetWhoami.mockReset();
  mockGetWhoami.mockResolvedValue(WHOAMI_BOUND);
  resetWhoamiForTests();
  resetAgentState();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("Decisions loading, empty and error states", () => {
  it("shows a loading state before the list resolves", () => {
    mockListPendingDecisions.mockReturnValue(new Promise(() => {}));
    renderDecisions();
    expect(screen.getByText("Loading pending decisions…")).toBeInTheDocument();
  });

  it("says so plainly when nothing is awaiting a decision", async () => {
    mockListPendingDecisions.mockResolvedValue({ items: [], record_count: 0 });
    renderDecisions();
    expect(
      await screen.findByText(/Nothing is awaiting a decision\./),
    ).toBeInTheDocument();
  });

  it("renders the error contract when the list request fails", async () => {
    mockListPendingDecisions.mockRejectedValue(
      new ApiError(0, "cannot reach the control plane", "start `nodes serve`"),
    );
    renderDecisions();
    await screen.findByText("error:", { exact: false });
    expect(
      screen.getByText("cannot reach the control plane", { exact: false }),
    ).toBeInTheDocument();
  });
});

describe("Decisions rendering", () => {
  beforeEach(() => {
    mockListPendingDecisions.mockResolvedValue(PENDING_DECISIONS);
  });

  it("groups undecided records by run and shows the ledger version to review at", async () => {
    renderDecisions();
    const card = await findRunCard();
    expect(within(card).getByText(/ledger version 7/)).toBeInTheDocument();
    for (const record of PENDING_RUN.records) {
      expect(within(card).getByText(record.id)).toBeInTheDocument();
    }
  });

  it("renders the claim payload in full, including the qualifying half", async () => {
    renderDecisions();
    const card = await findRunCard();
    // The claim's own words — a decider who cannot read the qualification
    // ("I could not run the suite locally") is being asked to rubber-stamp.
    expect(
      within(card).getByText(/could not run the suite locally/),
    ).toBeInTheDocument();
  });

  it("disables the decision until a rationale is present; the reviewer is the signed-in actor, never typed", async () => {
    const user = userEvent.setup();
    renderDecisions();
    const card = await findRunCard();
    const submit = within(card).getByRole("button", {
      name: "Record decision",
    });

    expect(submit).toBeDisabled(); // a decision with no stated reason stays refused
    expect(within(card).queryByLabelText(/reviewer/i)).toBeNull();
    expect(screen.queryByLabelText(/token/i)).toBeNull();
    expect(screen.getByText(/reviewing as/i)).toHaveTextContent(WHOAMI_EMAIL);

    await user.type(
      within(card).getByLabelText(/Why \(recorded on the decision\)/),
      "re-ran the suite on spark",
    );
    expect(submit).toBeEnabled();
  });

  it("keeps the decision disabled for an unbound login even with a rationale", async () => {
    mockGetWhoami.mockResolvedValue(WHOAMI_UNBOUND);
    const user = userEvent.setup();
    renderDecisions();
    const card = await findRunCard();
    await user.type(
      within(card).getByLabelText(/Why \(recorded on the decision\)/),
      "re-ran the suite on spark",
    );
    expect(
      within(card).getByRole("button", { name: "Record decision" }),
    ).toBeDisabled();
  });
});

describe("Decisions submission", () => {
  beforeEach(() => {
    mockListPendingDecisions.mockResolvedValue(PENDING_DECISIONS);
  });

  it("creates a review then commits it, neither carrying an Authorization header, naming the signed-in reviewer and the version this page read", async () => {
    const user = userEvent.setup();
    const fetchMock = stubDecisionFetch();
    renderDecisions();
    const card = await findRunCard();

    await user.type(
      within(card).getByLabelText(/Why \(recorded on the decision\)/),
      "re-ran the suite on spark and read the output",
    );
    await user.click(
      within(card).getByRole("button", { name: "Record decision" }),
    );

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));

    const [createUrl, createInit] = fetchMock.mock.calls[0];
    expect(createUrl).toBe(`/v1alpha1/runs/${CLAIM_RUN_ID}/reviews`);
    expect(headerNames(createInit)).not.toContain("authorization");
    expect(JSON.parse(createInit.body)).toEqual({
      record_ids: PENDING_RUN.records.map((record) => record.id),
      ledger_version: CLAIM_LEDGER_VERSION,
      reviewer_actor_id: WHOAMI_ACTOR_ID,
    });

    const [commitUrl, commitInit] = fetchMock.mock.calls[1];
    expect(commitUrl).toBe(`/v1alpha1/reviews/${REVIEW_REQUEST.id}/commit`);
    expect(headerNames(commitInit)).not.toContain("authorization");
    expect(JSON.parse(commitInit.body)).toEqual({
      decisions: Object.fromEntries(
        PENDING_RUN.records.map((record) => [record.id, "confirm"]),
      ),
      expected_ledger_version: CLAIM_LEDGER_VERSION,
      rationale: "re-ran the suite on spark and read the output",
    });

    expect(
      await within(card).findByText(/decision recorded/),
    ).toBeInTheDocument();
  });

  it("keeps the confirmation after the decided run leaves the pending list", async () => {
    // Found by driving this view against a live control plane: the card that
    // made the decision is gone on the next refresh (its run is no longer
    // pending), so a confirmation rendered inside it disappears and the click
    // looks like it did nothing. The acknowledgement has to outlive the card.
    const user = userEvent.setup();
    stubDecisionFetch();
    mockListPendingDecisions
      .mockResolvedValueOnce(PENDING_DECISIONS)
      .mockResolvedValue({ items: [], record_count: 0 });

    renderDecisions();
    const card = await findRunCard();
    await user.type(
      within(card).getByLabelText(/Why \(recorded on the decision\)/),
      "read the qualification",
    );
    await user.click(
      within(card).getByRole("button", { name: "Record decision" }),
    );

    // The queue empties...
    expect(
      await screen.findByText(/Nothing is awaiting a decision\./),
    ).toBeInTheDocument();
    // ...and the operator can still see what they just recorded.
    const recorded = screen.getByRole("status");
    expect(recorded).toHaveTextContent(REVIEW_REQUEST.id);
    expect(recorded).toHaveTextContent(CLAIM_RUN_ID);
  });

  // The verdict is per record, which is the grain the commit route decides
  // at: a run whose claim holds up and whose evidence does not is ONE review
  // with two answers. A record left at "not now" is simply not named by it.
  it("sends a verdict per record, and names only the records this review covers", async () => {
    const user = userEvent.setup();
    const fetchMock = stubDecisionFetch();
    renderDecisions();
    const card = await findRunCard();

    await user.click(verdictRadio(card, PENDING_RUN.records[0].id, "reject"));
    await user.click(verdictRadio(card, PENDING_RUN.records[1].id, "not now"));
    await user.type(
      within(card).getByLabelText(/Why \(recorded on the decision\)/),
      "the evidence is process-reported, not measured",
    );
    await user.click(
      within(card).getByRole("button", { name: "Record decision" }),
    );

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(JSON.parse(fetchMock.mock.calls[0][1].body).record_ids).toEqual([
      PENDING_RUN.records[0].id,
    ]);
    expect(JSON.parse(fetchMock.mock.calls[1][1].body).decisions).toEqual({
      [PENDING_RUN.records[0].id]: "reject",
    });
  });

  it("confirms one record and rejects another in a single review", async () => {
    const user = userEvent.setup();
    const fetchMock = stubDecisionFetch();
    renderDecisions();
    const card = await findRunCard();

    await user.click(verdictRadio(card, PENDING_RUN.records[1].id, "reject"));
    await user.type(
      within(card).getByLabelText(/Why \(recorded on the decision\)/),
      "the claim holds; the evidence is process-reported",
    );
    await user.click(
      within(card).getByRole("button", { name: "Record decision" }),
    );

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(JSON.parse(fetchMock.mock.calls[1][1].body).decisions).toEqual({
      [PENDING_RUN.records[0].id]: "confirm",
      [PENDING_RUN.records[1].id]: "reject",
    });
  });

  it("surfaces the API's refusal when the signed-in actor is not a human", async () => {
    const user = userEvent.setup();
    // What the control plane answers when the bound actor is an agent
    // (ledger rule reviewer_must_be_human) — the browser must show it, not
    // swallow it into a generic failure. The reviewer is no longer typed, so
    // the case is a login bound to an agent actor.
    mockGetWhoami.mockResolvedValue({ ...WHOAMI_BOUND, actor_id: "codex-thor" });
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(JSON.stringify(REVIEW_REQUEST), {
          status: 201,
          headers: { "content-type": "application/json" },
        }),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            code: 1,
            message:
              "ledger: authority refused [reviewer_must_be_human]: origin agent",
            remediation:
              "the producer named in origin may not write this record's authority; see PRD §10.4",
          }),
          { status: 400, headers: { "content-type": "application/json" } },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);

    renderDecisions();
    const card = await findRunCard();
    await user.type(
      within(card).getByLabelText(/Why \(recorded on the decision\)/),
      "I am sure of my own work",
    );
    await user.click(
      within(card).getByRole("button", { name: "Record decision" }),
    );

    expect(
      await within(card).findByText(/reviewer_must_be_human/),
    ).toBeInTheDocument();
    expect(within(card).queryByText(/decision recorded/)).toBeNull();
  });
});


describe("Decisions record payload rendering (task t27)", () => {
  it("renders a claim's statement as readable text, not escaped JSON", async () => {
    mockListPendingDecisions.mockResolvedValue(PENDING_DECISIONS);
    renderDecisions();
    const card = await findRunCard();

    const claim = PENDING_RUN.records[0];
    const statement = (claim.data as { statement: string }).statement;
    const rendered = within(card).getByText(statement);
    expect(rendered).toHaveClass("decisions-record__statement");

    // The old rendering put the statement inside JSON.stringify's output,
    // where a multi-line paragraph shows as a quoted string with literal \n
    // — the one field a decider must read was the one field they could not.
    const payload = card.querySelector(".decisions-record__data")!;
    expect(payload.textContent).not.toContain(statement);
  });

  it("keeps every non-statement field as the exact JSON payload", async () => {
    mockListPendingDecisions.mockResolvedValue(PENDING_DECISIONS);
    renderDecisions();
    const card = await findRunCard();

    expect(card.textContent).toContain("completion");
    // The evidence record has no statement at all, so its payload renders
    // whole, unchanged.
    expect(card.textContent).toContain("process_reported");
    expect(card.textContent).toContain("go test ./...");
  });

  it("preserves newlines in a multi-line statement", async () => {
    const multiline = "Line one.\n\nLine two, after a blank line.";
    mockListPendingDecisions.mockResolvedValue({
      ...PENDING_DECISIONS,
      items: [
        {
          ...PENDING_RUN,
          records: [
            {
              ...PENDING_RUN.records[0],
              data: { kind: "completion", statement: multiline },
            },
          ],
        },
      ],
    });
    renderDecisions();
    const card = await findRunCard();

    const rendered = card.querySelector(".decisions-record__statement")!;
    expect(rendered.textContent).toBe(multiline);
  });

  it("names the record in each verdict group's accessible label", async () => {
    mockListPendingDecisions.mockResolvedValue(PENDING_DECISIONS);
    renderDecisions();
    const card = await findRunCard();

    for (const record of PENDING_RUN.records) {
      const group = within(card).getByRole("group", {
        name: `Verdict for ${record.id}`,
      });
      expect(
        within(group).getAllByRole("radio").map((radio) => radio.getAttribute("value")),
      ).toEqual(["confirm", "reject", "skip"]);
    }
  });

  // A review names records; it never rewrites them (PRD §10.8). The card says
  // so on the record itself once its review has committed.
  it("leaves a decided record reading proposed, with the review beside it", async () => {
    const user = userEvent.setup();
    stubDecisionFetch();
    mockListPendingDecisions.mockResolvedValue(PENDING_DECISIONS);
    renderDecisions();
    const card = await findRunCard();
    await user.type(
      within(card).getByLabelText(/Why \(recorded on the decision\)/),
      "read the qualification",
    );
    await user.click(
      within(card).getByRole("button", { name: "Record decision" }),
    );

    await within(card).findByText(/decision recorded/);
    const row = card.querySelector(
      `[data-record-id="${PENDING_RUN.records[0].id}"]`,
    ) as HTMLElement;
    expect(row.querySelector('[data-authority="proposed"]')).not.toBeNull();
    expect(row.querySelector('[data-authority="confirmed"]')).not.toBeNull();
  });
});
