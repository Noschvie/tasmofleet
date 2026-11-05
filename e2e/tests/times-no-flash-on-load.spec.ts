import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import {
  App,
  Devenv,
  freePort,
  startApp,
  startDevenv,
  writeTasmoAdminDir,
} from "../support/stack";

// #635 (follow-up to #594): the Online and All chips on Devices are plain links, so every switch loads the
// whole page. The server writes each <time data-ts> in its fixed fallback form (here UTC) and time.js, a
// deferred script, rewrites it in the browser's locale and zone (here Asia/Tokyo, en-GB). The browser may
// draw a frame after parsing and before the deferred scripts have run, so the fallback showed for a moment.
//
// The probe runs before any page script and, in every animation frame, records the text of each time
// element that is visible, i.e. the text the frame draws (requestAnimationFrame callbacks run right before
// the frame is rendered, and no task runs in between). An element that was drawn with two different texts
// was reformatted after it had been shown: that is the flash. time.js is held back for a while (a slow
// disk, a busy CPU or a slow network make the same gap), so the gap is there on every run and the test
// does not depend on timing luck.
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
const HELD_MS = 400;
let app: App;
let env: Devenv;

test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE:
        env.vars.DEVENV_UPSTREAM_GITHUB_URL,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
      TZ: "UTC", // the fallback text is in UTC; the browser below is not
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
});

// One backup, so the backup pages and the audit log have times to show.
let deviceId = "";
test.beforeAll(async ({ browser }) => {
  test.setTimeout(120_000);
  const ctx = await browser.newContext({ extraHTTPHeaders: adam });
  const page = await ctx.newPage();
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: "esp32-dimmer", exact: true }).click();
  deviceId = page.url().match(/devices\/(\d+)/)![1];
  const csrf = await page.locator("input[name=csrf]").first().inputValue();
  const took = await page.request.post(
    `${app.url}/devices/${deviceId}/backup`,
    { headers: adam, form: { csrf }, maxRedirects: 0 },
  );
  expect(took.status(), "backup taken").toBe(303);
  // The device page has times (Last restart, Stability) only once the device has answered; until then they come by
  // an htmx swap after time.js has run, and a full load of the page would prove nothing.
  await expect
    .poll(
      async () =>
        (
          await page.request.get(`${app.url}/devices/${deviceId}`, {
            headers: adam,
          })
        ).text(),
      { timeout: 30_000 },
    )
    .toMatch(/<time[^>]*data-ts=/);
  await ctx.close();
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

interface Report {
  /** visible time elements the probe saw */
  seen: number;
  /** frames drawn while times were on the page and time.js had not run yet: the gap this file is about */
  early: number;
  /** elements drawn with more than one text: [texts drawn, in order] */
  flashed: string[][];
  /** elements whose final text differs from the page's own formatter */
  wrong: string[];
}

/** probe installs the frame recorder before any page script runs. */
async function installProbe(page: Page) {
  await page.addInitScript(() => {
    const drawn = new Map<Element, string[]>();
    let early = 0;
    (window as unknown as { __probe: () => unknown }).__probe = () => {
      const f = (
        window as unknown as {
          tasmofleetFormatTime?: (i: string, k: string) => string;
        }
      ).tasmofleetFormatTime;
      const flashed: string[][] = [];
      const wrong: string[] = [];
      drawn.forEach((texts, el) => {
        if (texts.length > 1) flashed.push(texts);
        const want = f
          ? f(
              el.getAttribute("datetime") ?? "",
              el.getAttribute("data-ts") ?? "",
            )
          : "";
        if (
          want &&
          el.getAttribute("data-ts") !== "title" &&
          el.textContent !== want
        )
          wrong.push(`${el.textContent} != ${want}`);
      });
      return { seen: drawn.size, early, flashed, wrong };
    };
    const frame = () => {
      const times = document.querySelectorAll("time[data-ts]");
      if (
        times.length &&
        !(window as unknown as { tasmofleetFormatTime?: unknown })
          .tasmofleetFormatTime
      )
        early++;
      times.forEach((el) => {
        const h = el as HTMLElement;
        if (
          !h.checkVisibility({
            visibilityProperty: true,
            opacityProperty: true,
            contentVisibilityAuto: true,
          })
        )
          return;
        const r = h.getBoundingClientRect();
        if (
          r.bottom <= 0 ||
          r.top >= innerHeight ||
          r.right <= 0 ||
          r.left >= innerWidth
        )
          return;
        const texts = drawn.get(el) ?? [];
        if (texts[texts.length - 1] !== h.textContent)
          texts.push(h.textContent ?? "");
        drawn.set(el, texts);
      });
      requestAnimationFrame(frame);
    };
    requestAnimationFrame(frame);
  });
}

