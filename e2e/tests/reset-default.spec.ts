import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// #623: "Reset to default" shows, right after the click and before Save, the value Save will store,
// says it is the default, and a reset that changes what a switch shows changes that switch at once.
// The backup days and a snapshot interval have no Reset: their default is "off", which unticking the days or the schedule switch does.
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

test("Devices, Backups: a reset shows the default at once, the schedule stays on, Save stores it", { tag: "@core" }, async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices/backups");
  // a schedule that is on, with a kept count and a protection age that differ from their defaults
  await page.locator("input[name='v.BACKUP_DAYS'][value='mon']").check();
  await page.locator("input[name='v.BACKUP_KEEP']").fill("5");
  await page.locator("input[name='v.BACKUP_MAX_AGE']").fill("48h");
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.goto(app.url + "/devices/backups");
  const sw = page.locator("input[name='v.BACKUP_DAYS'][value='mon']");
  await expect(sw).toBeChecked();
  await expect(page.locator("input[name='v.BACKUP_TIME']")).toHaveValue("03:00");
  // the days have no Reset: their default would be "no day", which unticking them does
  await expect(page.locator("input[name='clear.BACKUP_DAYS']")).toHaveCount(0);

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
    await expect(sw).toBeChecked(); // the schedule days are untouched
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
  await expect(page.locator("input[name='v.BACKUP_DAYS'][value='mon']")).toBeChecked();
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

// Every kind of control takes the reset the same way (#623). A radio group: undo brings the stored choice
// back, also when it sits after the default (checking the default radio unchecks the stored one, so the old
// state must be taken before anything changes). A text area: typing cancels the reset, as in a field, or
// Save would throw the typed networks away. A reset switch: the settings that hang on it follow at once.
test("System settings: undo brings a radio choice back, typing in a text area cancels a reset, dependents follow", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=general");
  await page.locator("input[name='v.LOG_LEVEL'][value='debug']").check({ force: true });
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.goto(app.url + "/settings/system?tab=general");
  const level = page.locator("input[name='v.LOG_LEVEL']:checked");
  await expect(level).toHaveValue("debug");
  await reset(page, "LOG_LEVEL").click();
  await expect(level).toHaveValue("info");
  await reset(page, "LOG_LEVEL").click();
  await expect(level).toHaveValue("debug");

  await page.goto(app.url + "/settings/system?tab=discovery");
  await field(page, "DISCOVERY_SCAN").locator(".swtrack").click();
  const nets = page.locator("textarea[name='v.DISCOVERY_SCAN_CIDRS']");
  await nets.fill("127.0.0.0/24");
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.goto(app.url + "/settings/system?tab=discovery");
  await reset(page, "DISCOVERY_SCAN_CIDRS").click();
  await expect(nets).toHaveValue("");
  await nets.fill("127.0.1.0/24");
  await expect(page.locator("input[name='clear.DISCOVERY_SCAN_CIDRS']")).not.toBeChecked();
  // the scan switch is off by default: resetting it dims its settings at once, undoing it brings them back
  const scanDeps = page.locator(".dep[data-depends='v.DISCOVERY_SCAN']");
  await expect(scanDeps).not.toHaveClass(/is-off/);
  await reset(page, "DISCOVERY_SCAN").click();
  await expect(scanDeps).toHaveClass(/is-off/);
  await reset(page, "DISCOVERY_SCAN").click();
  await expect(scanDeps).not.toHaveClass(/is-off/);
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.goto(app.url + "/settings/system?tab=discovery");
  await expect(nets).toHaveValue("127.0.1.0/24");
});

// Why (#623): the broker row resets as host, port and TLS; TLS left On would show a broker that Save
// does not keep.
test("the broker reset empties host and port and turns TLS off at once; undo and editing work", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=mqtt");
  const host = page.locator("input[name='mqtt.host']");
  const port = page.locator("input[name='mqtt.port']");
  const tls = page.locator("input[name='mqtt.tls']");
  await host.fill("127.0.0.1");
  await port.fill("18883");
  if (!(await tls.isChecked()))
    await page.locator("label.swhit:has(#f-mqtt-tls) .swtrack").click();
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.goto(app.url + "/settings/system?tab=mqtt");
  await expect(host).toHaveValue("127.0.0.1");
  await expect(tls).toBeChecked();
  const box = page.locator("input[name='clear.MQTT_URL']");
  await reset(page, "MQTT_URL").click();
  await expect(box).toBeChecked();
  await expect(host).toHaveValue("");
  await expect(port).toHaveValue("");
  await expect(tls).not.toBeChecked();
  await reset(page, "MQTT_URL").click();
  await expect(box).not.toBeChecked();
  await expect(host).toHaveValue("127.0.0.1");
  await expect(port).toHaveValue("18883");
  await expect(tls).toBeChecked();
  // switching TLS by hand after a reset is the person's own choice: the reset is off again
  await reset(page, "MQTT_URL").click();
  await expect(tls).not.toBeChecked();
  await page.locator("label.swhit:has(#f-mqtt-tls) .swtrack").click();
  await expect(tls).toBeChecked();
  await expect(box).not.toBeChecked();
});


// Why (#623): the narrow-field wrap makes room for "Default" only while the box is ticked (a wrap on
// every narrow field moved untouched rows), and the box is named "Default, set on save" for a screen reader.
test("a narrow row wraps only while its reset is ticked; the box is named by its text", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices/backups");
  await page.locator("input[name='v.BACKUP_KEEP']").fill("5");
  await page.locator("input[name='v.BACKUP_MAX_AGE']").fill("48h");
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.goto(app.url + "/devices/backups");
  const wrap = (key: string) =>
    field(page, key)
      .locator(".lrow")
      .first()
      .evaluate((e) => getComputedStyle(e).flexWrap);
  const before = {
    keep: await wrap("BACKUP_KEEP"),
    age: await wrap("BACKUP_MAX_AGE"),
  };
  await reset(page, "BACKUP_KEEP").click();
  const after = {
    keep: await wrap("BACKUP_KEEP"),
    age: await wrap("BACKUP_MAX_AGE"),
  };
  const named = await field(page, "BACKUP_KEEP")
    .getByRole("checkbox", { name: "Default, set on save", exact: true })
    .count();
  await reset(page, "BACKUP_MAX_AGE").click();
  // a narrow field on another tab with nothing ticked keeps one row
  await page.goto(app.url + "/settings/system?tab=network");
  const other = await field(page, "CR_WARN_PERCENT")
    .locator(".lrow")
    .first()
    .evaluate((e) => getComputedStyle(e).flexWrap);
  expect(before).toEqual({ keep: "nowrap", age: "nowrap" });
  expect(after).toEqual({ keep: "wrap", age: "nowrap" });
  expect(other).toBe("nowrap");
  expect(named).toBe(1);
});
