import { appendFileSync } from "node:fs";
import { pickForUpdate, startUpdate } from "../support/updates";
import { join } from "node:path";
import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Firmware page against the devenv fleet and its fake upstream: prefetch the
// files the fleet needs, delete cached files, and see a file held by a running
// OTA job refuse deletion.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
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
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_URL,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
    },
  });
  const dir = writeTasmoAdminDir(env.devices);
  // a device that never answers: its version stays unknown (finding 30)
  appendFileSync(join(dir, "devices.csv"), `99,"silent-unknown",127.0.0.1,,,bulb_1,1,1,0,0,1,1,"silent-unknown",0,silent_unknown,0\n`);
  const out = app.cli(["import", "tasmoadmin", dir]);
  expect(out).toContain("created: lamp-1m");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const operator = { "X-authentik-username": "olivia", "X-authentik-groups": "ops" };
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

async function openFirmware(page: import("@playwright/test").Page) {
  await page.goto(app.url + "/firmware");
  // the first index refresh runs asynchronously after start
  await expect(async () => {
    await page.reload();
    await expect(page.locator("#release-16\\.1\\.0")).toBeVisible();
  }).toPass({ timeout: 30_000 });
}

test("prefetch for my fleet, then delete a cached file and all unused ones", async ({ page }) => {
  test.setTimeout(120_000);
  await page.setExtraHTTPHeaders(admin);
  await openFirmware(page);
  // the newest release opens selected and is marked Latest
  await expect(page.locator("#release-16\\.1\\.0")).toHaveAttribute("aria-current", "true");
  await expect(page.locator("#release-16\\.1\\.0")).toContainText("Latest");
  // devices without a version are named, not silently left out (finding 30)
  await expect(page.locator("#release .fw-note", { hasText: "No file needed for" })).toContainText("silent-unknown");
  await expect(page.getByText("Nothing cached yet.")).toBeVisible();

  await page.getByRole("button", { name: "Prefetch for my fleet" }).click();
  await expect(page.getByRole("heading", { name: "Prefetch 16.1.0" })).toBeVisible();
  await expect(page.getByText(/State: finished\b/)).toBeVisible({ timeout: 60_000 });

  await page.goto(app.url + "/firmware");
  // every file the fleet needs is cached now: nothing left to prefetch
  await expect(page.locator("#release .fw-file").first()).toBeVisible();
  await expect(page.locator("#release").getByRole("button", { name: /^Prefetch/ })).toHaveCount(0);
  await expect(page.locator("#cache-total")).toContainText("files ·");
  const minimal = page.locator(".fw-crow").filter({ hasText: "tasmota-minimal.bin.gz" });
  await expect(minimal).toContainText("16.1.0");

  // delete one file in its dialog
  await minimal.getByRole("link", { name: /^Delete/ }).click();
  const dlg = page.locator("dialog[open]");
  await expect(dlg.getByRole("heading", { name: "Delete tasmota-minimal.bin.gz (16.1.0) from the cache?" })).toBeVisible();
  await dlg.getByRole("button", { name: "Delete file" }).click();
  await expect(page.locator("#toasts")).toContainText("Deleted 16.1.0/tasmota-minimal.bin.gz");
  await expect(page.locator("#release").getByRole("button", { name: "Prefetch tasmota-minimal.bin.gz" })).toBeVisible();

  // delete everything that is left
  await page.getByRole("button", { name: /Delete \d+ unused/ }).click();
  await page.getByRole("button", { name: /Delete \d+ files/ }).click();
  await expect(page.locator("#toasts")).toContainText(/Deleted \d+ files/);
  await expect(page.getByText("Nothing cached yet.")).toBeVisible();

  // both deletions are in the audit log
  await page.goto(app.url + "/audit");
  await expect(page.locator("table.audittable").getByText("Delete firmware file").first()).toBeVisible();
  await expect(page.locator("table.audittable").getByText("Download firmware").first()).toBeVisible();
});

// Why (N6): the release page is where an admin reads a release before updating. It is reached
// from the firmware card, reads only the cached feed, lists "Changes since" the fleet's oldest
// version (the devenv fleet runs 16.0.0: only 16.1.0 is new to it, 16.0.0 itself is left out),
// and its Prefetch starts the same tracked prefetch run as the firmware page.
test("release page from the firmware card: notes, changes since the oldest version, prefetch", async ({ page }) => {
  test.setTimeout(120_000);
  await page.setExtraHTTPHeaders(admin);
  await openFirmware(page);
  await page.locator("#release").getByRole("link", { name: "Release page", exact: true }).click();
  await expect(page).toHaveURL(/\/firmware\/releases\/16\.1\.0$/);
  await expect(page.getByRole("heading", { level: 1 })).toContainText("Tasmota 16.1.0");
  await expect(page.locator(".rel-files")).toContainText("tasmota.bin.gz");
  await expect(page.locator(".rel-older")).toContainText("16.0.0");

  await page.getByRole("link", { name: "Changes since 16.0.0" }).click();
  await expect(page).toHaveURL(/\?view=since$/);
  await expect(page.locator(".rel-sv")).toHaveText(["16.1.0"]);
  await page.getByRole("link", { name: "Release notes", exact: true }).click();

  await page.getByRole("button", { name: "Prefetch for my fleet" }).click();
  await expect(page.getByRole("heading", { name: "Prefetch 16.1.0" })).toBeVisible();
  await expect(page.getByText(/State: finished\b/)).toBeVisible({ timeout: 60_000 });
  await page.goto(app.url + "/firmware/releases/16.1.0");
  await expect(page.locator(".rel-files").getByRole("button", { name: /^Prefetch/ })).toHaveCount(0);
  await expect(page.locator(".rel-files .bdg", { hasText: "Cached" }).first()).toBeVisible();
});

