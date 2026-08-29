import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { getTicket } from "../api/client";
import { TICKET_PROJECTION, TICKET_URL } from "../fixtures/ticket-fixture";
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
    mockGetTicket.mockResolvedValue(structuredClone(TICKET_PROJECTION));
  });

  it("renders the complete projection, every claim as icon plus word, and the exact Jira back-link", async () => {
    renderRoute();
    expect(await screen.findByRole("heading", { name: TICKET_PROJECTION.ticket_id })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open in Jira" })).toHaveAttribute("href", TICKET_URL);

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
    expect(JSON.parse(init.body as string)).toEqual({ reply_id: expect.stringMatching(/\S/), replier: "operator", text: "Proceed" });
    expect(window.sessionStorage.getItem("nodes.human-decision-token")).toBe("decision-token");
    expect(await screen.findByRole("status")).toHaveTextContent("Reply sent");
  });

  it("resends the same reply_id when a failed send is retried, and mints a new one after success", async () => {
    const user = userEvent.setup();
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ code: 2, message: "internal error", remediation: "retry" }), {
        status: 500, headers: { "content-type": "application/json" },
      }))
      .mockResolvedValue(new Response(JSON.stringify({
        id: "reply-2", replier: "operator", text: "Proceed", created_at: "2026-08-29T10:00:00Z",
      }), { status: 201, headers: { "content-type": "application/json" } }));
    vi.stubGlobal("fetch", fetchMock);
    renderRoute();
    await screen.findByRole("heading", { name: TICKET_PROJECTION.ticket_id });
    await user.type(screen.getByLabelText("Decision token"), "decision-token");
    await user.type(screen.getByLabelText("Your name"), "operator");
    await user.type(screen.getByLabelText("Reply text"), "Proceed");
    await user.click(screen.getByRole("button", { name: "Send reply" }));
    await screen.findByRole("alert");
    await user.click(screen.getByRole("button", { name: "Send reply" }));
    await screen.findByRole("status");
    await user.type(screen.getByLabelText("Reply text"), "Another");
    await user.click(screen.getByRole("button", { name: "Send reply" }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
    const keys = fetchMock.mock.calls.map(([, init]) => JSON.parse((init as RequestInit).body as string).reply_id as string);
    expect(keys[0]).toMatch(/\S/);
    expect(keys[1]).toBe(keys[0]);
    expect(keys[2]).not.toBe(keys[0]);
  });

  it("shows the merged PR and freezes every reply control", async () => {
    mockGetTicket.mockResolvedValue({
      ...structuredClone(TICKET_PROJECTION),
      frozen: true,
      merged_pr: { url: "https://github.example.test/pulls/42", number: 42 },
    });
    renderRoute();
    expect(await screen.findByRole("status")).toHaveTextContent("Frozen");
    expect(screen.getByRole("link", { name: "PR #42" })).toHaveAttribute("href", "https://github.example.test/pulls/42");
    expect(screen.getByLabelText("Decision token")).toBeDisabled();
    expect(screen.getByLabelText("Reply text")).toBeDisabled();
    expect(screen.getByRole("button", { name: "Send reply" })).toBeDisabled();
  });
});
