import { readdirSync, rmSync } from "node:fs";
import { join } from "node:path";
import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Why (#205): a user who deletes a backup directory by hand is left with rows that point
// nowhere. The lists must say so, offer nothing that can only fail, and let an admin remove
// the entry. Both places: a device's Backups tab and Settings -> Backups (deleted devices).
let app: App;
let env: Devenv;
test.beforeAll(async () => {
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

test("a backup whose file was deleted by hand is marked, refused and removable", async ({ page }) => {
  test.setTimeout(90_000);
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: "plug-4m", exact: true }).click();
  const id = page.url().match(/devices\/(\d+)/)![1];
  const csrf = await page.locator("input[name=csrf]").first().inputValue();
  const took = await page.request.post(`${app.url}/devices/${id}/backup`, { headers: admin, form: { csrf }, maxRedirects: 0 });
  expect(took.status(), "backup taken").toBe(303);
  await page.goto(`${app.url}/devices/${id}/backups`);
  const rows = page.locator(".bk-row");
  await expect(rows).toHaveCount(1);
  await expect(page.getByText("File missing")).toHaveCount(0);
  await expect(rows.getByRole("link", { name: /^Restore/ })).toHaveCount(1);
  const backupId = await rows.locator("input[name=id]").inputValue();

  // #290: the row names the stored file, the Download hover help repeats it, and a phone
  // screen wraps the name instead of scrolling sideways
  const file = (await rows.locator(".bk-file").innerText()).trim();
  expect(file, "the file name as on disk").toMatch(/\.dmp$/);
  await expect(rows.getByRole("link", { name: "Download" })).toHaveAttribute("title", `Download ${file}`);
  await page.setViewportSize({ width: 375, height: 700 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), "no sideways scroll").toBe(true);
  await page.setViewportSize({ width: 1280, height: 720 });

  // remove the files by hand, the way the reporter removed a directory
  const dir = join(app.dataDir, "backups");
  for (const sub of readdirSync(dir)) rmSync(join(dir, sub), { recursive: true });

  await page.reload();
  await expect(page.getByText("File missing", { exact: true })).toHaveCount(1);
  const gone = await page.request.get(`${app.url}/backups/${backupId}`, { headers: admin });
  expect(gone.status(), "the download answers with the reason, not a crash").toBe(404);
  expect(await gone.text()).toContain("file of this backup is missing");
  const missing = rows.filter({ hasText: "File missing" });
  await expect(missing.getByRole("link", { name: "Download" })).toHaveCount(0);
  await expect(missing.getByRole("link", { name: /^Restore/ })).toHaveCount(0);
  const restore = await page.request.get(`${app.url}/backups/${backupId}/restore`, { headers: admin });
  expect(restore.status(), "restore is refused with the reason").toBe(404);
  expect(await restore.text()).toContain("file of this backup is missing");

  await missing.getByRole("link", { name: "Delete" }).click();
  const dlg = page.locator("dialog#dlg-bkdel");
  await expect(dlg).toBeVisible();
  await dlg.getByRole("button", { name: "Delete backup" }).click();
  await expect(rows).toHaveCount(0);
  await expect(page.getByText("No backups yet")).toBeVisible();
});
