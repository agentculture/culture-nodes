import { expect, test, type Page } from "@playwright/test";
import {
  JOB_RUNS_CURSOR,
  JOB_RUNS_PAGE_1,
  JOB_RUNS_PAGE_2,
  mockJobsTimelineApi,
  readAgentState,
} from "./fixtures/api";

test.beforeEach(async ({ page }) => {
  await mockJobsTimelineApi(page);
});

async function openJobs(page: Page) {
  await page.goto("/jobs");
  await expect
    .poll(async () => (await readAgentState(page)).status)
    .toBe("ready");
}

test("renders one row per node run, newest first, from the API only", async ({
  page,
}) => {
  await openJobs(page);

  const rows = page.locator("#jobs-table tbody tr");
  await expect(rows).toHaveCount(JOB_RUNS_PAGE_1.length);

  for (const item of JOB_RUNS_PAGE_1) {
    await expect(
      page.locator(`[data-node-run-id="${item.id}"]`),
    ).toHaveCount(1);
  }
  // Newest first: row order matches the fixture's own (already newest-first) order.
  const rowLocators = await rows.all();
  const rowIds = await Promise.all(
    rowLocators.map((row) => row.getAttribute("data-node-run-id")),
  );
  expect(rowIds).toEqual(JOB_RUNS_PAGE_1.map((item) => item.id));
});

test("each row names run, node, actor/runner, state, outcome, started and updated", async ({
  page,
}) => {
  await openJobs(page);
  const item = JOB_RUNS_PAGE_1[2]; // the failed/test row: has actor and outcome
  const row = page.locator(`[data-node-run-id="${item.id}"]`);

  await expect(row.getByRole("link", { name: item.run_id })).toHaveAttribute(
    "href",
    `/runs/${item.run_id}`,
  );
  await expect(row).toContainText(item.node_id);
  await expect(row).toContainText(item.actor_id!);
  await expect(row.locator(".status-chip__label")).toHaveText(item.state);
  await expect(row).toContainText(item.outcome!);
  await expect(row.locator("time")).toHaveCount(2);
});

test("requests node-runs with no time bound by default", async ({ page }) => {
  const request = page.waitForRequest((req) =>
    req.url().includes("/v1alpha1/node-runs"),
  );
  await openJobs(page);
  const url = new URL((await request).url());
  expect(url.searchParams.has("updated_since")).toBe(false);
  expect(url.searchParams.has("updated_until")).toBe(false);
});

test("selecting a preset updates the URL search params and drives updated_since on the outgoing request", async ({
  page,
}) => {
  await openJobs(page);

  const request = page.waitForRequest((req) =>
    req.url().includes("/v1alpha1/node-runs"),
  );
  await page.getByRole("button", { name: "Last hour" }).click();
  const requestUrl = new URL((await request).url());
  const updatedSince = requestUrl.searchParams.get("updated_since");
  expect(updatedSince).toBeTruthy();

  // The URL search params carry the exact same value — shareable/bookmarkable.
  const pageUrl = new URL(page.url());
  expect(pageUrl.searchParams.get("since")).toBe(updatedSince);

  await expect(
    page.getByRole("button", { name: "Last hour" }),
  ).toHaveAttribute("aria-pressed", "true");
});

test("a bookmarked since/until URL drives the initial request directly", async ({
  page,
}) => {
  const request = page.waitForRequest((req) =>
    req.url().includes("/v1alpha1/node-runs"),
  );
  await page.goto(
    "/jobs?since=2026-08-01T00%3A00%3A00.000Z&until=2026-08-02T00%3A00%3A00.000Z",
  );
  await expect
    .poll(async () => (await readAgentState(page)).status)
    .toBe("ready");

  const url = new URL((await request).url());
  expect(url.searchParams.get("updated_since")).toBe(
    "2026-08-01T00:00:00.000Z",
  );
  expect(url.searchParams.get("updated_until")).toBe(
    "2026-08-02T00:00:00.000Z",
  );
});