test("operators see the page without management actions", async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await openFirmware(page);
  await expect(page.getByRole("button", { name: "Prefetch for my fleet" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Check for new releases now" })).toHaveCount(0);
  await expect(page.getByRole("link", { name: /^Delete/ })).toHaveCount(0);
  // the whole notes are not open by default: Release notes opens them and Escape closes them
  await expect(page.locator("dialog[open]")).toHaveCount(0);
  await page.getByRole("link", { name: "Release notes" }).click();
  await expect(page.locator("dialog[open]")).toContainText("Release notes");
  await page.keyboard.press("Escape");
  await expect(page.locator("dialog[open]")).toHaveCount(0);
});

test("a file held by a running update cannot be deleted", async ({ page }) => {
  test.setTimeout(240_000);
  await page.setExtraHTTPHeaders(operator);
  await pickForUpdate(page, app.url, ["lamp-1m"]);
  await page.locator("label.upd-relopt", { hasText: "Tasmota 16.1.0" }).click();
  await startUpdate(page);

  // While the job runs, the cached files say which transaction needs a file and offer no
  // Delete for it.
  await page.setExtraHTTPHeaders(admin);
  await expect(async () => {
    await page.goto(app.url + "/firmware");
    await expect(page.locator(".fw-crow a.fw-needed[href*='/transactions/']").first()).toBeVisible();
  }).toPass({ timeout: 60_000 });
  const held = page.locator(".fw-crow").filter({ has: page.locator("a.fw-needed") }).first();
  await expect(held.getByRole("link", { name: /^Delete/ })).toHaveCount(0);
  const heldFile = (await held.locator(".fw-c-f").innerText()).trim();

  // the confirmation page of that file refuses as well (find it by its id)
  let refused = "";
  for (let id = 1; id <= 12 && !refused; id++) {
    await page.goto(app.url + `/firmware/artifacts/${id}/delete`);
    const text = await page.locator("main").innerText();
    if (text.includes(heldFile) && text.includes("needed by transaction")) refused = text;
  }
  expect(refused).toContain("cannot be deleted");
  await expect(page.getByRole("button", { name: "Delete file" })).toHaveCount(0);

  // the batch still finishes: nothing was pulled away from the device
  await expect(async () => {
    await page.goto(app.url + "/transactions");
    await expect(page.locator(".upd-hrow").first()).toContainText("Completed", { timeout: 2_000 });
  }).toPass({ timeout: 200_000 });
});

// Why: the page is two columns on a desktop and one on a phone; the release cards, the selected
// release and the cached files never reach past the screen, at either width.
for (const [name, width] of [["desktop", 1280], ["phone", 375]] as const) {
  test(`${name}: the firmware page fits the screen`, async ({ page }) => {
    await page.setExtraHTTPHeaders(admin);
    await page.setViewportSize({ width, height: 900 });
    await openFirmware(page);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1), "no sideways scroll").toBe(true);
    const rel = (await page.locator(".fw-rels").boundingBox())!;
    const main = (await page.locator(".fw-main").boundingBox())!;
    if (width >= 720) expect(main.x, "the selected release sits right of the list").toBeGreaterThan(rel.x + rel.width - 1);
    else expect(main.y, "on a phone the selected release follows the list").toBeGreaterThan(rel.y + rel.height - 1);
  });
}

// QA v0.9.0: the page after a check is a POST answer; it must be the same page (the selected
// release, the dialogs), and on a phone a device name in the notes dialog never breaks inside itself.
test("after a check the page is whole; phone dialog names stay whole", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await openFirmware(page);
  await page.getByRole("button", { name: "Check for new releases now" }).click();
  await expect(page.locator("#toasts")).toContainText(/No release newer than 16\.1\.0|New release/);
  await expect(page.locator("#release")).toContainText("Tasmota 16.1.0");

  await page.setViewportSize({ width: 375, height: 800 });
  await page.getByRole("link", { name: "Release notes" }).click();
  const names = page.locator("dialog[open] .dev a");
  await expect(names.first()).toBeVisible();
  const broken = await names.evaluateAll((els) =>
    els.filter((el) => el.getBoundingClientRect().height > 1.6 * parseFloat(getComputedStyle(el).lineHeight || "24")).map((el) => el.textContent),
  );
  expect(broken, "device names that wrap onto a second line").toEqual([]);
});
