import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// System settings page and the policy rule editor. The device networks and
// OIDC_CLIENT_ID are set by the environment (locked); everything else is stored in
// SQLite. Header authentication: header principals cannot confirm anything, so
// only settings without a step-up are changed here (the step-up paths are unit
// tested against the real handlers).
let app: App;
test.beforeAll(async () => {
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_OIDC_CLIENT_ID: "envclient",
    },
  });
});
test.afterAll(async () => {
  await app?.stop();
});

const operator = { "X-authentik-username": "olivia", "X-authentik-groups": "ops" };
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

test("a setting changed on the page applies live: the scheduled scan starts without a restart", async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  expect((await page.goto(app.url + "/settings/system"))?.status()).toBe(403);

  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/discovery/scan");
  await expect(page.getByText("No scan yet")).toBeVisible();

  await page.goto(app.url + "/settings/system");
  await page.getByRole("navigation", { name: "Settings sections" }).getByRole("link", { name: /Discovery/ }).click(); // one section at a time
  await page.locator("input[name='v.DISCOVERY_SCAN']").check();
  await page.locator("textarea[name='v.DISCOVERY_SCAN_CIDRS']").fill("127.0.0.1/32");
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Saved.")).toBeVisible();
  // the page comes back on the tab that was saved, so the new values are what the person sees
  await expect(page.getByRole("navigation", { name: "Settings sections" }).getByRole("link", { name: /Discovery/ })).toHaveAttribute("aria-current", "page");
  await expect(page.locator("input[name='v.DISCOVERY_SCAN']")).toBeChecked();

  // The schedule started a scan as "system" at once.
  await expect(async () => {
    await page.goto(app.url + "/discovery/scan");
    await expect(page.getByText(/^Last scan /)).toBeVisible(); // the scheduled scan ran
  }).toPass({ timeout: 20_000 });
  await page.goto(app.url + "/audit");
  await expect(page.locator("table.audittable").getByText("System settings").first()).toBeVisible();
});

test("a setting from the environment is locked, also against a forged request", { tag: "@core" }, async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=signin"); // the locked fields live on this tab
  await expect(page.locator("input[name='v.OIDC_CLIENT_ID']")).toHaveCount(0);
  await expect(page.getByText("set by environment").first()).toBeVisible();
  await expect(page.locator("input[name='v.LISTEN']")).toHaveCount(0);

  const csrf = await page.locator("input[name=csrf]").first().inputValue();
  const forged = await page.request.post(app.url + "/settings/system", {
    headers: admin,
    form: { csrf, "v.OIDC_CLIENT_ID": "9" },
  });
  expect(forged.status()).toBe(403);
  const bootstrap = await page.request.post(app.url + "/settings/system", {
    headers: admin,
    form: { csrf, "v.LISTEN": ":1" },
  });
  expect(bootstrap.status()).toBe(400);
  await page.reload();
  await expect(page.getByText("set by environment").first()).toBeVisible();
});

test("policy rule editor: create via the setting picker, live preview, edit, revert", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/policy");
  await expect(page.getByText("No rules yet")).toBeVisible();

  await page.getByRole("link", { name: "New rule" }).click();
  // The value control follows the setting: a number input for SaveData ...
  const command = page.locator("input[name='command']");
  const pick = async (name: string) => {
    await command.fill(name);
    await command.blur(); // the change event swaps the value control
  };
  await pick("SaveData");
  await expect(page.locator("input[name='want'][type='number']")).toHaveAttribute("max", "3600");
  // ... a dropdown for an enum ...
  await pick("PowerOnState");
  await expect(page.locator("select[name='want'] option")).toHaveCount(7);
  // ... and an on/off choice for a SetOption.
  await pick("SetOption0");
  await expect(page.locator("input[type='radio'][name='want']")).toHaveCount(2);
  await pick("SaveData");
  await page.locator("input[name='want']").fill("0");
  await page.locator("input[name='name']").fill("Flash wear");
  await page.locator(".pol-scope > summary").click();
  await page.locator("label.pol-pickc", { hasText: "ESP32 (all variants)" }).click();

  // The preview updates while editing and stores nothing.
  await expect(page.locator("#rule-preview")).toContainText(/Would flag \d+ of/);
  await page.locator("input[name='want']").fill("99999");
  await expect(page.locator("#rule-preview .ufield-err")).toBeVisible();
  await page.locator("input[name='want']").fill("0");
  await expect(page.locator("#rule-preview")).toContainText(/Would flag \d+ of/);

  await page.getByRole("button", { name: "Save rule" }).click();
  await expect(page.getByText("Rule saved")).toBeVisible();
  const card = page.locator(".pol-card", { hasText: "Flash wear" });
  await expect(card).toContainText("ESP32 (all variants)");
  await expect(card).toContainText("SaveData = 0");

  // selecting the card opens its editor (the menu has no Edit)
  await card.locator(".pol-card-main").click();
  await expect(page.locator("input[name='name']")).toHaveValue("Flash wear");
  await page.locator("input[name='want']").fill("1");
  await page.getByRole("button", { name: "Save rule" }).click();
  await expect(page.locator(".pol-card", { hasText: "Flash wear" })).toContainText("SaveData = 1");

  // The history compares the version before with the current one; reverting asks first.
  await page.getByRole("button", { name: "Revert to this version…" }).click();
  await page.locator("dialog[open]").getByRole("button", { name: /^Revert to v/ }).click();
  await expect(page.getByText(/Reverted/)).toBeVisible();
  await expect(page.locator(".pol-card", { hasText: "Flash wear" })).toContainText("SaveData = 0");

  // operators read, but cannot edit
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/policy");
  await expect(page.getByText("Flash wear").first()).toBeVisible();
  await expect(page.getByRole("link", { name: "New rule" })).toHaveCount(0);
  await expect(page.getByLabel("Rule actions")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Revert to this version…" })).toHaveCount(0);
});

