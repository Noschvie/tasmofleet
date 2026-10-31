import { mkdirSync } from "node:fs";
import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { expectNoOverflow } from "../support/layout";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Stability per device (#325): restart reasons, boot loops and weak Wi-Fi on the device Overview, the
// device list and the dashboard. st-stable stays as it is, st-bootloop is restarted through the control
// server with a reason, st-weakwifi has a weak link from the start (-84 dBm, 14 reconnects).
// UX_SHOTS=<dir> keeps screenshots (375 and 1280, light and dark).
const shots = process.env.UX_SHOTS;
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
let app: App;
let env: Devenv;

test.beforeAll(async () => {
  test.setTimeout(180_000);
  env = await startDevenv(["-stability-shapes"]);
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_GITHUB_URL,
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

async function control(path: string) {
  const r = await fetch(env.vars.DEVENV_CONTROL_URL + path, { method: "POST" });
  expect(r.status, path).toBe(204);
}

async function shot(page: Page, name: string) {
  if (!shots) return;
  mkdirSync(shots, { recursive: true });
  await page.evaluate(() => {
    for (const el of document.querySelectorAll<HTMLElement>("body *")) {
      const pos = getComputedStyle(el).position;
      if (pos === "sticky" || pos === "fixed") if (!el.closest("dialog")) el.style.position = "static";
    }
    window.scrollTo(0, 0);
  });
  await page.screenshot({ path: `${shots}/${name}.png`, fullPage: true });
}

const hrefs = new Map<string, string>();

/** open the overview of a device once its Stability card shows a verdict */
async function openOverview(page: Page, name: string, width = 1280) {
  await page.setExtraHTTPHeaders(adam);
  await page.setViewportSize({ width, height: 900 });
  if (!hrefs.has(name)) {
    await page.goto(app.url + "/devices");
    await page.locator("table[data-table=devices] .pwr:not([disabled])").first().waitFor({ state: "attached", timeout: 30_000 });
    hrefs.set(name, (await page.getByRole("link", { name, exact: true }).first().getAttribute("href"))!);
  }
  await page.goto(app.url + hrefs.get(name)!);
  await expect(page.locator("#stability")).toBeVisible({ timeout: 30_000 });
}

/** reload the overview until the Stability card's text has what the test waits for (bounded) */
async function cardShows(page: Page, text: RegExp | string, timeoutMs = 90_000) {
  await expect
    .poll(
      async () => {
        await page.reload();
        return (await page.locator("#stability").innerText().catch(() => "")) as string;
      },
      { timeout: timeoutMs, intervals: [1500] },
    )
    .toMatch(text instanceof RegExp ? text : new RegExp(text));
}

test("a device that keeps its link and its power is Stable, with no notice", async ({ page }) => {
  await openOverview(page, "st-stable");
  const card = page.locator("#stability");
  await expect(card.locator(".bdg")).toHaveText("Stable");
  await expect(card.locator(".stab-note")).toHaveCount(0);
  await expect(card).toContainText("Last restart");
  await expect(card).toContainText("Wi-Fi reconnects");
  await expectNoOverflow(page, "stability stable");
  await shot(page, "overview-healthy-1280");
});

test("a device with a weak link shows Weak Wi-Fi on the overview, the list and the dashboard", async ({ page }) => {
  await openOverview(page, "st-weakwifi");
  const card = page.locator("#stability");
  await expect(card.locator(".bdg")).toHaveText("Weak Wi-Fi");
  await expect(card.locator(".stab-note")).toContainText("-84 dBm");
  await expect(card.locator(".stab-note")).toContainText("14 times");
  await expect(card).toContainText("14 since boot");
  await expect(card.locator(".stab-boots")).toHaveCount(0); // the restart list is for a boot loop
  await shot(page, "overview-weakwifi-1280");

  await page.goto(app.url + "/devices");
  const row = page.locator("tr", { has: page.getByRole("link", { name: "st-weakwifi", exact: true }) });
  await expect(row.locator(".dv-unst")).toHaveText("Weak Wi-Fi");
  await page.goto(app.url + "/");
  await expect(page.locator(".att-item", { hasText: "st-weakwifi" })).toContainText("Weak Wi-Fi: -84 dBm, 14 reconnects since boot");
});

test("three restarts in an hour make a boot loop, with the reason in plain words", async ({ page }) => {
  test.setTimeout(240_000);
  await openOverview(page, "st-bootloop");
  for (let i = 1; i <= 3; i++) {
    await control("/device/st-bootloop/restart?reason=Exception");
    // the next restart is sent only after this one was seen: a device that is still rebooting ignores it
    await cardShows(page, new RegExp(`${i} in 1 h`));
  }
  const card = page.locator("#stability");
  await expect(card.locator(".bdg")).toHaveText("Boot loop");
  await expect(card.locator(".stab-note")).toContainText("Restarted 3 times in the last hour");
  await expect(card.locator(".stab-note")).toContainText("by a crash");
  await expect(card).toContainText("Crash (exception)");
  await expect(card.locator(".stab-boots li").first()).toContainText("Exception");
  await shot(page, "overview-bootloop-1280");

  await page.goto(app.url + "/devices");
  const row = page.locator("tr", { has: page.getByRole("link", { name: "st-bootloop", exact: true }) });
  await expect(row.locator(".dv-unst")).toHaveText("Boot loop");
  await page.goto(app.url + "/");
  await expect(page.locator(".att-item", { hasText: "st-bootloop" })).toContainText("Boot loop: restarted 3 times in the last hour (crash)");
  // a boot loop is listed above weak Wi-Fi
  const names = await page.locator(".att-item .att-name").allInnerTexts();
  expect(names.indexOf("st-bootloop")).toBeLessThan(names.indexOf("st-weakwifi"));
  await shot(page, "dashboard-1280");
});

for (const width of [375, 1280]) {
  for (const scheme of ["light", "dark"] as const) {
    test(`stability pages render without overflow at ${width}px ${scheme}`, async ({ page }) => {
      await page.emulateMedia({ colorScheme: scheme });
      for (const name of ["st-stable", "st-weakwifi"]) {
        await openOverview(page, name, width);
        await expectNoOverflow(page, `stability ${name}`);
      }
      await page.goto(app.url + "/devices");
      await expectNoOverflow(page, "devices with badges");
      await page.goto(app.url + "/");
      await expectNoOverflow(page, "dashboard with stability reasons");
    });
  }
}
