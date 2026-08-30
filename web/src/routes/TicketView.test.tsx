import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { getTicket } from "../api/client";
import { PENDING_TASK_ID, STALE_FRAME_TICKET_URL, TICKET_PROJECTION, TICKET_URL } from "../fixtures/ticket-fixture";
import TicketView from "./TicketView";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return { ...actual, getTicket: vi.fn() };
});

const mockGetTicket = vi.mocked(getTicket);

function renderRoute() {
  return render(
    <MemoryRouter initialEntries={[`/tickets/${TICKET_PROJECTION.ticket_id}`]}>
      <Routes><Route path="/tickets/:id" element={<TicketView />} /></Routes>
    </MemoryRouter>,
  );
}

describe("TicketView", () => {
  beforeEach(() => {
    window.sessionStorage.clear();
    // The decider id is remembered in localStorage across sittings, so it
    // must be cleared between cases or one test seeds the next one's field.
    window.localStorage.clear();
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

  it("posts a reply with the session-held decision token", async () => {
    const user = userEvent.setup();
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({
      id: "reply-2", replier: "operator", text: "Proceed", created_at: "2026-08-29T10:00:00Z",
    }), { status: 201, headers: { "content-type": "application/json" } }));
    vi.stubGlobal("fetch", fetchMock);
    renderRoute();
    await screen.findByRole("heading", { name: TICKET_PROJECTION.ticket_id });
    await user.type(screen.getByLabelText("Decision token"), "decision-token");
    await user.type(screen.getByLabelText("Your name"), "operator");
    await user.type(screen.getByLabelText("Reply text"), "Proceed");
    await user.click(screen.getByRole("button", { name: "Send reply" }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledOnce());
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(`/v1alpha1/tickets/${TICKET_PROJECTION.ticket_id}/replies`);
    expect((init.headers as Record<string, string>).authorization).toBe("Bearer decision-token");
    expect(JSON.parse(init.body as string)).toEqual({
      id: expect.stringMatching(/^[A-Za-z0-9_-]{8,64}$/),
      replier: "operator",
      text: "Proceed",
    });
    expect(window.sessionStorage.getItem("nodes.human-decision-token")).toBe("decision-token");
    expect(await screen.findByRole("status")).toHaveTextContent("Reply sent");
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
    expect(screen.getByLabelText("Decision token")).toBeDisabled();
    expect(screen.getByLabelText("Reply text")).toBeDisabled();
    expect(screen.getByRole("button", { name: "Send reply" })).toBeDisabled();
  });

  // Task t18 (spec c6/c10): the ticket page is where the Jira comment sends
  // a decider, so it has to be able to take the decision.
  it("renders one button per allowed outcome and posts the decision under the held token", async () => {
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
    // Nothing is decidable until a token is held AND a decider is named:
    // the token authenticates the deployment, not the person.
    expect(options[0]).toBeDisabled();

    await user.type(screen.getByLabelText("Decision token"), "decision-token");
    await user.type(screen.getByLabelText("Decider actor id"), "actor://company/operator");
    await user.click(within(screen.getByLabelText(`Outcomes for ${PENDING_TASK_ID}`)).getByRole("button", { name: "approved" }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledOnce());
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(`/v1alpha1/human-tasks/${PENDING_TASK_ID}/decision`);
    expect((init.headers as Record<string, string>).authorization).toBe("Bearer decision-token");
    expect(JSON.parse(init.body as string)).toEqual({
      outcome: "approved",
      decider_actor_id: "actor://company/operator",
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
    await user.type(screen.getByLabelText("Decision token"), "decision-token");
    await user.type(screen.getByLabelText("Decider actor id"), "operator");
    await user.click(within(screen.getByLabelText(`Outcomes for ${PENDING_TASK_ID}`)).getByRole("button", { name: "approved" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("the run moved since this page was read");
    expect(screen.getByRole("heading", { name: "Decisions" })).toBeInTheDocument();
  });
});


describe("TicketView layout (task t27)", () => {
  beforeEach(() => {
    window.sessionStorage.clear();
    window.localStorage.clear();
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
