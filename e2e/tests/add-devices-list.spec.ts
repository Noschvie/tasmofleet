import { mkdirSync } from "node:fs";
import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { insertProposals, publishRetained } from "../support/discovery-seed";
import { expectNoOverflow } from "../support/layout";
import { App, Devenv, startApp, startDevenv } from "../support/stack";

// #130: the New devices list filters by state (the MQTT last will), sorts by its headers and
// no longer shows the empty Model and Firmware columns. UX_SHOTS=<dir> keeps screenshots.
const shots = process.env.UX_SHOTS;
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
let app: App;
let env: Devenv;

// Three proposals, all offline (their retained last will says so); the devenv devices that announce themselves are the online ones.
const devices = [
  { mac: "aa:bb:cc:00:01:01", name: "Workshop lamp", topic: "workshop_lamp", host: "workshop-lamp-1a2b", ip: "127.0.0.11", seen: 600 },
  { mac: "aa:bb:cc:00:01:02", name: "", topic: "attic_fan", host: "tasmota-attic-3c4d", ip: "127.0.0.9", seen: 300 },
  { mac: "aa:bb:cc:00:01:03", name: "Balcony plug", topic: "balcony_plug", host: "balcony-plug-5e6f", ip: "127.0.0.100", seen: 60 },
];

test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
    },
  });
  await insertProposals(app, devices);
  for (const t of ["workshop_lamp", "attic_fan", "balcony_plug"]) await publishRetained(env.vars.TASMOFLEET_MQTT_URL, `tele/${t}/LWT`, "Offline");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

async function open(page: Page, query = "", width = 1280) {
  await page.setViewportSize({ width, height: 900 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices/new" + query);
  await expect(page.locator(".add-chips")).toBeVisible();
}

/** topics are the MQTT topics in the name cells' tooltips of the desktop table, top to bottom (#410: the Hostname column is gone). */
// The stack's own devices announce themselves too; only the three seeded ones are compared.
const ours = new Set(devices.map((d) => d.topic));
const topics = async (page: Page) =>
  (await page.locator(".add-disc-table tbody tr td.name").evaluateAll((cells) => cells.map((c) => (c.getAttribute("title") ?? "").split(" · topic ")[1] ?? ""))).filter((t) => ours.has(t));

async function shot(page: Page, name: string) {
  if (!shots) return;
  mkdirSync(shots, { recursive: true });
  await page.screenshot({ path: `${shots}/${name}.png`, fullPage: true });
}

test("the state chips filter the list and the headers sort it", async ({ page }) => {
  await open(page);
  // the broker delivers the retained last wills a moment after the page may first load
  await expect(async () => {
    await page.reload();
    await expect(page.getByRole("link", { name: /^Offline\s*3$/ })).toBeVisible({ timeout: 1000 });
  }).toPass({ timeout: 30_000 });
  await expect(page.getByRole("columnheader", { name: "Model" })).toHaveCount(0);
  await expect(page.getByRole("columnheader", { name: "Firmware" })).toHaveCount(0);
  await expect(page.getByRole("columnheader", { name: "Device name" })).toBeVisible();
  for (const gone of ["Hostname", "MAC", "First seen"]) await expect(page.getByRole("columnheader", { name: gone })).toHaveCount(0);
  // default: last seen, newest first
  expect(await topics(page)).toEqual(["balcony_plug", "attic_fan", "workshop_lamp"]);

  await page.getByRole("link", { name: /^Offline\s*3$/ }).click();
  await expect(page).toHaveURL(/state=offline/);
  expect(await topics(page)).toEqual(["balcony_plug", "attic_fan", "workshop_lamp"]);
  await page.getByRole("link", { name: /^Online\s*\d+$/ }).click();
  await expect(page).toHaveURL(/state=online/);
  expect(await topics(page)).toEqual([]); // the offline ones are not in the online list
  await page.getByRole("link", { name: /^All\s*\d+$/ }).click();

  // a header sorts ascending, a second click flips it; the filter chip keeps the sort
  await page.getByRole("columnheader", { name: "IP" }).getByRole("link").click();
  expect(await topics(page)).toEqual(["attic_fan", "workshop_lamp", "balcony_plug"]); // .9 < .11 < .100
  await expect(page.getByRole("columnheader", { name: "IP" })).toHaveAttribute("aria-sort", "ascending");
  await page.getByRole("columnheader", { name: "IP" }).getByRole("link").click();
  expect(await topics(page)).toEqual(["balcony_plug", "workshop_lamp", "attic_fan"]);
  await page.getByRole("link", { name: /^Offline\s*3$/ }).click();
  await expect(page).toHaveURL(/sort=ip/);
  expect(await topics(page)).toEqual(["balcony_plug", "workshop_lamp", "attic_fan"]);
});

for (const width of [375, 1280]) {
  for (const scheme of ["light", "dark"] as const) {
    test(`New devices list at ${width}px ${scheme}`, async ({ page }) => {
      await page.emulateMedia({ colorScheme: scheme });
      await open(page, "?sort=name&dir=asc", width);
      await expectNoOverflow(page, "New devices list");
      await shot(page, `add-devices-${width}-${scheme}`);
      await page.goto(app.url + "/devices/new?state=online&ignored=1");
      await shot(page, `add-devices-filtered-${width}-${scheme}`);
    });
  }
}

// last: it adds (and so removes from the list) the devices the others look at
test("Add selected still adds the ticked rows after filtering and sorting", async ({ page }) => {
  await open(page, "?state=offline&sort=name&dir=asc");
  await expect(page.getByRole("link", { name: /^Offline\s*3$/ })).toBeVisible();
  await page.getByRole("checkbox", { name: "Select Workshop lamp" }).check();
  await page.getByRole("checkbox", { name: "Select Balcony plug" }).check();
  await page.getByRole("button", { name: /^Add 2 selected$/ }).click();
  // The page posts every ticked id twice (table row and phone card). Both added devices are
  // reported as added, not as failures (#836): a redirect with the notice, no error banner.
  await expect(page).toHaveURL(/ok=accepted/);
  await expect(page.locator("#toasts")).toContainText("2 devices added.");
  await expect(page.getByRole("alert")).toHaveCount(0);
  await page.goto(app.url + "/devices");
  await expect(page.getByText("Workshop lamp").first()).toBeVisible();
  await expect(page.getByText("Balcony plug").first()).toBeVisible();
});
