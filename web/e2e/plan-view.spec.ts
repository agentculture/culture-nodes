import { expect, test } from "@playwright/test";
import { mockPlanApi, PLAN_SLUG, readAgentState } from "./fixtures/api";

test.beforeEach(async ({ page }) => {
  await mockPlanApi(page);
});

test("renders the plan's waves, per-task status, and real dependency edges (task t23)", async ({
  page,
}) => {
  await page.goto(`/plan/${PLAN_SLUG}`);
  await expect
    .poll(async () => (await readAgentState(page)).status)
    .toBe("ready");

  // Two real waves plus the unscheduled (rejected) bucket.
  await expect(page.locator("#plan-waves .plan-wave")).toHaveCount(3);
  await expect(
    page.locator('.plan-wave[data-wave="1"] .plan-task'),
  ).toHaveCount(2);
  await expect(
    page.locator('.plan-wave[data-wave="2"] .plan-task'),
  ).toHaveCount(2);
  await expect(page.locator("#plan-unscheduled")).toBeVisible();

  // t3's real dependency is only t1, never widened to "everything in the
  // previous wave" (spec c15) — t2 is in t1's wave but not t3's deps line.
  const t3Deps = page.locator(
    '.plan-task[data-task-ref="t3"] .plan-task__deps',
  );
  await expect(t3Deps).toContainText("t1");
  await expect(t3Deps).not.toContainText("t2");

  // Per-task status via the AuthorityChip vocabulary: t1 confirmed
  // (solid), t2 proposed (dashed) — not flattened to one plan-level value.
  const t1Chip = page.locator(
    '.plan-task[data-task-ref="t1"] .authority-chip',
  );
  await expect(t1Chip).toHaveAttribute("data-authority", "confirmed");
  await expect(t1Chip).toHaveCSS("border-style", "solid");
  const t2Chip = page.locator(
    '.plan-task[data-task-ref="t2"] .authority-chip',
  );
  await expect(t2Chip).toHaveAttribute("data-authority", "proposed");
  await expect(t2Chip).toHaveCSS("border-style", "dashed");
});

test("distinguishes deviation origin (user vs llm) at a glance, using the AuthorityChip vocabulary", async ({
  page,
}) => {
  await page.goto(`/plan/${PLAN_SLUG}`);
  await expect
    .poll(async () => (await readAgentState(page)).status)
    .toBe("ready");

  await expect(page.locator("#plan-deviations-table tbody tr")).toHaveCount(3);

  // d1: origin "human" (the user reported it) — SOLID/confirmed.
  const d1Row = page.locator('tr[data-deviation-ref="d1"]');
  await expect(d1Row).toHaveAttribute("data-origin", "human");
  const d1OriginChip = d1Row.locator(".plan-origin .authority-chip");
  await expect(d1OriginChip).toHaveAttribute("data-authority", "confirmed");
  await expect(d1OriginChip).toHaveCSS("border-style", "solid");
  await expect(d1Row).toContainText("user reports");

  // d2: origin "agent" (the system derived it) — DASHED/proposed, visibly
  // distinct styling from d1 even before either label is read.
  const d2Row = page.locator('tr[data-deviation-ref="d2"]');
  await expect(d2Row).toHaveAttribute("data-origin", "agent");
  const d2OriginChip = d2Row.locator(".plan-origin .authority-chip");
  await expect(d2OriginChip).toHaveAttribute("data-authority", "proposed");
  await expect(d2OriginChip).toHaveCSS("border-style", "dashed");
  await expect(d2Row).toContainText("system knows");
});

test("shows an honest empty state for a plan slug with no imports", async ({
  page,
}) => {
  await page.goto("/plan/never-imported");
  await expect
    .poll(async () => (await readAgentState(page)).status)
    .toBe("ready");
  await expect(page.locator("#plan-view-not-found")).toBeVisible();
});

test("prompts for a slug via the nav link, and the slug form navigates to /plan/:slug", async ({
  page,
}) => {
  await page.goto("/");
  await page.getByRole("link", { name: "Plan" }).click();
  await expect(page).toHaveURL(/\/plan$/);
  await expect(page.locator("#plan-view-empty")).toBeVisible();

  await page.locator("#plan-slug-input").fill(PLAN_SLUG);
  await page.getByRole("button", { name: "Go" }).click();
  await expect(page).toHaveURL(new RegExp(`/plan/${PLAN_SLUG}$`));
  await expect
    .poll(async () => (await readAgentState(page)).status)
    .toBe("ready");
  await expect(page.locator("#plan-waves")).toBeVisible();
});
