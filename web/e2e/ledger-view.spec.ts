import { expect, test } from "@playwright/test";
import { RUN_ID, mockApi, readAgentState } from "./fixtures/api";

test.beforeEach(async ({ page }) => {
  await mockApi(page);
});

test("lists the run's ledger records with authority rendered dashed/solid", async ({
  page,
}) => {
  await page.goto(`/runs/${RUN_ID}/ledger`);
  await expect
    .poll(async () => (await readAgentState(page)).status)
    .toBe("ready");

  await expect(page.locator("#ledger-version")).toHaveText("6");
  await expect(page.locator("#ledger-table tbody tr")).toHaveCount(6);

  // An agent's own claim: dashed, because nobody has confirmed it.
  const proposed = page
    .locator('.authority-chip[data-authority="proposed"]')
    .first();
  await expect(proposed).toHaveAttribute("data-edge-style", "DASHED");
  await expect(proposed).toHaveCSS("border-style", "dashed");

  // A human confirmation: solid.
  const confirmed = page.locator('.authority-chip[data-authority="confirmed"]');
  await expect(confirmed).toHaveAttribute("data-edge-style", "SOLID");
  await expect(confirmed).toHaveCSS("border-style", "solid");
});

test("computes a projection from the picker", async ({ page }) => {
  await page.goto(`/runs/${RUN_ID}/ledger`);
  await page.locator("#projection-select").selectOption("confirmed_claims");
  await expect(page.locator("#projection-table")).toBeVisible();
  await expect(page.locator("#projection-digest")).toContainText(
    "sha256:projection-confir",
  );
  await expect(page.locator("#projection-table tbody tr")).toHaveCount(1);
});

test("the run list links into the run view", async ({ page }) => {
  // `/` is the first-visit page for a signed-in person since task t17, so
  // this walk starts at the run list itself. What `/` does is e2e/home.spec.ts.
  await page.goto("/runs");
  await expect
    .poll(async () => (await readAgentState(page)).status)
    .toBe("ready");
  const row = page.locator(`#runs-table tr[data-run-id="${RUN_ID}"]`);
  await expect(row).toBeVisible();
  // The fixture run has a name (task t5), so the row's link text is its
  // name, not the bare id — click the row's link itself rather than
  // matching by accessible name.
  await row.getByRole("link").click();
  await expect(page).toHaveURL(new RegExp(`/runs/${RUN_ID}$`));
});
