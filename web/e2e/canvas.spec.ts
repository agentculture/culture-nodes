import { expect, test } from "@playwright/test";
import { CANVAS_DIGEST, CANVAS_SOURCE, CANVAS_VALIDATION, CANVAS_VERSION } from "../src/fixtures/canvas-fixture";
import { WHOAMI_BOUND } from "../src/fixtures/whoami-fixture";

test("mouse and keyboard author equivalent nodes/connections/deletion and publish CLI-identical bytes", async ({ page }) => {
  const published: string[] = [];
  await page.route("**/v1alpha1/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith("/whoami")) return route.fulfill({ json: WHOAMI_BOUND });
    if (path.endsWith("/workflows/validate")) return route.fulfill({ json: CANVAS_VALIDATION });
    if (path.endsWith("/workflows") && route.request().method() === "GET") return route.fulfill({ json: { items: [CANVAS_VERSION] } });
    if (path.endsWith("/workflows")) { const body = route.request().postDataJSON() as { source: string }; published.push(body.source); return route.fulfill({ status: 200, json: { ...CANVAS_VERSION, source: body.source } }); }
    return route.fulfill({ status: 404, json: {} });
  });
  await page.goto(`/design/canvas?source=${encodeURIComponent(CANVAS_SOURCE)}`);
  await page.getByRole("button", { name: /Add code/ }).click();
  await page.getByRole("button", { name: /Add wait/ }).focus();
  await page.keyboard.press("Enter");
  await page.getByRole("button", { name: "node code-2, code" }).click();
  await page.getByRole("button", { name: "Connect from selected" }).click();
  await page.getByRole("button", { name: "node wait-2, wait" }).focus();
  await page.keyboard.press("Enter");
  await page.getByRole("button", { name: "Delete selected" }).focus();
  await page.keyboard.press("Enter");
  await page.getByRole("button", { name: "Source" }).click();
  await expect(page.getByLabel("Workflow source")).toContainText("kind: code");
  await page.getByRole("button", { name: "Publish" }).click();
  expect(published[0]).toBe(await page.getByLabel("Workflow source").textContent());
  expect(CANVAS_VERSION.digest).toBe(CANVAS_DIGEST); // deterministic fixture stands in for `nodes workflow publish`
});

test("viewer receives the server's same publish refusal", async ({ page }) => {
  await page.route("**/v1alpha1/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith("/whoami")) return route.fulfill({ json: { ...WHOAMI_BOUND, roles: ["viewer"] } });
    if (path.endsWith("/workflows/validate")) return route.fulfill({ json: CANVAS_VALIDATION });
    if (path.endsWith("/workflows") && route.request().method() === "GET") return route.fulfill({ json: { items: [CANVAS_VERSION] } });
    return route.fulfill({ status: 403, json: { message: "viewer role cannot publish workflows", remediation: "request the workflow-author role" } });
  });
  await page.goto(`/design/canvas?source=${encodeURIComponent(CANVAS_SOURCE)}`);
  await page.getByRole("button", { name: "Publish" }).click();
  await expect(page.getByText("viewer role cannot publish workflows")).toBeVisible();
});
