import { mkdirSync } from "node:fs";
import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { expectNoOverflow } from "../support/layout";
import { aclShots } from "../support/aclshots";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// #840: the broker ACL notice is big, red and bold on the pages where MQTT settings and MQTT-carried secrets appear.
// Without an ACL every client of the broker reads commands, passwords and device backups, and nothing in TasmoFleet can
// show that this is fixed, so the notice cannot be closed. SHOT_DIR keeps screenshots of each screen.
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
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

// the notice is red (a danger banner), its lead is bold and larger than the text, and there is nothing to close it
async function expectLoudNotice(page: Page): Promise<void> {
  const notice = page.locator("[data-acl-warning]").first();
  await expect(notice).toBeVisible();
  const lead = notice.locator(".acl-lead");
  await expect(lead).toContainText("Without a broker ACL, every client of your MQTT broker can read your commands, passwords and device backups.");
  const style = await lead.evaluate((e) => {
    const s = getComputedStyle(e);
    return { weight: Number(s.fontWeight), size: parseFloat(s.fontSize) };
  });
  expect(style.weight).toBeGreaterThanOrEqual(700);
  expect(style.size).toBeGreaterThanOrEqual(16);
  const text = await notice.locator(".acl-text").evaluate((e) => parseFloat(getComputedStyle(e).fontSize));
  expect(text).toBeGreaterThanOrEqual(14);
  // red: the danger tokens give the border a red hue (r clearly above g and b), light and dark
  const [r, g, b] = await notice.evaluate((e) => {
    // the computed colour may be in any CSS colour space: paint it and read the pixel
    const c = document.createElement("canvas").getContext("2d", { willReadFrequently: true })!;
    c.fillStyle = getComputedStyle(e).borderTopColor;
    c.fillRect(0, 0, 1, 1);
    return [...c.getImageData(0, 0, 1, 1).data.slice(0, 3)];
  });
  expect(r).toBeGreaterThan(g + 40);
  expect(r).toBeGreaterThan(b + 40);
  await expect(notice.getByRole("button")).toHaveCount(0);
  const link = notice.getByRole("link", { name: /Set up the ACL/ });
  await expect(link).toHaveAttribute("href", /github\.com\/hellwand\/tasmofleet\/blob\/main\/docs\/mqtt-broker-acl\.md$/);
  await expect(link).toHaveAttribute("target", "_blank");
}

for (const width of [1280, 375]) {
  for (const scheme of ["light", "dark"] as const) {
    test(`broker ACL notice at ${width}px ${scheme}`, async ({ page }) => {
      await page.setExtraHTTPHeaders(adam);
      await page.setViewportSize({ width, height: 1000 });
      await page.emulateMedia({ colorScheme: scheme });

      await page.goto(app.url + "/settings/system?tab=mqtt");
      await expectLoudNotice(page);
      // it stands at the top of the MQTT tab, above the broker fields
      const noticeBox = await page.locator("[data-acl-warning]").first().boundingBox();
      const hostBox = await page.locator("input[name='mqtt.host']").boundingBox();
      expect(noticeBox!.y + noticeBox!.height).toBeLessThanOrEqual(hostBox!.y);
      await shot(page, "system-mqtt", width, scheme);
      await expectNoOverflow(page, "system MQTT tab");

      // the first-run setup, where the broker is entered first
      await page.goto(app.url + "/setup");
      await expectLoudNotice(page);
      await shot(page, "setup", width, scheme);
      await expectNoOverflow(page, "setup");

      await page.goto(app.url + "/devices/1/edit");
      await expectLoudNotice(page);
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
      await expectLoudNotice(page);
      await shot(page, "bulk-backup", width, scheme);
      await expectNoOverflow(page, "bulk backup scope");
    });
  }
}

// a page that has nothing to do with the broker does not carry the notice
test("the notice stays off other pages", async ({ page }) => {
  await page.setExtraHTTPHeaders(adam);
  for (const path of ["/devices", "/settings/system?tab=network"]) {
    await page.goto(app.url + path);
    const shown = await page.locator("[data-acl-warning]:visible").count();
    expect(shown, path).toBe(0);
  }
});

// #853: a backup over MQTT carries every secret of the device through the broker, so the notice also stands, once, by
// "Back up all now" on Devices, Backups while a broker is configured (not repeated on the schedule card). A device that
// is not on the broker is backed up over HTTP: its Back up button has no notice (the device with the notice over MQTT
// is in mqtt-transport.spec.ts).
test("the notice stands by Back up all now only, not on the schedule or by an HTTP device's Back up", async ({ page }) => {
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices/backups");
  const devices = page.locator("#card-devices");
  const schedule = page.locator("#card-schedule");
  await expect(devices.getByRole("button", { name: "Back up all now" })).toBeVisible();
  await expect(devices.locator("[data-acl-warning]")).toHaveCount(1);
  await expect(schedule.locator("[data-acl-warning]")).toHaveCount(0);
  await expect(page.locator("[data-acl-warning]")).toHaveCount(1);
  await expectLoudNotice(page);
  // it stands above the device table, by the buttons
  const noticeBox = await devices.locator("[data-acl-warning]").boundingBox();
  const tableBox = await devices.locator("table").boundingBox();
  expect(noticeBox!.y + noticeBox!.height).toBeLessThanOrEqual(tableBox!.y);
  for (const width of [1280, 768, 375]) {
    await page.setViewportSize({ width, height: 1000 });
    await expectNoOverflow(page, `Backups page at ${width}`);
  }
  await aclShots(page, "fleet-backups");

  await page.goto(app.url + "/devices/1");
  await expect(page.getByRole("button", { name: "Back up", exact: true })).toBeVisible();
  await expect(page.locator("[data-acl-warning]")).toHaveCount(0);
});

// #880: nothing in TasmoFleet can see an ACL, so an admin says that the broker has one (System, MQTT). The notice then
// goes away everywhere, for this broker only. Shots of the notice with its second line, and of the pages after.
test("an admin's confirmation hides the notice; the second line points to it", async ({ page }) => {
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices/backups");
  await expect(page.locator("[data-acl-warning]")).toHaveCount(1);
  await expect(page.locator("[data-acl-warning] .acl-confirm")).toContainText("Confirm it under System");
  await aclShots(page, "backups-open");
  await page.goto(app.url + "/settings/system?tab=mqtt");
  await expect(page.locator("[data-acl-warning]")).toHaveCount(1);
  await expect(page.locator("[data-acl-warning] .acl-confirm")).toHaveCount(0);
  await aclShots(page, "system-mqtt-open");
  await page.getByLabel("This broker has an ACL", { exact: false }).check();
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.goto(app.url + "/settings/system?tab=mqtt");
  await expect(page.getByLabel("This broker has an ACL", { exact: false })).toBeChecked();
  await expect(page.locator("[data-acl-warning]")).toHaveCount(0);
  await aclShots(page, "system-mqtt-confirmed");
  await page.goto(app.url + "/devices/backups");
  await expect(page.getByRole("button", { name: "Back up all now" })).toBeVisible();
  await expect(page.locator("[data-acl-warning]")).toHaveCount(0);
  await aclShots(page, "backups-confirmed");
  // another broker host: the confirmation no longer applies
  await page.goto(app.url + "/settings/system?tab=mqtt");
  const host = page.locator('input[name="mqtt.host"]');
  const old = await host.inputValue();
  await host.fill("other-" + old);
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.goto(app.url + "/devices/backups");
  await expect(page.locator("[data-acl-warning]")).toHaveCount(1);
});
