import { mkdirSync } from "node:fs";
import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { expectNoOverflow } from "../support/layout";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// The device Overview (#145) and its live refresh (#126): tiles, relay box, Identity | Connection,
// Activity; the page follows the device (a button pressed on it, a pulled plug) without a reload.
// UX_SHOTS=<dir> keeps screenshots (375 and 1280, light and dark).
const shots = process.env.UX_SHOTS;
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
let app: App;
let env: Devenv;

test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv(["-overview-shapes"]);
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
  await page.screenshot({ path: `${shots}/${name}.png`, fullPage: true });
}

// A probe renames an entry that still carries its imported name to the device's own DeviceName (#125),
// so a device is looked up by name once per worker and by address afterwards.
const hrefs = new Map<string, string>();

/** open the overview of a device once it has reported its relay state */
async function openDevice(page: Page, name: string, width = 1280) {
  await page.setExtraHTTPHeaders(adam);
  await page.setViewportSize({ width, height: 900 });
  await page.goto(app.url + "/devices");
  await page.locator("table[data-table=devices] .pwr:not([disabled])").first().waitFor({ state: "attached", timeout: 30_000 });
  const href = hrefs.get(name) ?? (await page.getByRole("link", { name, exact: true }).first().getAttribute("href"))!;
  hrefs.set(name, href);
  await page.goto(app.url + href);
  await expect(page.locator("#devlive .dev-relay, #devlive .dev-sensor").first()).toBeVisible({ timeout: 30_000 });
  return href;
}

for (const width of [375, 1280]) {
  for (const scheme of ["light", "dark"] as const) {
    test(`overview renders without overflow at ${width}px ${scheme}`, async ({ page }) => {
      await page.emulateMedia({ colorScheme: scheme });
      await openDevice(page, "plug-4m", width);
      await expect(page.locator(".dev-tile")).not.toHaveCount(0);
      await expect(page.locator("#identity")).toBeVisible();
      await expect(page.locator("#devconn")).toBeVisible();
      await expect(page.locator("#devact")).toBeVisible();
      await expectNoOverflow(page, "overview");
      await shot(page, `overview-online-${width}-${scheme}`);
    });
  }
}

// #126: a button pressed on the device shows on the open page without a reload.
test("a relay switched on the device follows live, without a reload", async ({ page }) => {
  await openDevice(page, "plug-4m");
  await page.evaluate(() => ((window as unknown as { __kept: number }).__kept = 1));
  const sw = page.locator("#devlive .dev-relay [role=switch]").first();
  const was = await sw.getAttribute("aria-checked");
  await control("/device/plug-4m/relay/1/" + (was === "true" ? "off" : "on"));
  await expect(sw).toHaveAttribute("aria-checked", was === "true" ? "false" : "true", { timeout: 20_000 });
  expect(await page.evaluate(() => (window as unknown as { __kept: number }).__kept)).toBe(1); // same document: no reload
});

// #145: the switch of the relay box itself updates the box in place.
test("the relay box switch updates in place", async ({ page }) => {
  await openDevice(page, "lamp-1m");
  await page.evaluate(() => ((window as unknown as { __kept: number }).__kept = 1));
  const sw = page.locator("#devlive .dev-relay [role=switch]").first();
  const was = await sw.getAttribute("aria-checked");
  await sw.click();
  await expect(sw).toHaveAttribute("aria-checked", was === "true" ? "false" : "true", { timeout: 20_000 });
  expect(await page.evaluate(() => (window as unknown as { __kept: number }).__kept)).toBe(1);
});

// #145 mockup 8 and #126: a pulled plug turns the page offline by itself: banner, dimmed state,
// disabled switches and actions; Edit, Delete and Activity stay; the Activity list gets the new row.
test("a pulled plug shows the offline banner, dims and disables, and adds an Activity row live", async ({ page }) => {
  await openDevice(page, "esp32c3-sensor");
  await expect(page.locator("#devhd-off")).toHaveCount(0);
  const rows = await page.locator("#devact .dev-act li").count();
  await control("/device/esp32c3-sensor/off");
  await expect(page.locator("#devhd-off")).toContainText("Offline since", { timeout: 60_000 });
  await expect(page.locator("#devlive")).toHaveAttribute("data-off", "");
  await expect(page.locator("#identity")).toHaveClass(/is-dim/);
  await expect(page.locator("#devconn")).toHaveClass(/is-dim/);
  await expect(page.getByRole("button", { name: "Refresh identity" })).toBeDisabled();
  await expect(page.getByRole("button", { name: "Restart" })).toBeDisabled();
  await expect(page.getByRole("button", { name: "Back up" })).toBeDisabled();
  for (const sw of await page.locator("#devlive [role=switch]").all()) await expect(sw).toBeDisabled();
  await expect(page.getByRole("link", { name: "Edit" })).toBeEnabled();
  await expect(page.getByRole("link", { name: "Delete" })).toBeEnabled();
  await expect(page.locator("#devact")).not.toHaveClass(/is-dim/);
  await expect(page.locator("#devact .dev-act li")).toHaveCount(rows + 1, { timeout: 20_000 });
  await shot(page, "overview-offline-1280");
  await expectNoOverflow(page, "overview offline");
});

// #145: the credentials of a device never reach the page
test("no credentials in the overview", async ({ page }) => {
  const href = await openDevice(page, "locked-plug");
  const html = await page.content();
  expect(html).not.toContain("devenv-fake-password");
  expect(await page.locator("#identity").innerText()).not.toMatch(/password/i);
  expect(href).toBeTruthy();
});

