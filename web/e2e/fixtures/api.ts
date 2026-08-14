import type { Page } from "@playwright/test";
import type { RunView } from "../../src/api/types";
import {
  LEDGER,
  NODE_RUN_USAGE_ITEMS,
  RUN_EVENTS,
  RUN_ID,
  RUN_VIEW,
  WORKFLOW_DIGEST,
  WORKFLOW_VERSION,
  eventsAsSse,
} from "../../src/fixtures/run-fixture";
import { BOARD_RUNS } from "../../src/fixtures/runs-board-fixture";
import {
  JOB_RUNS_CURSOR,
  JOB_RUNS_NAMED_RUNS,
  JOB_RUNS_PAGE_1,
  JOB_RUNS_PAGE_2,
} from "../../src/fixtures/node-runs-fixture";
import {
  WORKFLOW_VERSIONS,
  WORKFLOWS_RUNS,
} from "../../src/fixtures/workflows-fixture";
import {
  ACTIVE_EVENTS,
  activeEventsAsSse,
  ACTIVE_NODE_RUNS,
} from "../../src/fixtures/active-graphs-fixture";
import {
  INVALID_VALIDATION,
  INVALID_YAML_SOURCE,
  PUBLISHED_VERSION,
  VALID_VALIDATION,
} from "../../src/fixtures/authoring-fixture";
import {
  STATS_CURSOR,
  STATS_NODE_RUNS_FILTERED,
  STATS_NODE_RUNS_PAGE_1,
  STATS_NODE_RUNS_PAGE_2,
  STATS_RUNS,
  STATS_RUNS_FILTERED,
} from "../../src/fixtures/statistics-fixture";

export { RUN_ID, WORKFLOW_DIGEST };
export { BOARD_RUNS };
export { JOB_RUNS_CURSOR, JOB_RUNS_NAMED_RUNS, JOB_RUNS_PAGE_1, JOB_RUNS_PAGE_2 };
export { WORKFLOW_VERSIONS, WORKFLOWS_RUNS };
export {
  ACTIVE_EVENTS_TOTAL,
  ACTIVE_LAST_EVENT_ID,
  ACTIVE_NODE_ID,
  ACTIVE_PULSES_TOTAL,
  ACTIVE_RUN_ID,
} from "../../src/fixtures/active-graphs-fixture";
export { INVALID_YAML_SOURCE, PUBLISHED_VERSION };
export {
  STATS_CURSOR,
  STATS_NODE_RUNS_PAGE_1,
  STATS_NODE_RUNS_PAGE_2,
  STATS_RUNS,
};
export { VALID_YAML_SOURCE } from "../../src/fixtures/authoring-fixture";

const json = (body: unknown) => ({
  status: 200,
  contentType: "application/json",
  body: JSON.stringify(body),
});

/**
 * Serve the whole `/v1alpha1` surface the Run and Ledger views read, from the
 * shared fixture, via request interception.
 *
 * The SSE route fulfils the complete committed history in one body and then
 * closes, which is exactly what a real stream does once it reaches a
 * terminal run event. This fixture's run is still `running`, so the client
 * will try to resume — `?from=<sequence>` requests are answered with an
 * empty stream, the honest answer for "nothing newer than that yet".
 */
export async function mockApi(page: Page): Promise<void> {
  await page.route("**/v1alpha1/**", async (route) => {
    const url = new URL(route.request().url());
    // The client percent-encodes path segments (a workflow digest carries a
    // `:`), so compare against the decoded path the server sees.
    const path = decodeURIComponent(url.pathname);

    if (path === "/v1alpha1/runs") {
      await route.fulfill(json({ items: [RUN_VIEW.run] }));
      return;
    }
    if (path === "/v1alpha1/node-runs") {
      // The best-effort join useRunData.ts uses to recover per-node-run
      // usage (task t2/t5) — RunView's own node_runs carry no `usage`.
      await route.fulfill(json({ items: NODE_RUN_USAGE_ITEMS }));
      return;
    }
    if (path === `/v1alpha1/runs/${RUN_ID}`) {
      await route.fulfill(json(RUN_VIEW));
      return;
    }
    if (path === `/v1alpha1/workflows/${WORKFLOW_DIGEST}`) {
      await route.fulfill(json(WORKFLOW_VERSION));
      return;
    }
    if (path === `/v1alpha1/runs/${RUN_ID}/events`) {
      const from = Number(url.searchParams.get("from") ?? "0");
      const pending = RUN_EVENTS.filter((event) => Number(event.sequence) > from);
      await route.fulfill({
        status: 200,
        headers: {
          "content-type": "text/event-stream",
          "cache-control": "no-cache",
        },
        body: eventsAsSse(pending),
      });
      return;
    }
    if (path === `/v1alpha1/runs/${RUN_ID}/ledger`) {
      await route.fulfill(json(LEDGER));
      return;
    }
    if (path.startsWith(`/v1alpha1/runs/${RUN_ID}/ledger/projections/`)) {
      const name = path.split("/").pop() ?? "";
      await route.fulfill(
        json({
          kind: name,
          subject: RUN_ID,
          items: LEDGER.items.filter(
            (record) => record.authority === "confirmed",
          ),
          digest: `sha256:projection-${name}`,
        }),
      );
      return;
    }

    await route.fulfill({
      status: 404,
      contentType: "application/json",
      body: JSON.stringify({
        code: 1,
        message: `no fixture route for ${path}`,
        remediation: "add it to e2e/fixtures/api.ts",
      }),
    });
  });
}

