import { mkdirSync } from "node:fs";
import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// The Settings page (issue #275): eight tabs in one form, a card grid, info popovers, one save bar.
// Header authentication: header principals cannot confirm, so only settings without a step-up are
// changed here (the step-up paths are unit tested).
let app: App;
test.beforeAll(async () => {
  app = await startApp({
    env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32", TASMOFLEET_ADMIN_GROUPS: "adm" },
  });
});
test.afterAll(async () => {
  await app?.stop();
});
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
const tabs = (page: import("@playwright/test").Page) => page.getByRole("navigation", { name: "Settings sections" });

// Why: a person who edits one tab and looks at another must not lose the edit, and must be told
// which tabs hold unsaved changes. One Save then writes every tab.
test("edits survive a tab change, the bar lists the tabs with changes, one Save writes them all", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=general");
  const bar = page.locator("[data-sys-bar]");
  await expect(bar).toContainText("No unsaved changes");

  await page.locator("[data-key='METRICS_DEVICE_LABELS'] .swtrack").click();
  await expect(bar).toContainText("Unsaved changes:");
  await tabs(page).getByRole("link", { name: "Backups" }).click();
  await expect(page).toHaveURL(/tab=backups/);
  await page.locator("input[name='v.BACKUP_KEEP']").fill("7");
  await expect(bar.locator(".sbar-chip")).toHaveText(["General", "Backups"]);
  await expect(tabs(page).getByRole("link", { name: "General" })).toHaveClass(/is-dirty/);

  await tabs(page).getByRole("link", { name: "General" }).click();
  await expect(page.locator("input[name='v.METRICS_DEVICE_LABELS']")).toBeChecked(); // still there

  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Saved.")).toBeVisible();
  await page.goto(app.url + "/settings/system?tab=backups");
  await expect(page.locator("input[name='v.BACKUP_KEEP']")).toHaveValue("7");
  await page.goto(app.url + "/settings/system?tab=general");
  await expect(page.locator("input[name='v.METRICS_DEVICE_LABELS']")).toBeChecked();
  await expect(page.locator("[data-sys-bar]")).toContainText("No unsaved changes");
});

// Why: touch and keyboard users cannot hover. The info button opens a popover; Escape closes it;
// opening another one closes the first.
test("info popovers open on click, close on Escape and show one at a time", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=network");
  const first = page.getByRole("button", { name: "About Device networks" });
  const second = page.getByRole("button", { name: "About Telemetry period" });
  await first.click();
  await expect(page.locator("#info-DEVICE_NETWORKS")).toBeVisible();
  await expect(first).toHaveAttribute("aria-expanded", "true");
  await second.click();
  await expect(page.locator("#info-TELEPERIOD")).toBeVisible();
  await expect(page.locator("#info-DEVICE_NETWORKS")).toBeHidden();
  await page.keyboard.press("Escape");
  await expect(page.locator("#info-TELEPERIOD")).toBeHidden();
  await expect(second).toHaveAttribute("aria-expanded", "false");
});

// Why: scan interval and networks do nothing while the scan is off; they are dimmed (and not
// focusable) until the switch is on.
test("dependent settings follow their switch", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=scan");
  const dep = page.locator("[data-depends='v.DISCOVERY_SCAN']");
  await expect(dep).toHaveClass(/is-off/);
  await page.locator("[data-key='DISCOVERY_SCAN'] .swtrack").click();
  await expect(dep).not.toHaveClass(/is-off/);
  await dep.locator("input[name='v.DISCOVERY_SCAN_INTERVAL']").fill("2h");

});

