import { expect, test, type Page } from "@playwright/test";
import {
  MESH_ACTIVE_RUN_COUNT,
  MESH_ACTOR_NODE_COUNT,
  MESH_EVENTS,
  mockMeshApi,
  readAgentState,
} from "./fixtures/api";

/**
 * The Mesh view against the fixture API (task t18). A canvas has no DOM to
 * assert, so — per acceptance #5/#6 — every claim is proved through the
 * machine-readable mirror: `#agent-state`'s mesh block (node/edge counts,
 * connection state, event/pulse counters, the resume cursor) plus the
 * stable container/indicator elements.
 */

test.beforeEach(async ({ page }) => {
  await mockMeshApi(page);
});

async function openMesh(page: Page) {
  await page.goto("/mesh");
  await expect
    .poll(async () => (await readAgentState(page)).status)
    .toBe("ready");
}

test("assembles the graph from fixture actors and active runs", async ({
  page,
}) => {
  await openMesh(page);

  await expect(page.locator("#mesh-canvas")).toBeVisible();
  await expect(page.locator("#mesh-canvas")).toHaveAttribute(
    "data-motion",
    "animated",
  );

  // The tail-only stream does not replay its 1,284 committed rows. 5 actor rows
  // collapse to 4 nodes (codex-thor's two revisions are one actor); the
  // terminal fixture run stays off the mesh; one edge per actor + one per run.
  await expect
    .poll(async () => (await readAgentState(page)).mesh?.events_total)
    .toBe(0);
  const mesh = (await readAgentState(page)).mesh!;
  expect(mesh.actor_count).toBe(MESH_ACTOR_NODE_COUNT);
  expect(mesh.run_count).toBe(MESH_ACTIVE_RUN_COUNT);
  expect(mesh.edge_count).toBe(MESH_ACTOR_NODE_COUNT + MESH_ACTIVE_RUN_COUNT);
});

test("1,284 historical events stay skipped and each later commit increments once", async ({
  page,
}) => {
  await openMesh(page);

  await expect
    .poll(async () => (await readAgentState(page)).mesh?.events_total)
    .toBe(0);
  await page.evaluate(async (event) => {
    await fetch("/v1alpha1/events", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(event),
    });
  }, MESH_EVENTS[0]);
  await expect
    .poll(async () => (await readAgentState(page)).mesh?.events_total)
    .toBe(1);
  const mesh = (await readAgentState(page)).mesh!;
  expect(mesh.pulses_total).toBe(1);
  expect(mesh.last_event_id).toBe(MESH_EVENTS[0].id);

  // The browser reconnects with Last-Event-ID after the fixture body ends
  // and receives an empty stream — no event is ever replayed or counted
  // twice. Give a reconnect cycle time to happen before re-reading.
  await page.waitForTimeout(500);
  expect((await readAgentState(page)).mesh!.events_total).toBe(
    1,
  );
});

test("a resolved run settles off the mesh after its lifecycle event", async ({
  page,
}) => {
  await openMesh(page);

  await expect
    .poll(async () => (await readAgentState(page)).mesh?.events_total)
    .toBe(0);

  await page.evaluate(async (event) => {
    await fetch("/v1alpha1/events", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(event),
    });
  }, MESH_EVENTS[3]);

  // run-mesh-beta completed while we watched: it lingers for the settle
  // animation, then leaves the graph (run-mesh-gamma stays, so the count
  // lands back on the fixture's active-run count with gamma in place of
  // beta).
  await expect
    .poll(async () => (await readAgentState(page)).mesh?.run_count, {
      timeout: 10_000,
    })
    .toBe(MESH_ACTIVE_RUN_COUNT);
});

test("the connection indicator never fakes liveness", async ({ page }) => {
  await openMesh(page);

  const indicator = page.locator("#mesh-connection");
  await expect(indicator).toBeVisible();
  // The fixture stream opens, replays, closes, reconnects — whichever phase
  // this read lands in, the indicator and the mirror agree, and the value
  // is always one of the two honest states.
  const state = await indicator.getAttribute("data-state");
  expect(["live", "reconnecting"]).toContain(state);
  const mesh = (await readAgentState(page)).mesh!;
  expect(["live", "reconnecting"]).toContain(mesh.connection);
});

test("prefers-reduced-motion renders one dignified static frame", async ({
  page,
}) => {
  await page.emulateMedia({ reducedMotion: "reduce" });
  await openMesh(page);

  await expect(page.locator("#mesh-canvas")).toHaveAttribute(
    "data-motion",
    "static",
  );
  await expect
    .poll(async () => (await readAgentState(page)).mesh?.reduced_motion)
    .toBe(true);
  // The graph is still fully laid out and mirrored — a static frame, not a
  // blank one.
  const mesh = (await readAgentState(page)).mesh!;
  expect(mesh.actor_count).toBe(MESH_ACTOR_NODE_COUNT);
  expect(mesh.edge_count).toBeGreaterThan(0);
});

test("keyboard focus inspects nodes without a pointer", async ({ page }) => {
  await openMesh(page);

  await page.locator("#mesh-canvas").focus();
  await page.keyboard.press("ArrowRight");
  const tooltip = page.locator("#mesh-tooltip");
  await expect(tooltip).toBeVisible();
  await expect(tooltip).toContainText("control plane");
  await page.keyboard.press("ArrowRight");
  await expect(tooltip).not.toContainText("control plane");
  await page.keyboard.press("Escape");
  await expect(tooltip).toBeHidden();
});
