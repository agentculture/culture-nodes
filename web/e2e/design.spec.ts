import { expect, test, type Page } from "@playwright/test";
import {
  ACTIVE_EVENTS_TOTAL,
  ACTIVE_LAST_EVENT_ID,
  ACTIVE_NODE_ID,
  ACTIVE_PULSES_TOTAL,
  ACTIVE_RUN_ID,
  DELIVER_CHANGE_V1_SOURCE,
  DESIGN_GRAPH_SIZES,
  mockDesignApi,
  NODE_CATALOG_WORKFLOW_VERSIONS,
  ORPHAN_DIGEST,
  readAgentState,
  WORKFLOWS_RUNS,
} from "./fixtures/api";
import { WORKFLOW_DIGEST } from "../src/fixtures/run-fixture";
import { DELIVER_CHANGE_V2_DIGEST } from "../src/fixtures/workflows-fixture";

/**
 * The Design view (task t8, claims c24/c31/c36).
 *
 * The load-bearing spec here is the second test: the gallery is served a
 * namespace with published workflows and ZERO runs, and every published
 * workflow_key still draws its full graph. That is honesty condition h21 —
 * before this view existed, a published-but-idle workflow had no graph
 * anywhere in the app, because the only canvas that drew one (Active Graphs)
 * drew it from a non-terminal run.
 *
 * Replaces e2e/node-graphs.spec.ts: /graphs now redirects here, so that
 * spec's still-live assertions (the Nodes catalog, the Active graphs
 * presence and pulse contract, the authoring link, the skip link) moved
 * across with it, and the workflow-cards assertions were replaced by the
 * gallery's — the panel they described no longer exists.
 */

test.beforeEach(async ({ page }) => {
  await mockDesignApi(page);
});

async function openDesign(page: Page, url = "/design") {
  await page.goto(url);
  await expect
    .poll(async () => (await readAgentState(page)).status)
    .toBe("ready");
}

test("lists one gallery entry per published workflow_key, with its version count and owner", async ({
  page,
}) => {
  await openDesign(page);
  const index = page.locator("#design-workflow-list");
  await expect(index.locator("[data-workflow-key]")).toHaveCount(3);
  await expect(
    index.locator('[data-workflow-key="deliver-change"]'),
  ).toContainText("2 versions");
  await expect(
    index.locator('[data-workflow-key="deliver-change"]'),
  ).toContainText("team/platform-ai");
});

test("every published workflow_key draws its full graph with zero runs in the namespace (c31/h21)", async ({
  page,
}) => {
  await mockDesignApi(page, { runs: "none" });
  await openDesign(page);

  for (const [key, size] of Object.entries(DESIGN_GRAPH_SIZES)) {
    await page.locator(`#design-workflow-list [data-workflow-key="${key}"]`).click();
    const canvas = page.locator("#design-graph");
    await expect(canvas).toHaveAttribute("data-workflow-key", key);
    await expect(canvas).toHaveAttribute(
      "data-node-count",
      String(size.nodes),
    );
    await expect(canvas).toHaveAttribute(
      "data-edge-count",
      String(size.edges),
    );
    // Drawn, not merely counted: one rendered node per IR node.
    await expect(canvas.locator("[data-node-id]")).toHaveCount(size.nodes);
    // And the honest reason there is no run list — not an error, a fact.
    await expect(page.locator("#design-no-runs")).toBeVisible();
  }

  const state = await readAgentState(page);
  expect(state.design?.run_count).toBe(0);
  expect(state.design?.workflow_count).toBe(3);
});

test("the selected version is in the URL, so any graph is a link", async ({
  page,
}) => {
  await openDesign(page);
  await page
    .locator('#design-workflow-list [data-workflow-key="hello-world"]')
    .click();
  await expect(page).toHaveURL(/workflow=hello-world/);

  // The bookmark round-trips: a direct navigation lands on the same graph.
  await page.goto("/design?workflow=deliver-change&version=1");
  await expect(page.locator("#design-graph")).toHaveAttribute(
    "data-workflow-digest",
    WORKFLOW_DIGEST,
  );
});

test("switching version redraws the same key at the other published digest", async ({
  page,
}) => {
  await openDesign(page, "/design?workflow=deliver-change");
  await expect(page.locator("#design-graph")).toHaveAttribute(
    "data-workflow-digest",
    DELIVER_CHANGE_V2_DIGEST,
  );
  await page
    .locator(`#design-version-list [data-workflow-digest="${WORKFLOW_DIGEST}"]`)
    .click();
  await expect(page.locator("#design-graph")).toHaveAttribute(
    "data-workflow-digest",
    WORKFLOW_DIGEST,
  );
});

test("Open source shows the stored source byte-identical (c36/h28)", async ({
  page,
}) => {
  await openDesign(page, "/design?workflow=deliver-change&version=1");
  await expect(page.locator("#design-source")).toHaveCount(0);

  await page.getByRole("button", { name: "Open source" }).click();
  const pane = page.locator("#design-source");
  await expect(pane).toHaveAttribute("data-source-format", "yaml");
  // Every byte, in order — read with textContent rather than innerText so
  // no whitespace normalization can hide a difference.
  expect(await pane.evaluate((el) => el.textContent)).toBe(
    DELIVER_CHANGE_V1_SOURCE,
  );
  expect(
    (await readAgentState(page)).design?.source_bytes,
  ).toBe(DELIVER_CHANGE_V1_SOURCE.length);
});

