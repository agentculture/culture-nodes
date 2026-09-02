import { expect, test } from "@playwright/test";
import {
  PENDING_TASK_ID,
  SECOND_TICKET_CLAIM_RECORD_ID,
  SECOND_TICKET_RUN_ID,
  SECOND_TICKET_RUN_LEDGER_VERSION,
  TICKET_CLAIM_RECORD_ID,
  TICKET_EVIDENCE_RECORD_ID,
  TICKET_ID,
  TICKET_PROJECTION,
  TICKET_RUN_ID,
  TICKET_RUN_LEDGER_VERSION,
} from "../src/fixtures/ticket-fixture";
import { WHOAMI_ACTOR_ID, WHOAMI_EMAIL } from "../src/fixtures/whoami-fixture";
import { mockTicketApi, mockWhoami } from "./fixtures/api";

/**
 * The keyboard walk (task t18), re-cut for the derived identity model (task
 * t9, spec c8/c9): nothing is typed but the reply. The page learns who is
 * here from `GET /v1alpha1/whoami` — behind Cloudflare Access the edge
 * cookie carries the identity on every same-origin request — so the reply
 * names whoami's actor, no Authorization header is sent, and no token is
 * held anywhere in the browser.
 */
test("keyboard walk reaches the reply box and submits as the signed-in actor", async ({ page }) => {
  let submitted: { authorization?: string; body?: unknown } | undefined;
  await mockWhoami(page);
  await page.route("**/v1alpha1/tickets/**", async (route) => {
    if (route.request().method() === "POST") {
      submitted = {
        authorization: route.request().headers().authorization,
        body: route.request().postDataJSON(),
      };
      await route.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: "reply-e2e", replier: WHOAMI_ACTOR_ID, text: "Ship it", created_at: "2026-08-29T10:00:00Z" }) });
      return;
    }
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(TICKET_PROJECTION) });
  });
  await page.goto(`/tickets/${TICKET_ID}`);
  await expect(page.getByRole("heading", { name: TICKET_ID })).toBeVisible();
  await expect(page.locator("#app-header-identity")).toHaveText(`signed in as ${WHOAMI_EMAIL}`);
  await expect(page.locator("#ticket-token")).toHaveCount(0);
  await expect(page.locator("#ticket-replier")).toHaveCount(0);
  await expect(page.locator('input[type="password"]')).toHaveCount(0);

  // The reply thread and its form live behind the Conversation tab since
  // task t17 — the first screen is the flow rail and the pending decision.
  await page.getByRole("tab", { name: "Conversation" }).click();

  await page.keyboard.press("Tab");
  const focused = page.locator("*:focus");
  for (let count = 0; count < 40; count += 1) {
    if ((await focused.count()) > 0 && (await focused.first().getAttribute("id")) === "ticket-reply") break;
    await page.keyboard.press("Tab");
  }
  await expect(page.locator("#ticket-reply")).toBeFocused();
  await page.keyboard.type("Ship it");
  await page.keyboard.press("Tab");
  await page.keyboard.press("Enter");

  await expect(page.getByRole("status")).toHaveText("Reply sent.");
  expect(submitted).toEqual({
    authorization: undefined,
    body: { id: expect.stringMatching(/^[A-Za-z0-9_-]{8,64}$/), replier: WHOAMI_ACTOR_ID, text: "Ship it" },
  });
  expect(await page.evaluate(() => sessionStorage.length + localStorage.length)).toBe(0);
});

test("reduced motion disables ticket-route transitions and animations", async ({ page }) => {
  await page.emulateMedia({ reducedMotion: "reduce" });
  await mockWhoami(page);
  await page.route("**/v1alpha1/tickets/**", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(TICKET_PROJECTION) }));
  await page.goto(`/tickets/${TICKET_ID}`);
  const state = page.locator(".ticket-state").first();
  await expect(state).toHaveCSS("animation-name", "none");
  await expect(state).toHaveCSS("transition-duration", "0s");
});

/**
 * The ticket page decides everything (task t12, spec c11, decision c40).
 *
 * Two walks, both against the intercepted API, both asserting the REQUEST
 * rather than the page's own confirmation text — a surface that renders
 * "decision recorded" over a request that never carried the version it was
 * rendered from is exactly the failure the stale guard exists to catch.
 */
test("decides a human task on the ticket page, at the version the API served with it", async ({ page }) => {
  const captured = await mockTicketApi(page);
  await page.goto(`/tickets/${TICKET_ID}`);
  await expect(page.getByRole("heading", { name: "Decisions", exact: true })).toBeVisible();

  const outcomes = page.getByLabel(`Outcomes for ${PENDING_TASK_ID}`);
  await expect(outcomes.getByRole("button", { name: "approved" })).toBeEnabled();
  // `expired` is implied on every approval node and is never offered: it is
  // what the control plane records when it READS a fact (#265).
  await expect(outcomes.getByRole("button", { name: "expired" })).toHaveCount(0);
  await outcomes.getByRole("button", { name: "approved" }).click();

  await expect.poll(() => captured.length).toBe(1);
  expect(captured[0]).toEqual({
    url: `/v1alpha1/human-tasks/${PENDING_TASK_ID}/decision`,
    method: "POST",
    authorization: undefined,
    body: {
      outcome: "approved",
      decider_actor_id: WHOAMI_ACTOR_ID,
      response: { outcome: "approved" },
      // The version the API SERVED with the task, not one re-read here.
      expected_ledger_version: TICKET_RUN_LEDGER_VERSION,
    },
  });
});