// Why (N17): the log level is a UI setting now; the choice must be stored and shown again,
// so the admin sees which level runs (it applies live, unit tested in app).
test("log level: the chosen level is saved and shown again", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=general");
  const debug = page.locator("input[name='v.LOG_LEVEL'][value='debug']");
  await expect(debug).not.toBeChecked();
  await page.locator("label", { has: debug }).click();
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Saved.")).toBeVisible();
  await page.goto(app.url + "/settings/system?tab=general");
  await expect(page.locator("input[name='v.LOG_LEVEL'][value='debug']")).toBeChecked();
  await expect(page.locator("input[name='v.LOG_LEVEL'][value='info']")).not.toBeChecked();
});

// Why (N19): the metrics switch must really close /metrics (404, not just a hidden link) and
// open it again without a restart.
test("metrics switch: off answers 404, on serves the metrics again", async ({ page, request }) => {
  expect((await request.get(app.url + "/metrics")).status(), "on by default").toBe(200);
  await page.setExtraHTTPHeaders(admin);
  const toggle = page.locator("label.utoggle", { hasText: "Expose Prometheus metrics" });
  await page.goto(app.url + "/settings/system?tab=metrics");
  await expect(toggle.locator("input")).toBeChecked();
  await toggle.click();
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Saved.")).toBeVisible();
  expect((await request.get(app.url + "/metrics")).status(), "switched off").toBe(404);
  await page.goto(app.url + "/settings/system?tab=metrics");
  await toggle.click();
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Saved.")).toBeVisible();
  expect((await request.get(app.url + "/metrics")).status(), "switched on again").toBe(200);
});

// Why (#151): the scrape token is set on the page and applies to the next request. The test
// client is a loopback peer, which needs no token until one is set; then it needs the right one.
test("metrics token: set on the page it applies at once, resetting it opens the local scrape again", async ({ page, request }) => {
  await page.setExtraHTTPHeaders(admin);
  expect((await request.get(app.url + "/metrics")).status(), "no token yet: local scrape").toBe(200);
  await page.goto(app.url + "/settings/system?tab=metrics");
  await page.locator("input[name='v.METRICS_TOKEN']").fill("e2e-fake-scrape-token");
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Saved.")).toBeVisible();
  expect((await request.get(app.url + "/metrics")).status(), "token required now").toBe(401);
  const ok = await request.get(app.url + "/metrics", { headers: { Authorization: "Bearer e2e-fake-scrape-token" } });
  expect(ok.status(), "right token").toBe(200);
  await expect(page.locator("body")).not.toContainText("e2e-fake-scrape-token"); // write-only
  await page.locator("input[name='clear.METRICS_TOKEN']").check();
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Saved.")).toBeVisible();
  expect((await request.get(app.url + "/metrics")).status(), "token removed").toBe(200);
});

// Why (N9): "Reset settings…" puts every stored setting back to its default, but only after the
// admin confirmed it is them (it changes the broker, the networks and the sign-in at once).
// Header principals cannot confirm, so a local admin does it here.
test("reset settings: one confirmation, then the step-up, then the defaults", async ({ page }) => {
  app.cli(["user", "passwd", "--create", "--role", "admin", "resetter"], "e2e-fake-password-1\n");
  await page.setExtraHTTPHeaders({});
  await page.goto(app.url + "/login");
  await page.getByLabel("Username").fill("resetter");
  await page.getByLabel("Password").fill("e2e-fake-password-1");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Fleet overview" })).toBeVisible();

  // a stored value to reset
  await page.goto(app.url + "/settings/system?tab=backups");
  await page.locator("input[name='v.BACKUP_KEEP']").fill("4");
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Saved.")).toBeVisible();

  await page.goto(app.url + "/settings/system?tab=advanced");
  await page.getByRole("link", { name: "Reset settings…" }).click();
  const confirm = page.locator("dialog#dlg-reset");
  await expect(confirm).toBeVisible();
  await expect(confirm).toContainText("Devices, backups and users are kept.");
  await confirm.getByRole("button", { name: "Reset settings" }).click();

  const stepup = page.locator("dialog#stepup");
  await expect(stepup).toBeVisible();
  await expect(stepup).toContainText("Resetting all settings");
  await stepup.getByLabel("Password").fill("e2e-fake-password-1");
  await stepup.getByRole("button", { name: "Confirm" }).click();
  await expect(page).toHaveURL(/tab=advanced/);

  await page.getByRole("link", { name: "Reset settings…" }).click();
  await page.locator("dialog#dlg-reset").getByRole("button", { name: "Reset settings" }).click();
  await expect(page.getByText("All settings are back at their defaults.")).toBeVisible();
  await page.goto(app.url + "/settings/system?tab=backups");
  await expect(page.locator("input[name='v.BACKUP_KEEP']")).toHaveValue("10");

  await page.goto(app.url + "/audit");
  await expect(page.getByText("Settings reset", { exact: true }).first()).toBeVisible();
});