test("the gallery lists the selected workflow's own runs, never another's or an unmatched digest's", async ({
  page,
}) => {
  await openDesign(page, "/design?workflow=deliver-change");
  const runLinks = page.locator(".design-gallery__run-list a");
  await expect(runLinks).toHaveCount(2);
  await expect(runLinks.nth(0)).toHaveText("run-deliver-v2-01J8XKWORKFLOWS02");
  await expect(runLinks.nth(1)).toHaveText("run-deliver-v1-01J8XKWORKFLOWS04");

  // The orphan-digest run (matches no published version) renders nowhere.
  await expect(page.getByText("run-orphan-01J8XKWORKFLOWS0003")).toHaveCount(0);
});

test("a recent-run row is a real link into the existing Run view", async ({
  page,
}) => {
  await openDesign(page, "/design?workflow=deliver-change");
  const run = WORKFLOWS_RUNS.find(
    (candidate) => candidate.workflow_digest === DELIVER_CHANGE_V2_DIGEST,
  )!;
  const link = page.getByRole("link", { name: run.id });
  await expect(link).toHaveAttribute("href", `/runs/${run.id}`);
  await link.click();
  await expect(page).toHaveURL(new RegExp(`/runs/${run.id}$`));
});

test("requests every published workflow version and one runs page per workflow_key", async ({
  page,
}) => {
  const workflowsRequest = page.waitForRequest(
    (req) =>
      req.url().includes("/v1alpha1/workflows") &&
      !req.url().includes("/v1alpha1/workflows/"),
  );
  const runsRequest = page.waitForRequest((req) =>
    req.url().includes("/v1alpha1/runs?"),
  );
  await openDesign(page);
  await workflowsRequest;
  const runsUrl = new URL((await runsRequest).url());
  expect(runsUrl.searchParams.get("sort")).toBe("updated_at");
  expect(runsUrl.searchParams.get("workflow_key")).not.toBeNull();
});

test("/graphs and /workflows both redirect to /design, so old links survive", async ({
  page,
}) => {
  await page.goto("/graphs");
  await expect(page).toHaveURL(/\/design$/);
  await expect
    .poll(async () => (await readAgentState(page)).status)
    .toBe("ready");
  await expect(page.locator("#design-graph")).toBeVisible();

  await page.goto("/workflows");
  await expect(page).toHaveURL(/\/design$/);
});

test("the Nodes sub-view survives at /design?tab=nodes (t8 acceptance 3)", async ({
  page,
}) => {
  await openDesign(page, "/design?tab=nodes");
  await expect(page.locator("#design-nodes-panel")).toBeVisible();

  // The fixture's four published versions collapse to 8 distinct
  // definitions (NODE_CATALOG_DEFINITION_COUNT's derivation note).
  await expect(page.locator(".node-def-card")).toHaveCount(8);

  const intake = page.locator(
    '[data-definition-id="agent:actor://company/intake@sha256:111111"]',
  );
  await expect(intake).toHaveCount(1);
  await expect(intake).toContainText("agent");
  await expect(intake).toContainText("actor://company/intake@sha256:111111");
  await expect(
    intake.locator('[data-occurrence="deliver-change@v2:intake"]'),
  ).toHaveCount(1);

  // A ref-less definition says so honestly instead of inventing identity.
  await expect(page.locator('[data-definition-id="end"]')).toContainText(
    "identity is the kind alone",
  );
});

test("sub-view selection is URL-param driven and directly bookmarkable", async ({
  page,
}) => {
  await page.goto("/design");
  await expect(page).toHaveURL(/\/design$/); // no ?tab= on the default view
  await expect(page.getByRole("button", { name: "Gallery" })).toHaveAttribute(
    "aria-pressed",
    "true",
  );

  await page.getByRole("button", { name: "Active graphs" }).click();
  await expect(page).toHaveURL(/\/design\?tab=active$/);

  await page.goto("/design?tab=active");
  await expect(
    page.getByRole("button", { name: "Active graphs" }),
  ).toHaveAttribute("aria-pressed", "true");
  await expect(page.locator("#design-active-panel")).toBeVisible();
});

