import { mkdirSync } from "node:fs";
import { join } from "node:path";
import type { Browser, BrowserContext, Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { expectNoOverflow } from "../support/layout";
import { openSavedLayoutList, ownerShown, ownerLayout, ownerWide, ownerWindow, postPref, savedLayoutUser } from "../support/saved-layout";
import { realisticDevenvArgs, realisticPolicyRules, seedRealisticData } from "../support/seed";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Table columns and page size are saved per user on the server: they must
// survive a reload and show up in a second browser of the same user, with the
// layout already in the HTML (no jump after load), and never leak to another
// user. Tooltips: chips explain themselves and a cut-off value shows its text.
// Set UX_SHOTS=<dir> to keep screenshots (desktop and phone, Columns menu open).
const shots = process.env.UX_SHOTS;
// UX_SEED=1: the data of a real installation (27 devices, transactions, audit entries); slow, for the screenshots
const seeded = !!process.env.UX_SEED;

let app: App;
let env: Devenv;
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
const eve = { "X-authentik-username": "eve", "X-authentik-groups": "adm" };

test.beforeAll(async ({ browser }) => {
  test.setTimeout(420_000);
  env = await startDevenv(seeded ? realisticDevenvArgs : []);
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_GITHUB_URL,
      TASMOFLEET_POLICY_RULES: realisticPolicyRules,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
  // The width of a column follows its content (chip, firmware and status texts fill in as a device
  // comes online), so a test that drags a width and reloads needs a fleet that has settled: wait
  // until every simulated device is online before the first test.
  const total = String(Object.keys(env.devices).length);
  await expect
    .poll(
      async () => {
        const html = await (await fetch(app.url + "/devices", { headers: adam })).text();
        return /Online<span class="dv-n">(\d+)<\/span>/.exec(html)?.[1];
      },
      { timeout: 120_000, intervals: [1000] },
    )
    .toBe(total);
  if (seeded) {
    const seedPage = await browser.newPage({ extraHTTPHeaders: adam });
    try {
      await seedRealisticData(seedPage, app, env);
    } finally {
      await seedPage.close();
    }
  }
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const headCell = (page: Page, table: string, name: string) => page.locator(`table[data-table="${table}"] thead th`, { hasText: new RegExp(`^\\s*${name}\\s*$`) });
// A device event reloads the list and replaces the table. Locator.boundingBox() resolves the
// header and measures it in two steps, and on a slow machine the swap lands between them, again
// and again: the call then never settles within the poll's timeout although a header is on the
// page the whole time (reproduced with CPU throttling). So find and measure in one page-side step,
// which cannot see a half-replaced table, and give a slow runner time.
const width = async (page: Page, table: string, name: string) => {
  let w = 0;
  await expect
    .poll(
      async () =>
        (w = await page.evaluate(
          ([t, n]) => {
            const th = [...document.querySelectorAll(`table[data-table="${t}"] thead th`)].find((h) => h.textContent?.trim() === n);
            return th ? th.getBoundingClientRect().width : 0;
          },
          [table, name],
        )),
      { timeout: 15_000 },
    )
    .toBeGreaterThan(0);
  return w;
};
// The same one-step read for a resize handle: Locator.boundingBox() returns null (and the test then
// dies on box.x) when a live reload replaces the table between finding the handle and measuring it.
const handleBox = async (page: Page, name: string) => {
  let box = { x: 0, y: 0, width: 0, height: 0 };
  await expect
    .poll(
      async () =>
        (box = await page.evaluate((n) => {
          const h = [...document.querySelectorAll('[role="separator"]')].find((e) => e.getAttribute("aria-label") === `Resize column ${n}`);
          const r = h ? h.getBoundingClientRect() : new DOMRect();
          return { x: r.x, y: r.y, width: r.width, height: r.height };
        }, name)).width,
      { timeout: 15_000 },
    )
    .toBeGreaterThan(0);
  return box;
};
const saved = (page: Page, key: string) => page.waitForResponse((r) => r.url().endsWith(`/prefs/${key}`) && r.request().method() === "POST" && r.status() === 204);

/** a second browser of the same user: own cookies, own storage */
async function otherBrowser(browser: Browser, headers: Record<string, string>): Promise<{ ctx: BrowserContext; page: Page }> {
  const ctx = await browser.newContext({ extraHTTPHeaders: headers, viewport: { width: 1280, height: 800 } });
  return { ctx, page: await ctx.newPage() };
}

async function resetAll(page: Page, path: string, key: string) {
  if (!page.url().startsWith(app.url)) await page.goto(app.url + path);
  expect(await postPref(page, key, {})).toBe(204);
  await page.goto(app.url + path);
}

test("dragging a column edge resizes it; the width survives a reload and a second browser, and is in the HTML", async ({ page, browser }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  await resetAll(page, "/devices", "table.devices");
  const before = await width(page, "devices", "Firmware");
  const box = await handleBox(page, "Firmware");
  const done = saved(page, "table.devices");
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  await page.mouse.down();
  await page.mouse.move(box.x + box.width / 2 + 20, box.y + box.height / 2, { steps: 5 });
  await page.mouse.move(box.x + box.width / 2 + 40, box.y + box.height / 2, { steps: 5 });
  await page.mouse.up();
  await done;
  const dragged = await width(page, "devices", "Firmware");
  expect(dragged).toBeGreaterThan(before + 25);

  await page.reload();
  // the page lays the saved widths out again when its fonts are in (tables.js enhance): wait for that
  await expect.poll(async () => Math.abs((await width(page, "devices", "Firmware")) - dragged)).toBeLessThan(3);

  // already in the server's HTML: no layout jump after load
  const html = await (await page.request.get(app.url + "/devices", { headers: adam })).text();
  expect(html).toMatch(/<table[^>]*class="[^"]*tfix[^"]*"[^>]*data-table="devices"|<table[^>]*data-table="devices"[^>]*class="[^"]*tfix/);
  expect(html).toMatch(/<col data-id="firmware" width="[0-9]+">/); // pixels: the device table's widths are content widths (#828)

  const second = await otherBrowser(browser, adam);
  await second.page.goto(app.url + "/devices");
  expect(Math.abs((await width(second.page, "devices", "Firmware")) - dragged)).toBeLessThan(3);
  await second.ctx.close();

  // another user keeps the defaults
  const other = await otherBrowser(browser, eve);
  await other.page.goto(app.url + "/devices");
  await expect(other.page.locator('table[data-table="devices"]')).not.toHaveClass(/tfix/);
  await other.ctx.close();
  await expectNoOverflow(page, "devices with a saved layout");
});

test("keyboard: arrow keys change the width, Home resets the column", async ({ page, browser }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/audit");
  await resetAll(page, "/audit", "table.audit");
  const handle = page.getByRole("separator", { name: "Resize column User" });
  const start = await width(page, "audit", "User");
  await handle.focus();
  const done = saved(page, "table.audit");
  for (let i = 0; i < 5; i++) await page.keyboard.press("ArrowRight");
  await done;
  const wider = await width(page, "audit", "User");
  expect(wider).toBeGreaterThan(start + 40);
  await expect(handle).toBeFocused();

  const second = await otherBrowser(browser, adam);
  await second.page.goto(app.url + "/audit");
  expect(Math.abs((await width(second.page, "audit", "User")) - wider)).toBeLessThan(3);
  await second.ctx.close();

  const reset = saved(page, "table.audit");
  await handle.focus();
  await page.keyboard.press("Home");
  await reset;
  const home = await width(page, "audit", "User");
  expect(home).toBeLessThan(wider - 20);
  // keyboard also shrinks, down to the minimum and not below
  const shrunk = saved(page, "table.audit");
  for (let i = 0; i < 40; i++) await page.keyboard.press("ArrowLeft");
  await shrunk;
  expect(await width(page, "audit", "User")).toBeGreaterThanOrEqual(55);
});

test("Columns menu hides and shows columns, and Reset to default restores the table", async ({ page, browser }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/audit");
  await resetAll(page, "/audit", "table.audit");
  const menu = page.locator('table[data-table="audit"]').locator("xpath=ancestor::*[contains(@class,'tablewrap')]").first().locator("details.colmenu");
  await menu.locator("summary").click();
  await expect(menu.getByRole("checkbox", { name: "When" })).toBeDisabled(); // the identifying column cannot be hidden
  if (shots) {
    mkdirSync(shots, { recursive: true });
    await page.screenshot({ path: join(shots, "audit-columns-menu-1280.png"), fullPage: false });
  }
  const done = saved(page, "table.audit");
  await menu.getByRole("checkbox", { name: "Target" }).uncheck();
  await done;
  await expect(headCell(page, "audit", "Target")).toBeHidden();
  await expectNoOverflow(page, "audit with a column hidden");

  await page.reload();
  await expect(headCell(page, "audit", "Target")).toBeHidden();
  const second = await otherBrowser(browser, adam);
  await second.page.goto(app.url + "/audit");
  await expect(headCell(second.page, "audit", "Target")).toBeHidden();
  await second.ctx.close();

  // show it again
  await page.locator("details.colmenu summary").first().click();
  const again = saved(page, "table.audit");
  await page.locator("details.colmenu").first().getByRole("checkbox", { name: "Target" }).check();
  await again;
  await expect(headCell(page, "audit", "Target")).toBeVisible();
  await page.locator("details.colmenu").first().getByRole("checkbox", { name: "Target" }).uncheck();

  // reset
  await page.locator("details.colmenu[open] [data-cols-reset], details.colmenu [data-cols-reset]").first().click();
  await page.waitForLoadState("load");
  await expect(headCell(page, "audit", "Target")).toBeVisible();
  await expect(page.locator('table[data-table="audit"]')).not.toHaveClass(/tfix/);
});

// #805. A saved layout of the device table holds pixel widths, and the table is at least as wide as
// their sum (data-basew); a wider frame stretches every column. A save stored the widths the browser
// drew, stretch and rounding included, and the server counted the 43 px checkbox column as 44: a
// layout saved while the table fitted came back 1 px wider than its frame (a scrollbar that scrolls
// 1 px), and showing a column stored the stretched widths of all the others, so at 1920 px the table
// scrolled after one more column. The layouts are the maintainer's from the live database: as 0.36.0
// stored it (no "b"), and as 0.36.0 stored it after showing three more columns at 1920 px.
/** The device table's frame, its saved size and its header widths, read in one step from a table the
 * script has fitted (a live reload swaps the table at any time; a fitted one has its minimum width). */
async function deviceFrame(page: Page) {
  await page.evaluate(() => document.fonts.ready);
  let m = { client: 0, scroll: 0, basew: 0, min: 0, widths: [] as number[] };
  await expect
    .poll(
      async () =>
        (m = await page.evaluate(() => {
          const t = document.querySelector<HTMLTableElement>('table[data-table="devices"]');
          const f = t?.parentElement;
          if (!t || !f || !f.classList.contains("is-js")) return { client: 0, scroll: 0, basew: 0, min: 0, widths: [] };
          return {
            client: f.clientWidth,
            scroll: f.scrollWidth,
            basew: Number(t.getAttribute("data-basew") ?? 0),
            min: parseFloat(t.style.minWidth) || 0,
            widths: [...t.tHead!.rows[0].cells].map((c) => c.getBoundingClientRect().width),
          };
        })).min,
      { timeout: 15_000 },
    )
    .toBeGreaterThan(0);
  return m;
}
/** Ticks or unticks a column in the Columns menu and returns the layout the page saved. */
async function showColumn(page: Page, id: string, on: boolean): Promise<{ w: Record<string, number> }> {
  const menu = page.locator("details.colmenu").first();
  if (!(await menu.evaluate((d) => (d as HTMLDetailsElement).open))) await menu.locator("summary").click();
  const done = saved(page, "table.devices");
  const box = menu.locator(`[data-colid="${id}"]`);
  await (on ? box.check() : box.uncheck());
  return (await done).request().postDataJSON();
}

// The layout is stored without "b" (as 0.36.0 stored it): this covers its conversion on the first save
// and that a repeated save keeps its size. The 1 px of the selection column is pinned by the test
// "the selection column counts as drawn" below, which sizes the frame from the layout's own widths.
test("a layout saved while the table fits its frame still fits it after a reload (#805)", async ({ page }) => {
  await page.setViewportSize({ width: 1300, height: 900 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  try {
    expect(await postPref(page, "table.devices", ownerLayout)).toBe(204);
    await page.reload();
    const start = await deviceFrame(page);
    expect(start.scroll, "the layout fits a 1300 px window to begin with").toBeLessThanOrEqual(start.client);
    // a save that leaves the visible columns as they were, made while the table fills its frame
    await showColumn(page, "otaurl", true);
    await showColumn(page, "otaurl", false);
    const inPage = await deviceFrame(page);
    await page.reload();
    const after = await deviceFrame(page);
    expect(after.scroll, "1 px too wide is a scrollbar: no tolerance").toBeLessThanOrEqual(after.client);
    expect(Math.abs(after.basew - inPage.basew), "the page and the server give the saved layout the same size").toBeLessThanOrEqual(0.5);
    // saving again without a change keeps the size: no save adds to it
    await showColumn(page, "otaurl", true);
    await showColumn(page, "otaurl", false);
    await page.reload();
    const again = await deviceFrame(page);
    expect(again.scroll).toBeLessThanOrEqual(again.client);
    expect(Math.abs(again.min - after.min), "a second save without a change").toBeLessThanOrEqual(0.5);
  } finally {
    await resetAll(page, "/devices", "table.devices");
  }
});

// #821: the cases above use windows where the layout fits; the owner's window (1290 px) has a layout that is
// wider than the frame, as a saved layout with the optional columns shown always is. There the table
// scrolls by design, a save must still change nothing in view, and the size a page draws is the size the
// server writes (#805: 1 px too much, then every show added the stretch).
test("a saved layout wider than the frame, at the owner's window: a hide and show of a column changes no width, in the page or after a reload (#805, #821)", async ({ page }) => {
  await openSavedLayoutList(page, app.url, { width: ownerWindow, doc: ownerWide });
  try {
    const start = await deviceFrame(page);
    expect(start.scroll, "the layout is wider than the frame").toBeGreaterThan(start.client);
    // the first save of a 0.36.0 layout converts it; the second must not change it
    await showColumn(page, "restart", false);
    await showColumn(page, "restart", true);
    const inPage = await deviceFrame(page);
    await page.reload();
    const after = await deviceFrame(page);
    expect(Math.abs(after.basew - inPage.basew), "the page and the server give the saved layout the same size").toBeLessThanOrEqual(0.5);
    expect(after.scroll, "the table scrolls by its own width and not by a pixel more").toBeLessThanOrEqual(Math.ceil(after.min));
    await showColumn(page, "restart", false);
    await showColumn(page, "restart", true);
    await page.reload();
    const again = await deviceFrame(page);
    expect(Math.abs(again.min - after.min), "a second hide and show without a change").toBeLessThanOrEqual(0.5);
    // the widths of the columns that were not touched
    const w = (f: typeof after) => f.widths.map((x) => Math.round(x));
    expect(w(again), "no column changed its width").toEqual(w(after));
  } finally {
    await resetAll(page, "/devices", "table.devices");
  }
});

test("showing a column adds its width and stores the other columns as they were saved, not as the frame stretches them (#805)", async ({ page }) => {
  await page.setViewportSize({ width: 1920, height: 1080 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  try {
    expect(await postPref(page, "table.devices", ownerLayout)).toBe(204);
    await page.reload();
    const start = await deviceFrame(page);
    expect(start.scroll).toBeLessThanOrEqual(start.client);
    // a save that changes nothing in view: what this version stores for the layout
    await showColumn(page, "otaurl", true);
    const before = (await showColumn(page, "otaurl", false)).w;
    const one = (await showColumn(page, "restart", true)).w;
    const ids = Object.keys(before);
    // widths are tenths of pixels, saved as whole pixels: one pixel is rounding, the stretch is a third
    const px = (a: number, b: number) => Math.round(Math.abs(a - b) * 10);
    for (const id of ids) expect(px(one[id], before[id]), `${id} keeps its saved width when Last restart is shown`).toBeLessThanOrEqual(1);
    const mean = ids.reduce((a, id) => a + before[id], 0) / ids.length;
    expect(px(one.restart, mean), "the new column gets the mean of the saved widths").toBeLessThanOrEqual(1);
    const shown = await deviceFrame(page);
    expect(shown.scroll, "one more column still fits a 1920 px window").toBeLessThanOrEqual(shown.client);
    // Two more columns add their own widths and nothing else. (Before #828 three more columns also fitted the
    // window, but only because the conversion squeezed Name and Status to give Address and Firmware their
    // content; now those stay as stored and the layout is about 100 px wider, so three means do not fit.)
    await showColumn(page, "otaurl", true);
    const two = (await showColumn(page, "grouptopic", true)).w;
    for (const id of ids) expect(px(two[id], before[id]), `${id} keeps its saved width when two more columns are shown`).toBeLessThanOrEqual(1);
    const three = await deviceFrame(page);
    expect(Math.abs(three.min - shown.min - (two.otaurl + two.grouptopic) * 10), "the table grows by the widths of the two columns").toBeLessThanOrEqual(1);
    await page.reload();
    const reloaded = await deviceFrame(page);
    expect(Math.abs(reloaded.min - three.min), "a reload draws the same table").toBeLessThanOrEqual(0.5);
    expect(Math.abs(reloaded.basew - three.basew), "the page and the server give the layout the same size").toBeLessThanOrEqual(0.5);
  } finally {
    await resetAll(page, "/devices", "table.devices");
  }
});

test("a saved layout is as wide as its columns: the selection column counts as drawn (#805)", async ({ page }) => {
  await page.setViewportSize({ width: 1300, height: 900 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  try {
    // the maintainer's widths as this version saves them (content pixels, "c", #828): 1108 px of data
    // columns, the selection column drawn at its 43 px checkbox and the row menu at its 48 px button (its
    // stored width is ignored, #828). The frame is sized from these numbers, not from the page's
    // data-basew: a page or server that counts a column wrongly would move a frame sized from data-basew
    // along with the table.
    const data = Math.round(
      Object.entries(ownerLayout.w)
        .filter(([id]) => id !== "actions")
        .reduce((a, [, w]) => a + w, 0) * 10,
    );
    const need = data + 43 + 48;
    expect(await postPref(page, "table.devices", { ...ownerLayout, c: true })).toBe(204);
    await page.reload();
    const wide = await deviceFrame(page);
    expect(wide.basew, "the saved layout is as wide as its data columns, the drawn checkbox and the row menu").toBe(need);
    // a window whose frame is exactly that wide: the table fits it; one pixel less and it scrolls by one
    for (const less of [0, 1]) {
      // the page's side gutter changes near 1280 px, so the window is found by measuring, not by subtracting once
      let f = wide;
      for (let viewport = 1300, i = 0; i < 4 && f.client !== need - less; i++) {
        viewport -= f.client - (need - less);
        await page.setViewportSize({ width: viewport, height: 900 });
        await page.reload();
        f = await deviceFrame(page);
      }
      expect(f.client, "the frame follows the window").toBe(need - less);
      expect(f.scroll, less ? "one pixel too narrow" : "a frame as wide as the drawn columns fits them").toBe(f.client + less);
    }
  } finally {
    await page.setViewportSize({ width: 1300, height: 900 });
    await resetAll(page, "/devices", "table.devices");
  }
});

test("a layout stored with stretched widths before 0.36.1 fits its window again, and its first save changes nothing in view (#805)", async ({ page }) => {
  await page.setViewportSize({ width: 1920, height: 1080 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  try {
    expect(await postPref(page, "table.devices", ownerShown)).toBe(204);
    await page.reload();
    const start = await deviceFrame(page);
    expect(start.scroll, "the 14 columns fit a 1920 px window without a reset").toBeLessThanOrEqual(start.client);
    // a save that changes nothing (Status down and up again; each move saves and reloads)
    for (const dir of ["down", "up"]) {
      const menu = page.locator("details.colmenu").first();
      if (!(await menu.evaluate((d) => (d as HTMLDetailsElement).open))) await menu.locator("summary").click();
      const done = saved(page, "table.devices");
      const reloaded = page.waitForEvent("load");
      await menu.locator(`[data-move="${dir}"][data-moveid="status"]`).click();
      await done;
      await reloaded;
    }
    await page.reload();
    const after = await deviceFrame(page);
    expect(after.scroll).toBeLessThanOrEqual(after.client);
    expect(after.widths.length).toBe(start.widths.length);
    after.widths.forEach((w, i) => expect(Math.abs(w - start.widths[i]), `column ${i + 1} after the first save`).toBeLessThanOrEqual(1.5));
  } finally {
    await resetAll(page, "/devices", "table.devices");
  }
});

// #828. A column of the device table is as wide as the person set it, in pixels of content, and nothing
// else: the table is as wide as its columns, and when the frame is wider the room left over stays empty
// after the last data column, before the columns pinned right. Until then every width was a share of a
// table stretched to its frame: a double-click fit kept the table's total and took the room from (or gave
// it to) every other column, the room a wide frame had spare went to every column, the row menu included
// (159 px for a 48 px button), and a fit was stored at the stretch of the window it was made in, so Address,
// fitted at 1714 px, was cut again at 1287 px. The layout is the maintainer's from the live database, saved
// by 0.36.0 (stretched widths, no "b"); the issue measured it on a 20-device fleet.
const owner828 = {
  w: { actions: 15.9, address: 12.6, family: 12.3, firmware: 14.9, name: 33.3, status: 27.7, uptime: 16, wifi: 19.2 },
  h: ["power", "reach", "restart"],
  o: ["status", "reach", "wifi", "firmware", "family", "address", "uptime", "restart", "otaurl", "grouptopic", "power"],
};
const names828: Record<string, string> = { name: "Name", status: "Status", wifi: "Wi-Fi", firmware: "Firmware", family: "Chip", address: "Address", uptime: "Uptime", power: "Power" };
const issueSizes = [
  [1287, 900],
  [1440, 900],
  [1714, 1366],
] as const;

/** The drawn width of every visible header cell by its text ("(spare)" for the empty room), and the frame. */
async function deviceColumns(page: Page) {
  await deviceFrame(page);
  return page.evaluate(() => {
    const t = document.querySelector<HTMLTableElement>('table[data-table="devices"]')!;
    const f = t.parentElement!;
    const w: Record<string, number> = {};
    const order: string[] = [];
    for (const th of t.tHead!.rows[0].cells) {
      if (th.offsetParent === null) continue;
      const name = th.classList.contains("spare") ? "(spare)" : th.textContent!.trim();
      w[name] = th.getBoundingClientRect().width;
      order.push(name);
    }
    const right = (n: string) => [...t.tHead!.rows[0].cells].find((th) => th.textContent!.trim() === n)!.getBoundingClientRect().right;
    return { w, order, client: f.clientWidth, scroll: f.scrollWidth, frameRight: f.getBoundingClientRect().right - f.clientLeft, actionsRight: right("Actions") };
  });
}
/** The cells of a column that do not show all of their content: wider than their box (the box cuts them), no tolerance. */
async function cutCells(page: Page, name: string) {
  await deviceFrame(page);
  return page.evaluate((n) => {
    const t = document.querySelector<HTMLTableElement>('table[data-table="devices"]')!;
    const th = [...t.tHead!.rows[0].cells].find((h) => h.textContent!.trim() === n)!;
    let cut = 0,
      rows = 0;
    for (const r of t.tBodies[0].rows) {
      const c = r.cells[th.cellIndex];
      if (!c || c.colSpan > 1) continue;
      rows++;
      const ext = c.querySelector<HTMLElement>("a.ext");
      if (c.scrollWidth > c.clientWidth || (ext && ext.scrollWidth > ext.clientWidth)) cut++;
    }
    return { cut, rows, width: th.getBoundingClientRect().width };
  }, name);
}
/** Double-clicks the resize handle of a column (fit to content) and returns the layout the page saved. */
async function fitByDoubleClick(page: Page, name: string): Promise<{ w: Record<string, number>; c?: boolean }> {
  const box = await handleBox(page, name);
  const done = saved(page, "table.devices");
  await page.mouse.dblclick(box.x + box.width / 2, box.y + box.height / 2);
  return (await done).request().postDataJSON();
}
async function openDevices(page: Page, layout: unknown, size: readonly [number, number]) {
  await page.setViewportSize({ width: size[0], height: size[1] });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  expect(await postPref(page, "table.devices", layout)).toBe(204);
  await page.goto(app.url + "/devices");
  await deviceFrame(page);
}
const others = (before: Record<string, number>, after: Record<string, number>, skip: string[]) =>
  Object.keys(before)
    .filter((k) => !skip.includes(k))
    .map((k) => ({ k, d: Math.abs(after[k] - before[k]) }))
    .filter((x) => x.d > 0.5);

test("#828: a layout saved before 0.36.1 comes back with no cut cell at the owner's window widths", async ({ page }) => {
  try {
    for (const size of issueSizes) {
      await openDevices(page, owner828, size);
      for (const name of ["Name", "Status", "Wi-Fi", "Firmware", "Chip", "Address", "Uptime"]) {
        const c = await cutCells(page, name);
        expect(c.rows).toBeGreaterThan(0);
        expect(c.cut, `${name} at ${size[0]} px (${c.width.toFixed(1)} px wide)`).toBe(0);
      }
      // A frame with room for the whole layout lifts Address and Firmware to their floors and takes nothing
      // from the other columns: the room they need comes from the spare room, not from Name or Status (the
      // longest device name wrapped when every column paid its share).
      if (size[0] === 1714) {
        const m = await deviceColumns(page);
        expect(m.w["(spare)"], "the frame has room to spare").toBeGreaterThan(0);
        for (const [id, w] of Object.entries(owner828.w)) {
          if (id === "actions") continue;
          expect(m.w[names828[id]], `${names828[id]} against its stored ${w * 10} px at 1714 px`).toBeGreaterThanOrEqual(w * 10 - 0.5);
        }
      }
    }
  } finally {
    await resetAll(page, "/devices", "table.devices");
  }
});

test("#828: Power in a layout saved before 0.36.1 comes back as wide as its content, not as a share of the old table", async ({ page }) => {
  // Power is pinned right: it has no handle and no fit in the device table, so a share of the old stretched
  // table, stored by the next save, could only be undone by Reset. Stored here at 200 px, about three
  // times its content.
  const withPower = { ...owner828, h: ["reach", "restart"], w: { ...owner828.w, power: 20 } };
  try {
    await openDevices(page, withPower, [1714, 1366]);
    const content = async () => {
      await deviceFrame(page);
      return page.evaluate(() => {
        const t = document.querySelector<HTMLTableElement>('table[data-table="devices"]')!;
        const th = [...t.tHead!.rows[0].cells].find((h) => h.textContent!.trim() === "Power")!;
        // the width of a cell's content on one line, with its padding; the header also holds room for a handle (12 px), as a fit counts it
        const need = (cell: HTMLElement, extra: number) => {
          const r = document.createRange();
          r.selectNodeContents(cell);
          const cs = getComputedStyle(cell);
          return r.getBoundingClientRect().width + parseFloat(cs.paddingLeft) + parseFloat(cs.paddingRight) + extra;
        };
        let want = need(th, 12);
        for (const row of t.tBodies[0].rows) {
          const td = row.cells[th.cellIndex];
          if (td && td.colSpan === 1) want = Math.max(want, need(td, 0));
        }
        return { want, w: th.getBoundingClientRect().width, handle: !!th.querySelector(".colhandle") };
      });
    };
    const first = await content();
    expect(first.handle, "Power has no resize handle").toBe(false);
    expect((await cutCells(page, "Power")).cut, "Power shows all of its content").toBe(0);
    expect(first.w, `Power (${first.w.toFixed(1)} px) against its content (${first.want.toFixed(1)} px)`).toBeLessThanOrEqual(Math.ceil(first.want) + 1);
    // the next save stores the width that is drawn, and a reload draws it again
    const doc = await fitByDoubleClick(page, "Status");
    expect(doc.c).toBe(true);
    expect(Math.abs((doc.w.power ?? 0) * 10 - first.w), "the stored width of Power").toBeLessThanOrEqual(1);
    await page.reload();
    const after = await content();
    expect(Math.abs(after.w - first.w), "Power after a reload").toBeLessThanOrEqual(0.5);
  } finally {
    await resetAll(page, "/devices", "table.devices");
  }
});

test("#828: a layout saved by 0.36.1 (scaled below its content) comes back with no cut cell", async ({ page }) => {
  // after a fit at 1714 px 0.36.1 stored Address at 86 px for 156 px of content, scaled to the table's floor
  const scaled = { ...owner828, b: true, w: { name: 17.5, status: 14.2, wifi: 10.1, firmware: 7.8, family: 6.5, address: 8.6, uptime: 8.2, actions: 8.2 } };
  try {
    for (const size of [issueSizes[0], issueSizes[2]]) {
      await openDevices(page, scaled, size);
      for (const name of ["Firmware", "Chip", "Address", "Uptime"]) expect((await cutCells(page, name)).cut, `${name} at ${size[0]} px`).toBe(0);
    }
  } finally {
    await resetAll(page, "/devices", "table.devices");
  }
});

test("#828: a fit makes the column as wide as its content, and it stays so after a reload and at another window width", async ({ page }) => {
  try {
    await openDevices(page, owner828, [1714, 1366]);
    await fitByDoubleClick(page, "Address");
    expect((await cutCells(page, "Address")).cut, "right after the fit").toBe(0);
    const fitted = (await cutCells(page, "Address")).width;
    await page.reload();
    expect((await cutCells(page, "Address")).cut, "after a reload").toBe(0);
    expect(Math.abs((await cutCells(page, "Address")).width - fitted), "the fitted width after a reload").toBeLessThanOrEqual(0.5);
    // the window the release smoke check used
    await page.setViewportSize({ width: 1287, height: 900 });
    await page.reload();
    expect((await cutCells(page, "Address")).cut, "at 1287 px").toBe(0);
    expect(Math.abs((await cutCells(page, "Address")).width - fitted), "the fitted width at 1287 px").toBeLessThanOrEqual(0.5);

    // 0.36.1 stopped the fit 0.1 px short at 1440 px (155.9 px for 156): the fit leaves no cell cut, with no tolerance
    await openDevices(page, owner828, [1440, 900]);
    await fitByDoubleClick(page, "Address");
    await page.reload();
    expect((await cutCells(page, "Address")).cut, "fitted at 1440 px, after a reload").toBe(0);
  } finally {
    await resetAll(page, "/devices", "table.devices");
  }
});

test("#828: a fit changes only its own column; the room it frees stays empty after the last data column", async ({ page }) => {
  try {
    await openDevices(page, owner828, [1714, 1366]);
    const before = await deviceColumns(page);
    await fitByDoubleClick(page, "Status");
    const after = await deviceColumns(page);
    expect(after.w.Status, "Status fitted to its content").toBeLessThan(before.w.Status - 50);
    expect(others(before.w, after.w, ["Status", "(spare)"]), "the other columns after the fit of Status").toEqual([]);
    expect(after.w["(spare)"] - before.w["(spare)"], "the room Status gave up is spare room").toBeCloseTo(before.w.Status - after.w.Status, 0);
  } finally {
    await resetAll(page, "/devices", "table.devices");
  }
});

test("#828: the row menu column fits its button at every window width: no handle, no stretch, never saved", async ({ page }) => {
  try {
    for (const size of issueSizes) {
      await openDevices(page, owner828, size);
      const m = await page.evaluate(() => {
        const t = document.querySelector<HTMLTableElement>('table[data-table="devices"]')!;
        const th = [...t.tHead!.rows[0].cells].find((h) => h.textContent!.trim() === "Actions")!;
        const td = t.tBodies[0].rows[0].cells[th.cellIndex];
        const cs = getComputedStyle(td);
        const button = td.querySelector("summary")!.getBoundingClientRect().width;
        return { w: th.getBoundingClientRect().width, fits: button + parseFloat(cs.paddingLeft) + parseFloat(cs.paddingRight), handle: !!th.querySelector(".colhandle") };
      });
      expect(m.handle, `a resize handle on the row menu at ${size[0]} px`).toBe(false);
      expect(Math.abs(m.w - m.fits), `the row menu column (${m.w.toFixed(1)} px) against its button and padding (${m.fits.toFixed(1)} px) at ${size[0]} px`).toBeLessThanOrEqual(1);
    }
    // a save of any change leaves the row menu out
    const doc = await fitByDoubleClick(page, "Firmware");
    expect(Object.keys(doc.w)).not.toContain("actions");
  } finally {
    await resetAll(page, "/devices", "table.devices");
  }
});

test("#828: the last data column can be narrowed to its content; the room becomes spare room, and widening a column scrolls", async ({ page }) => {
  try {
    await openDevices(page, owner828, [1714, 1366]);
    const before = await deviceColumns(page);
    const last = before.order.filter((n) => n !== "(spare)" && n !== "Actions" && n !== "Power").pop()!;
    expect(last).toBe("Uptime");
    const box = await handleBox(page, "Uptime");
    const done = saved(page, "table.devices");
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    await page.mouse.down();
    await page.mouse.move(box.x + box.width / 2 - 20, box.y + box.height / 2, { steps: 4 });
    await page.mouse.move(box.x + box.width / 2 - 40, box.y + box.height / 2, { steps: 4 });
    await page.mouse.up();
    await done;
    const after = await deviceColumns(page);
    expect(after.w.Uptime - before.w.Uptime, "Uptime follows the pointer").toBeCloseTo(-40, 0);
    expect(others(before.w, after.w, ["Uptime", "(spare)"]), "the other columns when Uptime narrows").toEqual([]);
    expect(after.w["(spare)"] - before.w["(spare)"], "the spare room grows by what Uptime gave up").toBeCloseTo(40, 0);
    expect((await cutCells(page, "Uptime")).cut, "Uptime is still as wide as its content").toBe(0);

    // widening a column takes nothing from the others: the table grows and scrolls once it is wider than its frame
    const handle = page.getByRole("separator", { name: "Resize column Firmware" });
    await handle.focus();
    const widened = saved(page, "table.devices");
    for (let i = 0; i < 10; i++) await page.keyboard.press("Shift+ArrowRight");
    await widened;
    const wide = await deviceColumns(page);
    expect(wide.w.Firmware - after.w.Firmware, "Firmware after ten steps of 40 px").toBeCloseTo(400, 0);
    expect(others(after.w, wide.w, ["Firmware", "(spare)"]), "the other columns when Firmware widens").toEqual([]);
    expect(wide.scroll, "the table is wider than its frame and scrolls").toBeGreaterThan(wide.client);
  } finally {
    await resetAll(page, "/devices", "table.devices");
  }
});

test("#828: a saved layout is drawn at its saved widths in any wider frame, the spare room before the columns pinned right", async ({ page }) => {
  // the layout as this version saves it: content pixels (tenths in w), marked "c"
  const content = { w: { name: 22, status: 16.5, wifi: 11.5, firmware: 10, family: 9, address: 16, uptime: 9.5, power: 7.5 }, h: ["reach", "restart"], o: owner828.o, c: true };
  try {
    for (const size of [issueSizes[1], issueSizes[2]]) {
      await openDevices(page, content, size);
      const m = await deviceColumns(page);
      const names: Record<string, string> = { name: "Name", status: "Status", wifi: "Wi-Fi", firmware: "Firmware", family: "Chip", address: "Address", uptime: "Uptime", power: "Power" };
      for (const [id, w] of Object.entries(content.w)) expect(m.w[names[id]], `${names[id]} at ${size[0]} px`).toBeCloseTo(w * 10, 0);
      expect(m.order.slice(-3), "the spare room lies between the last data column and the pinned columns").toEqual(["(spare)", "Power", "Actions"]);
      expect(m.w["(spare)"], "the frame is wider than the layout").toBeGreaterThan(0);
      // empty room, not a column: no cell of it shows anything (an empty cell reads "-" in every table, #51)
      const shown = await page.evaluate(() =>
        [...document.querySelectorAll('table[data-table="devices"] td.spare')].map((td) => (td.textContent ?? "") + getComputedStyle(td, "::after").content).filter((x) => x !== "none" && x !== "normal" && x !== ""),
      );
      expect(shown, "what the spare cells show").toEqual([]);
      expect(Math.abs(m.actionsRight - m.frameRight), "the row menu sits at the frame's right edge").toBeLessThanOrEqual(1);
      expect(m.scroll).toBeLessThanOrEqual(m.client);
    }
  } finally {
    await resetAll(page, "/devices", "table.devices");
  }
});

test("the page size is saved with the other preferences", async ({ page, browser }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/audit");
  await resetAll(page, "/audit", "pager");
  await expect(page.locator(".pager-size [aria-current=page]")).toHaveText("25");
  await page.locator(".pager-size").getByRole("link", { name: "100" }).click(); // the save races the navigation: no response to wait for
  await expect(page).toHaveURL(/per=100/);
  await page.waitForTimeout(300); // let the keepalive save land
  await page.goto(app.url + "/audit"); // no per= in the URL
  await expect(page.locator(".pager-size [aria-current=page]")).toHaveText("100");
  const second = await otherBrowser(browser, adam);
  await second.page.goto(app.url + "/audit");
  await expect(second.page.locator(".pager-size [aria-current=page]")).toHaveText("100");
  await second.ctx.close();
  const other = await otherBrowser(browser, eve);
  await other.page.goto(app.url + "/audit");
  await expect(other.page.locator(".pager-size [aria-current=page]")).toHaveText("25");
  await other.ctx.close();
  await resetAll(page, "/audit", "pager");
});

test("the endpoint refuses a missing token, unknown columns and oversize bodies", async ({ page }) => {
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  expect((await page.request.post(`${app.url}/prefs/table.devices`, { data: { h: ["family"] } })).status()).toBe(403); // no token
  expect(await postPref(page, "table.devices", { h: ["nope"] })).toBe(400);
  expect(await postPref(page, "table.devices", { h: ["name"] })).toBe(400);
  expect(await postPref(page, "table.devices", { w: { name: 10 }, x: 1 })).toBe(400);
  expect(await postPref(page, "table.devices", { h: ["a".repeat(10_000)] })).toBe(413);
  expect(await postPref(page, "table.devices", {})).toBe(204);
});

test("phone width: saved columns do not change the dense list", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  expect(await postPref(page, "table.devices", { h: ["family", "uptime"], w: { name: 40, status: 10 } })).toBe(204);
  await page.reload();
  const table = page.locator('table[data-table="devices"]');
  expect(await table.evaluate((t) => getComputedStyle(t).tableLayout)).not.toBe("fixed");
  await expect(page.locator("details.colmenu").first()).toBeHidden();
  await expect(page.locator(".colhandle").first()).toBeHidden();
  await expectNoOverflow(page, "devices on a phone with saved columns");
  if (shots) {
    mkdirSync(shots, { recursive: true });
    await page.screenshot({ path: join(shots, "devices-375-saved-columns.png"), fullPage: true });
  }
  await page.setViewportSize({ width: 1280, height: 800 });
  await resetAll(page, "/devices", "table.devices");
});

test("tooltips: chips explain themselves and a cut-off value shows its full text", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  await resetAll(page, "/devices", "table.devices");
  await expect(page.locator(".dv-st.t-ok").first()).toHaveAttribute("title", /^Online: /, { timeout: 40_000 });
  const family = page.locator("td.c-fam").first();
  await expect(family).toHaveAttribute("title", /only firmware built for this chip/i);
  // every badge in the device list carries an explanation
  const bare = await page.locator("table[data-table=devices] .bdg:not([title])").count();
  expect(bare).toBe(0);

  // narrow the Status column until its text is cut off, then hover the cell
  const handle = page.getByRole("separator", { name: "Resize column Status" });
  await handle.focus();
  for (let i = 0; i < 25; i++) await page.keyboard.press("ArrowLeft");
  const cell = page.locator('table[data-table="devices"] tbody td[data-label="Status"]').first();
  await cell.hover({ position: { x: 2, y: 2 } });
  await expect(cell).toHaveAttribute("data-tip", /\S/);
  expect(await cell.getAttribute("title")).toBeNull(); // one bubble only, no native title
  expect(await cell.evaluate((c) => c.scrollWidth > c.clientWidth)).toBe(true);

  await page.goto(app.url + "/settings/system");
  const help = await page.locator(".sys-tab:not([hidden]) .sf").count();
  expect(help).toBeGreaterThan(0);
  await resetAll(page, "/devices", "table.devices");
});

// A live list update replaces the table. Why: it may be fetched before the 400 ms debounced
// save of a column width reaches the server, and it removes the focused resize handle; the
// user must keep both the width they just set and the keyboard focus (found as a flaky
// "tooltips" test: the remaining arrow presses went nowhere).
test("a list update during keyboard resizing keeps the new width and the focused handle", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  await resetAll(page, "/devices", "table.devices");
  const handle = page.getByRole("separator", { name: "Resize column Status" });
  await handle.focus();
  const w0 = await width(page, "devices", "Status");
  for (let i = 0; i < 5; i++) await page.keyboard.press("ArrowLeft");
  const w1 = await width(page, "devices", "Status");
  expect(w1).toBeLessThan(w0 - 20);

  // the update arrives inside the save debounce: the server still has the old layout
  // listen before the request goes out: a fast answer must not slip past the wait
  const reloaded = page.waitForResponse((r) => r.url().includes("/devices/table"));
  await page.evaluate(() => {
    const t = document.getElementById("devtable")!;
    (window as any).htmx.ajax("GET", t.getAttribute("hx-get")!, { target: t, swap: "innerHTML" });
  });
  await reloaded;
  await expect.poll(async () => Math.abs((await width(page, "devices", "Status")) - w1)).toBeLessThan(1.5);
  await expect(page.getByRole("separator", { name: "Resize column Status" })).toBeFocused();

  for (let i = 0; i < 5; i++) await page.keyboard.press("ArrowLeft");
  expect(await width(page, "devices", "Status")).toBeLessThan(w1 - 20);
  await resetAll(page, "/devices", "table.devices");
});

// The width of a column is saved 400 ms after the last arrow key. Why this matters: when a list update
// replaced the table inside that delay, the save measured the table it was started for, which is no
// longer in the page and measures 0 px everywhere, so every column was saved at the minimum weight and
// the user's whole layout became equal-width columns until they set it again (found as the flaky
// "keyboard resizing" test with 60 devices, where a list update is always on its way). The clock is
// held so the update lands inside the delay however fast or slow the machine is.
test("a list update inside the save delay does not save the replaced table as the layout", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  await page.clock.install();
  await page.goto(app.url + "/devices");
  await resetAll(page, "/devices", "table.devices");
  const handle = page.getByRole("separator", { name: "Resize column Status" });
  await handle.focus();
  await width(page, "devices", "Status");
  await page.clock.pauseAt(Date.now() + 5_000);
  for (let i = 0; i < 5; i++) await page.keyboard.press("ArrowLeft");
  const w1 = await width(page, "devices", "Status");

  // the update replaces the table; the save has not run (the page's timers stand still)
  await page.evaluate(
    () =>
      new Promise<void>((resolve) => {
        document.addEventListener("htmx:afterSwap", (e) => (e as CustomEvent).detail.target.id === "devtable" && resolve());
        const t = document.getElementById("devtable")!;
        t.querySelector("table")!.setAttribute("data-old", "");
        (window as any).htmx.ajax("GET", t.getAttribute("hx-get")!, { target: t, swap: "innerHTML" });
      }),
  );
  await expect(page.locator('#devtable table[data-table="devices"]:not([data-old])')).toHaveCount(1);

  // now the save runs
  const request = page.waitForRequest((r) => r.url().endsWith("/prefs/table.devices") && r.method() === "POST");
  await page.clock.runFor(1_000);
  const doc = JSON.parse((await request).postData() ?? "{}");
  // tenths of the pixels; 3 is the smallest weight a column can be saved with
  expect(doc.w.status * 10).toBeGreaterThan(w1 - 5);
  expect(doc.w.status * 10).toBeLessThan(w1 + 5);
  expect(Object.values(doc.w as Record<string, number>).filter((x) => x <= 3)).toEqual([]);
  await resetAll(page, "/devices", "table.devices");
});

// A device event reloads the list (sse:device, 500 ms after a burst) and replaces the table, its
// handles and the one under the mouse. Why: when that lands while the user drags a column edge, the
// drag must go on and the width must still be saved; before, the release went to the removed handle,
// nothing was saved and the next update put the old width back (found as a flaky "dragging" test in
// the full CI, where the simulated devices come online during the first test).
test("a list update during a mouse drag keeps resizing and saves the width", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  await resetAll(page, "/devices", "table.devices");
  const before = await width(page, "devices", "Firmware");
  const box = await handleBox(page, "Firmware");
  const x = box.x + box.width / 2,
    y = box.y + box.height / 2;
  const done = saved(page, "table.devices");
  await page.mouse.move(x, y);
  await page.mouse.down();
  await page.mouse.move(x + 20, y, { steps: 5 });

  const reloaded = page.waitForResponse((r) => r.url().includes("/devices/table"));
  await page.evaluate(() => {
    const t = document.getElementById("devtable")!;
    t.querySelector("table")!.setAttribute("data-old", "");
    (window as any).htmx.ajax("GET", t.getAttribute("hx-get")!, { target: t, swap: "innerHTML" });
  });
  await reloaded;
  // the table (and the handle the drag started on) has really been replaced
  await expect(page.locator('#devtable table[data-table="devices"]:not([data-old])')).toHaveCount(1);

  await page.mouse.move(x + 40, y, { steps: 5 });
  await page.mouse.up();
  await done;
  const dragged = await width(page, "devices", "Firmware");
  expect(dragged).toBeGreaterThan(before + 25);
  await expect(page.locator("body")).not.toHaveClass(/is-resizing/);
  await page.reload();
  // the page lays the saved widths out again when its fonts are in (tables.js enhance): wait for that
  await expect.poll(async () => Math.abs((await width(page, "devices", "Firmware")) - dragged)).toBeLessThan(3);
  await resetAll(page, "/devices", "table.devices");
});

// Why: two list updates overlap. The first was answered by the server before the user's new width
// was saved (old layout), the second left after the save. The first response then landed and was
// judged by the SECOND request's start ("the server had it already"), so the old layout stayed on
// screen until the second answer came (seen as the flaky mouse-drag test: width read in between).
// Every response is judged by its own start. The width is read in the swap's own task, right after the old answer lands.
test("a late answer to an older list request does not undo a width saved since", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  await resetAll(page, "/devices", "table.devices");
  const handle = page.getByRole("separator", { name: "Resize column Status" });

  // the first list request is answered with the old layout but delivered only when released
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  let held!: () => void;
  const isHeld = new Promise<void>((r) => (held = r));
  let first = true;
  await page.route(/\/devices\/table/, async (route) => {
    if (!first) return route.continue();
    first = false;
    const old = await route.fetch();
    held();
    await gate;
    await route.fulfill({ response: old, headers: { ...old.headers(), "x-held": "1" } });
  });
  // a different source element each time: htmx queues requests of one element behind each other
  const ajax = (source: "devtable" | "body") =>
    page.evaluate((src) => {
      const t = document.getElementById("devtable")!;
      const from = src === "body" ? document.body : t;
      // not returned: htmx.ajax's promise settles with the swap, and the first one is held back
      void (window as any).htmx.ajax("GET", t.getAttribute("hx-get")!, { source: from, target: t, swap: "innerHTML" });
    }, source);
  await ajax("devtable");
  await isHeld;

  await handle.focus();
  const w0 = await width(page, "devices", "Status");
  const done = saved(page, "table.devices");
  for (let i = 0; i < 5; i++) await page.keyboard.press("ArrowLeft");
  await done;
  const w1 = await width(page, "devices", "Status");
  expect(w1).toBeLessThan(w0 - 20);

  // a second request leaves after the save; then the old answer lands
  await page.evaluate(() => {
    (window as any).__widths = [];
    document.addEventListener("htmx:afterSwap", (e) => {
      const h = [...document.querySelectorAll('table[data-table="devices"] thead th')].find((x) => x.textContent?.trim() === "Status");
      const xhr = (e as CustomEvent).detail.xhr as XMLHttpRequest;
      (window as any).__widths.push({ held: xhr.getResponseHeader("x-held") === "1", w: h ? h.getBoundingClientRect().width : 0 });
    });
  });
  type Swap = { held: boolean; w: number };
  const swaps = () => page.evaluate(() => (window as any).__widths as Swap[]);
  await ajax("body");
  // release the old answer only after a newer one has landed (an extra SSE refresh counts too)
  await expect.poll(async () => (await swaps()).some((x) => !x.held)).toBe(true);
  release();
  await expect.poll(async () => (await swaps()).some((x) => x.held)).toBe(true);
  const old = (await swaps()).find((x) => x.held)!;
  expect(Math.abs(old.w - w1)).toBeLessThan(1.5);
  await resetAll(page, "/devices", "table.devices");
});

// The other half of the flaky "tooltips" test: a list update that lands after the handle got
// focus but before the first arrow key (no layout was set yet, so nothing is restored by width).
// The new table's handles are only created in the same settle step, so the focus has to wait for
// them; before, the arrow presses went to the page and the column never narrowed.
test("a list update before the first arrow key keeps the focused handle", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  await resetAll(page, "/devices", "table.devices");
  const handle = page.getByRole("separator", { name: "Resize column Status" });
  await handle.focus();
  const w0 = await width(page, "devices", "Status");

  // swap the table and look at the focus in the very task the swap settles in
  const focusedAfterSwap = await page.evaluate(
    () =>
      new Promise<string>((resolve) => {
        document.addEventListener("htmx:afterSettle", () => queueMicrotask(() => resolve(document.activeElement?.getAttribute("aria-label") ?? "none")), { once: true });
        const t = document.getElementById("devtable")!;
        (window as any).htmx.ajax("GET", t.getAttribute("hx-get")!, { target: t, swap: "innerHTML" });
      }),
  );
  expect(focusedAfterSwap).toBe("Resize column Status");

  for (let i = 0; i < 5; i++) await page.keyboard.press("ArrowLeft");
  expect(await width(page, "devices", "Status")).toBeLessThan(w0 - 20);
  await resetAll(page, "/devices", "table.devices");
});

test("the three main tables render at desktop and phone width (screenshots with UX_SHOTS)", async ({ page }, testInfo) => {
  const dir = shots ?? testInfo.outputPath("shots");
  mkdirSync(dir, { recursive: true });
  await page.setExtraHTTPHeaders(adam);
  for (const [name, w, h] of [["1280", 1280, 800], ["375", 375, 812]] as const) {
    await page.setViewportSize({ width: w, height: h });
    for (const [label, path] of [["devices", "/devices"], ["audit", "/audit"], ["transactions", "/transactions"]]) {
      await page.goto(app.url + path);
      await page.waitForLoadState("networkidle");
      await expectNoOverflow(page, `${label} at ${name}`);
      await page.screenshot({ path: join(dir, `${label}-${name}.png`), fullPage: true });
    }
  }
});

// v0.6.1 hotfix: hovering a page once copied the WHOLE page text into a native
// title (the cut-off check climbed from a container), and a second, native
// bubble showed next to the custom tooltip. Tooltips explain, they never list
// data: only cells get the cut-off text, every tooltip is at most 160
// characters, and no element carries both a title and the custom tooltip.
test("tooltips: never a page dump, never two bubbles, at most 160 characters", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  for (const path of ["/", "/devices", "/policy", "/policy/io", "/firmware", "/transactions", "/audit", "/settings/system", "/devices/new", "/discovery/scan", "/about"]) {
    const res = await page.goto(app.url + path);
    if (!res || res.status() >= 400) continue;
    await page.waitForLoadState("networkidle");
    // hover the containers and a spread of points over the page
    for (const sel of ["main", "main table", "main table tbody tr", "header"]) {
      const el = page.locator(sel).first();
      if (await el.count()) await el.hover({ force: true }).catch(() => {});
    }
    for (const y of [120, 260, 400, 540, 680]) for (const x of [80, 400, 760, 1100]) await page.mouse.move(x, y);
    const bad = await page.evaluate(() => {
      const out: string[] = [];
      for (const el of Array.from(document.querySelectorAll("[title], [data-tip]"))) {
        const t = el.getAttribute("data-tip") ?? el.getAttribute("title") ?? "";
        if (el.hasAttribute("title") && el.hasAttribute("data-tip")) out.push(`both: ${el.tagName}`);
        if (t.length > 160) out.push(`long (${t.length}): ${el.tagName}.${el.className}`);
        if (el.hasAttribute("data-autotitle") && !el.closest("td, th")) out.push(`auto outside a cell: ${el.tagName}`);
      }
      const tip = document.querySelector(".tooltip:not([hidden])");
      if (tip && (tip.textContent ?? "").length > 160) out.push("tooltip text over 160");
      return out;
    });
    expect(bad, path).toEqual([]);
  }
});

// v0.7.0: an (i) button inside a <label> becomes the label's control when it
// comes before the input (the first labelable element wins), so clicking the
// label text and screen readers would hit the button. Every label that wraps
// a form field must point at that field.
test("labels point at their own field, not at an (i) button", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  for (const path of ["/devices/new", "/updates/new", "/settings/system", "/policy/rules/new", "/firmware", "/discovery/scan", "/settings/system/snapshots", "/setup"]) {
    const res = await page.goto(app.url + path);
    if (!res || res.status() >= 400) continue;
    const bad = await page.evaluate(() =>
      Array.from(document.querySelectorAll("label")).flatMap((l) => {
        const field = l.querySelector("input:not([type=hidden]), select, textarea");
        return field && l.control !== field ? [`${(l.textContent ?? "").trim().slice(0, 40)}`] : [];
      }),
    );
    expect(bad, path).toEqual([]);
  }
});
