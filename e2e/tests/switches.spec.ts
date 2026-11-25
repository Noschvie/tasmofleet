import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { contrast } from "../support/contrast";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// #1096: a switch is one of two things and a selected state has one look. Five switch sizes and two off looks used to
// be drawn (the list row's switch was 44 x 32, below the 44 px a thumb needs), and four rules filled a pressed, open
// or selected control or a bar with the text colour, so black (light) or white (dark) meant "selected" on some
// controls and "plain" on others. Here: every power switch is at least 44 x 44 and inside its row, a setting switch
// is 38 x 22, every switch has one off fill, the bars that carry a selection are surface bars (never inverted) and
// a kept backup is the accent tint with a text that reads on it.
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
let app: App;
let env: Devenv;

test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_POLL_INTERVAL: "5s",
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir({ "plug-4m": env.devices["plug-4m"], "lamp-1m": env.devices["lamp-1m"] })]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

/** token resolves a colour token to what the browser paints (rgb), in the page's scheme. */
async function token(page: Page, name: string): Promise<string> {
  return page.evaluate((n) => {
    const probe = document.createElement("span");
    probe.style.setProperty("color", `var(${n})`);
    document.body.append(probe);
    const c = getComputedStyle(probe).color;
    probe.remove();
    return c;
  }, name);
}

/** deviceLink is the visible link of the plug (the table's at 1280, the card's at 375). */
function deviceLink(page: Page) {
  return page.locator("a[href*='/devices/']").filter({ hasText: "plug-4m" }).filter({ visible: true }).first();
}

let backupsUrl = "";
/** backupsPage opens the Backups tab of the plug, after one real backup was taken (once for the whole file). */
async function backupsPage(page: Page) {
  if (!backupsUrl) {
    await page.goto(app.url + "/devices");
    await deviceLink(page).click();
    const id = page.url().match(/devices\/(\d+)/)![1];
    const csrf = await page.locator("input[name=csrf]").first().inputValue();
    const took = await page.request.post(`${app.url}/devices/${id}/backup`, { headers: admin, form: { csrf }, maxRedirects: 0 });
    expect(took.status(), "backup taken").toBe(303);
    backupsUrl = `${app.url}/devices/${id}/backups`;
  }
  await page.goto(backupsUrl);
  await expect(page.locator(".bk-row").first()).toBeVisible();
}

/** waitForRelays waits until the list has a power switch that can be used: the first poll has read the relays. */
async function waitForRelays(page: Page) {
  await expect(page.locator(".pwr:not([disabled])").filter({ visible: true }).first()).toBeVisible({ timeout: 60_000 });
}

for (const width of [1280, 375]) {
  test(`every power switch is at least 44 x 44 px and inside its row or card (${width}px)`, async ({ page }) => {
    test.setTimeout(120_000);
    await page.setViewportSize({ width, height: 900 });
    await page.setExtraHTTPHeaders(admin);
    await page.goto(app.url + "/devices");
    await waitForRelays(page);
    const boxes = await page.locator(".pwr").filter({ visible: true }).evaluateAll((els) =>
      els.map((el) => {
        const r = el.getBoundingClientRect();
        const rowEl = el.closest("tr, .dcard")!;
        const row = rowEl.getBoundingClientRect();
        return { w: r.width, h: r.height, above: r.top - row.top, below: row.bottom - r.bottom, tableRow: rowEl.tagName === "TR", rowH: row.height };
      }),
    );
    expect(boxes.length, "the list shows power switches").toBeGreaterThan(0);
    for (const b of boxes) {
      expect(b.w, "a power switch is 44 px wide at least").toBeGreaterThanOrEqual(44);
      expect(b.h, "a power switch is 44 px high at least").toBeGreaterThanOrEqual(44);
      // a target that reaches into the next row would take its neighbour's taps
      expect(b.above, "the switch starts inside its row").toBeGreaterThanOrEqual(-0.5);
      expect(b.below, "the switch ends inside its row").toBeGreaterThanOrEqual(-0.5);
      // the bigger target must not make the table taller: a row is 44 px (the row height of the design system)
      if (b.tableRow) expect(b.rowH, "a device row stays 44 px high").toBeLessThanOrEqual(44.5);
    }

    // the relay switches of the device page are the same switch
    await deviceLink(page).click();
    const relay = page.locator("#devlive .devrelay-sw").first();
    await expect(relay).toBeVisible();
    for (const r of await page.locator("#devlive .devrelay-sw").evaluateAll((els) => els.map((el) => el.getBoundingClientRect().toJSON()))) {
      expect(r.width, "a relay switch is 44 px wide at least").toBeGreaterThanOrEqual(44);
      expect(r.height, "a relay switch is 44 px high at least").toBeGreaterThanOrEqual(44);
    }
  });
}