// One device per mockup shape (devenv -overview-shapes). Each renders what its shape promises, and
// is looked at in 375/1280, light and dark (UX_SHOTS).
const shapes: { device: string; check: (page: Page) => Promise<void> }[] = [
  {
    device: "ov-metered-plug", // mockup 1
    check: async (page) => {
      await expect(page.locator("[data-tile=power]")).toContainText(/\d+ W/, { timeout: 30_000 });
      await expect(page.locator("[data-tile=energy]")).toContainText("kWh");
      await expect(page.locator("#devlive .dev-box-meta")).toHaveText("metered");
    },
  },
  {
    device: "ov-plain-plug", // mockup 2
    check: async (page) => {
      await expect(page.locator("[data-tile=power]")).toHaveCount(0);
      await expect(page.locator("[data-tile=energy]")).toHaveCount(0);
      await expect(page.locator("#devlive .dev-box-meta")).toHaveText("no energy meter");
    },
  },
  {
    device: "ov-double-switch", // mockup 3
    check: async (page) => {
      await expect(page.locator("#devlive .dev-relay")).toHaveCount(2);
      await expect(page.locator("#devlive .dev-box-meta")).toContainText("one meter for all", { timeout: 30_000 });
      await expect(page.locator("#devlive .dev-relay-s").first()).toHaveText(/^(On|Off)$/);
    },
  },
  {
    device: "ov-power-strip", // mockup 4
    check: async (page) => {
      await expect(page.locator("#devlive .dev-relay")).toHaveCount(4);
      await expect(page.locator("#devlive .dev-box-meta")).toContainText("of 4 on · metered per channel", { timeout: 30_000 });
      await expect(page.locator("#devlive .dev-relay-n").first()).toContainText("Monitor");
      await expect(page.locator("#devlive .dev-relay-s").first()).toHaveText(/\d+ W/);
      await expect(page.locator("[data-tile=power]")).toContainText("all channels");
    },
  },
  {
    device: "ov-ethernet-rack", // mockup 5
    check: async (page) => {
      // Ethernet is a fact the probe reads; a device that was only imported does not know it yet
      await page.getByRole("button", { name: "Refresh identity" }).click();
      await expect(page.getByText("Identity refreshed.")).toBeVisible();
      await page.reload();
      await expect(page.locator("[data-tile=link]")).toContainText("Ethernet");
      await expect(page.locator("[data-tile=wifi]")).toHaveCount(0);
      await expect(page.getByRole("heading", { name: "Connection · Ethernet" })).toBeVisible();
      await expect(page.locator("#devconn")).toContainText("Ethernet MAC");
    },
  },
  {
    device: "ov-climate", // mockup 6
    check: async (page) => {
      await expect(page.getByRole("heading", { name: "Sensors" })).toBeVisible();
      await expect(page.locator("#devlive .dev-sensor", { hasText: "Temperature · BME280" })).toContainText("12.4 °C", { timeout: 30_000 });
      await expect(page.locator("#devlive .dev-relay")).toHaveCount(0);
    },
  },
  {
    device: "ov-http-only", // mockup 7
    check: async (page) => {
      await expect(page.locator("[data-tile=reach]")).toContainText("MQTT not configured");
    },
  },
];

for (const shape of shapes) {
  for (const width of [375, 1280]) {
    for (const scheme of ["light", "dark"] as const) {
      test(`shape ${shape.device} at ${width}px ${scheme}`, async ({ page }) => {
        await page.emulateMedia({ colorScheme: scheme });
        await openDevice(page, shape.device, width);
        await shape.check(page);
        await expectNoOverflow(page, shape.device);
        await shot(page, `shape-${shape.device}-${width}-${scheme}`);
      });
    }
  }
}

// Ethernet facts come from a probe: Refresh identity reads subnet mask and DNS (WP0) and the card shows them.
test("an Ethernet device shows subnet mask and DNS after Refresh identity", async ({ page }) => {
  await openDevice(page, "ov-ethernet-rack");
  await page.getByRole("button", { name: "Refresh identity" }).click();
  await expect(page.getByText("Identity refreshed.")).toBeVisible();
  await page.reload();
  const conn = page.locator("#devconn");
  await expect(conn.locator("dt", { hasText: "Subnet mask" }).locator("xpath=following-sibling::dd")).not.toHaveText("Unknown");
  await expect(conn.locator("dt", { hasText: "DNS" }).locator("xpath=following-sibling::dd")).not.toHaveText("Unknown");
});

// Mockup 8 in both schemes and sizes: an offline device with relays dimmed. One test, because the plug
// can only be pulled once.
test("an offline device with relays at 375/1280, light and dark", async ({ page }) => {
  await openDevice(page, "ov-offline");
  await control("/device/ov-offline/off");
  await expect(page.locator("#devhd-off")).toContainText("Offline since", { timeout: 60_000 });
  for (const width of [375, 1280]) {
    for (const scheme of ["light", "dark"] as const) {
      await page.emulateMedia({ colorScheme: scheme });
      await page.setViewportSize({ width, height: 900 });
      await expectNoOverflow(page, `offline ${width} ${scheme}`);
      await shot(page, `shape-ov-offline-${width}-${scheme}`);
    }
  }
});
