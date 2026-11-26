import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { layoutGuards, guardReport, wrappedUserMeta } from "../support/layout";
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
  await olivia.getByLabel(/^Actions for /).click();
  await olivia.getByRole("link", { name: "End all sessions" }).click();
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
  await expect(lena).toContainText("Password change due");
  // Why (v0.10.1): the opened menu must stay usable: its items are on top, not hidden by the row.
  await lena.getByLabel(/^Actions for /).click();
  const reset = lena.getByRole("link", { name: "Reset password" });
  expect(
    await reset.evaluate((e) => {
      const b = e.getBoundingClientRect(),
        at = document.elementFromPoint(b.left + b.width / 2, b.top + b.height / 2);
      return !!at && (e === at || e.contains(at));
    }),
    "menu item reachable",
  ).toBe(true);
  // a reset generates a temporary password, shown once
  await reset.click();
  const dlg = page.locator("dialog[open]");
  await expect(dlg).toContainText("Reset the password of Lena Berg?");
  await dlg.getByRole("button", { name: "Reset password" }).click();
  await expect(page.getByRole("heading", { name: "Password reset for lena" })).toBeVisible();
  const otp = await page.locator("#otp-value").inputValue();
  expect(otp.length, "a generated password is shown").toBeGreaterThanOrEqual(12);
  // Why (#1111): the admin reads the value out or types it over, so it is the one-time code's mono
  // --fs-word type, where l, 1, O and 0 differ, not the base input's body sans
  const look = await page.locator("#otp-value").evaluate((e) => {
    const probe = document.createElement("span");
    probe.style.cssText = "font-family: var(--font-mono); font-size: var(--fs-word)";
    document.body.append(probe);
    const want = getComputedStyle(probe),
      got = getComputedStyle(e);
    const r = { font: got.fontFamily === want.fontFamily, size: got.fontSize === want.fontSize };
    probe.remove();
    return r;
  });
  expect(look, "the temporary password is in the one-time code's mono type").toEqual({ font: true, size: true });
  // Why (#1111): Copy hands over exactly that value: the value sits in a read-only field, whose text is empty
  await page.context().grantPermissions(["clipboard-read", "clipboard-write"], { origin: new URL(app.url).origin });
  await page.locator(".otp-value").getByRole("button", { name: "Copy" }).click();
  await expect(page.locator(".otp-value").getByRole("button", { name: "Copied" })).toBeVisible();
  expect(await page.evaluate(() => navigator.clipboard.readText()), "Copy puts the temporary password on the clipboard").toBe(otp);

  // Why (N4): the temporary password signs lena in once and makes her choose her own at once; with
  // single sign-on on, her local form is behind "Sign in with a local account" (#1111)
  const lenaCtx = await browser.newContext();
  const lp = await lenaCtx.newPage();
  const signInLena = async (p: Page) => {
    await p.goto(app.url + "/login");
    await p.getByText("Sign in with a local account").click();
    await p.getByLabel("Username").fill("lena");
    await p.getByLabel("Password", { exact: true }).fill(otp);
    await p.getByRole("button", { name: "Sign in", exact: true }).click();
    await expect(p.getByRole("heading", { name: "Choose a new password" })).toBeVisible();
  };
  await signInLena(lp);
  const lenaCtx2 = await browser.newContext();
  const lp2 = await lenaCtx2.newPage();
  await signInLena(lp2);

  // the admin goes on to lena's page
  await page.getByRole("link", { name: "Done" }).click();
  await expect(page).toHaveURL(/\/users\/lena$/);
  const lenaHead = page.getByRole("heading", { level: 1, name: /Lena Berg/ });
  await expect(lenaHead).toBeVisible();

  // Why (#1162): End all sessions reaches a local user too: both of lena's browsers are signed out on
  // their next request, and the same password signs her in again (Deactivate is the revocation).
  await page.locator(".ud-acts").getByRole("link", { name: "End all sessions" }).click();
  const endAll = page.locator("dialog[open]");
  await expect(endAll).toContainText("Lena Berg is signed out everywhere and can sign in again with the same password.");
  await endAll.getByRole("button", { name: "End sessions" }).click();
  await expect(page.getByText("Signed out 2 session(s). lena can sign in again with the same password.")).toBeVisible();
  for (const p of [lp, lp2]) {
    await p.goto(app.url + "/account");
    await expect(p).toHaveURL(/\/login/);
  }
  await lenaCtx2.close();
  await signInLena(lp);

  // the admin ends that one session on her page; the answer stays on her page
  await page.goto(app.url + "/users/lena");
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
  await oliviaAgain.getByLabel(/^Actions for /).click();
  await oliviaAgain.getByRole("link", { name: "End all sessions" }).click();
  await page.locator("dialog[open]").getByRole("button", { name: "End sessions" }).click();
  await expect(page.getByText("Signed out 1 session")).toBeVisible();
  await op.goto(app.url + "/devices");
  await expect(op.getByRole("heading", { name: "Devices", exact: true })).toHaveCount(0); // sent back to sign in
  await page.goto(app.url + "/devices");
  await expect(page.getByRole("heading", { name: "Devices", exact: true })).toBeVisible(); // the admin is still in
  await ctx.close();
});

