import { expect, test } from "@playwright/test";
import {
  CLAIM_LEDGER_VERSION,
  CLAIM_RUN_ID,
  PENDING_RUN,
  REVIEW_REQUEST,
} from "../src/fixtures/pending-decisions-fixture";
import { TICKET_ID } from "../src/fixtures/ticket-fixture";
import { WHOAMI_ACTOR_ID, WHOAMI_EMAIL } from "../src/fixtures/whoami-fixture";
import { mockDecisionsApi } from "./fixtures/api";

/**
 * The Decisions view, in a browser (task t12) — the page the claim-deciding
 * moved FROM.
 *
 * It is not retired (decision c33: a surface is retired only when a better
 * one replaces it in that tab). The cross-ticket queue of proposed claims
 * still decides them here; what went is the Pending tab's inert checkbox,
 * which selected a record into a verdict no form on that tab could submit.
 * Its replacement is a link to the page that CAN take the decision.
 */
test("Pending tab sends a claim group to the ticket page and offers no dead checkbox", async ({ page }) => {
  await mockDecisionsApi(page);
  await page.goto("/decisions");
  await page.getByRole("button", { name: "Pending" }).click();

  await expect(page.getByRole("heading", { name: "Pending decisions" })).toBeVisible();
  await expect(page.getByText(PENDING_RUN.records[0].id)).toBeVisible();
  // The claim's own words, including the qualifying half.
  await expect(page.getByText(/could not run the suite locally/)).toBeVisible();

  await expect(
    page.getByRole("link", { name: `Decide these claims on ticket ${TICKET_ID}` }).first(),
  ).toHaveAttribute("href", `/tickets/${TICKET_ID}`);
  await expect(page.getByRole("checkbox")).toHaveCount(0);
});

test("Proposed claims tab records a per-record verdict as a review naming those records", async ({ page }) => {
  const captured = await mockDecisionsApi(page);
  await page.goto("/decisions");

  await expect(page.getByRole("heading", { name: "Decisions" })).toBeVisible();
  await expect(page.getByText(/reviewing as/i)).toContainText(WHOAMI_EMAIL);

  const card = page.locator(`[data-run-id="${CLAIM_RUN_ID}"]`);
  await expect(card.getByText(`ledger version ${CLAIM_LEDGER_VERSION}`)).toBeVisible();

  // The claim holds; the evidence is process-reported, so it does not.
  await card
    .getByRole("group", { name: `Verdict for ${PENDING_RUN.records[1].id}` })
    .getByRole("radio", { name: "reject" })
    .check();

  const submit = card.getByRole("button", { name: "Record decision" });
  await expect(submit).toBeDisabled(); // a decision with no stated reason stays refused
  await card
    .getByLabel("Why (recorded on the decision)")
    .fill("re-ran the suite on spark and read the output");
  await expect(submit).toBeEnabled();
  await submit.click();

  await expect.poll(() => captured.length).toBe(2);
  expect(captured[0]).toEqual({
    url: `/v1alpha1/runs/${CLAIM_RUN_ID}/reviews`,
    method: "POST",
    authorization: undefined,
    body: {
      record_ids: PENDING_RUN.records.map((record) => record.id),
      // The version this page READ, never a fresh fetch: that is what makes
      // the stale guard mean anything.
      ledger_version: CLAIM_LEDGER_VERSION,
      reviewer_actor_id: WHOAMI_ACTOR_ID,
    },
  });
  expect(captured[1]).toEqual({
    url: `/v1alpha1/reviews/${REVIEW_REQUEST.id}/commit`,
    method: "POST",
    authorization: undefined,
    body: {
      decisions: {
        [PENDING_RUN.records[0].id]: "confirm",
        [PENDING_RUN.records[1].id]: "reject",
      },
      expected_ledger_version: CLAIM_LEDGER_VERSION,
      rationale: "re-ran the suite on spark and read the output",
    },
  });

  // A review names records; it never rewrites them (PRD §10.8).
  const decided = card.locator(`[data-record-id="${PENDING_RUN.records[0].id}"]`);
  await expect(decided.locator('[data-authority="proposed"]')).toHaveCount(1);
  await expect(decided.locator('[data-authority="confirmed"]')).toHaveCount(1);
  await expect(page.locator("#decisions-recorded")).toContainText(REVIEW_REQUEST.id);
});

test("holds nothing in the browser: no token, no remembered actor id", async ({ page }) => {
  await mockDecisionsApi(page);
  await page.goto("/decisions");
  await expect(page.getByRole("heading", { name: "Decisions" })).toBeVisible();

  await expect(page.locator('input[type="password"]')).toHaveCount(0);
  expect(
    await page.evaluate(() => sessionStorage.length + localStorage.length),
  ).toBe(0);
});