/**
 * Serve the runs board's slice of `/v1alpha1`: `GET /v1alpha1/runs` returns
 * BOARD_RUNS (one run per RunState column, task t14), and each of those
 * runs' own `/runs/{id}` resolves too — a minimal RunView (no tokens/node
 * runs; the board fixture only carries what `RunList` items actually have)
 * — so following a card's link all the way into the Run view doesn't 404.
 * All BOARD_RUNS share run-fixture.ts's WORKFLOW_DIGEST, so the existing
 * WORKFLOW_VERSION fixture answers every workflow lookup a followed card
 * triggers.
 */
export async function mockRunsBoardApi(page: Page): Promise<void> {
  const byId = new Map(BOARD_RUNS.map((run) => [run.id, run]));

  await page.route("**/v1alpha1/**", async (route) => {
    const url = new URL(route.request().url());
    const path = decodeURIComponent(url.pathname);

    if (path === "/v1alpha1/runs") {
      await route.fulfill(json({ items: BOARD_RUNS }));
      return;
    }
    if (path === `/v1alpha1/workflows/${WORKFLOW_DIGEST}`) {
      await route.fulfill(json(WORKFLOW_VERSION));
      return;
    }

    const boardRun = byId.get(path.replace("/v1alpha1/runs/", ""));
    if (boardRun && path === `/v1alpha1/runs/${boardRun.id}`) {
      const view: RunView = { run: boardRun, tokens: [], node_runs: [] };
      await route.fulfill(json(view));
      return;
    }
    if (boardRun && path === `/v1alpha1/runs/${boardRun.id}/events`) {
      await route.fulfill({
        status: 200,
        headers: {
          "content-type": "text/event-stream",
          "cache-control": "no-cache",
        },
        body: eventsAsSse([]),
      });
      return;
    }
    if (boardRun && path === `/v1alpha1/runs/${boardRun.id}/ledger`) {
      await route.fulfill(json({ items: [], ledger_version: 0 }));
      return;
    }
    // The shared cross-run stream (task t30, issue #46): honestly empty by
    // default — every board test below that doesn't override this route
    // gets "nothing new yet" rather than a 404 the EventSource would retry
    // against forever.
    if (path === "/v1alpha1/events") {
      await route.fulfill({
        status: 200,
        headers: {
          "content-type": "text/event-stream",
          "cache-control": "no-cache",
        },
        body: "",
      });
      return;
    }

    await route.fulfill({
      status: 404,
      contentType: "application/json",
      body: JSON.stringify({
        code: 1,
        message: `no fixture route for ${path}`,
        remediation: "add it to e2e/fixtures/api.ts",
      }),
    });
  });
}

/**
 * Serve the jobs timeline's slice of `/v1alpha1` (task t15): `GET
 * /v1alpha1/node-runs` returns JOB_RUNS_PAGE_1 with `next_cursor` set to
 * JOB_RUNS_CURSOR on the first page, and JOB_RUNS_PAGE_2 (no further
 * cursor) once the client replays that cursor back — exactly the keyset
 * pagination contract openapi.yaml's listNodeRuns describes. Every job
 * row's own `run_id` resolves through `/v1alpha1/runs/{id}` too (a minimal
 * RunView, no tokens/node runs — the list item fixture carries only what
 * `NodeRunListItem` actually has), so following a row's link doesn't 404.
 *
 * `GET /v1alpha1/runs` answers JOB_RUNS_NAMED_RUNS (task t5): JobsTimeline
 * separately fetches it to join a name/category onto rows whose `run_id`
 * matches one of those two named/hinted fixture runs — the node-runs
 * listing itself carries neither.
 */