// Why (#419-S): between 720 and 1199 px the stacks are two half columns, and a one-line user row
// squeezed into a half column left names one letter wide. The list must take the full row there.
for (const width of [768, 1024]) {
  test(`the user list takes the full content width at ${width}px`, async ({ page }) => {
    await loginAs("admin");
    await page.setViewportSize({ width, height: 900 });
    await page.goto(app.url + "/login/sso?next=%2Fusers"); // a local account may exist (the test above adds one): no automatic redirect to the IdP
    await expect(page.locator(".users-list")).toBeVisible();
    const w = await page.evaluate(() => ({
      list: document.querySelector(".users-list")!.getBoundingClientRect().width,
      stacks: document.querySelector(".cstacks")!.getBoundingClientRect().width,
    }));
    expect(w.list).toBe(w.stacks);
  });
}

// Why (#1080): a badge that sticks out of its cell sits on top of the next one. "Must change
// password" (about 140 px) covered "Local" in the Source column of a 90 px track, so an admin could
// not read how that user signs in; on a phone every cell took a line of its own, five lines per
// user, so the list showed two users per screen. Three badges (role, Deactivated, Password change
// due) are the worst case a row can hold. A phone row has two lines, three when it holds a second badge (#1182).
let badgeUsersSeeded = false;
function seedBadgeUsers() {
  if (badgeUsersSeeded) return; // both widths use the same four accounts
  badgeUsersSeeded = true;
  app.cli(["user", "add", "--role", "operator", "sam"], "e2e-fake-password-1\n");
  app.cli(["user", "add", "--role", "operator", "--must-change", "pia"], "e2e-fake-password-1\n");
  app.cli(["user", "add", "--role", "operator", "--must-change", "otto"], "e2e-fake-password-1\n");
  app.cli(["user", "disable", "otto"]);
  // a long name, so a name column squeezed by the badge breaks it inside a word (#1182)
  app.cli(["user", "add", "--role", "operator", "--must-change", "bartholomew-must-change"], "e2e-fake-password-1\n");
}
for (const [width, height] of [
  [1280, 900],
  [375, 800],
] as const) {
  test(`users list: every badge stays in its column, two lines per user on a phone, three with a second badge (${width}px)`, async ({ page }) => {
    seedBadgeUsers();
    await loginAs("admin");
    await page.setViewportSize({ width, height });
    await page.goto(app.url + "/login/sso?next=%2Fusers");
    const rows = page.locator(".users-list .urow");
    await expect(rows.filter({ hasText: "otto" }).locator(".urow-badges .bdg"), "role, Deactivated, password change").toHaveCount(3);

    const m = await page.evaluate(() => {
      const box = (e: Element) => {
        const r = e.getBoundingClientRect();
        return { l: r.left, t: r.top, r: r.right, b: r.bottom };
      };
      type B = ReturnType<typeof box>;
      const hit = (a: B, b: B) => a.l < b.r - 0.5 && b.l < a.r - 0.5 && a.t < b.b - 0.5 && b.t < a.b - 0.5;
      const out: { who: string; bad: string[]; sourceX: number; metaTops: number[]; whoBox: B; roleMid: number; badgeCount: number }[] = [];
      for (const row of document.querySelectorAll(".users-list .urow")) {
        const who = row.querySelector(".urow-who")!;
        const cell = row.querySelector(".urow-badges")!;
        const metas = [...row.querySelectorAll(".urow-meta")];
        const others = [who, ...metas, ...(row.querySelector(".rowmenu, a.btn") ? [row.querySelector(".rowmenu, a.btn")!] : [])];
        const bad: string[] = [];
        const cb = box(cell);
        for (const bdg of cell.querySelectorAll(".bdg")) {
          const bb = box(bdg);
          const label = bdg.textContent!.trim();
          if (bb.l < cb.l - 0.5 || bb.r > cb.r + 0.5) bad.push(`${label} sticks out of its cell by ${Math.round(Math.max(cb.l - bb.l, bb.r - cb.r))} px`);
          for (const o of others) if (hit(bb, box(o))) bad.push(`${label} covers ${o.className || o.tagName}`);
        }
        const first = cell.querySelector(".bdg")!;
        const fb = box(first);
        out.push({
          who: row.querySelector(".urow-name > span")!.textContent!.trim(),
          bad,
          sourceX: box(metas[0]).l,
          metaTops: metas.map((e) => box(e).t),
          whoBox: box(who),
          roleMid: (fb.t + fb.b) / 2,
          badgeCount: cell.querySelectorAll(".bdg").length,
        });
      }
      return out;
    });
    expect(m.length, "the four accounts and the admins are listed").toBeGreaterThanOrEqual(5);
    expect(m.flatMap((r) => r.bad.map((b) => `${r.who}: ${b}`)), "no badge leaves its cell or covers another cell").toEqual([]);
    if (width >= 1000) {
      expect(new Set(m.map((r) => Math.round(r.sourceX))).size, "the Source column starts at one x in every row").toBe(1);
    } else {
      const sam = m.find((r) => r.who === "sam")!;
      expect(sam.badgeCount).toBe(1);
      expect(sam.roleMid, "line 1: the role badge sits beside the name").toBeGreaterThan(sam.whoBox.t);
      expect(sam.roleMid, "line 1: the role badge sits beside the name").toBeLessThan(sam.whoBox.b);
      for (const r of m) {
        expect(r.metaTops[0], `${r.who}: line 2 holds the source`).toBeGreaterThanOrEqual(r.whoBox.b - 0.5);
        expect(r.metaTops[1], `${r.who}: the last sign-in shares the source's line`).toBeCloseTo(r.metaTops[0], 0);
      }
      expect(await wrappedUserMeta(page), "line 2 is one line: source and last sign-in each fit beside the other").toEqual([]);
    }
    const found = await layoutGuards(page, ["a", "b", "o", "w"]);
    expect(guardReport(`users list ${width}px`, found)).toBe("");
  });
}

