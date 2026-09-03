import { createHash } from "node:crypto";
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test, type Locator, type Page } from "@playwright/test";
import {
  DESIGN_GRAPH_SIZES,
  MESH_ACTIVE_RUN_COUNT,
  MESH_ACTOR_NODE_COUNT,
  RUN_ID,
  mockApi,
  mockDesignApi,
  mockMeshApi,
  readAgentState,
} from "./fixtures/api";
import { MESH_HISTORICAL_EVENTS } from "../src/fixtures/mesh-fixture";
import { CANVAS_SOURCE, CANVAS_VERSION } from "../src/fixtures/canvas-fixture";
import { WHOAMI_BOUND } from "../src/fixtures/whoami-fixture";

/**
 * The acceptance walkthrough, executable (task t17).
 *
 * `docs/demos/web-ui-lift/culture-nodes-lifted.html` ends in a bar of six
 * acceptance checks, and `docs/demos/web-ui-lift/README.md` maps each to the
 * claim it proves. That demo is fixtures only — it demonstrates the shape of
 * the outcome, it does not run the product. This file is its executable twin:
 * the same six checks, in the same order, against the BUILT app, and each test
 * is named for the spec id of the outcome it proves.
 *
 * The last test is the honesty gate for the delivery summary. It reads the
 * demo's own CHECKS array and this file's own source, and fails if an
 * announced outcome has no step here — so "outcome X is delivered, proved by
 * step Y" can be quoted from a run instead of asserted by a human. The map it
 * attaches (`walkthrough-outcomes.json`) is what the summary quotes.
 *
 * One deliberate naming difference from the demo, in check c31: the demo's
 * idle workflow is called `nightly-regression`, a name that exists only in the
 * demo's own fixture. The app's committed fixture publishes `deliver-change`,
 * `hello-world` and `notify-team`, and this spec serves them with ZERO runs in
 * the namespace — the same fact the demo dramatises (a published, never-run
 * workflow still has a full graph), against rows that actually exist here.
 * Renaming a fixture row to match a demo would be inventing data.
 */

test.describe.configure({ mode: "default" });

const WEB_ROOT = fileURLToPath(new URL("..", import.meta.url));
const REPO_ROOT = fileURLToPath(new URL("../..", import.meta.url));

function repoFile(rel: string): string {
  return readFileSync(join(REPO_ROOT, rel), "utf8");
}

function webFile(rel: string): string {
  return readFileSync(join(WEB_ROOT, rel), "utf8");
}

/** Every non-test module under web/src, for the single-node-component guard. */
function sourceModules(dir = "src"): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(join(WEB_ROOT, dir), { withFileTypes: true })) {
    const rel = `${dir}/${entry.name}`;
    if (entry.isDirectory()) out.push(...sourceModules(rel));
    else if (/\.tsx?$/.test(entry.name) && !/\.test\.tsx?$/.test(entry.name))
      out.push(rel);
  }
  return out;
}

async function ready(page: Page): Promise<void> {
  await expect.poll(async () => (await readAgentState(page)).status).toBe("ready");
}

/**
 * The demo's publish digest is a local SHA-256 standing in for the compiler's
 * (`docs/demos/web-ui-lift/README.md`, "What the demo deliberately does not
 * claim"). Here the stand-in lives in the FIXTURE SERVER, never in the app:
 * validate and publish both answer the digest of exactly the bytes they were
 * posted, so "the canvas publishes what the CLI publishes" is a claim about
 * the bytes the browser sent, not about arithmetic the browser did.
 */
const digestOf = (source: string) =>
  `sha256:${createHash("sha256").update(source, "utf8").digest("hex")}`;

interface CanvasSink {
  published: Array<{ source: string; digest: string }>;
}

async function mockCanvasApi(page: Page): Promise<CanvasSink> {
  const sink: CanvasSink = { published: [] };
  await page.route("**/v1alpha1/**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    if (path.endsWith("/whoami")) return route.fulfill({ json: WHOAMI_BOUND });
    if (path.endsWith("/workflows/validate")) {
      const body = request.postDataJSON() as { source: string };
      return route.fulfill({
        json: { valid: true, digest: digestOf(body.source), diagnostics: [] },
      });
    }
    if (path.endsWith("/workflows") && request.method() === "GET")
      return route.fulfill({ json: { items: [CANVAS_VERSION] } });
    if (path.endsWith("/workflows")) {
      const body = request.postDataJSON() as { source: string };
      const digest = digestOf(body.source);
      sink.published.push({ source: body.source, digest });
      return route.fulfill({
        json: { ...CANVAS_VERSION, source: body.source, digest },
      });
    }
    return route.fulfill({ status: 404, json: {} });
  });
  return sink;
}

