import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";
import { openMenu, gotoNav, signOut, openAccountMenu } from "../support/nav";


// Break-glass local account (created with the CLI, never in the browser).
let app: App;
test.beforeAll(async () => {
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.2/32",
      TASMOFLEET_ADMIN_GROUPS: "Tasmofleet Admins",
    },
  });
  app.cli(["user", "passwd", "--create", "--role", "admin", "breakglass"], "e2e-fake-password-1\n");
  app.cli(["user", "passwd", "--create", "--role", "operator", "helper"], "e2e-fake-password-1\n");
});
test.afterAll(async () => app.stop());

// Why (D17, F4): the style guide shows sample data only, so every signed-in user may open it, an
// operator too, from the System menu (the navigation is the same for every role). It is still behind
// the sign-in: signed out, the address leads to the sign-in page, not to the guide.
test("the style guide needs a sign-in and is open to an operator from the System menu", async ({ page }) => {
  await page.goto(app.url + "/styleguide");
  await expect(page).toHaveURL(/\/login/);
  await expect(page.getByRole("heading", { name: "Styleguide" })).toHaveCount(0);
  await page.getByLabel("Username").fill("helper");
  await page.getByLabel("Password").fill("e2e-fake-password-1");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.goto(app.url + "/devices");
  await openMenu(page, "System");
  await expect(page, "an operator's System button leads to a page the role may open").toHaveURL(/\/styleguide$/);
  await expect(page.getByRole("heading", { name: "Styleguide", level: 1 })).toBeVisible();
  await expect(page.locator("nav.sn-in").getByRole("link", { name: "Users" }), "the menu is the same for every role").toBeVisible();
  const users = await page.request.get(app.url + "/users");
  expect(users.status(), "the page itself refuses what the role may not do").toBe(403);
  await signOut(page);
});

test("local sign-in works and wrong passwords are refused", { tag: "@core" }, async ({ page }) => {
  await page.goto(app.url + "/login");
  await page.getByLabel("Username").fill("breakglass");
  await page.getByLabel("Password").fill("wrong-password-xx");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("alert")).toContainText("Wrong username or password");
  // UX review #38: the name stays, the cursor is in the password field
  await expect(page.getByLabel("Username")).toHaveValue("breakglass");
  await expect(page.getByLabel("Password")).toBeFocused();
  await page.getByLabel("Username").fill("breakglass");
  await page.getByLabel("Password").fill("e2e-fake-password-1");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Fleet overview" })).toBeVisible();
  await openMenu(page, "System");
  await expect(page.locator("nav.sn-in").getByRole("link", { name: "Audit log" })).toBeVisible();
  await signOut(page);
  await page.goto(app.url + "/devices");
  await expect(page).not.toHaveURL(/\/devices$/);
});

// Why (#508): a tab left open past the idle expiry still shows the Sign out button. The server no
// longer knows the session, so its CSRF token is gone; the click used to end on a white "403 missing
// or invalid CSRF token" page. Signing out must always work: the person lands on the sign-in page.
test("Sign out after the session is gone signs out instead of answering 403", { tag: "@core" }, async ({ page }) => {
  await signInBreakglass(page);
  await page.context().clearCookies(); // the server-side effect of an expired session: no session cookie matches
  await openAccountMenu(page);
  await page.locator("details.usermenu:not([data-static])").getByRole("button", { name: "Sign out" }).click();
  await expect(page).toHaveURL(/\/$|\/login/);
  await expect(page.getByText("missing or invalid CSRF token")).toHaveCount(0);
  await expect(page.getByLabel("Username")).toBeVisible();
});

