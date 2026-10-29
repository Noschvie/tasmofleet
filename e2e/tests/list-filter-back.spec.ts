import { mkdirSync } from "node:fs";
import { test, expect } from "../support/fixtures";
import {
  App,
  Devenv,
  startApp,
  startDevenv,
  writeTasmoAdminDir,
} from "../support/stack";

// #127: the "Devices" crumb on a device tab returns to the list with the filter and sort the
// user left (per browser tab); the navigation's "Devices" link still opens the unfiltered list.
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
const shots = process.env.UX_SHOTS;
let app: App;
let env: Devenv;

test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

test("crumb returns to the list as left; nav and new tab stay unfiltered (#127)", async ({
  browser,
}) => {
  const ctx = await browser.newContext({ extraHTTPHeaders: adam });
  const page = await ctx.newPage();
  await page.goto(app.url + "/devices?health=online&sort=name&dir=desc");
  await expect(page.locator("table.devtable tbody tr").first()).toBeVisible({
    timeout: 30_000,
  });
  const url = page.url();
  const firstDevice = page
    .locator("table.devtable tbody a[href*='/devices/']:not([href$='/new'])")
    .first();
  await firstDevice.click();
  await page.getByRole("link", { name: "Settings" }).first().click();
  await expect(page).toHaveURL(/\/settings/);
  const crumb = page.locator("nav.crumbs a", { hasText: "Devices" });
  await expect(crumb).toHaveAttribute(
    "href",
    /\?health=online&sort=name&dir=desc/,
  );
  await crumb.click();
  await expect(page).toHaveURL(url);

  // sort survives too
  await page.goto(app.url + "/devices?health=online&sort=address&dir=desc");
  await page
    .locator("table.devtable tbody a[href*='/devices/']:not([href$='/new'])")
    .first()
    .click();
  await expect(
    page.locator("nav.crumbs a", { hasText: "Devices" }),
  ).toHaveAttribute("href", /sort=address&dir=desc/);

  // the navigation's own link is not rewritten
  const nav = page.locator("nav a[href$='/devices']").first();
  await expect(nav).toHaveAttribute("href", /\/devices$/);

  // a new tab of the same session never saw the list: plain link
  const tab2 = await ctx.newPage();
  await tab2.goto(page.url());
  await expect(
    tab2.locator("nav.crumbs a", { hasText: "Devices" }),
  ).toHaveAttribute("href", /\/devices$/);
  await ctx.close();
});

test("375 px crumb with a kept filter", async ({ browser }) => {
  const ctx = await browser.newContext({
    extraHTTPHeaders: adam,
    viewport: { width: 375, height: 800 },
  });
  const page = await ctx.newPage();
  await page.goto(app.url + "/devices?health=online&sort=name&dir=desc");
  await expect(
    page.locator("li.dcard a[href*='/devices/']:not([href$='/new'])").first(),
  ).toBeVisible({ timeout: 30_000 });
  await page
    .locator("li.dcard a[href*='/devices/']:not([href$='/new'])")
    .first()
    .click();
  await expect(page.locator("nav.crumbs a[data-keep-query]")).toHaveAttribute(
    "href",
    /health=online/,
  );
  if (shots) {
    mkdirSync(shots, { recursive: true });
    await page.screenshot({ path: `${shots}/crumb-375.png` });
  }
  await ctx.close();
});
