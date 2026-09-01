import { expect, test } from "@playwright/test";
import {
  DECIDED_TASK,
  LEDGER_VERSION,
  PENDING_TASK,
  PENDING_TASK_MINIMAL,
} from "../src/fixtures/human-tasks-fixture";
import { WHOAMI_ACTOR_ID, WHOAMI_EMAIL } from "../src/fixtures/whoami-fixture";
import { mockInboxApi } from "./fixtures/api";

/**
 * The Inbox, in a browser (task t12).
 *
 * The deciding of *claims* moved to the ticket page in this task; deciding
 * human tasks did not. What changed here is the affordance: the hand-rolled
 * radio fieldset plus "Submit decision" is now the shared `OutcomeButtons`,
 * the same one the Decisions queue and the ticket page offer. A spec covers
 * it because that is a live surface a person uses to route a paused run, and
 * until now nothing walked it end to end in a browser at all.
 */
test("decides a pending human task with one click, at the ledger version it read", async ({ page }) => {
  const captured = await mockInboxApi(page);
  await page.goto("/inbox");

  await expect(page.getByRole("heading", { name: "Inbox" })).toBeVisible();
  await expect(page.locator("#app-header-identity")).toHaveText(
    `signed in as ${WHOAMI_EMAIL}`,
  );
  await expect(page.getByText(/deciding as/i)).toContainText(WHOAMI_ACTOR_ID);

  const card = page.locator(`[data-human-task-id="${PENDING_TASK.id}"]`);
  // The guard version is READ, then shown, then submitted — not fabricated.
  await expect(card.getByText(String(LEDGER_VERSION))).toBeVisible();

  const approve = card.getByRole("button", { name: "approved" });
  await expect(approve).toBeEnabled();
  await approve.click();

  await expect.poll(() => captured.length).toBe(1);
  expect(captured[0]).toEqual({
    url: `/v1alpha1/human-tasks/${PENDING_TASK.id}/decision`,
    method: "POST",
    authorization: undefined,
    body: {
      outcome: "approved",
      decider_actor_id: WHOAMI_ACTOR_ID,
      response: { outcome: "approved" },
      expected_ledger_version: LEDGER_VERSION,
    },
  });
  await expect(card.getByRole("status")).toContainText("decision recorded");
});

test("offers exactly the outcomes the engine accepts, and nothing to type", async ({ page }) => {
  await mockInboxApi(page);
  await page.goto("/inbox");

  const card = page.locator(`[data-human-task-id="${PENDING_TASK.id}"]`);
  const outcomes = card.getByLabel(`Outcomes for ${PENDING_TASK.id}`);
  await expect(outcomes.getByRole("button")).toHaveText([
    "approved",
    "changes_required",
    "rejected",
  ]);
  // No free-text anything: no token panel, no decider field, no JSON payload
  // and no note. Identity comes from whoami (task t9) and the response is
  // derived from the task's own decision schema (task t12).
  await expect(card.locator("input, textarea, select")).toHaveCount(0);
  expect(
    await page.evaluate(() => sessionStorage.length + localStorage.length),
  ).toBe(0);
});

/**
 * `expired` is implied on every approval node's allowed_outcomes, and is the
 * outcome the control plane records when it READS a fact — a merged PR, a
 * passed deadline. `DecideHumanTask` refuses it from a decider (#265), so a
 * button for it was an offer to hand-produce an engine observation.
 */
test("never offers expired, and states the absence when a task offers no choice", async ({ page }) => {
  await mockInboxApi(page, {
    pending: [
      { ...PENDING_TASK, request: { ...PENDING_TASK.request, allowed_outcomes: ["approved", "expired"] } },
      { ...PENDING_TASK_MINIMAL, request: { allowed_outcomes: [] } },
    ],
    decided: [],
  });
  await page.goto("/inbox");

  const rich = page.locator(`[data-human-task-id="${PENDING_TASK.id}"]`);
  await expect(rich.getByRole("button", { name: "approved" })).toBeVisible();
  await expect(rich.getByRole("button", { name: "expired" })).toHaveCount(0);

  const empty = page.locator(`[data-human-task-id="${PENDING_TASK_MINIMAL.id}"]`);
  await expect(empty.getByText("needs an outcome set")).toBeVisible();
  await expect(empty.getByRole("button")).toHaveCount(0);
});

test("shows a decided task read-only under the confirmed-authority chip", async ({ page }) => {
  await mockInboxApi(page);
  await page.goto("/inbox");

  const card = page.locator(`[data-human-task-id="${DECIDED_TASK.id}"]`);
  await expect(card.locator('[data-authority="confirmed"]')).toHaveCount(1);
  await expect(card.getByText(/looks right/)).toBeVisible();
  await expect(card.getByRole("button")).toHaveCount(0);
});
