import { expect, test, type Page } from "@playwright/test";
import { BOARD_RUNS, mockRunsBoardApi, readAgentState } from "./fixtures/api";
import { snapshotMarkerSse } from "./fixtures/api";

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

test.describe("auto-refresh (issue #46, task t30)", () => {
  // Serve one committed run.created event over the shared cross-run stream
  // (mockRunsBoardApi's default /v1alpha1/events is empty; this override
  // takes precedence because Playwright tries the most-recently-registered
  // matching route first) and change what /v1alpha1/runs answers once the
  // reload it triggers actually fires — proving the board updates itself
  // from a real SSE event, with no browser reload anywhere in this test.
  test("moves a card between columns from a committed run event, without a reload", async ({
    page,
  }) => {
    const created = BOARD_RUNS.find((run) => run.state === "created")!;
    const movedRun = { ...created, state: "running" as const };

    // Bare-glob route patterns (e.g. "**/v1alpha1/runs") only match a URL
    // with NO query string appended — RunsBoard's own listRuns call always
    // carries `?sort=updated_at&...`, so matching on pathname via a
    // predicate is required here (a glob without a trailing `*` silently
    // never matches and falls through to mockRunsBoardApi's fixed handler).
    let runsRequests = 0;
    await page.route(
      (url) => url.pathname === "/v1alpha1/runs",
      async (route) => {
        if (route.request().method() !== "GET") {
          await route.continue();
          return;
        }
        runsRequests += 1;
        const items = runsRequests === 1 ? BOARD_RUNS : [movedRun, ...BOARD_RUNS.slice(1)];
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ items }),
        });
      },
    );

    await page.route(
      (url) => url.pathname === "/v1alpha1/events",
      async (route) => {
        const request = route.request();
        const requestUrl = new URL(request.url());
        const headers = await request.allHeaders();
        const from =
          headers["last-event-id"] ?? requestUrl.searchParams.get("from") ?? "";
        const latest = from === "latest";
        // The very first connection (no resume cursor yet) carries the
        // committed event; every reconnect after that is honestly empty —
        // the event is never replayed twice.
        const body =
          from === "" || latest
            ? `id: 01RUNSBOARD0000000000001\nevent: dev.culture.nodes.run.completed\ndata: ${JSON.stringify(
                {
                  id: "01RUNSBOARD0000000000001",
                  source: "nodes",
                  specversion: "1.0",
                  type: "dev.culture.nodes.run.completed",
                  subject: created.id,
                  time: "2026-08-13T00:00:00Z",
                  datacontenttype: "application/json",
                  data: { run_id: created.id },
                },
              )}\n\n`
            : "";
        await route.fulfill({
          status: 200,
          headers: {
            "content-type": "text/event-stream",
            "cache-control": "no-cache",
          },
          body: (latest ? snapshotMarkerSse("0") : "") + body,
        });
      },
    );

    // No intermediate "still created" check here on purpose: the first
    // scheduled reload after mount has no debounce delay (same as Mesh's
    // attribution refresh — see RunsBoard.tsx's scheduleReload), so the
    // event -> reload -> re-render round trip can finish before the very
    // next assertion even runs. The mechanism under test is proven by the
    // end state below: without the auto-refresh wiring, the fixture's
    // /v1alpha1/runs response never changes, so the card could never move.
    await openBoard(page);

    await expect(
      page.locator(`[data-column-state="running"] [data-run-id="${created.id}"]`),
    ).toHaveCount(1, { timeout: 10_000 });
    await expect(
      page.locator(`[data-column-state="created"] [data-run-id="${created.id}"]`),
    ).toHaveCount(0);
    // The board never nulled itself back to the loading state along the way.
    await expect(page.locator("#runs-board-loading")).toHaveCount(0);
  });
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
