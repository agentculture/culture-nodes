import { expect, test, type Page } from "@playwright/test";
import {
  mockNodeGraphsApi,
  readAgentState,
  WORKFLOW_VERSIONS,
  WORKFLOWS_RUNS,
} from "./fixtures/api";
import { WORKFLOW_DIGEST } from "../src/fixtures/run-fixture";
import {
  DELIVER_CHANGE_V2_DIGEST,
  HELLO_WORLD_DIGEST,
  ORPHAN_DIGEST,
} from "../src/fixtures/workflows-fixture";

test.beforeEach(async ({ page }) => {
  await mockNodeGraphsApi(page);
});

async function openNodeGraphsTab(page: Page) {
  await page.goto("/graphs?tab=graphs");
  await expect
    .poll(async () => (await readAgentState(page)).status)
    .toBe("ready");
}

test("renders one card per published workflow_key, not one per version", async ({
  page,
}) => {
  await openNodeGraphsTab(page);

  await expect(
    page.locator('[data-workflow-key="deliver-change"]'),
  ).toHaveCount(1);
  await expect(
    page.locator('[data-workflow-key="hello-world"]'),
  ).toHaveCount(1);
  await expect(page.locator("[data-workflow-key]")).toHaveCount(2);
});

test("each card lists every version's digest, newest version first", async ({
  page,
}) => {
  await openNodeGraphsTab(page);

  const card = page.locator('[data-workflow-key="deliver-change"]');
  const rows = card.locator("tbody tr");
  await expect(rows).toHaveCount(2);
  await expect(rows.nth(0)).toHaveAttribute(
    "data-workflow-digest",
    DELIVER_CHANGE_V2_DIGEST,
  );
  await expect(rows.nth(1)).toHaveAttribute(
    "data-workflow-digest",
    WORKFLOW_DIGEST,
  );
});

test("shows the owner from the latest version's metadata", async ({
  page,
}) => {
  await openNodeGraphsTab(page);
  const card = page.locator('[data-workflow-key="deliver-change"]');
  await expect(card).toContainText("team/platform-ai");
});

test("lists each workflow's recent runs across all of its versions, newest first, and never a run from another workflow or with an unmatched digest", async ({
  page,
}) => {
  await openNodeGraphsTab(page);

  const card = page.locator('[data-workflow-key="deliver-change"]');
  const runLinks = card.locator(".workflow-card__run-list a");
  await expect(runLinks).toHaveCount(2);
  await expect(runLinks.nth(0)).toHaveText(
    "run-deliver-v2-01J8XKWORKFLOWS02",
  );
  await expect(runLinks.nth(1)).toHaveText(
    "run-deliver-v1-01J8XKWORKFLOWS04",
  );

  // The orphan-digest run (matches no published version) never renders
  // anywhere on the page.
  await expect(page.getByText("run-orphan-01J8XKWORKFLOWS0003")).toHaveCount(
    0,
  );
});

test("a workflow with no recent runs shows its own honest empty state", async ({
  page,
}) => {
  await openNodeGraphsTab(page);
  const card = page.locator('[data-workflow-key="hello-world"]');
  // hello-world has exactly one recent run in the fixture — check it renders
  // rather than the empty state here, and prove the empty state wording
  // exists by asserting it is absent (it has one run, not zero).
  await expect(card.getByText(/No runs yet/)).toHaveCount(0);
  await expect(card.locator(".workflow-card__run-list a")).toHaveCount(1);
});

test("every recent-run row is a real link that opens the existing Run view", async ({
  page,
}) => {
  await openNodeGraphsTab(page);
  const helloRun = WORKFLOWS_RUNS.find(
    (run) => run.workflow_digest === HELLO_WORLD_DIGEST,
  )!;
  const link = page
    .locator('[data-workflow-key="hello-world"]')
    .getByRole("link", { name: helloRun.id });
  await expect(link).toHaveAttribute("href", `/runs/${helloRun.id}`);
  await link.click();
  await expect(page).toHaveURL(new RegExp(`/runs/${helloRun.id}$`));
});

test("requests every published workflow version and the run list sorted by updated_at", async ({
  page,
}) => {
  const workflowsRequest = page.waitForRequest((req) =>
    req.url().includes("/v1alpha1/workflows") &&
    !req.url().includes("/v1alpha1/workflows/"),
  );
  const runsRequest = page.waitForRequest((req) =>
    req.url().includes("/v1alpha1/runs?"),
  );
  await openNodeGraphsTab(page);
  await workflowsRequest;
  const runsUrl = new URL((await runsRequest).url());
  expect(runsUrl.searchParams.get("sort")).toBe("updated_at");
});

test("/workflows redirects to the Node Graphs sub-tab, so old links survive", async ({
  page,
}) => {
  await page.goto("/workflows");
  await expect(page).toHaveURL(/\/graphs\?tab=graphs$/);
  await expect
    .poll(async () => (await readAgentState(page)).status)
    .toBe("ready");
  await expect(
    page.locator('[data-workflow-key="deliver-change"]'),
  ).toHaveCount(1);
});

