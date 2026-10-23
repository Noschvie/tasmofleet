import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// The design port (PORT.md "Port notes"): the theme follows the system with no explicit choice,
// an explicit choice wins, and no GET request of the app ever carries the CSRF token in its URL.
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
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_URL,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
const LIGHT_BG = "rgb(244, 245, 247)";
const DARK_BG = "rgb(13, 16, 19)";

async function bodyBg(page: import("@playwright/test").Page): Promise<string> {
  return page.evaluate(() => getComputedStyle(document.body).backgroundColor);
}

// Why: "Auto" must follow the system setting, also while the page is open, without any script.
test("theme: no explicit choice follows the system, live", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  for (const [scheme, want] of [["dark", DARK_BG], ["light", LIGHT_BG]] as const) {
    await page.emulateMedia({ colorScheme: scheme });
    await page.goto(app.url + "/devices");
    expect(await page.evaluate(() => document.documentElement.hasAttribute("data-theme-auto"))).toBe(true);
    expect(await bodyBg(page), `${scheme} tokens`).toBe(want);
  }
  await page.emulateMedia({ colorScheme: "dark" });
  expect(await bodyBg(page), "live switch to dark").toBe(DARK_BG);
  await page.emulateMedia({ colorScheme: "light" });
  expect(await bodyBg(page), "live switch back to light").toBe(LIGHT_BG);
});

// Why: an explicit Light or Dark choice (data-theme, auto marker dropped) beats the system setting.
test("theme: an explicit choice wins over the system", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  for (const [system, choice, want] of [["dark", "light", LIGHT_BG], ["light", "dark", DARK_BG]] as const) {
    await page.emulateMedia({ colorScheme: system });
    await page.evaluate((c) => {
      document.documentElement.removeAttribute("data-theme-auto");
      document.documentElement.setAttribute("data-theme", c);
    }, choice);
    expect(await bodyBg(page), `${choice} on a ${system} system`).toBe(want);
  }
});

// Why: a token in a URL ends up in history, logs and Referer. Walk the pages and open every
// menu; with JavaScript the Delete... links open dialogs, without it they go to the confirmation page.
for (const js of [true, false]) {
  test(`no GET request carries csrf= (${js ? "with" : "without"} JavaScript)`, async ({ browser }) => {
    const ctx = await browser.newContext({ javaScriptEnabled: js, extraHTTPHeaders: admin });
    const page = await ctx.newPage();
    const urls: string[] = [];
    page.on("request", (r) => {
      if (r.method() === "GET") urls.push(r.url());
    });
    const paths = ["/", "/devices", "/firmware", "/styleguide", "/policy", "/audit", "/about"];
    for (const p of paths) {
      await page.goto(app.url + p);
      for (const s of await page.locator("td.rowacts > details.rowmenu > summary:visible").all()) {
        await s.click(); // open, look, close: an open menu covers the next row's button
        await s.click();
      }
    }
    await page.goto(app.url + "/devices");
    const del = page.locator('td.rowacts .rowmenu-body a.danger[href$="/delete"]').first();
    await page.locator("td.rowacts > details.rowmenu > summary").first().click();
    await del.click(); // Delete… leads to its confirmation page with and without JavaScript (P2)
    await expect(page).toHaveURL(/\/devices\/\d+\/delete$/);
    await expect(page.getByRole("button", { name: "Delete device" })).toBeVisible();
    expect(urls.filter((u) => /csrf=/i.test(u)), "GET requests with csrf=").toEqual([]);
    expect(urls.length).toBeGreaterThan(paths.length);
    await ctx.close();
  });
}

// Why: a restart interrupts a device, so it asks exactly once, naming the consequence, and sends Restart 1.
test("restart: one confirmation naming the consequence, then the device restarts", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  await page.locator("td.rowacts > details.rowmenu > summary").first().click();
  await page.locator('.rowmenu-body a[data-dlg-kind="restart"]').first().click();
  const dlg = page.locator("dialog[open]");
  await expect(dlg).toContainText("relays switch off for a few seconds");
  await dlg.getByRole("button", { name: "Restart", exact: true }).click();
  await expect(page.getByText("Restart sent.")).toBeVisible();
});

// Why: deleting a device asks exactly once: Delete… in the row menu leads to the delete page
// (design v2 "devdelete", N2: the backups, the choice, one acknowledgement), which deletes and
// returns to the list.
test("delete: the confirmation page deletes after one confirmation", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  const rows = await page.locator("tbody tr").count();
  await page.locator("td.rowacts > details.rowmenu > summary").last().click();
  await page.locator('td.rowacts .rowmenu-body a.danger[href$="/delete"]').last().click(); // the row's (the phone card's twin is hidden)
  await expect(page).toHaveURL(/\/devices\/\d+\/delete$/);
  await page.getByRole("checkbox", { name: /I understand/ }).check();
  await page.getByRole("button", { name: "Delete device" }).click();
  await expect(page.getByText("Device deleted.")).toBeVisible();
  await expect(page.locator("tbody tr")).toHaveCount(rows - 1);
});