// Why: the backup interval showed "0s" for manual only. The switch is a view of the same
// setting: off stores 0, on keeps the interval.
test("backup schedule switch dims the interval and stores 0 when off", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=backups");
  const sw = page.locator("input[name='sw.BACKUP_INTERVAL']");
  await expect(sw).not.toBeChecked(); // default: manual only
  await page.locator("[data-key='BACKUP_SCHEDULE'] .swtrack").click();
  await expect(page.locator("input[name='v.BACKUP_INTERVAL']")).toHaveValue("24h");
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Saved.")).toBeVisible();
  await expect(sw).toBeChecked();
  await page.locator("[data-key='BACKUP_SCHEDULE'] .swtrack").click();
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Saved.")).toBeVisible();
  await expect(sw).not.toBeChecked();
});

// Why: on a phone eight tabs scroll sideways; a picker replaces the strip, the cards lose their
// frame, nothing overflows, and Save is in a bar that stays in view.
test("phone: section picker, no horizontal overflow, save bar in view", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto(app.url + "/settings/system?tab=signin");
  await expect(page.locator("[data-sys-picker]")).toBeVisible();
  await expect(tabs(page)).toBeHidden();
  await page.locator("[data-sys-picker] select").selectOption("mqtt");
  await expect(page.locator("#sec-mqtt")).toBeVisible();
  await expect(page.locator("#sec-signin")).toBeHidden();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  const save = page.getByRole("button", { name: "Save settings" });
  await expect(save).toBeInViewport();
});

// Local accounts (the default): the Sign-in tab shows the identity provider cards, which wait for
// single sign-on to be selected.
test.describe("with local accounts", () => {
  let local: App;
  test.beforeAll(async () => {
    local = await startApp({ env: { TASMOFLEET_ALLOW_INSECURE_HTTP: "true" } });
    local.cli(["user", "passwd", "--create", "--role", "admin", "settings-admin"], "e2e-fake-password-1\n");
  });
  test.afterAll(async () => {
    await local?.stop();
  });
  async function login(page: import("@playwright/test").Page) {
    await page.setExtraHTTPHeaders({});
    await page.goto(local.url + "/login");
    await page.getByLabel("Username").fill("settings-admin");
    await page.getByLabel("Password").fill("e2e-fake-password-1");
    await page.getByRole("button", { name: "Sign in" }).click();
    await expect(page.getByRole("heading", { name: "Fleet overview" })).toBeVisible();
  }

  // Why: the identity provider does nothing in local mode, so its cards are dimmed until single
  // sign-on is chosen; the choice itself counts as an unsaved change of the Sign-in tab.
  test("the identity provider cards wait for single sign-on", async ({ page }) => {
    await login(page);
    await page.goto(local.url + "/settings/system?tab=signin");
    await expect(page.locator("#card-idp")).toHaveClass(/is-off/);
    await expect(page.locator("#card-roles")).toHaveClass(/is-off/);
    await page.locator("[data-key='AUTH_MODE'] label.rcard", { hasText: "Single sign-on" }).click();
    await expect(page.locator("#card-idp")).not.toHaveClass(/is-off/);
    await expect(page.locator("[data-sys-bar] .sbar-chip")).toHaveText(["Sign-in"]);
  });

  // Screenshots of every tab for review: SETTINGS_SHOTS=<dir>.
  test("screenshots of every tab", async ({ page }) => {
    test.skip(!process.env.SETTINGS_SHOTS, "set SETTINGS_SHOTS=<dir> to write screenshots");
    test.setTimeout(240_000);
    const dir = process.env.SETTINGS_SHOTS!;
    mkdirSync(dir, { recursive: true });
    await login(page);
    for (const scheme of ["light", "dark"] as const) {
      await page.emulateMedia({ colorScheme: scheme });
      for (const [size, w, h] of [["desktop", 1280, 900], ["phone", 375, 812]] as const) {
        await page.setViewportSize({ width: w, height: h });
        for (const tab of ["general", "network", "mqtt", "scan", "backups", "firmware", "signin", "advanced"]) {
          await page.goto(local.url + "/settings/system?tab=" + tab);
          await page.screenshot({ path: `${dir}/settings-${tab}-${size}-${scheme}.png`, fullPage: true });
        }
      }
    }
  });
});
