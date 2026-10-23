import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// What's new (design v2, N5): the avatar's dot says the changelog has releases the user has not
// seen; opening the page shows what was new and clears the dot.

let app: App;
test.beforeAll(async () => {
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
    },
  });
});
test.afterAll(async () => app?.stop());

// Why: a user who never opened What's new gets one hint; the page still marks the release that
// was new on this visit, and afterwards the hint is gone everywhere until the next release.
test("the avatar dot shows on the first visit and goes once What's new was opened", async ({ page }) => {
  await page.setExtraHTTPHeaders({ "X-authentik-username": "olivia", "X-authentik-groups": "ops" });
  await page.goto(app.url + "/devices");
  await expect(page.locator(".tb .avatar-dot")).toBeVisible();
  await page.locator(".tb summary.um-btn").click();
  const item = page.locator(".um-menu .um-news");
  await expect(item).toContainText("1 new since last visit");
  await item.click();
  await expect(page).toHaveURL(/\/whats-new$/);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("What’s new in TasmoFleet");
  await expect(page.locator(".wn-rel").first()).toContainText("New since your last visit");
  await expect(page.locator(".tb .avatar-dot")).toHaveCount(0);

  await page.goto(app.url + "/devices");
  await expect(page.locator(".tb .avatar-dot")).toHaveCount(0);
  await page.goto(app.url + "/whats-new");
  await expect(page.getByText("New since your last visit")).toHaveCount(0);
});

// Why: About's "What's new in x.y.z" and its Changelog link open the changelog of this binary in
// the app instead of the repository's newest one on GitHub.
test("About leads to What's new", async ({ page }) => {
  await page.setExtraHTTPHeaders({ "X-authentik-username": "adam", "X-authentik-groups": "adm" });
  await page.goto(app.url + "/about");
  await page.getByRole("link", { name: /^What’s new in / }).click();
  await expect(page).toHaveURL(/\/whats-new$/);
  await expect(page.locator(".crumbs")).toContainText("About");
});
