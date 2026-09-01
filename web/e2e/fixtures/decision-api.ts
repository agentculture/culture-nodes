// Decision-surface API mocks (ticket page, inbox, decisions) — split out of
// api.ts in the login-from-anywhere cycle when that file passed the 1000-line
// guard. Handlers are awaited: registering a route with `void` races the
// page's first requests, which then escape to a real API.
import type { Page } from "@playwright/test";
import { COMMIT_RESULT } from "../../src/fixtures/pending-decisions-fixture";
import { DECIDED_TASK } from "../../src/fixtures/human-tasks-fixture";
import { DECISION_RESULT } from "../../src/fixtures/human-tasks-fixture";
import { LEDGER_VERSION } from "../../src/fixtures/human-tasks-fixture";
import { PENDING_DECISIONS } from "../../src/fixtures/pending-decisions-fixture";
import { PENDING_TASK } from "../../src/fixtures/human-tasks-fixture";
import { PENDING_TASK_MINIMAL } from "../../src/fixtures/human-tasks-fixture";
import { REVIEW_REQUEST } from "../../src/fixtures/pending-decisions-fixture";
import { TICKET_ID } from "../../src/fixtures/ticket-fixture";
import { TICKET_PROJECTION } from "../../src/fixtures/ticket-fixture";
import { TICKET_REVIEWS_RESULT } from "../../src/fixtures/ticket-fixture";
import { WHOAMI_BOUND } from "../../src/fixtures/whoami-fixture";
import { WORKFLOW_DIGEST } from "../../src/fixtures/run-fixture";
import { json, type CapturedRequest } from "./api";

/**
 * The ticket page's slice of `/v1alpha1` (task t12): the projection with its
 * `pending_records`, the reply route, the human-task decision route and the
 * per-run claim-review batch route. Every POST is captured into the returned
 * array in the order it was sent.
 *
 * `ticket` lets a spec serve a different projection (a reload after a
 * conflict, say) without re-registering the route.
 */
export async function mockTicketApi(
  page: Page,
  options: {
    ticket?: () => unknown;
    reviewsResult?: unknown;
    decisionResult?: unknown;
  } = {},
): Promise<CapturedRequest[]> {
  const captured: CapturedRequest[] = [];
  const ticket = options.ticket ?? (() => TICKET_PROJECTION);

  await page.route("**/v1alpha1/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = decodeURIComponent(url.pathname);

    if (request.method() === "POST") {
      captured.push({
        url: path,
        method: request.method(),
        authorization: request.headers().authorization,
        body: request.postDataJSON(),
      });
      if (path.endsWith("/reviews")) {
        await route.fulfill(json(options.reviewsResult ?? TICKET_REVIEWS_RESULT));
        return;
      }
      if (path.endsWith("/decision")) {
        await route.fulfill(json(options.decisionResult ?? DECISION_RESULT));
        return;
      }
      if (path.endsWith("/replies")) {
        await route.fulfill({
          status: 201,
          contentType: "application/json",
          body: JSON.stringify({
            id: "reply-e2e",
            replier: WHOAMI_BOUND.actor_id,
            text: "Ship it",
            created_at: "2026-08-29T10:00:00Z",
          }),
        });
        return;
      }
    }

    if (path === "/v1alpha1/whoami") {
      await route.fulfill(json(WHOAMI_BOUND));
      return;
    }
    if (path === `/v1alpha1/tickets/${TICKET_ID}`) {
      await route.fulfill(json(ticket()));
      return;
    }
    await notFound(route, path);
  });

  return captured;
}

/**
 * The Inbox's slice (task t12): the two listings, each pending run's ledger
 * version (the stale guard the decision is measured against), and the
 * decision route. POSTs are captured.
 */
