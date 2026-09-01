import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import {
  ApiError,
  getRun,
  getTicket,
  listHumanTasks,
  listPendingDecisions,
} from "../api/client";
import type { TicketProjection } from "../api/types";
import ErrorNotice from "../components/ErrorNotice";
import TicketFlowRail from "../components/TicketFlowRail";
import { ticketFlow, type TicketFlow } from "../domain/ticket-flow";
import { findTicketKey } from "../domain/ticket-key";
import { useWhoami } from "../hooks/useWhoami";

/**
 * The first screen a signed-in person gets (task t17, spec c25, issue #270).
 *
 * Before this, `/` redirected to `/runs` — a table of engine rows, which is
 * the right landing for an operator and the wrong one for the person a Jira
 * comment just sent here. This page answers the only two questions that
 * person has: *is anything waiting on a human*, and *how do I get to it*.
 *
 * It is deliberately a wall of diagrams rather than a list. Each ticket with
 * pending work renders the same flow rail its own page leads with, so the
 * landing and the destination speak one visual language and a person can see
 * at a glance which ticket is at Review and which is still building.
 *
 * What it will not claim:
 *
 *   - It cannot say whose turn it is. `human_tasks.assigned_owner_id` is a
 *     role/team reference and nothing binds a task to a signed-in principal,
 *     so every card says "waiting on a person" and the page says once, in
 *     plain words, that this is a limit of the data and not a hedge.
 *   - A run with pending work whose input records no ticket key gets counted
 *     and named as exactly that, with a link to the queue that can still
 *     decide it — never dropped because it did not fit the card shape.
 *   - There is no ticket listing endpoint, so "tickets with pending work" is
 *     the only listing this page can honestly build: it is assembled from
 *     the two pending queues, not from a catalogue of every ticket.
 */

/** Runs read per visit. A queue longer than this says so rather than lying. */
const RUN_BUDGET = 12;

interface TicketCard {
  key: string;
  projection: TicketProjection;
  flow: TicketFlow;
}

interface HomeData {
  cards: TicketCard[];
  /** Runs with pending work whose input records no ticket key. */
  untickedRuns: number;
  /** Pending runs beyond RUN_BUDGET, left unread rather than silently dropped. */
  unread: number;
  pendingRuns: number;
}

export function Home() {
  const whoami = useWhoami();
  const [data, setData] = useState<HomeData | null>(null);
  const [error, setError] = useState<ApiError | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    load(controller.signal)
      .then((result) => {
        if (!controller.signal.aborted) setData(result);
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setError(
          cause instanceof ApiError
            ? cause
            : new ApiError(0, String(cause), "check the browser console"),
        );
      });
    return () => controller.abort();
  }, []);

  const greeting =
    whoami.status === "bound" ? whoami.displayName : "this control plane";

  return (
    <section className="view-rail home-view">
      <header className="home-view__head">
        <p className="eyebrow">Culture Nodes</p>
        <h1>{headline(data)}</h1>
        <p className="home-view__lede">{lede(data, greeting)}</p>
      </header>

      {error ? <ErrorNotice error={error} /> : null}

      {data === null && error === null ? (
        <p className="muted">Reading what is waiting on a person…</p>
      ) : null}

      {data && data.cards.length > 0 ? (
        <ul className="home-tickets" aria-label="Tickets with work waiting on a person">
          {data.cards.map((card) => (
            <li className="home-ticket" key={card.key} data-ticket-id={card.key}>
              <h2 className="home-ticket__key">
                <Link to={`/tickets/${encodeURIComponent(card.key)}`}>
                  {card.key}
                </Link>
              </h2>
              <TicketFlowRail flow={card.flow} ticketId={card.key} />
              <p className="home-ticket__go">
                <Link
                  className="home-ticket__cta"
                  to={`/tickets/${encodeURIComponent(card.key)}`}
                >
                  Open {card.key}
                </Link>
              </p>
            </li>
          ))}
        </ul>
      ) : null}

      {data && data.cards.length === 0 && data.untickedRuns === 0 ? (
        <p className="home-view__clear" data-testid="home-nothing-pending">
          Nothing on this control plane is waiting on a person right now.
        </p>
      ) : null}

      {data && data.untickedRuns > 0 ? (
        <p className="muted" data-testid="home-unticketed">
          {data.untickedRuns === 1
            ? "One run has work waiting on a person but records no ticket key"
            : `${data.untickedRuns} runs have work waiting on a person but record no ticket key`}
          , so they have no ticket page. Decide them in the{" "}
          <Link to="/decisions">decision queue</Link>.
        </p>
      ) : null}

      {data && data.unread > 0 ? (
        <p className="muted" data-testid="home-unread">
          {data.unread} more {data.unread === 1 ? "run is" : "runs are"} waiting
          beyond the {RUN_BUDGET} this page reads. The{" "}
          <Link to="/decisions">decision queue</Link> lists every one.
        </p>
      ) : null}

      <section className="home-how" aria-labelledby="home-how-title">
        <h2 id="home-how-title">How a ticket reaches you</h2>
        <p>
          When a run needs a person, the engine comments on its Jira ticket
          with a link straight to that ticket&apos;s page here — so the link
          finds you, you do not go looking for it. If you already know the
          key, the Tickets field in the header opens any ticket directly.
        </p>
        <p className="muted">
          Nothing here can say whose turn it is: a decision is raised for a
          role, not routed to a named person, so every card above says a
          person is needed and any of them can be decided by you.
        </p>
      </section>
    </section>
  );
}

