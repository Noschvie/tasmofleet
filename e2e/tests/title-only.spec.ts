import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test, expect } from "../support/fixtures";
import { designFixturePath, loadDesignFixture } from "../support/seed";
import { App, Devenv, freePort, startApp, startDevenv } from "../support/stack";

// #1073: a title attribute may repeat visible text, never be the only place that says it, and colour never carries
// a state alone. A phone has no hover, a keyboard and most screen readers never show a title. This spec covers the
// elements the issue names, not every title on every page (Last seen, Chip, Address and the like are not part of
// it): the devices list badges and status reasons, a failed download on the Firmware page and the release page, and
// the Actual value of a deviating setting.
let app: App;
let env: Devenv;
test.describe.configure({ mode: "serial" });
test.beforeAll(async () => {
  test.setTimeout(180_000);
  env = await startDevenv(["-design-fixture", designFixturePath]); // the control server that makes a download fail
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_POLICY_LIVE_INTERVAL: "0",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_GITHUB_URL,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
      TASMOFLEET_POLICY_RULES: JSON.stringify([
        { name: "Power after outage", command: "PowerOnState", want: "2" },
        { name: "LED hint", command: "LedState", want: "3", severity: "info" },
      ]),
    },
  });
  // a few online devices (the ESP32-C3 among them: its file is the one the download fails for), and one that never answers
  const fx = loadDesignFixture();
  const pick = [...fx.devices.filter((d) => d.chip === "ESP32-C3" && d.status === "online").slice(0, 2), ...fx.devices.filter((d) => d.chip === "ESP8266" && d.status === "online").slice(0, 2)];
  const rows = pick.map((d, i) => {
    const [host, port] = env.devices[d.name].addr.split(":");
    return `${i + 1},"${d.name}",${host},,,bulb_1,1,1,0,0,1,${port},"${d.name}",0,${env.devices[d.name].topic},0`;
  });
  rows.push(`99,"silent-unknown",127.0.0.1,,,bulb_1,1,1,0,0,1,1,"silent-unknown",0,silent_unknown,0`);
  const dir = mkdtempSync(join(tmpdir(), "tasmofleet-ta-"));
  writeFileSync(join(dir, "devices.csv"), rows.join("\n") + "\n");
  expect(app.cli(["import", "tasmoadmin", dir])).toContain("created:");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

async function feed(state: "dl-down" | "up") {
  const r = await fetch(`${env.vars.DEVENV_CONTROL_URL}/fixture/feed/${state}`, { method: "POST" });
  expect(r.status).toBe(204);
}

async function runLiveCheck(page: import("@playwright/test").Page) {
  await page.goto(app.url + "/policy/deviations");
  await page.getByRole("button", { name: "Run now" }).first().click();
  await expect(page.locator("tbody[data-dev-group]").first()).toBeVisible({ timeout: 60_000 });
}

// Why: "update", "1 deviation", the Status badge's hint and "MQTT down" explained themselves in a title only. The list
// now says which update ("Update 14.3.0") and what differs ("1 off policy") in words, and every state that is not
// plain Online shows its reason as text. innerText is what a person without a hover reads: attributes are not in it.
test("Devices: the update, the deviations and every status reason are visible text", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 1280, height: 900 });
  await runLiveCheck(page);
  await page.goto(app.url + "/devices");
  const upd = page.locator("table[data-table=devices] .dv-upd");
  await expect(upd.first()).toBeVisible({ timeout: 30_000 });
  for (const t of await upd.allInnerTexts()) expect(t).toMatch(/^Update \d+\.\d+\.\d+$/);
  const devs = page.locator("table[data-table=devices] .dv-devs");
  await expect(devs.first()).toBeVisible();
  for (const t of await devs.allInnerTexts()) expect(t).toMatch(/^\d+ off policy$/);
  // the device that never answers: its Status cell says why in words (Unknown or Offline, whichever the poll made it)
  const cell = page.locator("table[data-table=devices] tr", { hasText: "silent-unknown" }).locator("td[data-label=Status]");
  await expect(async () => {
    await page.reload();
    await expect(cell.locator(".dv-st:not([data-state=online])")).toBeVisible({ timeout: 2_000 });
  }).toPass({ timeout: 60_000 });
  await expect(cell.locator(".dv-why")).toBeVisible();
  expect((await cell.locator(".dv-why").innerText()).length).toBeGreaterThan(3);
  // no non-Online badge of the whole list is left without its reason
  for (const st of await page.locator("table[data-table=devices] .dv-st:not([data-state=online])").all()) {
    await expect(st.locator("xpath=following-sibling::*[contains(@class,'dv-why')]")).toBeVisible();
  }
});

