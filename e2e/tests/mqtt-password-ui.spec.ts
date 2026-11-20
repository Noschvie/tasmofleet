import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { expectNoOverflow } from "../support/layout";
import { aclNotice, aclShots } from "../support/aclshots";
import { seedPasswordStates, waitForOurs } from "../support/mqttpw";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// #609, the device MQTT password in the UI: the row on the device page (one word, one line, what it enables), the
// card on Settings, MQTT (counts, the devices without a password, "Use the broker password for these…") and the
// edit form's "No password". The fleet is the simulator's MQTT-only devices plus one that is not on the broker; the
// stored passwords are seeded (support/mqttpw.ts), because none of them can transfer a backup that would teach one.
// SHOT_DIR keeps screenshots (1280 and 375 px, light and dark).
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
const brokerSecret = "e2e-broker-secret-77";
let app: App;
let env: Devenv;

test.describe.configure({ mode: "serial" });

test.beforeAll(async () => {
  test.setTimeout(240_000);
  env = await startDevenv(["-only-mqtt-transport"]);
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_MQTT_PASSWORD: brokerSecret,
      TASMOFLEET_POLL_INTERVAL: "5s",
    },
  });
  const fleet = {
    "mq-esp32": env.devices["mq-esp32"],
    "mq-esp8266-1m": env.devices["mq-esp8266-1m"],
    "mq-fw-blocked": env.devices["mq-fw-blocked"],
    "mq-esp32-dual": env.devices["mq-esp32-dual"],
    "plain-http": { addr: "127.0.0.1:9", topic: "", profile: "esp8266" },
  };
  expect(app.cli(["import", "tasmoadmin", writeTasmoAdminDir(fleet)])).toContain("created: mq-esp32");
  await waitForOurs(app, ["mq-esp32", "mq-esp8266-1m", "mq-fw-blocked", "mq-esp32-dual"]);
  await seedPasswordStates(app, {
    "mq-esp32": { mode: "broker", source: "backup", setAgo: 20 * 3600_000 },
    "mq-fw-blocked": { mode: "empty", setAgo: 3 * 3600_000, badAgo: 5 * 60_000 },
    "mq-esp32-dual": { mode: "broker", setAgo: 2 * 3600_000 },
    "mq-esp8266-1m": { mode: "" },
  });
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

// openRow opens the device's page with the Connection fold open and returns the password row.
async function openRow(page: Page, name: string) {
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name, exact: true }).click();
  await page.locator("#devpaths-facts-sum").click();
  return page.locator("#devpaths-facts .ukv-row").filter({ hasText: "Device MQTT password" });
}

const enables = "back up, restore, read files and upload firmware over MQTT";

test("the device page says the state, where the password came from and what it enables", async ({ page }) => {
  const works = await openRow(page, "mq-esp32");
  await expect(works).toContainText("Works");
  await expect(works).toContainText("Same as TasmoFleet's broker password, taken from the backup of");
  await expect(works).toContainText(enables);
  await expect(works.getByRole("link", { name: "Change…" })).toBeVisible();
  await expect(works.locator(".pw-acts")).toContainText("Test");
  expect(await page.content()).not.toContain(brokerSecret);
  await aclShots(page, "device-works");
  await expectNoOverflow(page, "device page, password works");

  const notTested = await openRow(page, "mq-esp32-dual");
  await expect(notTested).toContainText("Not tested");
  await expect(notTested).toContainText("has not accepted it yet");

  const refused = await openRow(page, "mq-fw-blocked");
  await expect(refused).toContainText("Refused");
  await expect(refused).toContainText("backups and restores over MQTT are off");
  await aclShots(page, "device-refused");

  const unknown = await openRow(page, "mq-esp8266-1m");
  await expect(unknown).toContainText("Not known");
  await expect(unknown).toContainText("Needed to");
  await expect(unknown.getByRole("link", { name: "Enter…" })).toBeVisible();
  await expect(unknown.getByRole("link", { name: "Use the broker password…" })).toBeVisible();
  await aclShots(page, "device-unknown");

  const none = await openRow(page, "plain-http");
  await expect(none).toContainText("Not needed");
  await expect(none.locator(".pw-acts")).not.toContainText("Test");
  await aclShots(page, "device-not-needed");
});

test("the edit form offers No password", async ({ page }) => {
  await openRow(page, "mq-esp8266-1m");
  await page.getByRole("link", { name: "Edit" }).first().click();
  await expect(page.getByLabel("MQTT password source").locator("option")).toHaveText([
    "Not set",
    "Same as TasmoFleet's broker password",
    "Own password",
    "No password",
  ]);
  await aclShots(page, "edit-form");
});

test("the card counts the fleet, lists the devices without a password and gives them the broker password in one step", async ({ page }) => {
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/settings/system?tab=mqtt");
  const card = page.locator("#card-devicepasswords");
  await expect(card).toContainText("Device MQTT passwords");
  await expect(card.locator(".sys-sum").filter({ hasText: "On this broker" })).toContainText("4");
  await expect(card.locator(".sys-sum").filter({ hasText: "From backups" })).toContainText("1");
  await expect(card.locator(".sys-sum").filter({ hasText: "Not known" })).toContainText("1");
  await expect(card).toContainText("Not known (1)");
  await expect(card.locator(".sys-orphan").filter({ hasText: "mq-esp8266-1m" })).toContainText("TasmoFleet reaches it only over MQTT, so no backup can bring its password");
  await expect(card.getByRole("link", { name: "Set the password of mq-esp8266-1m" })).toBeVisible();
  expect(await page.content()).not.toContain(brokerSecret);
  await aclShots(page, "settings-mqtt");
  await expectNoOverflow(page, "settings, MQTT");

  // the dialog lists the device ticked, with the test on; Cancel changes nothing
  await card.getByRole("link", { name: /Use the broker password for 1 device/ }).click();
  const dlg = page.locator("#dlg-pwbroker");
  await expect(dlg).toBeVisible();
  await expect(dlg.getByRole("checkbox", { name: "mq-esp8266-1m" })).toBeChecked();
  await expect(dlg.getByRole("checkbox", { name: "Test each device afterwards" })).toBeChecked();
  await aclShots(page, "dialog");
  await dlg.getByRole("button", { name: "Cancel" }).click();
  await expect(dlg).toBeHidden();

  // the page of the same question (browsers without scripts) carries the broker ACL notice, since it starts tests
  await page.goto(app.url + "/settings/system/mqtt-passwords/broker");
  await expect(aclNotice(page)).toHaveCount(1);
  await expect(page.getByRole("checkbox", { name: "mq-esp8266-1m" })).toBeChecked();
  await aclShots(page, "confirm-page");
  await expectNoOverflow(page, "bulk confirmation page");

  // confirmed without the tests: the device has the broker password, never tested
  await page.getByRole("checkbox", { name: "Test each device afterwards" }).uncheck();
  await page.getByRole("button", { name: /Use the broker password/ }).click();
  await expect(page).toHaveURL(/ok=pw-bulk/);
  await expect(page.getByText("TasmoFleet's broker password is set for 1 device.")).toBeVisible();
  await expect(page.locator("#card-devicepasswords").locator(".sys-sum").filter({ hasText: "Not known" })).toContainText("0");
  const row = await openRow(page, "mq-esp8266-1m");
  await expect(row).toContainText("Not tested");
  await expect(row).toContainText("Same as TasmoFleet's broker password");
});
