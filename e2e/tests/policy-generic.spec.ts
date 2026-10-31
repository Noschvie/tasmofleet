import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// A policy rule for any Tasmota command (#36): the editor takes a free command, refuses what must
// never be sent (Status 0, secrets, commands that act or need a console confirmation) with the
// reason, and a saved rule is read live from the simulator fleet, previewed, and fixed by
// "Make compliant" like a curated setting. LedState is on no list: only the generic path knows it.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
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

test("a free command rule: refusals with the reason, then live read, preview and Make compliant", { tag: "@core" }, async ({ page }, testInfo) => {
  const shots = process.env.UI_SHOTS_DIR;
  // UI_SHOTS_DIR set: every screenshot at 375 and 1280 px, light and dark.
  const shot = async (name: string) => {
    if (!shots) return;
    const size = page.viewportSize() ?? { width: 1280, height: 720 };
    for (const [w, h] of [[375, 800], [1280, 900]]) {
      for (const scheme of ["light", "dark"] as const) {
        await page.setViewportSize({ width: w, height: h });
        await page.emulateMedia({ colorScheme: scheme });
        await page.screenshot({ path: `${shots}/${name}-${w}-${scheme}.png`, fullPage: true });
      }
    }
    await page.setViewportSize(size);
    await page.emulateMedia({ colorScheme: null });
  };
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/policy/rules/new");
  const any = page.locator("#any-command");

  // Refused, each with its reason at the field; nothing is stored.
  for (const [bad, why] of [
    ["Status0", "Status commands are never sent automatically."],
    ["MqttPassword", "Secret settings cannot be checked."],
    ["Template", "This command needs a confirmation in the console and cannot be a policy rule."],
  ] as const) {
    await any.fill(bad);
    await page.getByRole("button", { name: "Continue" }).first().click();
    await expect(page.locator("#err-any-command")).toHaveText(why);
    await expect(any).toHaveAttribute("aria-invalid", "true");
    if (bad === "Status0") await shot("error-status0");
  }

  // The free field: LedState is in no list. It opens the rule page as a draft.
  await any.fill("LedState");
  await page.getByRole("button", { name: "Continue" }).first().click();
  await expect(page).toHaveURL(/\/policy\/rules\/new\?command=LedState$/);
  // LedState is in no curated list, so the value field follows the catalogue: it answers a number (#269)
  await expect(page.locator("#want-control .cmd-kind")).toHaveText("number");
  await expect(page.locator("#want-control .ufield-help")).toContainText("Whole number.");
  await page.locator("input[name='want']").fill("3");
  await page.locator("input[name='name']").fill("LED state 3");
  await page.getByRole("radio", { name: /^Required/ }).check(); // only Required rules are fixed
  await shot("free-command");
  await page.getByRole("button", { name: "Save rule" }).click();
  // Save ends the editor on the overview: the flash names the rule and its row is marked
  await expect(page).toHaveURL(/\/policy\?ok=saved&rule=r[a-z0-9]+$/);
  await expect(page.getByText("Rule “LED state 3” saved")).toBeVisible();
  await expect(page.locator("tr.is-saved")).toContainText("LedState = 3");
  const ruleURL = app.url + "/policy/rules/" + new URL(page.url()).searchParams.get("rule");
  await page.goto(ruleURL);
  await expect(page.getByRole("heading", { level: 1 })).toContainText("LED state 3");
  await expect(page.locator(".pghead-sub")).toContainText("LedState = 3");

  // Check now reads LedState from the devices (the simulator reports 1); the rule page lists them.
  const devRow = page.locator("#rule-devices tr").filter({ hasText: "plug-4m-http" });
  await page.getByRole("button", { name: "Check now" }).click();
  await expect(page.getByText("Live check finished")).toBeVisible({ timeout: 60_000 });
  await expect(devRow).toContainText("live ·");
  await expect(page.locator("[data-tile='deviate'] .dev-tile-v")).not.toHaveText("0");
  await shot("preview-flags");

  // The same on the policy overview, for the Make compliant steps below: its banner names the deviation.
  await page.goto(app.url + "/policy");
  await page.getByRole("button", { name: "Run now" }).click();
  await expect(page.locator(".banner", { hasText: "deviate from a required rule" })).toBeVisible({ timeout: 60_000 });

  // Operators see the rule but have no way to change it or to fix.
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/policy");
  await expect(page.getByRole("button", { name: /Make compliant/ })).toHaveCount(0);
  expect((await page.request.post(app.url + "/policy/rules/save", { headers: operator, form: { csrf: "x", command: "Sleep", want: "1" } })).status()).toBe(403);

  // Make compliant sends exactly the reviewed command and ends compliant.
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/policy");
  await page.getByRole("button", { name: "Make compliant…" }).click();
  const dlg = page.locator("dialog#dlg-fix");
  await expect(dlg).toBeVisible();
  await expect(dlg).toContainText("LedState 3");
  await dlg.getByRole("button", { name: /^Run \d+ changes?$/ }).click();
  await expect(page).toHaveURL(/\/transactions\/\d+/);
  await expect(page.locator(".upd-dev").getByText("Applied", { exact: true }).first()).toBeVisible({ timeout: 60_000 });
  await page.goto(ruleURL + "?show=deviate");
  await expect(page.locator("#rule-devices")).toBeVisible();
  await expect(devRow).toHaveCount(0);
  testInfo.annotations.push({ type: "note", description: "LedState rule fixed through the generic path" });
});
