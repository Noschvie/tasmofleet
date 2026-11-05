import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Why (#409): Devices -> Backups is the one page that says how the fleet is backed up: the last
// scheduled run, a row per device, the storage and the schedule. It lives in the Devices group,
// uses the Dashboard's width, and an operator sees it without anything that starts a fleet run.
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
      TASMOFLEET_BACKUP_INTERVAL: "1m", // the least the schedule accepts
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

test("the Devices menu has Backups between Policy and Add, and the page lists every device", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  const total = await page.locator("table[data-table=devices] tbody tr").count();
  await page.getByRole("navigation", { name: "Devices" }).getByRole("link", { name: "Backups" }).click();
  await expect(page).toHaveURL(/\/devices\/backups$/);
  await expect(page.getByRole("heading", { name: "Backups", level: 1 })).toBeVisible();
  await expect(page.locator(".bk-table tbody tr")).toHaveCount(total);
  await expect(page.locator(".dv-chip").first()).toContainText(String(total));
  // the chips count what they show
  for (const chip of await page.locator(".dv-chip").all()) {
    const n = Number((await chip.locator(".dv-n").textContent())!);
    await chip.click();
    await expect(page.locator(".bk-table tbody tr")).toHaveCount(n);
  }
});

test("the page is as wide as the Dashboard's content and never scrolls sideways", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  for (const width of [1280, 1920, 375]) {
    await page.setViewportSize({ width, height: 900 });
    await page.goto(app.url + "/");
    const dash = (await page.locator("main").boundingBox())!.width;
    await page.goto(app.url + "/devices/backups");
    const main = (await page.locator("main").boundingBox())!.width;
    expect(main, `main width at ${width}`).toBeCloseTo(dash, 0);
    const cards = await page.locator(".syspage .cstacks").boundingBox();
    expect(cards!.width, "the card stacks are not capped at the Settings width").toBeGreaterThan(width >= 1920 ? 1300 : main - 80);
    const sw = await page.evaluate(() => document.documentElement.scrollWidth);
    expect(sw, `no sideways scroll at ${width}`).toBeLessThanOrEqual(width);
  }
});

test("on a phone the filter chips scroll sideways inside their row", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 375, height: 800 });
  await page.goto(app.url + "/devices/backups?filter=skipped");
  const row = await page.locator(".dv-chips").evaluate((el) => ({ sc: el.scrollWidth, w: el.clientWidth, ox: getComputedStyle(el).overflowX }));
  expect(row.sc).toBeGreaterThan(row.w);
  expect(["auto", "scroll"]).toContain(row.ox);
});

test("Back up all now leads to the bulk backup page with the devices it would try", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices/backups");
  await expect(page.getByRole("button", { name: /Back up selected/ })).toBeDisabled();
  await page.locator(".bk-table tbody input[type=checkbox]").first().check();
  await expect(page.getByRole("button", { name: /Back up selected \(1\)/ })).toBeEnabled();
  await page.getByRole("button", { name: "Back up all now" }).click();
  await expect(page).toHaveURL(/\/devices\/bulk\/backup/);
});

test("the schedule saves on this page and the row menu shows what the role may do", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices/backups");
  await page.locator("input[name='v.BACKUP_KEEP']").fill("5");
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page).toHaveURL(/\/devices\/backups\?ok=saved/);
  await expect(page.locator("input[name='v.BACKUP_KEEP']")).toHaveValue("5");
  await page.locator(".bk-table tbody tr").first().locator("summary").click();
  await expect(page.locator(".bk-table tbody tr").first().getByRole("menuitem", { name: "Open backups" })).toBeVisible();
});

test("an operator sees the page read-only", async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/devices/backups");
  await expect(page.getByRole("heading", { name: "Backups", level: 1 })).toBeVisible();
  await expect(page.getByRole("button", { name: "Back up all now" })).toHaveCount(0);
  await expect(page.locator(".bk-table tbody input[type=checkbox]")).toHaveCount(0);
  await expect(page.locator("input[name='v.BACKUP_KEEP']")).toHaveCount(0);
  await expect(page.getByText("Admins change the schedule")).toBeVisible();
  await expect(page.getByRole("button", { name: "Save settings" })).toHaveCount(0);
});

test("after a scheduled run the status bar links to the page and the last run is listed", async ({ page }) => {
  test.setTimeout(150_000);
  await page.setExtraHTTPHeaders(admin);
  await expect(async () => {
    await page.goto(app.url + "/devices/backups");
    await expect(page.locator("#card-lastrun")).toContainText("Backed up", { timeout: 2000 });
  }).toPass({ timeout: 120_000 });
  await page.goto(app.url + "/");
  const item = page.locator(".sb-wide a.sb-i, footer .sb-wide a.sb-i").filter({ hasText: "Backup" }).first();
  await expect(item).toHaveAttribute("href", /\/devices\/backups$/);
});