async function expectStableFirstPaint(canvas: Locator): Promise<void> {
  await expect(canvas).toHaveAttribute("data-layout-ready", "true");
  const nodes = canvas.locator(".react-flow__node");
  await expect(nodes.first()).toBeVisible();
  const read = () =>
    nodes.evaluateAll((items) =>
      items.map((item) => ({
        id: item.getAttribute("data-id"),
        style: item.getAttribute("style"),
      })),
    );
  const firstPaint = await read();
  expect(firstPaint.length).toBeGreaterThan(0);
  await canvas.page().waitForTimeout(250);
  expect(await read()).toEqual(firstPaint);
}

/* ------------------------------------------------------------------ *
 * 1 / 6 — c30 (h20): No history replay on arrival.
 * ------------------------------------------------------------------ */

test("c30 no history replay: the mesh arrives at 0 events with 1,284 committed rows behind it", async ({
  page,
}) => {
  // The premise of the check: there IS a history to replay. Before the
  // tail-only stream, every one of these animated on arrival.
  expect(MESH_HISTORICAL_EVENTS).toHaveLength(1284);

  await mockMeshApi(page);
  await page.goto("/mesh");
  await ready(page);

  const mesh = (await readAgentState(page)).mesh!;
  expect(mesh.events_total).toBe(0);
  expect(mesh.pulses_total).toBe(0);
  // Zero events, and still a full graph: the mesh is drawn from the committed
  // /mesh, /runs and /node-runs rows, never from a replayed event stream.
  expect(mesh.actor_count).toBe(MESH_ACTOR_NODE_COUNT);
  expect(mesh.run_count).toBe(MESH_ACTIVE_RUN_COUNT);

  // And it stays 0 across the reconnect the fixture body's close provokes:
  // the resumed stream is empty, not a replay from the beginning.
  await page.waitForTimeout(600);
  const after = (await readAgentState(page)).mesh!;
  expect(after.events_total).toBe(0);
  expect(["live", "reconnecting"]).toContain(after.connection);
});

/* ------------------------------------------------------------------ *
 * 2 / 6 — c31 (h21): Idle workflows have a graph.
 * ------------------------------------------------------------------ */

test("c31 idle workflows have a graph: every published key draws from Design with 0 runs", async ({
  page,
}) => {
  await mockDesignApi(page, { runs: "none" });
  await page.goto("/design");
  await ready(page);

  const keys = Object.keys(DESIGN_GRAPH_SIZES);
  expect(keys.length).toBeGreaterThan(0);
  for (const key of keys) {
    const size = DESIGN_GRAPH_SIZES[key];
    await page.locator(`#design-workflow-list [data-workflow-key="${key}"]`).click();
    const canvas = page.locator("#design-graph");
    await expect(canvas).toHaveAttribute("data-workflow-key", key);
    await expect(canvas).toHaveAttribute("data-node-count", String(size.nodes));
    await expect(canvas).toHaveAttribute("data-edge-count", String(size.edges));
    // Drawn, not merely counted.
    await expect(canvas.locator("[data-node-id]")).toHaveCount(size.nodes);
    // The honest reason the run list is empty — a fact, not an error.
    await expect(page.locator("#design-no-runs")).toBeVisible();
  }

  const design = (await readAgentState(page)).design!;
  expect(design.run_count).toBe(0);
  expect(design.workflow_count).toBe(keys.length);
});

/* ------------------------------------------------------------------ *
 * 3 / 6 — c32 (h22): One node component everywhere.
 * ------------------------------------------------------------------ */

