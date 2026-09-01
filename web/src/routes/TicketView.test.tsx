import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { getTicket, getWhoami } from "../api/client";
import {
  PENDING_TASK_ID,
  SECOND_TICKET_CLAIM_RECORD_ID,
  SECOND_TICKET_RUN_ID,
  SECOND_TICKET_RUN_LEDGER_VERSION,
  STALE_FRAME_TICKET_URL,
  TICKET_CLAIM_RECORD_ID,
  TICKET_EVIDENCE_RECORD_ID,
  TICKET_PROJECTION,
  TICKET_REVIEWS_RESULT,
  TICKET_RUN_ID,
  TICKET_RUN_LEDGER_VERSION,
  TICKET_URL,
} from "../fixtures/ticket-fixture";
import { WHOAMI_ACTOR_ID, WHOAMI_BOUND, WHOAMI_EMAIL, WHOAMI_UNBOUND } from "../fixtures/whoami-fixture";
import { resetWhoamiForTests } from "../hooks/useWhoami";
import TicketView from "./TicketView";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return { ...actual, getTicket: vi.fn(), getWhoami: vi.fn() };
});

const mockGetTicket = vi.mocked(getTicket);
const mockGetWhoami = vi.mocked(getWhoami);

function renderRoute() {
  return render(
    <MemoryRouter initialEntries={[`/tickets/${TICKET_PROJECTION.ticket_id}`]}>
      <Routes><Route path="/tickets/:id" element={<TicketView />} /></Routes>
    </MemoryRouter>,
  );
}

function headerNames(init: RequestInit): string[] {
  return Object.keys(init.headers as Record<string, string>).map((key) => key.toLowerCase());
}

