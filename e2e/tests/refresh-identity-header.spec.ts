import { test, expect } from "../support/fixtures";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// #104: "Refresh identity" swaps only the identity card, so the header, the breadcrumb and the
// page title kept the old name until a reload, and the list showed it too. After a good refresh
// all of them show the name the device reports now, with no reload.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv();
  app = await startApp({
    env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32", TASMOFLEET_ADMIN_GROUPS: "adm" },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

test("a refreshed name shows in the header, breadcrumb, title and list without a reload", async ({ page }) => {
  await page.setExtraHTTPHeaders({ "X-authentik-username": "adam", "X-authentik-groups": "adm" });
  await page.goto(app.url + "/devices");
  const href = await page.getByRole("link", { name: "plug-4m", exact: true }).getAttribute("href");
  // a device still named after Tasmota's default DeviceName, whose device then reports another DeviceName
  await page.goto(app.url + href + "/edit");
  await page.locator("input[name=name]").fill("Tasmota");
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Tasmota" })).toBeVisible();
  await page.goto(app.url + href);
  await page.getByRole("button", { name: "Refresh identity" }).click();
  await expect(page.getByText("Identity refreshed.")).toBeVisible();
  // the simulator reports its own DeviceName; the header must show the card's value, not "Tasmota"
  const reported = (await page.locator("#identity dt", { hasText: "Device name" }).locator("xpath=following-sibling::dd").innerText()).trim();
  expect(reported).not.toBe("Tasmota");
  const NEW = reported;
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(NEW);
  await expect(page.locator(".crumbs .crumb-here")).toHaveText(NEW);
  await expect(page.locator("#ph-title")).toHaveText(NEW); // the phone top bar (hidden on desktop)
  await expect(page).toHaveTitle(NEW + " - TasmoFleet");
  await expect(page.locator(".devhd")).toHaveCount(1);

  await page.goto(app.url + "/devices");
  await expect(page.getByRole("link", { name: NEW, exact: true })).toBeVisible();
});
