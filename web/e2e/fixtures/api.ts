import type { Page } from "@playwright/test";
import {
  LEDGER,
  RUN_EVENTS,
  RUN_ID,
  RUN_VIEW,
  WORKFLOW_DIGEST,
  WORKFLOW_VERSION,
  eventsAsSse,
} from "../../src/fixtures/run-fixture";

export { RUN_ID, WORKFLOW_DIGEST };

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