// Why (#1182): on a phone the "Password change due" badge (about 150 px) took a track of line 1 beside
// the name and left it 38 px, so a name broke every few characters ("chan/ge") and the row grew from 83
// to 235 px. An admin who cannot read a name cannot tell which account a row is. A row with a second
// badge puts its badges on a line of their own; the name never gives way to a badge.
test("phone: a row with a second badge keeps the name as wide as a row without one (#1182)", async ({ page }) => {
  seedBadgeUsers();
  await loginAs("admin");
  await page.setViewportSize({ width: 375, height: 800 });
  await page.goto(app.url + "/login/sso?next=%2Fusers");
  const rows = page.locator(".users-list .urow");
  await expect(rows.filter({ hasText: "bartholomew-must-change" }).locator(".urow-badges .bdg"), "role and password change").toHaveCount(2);

  const m = await page.evaluate(() => {
    const row = (name: string) => [...document.querySelectorAll(".users-list .urow")].find((r) => r.querySelector(".urow-name > span")!.textContent!.trim() === name)!;
    const who = (name: string) => row(name).querySelector(".urow-who")!.getBoundingClientRect().width;
    // pieces a name may break between (a hyphen or a dot): every client rect of a piece sits on one line
    const split: string[] = [];
    const span = row("bartholomew-must-change").querySelector(".urow-name > span")!;
    const text = span.firstChild as Text;
    let at = 0;
    for (const piece of text.data.split(/(?<=[-.])/)) {
      const range = document.createRange();
      range.setStart(text, at);
      range.setEnd(text, at + piece.length);
      at += piece.length;
      const tops = new Set([...range.getClientRects()].map((r) => Math.round(r.top)));
      if (tops.size > 1) split.push(piece);
    }
    return { badge: who("bartholomew-must-change"), plain: who("sam"), split, badgeRowHeight: row("bartholomew-must-change").getBoundingClientRect().height, plainRowHeight: row("sam").getBoundingClientRect().height };
  });
  expect(m.badge, "the name of a row with a second badge is as wide as one without").toBeGreaterThanOrEqual(m.plain);
  expect(m.split, "no name breaks inside a word").toEqual([]);
  expect(m.plainRowHeight, "a row without a second badge stays at two lines").toBeLessThan(100);
  expect(m.badgeRowHeight, "a second badge adds one line, not a column of broken names").toBeLessThan(140);
  const found = await layoutGuards(page, ["a", "b", "o", "w"]);
  expect(guardReport("users list 375px, second badge", found)).toBe("");
});
