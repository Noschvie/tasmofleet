import { copyFileSync } from "node:fs";
import { join } from "node:path";
import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Why (#994): the answer to Back up now and Retry (a refusal inside the cooldown, a failed backup, the API
// off, a missing MQTT password) is swapped into #backup-status. At the end of the tab, after a long list,
// it landed below the fold and the click seemed to do nothing. It now sits under the bar and the swap
// scrolls it into view, clear of the fixed status bar at the window's bottom edge.
//
// Each case takes its own backup on its own device first: the cooldown lives in the app's memory per
// device, so no case depends on the order of the others or on how long an earlier one took.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  test.setTimeout(120_000);
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
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

/**
 * takeAndPad opens the device, takes one real backup (it starts the cooldown), then pads its list with
 * `copies` older copies of that backup (own files each, the pattern of seedOlderBackups in support/seed.ts)
 * and one failed attempt older than all of them, so a Retry sits at the very bottom of a long list.
 */
async function takeAndPad(page: Page, device: string, copies: number): Promise<string> {
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: device, exact: true }).click();
  const id = page.url().match(/devices\/(\d+)/)![1];
  const csrf = await page.locator("input[name=csrf]").first().inputValue();
  const took = await page.request.post(`${app.url}/devices/${id}/backup`, { headers: admin, form: { csrf }, maxRedirects: 0 });
  expect(took.status(), "backup taken").toBe(303);

  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
  try {
    db.exec("PRAGMA busy_timeout=5000");
    const newest = db
      .prepare(
        `SELECT device_id, kind, taken_at, fw_version, path, size, sha256, decoder_version, note FROM backups
         WHERE device_id = ? AND taken_at = (SELECT MAX(taken_at) FROM backups WHERE device_id = ?)`,
      )
      .all(Number(id), Number(id)) as { device_id: number; kind: string; taken_at: number; fw_version: string; path: string; size: number; sha256: string; decoder_version: string; note: string }[];
    expect(newest.length, "the real backup is in the database").toBeGreaterThan(0);
    const insert = db.prepare(
      `INSERT INTO backups (device_id, kind, taken_at, fw_version, path, size, sha256, decoder_version, note) VALUES (?,?,?,?,?,?,?,?,?)`,
    );
    const day = 24 * 3600 * 1000;
    for (let i = 1; i <= copies; i++) {
      for (const b of newest) {
        const path = b.path.replace(/(\.[a-z]+)$/, `-pad${i}$1`);
        copyFileSync(join(app.dataDir, b.path), join(app.dataDir, path));
        insert.run(b.device_id, b.kind, b.taken_at - i * day, b.fw_version, path, b.size, b.sha256, b.decoder_version, b.note);
      }
    }
    db.prepare("INSERT INTO backup_attempts (device_id, at, kind, ref, fw_version, reason) VALUES (?,?,?,?,?,?)").run(
      Number(id),
      newest[0].taken_at - (copies + 1) * day,
      "manual",
      "",
      newest[0].fw_version,
      "The device did not answer.",
    );
  } finally {
    db.close();
  }
  return id;
}

/** answerIsVisible: the answer lies inside the window and nothing, the fixed status bar included, covers it. */
async function expectAnswerVisible(page: Page) {
  const status = page.locator("#backup-status");
  await expect(status).toContainText("Too many requests. Try again in a few seconds.");
  await expect(status).toBeInViewport({ ratio: 1 });
  const geo = await page.evaluate(() => {
    const s = document.querySelector("#backup-status")!.getBoundingClientRect();
    const bar = document.querySelector("footer.sb")!.getBoundingClientRect();
    const hit = document.elementFromPoint(s.left + s.width / 2, s.top + s.height / 2);
    return { statusBottom: s.bottom, barTop: bar.top, covered: !hit || !document.querySelector("#backup-status")!.contains(hit) };
  });
  expect(geo.statusBottom, "the answer ends above the fixed status bar").toBeLessThanOrEqual(geo.barTop);
  expect(geo.covered, "nothing lies over the answer").toBe(false);
}

test("the answer to a refused Back up now shows up under the bar without a jump", async ({ page }) => {
  test.setTimeout(90_000);
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 1280, height: 720 });
  const id = await takeAndPad(page, Object.keys(env.devices)[0], 12);
  await page.goto(`${app.url}/devices/${id}/backups`);
  expect(await page.evaluate(() => document.documentElement.scrollHeight), "a list taller than the window").toBeGreaterThan(720 * 1.5);
  // the status sits right under the bar, before the list
  const order = await page.evaluate(() => {
    const bar = document.querySelector(".bk-bar")!;
    const status = document.querySelector("#backup-status")!;
    const list = document.querySelector(".bk-list")!;
    return { afterBar: !!(bar.compareDocumentPosition(status) & Node.DOCUMENT_POSITION_FOLLOWING), beforeList: !!(status.compareDocumentPosition(list) & Node.DOCUMENT_POSITION_FOLLOWING) };
  });
  expect(order).toEqual({ afterBar: true, beforeList: true });

  await page.locator(".bk-bar").getByRole("button", { name: "Back up now" }).click();
  await expectAnswerVisible(page);
  expect(await page.evaluate(() => window.scrollY), "a page at the top does not jump").toBe(0);
});

test("the answer to Retry at the bottom of a long list scrolls into view", async ({ page }) => {
  test.setTimeout(90_000);
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 375, height: 667 });
  const id = await takeAndPad(page, Object.keys(env.devices)[1], 12);
  await page.goto(`${app.url}/devices/${id}/backups`);
  const retry = page.locator(".bk-row.is-failed").getByRole("button", { name: "Retry" });
  await expect(retry).toHaveCount(1);
  // nothing is ticked, so the selection bar (fixed, above the status bar) is hidden and cannot cover the answer
  await expect(page.locator(".bk-selbar[data-bulkbar]")).toBeHidden();
  await retry.scrollIntoViewIfNeeded();
  const before = await page.evaluate(() => window.scrollY);
  expect(before, "the Retry button is far down the page").toBeGreaterThan(667);
  await expect(page.locator("#backup-status")).not.toBeInViewport();

  await retry.click();
  await expectAnswerVisible(page);
  expect(await page.evaluate(() => window.scrollY), "the page scrolled up to the answer").toBeLessThan(before);
});
