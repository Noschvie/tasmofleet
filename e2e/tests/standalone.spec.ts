import { signOut } from "../support/nav";
import { readFileSync, existsSync } from "node:fs";
import { join } from "node:path";
import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// Standalone mode: no proxy, no IdP, local accounts only. Run once over plain
// HTTP (ALLOW_INSECURE_HTTP, trusted LAN) and once with the zero-configuration
// self-signed certificate (no variable selects it). First run: the generated admin password is read from the file
// in the data dir and the first sign-in forces a change.
for (const tlsMode of ["plain", "self-signed"] as const) {
  test.describe(`standalone, ${tlsMode}`, () => {
    let app: App;
    test.use({ ignoreHTTPSErrors: true });
    test.beforeAll(async () => {
      app = await startApp({
        scheme: tlsMode === "plain" ? "http" : "https",
        env: {
          TASMOFLEET_AUTH_MODE: "local",
          ...(tlsMode === "plain" ? { TASMOFLEET_ALLOW_INSECURE_HTTP: "true" } : {}),
        },
      });
    });
    test.afterAll(async () => app.stop());

    const newPassword = "e2e-standalone-pass-1";

    test("first run: forced password change, then normal use, then restart", { tag: "@core" }, async ({ page }) => {
      const file = join(app.dataDir, "initial-admin-password");
      expect(existsSync(file)).toBeTruthy();
      const initial = readFileSync(file, "utf8").trim();

      await page.goto(app.url + "/");
      await page.getByLabel("Username").fill("admin");
      await page.getByLabel("Password").fill(initial);
      await page.getByRole("button", { name: "Sign in" }).click();

      // nothing else is reachable before the change
      await expect(page.getByRole("heading", { name: "Choose a new password" })).toBeVisible();
      await expect(page.getByRole("button", { name: "Sign out" })).toBeVisible(); // QA #74: the forced change has a way out
      await page.goto(app.url + "/devices");
      await expect(page.getByRole("heading", { name: "Choose a new password" })).toBeVisible();

      await page.getByLabel("One-time password").fill(initial);
      await page.getByLabel("New password", { exact: true }).fill(newPassword);
      await page.getByLabel("Repeat new password").fill(newPassword);
      await page.getByRole("button", { name: "Save and continue" }).click();
      await page.goto(app.url + "/devices");
      await expect(page.getByRole("heading", { name: "Devices", exact: true })).toBeVisible();
      expect(existsSync(file), "the generated password file is removed after the change").toBeFalsy();

      // cookie flags follow the actual transport
      const cookies = await page.context().cookies();
      const session = cookies.find((c) => c.httpOnly);
      expect(session).toBeTruthy();
      expect(session!.secure).toBe(tlsMode === "self-signed");

      // sessions survive a restart (persisted keys), old password no longer works
      await signOut(page);
      const dataDir = app.dataDir;
      const port = new URL(app.url).port;
      await app.stop(true);
      app = await startApp({
        dataDir, port: Number(port), scheme: tlsMode === "plain" ? "http" : "https",
        env: {
          TASMOFLEET_AUTH_MODE: "local",
          ...(tlsMode === "plain" ? { TASMOFLEET_ALLOW_INSECURE_HTTP: "true" } : {}),
        },
      });
      await page.goto(app.url + "/login");
      await page.getByLabel("Username").fill("admin");
      await page.getByLabel("Password").fill(initial);
      await page.getByRole("button", { name: "Sign in" }).click();
      await expect(page.getByRole("alert")).toContainText("Wrong username or password");
      await page.getByLabel("Username").fill("admin");
      await page.getByLabel("Password").fill(newPassword);
      await page.getByRole("button", { name: "Sign in" }).click();
      await expect(page.getByRole("heading", { name: "Fleet overview" })).toBeVisible();
    });

    test("admin creates an operator; operators cannot manage accounts", { tag: "@core" }, async ({ page, browser }) => {
      await page.goto(app.url + "/login");
      await page.getByLabel("Username").fill("admin");
      await page.getByLabel("Password").fill(newPassword);
      await page.getByRole("button", { name: "Sign in" }).click();
      await page.goto(app.url + "/users/new");
      // step-up: the current password again, asked for when the form is sent
      await page.getByRole("link", { name: "Add user" }).click();
      await page.locator("dialog#users-stepup").getByLabel("Password").fill(newPassword);
      await page.locator("dialog#users-stepup").getByRole("button", { name: "Confirm" }).click();
      await page.goto(app.url + "/users/new");
      await page.getByLabel("Username").fill("olivia");
      await page.getByText("Set an initial password").click();
      await page.locator('input[name="password"]').fill("operator-pass-123");
      await page.locator('input[name="confirm"]').fill("operator-pass-123");
      await page.getByRole("button", { name: "Add user" }).click();
      await expect(page.locator(".urow", { hasText: "olivia" })).toBeVisible();

      const ctx = await browser.newContext({ ignoreHTTPSErrors: true });
      const op = await ctx.newPage();
      await op.goto(app.url + "/login");
      await op.getByLabel("Username").fill("olivia");
      await op.getByLabel("Password").fill("operator-pass-123");
      await op.getByRole("button", { name: "Sign in" }).click();
      // an account created by an admin must pick its own password first
      await expect(op.getByRole("heading", { name: "Choose a new password" })).toBeVisible();
      await op.getByLabel("One-time password").fill("operator-pass-123");
      await op.getByLabel("New password", { exact: true }).fill("operator-own-pass-456");
      await op.getByLabel("Repeat new password").fill("operator-own-pass-456");
      await op.getByRole("button", { name: "Save and continue" }).click();
      const res = await op.goto(app.url + "/users");
      expect(res?.status()).toBe(403);
      await ctx.close();
    });
  });
}
