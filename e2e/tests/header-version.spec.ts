import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// #124: the running version sits in the header (and in the phone's drawer) and opens What's new.
// It is the same text the About page prints.
let app: App;
test.beforeAll(async () => {
  test.setTimeout(120_000);
  app = await startApp({
    env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32", TASMOFLEET_ADMIN_GROUPS: "adm" },
  });
});
test.afterAll(async () => {
  await app?.stop();
});

test("the header shows the About version and links to What's new", async ({ page }) => {
  await page.setExtraHTTPHeaders({ "X-authentik-username": "adam", "X-authentik-groups": "adm" });
  await page.goto(app.url + "/about");
  const about = (await page.locator(".about-name").innerText()).replace(/^TasmoFleet\s+/, "").trim();
  const link = page.locator("header.top .tb .ver");
  await expect(link).toHaveText(about);
  await link.click();
  await expect(page).toHaveURL(/\/whats-new$/);
});

test("on a phone the drawer shows the version and the page does not scroll sideways", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 800 });
  await page.setExtraHTTPHeaders({ "X-authentik-username": "adam", "X-authentik-groups": "adm" });
  await page.goto(app.url + "/");
  await page.getByRole("link", { name: "Open navigation" }).click();
  await expect(page.locator("#nav-drawer .ver")).toBeVisible();
  await page.keyboard.press("Escape");
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
});