test("a custom since/until range updates the URL and the outgoing request with both bounds", async ({
  page,
}) => {
  await openJobs(page);

  await page.getByRole("button", { name: "Custom" }).click();
  await page.getByLabel("Since").fill("2026-08-01T09:00");
  await page.getByLabel("Until").fill("2026-08-02T09:00");

  const request = page.waitForRequest((req) =>
    req.url().includes("/v1alpha1/node-runs"),
  );
  await page.getByRole("button", { name: "Apply" }).click();
  const requestUrl = new URL((await request).url());
  expect(requestUrl.searchParams.get("updated_since")).toBeTruthy();
  expect(requestUrl.searchParams.get("updated_until")).toBeTruthy();

  const pageUrl = new URL(page.url());
  expect(pageUrl.searchParams.get("since")).toBe(
    requestUrl.searchParams.get("updated_since"),
  );
  expect(pageUrl.searchParams.get("until")).toBe(
    requestUrl.searchParams.get("updated_until"),
  );
});

test("Load more appends the next page and carries the cursor on the request", async ({
  page,
}) => {
  await openJobs(page);
  await expect(page.locator("#jobs-table tbody tr")).toHaveCount(
    JOB_RUNS_PAGE_1.length,
  );

  const request = page.waitForRequest((req) =>
    req.url().includes("/v1alpha1/node-runs") &&
    req.url().includes("cursor="),
  );
  await page.getByRole("button", { name: "Load more" }).click();
  const requestUrl = new URL((await request).url());
  expect(requestUrl.searchParams.get("cursor")).toBe(JOB_RUNS_CURSOR);

  await expect(page.locator("#jobs-table tbody tr")).toHaveCount(
    JOB_RUNS_PAGE_1.length + JOB_RUNS_PAGE_2.length,
  );
  for (const item of JOB_RUNS_PAGE_2) {
    await expect(
      page.locator(`[data-node-run-id="${item.id}"]`),
    ).toHaveCount(1);
  }
  // The second page has no next_cursor — the button must not linger.
  await expect(
    page.getByRole("button", { name: "Load more" }),
  ).toHaveCount(0);
});

test("keyboard-only: tab to a preset, Enter applies it; tab into a row link, Enter follows it", async ({
  page,
}) => {
  await openJobs(page);

  await page.getByRole("button", { name: "Last hour" }).focus();
  await expect(
    page.getByRole("button", { name: "Last hour" }),
  ).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(
    page.getByRole("button", { name: "Last hour" }),
  ).toHaveAttribute("aria-pressed", "true");

  const item = JOB_RUNS_PAGE_1[0];
  const link = page
    .locator(`[data-node-run-id="${item.id}"]`)
    .getByRole("link", { name: item.run_id });
  await link.focus();
  await expect(link).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(new RegExp(`/runs/${item.run_id}$`));
});

test("the skip link is still the first tab stop, unaffected by the new route", async ({
  page,
}) => {
  await openJobs(page);
  await page.keyboard.press("Tab");
  const skipLink = page.locator(".skip-link");
  await expect(skipLink).toBeFocused();
  await expect(skipLink).toHaveAttribute("href", "#main");
});

test("the header's Jobs link reaches /jobs from the run list, and back", async ({
  page,
}) => {
  await page.goto("/runs");
  await page.getByRole("link", { name: "Jobs", exact: true }).click();
  await expect(page).toHaveURL(/\/jobs$/);
  await page.getByRole("link", { name: "Runs", exact: true }).click();
  await expect(page).toHaveURL(/\/runs$/);
});

test("the page produces no uncaught errors while rendering, filtering, or paging", async ({
  page,
}) => {
  const pageErrors: string[] = [];
  page.on("pageerror", (error) => pageErrors.push(error.message));
  await openJobs(page);
  await page.getByRole("button", { name: "Last 24h" }).click();
  await page.waitForLoadState("networkidle");
  expect(pageErrors).toEqual([]);
});
