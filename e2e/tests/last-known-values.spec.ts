import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// #599: the Settings tab shows the values it read last, with their age, the moment it opens, and
// fetches fresh ones in the background. A device that does not answer leaves the old values on the page.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv(["-overview-shapes"]);
  app = await startApp({
    env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32", TASMOFLEET_ADMIN_GROUPS: "adm" },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

async function deviceHref(page: Page, name: string): Promise<string> {
  await page.goto(app.url + "/devices");
  return (await page.getByRole("link", { name, exact: true }).first().getAttribute("href"))!;
}

// Why: the point of #599. The second visit must not look empty while the device answers: the values
// of the first read are there at once with their age, the line says it is refreshing (busy), and the
// fresh values replace it when they arrive.
test("a second visit shows the last values at once and refreshes them in the background", { tag: "@core" }, async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  const href = await deviceHref(page, "ov-plain-plug");
  await page.goto(app.url + href + "/settings");
  const hostname = page.locator("input[name=Hostname]");
  await expect(hostname).toBeVisible({ timeout: 60_000 }); // the first visit has nothing to show but the loading state
  const value = await hostname.inputValue();
  expect(value).not.toBe("");

  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  let refreshes = 0;
  await page.route("**/settings/form", async (route) => {
    refreshes++;
    await gate; // a slow device
    await route.continue();
  });
  await page.goto(app.url + href + "/settings");
  const age = page.locator("p.agebar");
  await expect(hostname).toHaveValue(value); // there at once, while the device has not answered
  await expect(age).toHaveAttribute("aria-busy", "true");
  await expect(age).toContainText("Refreshing from the device");
  await expect(age).toContainText(/Read (just now|\d+s ago|\d+m ago)/);
  release();
  await expect(age).not.toHaveAttribute("aria-busy", "true");
  await expect(age).toContainText("Read just now");
  await expect(hostname).toHaveValue(value);
  expect(refreshes).toBe(1);
});

// Why: a refresh must not overwrite what the person types in the meantime; the page says so instead.
test("a refresh that arrives after the person started typing leaves their input alone", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  const href = await deviceHref(page, "ov-plain-plug");
  await page.goto(app.url + href + "/settings");
  await expect(page.locator("input[name=Hostname]")).toBeVisible({ timeout: 60_000 });
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  await page.route("**/settings/form", async (route) => {
    await gate;
    await route.continue();
  });
  await page.goto(app.url + href + "/settings");
  const hostname = page.locator("input[name=Hostname]");
  await hostname.fill("typed-by-hand");
  release();
  await expect(page.locator("p.agebar")).toContainText("unsaved changes");
  await expect(page.locator("p.agebar")).not.toHaveAttribute("aria-busy", "true");
  await expect(hostname).toHaveValue("typed-by-hand");
});

// Why: a device that stops answering must not blank the tab. The last values stay with their age and
// the reason the refresh failed.
test("a device that does not answer leaves the last values on the page", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  const href = await deviceHref(page, "ov-offline");
  await page.goto(app.url + href + "/settings");
  const hostname = page.locator("input[name=Hostname]");
  await expect(hostname).toBeVisible({ timeout: 60_000 });
  const value = await hostname.inputValue();
  const r = await fetch(env.vars.DEVENV_CONTROL_URL + "/device/ov-offline/off", { method: "POST" });
  expect(r.status).toBe(204);
  await page.goto(app.url + href + "/settings");
  const age = page.locator("p.agebar");
  await expect(age).toContainText("Refresh failed.", { timeout: 90_000 });
  await expect(age).toHaveAttribute("role", "alert");
  await expect(hostname).toHaveValue(value);
});
