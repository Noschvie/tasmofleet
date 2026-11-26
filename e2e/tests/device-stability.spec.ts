import { mkdirSync } from "node:fs";
import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { expectNoOverflow, overflowProblem } from "../support/layout";
import { seedWifiSamples } from "../support/seed";
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
  // The Wi-Fi sparkline needs two hourly samples; the app writes one per hour from the time it runs, so a
  // fresh instance has none. Twelve past hours that average -84 dBm, like the device's own reading, keep
  // the verdict and the card's numbers as they were (#679).
  await seedWifiSamples(app, "st-weakwifi", [-82, -86, -84, -86, -85, -83, -85, -84, -88, -81, -84, -80]);
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
  await expect(card).toContainText("Restarts");
  // #1112: the restart is the Uptime tile's and the Wi-Fi numbers the tile's and the Wi-Fi card's; the card repeats neither
  await expect(card).not.toContainText("Last restart");
  await expect(card).not.toContainText("Wi-Fi");
  await expectNoOverflow(page, "stability stable");
  await shot(page, "overview-healthy-1280");
});

test("a device with a weak link shows Weak Wi-Fi on the overview, the list and the dashboard", async ({ page }) => {
  await openOverview(page, "st-weakwifi");
  const card = page.locator("#stability");
  await expect(card.locator(".bdg")).toHaveText("Weak Wi-Fi");
  await expect(card.locator(".stab-note")).toContainText("-84 dBm");
  await expect(card.locator(".stab-note")).toContainText("14 times");
  // the drops and the signal of the last 24 hours are the Wi-Fi card's (#1112)
  await expect(page.locator("#devconn")).toContainText("14 times since the last restart");
  await expect(page.locator("#devconn")).toContainText("Signal, last 24 hours");
  await expect(card.locator(".stab-boots li")).toHaveCount(1); // the restart TasmoFleet noted when it first saw the device
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
    await cardShows(page, new RegExp(`${i === 1 ? "Once" : `${i} times`} in the last hour`));
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

// #1112: Recent restarts kept its date in a fixed 96 px column, so the date of an en-US browser ("Yesterday, 10:21 AM",
// "Nov 20, 2025, 9:59 AM") broke onto two lines while the same row in another locale fitted. The column takes the width of
// its widest date instead. The text is set here to the longest shape the page prints, so the test does not depend on the
// hour it runs; a line that wraps has two client rects, one that fits has one.
test("a restart's date stays on one line at 1280, whatever its wording", async ({ page }) => {
  await openOverview(page, "st-weakwifi", 1280);
  const times = page.locator("#stability .stab-boots .when time");
  await expect(times.first()).toBeVisible();
  const lines = await times.evaluateAll((els) =>
    els.map((el) => {
      el.textContent = "Yesterday, 10:21 AM";
      return el.getClientRects().length;
    }),
  );
  expect(lines, "line boxes of each date").not.toHaveLength(0);
  for (const n of lines) expect(n, "a date on two lines is a broken row").toBe(1);
});

// #679: the 24 h sparkline is a stretchable SVG (preserveAspectRatio none, strokes that do not scale), but a
// max-width of 240 px held it at that width while its column grew: at 540 px it filled 68 %, at 1280 px 56 %,
// and the rest of the row stayed empty. It must use the whole value column at every width, keeping its height.
// 375 and 412 are the phone widths of the report (the column is narrower than 240 px there, so they pass with
// the cap too); 540 is where the cap shows on a phone held sideways, 1280 on a desktop. One test per scheme
// walks all four widths, so the test as a whole fails whenever any width does.
for (const scheme of ["light", "dark"] as const) {
  test(`the Wi-Fi sparkline fills its column at every width (${scheme})`, async ({ page }) => {
    await page.emulateMedia({ colorScheme: scheme });
    const bad: string[] = [];
    for (const width of [375, 412, 540, 1280]) {
      await openOverview(page, "st-weakwifi", width);
      const spark = page.locator("#devconn svg.stab-spark"); // the Wi-Fi card since #1112
      // without the seeded samples there is no sparkline and nothing below would measure anything
      await expect(spark, `${width}px: the seeded samples draw a sparkline`).toHaveCount(1);
      await page.evaluate(() => document.fonts.ready);
      const m = await spark.evaluate((svg) => {
        const col = svg.closest("dd")!;
        const cs = getComputedStyle(col);
        const inner = col.getBoundingClientRect().width - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight);
        const r = svg.getBoundingClientRect();
        return { svg: r.width, col: inner, height: r.height };
      });
      if (Math.abs(m.svg - m.col) > 0.5) bad.push(`${width}px: the sparkline is ${m.svg}px in a column of ${m.col}px`);
      if (m.height !== 28) bad.push(`${width}px: the sparkline is ${m.height}px high, not 28px`);
      const over = await overflowProblem(page, `stability sparkline at ${width}px`);
      if (over) bad.push(over);
      await shot(page, `sparkline-weakwifi-${width}-${scheme}`);
    }
    expect(bad, "a sparkline short of its column").toEqual([]);
  });
}
