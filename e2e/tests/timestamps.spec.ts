import { test, expect } from "../support/fixtures";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// #128, #126: every timestamp is a <time datetime data-ts> that time.js shows in the browser's
// locale and time zone, whatever zone the server runs in. The server's text is only the fallback.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv();
  app = await startApp({
    env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32", TASMOFLEET_ADMIN_GROUPS: "adm", TZ: "UTC" },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const zones = [
  { timezoneId: "Asia/Tokyo", locale: "en-GB" },
  { timezoneId: "America/Los_Angeles", locale: "de-DE" },
];

// The expected text is computed in the page, with the page's own Intl (the browser under test),
// not in Node: ICU data differs between the two.
const expectedInPage = (iso: string, withTitle: boolean) => {
  const o: Intl.DateTimeFormatOptions = { year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit" };
  return withTitle
    ? new Intl.DateTimeFormat(undefined, { ...o, timeZoneName: "short" }).format(new Date(iso))
    : new Intl.DateTimeFormat(undefined, o).format(new Date(iso));
};

for (const z of zones) {
  test(`the audit log shows times and tooltips in the browser's zone ${z.timezoneId} (${z.locale})`, async ({ browser }) => {
    const ctx = await browser.newContext(z);
    const page = await ctx.newPage();
    await page.setExtraHTTPHeaders({ "X-authentik-username": "adam", "X-authentik-groups": "adm" });
    await page.goto(app.url + "/audit");
    const t = page.locator("time[data-ts]").first();
    await expect(t).toBeVisible();
    const iso = (await t.getAttribute("datetime"))!;
    const [text, title] = await page.evaluate(
      ([i, f]) => [new Function("return (" + f + ")")()(i, false), new Function("return (" + f + ")")()(i, true)],
      [iso, expectedInPage.toString()] as [string, string],
    );
    // the server runs in UTC: a text equal to the server's would mean the browser's zone was ignored
    await expect(t).toHaveText(text);
    // the tooltip sits on the <time> and follows the browser too, with the zone's name
    await expect(t).toHaveAttribute("title", title);
    await ctx.close();
  });
}

// An HTMX or SSE swap brings server text again; time.js must repaint it (htmx:afterSettle).
test("a swapped-in timestamp is repainted in the browser's zone", async ({ browser }) => {
  const ctx = await browser.newContext(zones[0]);
  const page = await ctx.newPage();
  await page.setExtraHTTPHeaders({ "X-authentik-username": "adam", "X-authentik-groups": "adm" });
  await page.goto(app.url + "/audit");
  await expect(page.locator("time[data-ts]").first()).toBeVisible();
  const iso = "2026-03-04T22:15:09Z";
  const got = await page.evaluate(async (i) => {
    const host = document.createElement("div");
    host.id = "swap-host";
    document.body.appendChild(host);
    // @ts-expect-error htmx is a page global
    window.htmx.swap(host, `<time datetime="${i}" data-ts="datetime-s">SERVER-TEXT</time>`, { swapStyle: "innerHTML" });
    await new Promise((r) => setTimeout(r, 300));
    return host.textContent;
  }, iso);
  const want = await page.evaluate(([i, f]) => new Function("return (" + f + ")")()(i, false), [iso, expectedInPage.toString()] as [string, string]);
  expect(got).not.toBe("SERVER-TEXT");
  expect(got).toBe(want);
  await ctx.close();
});
