import type { Page } from "@playwright/test";
import type { RunView } from "../../src/api/types";
import {
  LEDGER,
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
  JOB_RUNS_PAGE_1,
  JOB_RUNS_PAGE_2,
} from "../../src/fixtures/node-runs-fixture";
import {
  WORKFLOW_VERSIONS,
  WORKFLOWS_RUNS,
} from "../../src/fixtures/workflows-fixture";

export { RUN_ID, WORKFLOW_DIGEST };
export { BOARD_RUNS };
export { JOB_RUNS_CURSOR, JOB_RUNS_PAGE_1, JOB_RUNS_PAGE_2 };
export { WORKFLOW_VERSIONS, WORKFLOWS_RUNS };

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
 */
export async function mockJobsTimelineApi(page: Page): Promise<void> {
  const allJobs = [...JOB_RUNS_PAGE_1, ...JOB_RUNS_PAGE_2];
  const runViewById = new Map<string, RunView>(
    allJobs.map((item) => [
      item.run_id,
      {
        run: {
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
 * Serve the Workflows view's slice of `/v1alpha1` (task t8): `GET
 * /v1alpha1/workflows` returns WORKFLOW_VERSIONS (two workflow_keys, three
 * versions total) and `GET /v1alpha1/runs?sort=updated_at` returns
 * WORKFLOWS_RUNS — no server-side filter by workflow, exactly the two
 * documented operations this task is scoped to. Every fixture run's own
 * `run_id` resolves through `/v1alpha1/runs/{id}` (a minimal RunView) too,
 * so following a card's recent-run link doesn't 404.
 */
export async function mockWorkflowsApi(page: Page): Promise<void> {
  const runViewById = new Map<string, RunView>(
    WORKFLOWS_RUNS.map((run) => [run.id, { run, tokens: [], node_runs: [] }]),
  );

  await page.route("**/v1alpha1/**", async (route) => {
    const url = new URL(route.request().url());
    const path = decodeURIComponent(url.pathname);

    if (path === "/v1alpha1/workflows") {
      await route.fulfill(json({ items: WORKFLOW_VERSIONS }));
      return;
    }
    if (path === "/v1alpha1/runs") {
      await route.fulfill(json({ items: WORKFLOWS_RUNS }));
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

/** The parsed contents of the page's `#agent-state` node. */
export async function readAgentState(page: Page): Promise<{
  status: string;
  route: string;
  run: {
    id: string;
    state: string;
    node_states: Record<string, string>;
    selected: string | null;
  } | null;
}> {
  const text = await page.locator("#agent-state").textContent();
  return JSON.parse(text ?? "{}");
}