// Why (#508): signing out without a session must not let another site sign a person out. Its form post
// never carries the session cookie (SameSite=Lax), so the server sees no session, but the cookie-clearing
// answer would still end the valid session in this browser. The origin check refuses it.
test("a page on another site cannot sign you out", { tag: "@core" }, async ({ page, context }) => {
  await signInBreakglass(page);
  const evil = "http://localhost/evil.html"; // another site than 127.0.0.1, answered by the route below
  await page.route(evil, (route) => route.fulfill({ contentType: "text/html", body: `<form method="post" action="${app.url}/logout"><button>Win</button></form>` }));
  await page.goto(evil);
  await page.getByRole("button", { name: "Win" }).click();
  await expect(page.getByText("missing or invalid CSRF token")).toBeVisible();
  expect((await context.cookies(app.url)).map((c) => c.name)).toContain("tasmofleet");
  await page.goto(app.url + "/devices");
  await expect(page).toHaveURL(/\/devices$/);
});

// UX review #2: the account pages are part of the app: navigation, sign-out, the same
// layout. Only the sign-in and the forced first password change stay centred.
test("account pages carry the app shell", async ({ page }) => {
  await signInBreakglass(page);
  for (const path of ["/account", "/users"]) {
    await page.goto(app.url + path);
    await expect(page.locator("nav.tb-groups"), path).toBeVisible();
    await openAccountMenu(page);
    await expect(page.locator("details.usermenu").getByRole("button", { name: "Sign out" }), path).toBeVisible();
    await expect(page.locator("body.page-auth"), path).toHaveCount(0);
  }
  // close the account menu: open, it covers the first rows' "⋯" buttons
  await page.keyboard.press("Escape");
  await expect(page.locator("details.usermenu:not([data-static])")).not.toHaveAttribute("open", "");
  await stepUp(page, "helper", "Make admin");
  await expect(page.getByRole("heading", { name: "Users" })).toBeVisible();
  // one row per user; the actions sit in the row's "⋯" menu
  const helper = page.locator(".urow", { hasText: "helper" });
  await helper.getByLabel("User actions").click();
  await expect(helper.getByRole("menuitem", { name: "Reset password" })).toBeVisible();
  // your own row offers nothing that could lock you out, and says why (finding 88, design v2 P9)
  const own = page.locator(".urow", { hasText: "breakglass" });
  await own.getByLabel("User actions").click();
  await expect(own.getByRole("menuitem", { name: /Delete/ })).toHaveAttribute("aria-disabled", "true");
  await expect(own.getByText("You cannot delete your own account")).toBeVisible();
});

// Why: the server marks the last two rows of a list "up" because it cannot know the viewport. On
// a short list that put the only row's menu over the top bar although the page had 500 px free
// below it; the direction has to follow the space in the viewport when the menu opens.
test("the row menu of a short users list opens downward when there is room", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await signInBreakglass(page);
  await page.goto(app.url + "/users");
  await expect(page.getByRole("heading", { name: "Users" })).toBeVisible();
  const row = page.locator(".urow", { hasText: "helper" });
  const summary = row.locator("details.rowmenu > summary");
  await summary.click();
  const body = row.locator(".rowmenu-body");
  await expect(body).toBeVisible();
  const [s, b] = [await summary.boundingBox(), await body.boundingBox()];
  expect(b!.y, "menu body starts below its button").toBeGreaterThanOrEqual(s!.y + s!.height);
});

async function signInBreakglass(page: Page) {
  await page.goto(app.url + "/login");
  await page.getByLabel("Username").fill("breakglass");
  await page.getByLabel("Password").fill("e2e-fake-password-1");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Fleet overview" })).toBeVisible();
}

// stepUp confirms the admin's password in the "Confirm it's you" dialog, which an action of a
// locked users page opens (design v2, P9).
async function stepUp(page: Page, who: string, action: string) {
  const row = page.locator(".urow", { hasText: who });
  await row.getByLabel("User actions").click();
  await row.getByRole("menuitem", { name: action }).click();
  const dlg = page.locator("dialog#users-stepup");
  await expect(dlg).toBeVisible();
  await dlg.getByLabel("Password").fill("e2e-fake-password-1");
  await dlg.getByRole("button", { name: "Confirm" }).click();
}
