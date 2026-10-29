import { mkdirSync } from "node:fs";
import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { expectNoOverflow } from "../support/layout";
import {
  App,
  Devenv,
  startApp,
  startDevenv,
  writeTasmoAdminDir,
} from "../support/stack";

// #125: a name set by hand is never overwritten by "Refresh identity", but the Identity card says
// what the device calls itself, with "Use device name" and "Keep". Keep is remembered for exactly
// that name. UX_SHOTS=<dir> keeps screenshots.
const shots = process.env.UX_SHOTS;
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
let app: App;
let env: Devenv;
let shotDevice = "";
test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

// handNamed opens the device, types its name by hand, refreshes, and returns its page path and
// the DeviceName the device reports.
async function handNamed(page: Page, device: string, name: string) {
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  const href =
    (await page
      .getByRole("link", { name: device, exact: true })
      .getAttribute("href")) ?? "";
  await page.goto(app.url + href + "/edit");
  await page.locator("input[name=name]").fill(name);
  await page.getByRole("button", { name: "Save changes" }).click();
  await page.goto(app.url + href);
  await page.getByRole("button", { name: "Refresh identity" }).click();
  await expect(page.getByText("Identity refreshed.")).toBeVisible();
  const offer = page.locator(".name-offer");
  await expect(offer).toContainText("Name on the device:");
  const reported = (await offer.locator("b").innerText()).trim();
  return { href, reported };
}

test("a hand-set name stays; Use device name adopts, Keep is remembered", async ({
  page,
}) => {
  const names = Object.keys(env.devices);
  const keep = await handNamed(page, names[0], "Lüftung Keller");
  // the name was not overwritten, the card offers the device's
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(
    "Lüftung Keller",
  );
  await page.getByRole("button", { name: "Keep" }).click();
  await expect(page.locator(".name-offer")).toHaveCount(0);
  await page.reload();
  await expect(page.locator(".name-offer")).toHaveCount(0);
  await page.getByRole("button", { name: "Refresh identity" }).click();
  await expect(page.getByText("Identity refreshed.")).toBeVisible();
  await expect(page.locator(".name-offer")).toHaveCount(0); // the same device name stays declined
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(
    "Lüftung Keller",
  );
  expect(keep.reported).not.toBe("");

  const use = await handNamed(page, names[1], "Zirkulation alt");
  await page.getByRole("button", { name: "Use device name" }).click();
  await expect(page.locator(".name-offer")).toHaveCount(0);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(
    use.reported,
  );
  await expect(page.locator(".crumbs .crumb-here")).toHaveText(use.reported);
});

for (const width of [375, 1280]) {
  for (const scheme of ["light", "dark"] as const) {
    test(`the name notice at ${width}px ${scheme}`, async ({ page }) => {
      await page.emulateMedia({ colorScheme: scheme });
      await page.setViewportSize({ width, height: 900 });
      await handNamed(page, shotDevice || Object.keys(env.devices)[2], "Modbus Lüftung alt");
      shotDevice = "Modbus Lüftung alt"; // the link name of the next screenshot run
      await expectNoOverflow(page, "device page with the name notice");
      if (!shots) return;
      mkdirSync(shots, { recursive: true });
      await page.screenshot({
        path: `${shots}/name-offer-${width}-${scheme}.png`,
        fullPage: true,
      });
    });
  }
}
