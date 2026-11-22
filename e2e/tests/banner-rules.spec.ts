import { test, expect } from "../support/fixtures";
import { expectBannerRules } from "../support/layout";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Why (#1091, DS-007, DS-008): the design system gives an inline banner one action and a reading width of 820 px
// (design-system.md, Inline banners). Pages wrote their banners by hand and broke both: the policy overview's
// deviation banner held Show and Make compliant, and one line of it ran over the whole 1204 px of the page. Every
// banner is ui.Banner now, so the rule is checked on the pages that show one, at 1280 px where a banner that
// ignores the reading width is as wide as the page.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_POLICY_RULES: JSON.stringify([{ command: "PowerOnState", want: "2", note: "e2e target" }]),
    },
  });
  expect(app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)])).toContain("created: lamp-1m");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});
test.use({ extraHTTPHeaders: { "X-authentik-username": "adam", "X-authentik-groups": "adm" } });

test("the policy overview's deviation banner has one action and keeps its reading width", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/policy");
  await page.getByRole("button", { name: "Run now" }).click();
  const alert = page.locator(".banner", { hasText: "deviate from a required rule" });
  await expect(alert).toBeVisible({ timeout: 60_000 }); // the live read puts it there; no timing assumed
  await expect(alert.locator("a.banner-btn, button")).toHaveCount(1);
  await expect(alert.getByRole("link", { name: "Show", exact: true })).toHaveAttribute("href", /\/policy\/deviations$/);
  await expectBannerRules(page, "/policy after Run now");
});

for (const path of ["/devices/new", "/audit", "/devices", "/policy/deviations"]) {
  test(`banners of ${path} keep the reading width and have at most one action`, async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.goto(app.url + path);
    await expect(page.locator("main")).toBeVisible();
    await expectBannerRules(page, path);
  });
}
