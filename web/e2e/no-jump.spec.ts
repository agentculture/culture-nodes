import { expect, test, type Locator } from "@playwright/test";
import {
  RUN_ID,
  mockApi,
  mockDecisionsApi,
  mockDesignApi,
} from "./fixtures/api";

async function expectStableFirstPaint(canvas: Locator) {
  await expect(canvas).toHaveAttribute("data-layout-ready", "true");
  const nodes = canvas.locator(".react-flow__node");
  await expect(nodes.first()).toBeVisible();
  const firstPaint = await nodes.evaluateAll((items) =>
    items.map((item) => ({
      id: item.getAttribute("data-id"),
      style: item.getAttribute("style"),
    })),
  );
  await canvas.page().waitForTimeout(250);
  expect(
    await nodes.evaluateAll((items) =>
      items.map((item) => ({
        id: item.getAttribute("data-id"),
        style: item.getAttribute("style"),
      })),
    ),
  ).toEqual(firstPaint);
}

test("RunView nodes do not move after the canvas first paints", async ({
  page,
}) => {
  await mockApi(page);
  await page.goto(`/runs/${RUN_ID}`);
  await expect(page.locator("#view-toggle")).toHaveClass(/segmented-toggle/);
  await expectStableFirstPaint(page.locator("#run-canvas"));
});

test("Design nodes do not move after the canvas first paints", async ({
  page,
}) => {
  await mockDesignApi(page);
  await page.goto("/design");
  await expect(page.locator("#design-toggle")).toHaveClass(/segmented-toggle/);
  await expectStableFirstPaint(page.locator("#design-graph"));
});

test("Runs uses the shared segmented control", async ({ page }) => {
  await mockApi(page);
  await page.goto("/runs");
  await expect(page.locator("#runs-toggle")).toHaveClass(/segmented-toggle/);
});

test("Decisions uses the shared segmented control", async ({ page }) => {
  await mockDecisionsApi(page);
  await page.goto("/decisions");
  const toggle = page.getByRole("group", { name: "Decision views" });
  await expect(toggle).toHaveClass(/segmented-toggle/);
  await expect(toggle.locator(":scope > button")).toHaveCount(2);
  await expect(page.locator(".decisions-tabs")).toHaveCount(0);
});
