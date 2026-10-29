import { test, expect } from "../support/fixtures";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// M2-2: settings editor and policy view against the simulator fleet.
// Limits: the simulator replays captured replies and does not keep written
// settings, and the e2e image has no decode-config, so the drift view is fed
// by the live check, not by a backup.
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
      TASMOFLEET_POLICY_RULES: JSON.stringify([
        { command: "SetOption0", want: "0", families: ["esp8266"], note: "save_state off" },
        { command: "SetOption0", want: "1", families: ["esp32", "esp32s3"], note: "ESP32 exception" },
        { command: "PowerOnState", want: "5", note: "never satisfied by the replayed capture" },
      ]),
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

test("settings form: operators are refused, admins see named SetOptions and apply a change", { tag: "@core" }, async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: "plug-4m-http", exact: true }).click();
  const id = page.url().match(/devices\/(\d+)/)![1];
  expect((await page.goto(`${app.url}/devices/${id}/settings`))?.status()).toBe(403);

  await page.setExtraHTTPHeaders(admin);
  await page.goto(`${app.url}/devices/${id}/settings`);
  await expect(page.getByText("SetOption0 (save_state)")).toBeVisible();
  await expect(page.locator("input[name=MqttPassword]")).toHaveValue("");

  await page.locator("input[name=TelePeriod]").fill("60");
  await expect(page.locator("input[name=confirm], input[name=save]")).toHaveCount(0); // no flash choices: one save, always
  await expect(page.getByText("Applied immediately and saved to the device once.")).toBeVisible();
  await page.getByRole("button", { name: "Save to device" }).click();
  await expect(page.getByRole("heading", { name: "Result" })).toBeVisible();
  await expect(page.getByRole("row").filter({ hasText: "TelePeriod 60" })).toContainText(/done/i);

  await page.goto(app.url + "/audit");
  await expect(page.locator("table.audittable").getByText("Device setting").first()).toBeVisible();
});

// QA v0.9.0: Apply without a change came back as a red error next to "5 settings changed" and
// a form that looked wiped. It is a neutral notice, and the form matches the device again.
test("settings form: Apply without a change is a neutral notice and the counter stays at zero", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: "plug-4m-http", exact: true }).click();
  const id = page.url().match(/devices\/(\d+)/)![1];
  await page.goto(`${app.url}/devices/${id}/settings`);
  await page.getByRole("button", { name: "Save to device" }).click();
  await expect(page.locator(".banner[role=status]")).toContainText("Nothing changed.");
  await expect(page.locator(".banner[role=alert]")).toHaveCount(0);
  await expect(page.locator("[data-changed-count]")).toHaveText("No changes yet");
  await expect(page.locator("input[name=TelePeriod]")).not.toHaveValue("");
});

test("policy view: the live check finds drift per rule, the ESP32 exception is applied", async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/policy");
  await expect(page.getByRole("heading", { name: "Policy", level: 1 })).toBeVisible();
  await expect(page.getByText("SetOption0 = 0")).toBeVisible();

  await page.getByRole("button", { name: "Run now" }).click();
  await expect(page.locator("#live-tile")).toContainText("last run", { timeout: 60_000 });
  // per rule: PowerOnState 5 is never satisfied by the replay, so the overview counts a deviation on its row,
  // and the ESP8266 rule (the replayed OFF satisfies it) counts none
  const rows = page.locator("section#rules tbody tr");
  await expect(rows.nth(2)).toContainText("deviate");
  await expect(rows.nth(0)).not.toContainText("deviate");
  const esp8266 = (await rows.nth(0).locator("td.p1 a").getAttribute("href"))!;
  // The ESP8266 rule's page lists the device with the live value (PowerOnState rules read the
  // side-effect-free Status; the replayed capture says OFF), and the replayed OFF satisfies the
  // rule: the device is not among those that deviate.
  await page.goto(app.url + esp8266);
  await expect(page.locator("#rule-devices tr").filter({ hasText: "plug-4m-http" })).toContainText("live ·");
  await page.goto(app.url + esp8266 + "?show=deviate");
  await expect(page.locator("#rule-devices tr").filter({ hasText: "plug-4m-http" })).toHaveCount(0);
});

// Why (N9 + N2): a device deleted with "keep the backups" leaves backups no device page lists.
// Settings -> Backups counts them in its totals and lists them with Download, Keep and Delete;
// deleting one comes back to that tab (it used to lead to /devices/0/backups). The delete form
// is posted directly (the delete page is another package's).
test("settings backups: totals, and a deleted device's kept backup is listed and can be deleted", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: "lamp-1m", exact: true }).click();
  const id = page.url().match(/devices\/(\d+)/)![1];
  const csrf = await page.locator("input[name=csrf]").first().inputValue();
  const took = await page.request.post(`${app.url}/devices/${id}/backup`, { headers: admin, form: { csrf }, maxRedirects: 0 });
  expect(took.status(), "backup taken").toBe(303);

  await page.goto(app.url + "/settings/system?tab=backups");
  const sum = (label: string) => page.locator(".sys-sum", { hasText: label }).locator(".sys-sum-value");
  await expect(sum("Backups")).toHaveText(/^[1-9]\d*$/);
  await expect(page.locator(".sys-store").getByRole("row", { name: /lamp-1m/ })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Backups of deleted devices" })).toHaveCount(0);

  const del = await page.request.post(`${app.url}/devices/${id}/delete`, { headers: admin, form: { csrf, backups: "keep", ack: "1" }, maxRedirects: 0 });
  expect(del.status(), "device deleted, backups kept").toBe(303);

  await page.goto(app.url + "/settings/system?tab=backups");
  await expect(sum("Backups"), "the kept backup still counts").toHaveText(/^[1-9]\d*$/);
  const orphan = page.locator(".sys-orphan", { hasText: "lamp-1m" });
  await expect(orphan).toHaveCount(1);
  await expect(orphan.getByRole("link", { name: "Download" })).toBeVisible();
  await expect(page.locator(".sys-store").getByRole("row", { name: /lamp-1m/ })).toHaveCount(0);

  await orphan.getByRole("link", { name: "Delete" }).click();
  const dlg = page.locator("dialog#dlg-bkdel");
  await expect(dlg).toBeVisible();
  await dlg.getByRole("button", { name: "Delete backup" }).click();
  await expect(page).toHaveURL(/\/settings\/system\?tab=backups&ok=deleted/);
  await expect(page.getByText("Backup deleted.")).toBeVisible();
  await expect(page.locator(".sys-orphan", { hasText: "lamp-1m" })).toHaveCount(0);
});
