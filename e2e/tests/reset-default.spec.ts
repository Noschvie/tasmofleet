import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// #623: "Reset to default" shows, right after the click and before Save, the value Save will store,
// says it is the default, and a reset that changes what a switch shows changes that switch at once.
// A schedule's interval has no Reset: its default (0s) is "off", which the schedule switch does.
let app: App;
test.beforeAll(async () => {
  test.setTimeout(120_000);
  app = await startApp({
    env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32", TASMOFLEET_ADMIN_GROUPS: "adm" },
  });
});
test.afterAll(async () => {
  await app?.stop();
});
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
const field = (page: import("@playwright/test").Page, key: string) => page.locator(`.sf[data-key='${key}']`);
const reset = (page: import("@playwright/test").Page, key: string) => field(page, key).locator("label.srst");

test("Devices, Backups: a reset shows the default at once, the schedule stays on, Save stores it", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices/backups");
  // a schedule that is on, with a kept count and a protection age that differ from their defaults
  await field(page, "BACKUP_SCHEDULE").locator(".swtrack").click();
  await page.locator("input[name='v.BACKUP_KEEP']").fill("5");
  await page.locator("input[name='v.BACKUP_MAX_AGE']").fill("48h");
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.goto(app.url + "/devices/backups");
  const sw = page.locator("input[name='sw.BACKUP_INTERVAL']");
  await expect(sw).toBeChecked();
  await expect(page.locator("input[name='v.BACKUP_INTERVAL']")).toHaveValue("24h");
  // the interval has no Reset: its default would be "off", which is the switch's job
  await expect(page.locator("input[name='clear.BACKUP_INTERVAL']")).toHaveCount(0);

  for (const [key, was, def] of [["BACKUP_KEEP", "5", "10"], ["BACKUP_MAX_AGE", "48h", "720h"]]) {
    const input = page.locator(`input[name='v.${key}']`);
    await expect(input).toHaveValue(was);
    await reset(page, key).click();
    // right after the click, before Save: the default, enabled, marked as the default
    await expect(input).toHaveValue(def);
    await expect(input).toBeEnabled();
    await expect(reset(page, key).locator(".srst-on")).toBeVisible();
    await expect(reset(page, key)).toContainText("Default");
    await expect(page.locator("[data-sbar-state]")).toContainText("Unsaved changes");
    await expect(sw).toBeChecked(); // the schedule is untouched
    // changing one's mind brings the old value back
    await reset(page, key).click();
    await expect(input).toHaveValue(was);
  }
  // typing a value after a reset is the person's choice: it wins and the reset is off again
  await reset(page, "BACKUP_KEEP").click();
  await page.locator("input[name='v.BACKUP_KEEP']").fill("7");
  await expect(page.locator("input[name='clear.BACKUP_KEEP']")).not.toBeChecked();
  await reset(page, "BACKUP_KEEP").click();
  await reset(page, "BACKUP_MAX_AGE").click();
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.goto(app.url + "/devices/backups");
  await expect(page.locator("input[name='v.BACKUP_KEEP']")).toHaveValue("10");
  await expect(page.locator("input[name='v.BACKUP_MAX_AGE']")).toHaveValue("720h");
  await expect(page.locator("input[name='sw.BACKUP_INTERVAL']")).toBeChecked();
});

test("System, Snapshots: the snapshot interval has no Reset either while the schedule is on", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system/snapshots");
  await field(page, "SNAPSHOT_SCHEDULE").locator(".swtrack").click();
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.goto(app.url + "/settings/system/snapshots");
  await expect(page.locator("input[name='sw.SNAPSHOT_INTERVAL']")).toBeChecked();
  await expect(page.locator("input[name='clear.SNAPSHOT_INTERVAL']")).toHaveCount(0);
});

// A reset that changes what a switch shows updates the switch at once (METRICS_DEVICE_LABELS is off by default).
test("System, General: resetting a stored switch turns it to its default at once", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=general");
  const sw = page.locator("input[name='v.METRICS_DEVICE_LABELS']");
  await field(page, "METRICS_DEVICE_LABELS").locator(".swtrack").click();
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.goto(app.url + "/settings/system?tab=general");
  await expect(sw).toBeChecked();
  await reset(page, "METRICS_DEVICE_LABELS").click();
  await expect(sw).not.toBeChecked();
  await expect(reset(page, "METRICS_DEVICE_LABELS").locator(".srst-on")).toBeVisible();
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.goto(app.url + "/settings/system?tab=general");
  await expect(sw).not.toBeChecked();
});
