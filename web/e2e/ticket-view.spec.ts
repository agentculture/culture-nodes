import { expect, test } from "@playwright/test";
import { TICKET_ID, TICKET_PROJECTION } from "../src/fixtures/ticket-fixture";

test("keyboard walk reaches the reply box and submits with the decision token", async ({ page }) => {
  let submitted: { authorization?: string; body?: unknown } | undefined;
  await page.route("**/v1alpha1/tickets/**", async (route) => {
    if (route.request().method() === "POST") {
      submitted = {
        authorization: route.request().headers().authorization,
        body: route.request().postDataJSON(),
      };
      await route.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: "reply-e2e", replier: "operator", text: "Ship it", created_at: "2026-08-29T10:00:00Z" }) });
      return;
    }
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(TICKET_PROJECTION) });
  });
  await page.goto(`/tickets/${TICKET_ID}`);
  await expect(page.getByRole("heading", { name: TICKET_ID })).toBeVisible();

  await page.keyboard.press("Tab");
  const focused = page.locator("*:focus");
  for (let count = 0; count < 20; count += 1) {
    if ((await focused.count()) > 0 && (await focused.first().getAttribute("id")) === "ticket-token") break;
    await page.keyboard.press("Tab");
  }
  await expect(page.locator("#ticket-token")).toBeFocused();
  await page.keyboard.type("walk-token");
  await page.keyboard.press("Tab");
  await page.keyboard.type("operator");
  await page.keyboard.press("Tab");
  await page.keyboard.press("Tab");
  await page.keyboard.type("Ship it");
  await page.keyboard.press("Tab");
  await page.keyboard.press("Enter");

  await expect(page.getByRole("status")).toHaveText("Reply sent.");
  expect(submitted).toEqual({
    authorization: "Bearer walk-token",
    body: { replier: "operator", text: "Ship it" },
  });
  expect(await page.evaluate(() => sessionStorage.getItem("nodes.human-decision-token"))).toBe("walk-token");
});

test("reduced motion disables ticket-route transitions and animations", async ({ page }) => {
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.route("**/v1alpha1/tickets/**", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(TICKET_PROJECTION) }));
  await page.goto(`/tickets/${TICKET_ID}`);
  const state = page.locator(".ticket-state").first();
  await expect(state).toHaveCSS("animation-name", "none");
  await expect(state).toHaveCSS("transition-duration", "0s");
});
