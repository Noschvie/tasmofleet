import { mkdirSync } from "node:fs";
import type { Locator, Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { expectNoOverflow } from "../support/layout";
import { ACL_LEAD, aclNotice, aclShots } from "../support/aclshots";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// #840, #1078: without an ACL every client of the broker reads commands, passwords and device backups, and nothing in
// TasmoFleet can show that this is fixed, so the notice cannot be closed. Since #1078 it is one short warn banner, the
// same on every page that shows it (the lead, one sentence, one action: the guide): the big red version read as an
// incident and pushed each page's own content away. Pages that are not the place for it (the setup, the device's Edit
// form) carry a plain help line with the guide instead. SHOT_DIR keeps screenshots of each screen.
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
const olga = { "X-authentik-username": "olga", "X-authentik-groups": "ops" };
const sentence = "Any client on it can read device passwords and backups.";
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv();
  const fwPort = await freePort(); // the bulk pages come with the firmware server
  app = await startApp({
    env: {
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

async function shot(page: Page, name: string, width: number, scheme: string): Promise<void> {
  if (!process.env.SHOT_DIR) return;
  mkdirSync(process.env.SHOT_DIR, { recursive: true });
  await page.screenshot({ path: `${process.env.SHOT_DIR}/${name}-${width}-${scheme}.png`, fullPage: true });
}

// the notice is the one short warn banner: tone warn (never danger), the shared lead in bold, the one sentence in
// regular weight, exactly one action (the guide, in a new tab) and nothing that closes it
async function expectShortNotice(scope: Page | Locator): Promise<void> {
  const notice = aclNotice(scope);
  await expect(notice).toHaveCount(1);
  await expect(notice).toBeVisible();
  await expect(notice).toHaveClass(/\btone-warn\b/);
  await expect(notice).not.toHaveClass(/\btone-danger\b/);
  await expect(notice.locator("b")).toHaveText(ACL_LEAD);
  await expect(notice.locator(".banner-text")).toHaveText(`${ACL_LEAD} ${sentence}`);
  const weight = await notice.locator(".banner-text").evaluate((e) => Number(getComputedStyle(e).fontWeight));
  expect(weight, "the sentence is regular weight, only the lead is bold").toBeLessThan(600);
  await expect(notice.getByRole("button")).toHaveCount(0);
  await expect(notice.getByRole("link")).toHaveCount(1);
  const link = notice.getByRole("link", { name: /Set up the ACL/ });
  await expect(link).toHaveAttribute("href", /github\.com\/hellwand\/tasmofleet\/blob\/main\/docs\/mqtt-broker-acl\.md$/);
  await expect(link).toHaveAttribute("target", "_blank");
}

// the plain help line where the notice does not stand: the guide, in a new tab, and no notice
async function expectHelpLine(page: Page): Promise<void> {
  await expect(aclNotice(page)).toHaveCount(0);
  const help = page.locator(".acl-help");
  await expect(help).toHaveCount(1);
  await expect(help.getByRole("link", { name: /Set up the ACL/ })).toHaveAttribute("target", "_blank");
}

for (const width of [1280, 375]) {
  for (const scheme of ["light", "dark"] as const) {
    test(`broker ACL notice at ${width}px ${scheme}`, async ({ page }) => {
      await page.setExtraHTTPHeaders(adam);
      await page.setViewportSize({ width, height: 1000 });
      await page.emulateMedia({ colorScheme: scheme });

      await page.goto(app.url + "/settings/system?tab=mqtt");
      await expectShortNotice(page);
      // it stands at the top of the MQTT tab, above the broker fields
      const noticeBox = await aclNotice(page).boundingBox();
      const hostBox = await page.locator("input[name='mqtt.host']").boundingBox();
      expect(noticeBox!.y + noticeBox!.height).toBeLessThanOrEqual(hostBox!.y);
      await shot(page, "system-mqtt", width, scheme);
      await expectNoOverflow(page, "system MQTT tab");

      // the setup's last step: no notice (no broker is usable there yet), a help line with the guide
      await page.goto(app.url + "/setup");
      await expectHelpLine(page);
      await shot(page, "setup", width, scheme);
      await expectNoOverflow(page, "setup");

      // the device's Edit form: no notice, a help line under the MQTT password source
      await page.goto(app.url + "/devices/1/edit");
      await expectHelpLine(page);
      const source = await page.getByLabel("MQTT password source").boundingBox();
      const help = await page.locator(".acl-help").boundingBox();
      expect(help!.y).toBeGreaterThanOrEqual(source!.y + source!.height);
      await shot(page, "device-edit", width, scheme);
      await expectNoOverflow(page, "device edit");

      // the page that starts bulk backups, with a broker configured
      await page.goto(app.url + "/devices");
      const row = page.getByRole("row").filter({ hasText: "plug-4m-http" });
      if (width > 700) {
        await expect(row).toBeVisible();
        await row.getByRole("checkbox").check();
        await page.locator("[data-bulkbar]").getByRole("button", { name: /Back up/ }).click();
      } else {
        // the phone layout has no row checkboxes: post what the bar posts
        await page.evaluate(() => {
          const f = document.createElement("form");
          f.method = "post";
          f.action = "devices/bulk/backup";
          const csrf = JSON.parse(document.body.getAttribute("hx-headers") ?? "{}")["X-CSRF-Token"];
          for (const [k, v] of [["csrf", csrf], ["id", "1"]]) {
            const i = document.createElement("input");
            i.name = k;
            i.value = v;
            f.append(i);
          }
          document.body.append(f);
          f.submit();
        });
      }
      await expectShortNotice(page);
      await shot(page, "bulk-backup", width, scheme);
      await expectNoOverflow(page, "bulk backup scope");
    });
  }
}

// #1078 (A6-002): the setup's last step was about 1400 px tall at 1280, most of it the red notice between the device
// networks and the broker host. The wizard card is now shorter than 900 px.
test("the setup's last step is short at 1280px", async ({ page }) => {
  await page.setExtraHTTPHeaders(adam);
  await page.setViewportSize({ width: 1280, height: 1000 });
  await page.goto(app.url + "/setup");
  await expect(page.getByRole("heading", { name: "Basic settings" })).toBeVisible();
  const card = await page.locator(".auth-card").boundingBox();
  expect(card!.height, "the setup wizard card at 1280 px").toBeLessThan(900);
});

// #1078 (A6-021): the help under "Hide the broker ACL warning" starts where the label's text starts, not under the box
test("the hide option's help is aligned with its label", async ({ page }) => {
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/settings/system?tab=mqtt");
  const option = page.locator("label.sopt", { hasText: "Hide the broker ACL warning" });
  const label = await option.locator("span").first().boundingBox();
  const help = await option.locator("xpath=following-sibling::small[1]").boundingBox();
  expect(Math.abs(help!.x - label!.x), "the help's left edge is the label text's").toBeLessThanOrEqual(1);
});

// a page that has nothing to do with the broker does not carry the notice
test("the notice stays off other pages", async ({ page }) => {
  await page.setExtraHTTPHeaders(adam);
  for (const path of ["/devices", "/settings/system?tab=network"]) {
    await page.goto(app.url + path);
    await expect(aclNotice(page).filter({ visible: true }), path).toHaveCount(0);
  }
});

// #853: a backup over MQTT carries every secret of the device through the broker, so the notice also stands, once, by
// "Back up all now" on Devices, Backups while a broker is configured (not repeated on a card). Since #684 the button is
// in the page header and the notice is right below it, above the cards. A device that is not on the broker is backed up
// over HTTP: its Back up button has no notice (the device with the notice over MQTT is in mqtt-transport.spec.ts).
test("the notice stands by Back up all now only, not on a card or by an HTTP device's Back up", async ({ page }) => {
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices/backups");
  const head = page.locator(".pghead");
  const button = head.getByRole("button", { name: "Back up all now" });
  await expect(button).toBeVisible();
  await expect(page.locator("#card-devices").getByRole("button", { name: "Back up all now" })).toHaveCount(0);
  await expect(aclNotice(page.locator(".syspage"))).toHaveCount(0);
  await expectShortNotice(page);
  // it stands below the button and above the first card
  const buttonBox = await button.boundingBox();
  const noticeBox = await aclNotice(page).boundingBox();
  const cardBox = await page.locator("#card-lastrun").boundingBox();
  expect(buttonBox!.y + buttonBox!.height).toBeLessThanOrEqual(noticeBox!.y);
  expect(noticeBox!.y + noticeBox!.height).toBeLessThanOrEqual(cardBox!.y);
  for (const width of [1280, 768, 375]) {
    await page.setViewportSize({ width, height: 1000 });
    await expectNoOverflow(page, `Backups page at ${width}`);
  }
  await aclShots(page, "fleet-backups");

  await page.goto(app.url + "/devices/1");
  await expect(page.getByRole("button", { name: "Back up", exact: true })).toBeVisible();
  await expect(aclNotice(page)).toHaveCount(0);
});

// #1078: an operator sees the same short banner as an admin, with no second action to hide it
test("an operator sees the same banner, with nothing that hides it", async ({ page }) => {
  await page.setExtraHTTPHeaders(olga);
  await page.goto(app.url + "/devices/backups");
  await expectShortNotice(page);
  await expect(page.getByText("Hide it")).toHaveCount(0);
  await aclShots(page, "backups-operator");
});

// #880, #962, #1078: nothing in TasmoFleet can see an ACL, so an admin can hide the warning for this broker on System,
// MQTT; it goes away everywhere (the dashboard's item too), for this broker only. Shots of the pages before and after.
test("an admin hides the notice for this broker", async ({ page }) => {
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/");
  const item = page.locator(".att-item", { hasText: "MQTT broker" });
  await expect(item).toHaveCount(1);
  await expect(item).toContainText("No ACL");
  await expect(item).toHaveAttribute("href", /mqtt-broker-acl\.md$/);
  await aclShots(page, "dashboard-open");
  await page.goto(app.url + "/devices/backups");
  await expect(aclNotice(page)).toHaveCount(1);
  await aclShots(page, "backups-open");
  await page.goto(app.url + "/settings/system?tab=mqtt");
  await expect(aclNotice(page)).toHaveCount(1);
  await aclShots(page, "system-mqtt-open");

  const box = page.getByLabel("Hide the broker ACL warning");
  await expect(page.getByText("Every client of this broker can read device backups, passwords included. The warning comes back when you change the broker.")).toBeVisible();
  await box.check();
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.goto(app.url + "/settings/system?tab=mqtt");
  await expect(page.getByLabel("Hide the broker ACL warning")).toBeChecked();
  await expect(aclNotice(page)).toHaveCount(0);
  await aclShots(page, "system-mqtt-hidden");
  await page.goto(app.url + "/devices/backups");
  await expect(page.getByRole("button", { name: "Back up all now" })).toBeVisible();
  await expect(aclNotice(page)).toHaveCount(0);
  await aclShots(page, "backups-hidden");
  await page.goto(app.url + "/");
  await expect(page.locator(".att-item", { hasText: "MQTT broker" })).toHaveCount(0);
  // another broker host: the hidden warning no longer applies
  await page.goto(app.url + "/settings/system?tab=mqtt");
  const host = page.locator('input[name="mqtt.host"]');
  const old = await host.inputValue();
  await host.fill("other-" + old);
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.goto(app.url + "/devices/backups");
  await expect(aclNotice(page)).toHaveCount(1);
});
