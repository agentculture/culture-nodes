import { expect, test, type Page } from "@playwright/test";
import { RUN_ID, mockApi, readAgentState } from "./fixtures/api";

test.beforeEach(async ({ page }) => {
  await mockApi(page);
});

async function openRun(page: Page) {
  await page.goto(`/runs/${RUN_ID}`);
  await expect
    .poll(async () => (await readAgentState(page)).status)
    .toBe("ready");
}

/** Tab forward until focus lands on the named node card. */
async function tabToNode(page: Page, target: string): Promise<number> {
  const focused = page.locator("*:focus");
  for (let presses = 1; presses <= 40; presses += 1) {
    await page.keyboard.press("Tab");
    if ((await focused.count()) === 0) continue;
    if ((await focused.first().getAttribute("data-node-id")) === target) {
      return presses;
    }
  }
  throw new Error(`never reached node ${target} by tabbing`);
}

test("renders the first-slice run graph from the workflow IR", async ({
  page,
}) => {
  await openRun(page);

  await expect(page.locator("#run-state-chip")).toHaveAttribute(
    "data-run-state",
    "running",
  );
  for (const nodeId of ["intake", "plan", "build", "test", "verify", "finish"]) {
    await expect(page.locator(`.node-card[data-node-id="${nodeId}"]`)).toHaveCount(
      1,
    );
  }

  const state = await readAgentState(page);
  expect(state.run?.id).toBe(RUN_ID);
  expect(state.run?.state).toBe("running");
  expect(state.run?.node_states).toMatchObject({
    intake: "completed",
    plan: "completed",
    build: "active",
    test: "completed",
    verify: "completed",
    finish: "idle",
  });
  expect(state.run?.selected).toBeNull();
});

test("live events mark walked edges solid and leave untaken ones dashed", async ({
  page,
}) => {
  await openRun(page);

  const walked = page.locator('.react-flow__edge[data-id="test.passed->verify"]');
  await expect(walked).toHaveClass(/is-walked/);

  const loop = page.locator(
    '.react-flow__edge[data-id="verify.changes_required->build"]',
  );
  await expect(loop).toHaveClass(/is-loop/);
  await expect(loop).toHaveClass(/is-walked/);
  // Visibly distinct once walked, and named for a screen reader (§8.8).
  await expect(loop).toHaveAttribute(
    "aria-label",
    "edge verify changes_required to build, walked, loop",
  );
  await expect(loop.locator(".react-flow__edge-path")).toHaveCSS(
    "stroke-width",
    "3.4px",
  );

  const untaken = page.locator(
    '.react-flow__edge[data-id="verify.blocked->human-review"]',
  );
  await expect(untaken).toHaveClass(/is-unwalked/);
});

test("the timeline carries the same run information without the graph", async ({
  page,
}) => {
  await openRun(page);
  await expect(page.locator("#event-timeline li")).toHaveCount(19);

  await page.locator("#view-toggle-timeline").click();
  await expect(page.locator("#run-canvas")).toHaveCount(0);
  await expect(page.locator("#run-node-list")).toBeVisible();
  await expect(
    page.locator('#run-node-list tr[data-list-node-id="build"]'),
  ).toContainText("running");
});

test("keyboard-only: tab to a node, Enter opens its detail, Escape returns focus", async ({
  page,
}) => {
  await openRun(page);

  await tabToNode(page, "build");
  await page.keyboard.press("Enter");

  const panel = page.locator("#node-detail-panel");
  await expect(panel).toBeVisible();
  await expect(panel).toHaveAttribute("aria-label", "Node detail: build");
  await expect(panel).toBeFocused();
  // Both of build's visits are listed, the finished one and the in-flight one.
  await expect(panel.locator('[data-attempt-id="att-build-1"]')).toHaveCount(1);
  await expect(panel.locator('[data-attempt-id="att-build-2"]')).toHaveCount(1);
  await expect(panel).toContainText("dispatched");
  await expect(panel).toContainText("team/developer-experience");

  // The machine-readable mirror reflects the selection.
  await expect
    .poll(async () => (await readAgentState(page)).run?.selected)
    .toBe("build");

  await page.keyboard.press("Escape");
  await expect(panel).toHaveCount(0);
  await expect
    .poll(async () => (await readAgentState(page)).run?.selected)
    .toBeNull();
  // Focus went back to the node that opened the panel, not to <body>.
  await expect(
    page.locator('.node-card[data-node-id="build"]'),
  ).toBeFocused();
});

test("clicking a node opens its detail", async ({ page }) => {
  await openRun(page);
  await page.locator('.node-card[data-node-id="test"]').click();
  const panel = page.locator("#node-detail-panel");
  await expect(panel).toBeVisible();
  await expect(panel).toHaveAttribute("aria-label", "Node detail: test");
  // The code node's observed evidence — PRD §8.7's headspace slice.
  await expect(panel.locator("#node-detail-evidence")).toContainText(
    "artifact://workspace/pytest-report.xml",
  );
  // Runner-recorded, so `observed` authority — a fact measured, not claimed.
  await expect(
    panel.locator(
      '#node-detail-evidence .authority-chip[data-authority="observed"]',
    ),
  ).toHaveCount(1);
  await expect
    .poll(async () => (await readAgentState(page)).run?.selected)
    .toBe("test");
});

test("every node card is a named, reachable button", async ({ page }) => {
  await openRun(page);
  await expect(
    page.getByRole("button", { name: "node intake, agent, completed" }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "node build, agent, running" }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "node test, code, completed" }),
  ).toBeVisible();
});

test("arrow keys pan the canvas", async ({ page }) => {
  await openRun(page);
  const viewport = page.locator(".react-flow__viewport");
  const before = await viewport.getAttribute("style");
  await tabToNode(page, "intake");
  await page.keyboard.press("ArrowRight");
  await page.keyboard.press("ArrowRight");
  await expect
    .poll(async () => await viewport.getAttribute("style"))
    .not.toBe(before);
});

test("the page produces no uncaught errors while rendering a run", async ({
  page,
}) => {
  const pageErrors: string[] = [];
  page.on("pageerror", (error) => pageErrors.push(error.message));
  await openRun(page);
  await page.locator("#view-toggle-timeline").click();
  expect(pageErrors).toEqual([]);
});

test.describe("reduced motion", () => {
  // Emulates `prefers-reduced-motion: reduce` for this block's contexts.
  test.use({ contextOptions: { reducedMotion: "reduce" } });

  test("drops the pulse animation and states the same fact in text", async ({
    page,
  }) => {
    await openRun(page);
    const active = page.locator('.node-card[data-node-state="active"]');
    await expect(active).toHaveCount(1);
    await expect(active).not.toHaveClass(/is-pulse/);
    await expect(page.locator(".is-pulse")).toHaveCount(0);
    await expect(active).toContainText("attempt in flight");
  });
});
