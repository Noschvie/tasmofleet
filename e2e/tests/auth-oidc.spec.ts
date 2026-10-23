import { openMenu, gotoNav, signOut, openAccountMenu } from "../support/nav";
import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv } from "../support/stack";

// OIDC login against the mock IdP with Authentik-style group claims. No local account exists in
// the first tests, so /login goes straight to the IdP (OIDC is the only method).
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  const port = await freePort();
  const publicURL = `http://127.0.0.1:${port}`;
  env = await startDevenv(["-oidc-redirect", `${publicURL}/oidc/callback`]);
  app = await startApp({
    port,
    env: {
      TASMOFLEET_AUTH_MODE: "oidc",
      TASMOFLEET_PUBLIC_URL: publicURL,
      TASMOFLEET_ALLOW_INSECURE_HTTP: "true",
      TASMOFLEET_OIDC_ISSUER: env.vars.DEVENV_OIDC_ISSUER,
      TASMOFLEET_OIDC_CLIENT_ID: env.vars.DEVENV_OIDC_CLIENT_ID,
      TASMOFLEET_OIDC_CLIENT_SECRET: env.vars.DEVENV_OIDC_CLIENT_SECRET,
      TASMOFLEET_OPERATOR_GROUPS: "Tasmofleet Users",
      TASMOFLEET_ADMIN_GROUPS: "Tasmofleet Admins",
    },
  });
});
test.afterAll(async () => {
  await app.stop();
  await env.stop();
});

async function loginAs(who: string) {
  const r = await fetch(`${env.vars.DEVENV_CONTROL_URL}/login-as/${who}`, { method: "POST" });
  expect(r.status).toBe(204);
}

test("admin logs in through the IdP and out again", async ({ page }) => {
  await loginAs("admin");
  await page.goto(app.url + "/devices");
  await expect(page.getByRole("heading", { name: "Devices", exact: true })).toBeVisible();
  await expect(page.locator("summary.um-btn").getByText("adam")).toBeVisible();
  await openMenu(page, "System");
  await expect(page.locator("nav.sn-in").getByRole("link", { name: "Audit log" })).toBeVisible();
  await signOut(page);
  // after logout the next visit starts a new login; the mock IdP logs in the queued user
  await loginAs("operator");
  await page.goto(app.url + "/");
  await expect(page.locator("summary.um-btn").getByText("olivia")).toBeVisible();
  await expect(page.getByRole("link", { name: "Audit" })).toHaveCount(0);
});

test("a user in no mapped group gets no session", async ({ page }) => {
  await loginAs("nobody");
  const res = await page.goto(app.url + "/");
  expect(res?.status()).toBe(403);
  await expect(page.getByText("nina")).toHaveCount(0);
});

// One login page for both methods (decision 2026-10-19): the IdP button and the
// local form side by side, no automatic redirect, and the local account is the
// break-glass access while the IdP is configured.
test("the login page offers the IdP button and the local form together", async ({ page }) => {
  app.cli(["user", "passwd", "--create", "--role", "admin", "breakglass"], "e2e-fake-password-1\n");
  await page.goto(app.url + "/login");
  await expect(page.getByRole("link", { name: "Sign in with SSO" })).toBeVisible();
  await expect(page.getByLabel("Username")).toBeVisible();
  await expect(page.getByLabel("Password")).toBeVisible();
  await page.getByLabel("Username").fill("breakglass");
  await page.getByLabel("Password").fill("e2e-fake-password-1");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Fleet overview" })).toBeVisible();
});