export async function mockJobsTimelineApi(page: Page): Promise<void> {
  const allJobs = [...JOB_RUNS_PAGE_1, ...JOB_RUNS_PAGE_2];
  const namedById = new Map(JOB_RUNS_NAMED_RUNS.map((run) => [run.id, run]));
  const runViewById = new Map<string, RunView>(
    allJobs.map((item) => [
      item.run_id,
      {
        run: namedById.get(item.run_id) ?? {
          id: item.run_id,
          workflow_digest: WORKFLOW_DIGEST,
          state: "running",
          created_at: item.created_at,
          updated_at: item.updated_at,
        },
        tokens: [],
        node_runs: [],
      },
    ]),
  );

  await page.route("**/v1alpha1/**", async (route) => {
    const url = new URL(route.request().url());
    const path = decodeURIComponent(url.pathname);

    if (path === "/v1alpha1/node-runs") {
      if (url.searchParams.get("cursor") === JOB_RUNS_CURSOR) {
        await route.fulfill(json({ items: JOB_RUNS_PAGE_2 }));
        return;
      }
      await route.fulfill(
        json({ items: JOB_RUNS_PAGE_1, next_cursor: JOB_RUNS_CURSOR }),
      );
      return;
    }
    if (path === "/v1alpha1/runs") {
      await route.fulfill(json({ items: JOB_RUNS_NAMED_RUNS }));
      return;
    }
    if (path === `/v1alpha1/workflows/${WORKFLOW_DIGEST}`) {
      await route.fulfill(json(WORKFLOW_VERSION));
      return;
    }

    const runId = path.replace("/v1alpha1/runs/", "");
    const runView = runViewById.get(runId);
    if (runView && path === `/v1alpha1/runs/${runId}`) {
      await route.fulfill(json(runView));
      return;
    }
    if (runView && path === `/v1alpha1/runs/${runId}/events`) {
      await route.fulfill({
        status: 200,
        headers: {
          "content-type": "text/event-stream",
          "cache-control": "no-cache",
        },
        body: eventsAsSse([]),
      });
      return;
    }
    if (runView && path === `/v1alpha1/runs/${runId}/ledger`) {
      await route.fulfill(json({ items: [], ledger_version: 0 }));
      return;
    }

    await route.fulfill({
      status: 404,
      contentType: "application/json",
      body: JSON.stringify({
        code: 1,
        message: `no fixture route for ${path}`,
        remediation: "add it to e2e/fixtures/api.ts",
      }),
    });
  });
}

/**
 * Serve the Statistics view's slice of `/v1alpha1` (task t6): `GET
 * /v1alpha1/node-runs` paginates STATS_NODE_RUNS_PAGE_1 -> STATS_CURSOR ->
 * STATS_NODE_RUNS_PAGE_2 for an unbounded request (proving the view walks
 * every page before aggregating), and answers STATS_NODE_RUNS_FILTERED —
 * a deliberately different, smaller dataset — the moment the request
 * carries an `updated_since`/`updated_until` bound, so a spec can prove the
 * time filter genuinely drives a different aggregate rather than the view
 * silently keeping the unfiltered totals on screen. `GET /v1alpha1/runs`
 * (the category join, task t5's pattern) mirrors the same bounded/unbounded
 * split with STATS_RUNS / STATS_RUNS_FILTERED.
 */
export async function mockStatisticsApi(page: Page): Promise<void> {
  await page.route("**/v1alpha1/**", async (route) => {
    const url = new URL(route.request().url());
    const path = decodeURIComponent(url.pathname);
    const bounded =
      url.searchParams.has("updated_since") ||
      url.searchParams.has("updated_until");

    if (path === "/v1alpha1/node-runs") {
      if (bounded) {
        await route.fulfill(json({ items: STATS_NODE_RUNS_FILTERED }));
        return;
      }
      if (url.searchParams.get("cursor") === STATS_CURSOR) {
        await route.fulfill(json({ items: STATS_NODE_RUNS_PAGE_2 }));
        return;
      }
      await route.fulfill(
        json({ items: STATS_NODE_RUNS_PAGE_1, next_cursor: STATS_CURSOR }),
      );
      return;
    }
    if (path === "/v1alpha1/runs") {
      await route.fulfill(
        json({ items: bounded ? STATS_RUNS_FILTERED : STATS_RUNS }),
      );
      return;
    }

    await route.fulfill({
      status: 404,
      contentType: "application/json",
      body: JSON.stringify({
        code: 1,
        message: `no fixture route for ${path}`,
        remediation: "add it to e2e/fixtures/api.ts",
      }),
    });
  });
}

