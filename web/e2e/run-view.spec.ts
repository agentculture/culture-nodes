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

test("shows the run's given name, category, and token-first cost in the header (task t5)", async ({
  page,
}) => {
  await openRun(page);

  await expect(page.locator("#run-view-name")).toContainText(
    "deliver the ledger projection endpoint",
  );
  // A given name, never marked as a derived guess.
  await expect(
    page.locator("#run-view-name .run-name--derived"),
  ).toHaveCount(0);
  await expect(page.locator("#run-view-name .category-chip")).toContainText(
    "delivery",
  );

  const usage = page.locator("#run-usage-summary");
  await expect(usage).toHaveAttribute("data-usage-reported", "true");
  // Token-first: the primary figure is tokens, cost is secondary and only
  // present because the fixture actually reported one.
  await expect(usage).toContainText("14.9k in / 4.6k out");
  await expect(usage).toContainText("0.93 USD");

  const state = await readAgentState(page);
  expect(state.run?.name).toBe("deliver the ledger projection endpoint");
  expect(state.run?.category).toBe("delivery");
  expect(state.run?.usage?.reported).toBe(true);
  expect(state.run?.usage?.input_tokens).toBe(14900);
});

test("node detail shows per-node usage, merged across a looped node's visits, honestly reporting the in-flight attempt as not-reported (task t5)", async ({
  page,
}) => {
  await openRun(page);
  await tabToNode(page, "build");
  await page.keyboard.press("Enter");

  const panel = page.locator("#node-detail-panel");
  await expect(panel).toBeVisible();
  const usage = panel.locator("#node-detail-usage");
  await expect(usage).toBeVisible();
  // nr-build-1 reported 5200 in / 1800 out; nr-build-2 (still dispatched)
  // reported nothing yet — merged, the reported figures show and the
  // in-flight attempt surfaces as a partial-not-reported note, never as
  // an invented zero folded into the totals.
  await expect(usage).toContainText("5.2k in / 1.8k out");
  await expect(usage).toContainText("0.52 USD");
  await expect(usage).toContainText("1 attempt not reported");

  // A node with straightforward, fully-reported usage shows no such note.
  await page.keyboard.press("Escape");
  await page.locator('.node-card[data-node-id="verify"]').click();
  const verifyUsage = page.locator("#node-detail-usage");
  await expect(verifyUsage).toContainText("4.1k in / 1.4k out");
  await expect(verifyUsage).not.toContainText("not reported");
});

test("the approver reads changed files, snapshot digest, artifact refs, and attempt cost entirely in-page (task t11)", async ({
  page,
}) => {
  await openRun(page);

  // Discoverable before opening the panel: the build node's card carries
  // the evidence marker (task t11 acceptance #3), both as a stable
  // attribute (independent of zoom band) and as a visible badge at this
  // view's default zoom.
  const buildCard = page.locator('.node-card[data-node-id="build"]');
  await expect(buildCard).toHaveAttribute("data-node-evidence", "true");
  await expect(buildCard).toContainText("evidence");

  // A node with no workspace evidence carries the marker as explicitly
  // false, never an omitted/ambiguous attribute.
  await expect(
    page.locator('.node-card[data-node-id="intake"]'),
  ).toHaveAttribute("data-node-evidence", "false");

  await buildCard.click();
  const panel = page.locator("#node-detail-panel");
  await expect(panel).toBeVisible();

  // Changed files, from the worker hook's measured changed_paths.
  const changedPaths = panel.locator('[data-evidence-changed-paths="true"]');
  await expect(changedPaths).toContainText("internal/worker/hooks.go");
  await expect(changedPaths).toContainText("internal/runners/dispatch.go");

  // The snapshot digest, as a mono chip.
  await expect(
    panel.locator('[data-evidence-snapshot-digest="true"]'),
  ).toHaveText(`sha256:${"c".repeat(64)}`);

  // The artifact ref the diff itself lives at.
  await expect(
    panel.locator('[data-evidence-artifact-refs="true"]'),
  ).toContainText("artifact://diff/att-build-1");

  // Per-attempt cost, read from the same panel, in the same pause — task
  // t2/t5's node-run usage join, now with attempts_reported spelled out
  // explicitly (task t11 acceptance #2).
  const usage = panel.locator("#node-detail-usage");
  await expect(usage).toContainText("5.2k in / 1.8k out");
  await expect(usage).toContainText("0.52 USD");
  await expect(usage).toHaveAttribute("data-attempts-reported", "1");
  await expect(usage).toContainText("1 attempt reported");
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
