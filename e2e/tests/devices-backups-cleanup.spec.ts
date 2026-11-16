import { mkdirSync, writeFileSync, existsSync } from "node:fs";
import { join } from "node:path";
import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { openSchedule } from "../support/nav";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Why (#622): Devices -> Backups is the central overview of the fleet's backups, so cleaning up happens
// there in bulk: the backups of deleted devices, the unreferenced files, and Prune now for the current
// devices. Each asks once, naming count and size, and nothing a rule forbids is ever taken. This app has
// no backup schedule: a scheduled run would prune the very backups Prune now is meant to show.
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
      TASMOFLEET_OPERATOR_GROUPS: "ops",
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
const operator = { "X-authentik-username": "olga", "X-authentik-groups": "ops" };

// idOf opens a device from the list by name and returns its id and the page's CSRF token.
async function idOf(page: Page, name: string): Promise<{ id: string; csrf: string }> {
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name, exact: true }).click();
  const id = page.url().match(/devices\/(\d+)/)![1];
  return { id, csrf: await page.locator("input[name=csrf]").first().inputValue() };
}

async function backUp(page: Page, id: string, csrf: string) {
  const r = await page.request.post(`${app.url}/devices/${id}/backup`, { headers: admin, form: { csrf }, maxRedirects: 0 });
  expect(r.status(), "backup taken").toBe(303);
}

// The whole story runs in one test: the setup (two backups of each of two devices, one a minute after
// the other because of the cooldown between manual backups, one device deleted with its backups kept)
// is the slow part and every step builds on the one before it.
test("bulk cleanup: deleted devices' backups, unreferenced files, Prune now", async ({ page }) => {
  test.setTimeout(240_000);
  await page.setExtraHTTPHeaders(admin);
  const names = Object.keys(env.devices).sort();
  const live = await idOf(page, names[0]);
  const gone = await idOf(page, names[1]);

  await backUp(page, live.id, live.csrf);
  await backUp(page, gone.id, gone.csrf);
  await page.waitForTimeout(62_000); // the cooldown between two manual backups of one device
  await backUp(page, live.id, live.csrf);
  await backUp(page, gone.id, gone.csrf);

  // delete the second device and keep its two backups: they become the deleted devices' backups
  await page.goto(`${app.url}/devices/${gone.id}/delete`);
  await page.getByRole("checkbox", { name: /I understand/ }).check();
  await page.getByRole("button", { name: "Delete device" }).click();
  await expect(page).toHaveURL(/\/devices\?ok=deleted$/);

  // two unreferenced files in an old numbered folder
  const old = join(app.dataDir, "backups", "9001");
  mkdirSync(old, { recursive: true });
  writeFileSync(join(old, "20261027T104345Z-dmp.dmp"), "x".repeat(2048));
  writeFileSync(join(old, "20261027T104345Z-snapshot.json"), "y".repeat(1024));

  // a retention of one backup and no age rule: the Storage list names the older run of the live device and not the newest,
  // which Prune now never takes (it takes no backup, so the device would be left with none)
  await page.goto(app.url + "/devices/backups");
  await openSchedule(page);
  await page.locator("input[name='v.BACKUP_KEEP']").fill("1");
  await page.locator("input[name='v.BACKUP_MAX_AGE']").fill("0"); // no "never prune younger than" age: the fresh backups count
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page).toHaveURL(/\/devices\/backups\?ok=saved/);

  // --- backups of deleted devices: a kept run stays, the others are selected and deleted
  const orphans = page.locator("#card-orphans");
  await expect(orphans).toContainText("2 backups");
  await orphans.locator("summary", { hasText: "Backups" }).click();
  await orphans.locator(".bk-row").last().getByRole("button", { name: "Keep" }).click();
  await expect(page.getByText("Backup kept")).toBeVisible();
  await orphans.locator("summary", { hasText: "Backups" }).click();
  await expect(orphans.locator("input[type=checkbox][disabled]")).toHaveCount(1);
  await expect(orphans.locator(".bk-selbar[data-bulkbar]")).toBeHidden();
  await orphans.getByLabel(/Select all that can go \(1; 1 kept stay\)/).check();
  await expect(orphans.locator(".bk-selbar[data-bulkbar]")).toBeVisible();
  await expect(orphans.locator("[data-bar-count]")).toHaveText("1");
  // the device table has its own selection: nothing there is ticked
  await expect(page.getByRole("button", { name: /Back up selected/ })).toBeDisabled();
  await orphans.getByRole("button", { name: "Delete selected…" }).click();
  await expect(page.getByRole("heading", { level: 1, name: /Delete 1 backup of deleted devices/ })).toBeVisible();
  await page.getByRole("button", { name: "Delete 1 backup" }).click();
  await expect(page).toHaveURL(/\/devices\/backups\?.*ok=orphans-deleted/);
  await expect(page.getByText(/Deleted 1 backup of deleted devices/)).toBeVisible();
  await expect(page.locator("#card-orphans")).toContainText("1 backup");
  await expect(page.locator("#card-orphans .bk-row .bdg", { hasText: "Kept" })).toHaveCount(1);

  // --- unreferenced files: Delete all, one dialog naming count and size
  const unref = page.locator("#card-unreferenced");
  await expect(unref).toContainText("2 files");
  await unref.getByRole("link", { name: "Delete all…" }).click();
  const dlg = page.locator("dialog#dlg-unref");
  await expect(dlg).toBeVisible();
  await expect(dlg).toContainText("Delete 2 unreferenced files?");
  await expect(dlg).toContainText("3.0 KiB in all");
  await dlg.getByRole("button", { name: "Delete 2 files" }).click();
  await expect(page.getByText(/Deleted 2 unreferenced files/)).toBeVisible();
  await expect(page.locator("#card-unreferenced")).toHaveCount(0);
  expect(existsSync(join(old, "20261027T104345Z-dmp.dmp")), "the file is gone from the disk").toBe(false);

  // --- Prune now: the older run of the live device goes, the newest stays
  const storage = page.locator("#card-storage");
  await expect(storage).toContainText("Pruned at the next run (1)");
  await storage.getByRole("link", { name: "Prune now…" }).click();
  const prune = page.locator("dialog#dlg-fprune");
  await expect(prune).toBeVisible();
  await expect(prune).toContainText("Prune 1 backup now?");
  await expect(prune).toContainText("newest backup of a device");
  await prune.getByRole("button", { name: "Prune 1 backup" }).click();
  await expect(page.getByText(/Pruned 1 backup of 1 device/)).toBeVisible();
  await expect(page.locator(".bk-table tbody tr", { hasText: names[0] })).toContainText("1 backup");
  await page.goto(`${app.url}/devices/${live.id}/backups`);
  await expect(page.locator(".bk-row")).toHaveCount(1);
  await expect(page.locator(".bk-row .bdg", { hasText: "Newest" })).toHaveCount(1);
});

// Why (#622): admins only. An operator sees the counts and the lists, no box and no button that
// deletes, and the routes answer 403.
test("an operator sees no bulk control and the bulk routes refuse", async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/devices/backups");
  await expect(page.getByRole("heading", { name: "Backups", level: 1 })).toBeVisible();
  for (const label of ["Prune now…", "Delete all…", "Delete selected…"]) {
    await expect(page.getByText(label, { exact: true })).toHaveCount(0);
  }
  await expect(page.locator("#card-orphans input[type=checkbox]")).toHaveCount(0);
  for (const path of ["/devices/backups/prune", "/devices/backups/orphans/delete", "/devices/backups/unreferenced/delete"]) {
    expect((await page.request.get(app.url + path, { headers: operator })).status(), path).toBe(403);
  }
});