export async function mockInboxApi(
  page: Page,
  options: { pending?: unknown[]; decided?: unknown[] } = {},
): Promise<CapturedRequest[]> {
  const captured: CapturedRequest[] = [];
  const pending = options.pending ?? [PENDING_TASK, PENDING_TASK_MINIMAL];
  const decided = options.decided ?? [DECIDED_TASK];

  await page.route("**/v1alpha1/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = decodeURIComponent(url.pathname);

    if (request.method() === "POST") {
      captured.push({
        url: path,
        method: request.method(),
        authorization: request.headers().authorization,
        body: request.postDataJSON(),
      });
      await route.fulfill(json(DECISION_RESULT));
      return;
    }

    if (path === "/v1alpha1/whoami") {
      await route.fulfill(json(WHOAMI_BOUND));
      return;
    }
    if (path === "/v1alpha1/human-tasks") {
      const status = url.searchParams.get("status");
      await route.fulfill(json({ items: status === "decided" ? decided : pending }));
      return;
    }
    if (path.endsWith("/ledger")) {
      await route.fulfill(json({ items: [], ledger_version: LEDGER_VERSION }));
      return;
    }
    if (path === "/v1alpha1/events") {
      await emptyStream(route);
      return;
    }
    await notFound(route, path);
  });

  return captured;
}

/**
 * The Decisions view's slice (task t12): the pending-decisions listing both
 * tabs read, an empty human-task listing, the run lookup the Pending tab uses
 * to find each claim group's ticket, and the two review calls the Proposed
 * claims tab makes. POSTs are captured.
 */
export async function mockDecisionsApi(
  page: Page,
  options: { tasks?: unknown[]; ticketKey?: string } = {},
): Promise<CapturedRequest[]> {
  const captured: CapturedRequest[] = [];
  const tasks = options.tasks ?? [];
  const ticketKey = options.ticketKey ?? TICKET_ID;

  await page.route("**/v1alpha1/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = decodeURIComponent(url.pathname);

    if (request.method() === "POST") {
      captured.push({
        url: path,
        method: request.method(),
        authorization: request.headers().authorization,
        body: request.postDataJSON(),
      });
      await route.fulfill(
        path.endsWith("/commit") ? json(COMMIT_RESULT) : json(REVIEW_REQUEST),
      );
      return;
    }

    if (path === "/v1alpha1/whoami") {
      await route.fulfill(json(WHOAMI_BOUND));
      return;
    }
    if (path === "/v1alpha1/pending-decisions") {
      await route.fulfill(json(PENDING_DECISIONS));
      return;
    }
    if (path === "/v1alpha1/human-tasks") {
      await route.fulfill(json({ items: tasks }));
      return;
    }
    if (path.endsWith("/ledger")) {
      await route.fulfill(json({ items: [], ledger_version: 12 }));
      return;
    }
    if (path.startsWith("/v1alpha1/runs/")) {
      const id = path.replace("/v1alpha1/runs/", "");
      await route.fulfill(
        json({
          run: {
            id,
            workflow_digest: WORKFLOW_DIGEST,
            state: "waiting",
            created_at: "2026-08-15T09:00:00Z",
            updated_at: "2026-08-15T09:00:00Z",
            input: { ticket_key: ticketKey },
          },
          tokens: [],
          node_runs: [],
        }),
      );
      return;
    }
    if (path === "/v1alpha1/events") {
      await emptyStream(route);
      return;
    }
    await notFound(route, path);
  });

  return captured;
}

async function emptyStream(route: Parameters<Parameters<Page["route"]>[1]>[0]) {
  await route.fulfill({
    status: 200,
    headers: { "content-type": "text/event-stream", "cache-control": "no-cache" },
    body: "",
  });
}

async function notFound(
  route: Parameters<Parameters<Page["route"]>[1]>[0],
  path: string,
) {
  await route.fulfill({
    status: 404,
    contentType: "application/json",
    body: JSON.stringify({
      code: 1,
      message: `no fixture route for ${path}`,
      remediation: "add it to e2e/fixtures/api.ts",
    }),
  });
}