/**
 * Serve the Node Graphs tab's "Node Graphs" sub-tab (task t8, re-homed under
 * task t28's tab shell): `GET /v1alpha1/workflows` returns WORKFLOW_VERSIONS
 * (two workflow_keys, three versions total) and `GET
 * /v1alpha1/runs?sort=updated_at` returns WORKFLOWS_RUNS — no server-side
 * filter by workflow, exactly the two documented operations this task is
 * scoped to. Every fixture run's own `run_id` resolves through
 * `/v1alpha1/runs/{id}` (a minimal RunView) too, so following a card's
 * recent-run link doesn't 404.
 *
 * The Nodes sub-tab (task t29's catalog, rendered by t31) derives from the
 * same workflows listing. The Active Graphs sub-tab (task t31) additionally
 * reads `GET /v1alpha1/node-runs` (ACTIVE_NODE_RUNS: one running row on the
 * one non-terminal run) and the cross-run SSE stream `GET /v1alpha1/events`
 * (ACTIVE_EVENTS: one committed event on the known run — a visible pulse —
 * and one naming a run the view never loaded, which must be a no-op, h14).
 * The events route honours both resume spellings exactly like mockMeshApi.
 */
export async function mockNodeGraphsApi(page: Page): Promise<void> {
  const runViewById = new Map<string, RunView>(
    WORKFLOWS_RUNS.map((run) => [run.id, { run, tokens: [], node_runs: [] }]),
  );

  await page.route("**/v1alpha1/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = decodeURIComponent(url.pathname);

    if (path === "/v1alpha1/workflows") {
      await route.fulfill(json({ items: WORKFLOW_VERSIONS }));
      return;
    }
    if (path === "/v1alpha1/runs") {
      await route.fulfill(json({ items: WORKFLOWS_RUNS }));
      return;
    }
    if (path === "/v1alpha1/node-runs") {
      await route.fulfill(json({ items: ACTIVE_NODE_RUNS }));
      return;
    }
    if (path === "/v1alpha1/events") {
      const headers = await request.allHeaders();
      const from = headers["last-event-id"] ?? url.searchParams.get("from") ?? "";
      const pending = ACTIVE_EVENTS.filter((event) => event.id > from);
      await route.fulfill({
        status: 200,
        headers: {
          "content-type": "text/event-stream",
          "cache-control": "no-cache",
        },
        body: activeEventsAsSse(pending),
      });
      return;
    }

    const runId = path.replace("/v1alpha1/runs/", "");
    const runView = runViewById.get(runId);
    if (runView && path === `/v1alpha1/runs/${runId}`) {
      await route.fulfill(json(runView));
      return;
    }
    if (runView && path === `/v1alpha1/runs/${runId}/events`) {
      await route.fulfill({
        status: 200,
        headers: {
          "content-type": "text/event-stream",
          "cache-control": "no-cache",
        },
        body: eventsAsSse([]),
      });
      return;
    }
    if (runView && path === `/v1alpha1/runs/${runId}/ledger`) {
      await route.fulfill(json({ items: [], ledger_version: 0 }));
      return;
    }
    const digest = path.replace("/v1alpha1/workflows/", "");
    const version = WORKFLOW_VERSIONS.find((v) => v.digest === digest);
    if (version && path === `/v1alpha1/workflows/${digest}`) {
      await route.fulfill(json(version));
      return;
    }

    await route.fulfill({
      status: 404,
      contentType: "application/json",
      body: JSON.stringify({
        code: 1,
        message: `no fixture route for ${path}`,
        remediation: "add it to e2e/fixtures/api.ts",
      }),
    });
  });
}

