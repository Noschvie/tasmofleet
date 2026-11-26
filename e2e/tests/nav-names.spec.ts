import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// #1106: one name per page. The top bar, the sub-navigation and the h1 already say where a page is, so a trail of
// one or two steps is not drawn, and a group with one page (the dashboard) has no sub-navigation row.

let app: App;
test.beforeAll(async () => {
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
    },
  });
});
test.afterAll(async () => app?.stop());

const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

for (const width of [1280, 375]) {
  // Why (A1-001, A1-004): on the dashboard the crumb "Dashboard" and the sub-navigation row "Overview" were two
  // more names for the same screen and 70 px of chrome above its title; on the Devices list the crumb "Devices /
  // List" repeated the lit tab and the h1 (on a phone "Devices" three times in the first 120 px).
  test(`no crumb on the dashboard and the Devices list, no sub-navigation row on the dashboard at ${width} px`, async ({ page }) => {
    await page.setExtraHTTPHeaders(admin);
    await page.setViewportSize({ width, height: 800 });
    await page.goto(app.url + "/");
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    await expect(page.locator("nav.crumbs"), "the dashboard draws no trail").toHaveCount(0);
    await expect(page.locator("header.top .sn-item"), "the dashboard's one page needs no tab row").toHaveCount(0);
    await page.goto(app.url + "/devices");
    await expect(page.getByRole("heading", { level: 1, name: "Devices" })).toBeVisible();
    await expect(page.locator("nav.crumbs"), "the Devices list draws no trail").toHaveCount(0);
  });

  // A guard, not a test that fails on main (main's dashboard still had its row): --header-h is what a bar that sticks
  // under the header and the scroll padding use. A page without the tab row must not keep the row's 40 px in it.
  test(`--header-h is the header's height on the dashboard and the Devices list at ${width} px`, async ({ page }) => {
    await page.setExtraHTTPHeaders(admin);
    await page.setViewportSize({ width, height: 800 });
    for (const path of ["/", "/devices"]) {
      await page.goto(app.url + path);
      await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
      const m = await page.evaluate(() => {
        const probe = document.createElement("div");
        probe.style.height = "var(--header-h)";
        document.body.append(probe);
        const want = probe.getBoundingClientRect().height;
        probe.remove();
        return { want, got: document.querySelector("header.top")!.getBoundingClientRect().height };
      });
      expect(Math.abs(m.got - m.want), `${path}: header ${m.got} px, --header-h ${m.want} px`).toBeLessThanOrEqual(0.5);
    }
  });
}
