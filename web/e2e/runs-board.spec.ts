import { expect, test, type Page } from "@playwright/test";
import { BOARD_RUNS, mockRunsBoardApi, readAgentState } from "./fixtures/api";

test.beforeEach(async ({ page }) => {
  await mockRunsBoardApi(page);
});

async function openBoard(page: Page) {
  await page.goto("/board");
  await expect
    .poll(async () => (await readAgentState(page)).status)
    .toBe("ready");
}

const RUN_STATE_COLUMNS = [
  "created",
  "running",
  "waiting",
  "completed",
  "failed",
  "cancelled",
] as const;

test("renders one column per RunState, each fixture run under its own committed state", async ({
  page,
}) => {
  await openBoard(page);

  for (const state of RUN_STATE_COLUMNS) {
    await expect(page.locator(`[data-column-state="${state}"]`)).toHaveCount(1);
  }
  // Never an invented column, and never the abbreviated "queued" word from
  // the acceptance criteria's shorthand — the real RunState vocabulary only.
  await expect(page.locator("[data-column-state]")).toHaveCount(6);
  await expect(page.getByText("queued", { exact: false })).toHaveCount(0);

  for (const run of BOARD_RUNS) {
    await expect(
      page.locator(
        `[data-column-state="${run.state}"] [data-run-id="${run.id}"]`,
      ),
    ).toHaveCount(1);
  }
});

test("the approval-paused run lands under waiting, alongside any other wait", async ({
  page,
}) => {
  await openBoard(page);

  const approvalPaused = BOARD_RUNS.find((run) =>
    run.id.startsWith("run-waiting-approval"),
  );
  expect(approvalPaused).toBeDefined();
  await expect(
    page.locator(
      `[data-column-state="waiting"] [data-run-id="${approvalPaused!.id}"]`,
    ),
  ).toHaveCount(1);
});

test("requests runs sorted by updated_at (t11 params)", async ({ page }) => {
  const request = page.waitForRequest((req) =>
    req.url().includes("/v1alpha1/runs?"),
  );
  await openBoard(page);
  const url = new URL((await request).url());
  expect(url.searchParams.get("sort")).toBe("updated_at");
});

test("every card names its run, state and update time, and is a real link", async ({
  page,
}) => {
  await openBoard(page);
  const run = BOARD_RUNS[0];
  const card = page.locator(`[data-run-id="${run.id}"]`);
  await expect(card).toHaveAttribute("href", `/runs/${run.id}`);
  await expect(card).toHaveAccessibleName(
    new RegExp(`^run ${run.id}, ${run.state}, updated `),
  );
});

test("keyboard-only: tab to a card, Enter opens its own run view", async ({
  page,
}) => {
  await openBoard(page);
  const run = BOARD_RUNS[0];
  const card = page.locator(`[data-run-id="${run.id}"]`);
  await card.focus();
  await expect(card).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(new RegExp(`/runs/${run.id}$`));
});

test("the skip link is still the first tab stop, unaffected by the new route", async ({
  page,
}) => {
  await openBoard(page);
  await page.keyboard.press("Tab");
  const skipLink = page.locator(".skip-link");
  await expect(skipLink).toBeFocused();
  await expect(skipLink).toHaveAttribute("href", "#main");
});

test("the header's Board link reaches /board from the run list, and back", async ({
  page,
}) => {
  await page.goto("/runs");
  // exact: true — several of the board fixture's own run ids contain the
  // substring "BOARD" and would otherwise match a loose name lookup too.
  await page.getByRole("link", { name: "Board", exact: true }).click();
  await expect(page).toHaveURL(/\/board$/);
  await page.getByRole("link", { name: "Runs", exact: true }).click();
  await expect(page).toHaveURL(/\/runs$/);
});

test("the page produces no uncaught errors while rendering the board", async ({
  page,
}) => {
  const pageErrors: string[] = [];
  page.on("pageerror", (error) => pageErrors.push(error.message));
  await openBoard(page);
  expect(pageErrors).toEqual([]);
});

test.describe("reduced motion", () => {
  // Emulates `prefers-reduced-motion: reduce` for this block's contexts.
  test.use({ contextOptions: { reducedMotion: "reduce" } });

  test("drops the pulse on the running card and states the same fact in text", async ({
    page,
  }) => {
    await openBoard(page);
    const runningRun = BOARD_RUNS.find((run) => run.state === "running")!;
    const card = page.locator(`[data-run-id="${runningRun.id}"]`);
    await expect(card).not.toHaveClass(/is-pulse/);
    await expect(page.locator(".is-pulse")).toHaveCount(0);
    await expect(card).toContainText("updating live");
  });
});
