import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Overlays (menus and the tooltip) must not cover each other. The auto-tooltip used to copy the
// run-together text of an open row menu into a bubble on top of the menu, and the "Show or hide
// columns" bubble stayed open over the Columns menu it had just opened (visual QA findings 2 and 3).
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_URL,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

test("an open row menu is never covered by a bubble", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await page.setExtraHTTPHeaders(admin);
  // the device list's row menus (the firmware page, the old sample, lists releases as cards since design v2)
  await page.goto(app.url + "/devices");
  const summary = page.locator("details.rowmenu summary:visible").first(); // the phone's device cards
  await expect(summary).toBeVisible();
  await summary.click();
  const body = page.locator("details.rowmenu[open] .rowmenu-body:visible").first();
  await expect(body).toBeVisible();
  // pointer over the menu and the trigger, long enough for the bubble delay
  for (const target of [body, summary]) {
    await target.hover();
    await page.waitForTimeout(600);
    await expect(page.locator(".tooltip:not([hidden])")).toHaveCount(0);
  }
  expect(await page.locator("details.rowmenu [data-autotitle]").count(), "no run-together text copied from a menu").toBe(0);
});

test("the Columns menu opens without its bubble on top", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  const button = page.locator("details.colmenu > summary").first();
  await button.hover();
  // the Columns button has a visible label and no title tooltip any more (QA #3)
  await button.click();
  await expect(page.locator("details.colmenu[open] .colpanel").first()).toBeVisible();
  await page.waitForTimeout(600);
  await expect(page.locator(".tooltip:not([hidden])")).toHaveCount(0);
});
