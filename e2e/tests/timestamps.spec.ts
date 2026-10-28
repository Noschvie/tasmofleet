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

for (const z of zones) {
  test(`the audit log shows times in the browser's zone ${z.timezoneId} (${z.locale})`, async ({ browser }) => {
    const ctx = await browser.newContext(z);
    const page = await ctx.newPage();
    await page.setExtraHTTPHeaders({ "X-authentik-username": "adam", "X-authentik-groups": "adm" });
    await page.goto(app.url + "/audit");
    const t = page.locator("time[data-ts]").first();
    await expect(t).toBeVisible();
    const iso = (await t.getAttribute("datetime"))!;
    const expected = new Intl.DateTimeFormat(z.locale, {
      timeZone: z.timezoneId, year: "numeric", month: "2-digit", day: "2-digit",
      hour: "2-digit", minute: "2-digit", second: "2-digit",
    }).format(new Date(iso));
    // the server runs in UTC: a text equal to the server's would mean the browser's zone was ignored
    await expect(t).toHaveText(expected);
    await ctx.close();
  });
}