test("c32 one node component everywhere: Mesh, a run, the gallery and the canvas draw the same CultureNode", async ({
  page,
}) => {
  // Half the check is what renders. `.culture-node` and its two internals are
  // emitted by exactly one module, so finding them on four surfaces is the
  // DOM's own statement that one component drew all four.
  const surfaces: Array<[string, () => Promise<unknown>, string]> = [
    ["mesh", () => mockMeshApi(page), "/mesh"],
    ["run", () => mockApi(page), `/runs/${RUN_ID}`],
    ["gallery", () => mockDesignApi(page), "/design"],
    [
      "canvas",
      () => mockCanvasApi(page),
      `/design/canvas?source=${encodeURIComponent(CANVAS_SOURCE)}`,
    ],
  ];
  for (const [name, mock, url] of surfaces) {
    await page.unrouteAll({ behavior: "ignoreErrors" });
    await mock();
    await page.goto(url);
    const node = page.locator(".culture-node").first();
    await expect(node, `${name} draws a CultureNode`).toBeVisible();
    await expect(node.locator(".culture-node__core")).toHaveCount(1);
    await expect(node.locator(".culture-node__halo")).toHaveCount(1);
  }

  // The other half is that there is nothing else it COULD be: every module
  // that renders a node reaches the one component, and no other module emits
  // its class names.
  expect(webFile("src/components/WorkflowNode.tsx")).toContain(
    "culture-design/CultureNode",
  );
  for (const rel of [
    "src/routes/Mesh.tsx",
    "src/routes/Design.tsx",
    "src/routes/DesignCanvas.tsx",
    "src/components/ActiveGraphCanvas.tsx",
  ])
    expect(webFile(rel), `${rel} imports the shared node`).toContain(
      "culture-design/CultureNode",
    );
  // RunView reaches it through the WorkflowNode adapter rather than directly.
  expect(webFile("src/routes/RunView.tsx")).toContain("components/WorkflowNode");

  const emitters = sourceModules().filter((rel) =>
    webFile(rel).includes("culture-node__"),
  );
  expect(emitters).toEqual(["src/culture-design/CultureNode.tsx"]);
});

/* ------------------------------------------------------------------ *
 * 4 / 6 — c33 (h23): Canvas publish equals CLI publish.
 * ------------------------------------------------------------------ */

test("c33 canvas publish equals CLI publish: add, connect, validate, publish yields one digest", async ({
  page,
}) => {
  const sink = await mockCanvasApi(page);
  await page.goto(`/design/canvas?source=${encodeURIComponent(CANVAS_SOURCE)}`);

  // Add with the mouse, then connect the new node to an existing one.
  await page.getByRole("button", { name: "Add code" }).click();
  await expect(page.getByRole("button", { name: "node code-2, code" })).toBeVisible();
  await page.getByRole("button", { name: "Connect from selected" }).click();
  await page.getByRole("button", { name: "node finish, end" }).click();

  // Validate is the debounced round-trip the canvas already makes; Publish
  // stays disabled until the server has said the source is valid.
  const publish = page.getByRole("button", { name: "Publish" });
  await expect(publish).toBeEnabled();
  await page.getByRole("button", { name: "Source" }).click();
  const shown = (await page.getByLabel("Workflow source").textContent()) ?? "";
  expect(shown).toContain("code-2");
  expect(shown).toContain("code-2.completed");
  // The document is edited, not regenerated: the author's comment survives.
  expect(shown).toContain("# comments live here");

  await publish.click();
  await expect(page.getByRole("status")).toContainText("Published sha256:");

  expect(sink.published).toHaveLength(1);
  const canvasPublish = sink.published[0];
  // What the canvas sent is byte-for-byte what it showed the author.
  expect(canvasPublish.source).toBe(shown);
  // …and what it reported is the digest the server gave those bytes; the
  // browser never computed one.
  await expect(page.getByRole("status")).toContainText(canvasPublish.digest);

  // Now the same bytes through the CLI path — a plain POST of the source, the
  // way `nodes workflow publish` sends it, with no canvas involved.
  const cli = await page.evaluate(async (source) => {
    const response = await fetch("/v1alpha1/workflows", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ format: "yaml", source }),
    });
    return (await response.json()) as { digest: string };
  }, canvasPublish.source);
  expect(cli.digest).toBe(canvasPublish.digest);

  // The comparison means something only if the digest tracks the bytes: a
  // different source must not land on the same digest.
  const other = await page.evaluate(async (source) => {
    const response = await fetch("/v1alpha1/workflows", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ format: "yaml", source: `${source}\n  extra: 1` }),
    });
    return (await response.json()) as { digest: string };
  }, canvasPublish.source);
  expect(other.digest).not.toBe(canvasPublish.digest);
});

/* ------------------------------------------------------------------ *
 * 5 / 6 — c34 (h24): Mesh draws real relationships.
 * ------------------------------------------------------------------ */

