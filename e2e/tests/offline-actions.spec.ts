import { mkdirSync } from "node:fs";
import type { Page } from "@playwright/test";
import { deviceBox, deviceEntry } from "../support/devicelist";
import { test, expect } from "../support/fixtures";
import { expectNoOverflow } from "../support/layout";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Actions that need a live device (#93): the bulk bar says how many selected devices are not
// online and will be skipped, and switches every action off when none is online; the row menu's
// Back up and Restart are off for a device that does not answer. UX_SHOTS=<dir> keeps screenshots.
const shots = process.env.UX_SHOTS;
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
let app: App;
let env: Devenv;
let online = "";
const dead = "garage-plug-offline";

test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_GITHUB_URL,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
    },
  });
  online = Object.keys(env.devices)[0];
  // a device nobody answers for: the list shows it as not online
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir({ ...env.devices, [dead]: { ...Object.values(env.devices)[0], addr: "127.0.0.1:1", topic: "garage_plug_offline" } })]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

async function open(page: Page, width: number) {
  await page.setViewportSize({ width, height: 900 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  await expect(deviceEntry(page, online).getByText("Online", { exact: true }).first()).toBeVisible({ timeout: 40_000 });
  await expect(deviceEntry(page, dead).getByText("Unknown", { exact: true }).first()).toBeVisible({ timeout: 60_000 });
}

async function shot(page: Page, name: string) {
  if (!shots) return;
  mkdirSync(shots, { recursive: true });
  await page.screenshot({ path: `${shots}/${name}.png`, fullPage: true });
}

for (const width of [375, 1280]) {
  for (const scheme of ["light", "dark"] as const) {
    test(`bulk bar at ${width}px ${scheme}: skips are announced, no device online switches the actions off`, async ({ page }) => {
      await page.emulateMedia({ colorScheme: scheme });
      await open(page, width);
      const bar = page.locator("[data-bulkbar]");
      const acts = bar.locator("[data-needs-device]");

      // only the dead device: nothing can run, so nothing can be clicked
      await deviceBox(page, dead).check();
      await expect(bar.locator("[data-bar-note]")).toHaveText(/it is not online$/);
      for (const b of await acts.all()) await expect(b).toBeDisabled();
      await expectNoOverflow(page, "bar, none online");
      await shot(page, `offline-bar-none-${width}-${scheme}`);

      // one online and the dead one: the actions work and the note says who is left out
      await deviceBox(page, online).check();
      await expect(bar.locator("[data-bar-note]")).toHaveText(/1 not online, will be skipped$/);
      for (const b of await acts.all()) await expect(b).toBeEnabled();
      await expectNoOverflow(page, "bar, one skipped");
      await shot(page, `offline-bar-some-${width}-${scheme}`);

      // only the online one: no note
      await deviceBox(page, dead).uncheck();
      await expect(bar.locator("[data-bar-note]")).toBeHidden();
    });
  }
}

test("row menu: Back up now is off for a device that does not answer", async ({ page }) => {
  await open(page, 1280);
  const menu = (name: string) => deviceEntry(page, name).locator("details.rowmenu").first();
  await menu(dead).locator("summary").click();
  await expect(menu(dead).getByRole("menuitem", { name: "Back up now" })).toHaveAttribute("aria-disabled", "true");
  await shot(page, "offline-rowmenu-1280");
  await menu(dead).locator("summary").click();
  await menu(online).locator("summary").click();
  await expect(menu(online).getByRole("menuitem", { name: "Back up now" })).not.toHaveAttribute("aria-disabled", "true");
});
