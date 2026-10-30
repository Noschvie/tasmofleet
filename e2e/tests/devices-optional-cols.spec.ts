import { mkdirSync } from "node:fs";
import { join } from "node:path";
import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { expectNoOverflow } from "../support/layout";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Why (#239): OTA URL and Group topic are optional device-list columns (hidden until chosen in the
// Columns menu). An OTA URL is the longest value a cell holds, so it must be cut off by its cell
// with the whole value on hover, never widen the table or the page. They sort like the others.
// UX_SHOTS=<dir> keeps screenshots (375 and 1280, light and dark).
const shots = process.env.UX_SHOTS;
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

let app: App;
let env: Devenv;

test.beforeAll(async () => {
  test.setTimeout(240_000);
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_URL,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
    },
  });
  const pick = ["lamp-1m", "plug-4m"];
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(Object.fromEntries(pick.map((n) => [n, env.devices[n]])))]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const table = (page: Page) => page.locator('table[data-table="devices"]');

test("OTA URL and Group topic are optional columns, cut off by the cell and sortable", async ({ page }) => {
  await page.setExtraHTTPHeaders(adam);
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto(app.url + "/devices");
  await expect(page.getByRole("row").filter({ hasText: "lamp-1m" }).getByText("Online", { exact: true }).first()).toBeVisible({ timeout: 40_000 });
  // hidden by default
  await expect(table(page).locator("thead th", { hasText: "OTA URL" })).toBeHidden();
  await expect(table(page).locator("thead th", { hasText: "Group topic" })).toBeHidden();

  await page.locator("details.colmenu > summary").first().click();
  await page.locator("details.colmenu").first().getByRole("checkbox", { name: "OTA URL" }).check();
  await page.locator("details.colmenu").first().getByRole("checkbox", { name: "Group topic" }).check();
  await expect(table(page).locator("thead th", { hasText: "OTA URL" })).toBeVisible();
  await expect(table(page).locator("thead th", { hasText: "Group topic" })).toBeVisible();

  // a read value is cut off by its cell, the whole value is the title, nothing widens the page
  const ota = table(page).locator('td[data-label="OTA URL"] .dv-trunc[title]').first();
  await expect(ota).toBeVisible({ timeout: 40_000 });
  const full = (await ota.getAttribute("title"))!;
  expect(full).toMatch(/^https?:\/\//);
  await expect(table(page).locator('td[data-label="Group topic"] .dv-trunc[title]').first()).toBeVisible();
  const cut = await ota.evaluate((e) => e.scrollWidth > e.clientWidth);
  expect(cut, `${full} should be longer than its cell`).toBe(true);
  await expectNoOverflow(page, "devices with OTA URL and Group topic");

  // sorting by the new headers works (close the Columns menu first, its hint lies over the header)
  await page.locator("details.colmenu > summary").first().click();
  await table(page).locator("thead th a", { hasText: "OTA URL" }).click();
  await expect(page).toHaveURL(/sort=otaurl/);
  await expect(table(page).locator("tbody tr").first()).toBeVisible();

  if (shots) {
    mkdirSync(shots, { recursive: true });
    for (const scheme of ["light", "dark"] as const) {
      await page.emulateMedia({ colorScheme: scheme });
      for (const w of [1280, 375]) {
        await page.setViewportSize({ width: w, height: 800 });
        await page.goto(app.url + "/devices?sort=otaurl&dir=asc");
        await expect(page.locator("table[data-table=devices], li.dcard").first()).toBeAttached();
        await expectNoOverflow(page, `devices at ${w}`);
        // the table scrolls sideways inside its frame: show the end, where the new columns are
        await page.locator(".tablescroll").first().evaluate((e) => (e.scrollLeft = e.scrollWidth));
        await page.screenshot({ path: join(shots, `devices-cols-${w}-${scheme}.png`), fullPage: true });
      }
    }
  }
});