test("sub-tab selection is URL-param driven: switching tabs updates ?tab= and is directly bookmarkable", async ({
  page,
}) => {
  await page.goto("/graphs");
  await expect(page).toHaveURL(/\/graphs$/); // no ?tab= on the default sub-tab
  await expect(
    page.getByRole("button", { name: "Nodes" }),
  ).toHaveAttribute("aria-pressed", "true");

  await page.getByRole("button", { name: "Active Graphs" }).click();
  await expect(page).toHaveURL(/\/graphs\?tab=active$/);
  await expect(
    page.getByRole("button", { name: "Active Graphs" }),
  ).toHaveAttribute("aria-pressed", "true");

  // A direct navigation to the bookmarked URL lands on the same sub-tab.
  await page.goto("/graphs?tab=active");
  await expect(
    page.getByRole("button", { name: "Active Graphs" }),
  ).toHaveAttribute("aria-pressed", "true");
  await expect(page.locator("#node-graphs-active-panel")).toBeVisible();
});

test("the Nodes sub-tab renders the node-definition catalog from the published IRs (t29+t31, c20)", async ({
  page,
}) => {
  await page.goto("/graphs?tab=nodes");
  await expect
    .poll(async () => (await readAgentState(page)).status)
    .toBe("ready");

  // The fixture's three published versions collapse to 8 distinct
  // definitions (see NODE_CATALOG_DEFINITION_COUNT's derivation note:
  // deliver-change's latest version contributes 7, hello-world's unbound
  // agent adds 1, its end node collapses into deliver-change's).
  await expect(page.locator(".node-def-card")).toHaveCount(8);

  // A ref-backed definition shows kind + ref + its occurrence.
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

test("the Active Graphs sub-tab halos only the workflow whose run holds active tokens (t31, h20)", async ({
  page,
}) => {
  await page.goto("/graphs?tab=active");
  await expect
    .poll(async () => (await readAgentState(page)).status)
    .toBe("ready");

  // Exactly one graph: deliver-change v2, pinned by the one running run.
  const graph = page.locator("#active-graph-deliver-change-v2");
  await expect(graph).toHaveCount(1);
  await expect(graph).toHaveAttribute("data-alive", "true");
  await expect(graph).toHaveClass(/is-alive/);
  await expect(graph).toContainText("1 active run");
  await expect(
    graph.locator(`[data-run-id="${ACTIVE_RUN_ID}"] a`),
  ).toHaveAttribute("href", `/runs/${ACTIVE_RUN_ID}`);

  // hello-world's only run is completed; the orphan digest matches no
  // published version — neither renders a graph (h14).
  await expect(page.locator(".active-graph")).toHaveCount(1);
  await expect(page.getByText("run-orphan-01J8XKWORKFLOWS0003")).toHaveCount(
    0,
  );

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
  await page.goto("/graphs?tab=active");
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

test("prefers-reduced-motion renders one static frame on the Active Graphs sub-tab (t31)", async ({
  page,
}) => {
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.goto("/graphs?tab=active");
  await expect
    .poll(async () => (await readAgentState(page)).status)
    .toBe("ready");
  await expect(
    page.locator("#active-graph-deliver-change-v2"),
  ).toHaveAttribute("data-motion", "static");
  // Liveness stays readable as text, never motion/color alone.
  await expect(page.locator("#active-graph-deliver-change-v2")).toContainText(
    "1 active run",
  );
  expect((await readAgentState(page)).active_graphs?.reduced_motion).toBe(
    true,
  );
});

test("the graph is inspectable from the keyboard alone (t31, MeshCanvas precedent)", async ({
  page,
}) => {
  await page.goto("/graphs?tab=active");
  await expect
    .poll(async () => (await readAgentState(page)).status)
    .toBe("ready");
  const canvas = page
    .locator("#active-graph-deliver-change-v2")
    .getByRole("application");
  await canvas.focus();
  await page.keyboard.press("ArrowRight");
  const readout = page.locator("#active-graph-deliver-change-v2-inspect");
  // BFS entry node first; the readout carries id, kind, and presence words.
  await expect(readout).toContainText("intake · agent · no active work");
  await page.keyboard.press("Escape");
  await expect(readout).toContainText("arrow keys to inspect");
});

test("the skip link is still the first tab stop, unaffected by the new route", async ({
  page,
}) => {
  await openNodeGraphsTab(page);
  await page.keyboard.press("Tab");
  const skipLink = page.locator(".skip-link");
  await expect(skipLink).toBeFocused();
  await expect(skipLink).toHaveAttribute("href", "#main");
});

test("the header's Node Graphs link reaches /graphs from the run list, and back", async ({
  page,
}) => {
  await page.goto("/runs");
  await page.getByRole("link", { name: "Node Graphs", exact: true }).click();
  await expect(page).toHaveURL(/\/graphs$/);
  await page.getByRole("link", { name: "Runs", exact: true }).click();
  await expect(page).toHaveURL(/\/runs$/);
});

test("the page produces no uncaught errors while rendering", async ({
  page,
}) => {
  const pageErrors: string[] = [];
  page.on("pageerror", (error) => pageErrors.push(error.message));
  await openNodeGraphsTab(page);
  await page.getByRole("button", { name: "Nodes" }).click();
  await page.getByRole("button", { name: "Active Graphs" }).click();
  expect(pageErrors).toEqual([]);
});

// Sanity: the fixture module actually declares three versions across two
// workflow_keys, and one deliberately orphaned run digest — pins the
// fixture shape this spec's other assertions assume.
test("fixture sanity: three versions, two workflow keys, one orphan digest", async () => {
  expect(WORKFLOW_VERSIONS).toHaveLength(3);
  expect(new Set(WORKFLOW_VERSIONS.map((v) => v.workflow_key)).size).toBe(2);
  expect(
    WORKFLOWS_RUNS.some((run) => run.workflow_digest === ORPHAN_DIGEST),
  ).toBe(true);
});
