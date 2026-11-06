import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// #512: the rule pages must work without JavaScript. "Show all N" under the devices card of an
// unsaved draft used to link to /policy/rules/new?all=1, which dropped the draft and answered a
// plain-text 400 "unknown form field".
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv(["-fleet-size", "30"]); // more devices than the card lists before "Show all"
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
    },
  });
  expect(app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)])).toContain("created:");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

test("Show all on an unsaved rule draft works without JavaScript and keeps the draft", async ({ browser }) => {
  const ctx = await browser.newContext({ javaScriptEnabled: false, extraHTTPHeaders: admin });
  const page = await ctx.newPage();
  await page.goto(app.url + "/policy/rules/new?template=timezone");
  await page.getByLabel("Name", { exact: true }).fill("My unsaved draft"); // typed, not saved
  const rows = page.locator("#rule-devices tbody tr");
  const shown = await rows.count();
  const showAll = page.getByRole("button", { name: /^Show all \d+$/ });
  await expect(showAll).toBeVisible();
  await showAll.click();
  await expect(page.getByText("unknown form field")).toHaveCount(0);
  await expect(page.getByRole("heading", { level: 1 })).toContainText("My unsaved draft");
  await expect(page.getByLabel("Name", { exact: true })).toHaveValue("My unsaved draft");
  expect(await rows.count()).toBeGreaterThan(shown);
  await expect(page.getByRole("button", { name: /^Show all/ })).toHaveCount(0);
  await ctx.close();
});

// #660: with JavaScript, the form's own previews (the change that blurring the field fires, and the debounced
// input one) answer after the "Show all" click and put the short list back, so the first click after typing did
// nothing. Show all must win, and stay: a preview that follows keeps the whole list.
test("Show all on a rule draft right after typing shows all devices on the first click", async ({ browser }) => {
  const ctx = await browser.newContext({ extraHTTPHeaders: admin });
  const page = await ctx.newPage();
  await page.goto(app.url + "/policy/rules/new?template=timezone");
  const rows = page.locator("#rule-devices tbody tr");
  await expect(rows.first()).toBeVisible();
  const shown = await rows.count();
  const showAll = page.getByRole("button", { name: /^Show all \d+$/ });
  await expect(showAll).toBeVisible();
  await page.getByLabel("Name", { exact: true }).fill("Typed just before the click");
  await showAll.click(); // no pause: the previews of the typing are still on their way
  await expect(showAll).toHaveCount(0);
  await expect.poll(() => rows.count()).toBeGreaterThan(shown);
  const all = await rows.count();
  await page.waitForTimeout(1000); // past the debounced input preview and any late answer
  expect(await rows.count()).toBe(all);
  await ctx.close();
});
