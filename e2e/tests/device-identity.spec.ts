import { test, expect } from "../support/fixtures";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// #289: the Identity card shows the StatusFWR fields of Status 2 under Tasmota's names, apart from
// TasmoFleet's own reading, and the header names the hostname. #308: the Settings tab comes at once
// and fetches the device's settings into itself.
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
let app: App;
let env: Devenv;
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

// The first test refreshes the identity, which may rename the device, so the id is looked up once
// by its imported name and reused.
let deviceId = "";
async function openDevice(page: import("@playwright/test").Page): Promise<string> {
  await page.setExtraHTTPHeaders(adam);
  if (deviceId) {
    await page.goto(`${app.url}/devices/${deviceId}`);
    return deviceId;
  }
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: "plug-4m-http", exact: true }).click();
  deviceId = page.url().match(/devices\/(\d+)/)![1];
  return deviceId;
}

test("identity card: StatusFWR under Tasmota's names, TasmoFleet's reading apart", { tag: "@core" }, async ({ page }) => {
  await openDevice(page);
  await page.getByRole("button", { name: "Refresh identity" }).click();
  const card = page.locator("#identity");
  await expect(card.getByText("Identity refreshed.")).toBeVisible();
  for (const name of ["Version", "BuildDateTime", "Core", "SDK", "CpuFrequency", "Hardware", "CR"]) {
    await expect(card.locator("dt", { hasText: new RegExp(`^${name}\\b`) })).toBeVisible();
  }
  await expect(card.locator("button.info")).toHaveCount(7);
  await expect(card.locator("dt", { hasText: /^CR/ }).locator("button.info")).toHaveAttribute("title", /settings text area/i);
  const reading = card.getByRole("heading", { name: /TasmoFleet's reading/ });
  await expect(reading).toBeVisible();
  await expect(card.locator("dt", { hasText: /^Variant/ })).toBeVisible();
  // the header names the hostname the device reported
  await expect(page.locator(".devhd-meta")).toContainText(/\S+/);
});

test("settings tab: the tab comes at once with a loading state, then the form", { tag: "@core" }, async ({ page }) => {
  const id = await openDevice(page);
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  await page.route(`**/devices/${id}/settings/form`, async (route) => {
    await gate; // a slow device
    await route.continue();
  });
  await page.getByRole("link", { name: "Settings", exact: true }).click();
  await expect(page.getByRole("link", { name: "Settings", exact: true })).toHaveAttribute("aria-current", "page");
  await expect(page.getByRole("heading", { name: /Reading the settings/ })).toBeVisible();
  await expect(page.locator("input[name=TelePeriod]")).toHaveCount(0);
  release();
  await expect(page.locator("input[name=TelePeriod]")).toBeVisible();
  await expect(page.getByRole("heading", { name: /Reading the settings/ })).toHaveCount(0);
});
