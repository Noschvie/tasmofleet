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
      for (const b of await acts.all()) {
        await expect(b).toBeDisabled();
        // the reason is the visible note, read out through aria-describedby; self-explanatory buttons take no tooltip
        await expect(b).toHaveAttribute("aria-describedby", "bulk-note");
        await expect(b).not.toHaveAttribute("title", /.+/);
      }
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
  await expect(menu(dead).getByRole("button", { name: "Back up now" })).toHaveAttribute("aria-disabled", "true");
  await shot(page, "offline-rowmenu-1280");
  await menu(dead).locator("summary").click();
  await menu(online).locator("summary").click();
  await expect(menu(online).getByRole("button", { name: "Back up now" })).not.toHaveAttribute("aria-disabled", "true");
});

// #1059: select.js no longer walks the whole document for every settled live cell. The one thing in a live
// cell that its sync reads is the health badge, so a health cell that turns offline must still switch the
// row menu's Back up now off and bring the bulk bar's note: a scope that stops at the select-all controls
// would leave both stale until the next list reload.
test("a health cell that turns offline switches its row menu and the bulk bar off (#1059)", async ({ page }) => {
  await open(page, 1280);
  await deviceBox(page, online).check();
  const bar = page.locator("[data-bulkbar]");
  await expect(bar.locator("[data-bar-note]")).toBeHidden();
  const menu = deviceEntry(page, online).locator("details.rowmenu").first();
  await menu.locator("summary").click();
  const item = menu.getByRole("button", { name: "Back up now" });
  await expect(item).not.toHaveAttribute("aria-disabled", "true");
  // the swap of one live cell, as the stream does it, with the badge the server renders for a down device
  await deviceEntry(page, online)
    .locator("[sse-swap^=health-]")
    .first()
    .evaluate((el) => {
      // @ts-expect-error htmx is a page global
      window.htmx.swap(el, '<span class="bdg dv-st" data-state="down">Down</span>', { swapStyle: "innerHTML" });
    });
  await expect(item).toHaveAttribute("aria-disabled", "true");
  await expect(bar.locator("[data-bar-note]")).toHaveText(/it is not online$/);
  for (const b of await bar.locator("[data-needs-device]").all()) await expect(b).toBeDisabled();
});

// #1082: a device that never answered is Unknown, not "Offline since <the time TasmoFleet started>": the badge, the one
// banner and Reachability say so, there is no stale Stable verdict, and Restart is blocked with the banner as its reason.
// The device is in that state when the page loads (its address answers nothing), so nothing here waits on a live change.
test("a device that never answered says Unknown once (#1082)", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 900 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: dead, exact: true }).first().click();
  await expect(page.locator(".devhd-state")).toHaveText("Unknown");
  await expect(page.locator("#devhd-notyet")).toContainText("has not answered yet");
  await expect(page.locator("#devhd-off")).toHaveCount(0);
  await expect(page.locator("[data-tile=reach]")).toContainText("Unknown");
  await expect(page.locator("#stability .bdg")).toHaveText("No data");
  await expect(page.locator("#dc-relays, #dc-sensors")).toHaveCount(0);
  const restart = page.getByRole("button", { name: "Restart", exact: true });
  await expect(restart).toHaveAttribute("aria-disabled", "true");
  await expect(restart).toHaveAttribute("aria-describedby", "devhd-notyet");
  await shot(page, "never-answered-375");
});

// #1072: Restart of an offline device was a bare <button disabled>: Tab skipped it and nothing said why. It is now
// blocked, not disabled: Tab reaches it, and the offline banner it names is on screen. Last in the file, because the
// plug of the device is pulled for good.
test("an offline device's Restart is reached by Tab and its reason is on screen at 375 px (#1072)", async ({ page }) => {
  // a device that answered before its plug is pulled: one that never answered (the locked plug asks for a password nobody
  // gave) is Unknown, not offline (#1082)
  const victim = Object.keys(env.devices).filter((n) => n !== online && n !== dead && n !== "locked-plug").at(-1)!;
  await page.setViewportSize({ width: 375, height: 900 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  await expect(deviceEntry(page, victim).getByText("Online", { exact: true }).first()).toBeVisible({ timeout: 40_000 });
  const r = await fetch(env.vars.DEVENV_CONTROL_URL + `/device/${victim}/off`, { method: "POST" });
  expect(r.status).toBe(204);
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: victim, exact: true }).first().click();
  const banner = page.locator("#devhd-off");
  await expect(banner).toContainText("Offline since", { timeout: 60_000 });
  const restart = page.getByRole("button", { name: "Restart", exact: true });
  await expect(restart).toHaveAttribute("aria-disabled", "true");
  await expect(restart).not.toHaveAttribute("disabled", /.*/);
  await expect(restart).toHaveAttribute("aria-describedby", "devhd-off");
  await expect(banner, "the reason is visible text, not a tooltip").toBeVisible();
  // #1082: one banner, a lead and one sentence; no second paragraph in the header and no list of the buttons
  await expect(banner).toContainText("Controls are off until it is back.");
  await expect(banner).not.toContainText("Restart");
  await expect(page.locator("#devhd-why")).toHaveCount(0);
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  for (let i = 0; i < 80; i++) {
    await page.keyboard.press("Tab");
    if (await restart.evaluate((el) => el === document.activeElement)) break;
  }
  await expect(restart, "Tab reaches a blocked Restart").toBeFocused();
  // the blocked look is half opacity, which would cut the 2px focus ring to about 2:1: focused, it draws at full strength
  expect(await restart.evaluate((el) => getComputedStyle(el).opacity), "the focus ring of a blocked button is not dimmed").toBe("1");
  await shot(page, "offline-restart-375");
});