test("confirms a claim on the ticket page and shows each run's own outcome", async ({ page }) => {
  const captured = await mockTicketApi(page);
  await page.goto(`/tickets/${TICKET_ID}`);
  await expect(
    page.getByRole("heading", { name: "Claims awaiting a decision" }),
  ).toBeVisible();

  const firstGroup = page.locator(`[data-run-id="${TICKET_RUN_ID}"]`);
  await expect(
    firstGroup.getByText(`ledger version ${TICKET_RUN_LEDGER_VERSION}`),
  ).toBeVisible();
  // The qualifying half of the claim is on screen: a decision on a claim
  // nobody read is what this surface exists to prevent.
  await expect(firstGroup.getByText(/the board half is unproven/)).toBeVisible();

  // Reject the evidence, leave the claim confirmed, drop the other run out.
  await firstGroup
    .getByRole("group", { name: `Verdict for ${TICKET_EVIDENCE_RECORD_ID}` })
    .getByRole("radio", { name: "reject" })
    .check();
  await page
    .locator(`[data-run-id="${SECOND_TICKET_RUN_ID}"]`)
    .getByRole("group", { name: `Verdict for ${SECOND_TICKET_CLAIM_RECORD_ID}` })
    .getByRole("radio", { name: "not now" })
    .check();

  const submit = page.getByRole("button", { name: "Record decisions" });
  await expect(submit).toBeDisabled(); // no rationale stated yet
  await page.getByLabel("Why (recorded on every decision)").fill("read the qualification");
  await expect(submit).toBeEnabled();
  await submit.click();

  await expect.poll(() => captured.length).toBe(1);
  expect(captured[0]).toEqual({
    url: `/v1alpha1/tickets/${TICKET_ID}/reviews`,
    method: "POST",
    authorization: undefined,
    body: {
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
    },
  });

  // Per-run results, inline. The fixture's second run moved under the
  // decider, so it reports conflict and offers to reload just that group.
  const committed = page.getByTestId(`review-result-${TICKET_RUN_ID}`);
  await expect(committed).toContainText("decision recorded");
  await expect(committed).toContainText("a review names them, it never rewrites them");

  // The confirmed record still reads proposed, with the review beside it.
  const decidedRecord = firstGroup.locator(`[data-record-id="${TICKET_CLAIM_RECORD_ID}"]`);
  await expect(decidedRecord.locator('[data-authority="proposed"]')).toHaveCount(1);
  await expect(decidedRecord.locator('[data-authority="confirmed"]')).toHaveCount(1);
});

test("re-reads only the conflicted group when its reload is used", async ({ page }) => {
  let served = TICKET_PROJECTION;
  const captured = await mockTicketApi(page, { ticket: () => served });
  await page.goto(`/tickets/${TICKET_ID}`);
  await page.getByLabel("Why (recorded on every decision)").fill("read both claims");
  await page.getByRole("button", { name: "Record decisions" }).click();

  const conflicted = page.getByTestId(`review-result-${SECOND_TICKET_RUN_ID}`);
  await expect(conflicted).toContainText("conflict");
  expect(captured).toHaveLength(1);

  const moved = structuredClone(TICKET_PROJECTION);
  moved.pending_records![1].ledger_version = SECOND_TICKET_RUN_LEDGER_VERSION + 5;
  served = moved;
  await conflicted.getByRole("button", { name: "Reload this group" }).click();

  const secondGroup = page.locator(`[data-run-id="${SECOND_TICKET_RUN_ID}"]`);
  await expect(
    secondGroup.getByText(`ledger version ${SECOND_TICKET_RUN_LEDGER_VERSION + 5}`),
  ).toBeVisible();
  // The neighbouring run's committed result is untouched by the reload.
  await expect(page.getByTestId(`review-result-${TICKET_RUN_ID}`)).toContainText(
    "decision recorded",
  );
});

/**
 * Frame claims come from the custody checkout, and this page does not run
 * devague confirm (spec c13, honesty condition h20). It says what state each
 * claim arrived in and offers nothing to change it.
 */
test("renders frame claims read-only, with their confirmation state", async ({ page }) => {
  await mockTicketApi(page);
  await page.goto(`/tickets/${TICKET_ID}`);
  const claims = page.locator("#ticket-frame-claims");
  await expect(claims.getByRole("heading", { name: "Frame claims" })).toBeVisible();
  await expect(claims.getByText(/confirmed in the custody checkout/)).toBeVisible();
  await expect(claims.locator("[data-claim-id]")).toHaveCount(3);
  await expect(claims.locator("input, select, textarea, button")).toHaveCount(0);
});