/** holdTimeJs answers the page's time.js only after HELD_MS, as a slow machine or network would. */
async function holdTimeJs(page: Page) {
  await page.route(/\/static\/time[^/]*\.js/, async (route) => {
    await new Promise((r) => setTimeout(r, HELD_MS));
    await route.continue();
  });
}

async function report(page: Page): Promise<Report> {
  // time.js has run (its global exists) and a few more frames were drawn
  await page.waitForFunction(
    () =>
      typeof (window as unknown as { tasmofleetFormatTime?: unknown })
        .tasmofleetFormatTime === "function",
    null,
    { timeout: 15_000 },
  );
  await page.evaluate(
    () =>
      new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))),
  );
  return page.evaluate(() =>
    (window as unknown as { __probe: () => Report }).__probe(),
  );
}

async function browserPage(
  browser: import("@playwright/test").Browser,
  opts: { javaScriptEnabled?: boolean } = {},
) {
  // 1600 px: the Devices list's time columns are on screen (at 1280 they lie outside the table's scroll view, and
  // the probe would see times that are laid out but not drawn)
  const ctx = await browser.newContext({
    timezoneId: "Asia/Tokyo",
    locale: "en-GB",
    viewport: { width: 1600, height: 900 },
    ...opts,
  });
  const page = await ctx.newPage();
  await page.setExtraHTTPHeaders(adam);
  return { ctx, page };
}

test(
  "switching the Online and All chips never draws a time in the server's format",
  { tag: "@core" },
  async ({ browser }) => {
    const { ctx, page } = await browserPage(browser);
    await installProbe(page);
    await holdTimeJs(page);
    await page.goto(app.url + "/devices");
    for (let i = 0; i < 3; i++) {
      for (const chip of ["Online", "All"]) {
        const link = page.locator(`a.dv-chip:has-text("${chip}")`).first();
        await expect(link).toBeVisible({ timeout: 40_000 });
        await Promise.all([
          page.waitForNavigation({ waitUntil: "load" }),
          link.click(),
        ]);
        await expect(
          page.locator("table[data-table=devices] time[data-ts]").first(),
        ).toBeAttached({ timeout: 40_000 });
        const r = await report(page);
        expect(r.seen, `${chip}: the page shows times`).toBeGreaterThan(0);
        expect(
          r.early,
          `${chip}: frames were drawn before time.js ran`,
        ).toBeGreaterThan(0);
        expect(r.flashed, `${chip}: times drawn in two formats`).toEqual([]);
        expect(r.wrong, `${chip}: final text is the browser's`).toEqual([]);
      }
    }
    await ctx.close();
  },
);

// Every page that carries times, on a full load. The fix must not be specific to Devices.
const pages: { name: string; path: () => string }[] = [
  { name: "device page", path: () => `/devices/${deviceId}` },
  { name: "device backups", path: () => `/devices/${deviceId}/backups` },
  { name: "fleet backups", path: () => "/devices/backups" },
  { name: "audit log", path: () => "/audit" },
];
for (const p of pages) {
  test(`a full load of the ${p.name} never draws a time in the server's format`, async ({
    browser,
  }) => {
    const { ctx, page } = await browserPage(browser);
    await installProbe(page);
    await holdTimeJs(page);
    await page.goto(app.url + p.path());
    await expect(page.locator("time[data-ts]").first()).toBeAttached({
      timeout: 40_000,
    });
    const r = await report(page);
    expect(r.seen, `${p.name} shows times`).toBeGreaterThan(0);
    expect(r.early, "frames were drawn before time.js ran").toBeGreaterThan(0);
    expect(r.flashed, "times drawn in two formats").toEqual([]);
    expect(r.wrong, "final text is the browser's").toEqual([]);
    await ctx.close();
  });
}