/**
 * Serve the authoring slice's endpoints (task t9): `POST
 * /v1alpha1/workflows/validate` and `POST /v1alpha1/workflows`, plus the
 * Workflows list endpoints (`GET /v1alpha1/workflows`, `GET
 * /v1alpha1/runs?sort=updated_at`) so a spec can reach `/workflows/new` the
 * way an operator actually would: via the "New workflow" link on `/workflows`
 * (task t8's view), rather than a direct `page.goto`.
 *
 * Validate/publish decide their response by inspecting the request body's
 * `source` — whichever of INVALID_YAML_SOURCE/anything-else was actually
 * POSTed — the same "the fixture answers what the client actually sent"
 * discipline every other mock* here follows. Publish echoes the exact
 * `source` string it received back onto the returned WorkflowVersion, which
 * is what lets a spec assert digest-identity/byte-fidelity end to end: the
 * response's `source` field only ever holds what the client actually sent
 * over the wire, never a value the fixture invented.
 */
export async function mockAuthoringApi(page: Page): Promise<void> {
  await page.route("**/v1alpha1/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = decodeURIComponent(url.pathname);
    const method = request.method();

    if (path === "/v1alpha1/workflows/validate" && method === "POST") {
      const body = request.postDataJSON() as { source: string };
      await route.fulfill(
        json(body.source.includes("does-not-exist") ? INVALID_VALIDATION : VALID_VALIDATION),
      );
      return;
    }
    if (path === "/v1alpha1/workflows" && method === "POST") {
      const body = request.postDataJSON() as {
        source: string;
        format?: string;
      };
      await route.fulfill({
        status: 201,
        contentType: "application/json",
        body: JSON.stringify({ ...PUBLISHED_VERSION, source: body.source }),
      });
      return;
    }
    if (path === "/v1alpha1/workflows" && method === "GET") {
      await route.fulfill(json({ items: WORKFLOW_VERSIONS }));
      return;
    }
    if (path === "/v1alpha1/runs" && method === "GET") {
      await route.fulfill(json({ items: WORKFLOWS_RUNS }));
      return;
    }
    const runId = path.replace("/v1alpha1/runs/", "");
    const workflowsRun = WORKFLOWS_RUNS.find((run) => run.id === runId);
    if (workflowsRun && path === `/v1alpha1/runs/${runId}`) {
      await route.fulfill(
        json({ run: workflowsRun, tokens: [], node_runs: [] }),
      );
      return;
    }

    await route.fulfill({
      status: 404,
      contentType: "application/json",
      body: JSON.stringify({
        code: 1,
        message: `no fixture route for ${method} ${path}`,
        remediation: "add it to e2e/fixtures/api.ts",
      }),
    });
  });
}

import {
  MESH_ACTORS,
  MESH_EVENTS,
  MESH_NODE_RUNS,
  MESH_RUNS,
  meshEventsAsSse,
} from "../../src/fixtures/mesh-fixture";

export {
  MESH_ACTIVE_RUN_COUNT,
  MESH_ACTOR_NODE_COUNT,
  MESH_EVENTS_TOTAL,
  MESH_LAST_EVENT_ID,
  MESH_PULSES_TOTAL,
} from "../../src/fixtures/mesh-fixture";

/**
 * Serve the Mesh view's slice of `/v1alpha1` (task t18): the actors listing
 * (5 rows, two of them revisions of the same actor_key), the runs list
 * (active + terminal), the node-runs rows that attribute runs to actors,
 * and `GET /v1alpha1/events` — the cross-run SSE stream — replaying
 * MESH_EVENTS exactly as writeCrossRunSSEEvent frames them, then closing.
 *
 * Resume honesty: the events route honours BOTH resume spellings the real
 * endpoint accepts — the `Last-Event-ID` header a reconnecting EventSource
 * sends automatically, and the `?from=` parameter the client uses when it
 * reopens explicitly — replaying only events with id strictly greater than
 * the cursor. After the body closes the browser reconnects with the last
 * ULID and receives an empty stream: the honest answer for "nothing newer
 * yet", and the proof that no event is ever double-counted.
 */
export async function mockMeshApi(page: Page): Promise<void> {
  await page.route("**/v1alpha1/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = decodeURIComponent(url.pathname);

    if (path === "/v1alpha1/actors") {
      await route.fulfill(json({ items: MESH_ACTORS }));
      return;
    }
    if (path === "/v1alpha1/runs") {
      await route.fulfill(json({ items: MESH_RUNS }));
      return;
    }
    if (path === "/v1alpha1/node-runs") {
      await route.fulfill(json({ items: MESH_NODE_RUNS }));
      return;
    }
    if (path === "/v1alpha1/events") {
      const headers = await request.allHeaders();
      const from = headers["last-event-id"] ?? url.searchParams.get("from") ?? "";
      const pending = MESH_EVENTS.filter((event) => event.id > from);
      await route.fulfill({
        status: 200,
        headers: {
          "content-type": "text/event-stream",
          "cache-control": "no-cache",
        },
        body: meshEventsAsSse(pending),
      });
      return;
    }

    await route.fulfill({
      status: 404,
      contentType: "application/json",
      body: JSON.stringify({
        code: 1,
        message: `no fixture route for ${path}`,
        remediation: "add it to e2e/fixtures/api.ts",
      }),
    });
  });
}