describe("TicketView", () => {
  beforeEach(() => {
    resetWhoamiForTests();
    mockGetWhoami.mockReset();
    mockGetWhoami.mockResolvedValue(WHOAMI_BOUND);
    mockGetTicket.mockResolvedValue(structuredClone(TICKET_PROJECTION));
  });

  it("renders the complete projection, every claim as icon plus word, and the exact Jira back-link", async () => {
    renderRoute();
    expect(await screen.findByRole("heading", { name: TICKET_PROJECTION.ticket_id })).toBeInTheDocument();
    // The API composes the back-link (task t18): the projection's own
    // ticket_url wins over whatever an older posted frame claimed.
    expect(screen.getByRole("link", { name: "Open in Jira" })).toHaveAttribute("href", TICKET_URL);
    expect(screen.getByRole("link", { name: "Open in Jira" })).not.toHaveAttribute("href", STALE_FRAME_TICKET_URL);

    const claims = document.querySelectorAll("[data-claim-id]");
    expect(claims).toHaveLength(3);
    for (const claim of claims) {
      const state = claim.querySelector("[data-state]");
      expect(state).not.toBeNull();
      expect(state?.querySelector("[aria-hidden=true]")).not.toHaveTextContent("");
      expect(within(state as HTMLElement).getByText(state?.getAttribute("data-state") ?? "")).toBeInTheDocument();
    }
    expect(screen.getByText("Questions and decisions")).toBeInTheDocument();
    expect(screen.getByText("Runs and reports")).toBeInTheDocument();
    expect(screen.getByText("Reply thread")).toBeInTheDocument();
    expect(screen.getByText("Yes, use the existing token.")).toBeInTheDocument();
  });

  // Task t9 (spec c8): identity is derived from the signed-in principal.
  // There is no token field and no name field; the reply names the actor
  // whoami bound the login to, and nothing rides in an Authorization header.
  it("offers no token or name field, says who is replying, and posts the reply as the signed-in actor with no Authorization header", async () => {
    const user = userEvent.setup();
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({
      id: "reply-2", replier: WHOAMI_ACTOR_ID, text: "Proceed", created_at: "2026-08-29T10:00:00Z",
    }), { status: 201, headers: { "content-type": "application/json" } }));
    vi.stubGlobal("fetch", fetchMock);
    renderRoute();
    await screen.findByRole("heading", { name: TICKET_PROJECTION.ticket_id });
    expect(screen.queryByLabelText(/token/i)).toBeNull();
    expect(screen.queryByLabelText(/your name/i)).toBeNull();
    expect(document.querySelector('input[type="password"]')).toBeNull();
    expect(await screen.findByText(/replying as/i)).toHaveTextContent(WHOAMI_EMAIL);

    await user.type(screen.getByLabelText("Reply text"), "Proceed");
    await waitFor(() => expect(screen.getByRole("button", { name: "Send reply" })).toBeEnabled());
    await user.click(screen.getByRole("button", { name: "Send reply" }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledOnce());
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(`/v1alpha1/tickets/${TICKET_PROJECTION.ticket_id}/replies`);
    expect(headerNames(init)).not.toContain("authorization");
    expect(JSON.parse(init.body as string)).toEqual({
      id: expect.stringMatching(/^[A-Za-z0-9_-]{8,64}$/),
      replier: WHOAMI_ACTOR_ID,
      text: "Proceed",
    });
    expect(await screen.findByRole("status")).toHaveTextContent("Reply sent");
  });

  it("cannot reply or decide from an unbound login", async () => {
    mockGetWhoami.mockResolvedValue(WHOAMI_UNBOUND);
    const user = userEvent.setup();
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    renderRoute();
    await screen.findByRole("heading", { name: "Decisions" });
    await waitFor(() => expect(mockGetWhoami).toHaveBeenCalled());
    await user.type(screen.getByLabelText("Reply text"), "Proceed");
    expect(screen.getByRole("button", { name: "Send reply" })).toBeDisabled();
    const approve = within(screen.getByLabelText(`Outcomes for ${PENDING_TASK_ID}`)).getByRole("button", { name: "approved" });
    expect(approve).toBeDisabled();
    await user.click(approve);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("shows the merged PR and freezes every reply control", async () => {
    mockGetTicket.mockResolvedValue({
      ...structuredClone(TICKET_PROJECTION),
      frozen: true,
      merged_pr: { url: "https://github.example.test/pulls/42", number: 42 },
      freeze: {
        reason: "ticket_frozen",
        ticket_status: "Done",
        cancelled_runs: 2,
        parked_runs: 0,
        banner: "Ticket status Done: 2 runs cancelled and 0 parked with reason ticket_frozen.",
      },
    });
    renderRoute();
    // The banner names what the freeze did to the ticket's runs (task t17,
    // spec c28/h19) — the count comes from the API's own summary, so this
    // asserts the page RENDERS it rather than recomputing it here.
    expect(await screen.findByRole("status")).toHaveTextContent(
      "Ticket status Done: 2 runs cancelled and 0 parked with reason ticket_frozen.",
    );
    expect(await screen.findByRole("status")).toHaveTextContent("Frozen");
    expect(screen.getByRole("link", { name: "PR #42" })).toHaveAttribute("href", "https://github.example.test/pulls/42");
    expect(screen.getByLabelText("Question (optional)")).toBeDisabled();
    expect(screen.getByLabelText("Reply text")).toBeDisabled();
    expect(screen.getByRole("button", { name: "Send reply" })).toBeDisabled();
  });

  // Task t18 (spec c6/c10): the ticket page is where the Jira comment sends
  // a decider, so it has to be able to take the decision.
  it("renders one button per allowed outcome, enabled once whoami is bound, and posts the decision as the signed-in actor", async () => {
    const user = userEvent.setup();
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({
      human_task_id: PENDING_TASK_ID, run_id: "run-ticket-1", node_run_id: "nr-1",
      outcome: "approved", ledger_records: [], run_state: "running",
    }), { status: 200, headers: { "content-type": "application/json" } }));
    vi.stubGlobal("fetch", fetchMock);
    renderRoute();

    await screen.findByRole("heading", { name: "Decisions" });
    const options = within(screen.getByLabelText(`Outcomes for ${PENDING_TASK_ID}`)).getAllByRole("button");
    expect(options.map((button) => button.textContent)).toEqual(["approved", "rejected"]);
    // Nothing to type: the buttons are live as soon as whoami binds the login.
    await waitFor(() => expect(options[0]).toBeEnabled());
    expect(screen.queryByLabelText(/decider/i)).toBeNull();

    await user.click(within(screen.getByLabelText(`Outcomes for ${PENDING_TASK_ID}`)).getByRole("button", { name: "approved" }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledOnce());
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(`/v1alpha1/human-tasks/${PENDING_TASK_ID}/decision`);
    expect(headerNames(init)).not.toContain("authorization");
    expect(JSON.parse(init.body as string)).toEqual({
      outcome: "approved",
      decider_actor_id: WHOAMI_ACTOR_ID,
      response: { outcome: "approved" },
      // The version the API SERVED with the task, not one re-read here.
      expected_ledger_version: 7,
    });

    // The decided item leaves the list; with none left the whole section goes.
    await waitFor(() => expect(screen.queryByRole("heading", { name: "Decisions" })).toBeNull());
  });

  it("shows no Decisions section when nothing on the ticket is pending", async () => {
    mockGetTicket.mockResolvedValue({ ...structuredClone(TICKET_PROJECTION), pending_tasks: [] });
    renderRoute();
    await screen.findByRole("heading", { name: TICKET_PROJECTION.ticket_id });
    expect(screen.queryByRole("heading", { name: "Decisions" })).toBeNull();
    // Open in Jira is not conditional on there being a decision to take.
    expect(screen.getByRole("link", { name: "Open in Jira" })).toHaveAttribute("href", TICKET_URL);
  });

  it("surfaces a refused decision instead of dropping the item", async () => {
    const user = userEvent.setup();
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({
      message: "the run moved since this page was read", remediation: "reload the ticket and decide again",
    }), { status: 409, headers: { "content-type": "application/json" } })));
    renderRoute();
    await screen.findByRole("heading", { name: "Decisions" });
    const approve = within(screen.getByLabelText(`Outcomes for ${PENDING_TASK_ID}`)).getByRole("button", { name: "approved" });
    await waitFor(() => expect(approve).toBeEnabled());
    await user.click(approve);

    expect(await screen.findByRole("alert")).toHaveTextContent("the run moved since this page was read");
    expect(screen.getByRole("heading", { name: "Decisions" })).toBeInTheDocument();
  });
});


