import { test, expect } from "../support/fixtures";
import { pickForUpdate } from "../support/updates";
import { realisticPolicyRules } from "../support/seed";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";
import type { Page } from "@playwright/test";

// Destructive confirmations (#1093): the dialog opens on Cancel, never on the first focusable thing in it
// (the Copy button of a file's checksum), and its confirming button is danger-solid, whichever opener filled it.
// Everything asserted stands at page load or after a click: no live-swap timing.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
      TZ: "UTC",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_URL,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
      TASMOFLEET_POLICY_RULES: realisticPolicyRules,
    },
  });
  expect(app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)])).toContain("created: lamp-1m");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

/** expectDangerDialog: the open dialog's confirming button is danger-solid and the focus is on the dismissing button. */
async function expectDangerDialog(page: Page, dismiss = "Cancel") {
  const dlg = page.locator("dialog[open]");
  await expect(dlg).toBeVisible();
  await expect(dlg.locator('button[type="submit"]')).toHaveClass(/btn-danger-solid/);
  await expect(dlg.locator('button[type="submit"]')).not.toHaveClass(/btn-primary/);
  await expect(dlg.getByRole("button", { name: dismiss, exact: true })).toBeFocused();
  return dlg;
}

test.beforeEach(async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
});

// Why: Enter or Space on a dialog that has just opened must not run the destructive action.
test("restart: the dialog is danger-solid and opens on Cancel", async ({ page }) => {
  await expect(async () => {
    await page.goto(app.url + "/devices");
    await page.getByRole("link", { name: "lamp-1m", exact: true }).click();
    await expect(page.getByRole("link", { name: "Restart", exact: true })).toBeVisible({ timeout: 1000 });
  }).toPass({ timeout: 30_000 });
  await page.getByRole("link", { name: "Restart", exact: true }).click();
  const dlg = await expectDangerDialog(page);
  await expect(dlg.getByRole("heading", { name: "Restart lamp-1m?" })).toBeVisible();
  await expect(dlg.getByRole("button", { name: "Restart device", exact: true })).toBeVisible();
});

test("restart from the console is the same confirmation, not a one-click send", async ({ page }) => {
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: "lamp-1m", exact: true }).click();
  await page.getByRole("link", { name: "Console" }).click();
  await expect(page.locator(".console-quick button[value='Restart 1']")).toHaveCount(0);
  await page.locator(".console-quick").getByRole("link", { name: "Restart 1" }).click();
  await expectDangerDialog(page);
});

test("a policy rule's delete dialog is one shared dialog that opens on Cancel", async ({ page }) => {
  await page.goto(app.url + "/policy");
  await expect(page.locator("dialog#dlg-rule-delete")).toHaveCount(1);
  const rows = page.locator("section#rules tbody tr");
  await rows.nth(1).locator("summary").click();
  await rows.nth(1).getByRole("link", { name: "Delete", exact: true }).click();
  const dlg = await expectDangerDialog(page);
  const id = await dlg.locator("input[name=id]").inputValue();
  await dlg.getByRole("button", { name: "Cancel", exact: true }).click();
  // the next opener refills the dialog completely: another rule's id, never the previous one's
  await rows.nth(0).locator("summary").click();
  await rows.nth(0).getByRole("link", { name: "Delete", exact: true }).click();
  const again = await expectDangerDialog(page);
  expect(await again.locator("input[name=id]").inputValue()).not.toBe(id);
});

test("a cached file's delete dialog opens on Cancel, not on the Copy button", async ({ page }) => {
  await page.goto(app.url + "/firmware");
  await expect(async () => {
    await page.reload();
    await expect(page.locator("#release-16\\.1\\.0")).toBeVisible({ timeout: 1000 });
  }).toPass({ timeout: 30_000 });
  await page.getByRole("button", { name: "Prefetch for my fleet" }).click();
  await expect(page.getByText(/State: finished\b/)).toBeVisible({ timeout: 60_000 });
  await page.goto(app.url + "/firmware");
  await page.locator(".fw-crow summary").first().click();
  await page.locator(".fw-crow").first().getByRole("link", { name: "Delete", exact: true }).click();
  const dlg = await expectDangerDialog(page);
  await expect(dlg.getByRole("button", { name: /Copy the SHA-256/ })).not.toBeFocused();
});

test("cancelling a plan: the dialog names Keep schedule and opens on it", async ({ page }) => {
  await expect(async () => {
    await page.goto(app.url + "/updates/new");
    await expect(page.getByLabel("Select lamp-1m", { exact: true })).toBeEnabled({ timeout: 1000 });
  }).toPass({ timeout: 30_000 });
  await pickForUpdate(page, app.url, ["lamp-1m"]);
  await page.locator("label.upd-relopt", { hasText: "Tasmota 16.1.0" }).click();
  await page.getByRole("button", { name: "Review update" }).click();
  await page.getByLabel("Start at a chosen time").check();
  await page.getByText("Tomorrow night 03:00").click();
  await page.locator("button[form=start]").click();
  await expect(page).toHaveURL(/\/transactions\/\d+$/);
  await page.getByRole("link", { name: /^Cancel TX-/ }).click();
  const dlg = await expectDangerDialog(page, "Keep schedule");
  await expect(dlg.getByRole("button", { name: "Cancel schedule", exact: true })).toBeVisible();
});
