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

  await expect(page.locator("#ledger-version")).toHaveText("5");
  await expect(page.locator("#ledger-table tbody tr")).toHaveCount(5);

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
  await page.goto("/");
  await expect(page).toHaveURL(/\/runs$/);
  await expect
    .poll(async () => (await readAgentState(page)).status)
    .toBe("ready");
  await expect(page.locator(`#runs-table tr[data-run-id="${RUN_ID}"]`)).toBeVisible();
  await page.getByRole("link", { name: RUN_ID }).click();
  await expect(page).toHaveURL(new RegExp(`/runs/${RUN_ID}$`));
});