import {
  PLAN_IMPORT,
  PLAN_IMPORT_SUMMARIES,
  PLAN_SLUG,
} from "../../src/fixtures/plan-fixture";

export { PLAN_IMPORT, PLAN_SLUG };

/**
 * Serve the Plan view's slice of `/v1alpha1` (task t23): `GET
 * /v1alpha1/plan-imports?slug=` answers PLAN_IMPORT_SUMMARIES (two
 * snapshots, most recent first) for PLAN_SLUG and an empty list for any
 * other slug, and `GET /v1alpha1/plan-imports/{id}` resolves the most
 * recent snapshot's full tasks/deviations — the same fixture
 * src/routes/PlanView.test.tsx exercises via mocked client calls, served
 * here over the real network-mocked route instead.
 */
export async function mockPlanApi(page: Page): Promise<void> {
  await page.route("**/v1alpha1/**", async (route) => {
    const url = new URL(route.request().url());
    const path = decodeURIComponent(url.pathname);

    if (path === "/v1alpha1/plan-imports") {
      const slug = url.searchParams.get("slug");
      await route.fulfill(
        json({ items: slug === PLAN_SLUG ? PLAN_IMPORT_SUMMARIES : [] }),
      );
      return;
    }
    if (path === `/v1alpha1/plan-imports/${PLAN_IMPORT.id}`) {
      await route.fulfill(json(PLAN_IMPORT));
      return;
    }
    // The one spec that reaches the Plan view via the header link starts
    // on the Runs list — honestly empty, and the shared app-wide SSE
    // stream (task t27) is served the same "nothing new yet" empty body
    // every other mock*Api here answers it with, so it never retries.
    if (path === "/v1alpha1/runs") {
      await route.fulfill(json({ items: [] }));
      return;
    }
    if (path === "/v1alpha1/events") {
      await route.fulfill({
        status: 200,
        headers: {
          "content-type": "text/event-stream",
          "cache-control": "no-cache",
        },
        body: "",
      });
      return;
    }

    await route.fulfill({
      status: 404,
      contentType: "application/json",
      body: JSON.stringify({
        code: 1,
        message: `no fixture route for ${path}`,
        remediation: "add it to e2e/fixtures/api.ts",
      }),
    });
  });
}

/** The parsed contents of the page's `#agent-state` node. */
export async function readAgentState(page: Page): Promise<{
  status: string;
  route: string;
  run: {
    id: string;
    state: string;
    node_states: Record<string, string>;
    selected: string | null;
    name?: string | null;
    display_hint?: string | null;
    category?: string | null;
    usage?: {
      input_tokens: number;
      output_tokens: number;
      cost: number | null;
      currency: string | null;
      reported: boolean;
    } | null;
  } | null;
  authoring?: {
    step: string;
    valid: boolean | null;
    diagnostics_count: number;
    digest: string | null;
  } | null;
  statistics?: {
    total_runs: number;
    reported_runs: number;
    excluded_runs: number;
    total_input_tokens: number;
    total_output_tokens: number;
    avg_input_tokens: number | null;
    median_input_tokens: number | null;
    avg_output_tokens: number | null;
    median_output_tokens: number | null;
    cost_currency: string | null;
    avg_cost: number | null;
    median_cost: number | null;
    category_count: number;
  } | null;
  mesh?: {
    actor_count: number;
    run_count: number;
    edge_count: number;
    connection: string;
    last_event_id: string | null;
    events_total: number;
    pulses_total: number;
    reduced_motion: boolean;
  } | null;
  active_graphs?: {
    graph_count: number;
    active_run_count: number;
    active_node_count: number;
    connection: string;
    last_event_id: string | null;
    events_total: number;
    pulses_total: number;
    reduced_motion: boolean;
  } | null;
}> {
  const text = await page.locator("#agent-state").textContent();
  return JSON.parse(text ?? "{}");
}
