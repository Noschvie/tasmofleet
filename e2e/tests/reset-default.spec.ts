import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";
import { openSchedule } from "../support/nav";

// #623: "Reset to default" shows, right after the click and before Save, the value Save will store,
// says it is the default, and a reset that changes what a switch shows changes that switch at once.
// The backup and snapshot days have no Reset: their default is no day, which unticking the days does.
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
  await openSchedule(page);
  // a schedule that is on, with a kept count and a protection age that differ from their defaults
  await page.locator("input[name='v.BACKUP_DAYS'][value='mon']").check();
  await page.locator("input[name='v.BACKUP_KEEP']").fill("5");
  await page.locator("input[name='v.BACKUP_MAX_AGE']").fill("48h");
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.goto(app.url + "/devices/backups");
  await openSchedule(page);
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
    await expect(reset(page, key).locator(".srst-on")).toContainText("Undo"); // the default already shows: a second click undoes it (#1075)
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

test("System, Snapshots: the snapshot days have no Reset", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system/snapshots");
  await page.locator("input[name='v.SNAPSHOT_DAYS'][value='mon']").check();
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.goto(app.url + "/settings/system/snapshots");
  await expect(page.locator("input[name='v.SNAPSHOT_DAYS'][value='mon']")).toBeChecked();
  await expect(page.locator("input[name='clear.SNAPSHOT_DAYS']")).toHaveCount(0);
});

// A reset that changes what a switch shows updates the switch at once (METRICS_DEVICE_LABELS is off by default).
test("System, General: resetting a stored switch turns it to its default at once", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=general");
  const sw = page.locator("input[name='v.METRICS_DEVICE_LABELS']");
  await field(page, "METRICS_DEVICE_LABELS").locator(".uswitch").click();
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
  await field(page, "DISCOVERY_SCAN").locator(".uswitch").click();
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
    await page.locator("label.uswitch:has(#f-mqtt-tls)").click();
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
  await page.locator("label.uswitch:has(#f-mqtt-tls)").click();
  await expect(tls).toBeChecked();
  await expect(box).not.toBeChecked();
});


// Why (#623, #1075): a narrow field (w-xs) used to show a bare arrow glyph and, ticked, "arrow Default" in a wrapped
// row: no words to read, and a row that moved under the pointer. Every width now says "Reset to default" in words,
// ticked it reads "Undo" (the field already holds the default), and neither state moves the row or its neighbour.
test("a narrow field says Reset to default in words and reads Undo when ticked, without moving", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices/backups");
  await openSchedule(page);
  await page.locator("input[name='v.BACKUP_KEEP']").fill("5");
  await page.locator("input[name='v.BACKUP_MAX_AGE']").fill("48h");
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.goto(app.url + "/devices/backups");
  await openSchedule(page);
  await expect(field(page, "BACKUP_KEEP")).toHaveClass(/w-xs/); // the field this test is about is a narrow one
  const row = (key: string) =>
    field(page, key)
      .locator(".lrow")
      .first()
      .evaluate((e) => ({ wrap: getComputedStyle(e).flexWrap, ...(({ y, height }) => ({ y: y + scrollY, height }))(e.getBoundingClientRect()) }));
  const ctl = reset(page, "BACKUP_KEEP");
  const off = ctl.locator(".srst-off");
  const before = { keep: await row("BACKUP_KEEP"), age: await row("BACKUP_MAX_AGE") };
  // in words: the text has a laid-out width (it was font-size 0 behind a glyph)
  await expect(off).toBeVisible();
  await expect(off).toHaveText("Reset to default");
  const textW = await off.evaluate((e) => {
    const r = document.createRange();
    r.selectNodeContents(e);
    return r.getBoundingClientRect().width;
  });
  expect(textW).toBeGreaterThan(60);
  // two narrow fields side by side in a half-width card: the label stays on one line (the reset drops under it
  // when label, info button and words do not fit) instead of breaking into one word per line
  const lblH = await field(page, "BACKUP_KEEP").locator(".lbl").evaluate((e) => e.getBoundingClientRect().height);
  expect(lblH).toBeLessThan(20);
  const w0 = (await ctl.boundingBox())!.width;

  await ctl.click();
  await expect(field(page, "BACKUP_KEEP").locator("input[name='v.BACKUP_KEEP']")).toHaveValue("10"); // the default shows before Save
  await expect(ctl.locator(".srst-on")).toBeVisible();
  await expect(ctl.locator(".srst-on")).toContainText("Undo");
  await expect(off).toBeHidden();
  // the box is named by its visible text and says what it does
  await expect(field(page, "BACKUP_KEEP").getByRole("checkbox", { name: "Undo reset to default", exact: true })).toHaveCount(1);
  const after = { keep: await row("BACKUP_KEEP"), age: await row("BACKUP_MAX_AGE") };
  expect(after).toEqual(before); // neither this row nor its neighbour moved
  expect(Math.abs((await ctl.boundingBox())!.width - w0)).toBeLessThan(1); // the control keeps its width
  expect(before.keep.wrap).toBe("wrap"); // a row with a reset may wrap, whether or not it is ticked (the rows compared above hold wrap too)

  await ctl.click(); // undo: the stored value is back, the words are back
  await expect(field(page, "BACKUP_KEEP").locator("input[name='v.BACKUP_KEEP']")).toHaveValue("5");
  await expect(off).toBeVisible();
});

