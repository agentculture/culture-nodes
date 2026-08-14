import { beforeEach, afterEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import Inbox from "./Inbox";
import { ApiError, getLedger, listHumanTasks } from "../api/client";
import {
  DECIDED_TASK,
  DECISION_RESULT,
  LEDGER_VERSION,
  PENDING_TASK,
  PENDING_TASK_MINIMAL,
} from "../fixtures/human-tasks-fixture";
import { getAgentState, resetAgentState } from "../agent-state/store";
import { resetSharedEventsForTests } from "../hooks/useSharedEvents";

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

/**
 * The stub-API pattern Workflows.test.tsx established, with one deliberate
 * difference: `decideHumanTask` is NOT mocked. The acceptance for t14 is a
 * component test asserting the POST body *and the Authorization header* a
 * submission actually sends, so the decision path runs the real client
 * helper against a stubbed global `fetch` — the reads (list, ledger) stay
 * module-mocked like every other route test.
 */
vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return { ...actual, listHumanTasks: vi.fn(), getLedger: vi.fn() };
});

const mockListHumanTasks = vi.mocked(listHumanTasks);
const mockGetLedger = vi.mocked(getLedger);

const TOKEN = "sekrit-decision-token";

function renderInbox() {
  return render(
    <MemoryRouter initialEntries={["/inbox"]}>
      <Inbox />
    </MemoryRouter>,
  );
}

/** Both list calls (pending, decided) resolve with the standard fixture. */
function resolveFixture({
  pending = [PENDING_TASK, PENDING_TASK_MINIMAL],
  decided = [DECIDED_TASK],
} = {}) {
  mockListHumanTasks.mockImplementation(async (_signal, params) =>
    params?.status === "pending" ? { items: pending } : { items: decided },
  );
  mockGetLedger.mockResolvedValue({
    items: [],
    ledger_version: LEDGER_VERSION,
  });
}

function stubDecisionFetch(
  body: unknown = DECISION_RESULT,
  status = 200,
): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn().mockResolvedValue(
    new Response(JSON.stringify(body), {
      status,
      headers: { "content-type": "application/json" },
    }),
  );
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

async function findPendingCard() {
  await screen.findByText(PENDING_TASK.id);
  return document.querySelector(
    `[data-human-task-id="${PENDING_TASK.id}"]`,
  ) as HTMLElement;
}

async function holdToken(user: ReturnType<typeof userEvent.setup>) {
  await user.type(screen.getByLabelText("Decision token"), TOKEN);
  await user.click(screen.getByRole("button", { name: "Hold token" }));
}

/** Fill the full decision form on the rich pending task's card. */
async function fillDecision(
  user: ReturnType<typeof userEvent.setup>,
  card: HTMLElement,
  { payload, note }: { payload?: string; note?: string } = {},
) {
  await user.click(within(card).getByRole("radio", { name: "approved" }));
  await user.type(within(card).getByLabelText("Decider actor id"), "ori");
  if (payload !== undefined) {
    // user-event's type() parses `{`/`[` as keyboard descriptors; paste the
    // JSON instead of escaping every brace.
    await user.click(within(card).getByLabelText(/Decision payload/));
    await user.paste(payload);
  }
  if (note !== undefined) {
    await user.type(within(card).getByLabelText(/Note/), note);
  }
}

