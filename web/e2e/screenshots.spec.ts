import { expect, test, type Page } from "@playwright/test";
import { TICKET_ID } from "../src/fixtures/ticket-fixture";
import {
  RUN_ID,
  mockApi,
  mockAuthoringApi,
  mockDecisionsApi,
  mockHomeApi,
  mockInboxApi,
  mockJobsTimelineApi,
  mockDesignApi,
  mockMeshApi,
  mockPlanApi,
  mockRunsBoardApi,
  mockTicketApi,
} from "./fixtures/api";

/**
 * A screenshot pass over the views task t27 changed. Not an assertion suite —
 * it captures, it does not check — so it is SKIPPED unless `NODES_SHOTS` names
 * an output directory:
 *
 *   NODES_SHOTS=/tmp/shots npx playwright test e2e/screenshots.spec.ts
 *
 * The directory receives the fixture-backed baseline set, including
 * mesh.png, mesh-dark.png and active-graphs.png.
 *
 * It runs off the same request-interception fixtures the real e2e specs use,
 * so it needs no Go server, no Postgres and no network — the shots show the
 * app against known data rather than whatever a live deployment happened to
 * hold that afternoon.
 */

const OUT = process.env.NODES_SHOTS;

test.describe("t27 site polish shots", () => {
  test.skip(!OUT, "set NODES_SHOTS=<dir> to capture screenshots");

  /**
   * `GET /v1alpha1/version` is not in the fixture routes (the specs that
   * exist predate the header reading it), so it is stubbed here. Stamped and
   * clean, which is the case a reader should see most often.
   *
   * Call this AFTER the view's own `mockXApi`: Playwright matches the most
   * recently registered route first, and every fixture registers a glob
   * catch-all over `/v1alpha1` that would otherwise 404 this one.
   */
  async function stubVersion(page: Page) {
    await page.route("**/v1alpha1/version", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          version: "0.51.0",
          revision: "4f2c9a71b3e8d05612a7c4f9e8b0d3a165c7e921",
          revision_source: "build_flag",
          staleness:
            "this binary was stamped at build time with the revision the deploy shipped; " +
            "it names the commit the SOURCE came from and cannot say whether that tree was clean",
        }),
      }),
    );
  }

  async function shoot(page: Page, name: string) {
    // ELK lays a graph canvas out asynchronously, and the canvas fades in over
    // 180ms once its positions land (`.layout-canvas` in styles/app.css). A
    // view's own readiness signal — a state chip, a first node — fires well
    // before that, and a late layout pass restarts the fade, so a shot taken
    // on that signal catches the canvas half-transparent over the page behind
    // it: a washed-out light box rather than the terminal ground. Waiting on
    // the fade's END STATE rather than a fixed beat is what makes the capture
    // deterministic — `toHaveCSS` retries until it holds.
    const canvases = page.locator(".layout-canvas");
    for (let i = 0; i < (await canvases.count()); i += 1) {
      await expect(canvases.nth(i)).toHaveCSS("opacity", "1");
    }
    await page.waitForTimeout(200);
    await page.screenshot({ path: `${OUT}/${name}.png`, fullPage: true });
  }

  test("runs list, wide and narrow", async ({ page }) => {
    await mockRunsBoardApi(page);
    await stubVersion(page);
    await page.goto("/runs");
    await page.getByRole("table").waitFor();
    await shoot(page, "runs-list");

    await page.setViewportSize({ width: 420, height: 900 });
    await shoot(page, "runs-list-narrow");

    // The collapsed nav, open — where the Tickets key field lives on a phone.
    await page.getByRole("button", { name: "Menu" }).click();
    await shoot(page, "runs-list-narrow-menu");
  });

  test("board", async ({ page }) => {
    await mockRunsBoardApi(page);
    await stubVersion(page);
    await page.goto("/runs?view=board");
    await page.locator(".runs-board__columns").waitFor();
    await shoot(page, "board");
  });

  test("jobs", async ({ page }) => {
    await mockJobsTimelineApi(page);
    await stubVersion(page);
    await page.goto("/runs?view=jobs");
    await page.getByRole("table").first().waitFor();
    await shoot(page, "jobs");

    await page.setViewportSize({ width: 420, height: 900 });
    await shoot(page, "jobs-narrow");
  });

  test("run view", async ({ page }) => {
    await mockApi(page);
    await stubVersion(page);
    await page.goto(`/runs/${RUN_ID}`);
    await page.locator("#run-state-chip").waitFor();
    await shoot(page, "run-view");
  });

  test("mesh, light and dark", async ({ page }) => {
    await mockMeshApi(page);
    await stubVersion(page);
    await page.goto("/mesh");
    await page.locator("#mesh-canvas .react-flow__node").first().waitFor();
    await page.locator('#mesh-canvas[data-layout-ready="true"]').waitFor();
    await shoot(page, "mesh");
    await page.emulateMedia({ colorScheme: "dark" });
    await shoot(page, "mesh-dark");
  });

  test("active graphs", async ({ page }) => {
    await mockDesignApi(page);
    await stubVersion(page);
    await page.goto("/design?tab=active");
    await page.locator("[id^=active-graph-][id$=-canvas]").first().waitFor();
    await shoot(page, "active-graphs");
  });

  test("new workflow, with the sample loaded", async ({ page }) => {
    await mockAuthoringApi(page);
    await stubVersion(page);
    await page.goto("/design/new");
    await page.getByRole("button", { name: "Load a sample" }).click();
    await shoot(page, "author-workflow-sample");
  });

  test("generate workflow", async ({ page }) => {
    await stubVersion(page);
    await page.goto("/design/generate");
    await page.getByLabel(/description/i).waitFor();
    await shoot(page, "generate-workflow");
  });

  /**
   * The ticket page, signed in — the state a person on a Jira link is
   * actually in. Two shots, because the whole of task t17's first criterion
   * is about what fits on the FIRST screen: `ticket-above-fold` is the
   * 1280x800 viewport with no scrolling, `ticket` is the whole page.
   */
  test("ticket", async ({ page }) => {
    await mockTicketApi(page);
    await stubVersion(page);
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto(`/tickets/${TICKET_ID}`);
    await page.getByRole("heading", { name: TICKET_ID }).waitFor();
    await page.screenshot({ path: `${OUT}/ticket-above-fold.png` });
    await shoot(page, "ticket");

    // Both themes, because every value on the new surfaces resolves through
    // culture-design/tokens.css and a hard-coded colour would only show here.
    await page.emulateMedia({ colorScheme: "dark" });
    await page.screenshot({ path: `${OUT}/ticket-above-fold-dark.png` });
  });

  /**
   * The three remaining human-facing surfaces. They are shot at the same
   * 1280x800 first screen as the ticket page, plus full-page, because the
   * finding this pass exists to evidence (issue #270) is about how much text
   * a person meets before anything they can act on.
   */
  test("home", async ({ page }) => {
    await mockHomeApi(page);
    await stubVersion(page);
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/");
    await page.getByRole("heading", { level: 1 }).first().waitFor();
    await page.screenshot({ path: `${OUT}/home-above-fold.png` });
    await shoot(page, "home");

    await page.emulateMedia({ colorScheme: "dark" });
    await page.screenshot({ path: `${OUT}/home-above-fold-dark.png` });
  });

  test("inbox", async ({ page }) => {
    await mockInboxApi(page);
    await stubVersion(page);
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/inbox");
    await page.getByRole("heading", { level: 1 }).first().waitFor();
    await page.screenshot({ path: `${OUT}/inbox-above-fold.png` });
    await shoot(page, "inbox");
  });

  test("decisions", async ({ page }) => {
    await mockDecisionsApi(page);
    await stubVersion(page);
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/decisions");
    await page.getByRole("heading", { level: 1 }).first().waitFor();
    await page.screenshot({ path: `${OUT}/decisions-above-fold.png` });
    await shoot(page, "decisions");
  });

  test("plan", async ({ page }) => {
    await mockPlanApi(page);
    await stubVersion(page);
    await page.goto("/plan");
    await page.getByLabel("Plan slug").waitFor();
    await shoot(page, "plan-empty");
  });
});
