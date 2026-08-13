import { expect, test } from "@playwright/test";
import {
  INVALID_YAML_SOURCE,
  mockAuthoringApi,
  readAgentState,
  VALID_YAML_SOURCE,
} from "./fixtures/api";
import { AUTHORING_DIGEST } from "../src/fixtures/authoring-fixture";

test.beforeEach(async ({ page }) => {
  await mockAuthoringApi(page);
});

test("the Workflows view links to the authoring surface, and back", async ({
  page,
}) => {
  await page.goto("/workflows");
  await expect
    .poll(async () => (await readAgentState(page)).status)
    .toBe("ready");

  await page.getByRole("link", { name: "New workflow" }).click();
  await expect(page).toHaveURL(/\/workflows\/new$/);
  await expect(page.locator("h1")).toHaveText("New workflow");
});

test("paste invalid YAML -> diagnostics render verbatim, Publish stays disabled, and nothing is published", async ({
  page,
}) => {
  let publishCalled = false;
  await page.route("**/v1alpha1/workflows", async (route) => {
    if (route.request().method() === "POST") publishCalled = true;
    await route.continue();
  });

  await page.goto("/workflows/new");
  await page.locator("#workflow-source-input").fill(INVALID_YAML_SOURCE);
  await page.getByRole("button", { name: "Validate" }).click();

  const diagnostic = page.locator('[data-diagnostic-level="error"]');
  await expect(diagnostic).toHaveCount(1);
  await expect(diagnostic).toContainText(
    'edge target "does-not-exist" is not a declared node',
  );
  await expect(diagnostic).toContainText("declare a node named");

  await expect(page.getByRole("button", { name: "Publish" })).toBeDisabled();
  await expect(page.locator("#workflow-preview-canvas")).toHaveCount(0);

  // Nothing was published for the invalid document — try clicking Publish
  // anyway (it is disabled, so this must be a no-op) and confirm no request
  // ever reached the publish endpoint.
  await page
    .getByRole("button", { name: "Publish" })
    .click({ force: true })
    .catch(() => {
      /* a disabled button may refuse the click outright; either way is fine */
    });
  expect(publishCalled).toBe(false);
  await expect(page.locator("#workflow-publish-result")).toHaveCount(0);
});

test("paste valid YAML -> read-only preview renders before publish -> publish shows the digest", async ({
  page,
}) => {
  await page.goto("/workflows/new");
  await page.locator("#workflow-source-input").fill(VALID_YAML_SOURCE);
  await page.getByRole("button", { name: "Validate" }).click();

  await expect(page.locator("#workflow-diagnostics-empty")).toBeVisible();
  const canvas = page.locator("#workflow-preview-canvas");
  await expect(canvas).toBeVisible();
  await expect(canvas.locator("[data-node-id]")).toHaveCount(2);
  await expect(canvas.locator('[data-node-id="greet"]')).toBeVisible();
  await expect(canvas.locator('[data-node-id="finish"]')).toBeVisible();

  // Publish is disabled before publishing has been confirmed valid, and the
  // result panel must not exist yet — the preview is read-only and comes
  // strictly before publish.
  await expect(page.locator("#workflow-publish-result")).toHaveCount(0);

  const publishRequest = page.waitForRequest(
    (req) =>
      req.url().endsWith("/v1alpha1/workflows") && req.method() === "POST",
  );
  await page.getByRole("button", { name: "Publish" }).click();
  const request = await publishRequest;

  // The exact pasted string went over the wire — no client-side
  // re-serialization (byte-identity with a CLI publish of the same source).
  const body = request.postDataJSON() as { source: string; format?: string };
  expect(body.source).toBe(VALID_YAML_SOURCE);
  expect(body.format).toBe("yaml");

  await expect(page.locator("#workflow-publish-result")).toBeVisible();
  await expect(
    page.locator(`[data-published-digest="${AUTHORING_DIGEST}"]`),
  ).toHaveText(AUTHORING_DIGEST);
});

test("upload path: a .yaml file populates the source and validates the same way as a paste", async ({
  page,
}) => {
  await page.goto("/workflows/new");
  await page.setInputFiles("#workflow-file-input", {
    name: "workflow.yaml",
    mimeType: "application/x-yaml",
    buffer: Buffer.from(VALID_YAML_SOURCE),
  });

  await expect(page.locator("#workflow-source-input")).toHaveValue(
    VALID_YAML_SOURCE,
  );
  await expect(page.locator("#workflow-format-select")).toHaveValue("yaml");

  await page.getByRole("button", { name: "Validate" }).click();
  await expect(page.locator("#workflow-diagnostics-empty")).toBeVisible();
  await expect(page.locator("#workflow-preview-canvas")).toBeVisible();
});

test("upload path: a .json file infers the json format", async ({ page }) => {
  const jsonSource = JSON.stringify({
    spec: { entry: "a", nodes: { a: { kind: "agent" } }, edges: [] },
  });
  await page.goto("/workflows/new");
  await page.setInputFiles("#workflow-file-input", {
    name: "workflow.json",
    mimeType: "application/json",
    buffer: Buffer.from(jsonSource),
  });

  await expect(page.locator("#workflow-source-input")).toHaveValue(
    jsonSource,
  );
  await expect(page.locator("#workflow-format-select")).toHaveValue("json");
});

test("agent-state reflects the authoring step through validate and publish", async ({
  page,
}) => {
  await page.goto("/workflows/new");
  await expect
    .poll(async () => (await readAgentState(page)).authoring?.step)
    .toBe("editing");

  await page.locator("#workflow-source-input").fill(VALID_YAML_SOURCE);
  await page.getByRole("button", { name: "Validate" }).click();
  await expect
    .poll(async () => (await readAgentState(page)).authoring?.step)
    .toBe("valid");

  await page.getByRole("button", { name: "Publish" }).click();
  await expect
    .poll(async () => (await readAgentState(page)).authoring?.step)
    .toBe("published");
  const state = await readAgentState(page);
  expect(state.authoring?.digest).toBe(AUTHORING_DIGEST);
});

test("the page produces no uncaught errors while authoring", async ({
  page,
}) => {
  const pageErrors: string[] = [];
  page.on("pageerror", (error) => pageErrors.push(error.message));
  await page.goto("/workflows/new");
  await page.locator("#workflow-source-input").fill(VALID_YAML_SOURCE);
  await page.getByRole("button", { name: "Validate" }).click();
  await expect(page.locator("#workflow-preview-canvas")).toBeVisible();
  await page.getByRole("button", { name: "Publish" }).click();
  await expect(page.locator("#workflow-publish-result")).toBeVisible();
  expect(pageErrors).toEqual([]);
});
