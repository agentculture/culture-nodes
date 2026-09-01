import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { getTicket, getWhoami } from "../api/client";
import { PENDING_TASK_ID, STALE_FRAME_TICKET_URL, TICKET_PROJECTION, TICKET_URL } from "../fixtures/ticket-fixture";
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
