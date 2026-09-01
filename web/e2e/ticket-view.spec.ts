import { expect, test } from "@playwright/test";
import { TICKET_ID, TICKET_PROJECTION } from "../src/fixtures/ticket-fixture";
import { WHOAMI_ACTOR_ID, WHOAMI_EMAIL } from "../src/fixtures/whoami-fixture";
import { mockWhoami } from "./fixtures/api";

/**
 * The keyboard walk (task t18), re-cut for the derived identity model (task
 * t9, spec c8/c9): nothing is typed but the reply. The page learns who is
 * here from `GET /v1alpha1/whoami` — behind Cloudflare Access the edge
 * cookie carries the identity on every same-origin request — so the reply
 * names whoami's actor, no Authorization header is sent, and no token is
 * held anywhere in the browser.
 */
test("keyboard walk reaches the reply box and submits as the signed-in actor", async ({ page }) => {
  let submitted: { authorization?: string; body?: unknown } | undefined;
  await mockWhoami(page);
  await page.route("**/v1alpha1/tickets/**", async (route) => {
    if (route.request().method() === "POST") {
      submitted = {
        authorization: route.request().headers().authorization,
        body: route.request().postDataJSON(),
      };
      await route.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: "reply-e2e", replier: WHOAMI_ACTOR_ID, text: "Ship it", created_at: "2026-08-29T10:00:00Z" }) });
      return;
    }
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(TICKET_PROJECTION) });
  });
  await page.goto(`/tickets/${TICKET_ID}`);
  await expect(page.getByRole("heading", { name: TICKET_ID })).toBeVisible();
  await expect(page.locator("#app-header-identity")).toHaveText(`signed in as ${WHOAMI_EMAIL}`);
  await expect(page.locator("#ticket-token")).toHaveCount(0);
  await expect(page.locator("#ticket-replier")).toHaveCount(0);
  await expect(page.locator('input[type="password"]')).toHaveCount(0);

  await page.keyboard.press("Tab");
  const focused = page.locator("*:focus");
  for (let count = 0; count < 40; count += 1) {
    if ((await focused.count()) > 0 && (await focused.first().getAttribute("id")) === "ticket-reply") break;
    await page.keyboard.press("Tab");
  }
  await expect(page.locator("#ticket-reply")).toBeFocused();
  await page.keyboard.type("Ship it");
  await page.keyboard.press("Tab");
  await page.keyboard.press("Enter");

  await expect(page.getByRole("status")).toHaveText("Reply sent.");
  expect(submitted).toEqual({
    authorization: undefined,
    body: { id: expect.stringMatching(/^[A-Za-z0-9_-]{8,64}$/), replier: WHOAMI_ACTOR_ID, text: "Ship it" },
  });
  expect(await page.evaluate(() => sessionStorage.length + localStorage.length)).toBe(0);
});

test("reduced motion disables ticket-route transitions and animations", async ({ page }) => {
  await page.emulateMedia({ reducedMotion: "reduce" });
  await mockWhoami(page);
  await page.route("**/v1alpha1/tickets/**", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(TICKET_PROJECTION) }));
  await page.goto(`/tickets/${TICKET_ID}`);
  const state = page.locator(".ticket-state").first();
  await expect(state).toHaveCSS("animation-name", "none");
  await expect(state).toHaveCSS("transition-duration", "0s");
});
