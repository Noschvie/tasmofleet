import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Bulk "Send command": an admin selects devices, types the command on the scope
// page, confirms the preview, and the result page shows the answer per device.
// Operators never see the button.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv();
  const fwPort = await freePort(); // the transaction pages come with the firmware server
  app = await startApp({
    env: {
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_POLL_INTERVAL: "5s",
    },
  });
  expect(app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)])).toContain("created: plug-4m-http");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const operator = { "X-authentik-username": "olivia", "X-authentik-groups": "ops" };
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

test("bulk send command: scope, preview, send as a transaction, result per device; operators have no button", async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/devices");
  await expect(page.getByRole("button", { name: /Send command/ })).toHaveCount(0);

  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  const row = page.getByRole("row").filter({ hasText: "plug-4m-http" });
  await expect(row.getByText("Online", { exact: true })).toBeVisible({ timeout: 30_000 });
  await row.getByRole("checkbox").check();
  await page.locator("[data-bulkbar]").getByRole("button", { name: /Send command/ }).click();
  await expect(page.getByRole("heading", { name: "Send a console command", level: 1 })).toBeVisible();
  await expect(page.locator(".bk-devs")).toContainText("plug-4m-http"); // the selection is the scope
  await page.getByRole("textbox", { name: "Command" }).fill("Status 2");
  await page.getByRole("button", { name: "Send to 1 device" }).click();
  await expect(page.getByRole("heading", { name: "Send Status 2 to 1 device?" })).toBeVisible(); // nothing sent yet
  await expect(page.locator(".banner.tone-danger")).toHaveCount(0); // a read command names no consequence
  await page.getByRole("button", { name: "Send to 1 device" }).click();

  await expect(page).toHaveURL(/\/devices\/bulk\/result\/\d+/);
  await expect(page.getByText("› Status 2")).toBeVisible();
  await expect(page.getByRole("heading", { name: "Command finished", level: 1 })).toBeVisible({ timeout: 60_000 });
  await expect(page.locator(".bk-rrow").filter({ hasText: "plug-4m-http" })).toContainText("Succeeded");
});

test("bulk send command: a destructive command names its consequence in the preview", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  const row = page.getByRole("row").filter({ hasText: "plug-4m-http" });
  await expect(row.getByText("Online", { exact: true })).toBeVisible({ timeout: 30_000 });
  await row.getByRole("checkbox").check();
  await page.locator("[data-bulkbar]").getByRole("button", { name: /Send command/ }).click();
  await page.getByRole("textbox", { name: "Command" }).fill("Reset 1");
  await page.getByRole("button", { name: "Send to 1 device" }).click();
  await expect(page.getByRole("alert").filter({ hasText: "erases the device configuration" })).toBeVisible();
  await page.getByRole("link", { name: "Cancel" }).click(); // nothing is sent
});
