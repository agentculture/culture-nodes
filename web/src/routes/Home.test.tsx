import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  getRun,
  getTicket,
  getWhoami,
  listHumanTasks,
  listPendingDecisions,
} from "../api/client";
import { PENDING_TASK } from "../fixtures/human-tasks-fixture";
import { TICKET_PROJECTION } from "../fixtures/ticket-fixture";
import { WHOAMI_BOUND, WHOAMI_EMAIL } from "../fixtures/whoami-fixture";
import { resetWhoamiForTests } from "../hooks/useWhoami";
import Home from "./Home";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    getRun: vi.fn(),
    getTicket: vi.fn(),
    getWhoami: vi.fn(),
    listHumanTasks: vi.fn(),
    listPendingDecisions: vi.fn(),
  };
});

const mockGetRun = vi.mocked(getRun);
const mockGetTicket = vi.mocked(getTicket);
const mockGetWhoami = vi.mocked(getWhoami);
const mockListHumanTasks = vi.mocked(listHumanTasks);
const mockListPendingDecisions = vi.mocked(listPendingDecisions);

const TICKET_KEY = TICKET_PROJECTION.ticket_id;

function renderHome() {
  return render(
    <MemoryRouter>
      <Home />
    </MemoryRouter>,
  );
}

function runWithTicket(id: string, ticketKey: string | null) {
  return {
    run: {
      id,
      workflow_digest: "sha256:abc",
      state: "waiting" as const,
      created_at: "2026-08-15T09:00:00Z",
      updated_at: "2026-08-15T09:00:00Z",
      input: ticketKey ? { ticket_key: ticketKey } : { note: "no ticket here" },
    },
    tokens: [],
    node_runs: [],
  };
}

/**
 * The first screen a signed-in person gets (task t17, spec c25, issue #270).
 *
 * The assertions that matter most are the refusals: the page must never name
 * the reader as the person a decision is waiting on (nothing binds a human
 * task to a principal), and a run it cannot turn into a ticket page must be
 * counted and named rather than quietly dropped.
 */
describe("Home", () => {
  beforeEach(() => {
    resetWhoamiForTests();
    for (const mock of [
      mockGetRun,
      mockGetTicket,
      mockGetWhoami,
      mockListHumanTasks,
      mockListPendingDecisions,
    ]) {
      mock.mockReset();
    }
    mockGetWhoami.mockResolvedValue(WHOAMI_BOUND);
    mockListHumanTasks.mockResolvedValue({ items: [] });
    mockListPendingDecisions.mockResolvedValue({ items: [], record_count: 0 });
  });

  it("names each ticket with pending work, and links to its page", async () => {
    mockListHumanTasks.mockResolvedValue({ items: [PENDING_TASK] });
    mockGetRun.mockResolvedValue(runWithTicket(PENDING_TASK.run_id, TICKET_KEY));
    mockGetTicket.mockResolvedValue(structuredClone(TICKET_PROJECTION));
    renderHome();

    expect(
      await screen.findByRole("link", { name: `Open ${TICKET_KEY}` }),
    ).toHaveAttribute("href", `/tickets/${TICKET_KEY}`);
    expect(
      screen.getByRole("heading", { name: "One ticket is waiting on a person" }),
    ).toBeInTheDocument();
  });

  it("draws each ticket's flow rail, so the landing and the ticket page speak one language", async () => {
    mockListHumanTasks.mockResolvedValue({ items: [PENDING_TASK] });
    mockGetRun.mockResolvedValue(runWithTicket(PENDING_TASK.run_id, TICKET_KEY));
    mockGetTicket.mockResolvedValue(structuredClone(TICKET_PROJECTION));
    renderHome();

    const rail = await screen.findByTestId("ticket-flow-rail");
    expect(rail.querySelectorAll("[data-stage]")).toHaveLength(5);
    expect(rail.querySelector('[data-stage="review"][data-current="true"]')).not.toBeNull();
  });

  it("never claims the decision is waiting on the reader", async () => {
    mockListHumanTasks.mockResolvedValue({ items: [PENDING_TASK] });
    mockGetRun.mockResolvedValue(runWithTicket(PENDING_TASK.run_id, TICKET_KEY));
    mockGetTicket.mockResolvedValue(structuredClone(TICKET_PROJECTION));
    renderHome();

    await screen.findByRole("link", { name: `Open ${TICKET_KEY}` });
    expect(
      screen.getByText(/a decision is raised for a role, not routed to a named person/i),
    ).toBeInTheDocument();
    expect(screen.queryByText(/waiting on you/i)).toBeNull();
  });

  it("says how a ticket reaches a person, and says it in the header field too", async () => {
    renderHome();
    expect(
      await screen.findByRole("heading", { name: "How a ticket reaches you" }),
    ).toBeInTheDocument();
    expect(screen.getByText(/comments on its Jira ticket/i)).toBeInTheDocument();
    expect(screen.getByText(/Tickets field in the header/i)).toBeInTheDocument();
  });

  it("states plainly that nothing is pending, rather than rendering an empty page", async () => {
    renderHome();
    expect(await screen.findByTestId("home-nothing-pending")).toHaveTextContent(
      "Nothing on this control plane is waiting on a person right now.",
    );
    expect(
      screen.getByRole("heading", { name: "Nothing is waiting on a person" }),
    ).toBeInTheDocument();
  });

  it("counts a pending run that records no ticket key instead of dropping it", async () => {
    mockListHumanTasks.mockResolvedValue({ items: [PENDING_TASK] });
    mockGetRun.mockResolvedValue(runWithTicket(PENDING_TASK.run_id, null));
    renderHome();

    expect(await screen.findByTestId("home-unticketed")).toHaveTextContent(
      /records no ticket key/,
    );
    expect(mockGetTicket).not.toHaveBeenCalled();
  });

  it("names the signed-in person rather than greeting a generic visitor", async () => {
    renderHome();
    await waitFor(() =>
      expect(screen.getByText(new RegExp(WHOAMI_EMAIL))).toBeInTheDocument(),
    );
  });

  it("surfaces a failed read instead of showing an empty queue", async () => {
    mockListHumanTasks.mockRejectedValue(new Error("boom"));
    renderHome();
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(screen.queryByTestId("home-nothing-pending")).toBeNull();
  });
});
