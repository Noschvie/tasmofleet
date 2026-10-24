import type { Page } from "@playwright/test";
import { deviceEntry } from "../support/devicelist";
import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Why: the top bar's menus must lie over everything the page draws under them. v0.11.3 drew the
// user menu below the device list's Columns button (a toolbar menu layer above the sticky header),
// so the button showed through the menu. The z-index scale in app.css (--z-*) orders the layers;
// this guard asks the browser what is on top at every point of the open menu, so any layer that
// slips above the top bar fails here, whichever element it is.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv(["-fleet-size", "6", "-releases", "2"]);
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

/** coveredPoints samples the panel's box on a grid and returns where something outside it is on top. */
async function coveredPoints(page: Page, panel: string): Promise<string[]> {
  return page.evaluate((sel) => {
    const p = document.querySelector(sel)!;
    const r = p.getBoundingClientRect();
    // 8 px in from the edge: the rounded corners show what lies behind them by design
    const x0 = Math.max(r.left, 0) + 8, x1 = Math.min(r.right, innerWidth) - 8;
    const y0 = Math.max(r.top, 0) + 8, y1 = Math.min(r.bottom, innerHeight) - 8;
    const bad: string[] = [];
    for (let y = y0; y <= y1; y += 6) {
      for (let x = x0; x <= x1; x += 6) {
        const el = document.elementFromPoint(x, y);
        if (!el || !p.contains(el)) {
          bad.push(`(${Math.round(x)},${Math.round(y)}) ${el ? el.tagName.toLowerCase() + "." + [...el.classList].join(".") : "nothing"}`);
        }
      }
    }
    return bad;
  }, panel);
}

// every menu the top bar opens, per width: the user menu on a wide screen, the navigation drawer
// (opened by either phone button) on a phone
const menus = [
  { width: 1280, name: "user menu", trigger: "details.usermenu > summary", panel: ".um-menu" },
  { width: 375, name: "navigation drawer", trigger: 'a[aria-label="Open navigation"]', panel: "dialog#nav-drawer" },
  { width: 375, name: "account and theme drawer", trigger: 'a[aria-label="Account and theme"]', panel: "dialog#nav-drawer" },
];

for (const m of menus) {
  test(`the open ${m.name} lies over the device list at ${m.width} px`, { tag: "@core" }, async ({ page }) => {
    await page.setExtraHTTPHeaders(admin);
    await page.emulateMedia({ colorScheme: "dark" });
    await page.setViewportSize({ width: m.width, height: 800 });
    await page.goto(app.url + "/devices");
    await expect(deviceEntry(page, /./).first()).toBeVisible();
    await page.locator(m.trigger).click();
    await expect(page.locator(m.panel)).toBeVisible();
    // the menu's box must actually reach over page content, or the guard proves nothing
    const reach = await page.evaluate((sel) => {
      const box = document.querySelector(sel)!.getBoundingClientRect();
      const main = document.querySelector("main")!.getBoundingClientRect();
      return box.bottom - main.top;
    }, m.panel);
    expect(reach).toBeGreaterThan(0);
    expect(await coveredPoints(page, m.panel)).toEqual([]);
  });
}