function headline(data: HomeData | null): string {
  if (data === null) return "What is waiting on a person";
  if (data.pendingRuns === 0) return "Nothing is waiting on a person";
  return data.cards.length === 1
    ? "One ticket is waiting on a person"
    : `${data.cards.length || data.pendingRuns} ${
        data.cards.length === 0 ? "runs are" : "tickets are"
      } waiting on a person`;
}

function lede(data: HomeData | null, greeting: string): string {
  if (data === null) {
    return `Signed in as ${greeting}. Reading the pending queues…`;
  }
  if (data.pendingRuns === 0) {
    return `Signed in as ${greeting}. Every decision the engine has raised has been answered.`;
  }
  return `Signed in as ${greeting}. Each ticket below is where the engine stopped and asked for a human verdict.`;
}

/**
 * Assemble the listing from the two pending queues.
 *
 * The order is deliberate and it is the reason this is one function rather
 * than three effects: run ids come from the queues, ticket keys come from the
 * runs, and projections come from the keys — so a partial failure anywhere
 * would otherwise leave the page half-rendered with no way to say which half.
 * A ticket whose projection cannot be read is dropped from the cards and
 * still counted in `pendingRuns`, so the headline never under-reports.
 */
async function load(signal: AbortSignal): Promise<HomeData> {
  const [tasks, decisions] = await Promise.all([
    listHumanTasks(signal, { status: "pending" }),
    listPendingDecisions(signal),
  ]);

  const runIds: string[] = [];
  for (const id of [
    ...tasks.items.map((task) => task.run_id),
    ...decisions.items.map((group) => group.run_id),
  ]) {
    if (!runIds.includes(id)) runIds.push(id);
  }

  const read = runIds.slice(0, RUN_BUDGET);
  const keys: string[] = [];
  let untickedRuns = 0;
  for (const runId of read) {
    const detail = await getRun(runId, signal);
    const key = findTicketKey(detail.run.input);
    if (!key) {
      untickedRuns += 1;
      continue;
    }
    if (!keys.includes(key)) keys.push(key);
  }

  const cards: TicketCard[] = [];
  for (const key of keys) {
    try {
      const projection = await getTicket(key, signal);
      cards.push({ key, projection, flow: ticketFlow(projection) });
    } catch {
      // A ticket whose projection will not read is not a reason to blank the
      // page; it stays in the pending count and out of the cards.
      untickedRuns += 1;
    }
  }

  return {
    cards,
    untickedRuns,
    unread: Math.max(0, runIds.length - RUN_BUDGET),
    pendingRuns: runIds.length,
  };
}

export default Home;
