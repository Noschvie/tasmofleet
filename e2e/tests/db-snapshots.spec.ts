import { readFileSync } from "node:fs";
import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// Settings, Backups: the TasmoFleet database card and the snapshot schedule (issue #301).
// A local admin, because the download needs the password confirmation that header principals
// cannot give.
let app: App;
test.beforeAll(async () => {
  app = await startApp({ env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.2/32", TASMOFLEET_ADMIN_GROUPS: "adm" } });
  app.cli(["user", "passwd", "--create", "--role", "admin", "snapper"], "e2e-fake-password-1\n");
});
test.afterAll(async () => {
  await app?.stop();
});

async function signIn(page: import("@playwright/test").Page) {
  await page.goto(app.url + "/login");
  await page.getByLabel("Username").fill("snapper");
  await page.getByLabel("Password").fill("e2e-fake-password-1");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Fleet overview" })).toBeVisible();
}

// Why: the file holds every secret of the installation, so it is only handed out after the
// password, it is a real SQLite file, and secret.key comes with it only when asked for.
test("download: password first, then a SQLite file; the key only inside a .tar when ticked", { tag: "@core" }, async ({ page }) => {
  await signIn(page);
  await page.goto(app.url + "/settings/system?tab=backups");
  const card = page.locator("#card-database");
  await expect(card).toContainText("The file holds secrets. Keep it like a password.");
  await expect(card.locator("input[name='include_key']")).not.toBeChecked();

  await card.getByRole("button", { name: "Download snapshot" }).click();
  const stepup = page.locator("dialog#stepup");
  await expect(stepup).toBeVisible();
  await expect(stepup).toContainText("Downloading a snapshot of the database");
  await stepup.getByLabel("Password").fill("e2e-fake-password-1");
  await stepup.getByRole("button", { name: "Confirm" }).click();
  await expect(page).toHaveURL(/tab=backups/);

  const [plain] = await Promise.all([page.waitForEvent("download"), page.locator("#card-database").getByRole("button", { name: "Download snapshot" }).click()]);
  expect(plain.suggestedFilename()).toMatch(/^tasmofleet-\d{8}-\d{4}\.db$/);
  expect(readFileSync((await plain.path())!).subarray(0, 15).toString()).toBe("SQLite format 3");

  await page.locator("#card-database input[name='include_key']").check();
  const [withKey] = await Promise.all([page.waitForEvent("download"), page.locator("#card-database").getByRole("button", { name: "Download snapshot" }).click()]);
  expect(withKey.suggestedFilename()).toMatch(/^tasmofleet-\d{8}-\d{4}\.tar$/);
  const tar = readFileSync((await withKey.path())!);
  expect(tar.includes("secret.key")).toBe(true);
  expect(tar.includes("tasmofleet.db")).toBe(true);

  await page.goto(app.url + "/audit");
  await expect(page.locator("table.audittable").getByText("Database snapshot download").first()).toBeVisible();
});

// Why: the schedule is off until switched on; switching it on takes a snapshot at once (like the
// other schedules), it is listed with Download and Delete, and the card says when it ran.
test("schedule: switch on, a snapshot appears, it can be downloaded and deleted", async ({ page }) => {
  await signIn(page);
  await page.goto(app.url + "/settings/system?tab=backups");
  await expect(page.locator("#card-database")).toContainText("No snapshots yet.");
  await expect(page.locator("input[name='sw.SNAPSHOT_INTERVAL']")).not.toBeChecked();

  await page.locator("[data-key='SNAPSHOT_SCHEDULE'] .swtrack").click();
  await expect(page.locator("input[name='v.SNAPSHOT_INTERVAL']")).toHaveValue("24h");
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Saved.")).toBeVisible();

  await expect(async () => {
    await page.goto(app.url + "/settings/system?tab=backups");
    await expect(page.locator("#card-database .sys-orphan")).toHaveCount(1);
  }).toPass({ timeout: 20_000 });
  await expect(page.locator("#card-snapshots")).toContainText("Last snapshot");
  await expect(page.locator("#card-database .sys-orphan")).toContainText("Latest");

  // A stored snapshot is as secret as a fresh one: this session has not confirmed yet.
  await page.locator("#card-database .sys-orphan").getByRole("link", { name: "Download" }).click();
  const stepup = page.locator("dialog#stepup");
  await expect(stepup).toBeVisible();
  await stepup.getByLabel("Password").fill("e2e-fake-password-1");
  await stepup.getByRole("button", { name: "Confirm" }).click();
  await expect(page).toHaveURL(/tab=backups/);
  const [dl] = await Promise.all([page.waitForEvent("download"), page.locator("#card-database .sys-orphan").getByRole("link", { name: "Download" }).click()]);
  expect(dl.suggestedFilename()).toMatch(/^tasmofleet-\d{8}-\d{4}\.db$/);

  await page.locator("#card-database .sys-orphan").getByRole("link", { name: "Delete" }).click();
  const dlg = page.locator("dialog#dlg-bkdel");
  await expect(dlg).toBeVisible();
  await dlg.getByRole("button", { name: "Delete snapshot" }).click();
  await expect(page.getByText("Snapshot deleted.")).toBeVisible();
  await expect(page.locator("#card-database .sys-orphan")).toHaveCount(0);
});