// Why (design v2, P3): the device header and its tabs are one component on every tab of a device:
// the same name and state, the tab you are on marked, the trail naming the device. Back up works on
// every tab without JavaScript and returns to the tab it was pressed on.
test("device pages: one header with the tabs on every tab; Back up returns to its tab", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: "lamp-1m", exact: true }).click();
  const deviceURL = page.url();
  for (const [sub, tab] of [["", "Overview"], ["/settings", "Settings"], ["/console", "Console"], ["/backups", "Backups"], ["/drift", "Drift"]] as const) {
    await page.goto(deviceURL + sub);
    await expect(page.locator("nav.utabs a[aria-current=page]"), sub).toHaveText(tab);
    await expect(page.locator("nav.crumbs [aria-current=page]"), sub).toHaveText("lamp-1m");
    await expect(page.locator("h1"), `${sub}: one device header on every tab`).toHaveText("lamp-1m");
    await expect(page.locator(".devhd-state"), sub).toBeVisible();
  }
  await page.goto(deviceURL + "/console");
  await page.getByRole("button", { name: "Back up", exact: true }).click();
  await expect(page).toHaveURL(/\/console\?ok=backup-taken$/, { timeout: 30_000 });
  await expect(page.getByText("Backup taken.").first()).toBeVisible();
});

// Why (QA v0.9.0, F4): "if a component is not on the style guide, it is not part of the design system".
// Every overlay is shown open and in the flow (user menu, drawer list, row menus, dialogs, step-up), so a
// reviewer and make styleguide-docs see them without clicking; the page scripts leave them open. The
// toast buttons raise real toasts, and the tabs stay one scrolling row on a phone.
for (const [name, size] of [["desktop", { width: 1280, height: 900 }], ["phone", { width: 375, height: 812 }]] as const) {
  test(`style guide: open menus, dialogs, tabs and toasts (${name})`, async ({ page }) => {
    await page.setExtraHTTPHeaders(admin);
    await page.setViewportSize(size);
    await page.goto(app.url + "/styleguide");
    const menus = page.locator("#sg-menus");
    await expect(menus.locator("details.usermenu[open] > .um-menu")).toBeVisible();
    await expect(menus.locator(".sg-drawer .dr")).toBeVisible();
    await expect(menus.locator("details.umenu[open] .menu-item.danger")).toBeVisible();
    await expect(menus.locator(".menu-why").first()).toBeVisible();
    const dialogs = page.locator("#sg-dialogs");
    await expect(dialogs.locator("dialog.udlg[open]")).toHaveCount(3);
    for (const d of await dialogs.locator("dialog.udlg[open]").all()) {
      const box = (await d.boundingBox())!;
      expect(box.x + box.width, "a dialog in the flow stays inside the page").toBeLessThanOrEqual(size.width);
    }
    // an open static menu is not closed by the page scripts: a click elsewhere keeps it, so does Escape
    await page.getByRole("heading", { name: "Styleguide", level: 1 }).click();
    await page.keyboard.press("Escape");
    await expect(menus.locator("details.umenu[open]").first()).toBeVisible();
    await expect(menus.locator("details.usermenu[open]")).toBeVisible();
    // tabs: one row; on a phone it scrolls sideways instead of wrapping
    const tabs = page.locator("#sg-tabs nav.utabs");
    await expect(tabs).toBeVisible();
    const tabBoxes = await tabs.locator(".utab").evaluateAll((els) => els.map((e) => e.getBoundingClientRect().top));
    expect(new Set(tabBoxes).size, "tabs on one line").toBe(1);
    // "Show failure" raises a toast that stays until dismissed
    await page.getByRole("button", { name: "Show failure" }).click();
    const toast = page.locator("#toasts .toast.tone-danger");
    await expect(toast).toBeVisible();
    await toast.getByRole("button", { name: "Dismiss" }).click();
    await expect(toast).toHaveCount(0);
  });
}

// Why: a real row menu (ui.RowMenu) behaves like the user menu: one open at a time, Escape and a
// click elsewhere close it. menu.js gives the <details> what it lacks, the static examples excepted.
test("row menu: Escape and a click elsewhere close it, one open at a time", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/styleguide");
  // the live user menu of the top bar and a row menu built like the gallery's, but live
  await page.evaluate(() => {
    const src = document.querySelector("#sg-menus details.umenu")!;
    const live = src.cloneNode(true) as HTMLDetailsElement;
    live.removeAttribute("data-static");
    live.open = false;
    live.id = "live-menu";
    document.querySelector("#sg-buttons")!.appendChild(live);
  });
  const live = page.locator("#live-menu");
  await live.locator("summary").click();
  await expect(live).toHaveAttribute("open", "");
  await page.keyboard.press("Escape");
  await expect(live).not.toHaveAttribute("open", "");
  await live.locator("summary").click();
  await page.locator("details.usermenu:not([data-static]) > summary").click();
  await expect(live, "opening the user menu closes the row menu").not.toHaveAttribute("open", "");
  await page.getByRole("heading", { name: "Styleguide", level: 1 }).click();
  await expect(page.locator("details.usermenu:not([data-static])")).not.toHaveAttribute("open", "");
});

// Why (QA v0.9.0, design v2): the update options are plain content at every width (the prototype
// shows them open), so neither a capture script nor a user has to find a fold to change them.
test("updates/new: every update option is visible without unfolding, desktop and phone", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  for (const [w, h] of [[1280, 900], [375, 812]]) {
    await page.setViewportSize({ width: w, height: h });
    await page.goto(app.url + "/updates/new");
    for (const name of ["backup", "policy", "power_after", "delivery", "variant"]) {
      await expect(page.locator(`.upd-opts [name=${name}]`), `${name} at ${w}`).toBeVisible();
    }
    await expect(page.locator(".upd-opts details")).toHaveCount(0);
  }
});