test("c34 mesh draws real relationships: actor→machine, actor→workflow, run→actor, run→workflow, and an unknown probe", async ({
  page,
}) => {
  await mockMeshApi(page);
  await page.goto("/mesh");
  await ready(page);

  // Counted off the committed fixture rows, not off the drawing:
  // actor→machine — four of the five /mesh actors name a machine that exists
  //   (human/ori names none, and the mesh does not invent one);
  // actor→workflow — mesh-demo's `work` node pins actor://codex-thor;
  // run→actor — only run-mesh-alpha has a node-run with an actor_id;
  // run→workflow — both active runs carry sha256:mesh-wf.
  const expected: Record<string, number> = {
    "actor-machine": 4,
    "actor-workflow": 1,
    "run-actor": 1,
    "run-workflow": 2,
  };
  for (const [relation, count] of Object.entries(expected))
    await expect(
      page.locator(`.react-flow__edge.mesh-edge--${relation}`),
      `${relation} edges`,
    ).toHaveCount(count);

  const total = Object.values(expected).reduce((a, b) => a + b, 0);
  expect((await readAgentState(page)).mesh!.edge_count).toBe(total);

  // The failed probe renders as unknown, carrying the error it actually got —
  // never as a green, answering bridge.
  const failed = page.locator('[data-node-id="reachy-bridge"]');
  await expect(failed).toBeVisible();
  await expect(failed).not.toHaveText(/answering/i);
  const unknown = page.locator(".status-chip--unknown", {
    hasText: "dial tcp 10.0.0.9:8090: i/o timeout",
  });
  await expect(unknown).toBeVisible();
  expect((await readAgentState(page)).mesh!.probe_failures).toBe(1);
});

/* ------------------------------------------------------------------ *
 * 6 / 6 — c26 (h6): Fluid, no layout jump, one segmented toggle.
 * The demo's bar calls this check "fluid"; the spec id is c26.
 * ------------------------------------------------------------------ */

test("c26 fluid: no node position changes after first paint, and one segmented toggle", async ({
  page,
}) => {
  await mockApi(page);
  await page.goto(`/runs/${RUN_ID}`);
  await expect(page.locator("#view-toggle")).toHaveClass(/segmented-toggle/);
  await expectStableFirstPaint(page.locator("#run-canvas"));

  await page.unrouteAll({ behavior: "ignoreErrors" });
  await mockDesignApi(page);
  await page.goto("/design");
  await expect(page.locator("#design-toggle")).toHaveClass(/segmented-toggle/);
  await expectStableFirstPaint(page.locator("#design-graph"));
});

/* ------------------------------------------------------------------ *
 * The honesty gate: an announced outcome with no step is not delivered.
 * ------------------------------------------------------------------ */

/**
 * The demo announces its checks by id; five of them already carry their spec
 * id, and the sixth ("fluid") is c26 in the README's table. This is the only
 * place that translation is written down, and the test below fails if the demo
 * ever announces an id this map does not know.
 */
const ANNOUNCED_TO_SPEC_ID: Record<string, string> = {
  c30: "c30",
  c31: "c31",
  c32: "c32",
  c33: "c33",
  c34: "c34",
  fluid: "c26",
};

test("every outcome the demo announces has a walkthrough step that proves it", async ({}, testInfo) => {
  const demo = repoFile("docs/demos/web-ui-lift/culture-nodes-lifted.html");
  const checks = demo.slice(demo.indexOf("const CHECKS = ["));
  const announced = [
    ...checks.slice(0, checks.indexOf("];")).matchAll(/\["([a-z0-9]+)",/g),
  ].map((match) => match[1]);
  expect(announced).toHaveLength(6);
  expect(announced).toEqual(Object.keys(ANNOUNCED_TO_SPEC_ID));

  const self = webFile("e2e/walkthrough.spec.ts");
  const readme = repoFile("docs/demos/web-ui-lift/README.md");
  const map = announced.map((id) => {
    const specId = ANNOUNCED_TO_SPEC_ID[id];
    const step = self.match(
      new RegExp(`^test\\("(${specId} [^"]+)"`, "m"),
    )?.[1];
    return { announced: id, spec_id: specId, step: step ?? null };
  });
  await testInfo.attach("walkthrough-outcomes.json", {
    body: JSON.stringify(map, null, 2),
    contentType: "application/json",
  });

  // An outcome without a step is not delivered — and says so here, by name.
  expect(map.filter((entry) => entry.step === null)).toEqual([]);
  // The README is the same map in prose; it must name every spec id too.
  for (const entry of map) expect(readme).toContain(entry.spec_id);
});