for (const scheme of ["light", "dark"] as const) {
  test(`the sizes of the two switches and one off look (${scheme})`, async ({ page }) => {
    test.setTimeout(120_000);
    await page.emulateMedia({ colorScheme: scheme });
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.setExtraHTTPHeaders(admin);

    await page.goto(app.url + "/devices");
    await waitForRelays(page);
    const power = await page.locator(".pwr .sw").filter({ visible: true }).evaluateAll((els) =>
      els.map((el) => {
        const r = el.getBoundingClientRect();
        return { w: r.width, h: r.height, on: el.parentElement!.getAttribute("aria-checked") === "true", bg: getComputedStyle(el).backgroundColor };
      }),
    );
    for (const p of power) expect([p.w, p.h], "a power switch track is 34 x 20").toEqual([34, 20]);

    await page.goto(app.url + "/settings/system?tab=general");
    const settings = await page.locator(".uswitch .sw").filter({ visible: true }).evaluateAll((els) =>
      els.map((el) => {
        const r = el.getBoundingClientRect();
        return { w: r.width, h: r.height, on: (el.previousElementSibling as HTMLInputElement).checked, bg: getComputedStyle(el).backgroundColor };
      }),
    );
    expect(settings.length, "the notification settings are switches").toBeGreaterThan(0);
    for (const s of settings) expect([s.w, s.h], "a setting switch track is 38 x 22").toEqual([38, 22]);

    const off = [...power, ...settings].filter((s) => !s.on).map((s) => s.bg);
    const on = [...power, ...settings].filter((s) => s.on).map((s) => s.bg);
    expect(off.length, "switches that are off to compare").toBeGreaterThan(0);
    expect(new Set(off).size, `every switch that is off has one fill, got ${[...new Set(off)]}`).toBe(1);
    expect(off[0], "the off fill is the control border").toBe(await token(page, "--color-control-border"));
    expect(new Set(on).size, "every switch that is on is the accent").toBeLessThanOrEqual(1);
    if (on.length) expect(on[0]).toBe(await token(page, "--color-accent"));
  });
}

for (const scheme of ["light", "dark"] as const) {
  for (const width of [1280, 375]) {
    test(`the bulk bar and the backups selection bar are surface bars, not inverted (${width}px, ${scheme})`, async ({ page }) => {
      test.setTimeout(150_000);
      await page.emulateMedia({ colorScheme: scheme });
      await page.setViewportSize({ width, height: 900 });
      await page.setExtraHTTPHeaders(admin);
      const surface = await (async () => {
        await page.goto(app.url + "/devices");
        return token(page, "--color-surface");
      })();
      const text = await token(page, "--color-text");

      const bulk = page.locator(".bulkbar[data-bulkbar]");
      await expect(bulk).toBeHidden();
      if (width > 700) await page.locator("table[data-table=devices] tbody tr").first().locator("input[name=id]").check();
      else await page.locator("label.dcard-sel").first().click();
      await expect(bulk).toBeVisible();
      const look = await bulk.evaluate((el) => {
        const s = getComputedStyle(el);
        return { bg: s.backgroundColor, fg: s.color, shadow: s.boxShadow };
      });
      expect(look.bg, "the bulk bar is a surface bar").toBe(surface);
      expect(look.fg, "its text is the text colour").toBe(text);
      expect(look.shadow, "it floats on the bar shadow").not.toBe("none");

      // one backup of a device: the Backups tab lists it with a tick box and the selection bar
      await backupsPage(page);
      const sel = page.locator(".bk-selbar[data-bulkbar]");
      await expect(sel).toBeHidden();
      await page.locator(".bk-row input[type=checkbox]").first().check();
      await expect(sel).toBeVisible();
      const bar = await sel.evaluate((el) => {
        const s = getComputedStyle(el);
        return { bg: s.backgroundColor, fg: s.color, shadow: s.boxShadow };
      });
      expect(bar.bg, "the selection bar is a surface bar").toBe(surface);
      expect(bar.fg, "its text is the text colour").toBe(text);
      expect(bar.shadow, "it has the bar shadow").not.toBe("none");
      await expect(sel).toContainText("1 selected");
    });
  }
}

for (const scheme of ["light", "dark"] as const) {
  test(`a kept backup is the accent tint and reads (${scheme})`, async ({ page }) => {
    test.setTimeout(120_000);
    await page.emulateMedia({ colorScheme: scheme });
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.setExtraHTTPHeaders(admin);
    await backupsPage(page);
    const keep = page.locator(".bk-keep").first();
    if ((await keep.getAttribute("aria-pressed")) !== "true") {
      await keep.click();
      await expect(page.locator(".bk-keep[aria-pressed=true]").first()).toBeVisible();
    }
    const kept = page.locator(".bk-keep[aria-pressed=true]").first();
    const look = await kept.evaluate((el) => {
      const s = getComputedStyle(el);
      return { bg: s.backgroundColor, fg: s.color, border: s.borderTopColor };
    });
    expect(look.bg, "a kept backup is the accent tint").toBe(await token(page, "--color-accent-bg"));
    expect(look.fg, "its text is the accent text").toBe(await token(page, "--color-accent-fg"));
    expect(look.border, "its border is the accent").toBe(await token(page, "--color-accent"));
    expect(await contrast(page, look.fg, look.bg), "the accent text reads on the accent tint").toBeGreaterThanOrEqual(4.5);
  });
}
