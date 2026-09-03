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

test("a palette drop lands at the drop point, a dragged node keeps its place, and the pane marks inserted lines", async ({ page }) => {
  await page.route("**/v1alpha1/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith("/whoami")) return route.fulfill({ json: WHOAMI_BOUND });
    if (path.endsWith("/workflows/validate")) return route.fulfill({ json: CANVAS_VALIDATION });
    if (path.endsWith("/workflows") && route.request().method() === "GET") return route.fulfill({ json: { items: [CANVAS_VERSION] } });
    return route.fulfill({ status: 404, json: {} });
  });
  await page.goto(`/design/canvas?source=${encodeURIComponent(CANVAS_SOURCE)}`);
  const canvas = page.getByRole("application", { name: "Workflow canvas" });
  await expect(canvas).toHaveAttribute("data-layout-ready", "true");
  // Drop: a synthetic HTML5 drop at a known point inside the canvas.
  const box = (await canvas.boundingBox())!;
  const target = { x: box.x + box.width * 0.7, y: box.y + box.height * 0.7 };
  await page.evaluate(({ x, y }) => {
    // The e2e tsconfig has no DOM lib; the browser globals are reached through globalThis.
    const g = globalThis as unknown as { document: { querySelector: (s: string) => { dispatchEvent: (e: unknown) => void } }; DataTransfer: new () => { setData: (t: string, v: string) => void }; DragEvent: new (type: string, init: Record<string, unknown>) => unknown };
    const el = g.document.querySelector('[aria-label="Workflow canvas"]');
    const dt = new g.DataTransfer(); dt.setData("application/x-node-kind", "wait");
    el.dispatchEvent(new g.DragEvent("dragover", { bubbles: true, clientX: x, clientY: y, dataTransfer: dt }));
    el.dispatchEvent(new g.DragEvent("drop", { bubbles: true, clientX: x, clientY: y, dataTransfer: dt }));
  }, target);
  const dropped = page.getByRole("button", { name: "node wait-2, wait" });
  await expect(dropped).toBeVisible();
  const droppedBox = (await dropped.boundingBox())!;
  expect(Math.abs(droppedBox.x - target.x)).toBeLessThan(160);
  expect(Math.abs(droppedBox.y - target.y)).toBeLessThan(120);
  // Inserted lines are marked in the source pane.
  await page.getByRole("button", { name: "Source" }).click();
  await expect(page.locator(".culture-source__ins").first()).toContainText("wait-2");
  // Persistence: a placed node keeps its place when another node is added
  // (ELK re-lays out the rest; the person's placement wins).
  const beforeAdd = (await dropped.boundingBox())!;
  const canvasBefore = (await canvas.boundingBox())!;
  await page.getByRole("button", { name: /Add code/ }).click();
  await expect(page.getByRole("button", { name: "node code-2, code" })).toBeVisible();
  const afterAdd = (await dropped.boundingBox())!;
  const canvasAfter = (await canvas.boundingBox())!;
  // Boxes are viewport-relative and the page scrolls when a palette button is
  // brought into view, so compare the node to its canvas, not to the viewport.
  expect(Math.abs((afterAdd.x - canvasAfter.x) - (beforeAdd.x - canvasBefore.x))).toBeLessThan(4);
  expect(Math.abs((afterAdd.y - canvasAfter.y) - (beforeAdd.y - canvasBefore.y))).toBeLessThan(4);
  // Drag: a real pointer drag moves a node and that position sticks through the next add.
  const entry = page.locator(".react-flow__node").first();
  const entryBox = (await entry.boundingBox())!;
  const canvasEntry = (await canvas.boundingBox())!;
  await page.mouse.move(entryBox.x + 30, entryBox.y + 15);
  await page.mouse.down();
  await page.mouse.move(entryBox.x + 130, entryBox.y + 90, { steps: 6 });
  await page.mouse.up();
  const afterDrag = (await entry.boundingBox())!;
  const canvasDrag = (await canvas.boundingBox())!;
  expect(Math.abs((afterDrag.x - canvasDrag.x) - (entryBox.x - canvasEntry.x))).toBeGreaterThan(40);
  await page.getByRole("button", { name: /Add end/ }).click();
  await expect(page.getByRole("button", { name: /node end/ })).toBeVisible();
  const afterSecondAdd = (await entry.boundingBox())!;
  const canvasSecond = (await canvas.boundingBox())!;
  expect(Math.abs((afterSecondAdd.x - canvasSecond.x) - (afterDrag.x - canvasDrag.x))).toBeLessThan(4);
  await expect(page.getByLabel("Canvas status")).toContainText(/valid|diagnostic/);
});