beforeEach(() => {
  mockListHumanTasks.mockReset();
  mockGetLedger.mockReset();
  window.sessionStorage.clear();
  window.localStorage.clear();
  resetAgentState();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("Inbox loading/empty/error", () => {
  it("shows a loading state before the lists resolve", () => {
    mockListHumanTasks.mockReturnValue(new Promise(() => {}));
    renderInbox();
    expect(screen.getByText("Loading inbox…")).toBeInTheDocument();
  });

  it("shows the empty state when nothing is pending and nothing was decided", async () => {
    resolveFixture({ pending: [], decided: [] });
    renderInbox();
    expect(
      await screen.findByText(/No human tasks yet\./),
    ).toBeInTheDocument();
  });

  it("renders the error contract when the list request fails", async () => {
    mockListHumanTasks.mockRejectedValue(
      new ApiError(0, "cannot reach the control plane", "start `nodes serve`"),
    );
    renderInbox();
    await screen.findByText("error:", { exact: false });
    expect(
      screen.getByText("cannot reach the control plane", { exact: false }),
    ).toBeInTheDocument();
  });

  it("requests pending and decided tasks from the list endpoint", async () => {
    resolveFixture();
    renderInbox();
    await screen.findByText(PENDING_TASK.id);
    const statuses = mockListHumanTasks.mock.calls.map(
      ([, params]) => params?.status,
    );
    expect(statuses).toContain("pending");
    expect(statuses).toContain("decided");
  });
});

describe("Inbox pending task rendering", () => {
  it("renders the request payload legibly: outcomes, deadline, schema ref, context refs, audit block", async () => {
    resolveFixture();
    renderInbox();
    const card = await findPendingCard();
    const scoped = within(card);
    // The §8.8 waiting vocabulary: glyph + word, same chip as everywhere.
    expect(scoped.getByText("waiting")).toBeInTheDocument();
    expect(scoped.getByText("⏸")).toBeInTheDocument();
    // Allowed outcomes are the form's choices.
    for (const outcome of PENDING_TASK.request.allowed_outcomes!) {
      expect(scoped.getByRole("radio", { name: outcome })).toBeInTheDocument();
    }
    expect(
      scoped.getByText("schemas/decisions/release-signoff.json"),
    ).toBeInTheDocument();
    expect(scoped.getByText("team/platform-ai-approvers")).toBeInTheDocument();
    // Deadline renders as a real <time>.
    const deadline = card.querySelector(
      `time[datetime="${PENDING_TASK.request.deadline}"]`,
    );
    expect(deadline).toBeInTheDocument();
    // Context refs and the audit trail.
    expect(scoped.getByText("nodes.build.output")).toBeInTheDocument();
    expect(scoped.getByText("nodes.build.output.diff")).toBeInTheDocument();
    // A literal binding renders as the declared value, not as a pointer and
    // not as "[object Object]" — naming what the task observes is the whole
    // reason the shape exists (issue #73).
    expect(
      scoped.getByText('{"kind":"github_pr_merged","pr":42}'),
    ).toBeInTheDocument();
    expect(scoped.getByText("release-signoff")).toBeInTheDocument();
    expect(scoped.getByText(/build → succeeded/)).toBeInTheDocument();
    // The run link goes to the existing Run view.
    expect(
      scoped.getByRole("link", { name: PENDING_TASK.run_id }),
    ).toHaveAttribute("href", `/runs/${PENDING_TASK.run_id}`);
  });

  it("never fabricates absent request fields on the minimal task", async () => {
    resolveFixture();
    renderInbox();
    await screen.findByText(PENDING_TASK_MINIMAL.id);
    const card = document.querySelector(
      `[data-human-task-id="${PENDING_TASK_MINIMAL.id}"]`,
    ) as HTMLElement;
    expect(within(card).queryByText(/deadline/i)).not.toBeInTheDocument();
    expect(within(card).queryByText(/context/i)).not.toBeInTheDocument();
  });

  it("shows the ledger version the decision will be guarded against", async () => {
    resolveFixture();
    renderInbox();
    const card = await findPendingCard();
    expect(
      await within(card).findByText(String(LEDGER_VERSION)),
    ).toBeInTheDocument();
    expect(mockGetLedger).toHaveBeenCalledWith(
      PENDING_TASK.run_id,
      expect.anything(),
    );
  });
});

describe("Inbox decided task rendering", () => {
  it("shows a decided task read-only: response, resolved time, confirmed authority, no form", async () => {
    resolveFixture();
    renderInbox();
    await screen.findByText(DECIDED_TASK.id);
    const card = document.querySelector(
      `[data-human-task-id="${DECIDED_TASK.id}"]`,
    ) as HTMLElement;
    const scoped = within(card);
    // A human decision is confirmed authority — same chip as the ledger.
    expect(card.querySelector('[data-authority="confirmed"]')).toBeTruthy();
    expect(scoped.getByText(/looks right/)).toBeInTheDocument();
    expect(
      card.querySelector(`time[datetime="${DECIDED_TASK.resolved_at}"]`),
    ).toBeInTheDocument();
    expect(
      scoped.queryByRole("button", { name: "Submit decision" }),
    ).not.toBeInTheDocument();
  });
});

describe("Inbox token retention (risk r1)", () => {
  it("holds the token in sessionStorage only — never localStorage, never a cookie — and shows it held", async () => {
    resolveFixture();
    const user = userEvent.setup();
    renderInbox();
    await screen.findByText(PENDING_TASK.id);

    expect(screen.getByText(/no token held/i)).toBeInTheDocument();
    await holdToken(user);

    expect(
      window.sessionStorage.getItem("nodes.human-decision-token"),
    ).toBe(TOKEN);
    expect(window.localStorage.length).toBe(0);
    expect(document.cookie).toBe("");
    expect(screen.getByText(/token held/i)).toBeInTheDocument();
    // The credential never renders back into the page.
    expect(screen.queryByText(TOKEN)).not.toBeInTheDocument();
  });

  it("clears the token from sessionStorage via the clear affordance", async () => {
    resolveFixture();
    const user = userEvent.setup();
    renderInbox();
    await screen.findByText(PENDING_TASK.id);
    await holdToken(user);

    await user.click(screen.getByRole("button", { name: "Clear token" }));
    expect(
      window.sessionStorage.getItem("nodes.human-decision-token"),
    ).toBeNull();
    expect(screen.getByText(/no token held/i)).toBeInTheDocument();
  });

  it("refuses to submit without a token: the form is disabled and no request leaves the browser", async () => {
    resolveFixture();
    const fetchMock = stubDecisionFetch();
    const user = userEvent.setup();
    renderInbox();
    const card = await findPendingCard();
    await within(card).findByText(String(LEDGER_VERSION));

    await fillDecision(user, card);
    const submit = within(card).getByRole("button", {
      name: "Submit decision",
    });
    expect(submit).toBeDisabled();
    await user.click(submit);
    expect(fetchMock).not.toHaveBeenCalled();
  });
});

describe("Inbox decision submission", () => {
  it("POSTs the decision with the bearer token, the chosen outcome, the payload+note response, and the ledger guard", async () => {
    resolveFixture();
    const fetchMock = stubDecisionFetch();
    const user = userEvent.setup();
    renderInbox();
    const card = await findPendingCard();
    await within(card).findByText(String(LEDGER_VERSION));

    await holdToken(user);
    await fillDecision(user, card, {
      payload: '{"release_ok": true}',
      note: "ship it",
    });
    await user.click(
      within(card).getByRole("button", { name: "Submit decision" }),
    );

    await screen.findByText(/decision recorded/i);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(`/v1alpha1/human-tasks/${PENDING_TASK.id}/decision`);
    expect(init.method).toBe("POST");
    expect(
      (init.headers as Record<string, string>).authorization,
    ).toBe(`Bearer ${TOKEN}`);
    expect(JSON.parse(init.body as string)).toEqual({
      outcome: "approved",
      decider_actor_id: "ori",
      response: { release_ok: true, note: "ship it" },
      expected_ledger_version: LEDGER_VERSION,
    });
  });

  it("reloads the lists after a recorded decision", async () => {
    resolveFixture();
    stubDecisionFetch();
    const user = userEvent.setup();
    renderInbox();
    const card = await findPendingCard();
    await within(card).findByText(String(LEDGER_VERSION));
    const listCallsBefore = mockListHumanTasks.mock.calls.length;

    await holdToken(user);
    await fillDecision(user, card);
    await user.click(
      within(card).getByRole("button", { name: "Submit decision" }),
    );

    await screen.findByText(/decision recorded/i);
    expect(mockListHumanTasks.mock.calls.length).toBeGreaterThan(
      listCallsBefore,
    );
  });

  it("refuses an unparseable decision payload locally: no request leaves the browser", async () => {
    resolveFixture();
    const fetchMock = stubDecisionFetch();
    const user = userEvent.setup();
    renderInbox();
    const card = await findPendingCard();
    await within(card).findByText(String(LEDGER_VERSION));

    await holdToken(user);
    await fillDecision(user, card, { payload: "{not json" });
    await user.click(
      within(card).getByRole("button", { name: "Submit decision" }),
    );

    expect(
      await within(card).findByText(/not valid JSON/),
    ).toBeInTheDocument();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("surfaces a refused decision through the error contract", async () => {
    resolveFixture();
    stubDecisionFetch(
      {
        code: 401,
        message: "the bearer token is not valid for this deployment",
        remediation: "authorization failed",
      },
      401,
    );
    const user = userEvent.setup();
    renderInbox();
    const card = await findPendingCard();
    await within(card).findByText(String(LEDGER_VERSION));

    await holdToken(user);
    await fillDecision(user, card);
    await user.click(
      within(card).getByRole("button", { name: "Submit decision" }),
    );

    expect(
      await within(card).findByText(/not valid for this deployment/),
    ).toBeInTheDocument();
  });
});

describe("Inbox auto-refresh (issue #46, task t30)", () => {
  beforeEach(() => {
    resetSharedEventsForTests();
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
  });

  afterEach(() => {
    resetSharedEventsForTests();
  });

  it("refetches on a human-task event, staying stale-while-revalidate: no loading regression, no nulled list", async () => {
    resolveFixture();
    renderInbox();
    await screen.findByRole("heading", { name: "Pending" });
    await waitFor(() => expect(getAgentState().status).toBe("ready"));

    const source = FakeEventSource.instances[0];
    act(() => source.open());

    let resolveReload: (() => void) | undefined;
    mockListHumanTasks.mockImplementationOnce(
      () =>
        new Promise(() => {
          /* pending decided-status call from the reload's Promise.all */
        }),
    );
    mockListHumanTasks.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveReload = () => resolve({ items: [PENDING_TASK, PENDING_TASK_MINIMAL] });
        }),
    );

    act(() => {
      source.emit("dev.culture.nodes.human-task.created", {}, "01EVT1");
    });

    // The reload's own two listHumanTasks calls (pending, decided) join the
    // two from the initial mount — wait for both to have started.
    await waitFor(() => expect(mockListHumanTasks).toHaveBeenCalledTimes(4));

    // The reload fetch is in flight — the rendered cards and agent-state
    // must still be exactly as they were (stale-while-revalidate).
    expect(screen.getByText(PENDING_TASK.id)).toBeInTheDocument();
    expect(screen.queryByText("Loading inbox…")).not.toBeInTheDocument();
    expect(getAgentState().status).toBe("ready");

    await act(async () => {
      resolveReload?.();
    });
    expect(getAgentState().status).toBe("ready");
  });

  it("debounces a burst of simultaneous events into a single refetch", async () => {
    resolveFixture();
    renderInbox();
    await screen.findByRole("heading", { name: "Pending" });

    const source = FakeEventSource.instances[0];
    act(() => source.open());
    mockListHumanTasks.mockClear();
    resolveFixture();

    act(() => {
      source.emit("dev.culture.nodes.human-task.created", {}, "01EVT1");
      source.emit("dev.culture.nodes.human-task.decided", {}, "01EVT2");
    });

    await waitFor(() => expect(mockListHumanTasks).toHaveBeenCalledTimes(2));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(mockListHumanTasks).toHaveBeenCalledTimes(2);
  });

  it("ignores an event type this view did not subscribe to", async () => {
    resolveFixture();
    renderInbox();
    await screen.findByRole("heading", { name: "Pending" });

    const source = FakeEventSource.instances[0];
    act(() => source.open());
    mockListHumanTasks.mockClear();

    act(() => {
      source.emit("dev.culture.nodes.run.created", { run_id: "a" }, "01EVT1");
    });
    await new Promise((resolve) => setTimeout(resolve, 20));

    expect(mockListHumanTasks).not.toHaveBeenCalled();
  });
});
