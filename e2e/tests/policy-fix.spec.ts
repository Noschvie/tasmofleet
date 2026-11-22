import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// "Make compliant" from the Deviating devices tab, against the simulator fleet: a live check (the policy
// overview's Run now, whose banner has the one action Show) finds the deviation, the plan page shows the exact
// command, Run applies it as a transaction and the rule page then shows the device as compliant.
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
      TASMOFLEET_POLICY_RULES: JSON.stringify([{ command: "PowerOnState", want: "2", note: "e2e target" }]),
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

// Why: without JavaScript "Make compliant…" on the Deviating devices tab posts and the plan is a page: the exact
// commands, the reasons a device is left out, and Run. (The overview's banner has no Make compliant: one action.)
test("make compliant without JavaScript: the plan is a page", async ({ browser }) => {
  const ctx = await browser.newContext({ javaScriptEnabled: false, extraHTTPHeaders: admin });
  const page = await ctx.newPage();
  await page.goto(app.url + "/policy");
  await page.getByRole("button", { name: "Run now" }).click(); // a form: the values are read live
  await page.goto(app.url + "/policy/deviations");
  await page.getByRole("button", { name: /^Make compliant…/ }).click();
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(/^Make \d+ devices? compliant$/);
  await expect(page.getByText("saved to the device once")).toBeVisible();
  await expect(page.locator(".pol-fix-cmd .mono").first()).toHaveText(/^PowerOnState 2$/);
  await expect(page.getByRole("button", { name: /^Run \d+ changes?$/ })).toBeVisible();
  await ctx.close();
});

test("make compliant: operators see no action, admins review the plan, run it and end compliant", { tag: "@core" }, async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/policy");
  await page.getByRole("button", { name: "Run now" }).click();
  // the banner names the Required deviation; an operator may look, not fix
  const alert = page.locator(".banner", { hasText: "deviate from a required rule" });
  await expect(alert).toBeVisible({ timeout: 60_000 });
  await expect(page.getByRole("button", { name: /Make compliant/ })).toHaveCount(0);
  await page.goto(app.url + "/policy/deviations");
  await expect(page.getByRole("button", { name: /Make compliant/ })).toHaveCount(0);
  await page.goto(app.url + "/policy");
  await page.locator("td.p1 a").first().click(); // the only rule: its page
  const ruleURL = page.url();
  const opRow = page.locator("#rule-devices tr").filter({ hasText: "plug-4m-http" });
  await expect(opRow).toBeVisible();
  await expect(opRow).not.toContainText(/\b2\b/);

  // the admin's banner has the one action Show; the plan opens from the Deviating devices tab
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/policy");
  await expect(alert.getByRole("link", { name: "Show", exact: true })).toHaveAttribute("href", /\/policy\/deviations$/);
  await expect(page.getByRole("button", { name: /Make compliant/ })).toHaveCount(0);
  await alert.getByRole("link", { name: "Show", exact: true }).click();
  await expect(page).toHaveURL(/\/policy\/deviations$/);
  await page.getByRole("button", { name: /^Make compliant…/ }).click();
  await expect(page.locator(".pol-fix-card", { hasText: "plug-4m-http" })).toContainText("PowerOnState 2");
  await expect(page.locator(".pol-fix-foot input[type=checkbox]")).toHaveCount(0); // the plan plus Run, no confirmation tick
  await page.getByRole("button", { name: /^Run \d+ changes?$/ }).click();

  await expect(page).toHaveURL(/\/transactions\/\d+/);
  await expect(page.getByRole("heading", { name: "Make compliant", level: 1 })).toBeVisible();
  await expect(page.locator(".upd-dev", { hasText: "plug-4m-http" }).getByText("Verified", { exact: true }).first()).toBeVisible({ timeout: 60_000 });

  // the rule page lists the devices that still deviate: the fixed one is gone
  await page.goto(ruleURL + "?show=deviate");
  await expect(page.locator("#rule-devices")).toBeVisible();
  await expect(page.locator("#rule-devices tr").filter({ hasText: "plug-4m-http" })).toHaveCount(0);

  await page.goto(app.url + "/audit");
  await expect(page.locator("table.audittable").getByText("Make compliant (device)").first()).toBeVisible();
});
