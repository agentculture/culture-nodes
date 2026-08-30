import { test, type Page } from "@playwright/test";
import { TICKET_ID, TICKET_PROJECTION } from "../src/fixtures/ticket-fixture";
import {
  RUN_ID,
  mockApi,
  mockAuthoringApi,
  mockJobsTimelineApi,
  mockPlanApi,
  mockRunsBoardApi,
} from "./fixtures/api";

/**
 * A screenshot pass over the views task t27 changed. Not an assertion suite —
 * it captures, it does not check — so it is SKIPPED unless `NODES_SHOTS` names
 * an output directory:
 *
 *   NODES_SHOTS=/tmp/shots npx playwright test e2e/screenshots.spec.ts
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
    await page.goto("/board");
    await page.locator(".runs-board__columns").waitFor();
    await shoot(page, "board");
  });

  test("jobs", async ({ page }) => {
    await mockJobsTimelineApi(page);
    await stubVersion(page);
    await page.goto("/jobs");
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

  test("new workflow, with the sample loaded", async ({ page }) => {
    await mockAuthoringApi(page);
    await stubVersion(page);
    await page.goto("/workflows/new");
    await page.getByRole("button", { name: "Load a sample" }).click();
    await shoot(page, "author-workflow-sample");
  });

  test("generate workflow", async ({ page }) => {
    await stubVersion(page);
    await page.goto("/workflows/generate");
    await page.getByLabel(/description/i).waitFor();
    await shoot(page, "generate-workflow");
  });

  test("ticket", async ({ page }) => {
    await page.route("**/v1alpha1/tickets/**", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(TICKET_PROJECTION),
      }),
    );
    await stubVersion(page);
    await page.goto(`/tickets/${TICKET_ID}`);
    await page.getByRole("heading", { name: TICKET_ID }).waitFor();
    await shoot(page, "ticket");
  });

  test("plan", async ({ page }) => {
    await mockPlanApi(page);
    await stubVersion(page);
    await page.goto("/plan");
    await page.getByLabel("Plan slug").waitFor();
    await shoot(page, "plan-empty");
  });
});
