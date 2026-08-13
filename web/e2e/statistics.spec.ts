import { expect, test, type Page } from "@playwright/test";
import { mockStatisticsApi, readAgentState } from "./fixtures/api";

test.beforeEach(async ({ page }) => {
  await mockStatisticsApi(page);
});

async function openStatistics(page: Page) {
  await page.goto("/stats");
  await expect
    .poll(async () => (await readAgentState(page)).status)
    .toBe("ready");
}

test("states the denominator: total, reported, and excluded runs, with excluded visible", async ({
  page,
}) => {
  await openStatistics(page);

  const denominator = page.locator("#statistics-denominator");
  await expect(denominator).toContainText("5 runs in this window");
  await expect(denominator).toContainText("4 reported usage");

  const excluded = page.locator("#statistics-excluded");
  await expect(excluded).toContainText("1 excluded (usage never reported)");
});

test("renders total tokens, average and median tokens per run, computed over the full paginated window", async ({
  page,
}) => {
  await openStatistics(page);

  await expect(page.locator("#stat-total-usage")).toContainText("7.5k in");
  await expect(page.locator("#stat-tile-avg-tokens")).toContainText("1.9k in");
  await expect(page.locator("#stat-tile-median-tokens")).toContainText(
    "1.8k in",
  );
  await expect(page.locator("#stat-tile-avg-cost")).toContainText("3.75");
  await expect(page.locator("#stat-tile-avg-cost")).toContainText("3.50");
});

test("renders a per-category breakdown with an uncategorized bucket, honoring the same denominator", async ({
  page,
}) => {
  await openStatistics(page);

  const ciRow = page.locator('#category-stats-table tr[data-category="ci"]');
  await expect(ciRow).toBeVisible();
  await expect(ciRow.locator('[data-stat="category-total-runs"]')).toHaveText(
    "3",
  );
  await expect(
    ciRow.locator('[data-stat="category-reported-runs"]'),
  ).toHaveText("2");
  await expect(
    ciRow.locator('[data-stat="category-excluded-runs"]'),
  ).toHaveText("1");

  const uncategorizedRow = page.locator(
    '#category-stats-table tr[data-category="uncategorized"]',
  );
  await expect(uncategorizedRow).toContainText("Uncategorized");
});

test("registers totals and the denominator in #agent-state", async ({
  page,
}) => {
  await openStatistics(page);
  const state = await readAgentState(page);
  expect(state.statistics).toMatchObject({
    total_runs: 5,
    reported_runs: 4,
    excluded_runs: 1,
    total_input_tokens: 7500,
    total_output_tokens: 3750,
    cost_currency: "USD",
    avg_cost: 3.75,
    median_cost: 3.5,
  });
});

test("selecting a time-range preset re-requests and changes the aggregate", async ({
  page,
}) => {
  await openStatistics(page);
  await expect(page.locator("#statistics-denominator")).toContainText(
    "5 runs in this window",
  );

  const request = page.waitForRequest((req) =>
    req.url().includes("/v1alpha1/node-runs"),
  );
  await page.getByRole("button", { name: "Last hour" }).click();
  const requestUrl = new URL((await request).url());
  expect(requestUrl.searchParams.get("updated_since")).toBeTruthy();

  // The fixture answers a bounded request with a different, smaller
  // dataset (STATS_NODE_RUNS_FILTERED) — the aggregate must move.
  await expect(page.locator("#statistics-denominator")).toContainText(
    "1 run in this window",
  );
  await expect(page.locator("#statistics-denominator")).not.toContainText(
    "5 runs in this window",
  );
});

test("the header's Statistics link reaches /stats from the run list, and back", async ({
  page,
}) => {
  await page.goto("/runs");
  await page.getByRole("link", { name: "Statistics", exact: true }).click();
  await expect(page).toHaveURL(/\/stats$/);
  await page.getByRole("link", { name: "Runs", exact: true }).click();
  await expect(page).toHaveURL(/\/runs$/);
});

test("the skip link is still the first tab stop, unaffected by the new route", async ({
  page,
}) => {
  await openStatistics(page);
  await page.keyboard.press("Tab");
  const skipLink = page.locator(".skip-link");
  await expect(skipLink).toBeFocused();
  await expect(skipLink).toHaveAttribute("href", "#main");
});

test("the page produces no uncaught errors while rendering or filtering", async ({
  page,
}) => {
  const pageErrors: string[] = [];
  page.on("pageerror", (error) => pageErrors.push(error.message));
  await openStatistics(page);
  await page.getByRole("button", { name: "Last 24h" }).click();
  await page.waitForLoadState("networkidle");
  expect(pageErrors).toEqual([]);
});
