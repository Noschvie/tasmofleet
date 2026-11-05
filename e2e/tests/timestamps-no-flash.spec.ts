import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// #594: the Devices list refreshes its table by an htmx swap. The swapped-in <time data-ts> carries the
// server's fallback text (its zone, a fixed 24-hour format); time.js turns it into the browser's locale.
// While that ran on htmx:afterSettle (~20 ms after the swap) the browser drew the fallback first, and the
// "Last restart" column flickered on every refresh. The text must already be the browser's when the swap
// is done, i.e. at htmx:afterSwap, before any frame is drawn: a listener registered after time.js sees it.
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
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
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_GITHUB_URL,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
      TZ: "UTC", // the fallback text is in UTC; the browser below is not
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

test("a refreshed Devices list shows browser-locale times at htmx:afterSwap, not the server's fallback", async ({ browser }) => {
  const ctx = await browser.newContext({ timezoneId: "Asia/Tokyo", locale: "en-GB" });
  const page = await ctx.newPage();
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  const restart = page.locator("table[data-table=devices] td.c-restart time[data-ts]").first();
  await restart.waitFor({ state: "attached", timeout: 40_000 });

  // registered after time.js, so it runs after time.js's own handlers for the same event
  await page.evaluate(() => {
    const w = window as unknown as { __atSwap: { text: string; iso: string; key: string }[] };
    w.__atSwap = [];
    document.body.addEventListener("htmx:afterSwap", (e) => {
      const t = e.target as Element;
      t.querySelectorAll("time[data-ts]").forEach((el) => {
        w.__atSwap.push({ text: el.textContent ?? "", iso: el.getAttribute("datetime") ?? "", key: el.getAttribute("data-ts") ?? "" });
      });
    });
  });
  // the refresh the list does on every device change: GET /devices/table into #devtable
  await page.evaluate(() => {
    // @ts-expect-error htmx is a page global
    window.htmx.trigger("#devtable", "sse:resync");
  });
  await expect
    .poll(() => page.evaluate(() => (window as unknown as { __atSwap: unknown[] }).__atSwap.length), { timeout: 15_000 })
    .toBeGreaterThan(0);

  const seen = await page.evaluate(() => (window as unknown as { __atSwap: { text: string; iso: string; key: string }[] }).__atSwap);
  // the oracle is the page's own formatter (its output per key is covered by timestamps.spec.ts)
  const want = await page.evaluate(
    (ts) => ts.map((t) => (window as unknown as { tasmofleetFormatTime: (i: string, k: string) => string }).tasmofleetFormatTime(t.iso, t.key)),
    seen,
  );
  expect(want.every((w) => w !== ""), "the oracle knows every key").toBe(true);
  expect(seen.map((s) => s.text)).toEqual(want);
  await ctx.close();
});

// Other swap paths the app uses: outerHTML (the target element is replaced) and an out-of-band part.
for (const style of ["outerHTML", "innerHTML"] as const) {
  test(`a ${style} swap is repainted by htmx:afterSwap`, async ({ browser }) => {
    const ctx = await browser.newContext({ timezoneId: "Asia/Tokyo", locale: "en-GB" });
    const page = await ctx.newPage();
    await page.setExtraHTTPHeaders(adam);
    await page.goto(app.url + "/audit");
    await expect(page.locator("time[data-ts]").first()).toBeVisible();
    const got = await page.evaluate((st) => {
      const host = document.createElement("div");
      document.body.appendChild(host);
      let seen = "";
      document.body.addEventListener("htmx:afterSwap", () => {
        seen = document.querySelector("#flash-probe")?.textContent ?? "";
      });
      // @ts-expect-error htmx is a page global
      window.htmx.swap(host, `<time id="flash-probe" datetime="2026-03-04T22:15:09Z" data-ts="datetime-s">SERVER-TEXT</time>`, { swapStyle: st });
      return seen;
    }, style);
    expect(got).not.toBe("SERVER-TEXT");
    expect(got).not.toBe("");
    await ctx.close();
  });
}
