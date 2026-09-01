import { expect, test } from "@playwright/test";
import { TICKET_ID } from "../src/fixtures/ticket-fixture";
import { mockHomeApi, mockTicketApi, mockWhoami } from "./fixtures/api";

/**
 * The first-visit path (task t17, spec c25, issue #270).
 *
 * `/` used to redirect to the run table — the right landing for an operator
 * and the wrong one for the person a Jira comment just sent here. These walks
 * pin the two things that make it a path rather than a page: a signed-in
 * person gets the work view, and the ticket it names is one click away.
 */

test("a signed-in person lands on their work, not the run table", async ({ page }) => {
  await mockHomeApi(page);
  await page.goto("/");

  await expect(page).toHaveURL(/\/$/);
  await expect(
    page.getByRole("heading", { name: /waiting on a person/i }),
  ).toBeVisible();
  await expect(page.getByRole("heading", { name: "How a ticket reaches you" })).toBeVisible();
});

test("names the ticket, draws its flow, and links to the page that decides it", async ({ page }) => {
  await mockHomeApi(page);
  await page.goto("/");

  const card = page.locator(`[data-ticket-id="${TICKET_ID}"]`);
  await expect(card).toBeVisible();
  // The same diagram the ticket page leads with — five stops, one current.
  await expect(card.locator("[data-stage]")).toHaveCount(5);
  await expect(card.locator('[data-current="true"]')).toHaveCount(1);

  await card.getByRole("link", { name: `Open ${TICKET_ID}` }).click();
  await expect(page).toHaveURL(new RegExp(`/tickets/${TICKET_ID}$`));
});

test("a reader with no Access identity keeps the run table", async ({ page }) => {
  await mockHomeApi(page);
  // Registered AFTER the wide mock, so it answers first (Playwright runs the
  // last-registered route first). A 401 is "no identity reached the control
  // plane" — reads stay open on the LAN listener, so the honest landing for
  // that reader is the run list, not an empty personal queue.
  await mockWhoami(page, { status: 401 });
  await page.goto("/");

  await expect(page).toHaveURL(/\/runs$/);
});

/**
 * The ticket page's first screen (task t17, criterion 1). Asserted as
 * geometry rather than as text: at 1280x800 the flow diagram and the pending
 * decision's buttons must be inside the viewport without scrolling, and the
 * tab strip that carries claims, runs, reports and the thread must be below
 * them.
 */
test("the ticket page's first screen is the flow and the decision", async ({ page }) => {
  await mockTicketApi(page);
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto(`/tickets/${TICKET_ID}`);
  await page.getByRole("heading", { name: TICKET_ID }).waitFor();

  const rail = page.getByTestId("ticket-flow-rail");
  await expect(rail).toBeVisible();
  await expect(rail.locator("[data-stage]")).toHaveCount(5);

  const railBox = (await rail.boundingBox())!;
  expect(railBox.y + railBox.height).toBeLessThan(800);

  // The decision a person came to take, with its buttons, above the fold.
  const outcomes = page.locator(".ticket-decisions .inbox-card__outcomes").first();
  const outcomesBox = (await outcomes.boundingBox())!;
  expect(outcomesBox.y + outcomesBox.height).toBeLessThan(800);
  await expect(outcomes.getByRole("button", { name: "approved" })).toBeVisible();
  // `expired` is the engine's outcome, never a button (issue #265).
  await expect(outcomes.getByRole("button", { name: "expired" })).toHaveCount(0);

  // Everything else is behind the tab strip, and the strip starts below the
  // first screen rather than competing with the decision.
  const tabs = page.getByRole("tablist", { name: "Ticket detail" });
  const tabsBox = (await tabs.boundingBox())!;
  expect(tabsBox.y).toBeGreaterThan(outcomesBox.y);
  await expect(page.locator("#ticket-frame-claims")).toBeVisible();
  await expect(page.locator("#ticket-reply")).toHaveCount(0);

  await page.getByRole("tab", { name: "Conversation" }).click();
  await expect(page.locator("#ticket-reply")).toBeVisible();
  await expect(page.locator("#ticket-frame-claims")).toHaveCount(0);
});

test("the ticket tab strip is one tab stop, walked with the arrow keys", async ({ page }) => {
  await mockTicketApi(page);
  await page.goto(`/tickets/${TICKET_ID}`);
  await page.getByRole("tab", { name: "Claims & questions" }).focus();
  await page.keyboard.press("ArrowRight");
  await expect(page.getByRole("tab", { name: "Runs & reports" })).toBeFocused();
  await expect(page.getByRole("tab", { name: "Runs & reports" })).toHaveAttribute(
    "aria-selected",
    "true",
  );
  await page.keyboard.press("ArrowLeft");
  await expect(page.getByRole("tab", { name: "Claims & questions" })).toBeFocused();
});
