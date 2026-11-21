import { test, expect } from "../support/fixtures";
import { expectNoOverflow } from "../support/layout";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// #1085: refused, missing and switched-off pages are one card, centred, in the place the address belongs to,
// and a person without a role gets a card with Sign out and no navigation. The app has no firmware base URL,
// so /firmware and /updates/new are the switched-off pages.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
    },
  });
  expect(app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)])).toContain("created: lamp-1m");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const operator = { "X-authentik-username": "olivia", "X-authentik-groups": "ops" };
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
const nobody = { "X-authentik-username": "nina", "X-authentik-groups": "unrelated" };

/** deviceID is the id of lamp-1m, read from the device list. */
async function deviceID(page: import("@playwright/test").Page): Promise<string> {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: "lamp-1m", exact: true }).click();
  return page.url().match(/devices\/(\d+)/)![1];
}

test("the error card is centred at 1280 and every refusal fits a phone", async ({ page }) => {
  const id = await deviceID(page);
  await page.setViewportSize({ width: 1280, height: 800 });
  for (const path of ["/no/such/page", "/audit", "/firmware"]) {
    await page.goto(app.url + path);
    const box = await page.locator(".errpage").boundingBox();
    const card = await page.locator(".errpage-in").boundingBox();
    expect(box && card, path).toBeTruthy();
    expect(card!.width, path).toBeLessThanOrEqual(560);
    // the margins left and right of the card are the same, within 2 px: it is centred, not hugging the left edge
    expect(Math.abs(card!.x - box!.x - (box!.x + box!.width - card!.x - card!.width)), path).toBeLessThanOrEqual(2);
  }
  await page.setViewportSize({ width: 375, height: 812 });
  for (const path of ["/no/such/page", "/audit", "/firmware", `/devices/${id}/settings`, "/devices/new"]) {
    await page.goto(app.url + path);
    await expect(page.locator(".errpage-in")).toBeVisible();
    await expectNoOverflow(page, path);
  }
});

test("a refused device page stays inside the device's header with its tabs", async ({ page }) => {
  const id = await deviceID(page);
  await page.setViewportSize({ width: 1280, height: 800 });
  const res = await page.goto(`${app.url}/devices/${id}/settings`);
  expect(res?.status()).toBe(403);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("lamp-1m");
  await expect(page.getByRole("heading", { level: 2, name: "Only admins can do this" })).toBeVisible();
  await expect(page.locator("nav[aria-label='Main'] a[aria-current='true']")).toHaveText("Devices");
  // the header's own tabs, scoped to the device's tab bar ("Backups" is also an entry of the Devices sub-navigation)
  const tabs = page.getByRole("navigation", { name: "Device pages" });
  await expect(tabs.getByRole("link", { name: "Overview", exact: true })).toBeVisible();
  await expect(tabs.getByRole("link", { name: "Backups", exact: true })).toBeVisible();
  await expect(page).toHaveTitle("Only admins can do this - TasmoFleet");
  await page.locator(".errpage").getByRole("link", { name: "Back to lamp-1m" }).click();
  await expect(page).toHaveURL(`${app.url}/devices/${id}`);
});

test("an operator keeps the group of an admin page and its way back", async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.setViewportSize({ width: 1280, height: 800 });
  for (const [path, group, back] of [
    ["/devices/new", "Devices", "Back to Devices"],
    ["/settings/system", "System", "Back to System"],
    ["/audit", "System", "Back to System"],
  ]) {
    const res = await page.goto(app.url + path);
    expect(res?.status(), path).toBe(403);
    await expect(page.locator("nav[aria-label='Main'] a[aria-current='true']"), path).toHaveText(group);
    await expect(page.getByRole("heading", { level: 1 }), path).toHaveText("Only admins can do this");
    await expect(page.locator(".errpage").getByRole("link", { name: back }), path).toBeVisible();
  }
});

test("a switched-off page tells an operator who can turn it on and an admin what to set", async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/firmware");
  await expect(page.locator(".errpage")).toContainText("This feature is switched off. An admin can turn it on.");
  await expect(page.locator(".errpage")).not.toContainText("FIRMWARE_BASE_URL");
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/firmware");
  await expect(page.locator(".errpage-path")).toHaveText("FIRMWARE_BASE_URL");
});

test("a person without a role gets a card with Sign out and no navigation", async ({ page }) => {
  await page.setExtraHTTPHeaders(nobody);
  const res = await page.goto(app.url + "/");
  expect(res?.status()).toBe(403);
  await expect(page.getByRole("heading", { level: 1, name: "Access denied" })).toBeVisible();
  await expect(page.getByText("You can sign in, but your account has no access to TasmoFleet yet. Ask an admin to give you a role.")).toBeVisible();
  await expect(page.locator("nav[aria-label='Main']")).toHaveCount(0);
  await expect(page.getByRole("link", { name: "Dashboard" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Sign out" })).toBeVisible();
  for (const width of [1280, 375]) {
    await page.setViewportSize({ width, height: 800 });
    await expectNoOverflow(page, `no-role card at ${width}`);
  }
});
