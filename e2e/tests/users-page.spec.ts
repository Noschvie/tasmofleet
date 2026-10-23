import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv } from "../support/stack";

// Users page (v0.8.0, design v2 P9): the list of every principal is open to an admin
// without a step-up; changes need the IdP re-authentication (OIDC admin), asked for
// when an action is chosen; SSO users get "End all sessions".
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
  const r = await fetch(`${env.vars.DEVENV_CONTROL_URL}/login-as/${who}`, {
    method: "POST",
  });
  expect(r.status).toBe(204);
}

test("OIDC admin sees everyone, steps up, manages a local account and ends an SSO user's sessions", async ({
  page,
  browser,
}) => {
  // olivia (operator) signs in first, in a browser of her own
  await loginAs("operator");
  const ctx = await browser.newContext();
  const op = await ctx.newPage();
  await op.goto(app.url + "/");
  await expect(op.locator("summary.um-btn").getByText("olivia")).toBeVisible();
  expect((await op.goto(app.url + "/users"))?.status()).toBe(403); // operators never see the page

  await loginAs("admin");
  await page.goto(app.url + "/users");
  await expect(page.getByRole("heading", { name: "Users" })).toBeVisible();
  // the list is open without any confirmation; the changes are locked
  const olivia = page.locator(".urow", { hasText: "olivia" });
  await expect(olivia).toContainText("SSO");
  await expect(olivia).toContainText(/operator/i);
  await expect(page.locator('form[action$="/users/sessions"]')).toHaveCount(0);

  // an action asks for the IdP re-authentication first
  await olivia.getByLabel("User actions").click();
  await olivia.getByRole("menuitem", { name: "End all sessions" }).click();
  const stepUp = page.locator("dialog#users-stepup");
  await expect(stepUp).toBeVisible();
  await loginAs("admin"); // the IdP's answer to the second authentication
  await stepUp.getByRole("button", { name: /Re-authenticate with/ }).click();
  await expect(page).toHaveURL(/\/users$/);

  // create a local operator on its own page
  await page.getByRole("link", { name: "Add local user" }).click();
  await expect(page.getByRole("heading", { name: "Add local user" })).toBeVisible();
  await page.getByLabel("Name", { exact: true }).fill("Lena Berg");
  await page.getByLabel("Username").fill("lena");
  await page.getByText("Set an initial password").click();
  await page.locator('input[name="password"]').fill("local-operator-pass-1");
  await page.locator('input[name="confirm"]').fill("local-operator-pass-1");
  await page.getByRole("button", { name: "Add user" }).click();
  const lena = page.locator(".urow", { hasText: "lena" });
  await expect(lena).toContainText("Local");
  await expect(lena).toContainText("Must change password");
  // Why (v0.10.1): the opened menu must stay usable: its items are on top, not hidden by the row.
  await lena.getByLabel("User actions").click();
  const reset = lena.getByRole("menuitem", { name: "Reset password" });
  expect(
    await reset.evaluate((e) => {
      const b = e.getBoundingClientRect(),
        at = document.elementFromPoint(b.left + b.width / 2, b.top + b.height / 2);
      return !!at && (e === at || e.contains(at));
    }),
    "menu item reachable",
  ).toBe(true);
  // a reset generates a one-time password, shown once
  await reset.click();
  const dlg = page.locator("dialog[open]");
  await expect(dlg).toContainText("Reset the password of Lena Berg?");
  await dlg.getByRole("button", { name: "Reset password" }).click();
  await expect(page.getByRole("heading", { name: "Password reset for lena" })).toBeVisible();
  const otp = ((await page.locator("#otp-value").textContent()) ?? "").trim();
  expect(otp.length, "a generated password is shown").toBeGreaterThanOrEqual(12);

  // Why (N4): the one-time password signs lena in once and makes her choose her own at once
  const lenaCtx = await browser.newContext();
  const lp = await lenaCtx.newPage();
  await lp.goto(app.url + "/login");
  await lp.getByLabel("Username").fill("lena");
  await lp.getByLabel("Password", { exact: true }).fill(otp);
  await lp.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(lp.getByRole("heading", { name: "Choose a new password" })).toBeVisible();

  // the admin goes on to lena's page and ends that one session there; the answer stays on her page
  await page.getByRole("link", { name: "Done" }).click();
  await expect(page).toHaveURL(/\/users\/lena$/);
  const lenaHead = page.getByRole("heading", { level: 1, name: /Lena Berg/ });
  await expect(lenaHead).toBeVisible();
  const end = page.locator(".ud-sessions").getByRole("button", { name: "End session" });
  await expect(end).toHaveCount(1);
  await end.click();
  await expect(page.getByText("Session ended.")).toBeVisible();
  await expect(lenaHead).toBeVisible();
  await lp.goto(app.url + "/account");
  await expect(lp).toHaveURL(/\/login/); // lena is signed out
  await lenaCtx.close();
  // from=detail: a change confirmed on the page answers on the page, not on the list
  await page.locator(".ud-acts").getByRole("link", { name: "Make admin" }).click();
  await page.locator("dialog[open]").getByRole("button", { name: "Make admin" }).click();
  await expect(page.getByText("Role of lena changed.")).toBeVisible();
  await expect(lenaHead).toContainText("Admin");

  // End sessions signs olivia out and only her
  await page.goto(app.url + "/users");
  const oliviaAgain = page.locator(".urow", { hasText: "olivia" });
  await oliviaAgain.getByLabel("User actions").click();
  await oliviaAgain.getByRole("menuitem", { name: "End all sessions" }).click();
  await page.locator("dialog[open]").getByRole("button", { name: "End sessions" }).click();
  await expect(page.getByText("Signed out 1 session")).toBeVisible();
  await op.goto(app.url + "/devices");
  await expect(op.getByRole("heading", { name: "Devices", exact: true })).toHaveCount(0); // sent back to sign in
  await page.goto(app.url + "/devices");
  await expect(page.getByRole("heading", { name: "Devices", exact: true })).toBeVisible(); // the admin is still in
  await ctx.close();
});