// Why: the phone's device card says the same words as the table row; its badges are the only place it shows the update
// and the deviations at all.
test("Devices on a phone: the card says which update and what differs", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto(app.url + "/devices");
  const card = page.locator("li.dcard .bdg.t-accent").first();
  await expect(card).toBeVisible({ timeout: 30_000 });
  await expect(card).toHaveText(/^Update \d+\.\d+\.\d+$/);
  await expect(page.locator("li.dcard .bdg.t-warn", { hasText: /off policy/ }).first()).toBeVisible();
});

// Why: a download that fails said why only in a title, so on a phone or by keyboard "Download failed" told nothing about
// what to do. The reason is a line in the file's row, on the Firmware page and on the release page, with Retry.
test("Firmware and the release page: a failed download says why in the file's row", async ({ page }) => {
  test.setTimeout(120_000);
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 1280, height: 900 });
  await feed("dl-down");
  try {
    await page.goto(app.url + "/firmware");
    const row = page.locator("#release .fw-file", { has: page.getByRole("button", { name: /^Prefetch / }) }).first();
    await expect(row).toBeVisible({ timeout: 60_000 });
    await row.getByRole("button", { name: /^Prefetch / }).click();
    const failed = page.locator("#release .fw-file", { hasText: "Download failed" }).first();
    await expect(failed).toBeVisible({ timeout: 30_000 });
    await expect(failed.locator(".fw-file-why")).toBeVisible();
    expect((await failed.locator(".fw-file-why").innerText()).trim().length).toBeGreaterThan(5);
    await expect(failed.getByRole("button", { name: /^Retry / })).toBeVisible();

    await page.goto(app.url + "/firmware");
    await page.locator("#release").getByRole("link", { name: "Release page", exact: true }).click();
    const rrow = page.locator(".rel-files .rel-file", { has: page.getByRole("button", { name: /^Prefetch / }) }).first();
    await expect(rrow).toBeVisible();
    await rrow.getByRole("button", { name: /^Prefetch / }).click();
    const rfailed = page.locator(".rel-files .rel-file", { hasText: "Download failed" }).first();
    await expect(rfailed).toBeVisible({ timeout: 30_000 });
    await expect(rfailed.locator(".fw-file-why")).toBeVisible();
    expect((await rfailed.locator(".fw-file-why").innerText()).trim().length).toBeGreaterThan(5);
  } finally {
    await feed("up");
  }
});

// Why: the differing Actual value was told from the Expected one by an amber tint alone. It carries a not-equal sign for
// everyone and the word "Differs" for a screen reader, and its title repeats what the cell shows.
test("Policy deviations: the Actual value is marked beyond its colour", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 1280, height: 900 });
  await runLiveCheck(page);
  const cells = page.locator("td[data-label=Actual]");
  expect(await cells.count()).toBeGreaterThan(0);
  for (const c of await cells.all()) {
    await expect(c.locator(".pol-dev-mark")).toBeVisible();
    await expect(c.locator(".pol-dev-mark")).toHaveText("≠");
    await expect(c.locator(".sr-only")).toHaveText("Differs:");
    const title = (await c.getAttribute("title")) ?? "";
    expect(await c.innerText(), "the title only repeats the visible value").toContain(title);
  }
});

// Why: the bubble of a badge in a table cell is centred on the badge's line, so it stays on the badge's own row. The
// bubble is a fixed box and hide() left it where the last bubble stood: the next one was measured as wide as the room
// right of that old spot, wrapped to more lines than it ends up with, and was centred too high (the guard of
// tablet-guards, #1073: the bubble of "Online" lay over the row above). It is measured from the left edge now.
// The test hovers the Firmware cell's "Update" badge, not the Status badge: the Status cell is replaced by the event
// stream on every health message (every poll), so a hovered Status badge can be swapped out under the pointer and a
// bubble left over from the swapped-out badge would be measured. The fleet is settled (all Online) first, and the
// bubble is found by its own text, so a stale one is never the one measured.
test("Devices: a bubble is centred on its badge whatever the last bubble left behind", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/devices");
  await expect(page.locator("table[data-table=devices] .dv-st[data-state=online]")).toHaveCount(4, { timeout: 30_000 });
  const badge = page.locator("table[data-table=devices] td[data-label=Firmware] .dv-upd").first();
  const tip = page.locator(".tooltip:not([hidden])", { hasText: "A newer stable release is available" });
  await badge.hover();
  await expect(tip).toBeVisible();
  await page.mouse.move(0, 0);
  await expect(tip).toHaveCount(0);
  await page.evaluate(() => {
    document.getElementById("tf-tooltip")!.style.left = document.documentElement.clientWidth - 40 + "px"; // the last bubble stood at the right edge
  });
  await badge.hover();
  await expect(tip).toBeVisible();
  const [b, t] = await Promise.all([badge.boundingBox(), tip.boundingBox()]);
  expect(Math.abs(t!.y + t!.height / 2 - (b!.y + b!.height / 2)), "the bubble's centre is the badge's centre").toBeLessThan(4);
});