test("the Active graphs sub-view halos only the workflow whose run holds active tokens (t31, h20)", async ({
  page,
}) => {
  await openDesign(page, "/design?tab=active");

  // Exactly one graph: deliver-change v2, pinned by the one running run.
  const graph = page.locator("#active-graph-deliver-change-v2");
  await expect(graph).toHaveCount(1);
  await expect(graph).toHaveAttribute("data-alive", "true");
  await expect(graph).toHaveClass(/is-alive/);
  await expect(graph).toContainText("1 active run");
  await expect(
    graph.locator(`[data-run-id="${ACTIVE_RUN_ID}"] a`),
  ).toHaveAttribute("href", `/runs/${ACTIVE_RUN_ID}`);

  // hello-world's only run is completed, notify-team has none, and the
  // orphan digest matches no published version — none renders a graph (h14).
  await expect(page.locator(".active-graph")).toHaveCount(1);
  await expect(page.getByText("run-orphan-01J8XKWORKFLOWS0003")).toHaveCount(0);

  // Node presence: only the committed running node-run's node is live.
  await expect(
    graph.locator(`[data-node-id="${ACTIVE_NODE_ID}"]`),
  ).toHaveAttribute("data-node-live", "true");
  await expect(graph.locator('[data-node-id="intake"]')).toHaveAttribute(
    "data-node-live",
    "false",
  );
});

test("every rendered pulse traces to a committed event; an unknown-run event is a no-op (t31, h14)", async ({
  page,
}) => {
  await page.goto("/design?tab=active");
  // The fixture stream replays two committed events: one on the loaded run
  // (a pulse), one naming a run the view never fetched (a no-op).
  await expect
    .poll(async () => (await readAgentState(page)).active_graphs?.events_total)
    .toBe(ACTIVE_EVENTS_TOTAL);

  const state = (await readAgentState(page)).active_graphs!;
  expect(state.pulses_total).toBe(ACTIVE_PULSES_TOTAL);
  expect(state.graph_count).toBe(1);
  expect(state.active_run_count).toBe(1);
  expect(state.active_node_count).toBe(1);
  expect(state.last_event_id).toBe(ACTIVE_LAST_EVENT_ID);
  expect(state.reduced_motion).toBe(false);
});

test("prefers-reduced-motion renders one static frame on both canvases (t31, t8)", async ({
  page,
}) => {
  await page.emulateMedia({ reducedMotion: "reduce" });
  await openDesign(page, "/design?tab=active");
  await expect(page.locator("#active-graph-deliver-change-v2")).toHaveAttribute(
    "data-motion",
    "static",
  );
  // Liveness stays readable as text, never motion or colour alone.
  await expect(page.locator("#active-graph-deliver-change-v2")).toContainText(
    "1 active run",
  );
  expect((await readAgentState(page)).active_graphs?.reduced_motion).toBe(true);

  await openDesign(page);
  await expect(
    page.locator("#design-graph [data-node-id]").first(),
  ).toHaveAttribute("data-motion", "static");
});

test("the gallery graph is inspectable from the keyboard alone", async ({
  page,
}) => {
  await openDesign(page, "/design?workflow=deliver-change");
  const canvas = page.locator("#design-graph");
  await expect(canvas).toHaveAttribute("role", "application");
  await canvas.focus();
  await page.keyboard.press("ArrowRight");
  // Breadth-first order, so the entry node first.
  await expect(page.locator("#design-graph-inspect")).toContainText(
    "intake · agent",
  );
  await page.keyboard.press("Escape");
  await expect(page.locator("#design-graph-inspect")).toContainText(
    "arrow keys to inspect",
  );
});

test("the skip link is still the first tab stop on the new route", async ({
  page,
}) => {
  await openDesign(page);
  await page.keyboard.press("Tab");
  const skipLink = page.locator(".skip-link");
  await expect(skipLink).toBeFocused();
  await expect(skipLink).toHaveAttribute("href", "#main");
});

test("the header's graphs link reaches Design from the run list, and back", async ({
  page,
}) => {
  await page.goto("/runs");
  await page.getByRole("link", { name: "Node Graphs", exact: true }).click();
  // The header still points at /graphs (task t9 renames the nav); the
  // redirect is what makes the link land on the view that draws graphs.
  await expect(page).toHaveURL(/\/design$/);
  await page.getByRole("link", { name: "Runs", exact: true }).click();
  await expect(page).toHaveURL(/\/runs$/);
});

test("the page produces no uncaught errors while rendering any sub-view", async ({
  page,
}) => {
  const pageErrors: string[] = [];
  page.on("pageerror", (error) => pageErrors.push(error.message));
  await openDesign(page);
  await page.getByRole("button", { name: "Nodes" }).click();
  await page.getByRole("button", { name: "Active graphs" }).click();
  await page.getByRole("button", { name: "Gallery" }).click();
  expect(pageErrors).toEqual([]);
});

// Sanity: the fixture module actually declares the shape this spec's other
// assertions assume — four versions across three workflow_keys, one
// deliberately orphaned run digest, and a graph size for every key.
test("fixture sanity: four versions, three workflow keys, one orphan digest", async () => {
  expect(NODE_CATALOG_WORKFLOW_VERSIONS).toHaveLength(4);
  expect(
    new Set(NODE_CATALOG_WORKFLOW_VERSIONS.map((v) => v.workflow_key)).size,
  ).toBe(3);
  expect(Object.keys(DESIGN_GRAPH_SIZES).sort()).toEqual(
    [...new Set(NODE_CATALOG_WORKFLOW_VERSIONS.map((v) => v.workflow_key))].sort(),
  );
  expect(
    WORKFLOWS_RUNS.some((run) => run.workflow_digest === ORPHAN_DIGEST),
  ).toBe(true);
});