// Why (#1075): the phone rule for checkbox labels (display: flex) used to beat the reset's one-cell grid, so the
// hidden text kept its room beside the visible one: the control was wider than its words, "Reset to default" ended
// short of the field's edge, and "Undo" sat after a blank gap. At 375 px both texts start at the same left edge,
// the control is as wide as the words it shows, and on a wide field it ends at the label row's right edge.
test("at 375 px the reset control is one cell: both texts share a left edge, no blank gap, flush right", async ({
  page,
}) => {
  await page.setViewportSize({ width: 375, height: 800 });
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices/backups");
  await openSchedule(page);
  await page.locator("input[name='v.BACKUP_KEEP']").fill("6"); // not the default and not the 5 of the test before: Save shows only for a change
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.goto(app.url + "/settings/system?tab=firmware");
  await page.locator("input[name='v.FIRMWARE_BASE_URL']").fill("http://192.0.2.1:8080");
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.waitForLoadState("load");

  const check = async (key: string, url: string, flushRight: boolean) => {
    await page.goto(app.url + url);
    if (url === "/devices/backups") await openSchedule(page);
    const ctl = reset(page, key);
    for (const ticked of [false, true]) {
      if (ticked) await ctl.click();
      const m = await ctl.evaluate((e) => {
        const r = (el: Element) => el.getBoundingClientRect();
        const off = r(e.querySelector(".srst-off")!);
        const on = r(e.querySelector(".srst-on")!);
        const c = r(e);
        return { display: getComputedStyle(e).display, offL: off.left, onL: on.left, offW: off.width, w: c.width, right: c.right, metaRight: r(e.closest(".meta")!).right };
      });
      const where = `${key} ${ticked ? "ticked" : "unticked"}`;
      expect(m.display, where).toMatch(/grid$/); // "grid": a flex item is blockified
      expect(Math.abs(m.offL - m.onL), `${where}: both texts start at the same left edge`).toBeLessThan(1);
      expect(m.w, `${where}: the control is no wider than the longer text`).toBeLessThanOrEqual(m.offW + 1);
      if (flushRight) expect(Math.abs(m.right - m.metaRight), `${where}: ends at the label row's right edge`).toBeLessThan(1);
    }
  };
  try {
    await check("FIRMWARE_BASE_URL", "/settings/system?tab=firmware", true); // a wide field
    await check("BACKUP_KEEP", "/devices/backups", false); // a narrow one
  } finally {
    // put the stored values back: this file's tests share one app
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.goto(app.url + "/settings/system?tab=firmware");
    await reset(page, "FIRMWARE_BASE_URL").click();
    await page.getByRole("button", { name: "Save settings" }).click();
    await page.waitForLoadState("load");
    await page.goto(app.url + "/devices/backups");
    await openSchedule(page);
    await reset(page, "BACKUP_KEEP").click();
    await page.getByRole("button", { name: "Save settings" }).click();
    await page.waitForLoadState("load");
  }
});