/**
 * The ticket page decides the ticket's claims too (task t12, spec c11,
 * decision c40). `pending_records` arrives grouped by run, each group quoted
 * at the ledger version THIS response read, and the page submits one review
 * per run in a single action — a person deciding a ticket never has to know
 * the ledger is per run.
 */
describe("TicketView claim reviews (task t12)", () => {
  beforeEach(() => {
    resetWhoamiForTests();
    mockGetWhoami.mockReset();
    mockGetWhoami.mockResolvedValue(WHOAMI_BOUND);
    mockGetTicket.mockResolvedValue(structuredClone(TICKET_PROJECTION));
  });

  function reviewsFetch(result = TICKET_REVIEWS_RESULT) {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify(result), {
        status: 200,
        headers: { "content-type": "application/json" },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);
    return fetchMock;
  }

  async function findClaimGroup(runId = TICKET_RUN_ID) {
    await screen.findByRole("heading", { name: "Claims awaiting a decision" });
    return document.querySelector(`[data-run-id="${runId}"]`) as HTMLElement;
  }

  it("groups the pending records per run, quoting the version each was served at", async () => {
    renderRoute();
    const first = await findClaimGroup();
    expect(within(first).getByText(new RegExp(`ledger version ${TICKET_RUN_LEDGER_VERSION}`))).toBeInTheDocument();
    expect(within(first).getByText(TICKET_CLAIM_RECORD_ID)).toBeInTheDocument();
    expect(within(first).getByText(TICKET_EVIDENCE_RECORD_ID)).toBeInTheDocument();
    // The qualifying half of the claim, rendered as prose a person can read.
    expect(within(first).getByText(/the board half is unproven/)).toBeInTheDocument();

    const second = document.querySelector(`[data-run-id="${SECOND_TICKET_RUN_ID}"]`) as HTMLElement;
    expect(within(second).getByText(new RegExp(`ledger version ${SECOND_TICKET_RUN_LEDGER_VERSION}`))).toBeInTheDocument();
  });

  it("keeps the single submit refused until a rationale is stated", async () => {
    const user = userEvent.setup();
    renderRoute();
    await findClaimGroup();
    const submit = screen.getByRole("button", { name: "Record decisions" });
    expect(submit).toBeDisabled();
    await user.type(screen.getByLabelText(/Why \(recorded on every decision\)/), "read both claims");
    await waitFor(() => expect(submit).toBeEnabled());
  });

  it("keeps the submit refused for an unbound login even with a rationale", async () => {
    mockGetWhoami.mockResolvedValue(WHOAMI_UNBOUND);
    const user = userEvent.setup();
    renderRoute();
    await findClaimGroup();
    await user.type(screen.getByLabelText(/Why \(recorded on every decision\)/), "read both claims");
    expect(screen.getByRole("button", { name: "Record decisions" })).toBeDisabled();
  });

  it("posts every selected run at its served ledger version in ONE request, with no Authorization header", async () => {
    const user = userEvent.setup();
    const fetchMock = reviewsFetch();
    renderRoute();
    const first = await findClaimGroup();

    // Reject the evidence record, leave the claim confirmed, and drop the
    // second run's record out of this review entirely.
    await user.click(
      within(within(first).getByRole("group", { name: `Verdict for ${TICKET_EVIDENCE_RECORD_ID}` }))
        .getByRole("radio", { name: "reject" }),
    );
    const second = document.querySelector(`[data-run-id="${SECOND_TICKET_RUN_ID}"]`) as HTMLElement;
    await user.click(
      within(within(second).getByRole("group", { name: `Verdict for ${SECOND_TICKET_CLAIM_RECORD_ID}` }))
        .getByRole("radio", { name: "not now" }),
    );

    // A run needing both answers is two reviews at one version, and the
    // ledger will only take the first — so the page says so BEFORE the click
    // rather than letting it arrive as a surprise conflict.
    expect(
      within(first).getByText(/both confirmations and rejections/),
    ).toBeInTheDocument();

    await user.type(screen.getByLabelText(/Why \(recorded on every decision\)/), "read the qualification");
    await user.click(screen.getByRole("button", { name: "Record decisions" }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledOnce());
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(`/v1alpha1/tickets/${TICKET_PROJECTION.ticket_id}/reviews`);
    expect(headerNames(init)).not.toContain("authorization");
    expect(JSON.parse(init.body as string)).toEqual({
      runs: [
        {
          run_id: TICKET_RUN_ID,
          expected_ledger_version: TICKET_RUN_LEDGER_VERSION,
          records: [TICKET_CLAIM_RECORD_ID],
          verdict: "confirmed",
        },
        {
          run_id: TICKET_RUN_ID,
          expected_ledger_version: TICKET_RUN_LEDGER_VERSION,
          records: [TICKET_EVIDENCE_RECORD_ID],
          verdict: "rejected",
        },
      ],
      rationale: "read the qualification",
      reviewer_actor_id: WHOAMI_ACTOR_ID,
    });
  });

  it("shows each run's own outcome inline and offers to reload only the conflicted group", async () => {
    const user = userEvent.setup();
    reviewsFetch();
    renderRoute();
    await findClaimGroup();
    await user.type(screen.getByLabelText(/Why \(recorded on every decision\)/), "read the qualification");
    await user.click(screen.getByRole("button", { name: "Record decisions" }));

    const committed = await screen.findByTestId(`review-result-${TICKET_RUN_ID}`);
    expect(committed).toHaveTextContent("decision recorded");
    expect(committed).toHaveTextContent("review-01JTICKETREVIEW00000001");
    expect(within(committed).queryByRole("button", { name: /Reload this group/ })).toBeNull();

    const conflicted = screen.getByTestId(`review-result-${SECOND_TICKET_RUN_ID}`);
    expect(conflicted).toHaveTextContent("the run moved since this page was read");
    expect(within(conflicted).getByRole("button", { name: /Reload this group/ })).toBeInTheDocument();
  });

  // PRD §10.8, restated on the surface that does it: a review names records,
  // it never rewrites them.
  it("leaves a decided record reading proposed, with the review beside it", async () => {
    const user = userEvent.setup();
    reviewsFetch();
    renderRoute();
    await findClaimGroup();
    await user.type(screen.getByLabelText(/Why \(recorded on every decision\)/), "read the qualification");
    await user.click(screen.getByRole("button", { name: "Record decisions" }));

    await screen.findByTestId(`review-result-${TICKET_RUN_ID}`);
    const row = document.querySelector(`[data-record-id="${TICKET_CLAIM_RECORD_ID}"]`) as HTMLElement;
    expect(row.querySelector('[data-authority="proposed"]')).not.toBeNull();
    expect(row.querySelector('[data-authority="confirmed"]')).not.toBeNull();
    expect(
      screen.getByTestId(`review-result-${TICKET_RUN_ID}`),
    ).toHaveTextContent("a review names them, it never rewrites them");
  });

  it("reloads only the conflicted group, at the version the reload served", async () => {
    const user = userEvent.setup();
    reviewsFetch();
    renderRoute();
    await findClaimGroup();
    await user.type(screen.getByLabelText(/Why \(recorded on every decision\)/), "read the qualification");
    await user.click(screen.getByRole("button", { name: "Record decisions" }));
    await screen.findByTestId(`review-result-${SECOND_TICKET_RUN_ID}`);

    // The reload re-reads the ticket and takes only that group out of it.
    const moved = structuredClone(TICKET_PROJECTION);
    moved.pending_records![1].ledger_version = 9;
    mockGetTicket.mockResolvedValue(moved);
    await user.click(
      within(screen.getByTestId(`review-result-${SECOND_TICKET_RUN_ID}`))
        .getByRole("button", { name: /Reload this group/ }),
    );

    const second = document.querySelector(`[data-run-id="${SECOND_TICKET_RUN_ID}"]`) as HTMLElement;
    await waitFor(() => expect(within(second).getByText(/ledger version 9/)).toBeInTheDocument());
    // The committed group is untouched by a reload of its neighbour.
    expect(screen.getByTestId(`review-result-${TICKET_RUN_ID}`)).toHaveTextContent("decision recorded");
    expect(screen.queryByTestId(`review-result-${SECOND_TICKET_RUN_ID}`)).toBeNull();
  });

  /**
   * Two passes over one ticket: the first commits one run and conflicts on
   * the other, the second decides only what the first did not. A record a
   * committed review already named must not be re-sent — the commit itself
   * moved that run past the version the page holds, so re-sending it would
   * manufacture a conflict on work that already landed.
   */
  it("re-submits only what the first pass did not decide", async () => {
    const user = userEvent.setup();
    const fetchMock = reviewsFetch();
    renderRoute();
    await findClaimGroup();
    await user.type(screen.getByLabelText(/Why \(recorded on every decision\)/), "read both");
    await user.click(screen.getByRole("button", { name: "Record decisions" }));
    await screen.findByTestId(`review-result-${SECOND_TICKET_RUN_ID}`);

    const moved = structuredClone(TICKET_PROJECTION);
    moved.pending_records![1].ledger_version = 9;
    mockGetTicket.mockResolvedValue(moved);
    await user.click(
      within(screen.getByTestId(`review-result-${SECOND_TICKET_RUN_ID}`))
        .getByRole("button", { name: /Reload this group/ }),
    );
    await waitFor(() =>
      expect(screen.queryByTestId(`review-result-${SECOND_TICKET_RUN_ID}`)).toBeNull(),
    );

    fetchMock.mockResolvedValue(
      new Response(JSON.stringify({
        ticket_id: TICKET_PROJECTION.ticket_id,
        committed_runs: 1,
        runs: [{ run_id: SECOND_TICKET_RUN_ID, status: "committed", review_id: "review-2", ledger_version: 11 }],
      }), { status: 200, headers: { "content-type": "application/json" } }),
    );
    await user.click(screen.getByRole("button", { name: "Record decisions" }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(JSON.parse(fetchMock.mock.calls[1][1].body as string).runs).toEqual([
      {
        run_id: SECOND_TICKET_RUN_ID,
        expected_ledger_version: 9,
        records: [SECOND_TICKET_CLAIM_RECORD_ID],
        verdict: "confirmed",
      },
    ]);
    // The first pass's committed outcome is still on screen.
    expect(screen.getByTestId(`review-result-${TICKET_RUN_ID}`)).toHaveTextContent(
      "decision recorded",
    );
  });

  it("offers nothing more to record once every claim has been decided", async () => {
    const user = userEvent.setup();
    reviewsFetch({
      ticket_id: TICKET_PROJECTION.ticket_id,
      committed_runs: 2,
      runs: [
        { run_id: TICKET_RUN_ID, status: "committed", review_id: "r1", ledger_version: 9 },
        { run_id: SECOND_TICKET_RUN_ID, status: "committed", review_id: "r2", ledger_version: 6 },
      ],
    });
    renderRoute();
    await findClaimGroup();
    await user.type(screen.getByLabelText(/Why \(recorded on every decision\)/), "read both");
    await user.click(screen.getByRole("button", { name: "Record decisions" }));

    await screen.findByTestId(`review-result-${SECOND_TICKET_RUN_ID}`);
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Record decisions" })).toBeDisabled(),
    );
  });

  it("says so plainly, and offers nothing, when no claim is awaiting a decision", async () => {
    mockGetTicket.mockResolvedValue({ ...structuredClone(TICKET_PROJECTION), pending_records: [] });
    renderRoute();
    await screen.findByRole("heading", { name: TICKET_PROJECTION.ticket_id });
    expect(screen.queryByRole("heading", { name: "Claims awaiting a decision" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Record decisions" })).toBeNull();
  });

  it("tolerates a control plane older than t14, which serves no pending_records at all", async () => {
    const older = structuredClone(TICKET_PROJECTION);
    delete older.pending_records;
    mockGetTicket.mockResolvedValue(older);
    renderRoute();
    await screen.findByRole("heading", { name: TICKET_PROJECTION.ticket_id });
    expect(screen.queryByRole("heading", { name: "Claims awaiting a decision" })).toBeNull();
  });

  it("surfaces a refused batch without pretending anything was decided", async () => {
    const user = userEvent.setup();
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({
      message: "ledger: authority refused [reviewer_must_be_human]: origin agent",
      remediation: "the producer named in origin may not write this record's authority; see PRD §10.4",
    }), { status: 400, headers: { "content-type": "application/json" } })));
    renderRoute();
    await findClaimGroup();
    await user.type(screen.getByLabelText(/Why \(recorded on every decision\)/), "I am sure of my own work");
    await user.click(screen.getByRole("button", { name: "Record decisions" }));

    expect(await screen.findByText(/reviewer_must_be_human/)).toBeInTheDocument();
    expect(screen.queryByTestId(`review-result-${TICKET_RUN_ID}`)).toBeNull();
  });
});

/**
 * Frame claims are the custody checkout's, not this page's (spec c13/h20):
 * internal/devague.MapFrameClaims has no production caller and the live path
 * is an opaque frame_json blob, so the page SHOWS a frame claim and its
 * confirmation state and offers nothing to change it.
 */
describe("TicketView frame claims are read-only (task t12, spec c13)", () => {
  beforeEach(() => {
    resetWhoamiForTests();
    mockGetWhoami.mockReset();
    mockGetWhoami.mockResolvedValue(WHOAMI_BOUND);
    mockGetTicket.mockResolvedValue(structuredClone(TICKET_PROJECTION));
  });

  it("renders each frame claim with its confirmation state and no control at all", async () => {
    renderRoute();
    await screen.findByRole("heading", { name: TICKET_PROJECTION.ticket_id });
    const section = document.querySelector("#ticket-frame-claims") as HTMLElement;
    expect(section).not.toBeNull();

    const states = Array.from(section.querySelectorAll("[data-claim-id] [data-state]"))
      .map((node) => node.getAttribute("state") ?? node.getAttribute("data-state"));
    expect(states).toEqual(["confirmed", "proposed", "rejected"]);

    expect(section.querySelectorAll("input, select, textarea, button")).toHaveLength(0);
    expect(within(section).getByText(/confirmed in the custody checkout/i)).toBeInTheDocument();
  });
});

describe("TicketView layout (task t27)", () => {
  beforeEach(() => {
    resetWhoamiForTests();
    mockGetWhoami.mockReset();
    mockGetWhoami.mockResolvedValue(WHOAMI_BOUND);
    mockGetTicket.mockResolvedValue(structuredClone(TICKET_PROJECTION));
  });

  it("sits on the app's full-width rail, like every other view", async () => {
    renderRoute();
    await screen.findByRole("heading", { name: TICKET_PROJECTION.ticket_id });

    const rail = document.querySelector(".ticket-view")!;
    expect(rail).toHaveClass("view-rail");
  });

  it("renders the loading and error states on the same rail", async () => {
    mockGetTicket.mockReturnValue(new Promise(() => {}));
    renderRoute();
    expect(await screen.findByText("Loading ticket projection…")).toBeInTheDocument();
    expect(document.querySelector("section.view-rail")).not.toBeNull();
  });
});