test("no time is drawn in the server's format under CPU throttling alone, without holding time.js back", async ({
  browser,
}) => {
  const { ctx, page } = await browserPage(browser);
  await installProbe(page);
  const cdp = await ctx.newCDPSession(page);
  await cdp.send("Emulation.setCPUThrottlingRate", { rate: 6 });
  await page.goto(app.url + "/devices");
  const link = page.locator('a.dv-chip:has-text("Online")').first();
  await expect(link).toBeVisible({ timeout: 40_000 });
  await Promise.all([
    page.waitForNavigation({ waitUntil: "load" }),
    link.click(),
  ]);
  const r = await report(page);
  expect(r.flashed).toEqual([]);
  await ctx.close();
});

test("without JavaScript the server's text shows", async ({ browser }) => {
  const { ctx, page } = await browserPage(browser, {
    javaScriptEnabled: false,
  });
  await page.goto(app.url + "/audit");
  const t = page.locator("time[data-ts=datetime-s]").first();
  await expect(t).toBeAttached({ timeout: 40_000 });
  // at once (noscript.css), not after app.css's one-second fallback: checked once, without a retry that would wait for it
  expect(
    await t.evaluate((el) => el.checkVisibility({ visibilityProperty: true })),
    "visible at once",
  ).toBe(true);
  // the server's fixed form in its own zone (UTC), not the Tokyo / en-GB form a script would write
  const iso = (await t.getAttribute("datetime")) ?? "";
  expect(await t.textContent()).toBe(
    iso.slice(0, 10) + " " + iso.slice(11, 19),
  );
  await ctx.close();
});

test("when the browser cannot format a time, the server's text stays visible", async ({
  browser,
}) => {
  const { ctx, page } = await browserPage(browser);
  // an unsupported locale or zone makes Intl.DateTimeFormat throw
  await page.addInitScript(() => {
    const Orig = Intl.DateTimeFormat;
    // @ts-expect-error replace the constructor
    Intl.DateTimeFormat = function () {
      throw new RangeError("Incorrect locale information provided");
    };
    Intl.DateTimeFormat.prototype = Orig.prototype;
  });
  await page.goto(app.url + "/audit");
  const t = page.locator("time[data-ts=datetime-s]").first();
  await expect(t).toBeAttached({ timeout: 40_000 });
  await page.waitForFunction(
    () =>
      typeof (window as unknown as { tasmofleetFormatTime?: unknown })
        .tasmofleetFormatTime === "function",
    null,
    { timeout: 15_000 },
  );
  await page.evaluate(
    () =>
      new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))),
  );
  // as soon as time.js has handled it, not after app.css's one-second fallback: checked once, without a retry
  expect(
    await t.evaluate((el) => el.checkVisibility({ visibilityProperty: true })),
    "visible once time.js has run",
  ).toBe(true);
  const iso = (await t.getAttribute("datetime")) ?? "";
  expect(await t.textContent()).toBe(
    iso.slice(0, 10) + " " + iso.slice(11, 19),
  );
  await ctx.close();
});

// The fail-safe: app.css hides a time until time.js has handled it, so a time.js that never arrives (blocked,
// a network error) must not leave the times invisible. The server's text shows after about a second.
test("when time.js never loads, the server's text shows after a moment", async ({
  browser,
}) => {
  const { ctx, page } = await browserPage(browser);
  await page.route(/\/static\/time[^/]*\.js/, (route) => route.abort());
  await page.goto(app.url + "/audit");
  const t = page.locator("time[data-ts=datetime-s]").first();
  await expect(t).toBeAttached({ timeout: 40_000 });
  await expect(t).toBeVisible({ timeout: 10_000 });
  const iso = (await t.getAttribute("datetime")) ?? "";
  expect(await t.textContent()).toBe(
    iso.slice(0, 10) + " " + iso.slice(11, 19),
  );
  await ctx.close();
});
