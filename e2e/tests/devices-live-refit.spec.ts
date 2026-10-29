import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// The live cells of the device list swap one by one (status, uptime, Wi-Fi, power: several per device and
// poll). The table fits its columns after a swap, which measures every row, so those swaps are coalesced
// into one fit per 250 ms. The phone cards (hidden on a desktop) have live cells of their own; while their
// swaps were not coalesced each ran a full fit of the table, and while the 60-device fleet came online
// the page was busy for minutes: the "devices online" step of seeding the fleet ran into its time limit in
// CI (v0.18.0, pinned-overlap.spec.ts).
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
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

// 200 swaps of one live cell arrive within a few milliseconds, as while a fleet comes online
for (const [what, cell] of [
  ["a table row", "table[data-table=devices] tbody td[data-label=Status]"],
  ["a phone card (hidden at this width, but its cells swap all the same)", "li.dcard .dcard-st"],
] as const) {
  test(`swaps of a live cell in ${what.split(" (")[0]} refit the table once per throttle window`, async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.setExtraHTTPHeaders(adam);
    // every fit starts with markEmptyCells, which lists the cells by this selector: count that
    await page.addInitScript(() => {
      const w = window as unknown as { __fits: number };
      w.__fits = 0;
      const orig = Document.prototype.querySelectorAll;
      Document.prototype.querySelectorAll = function (this: Document, sel: string) {
        if (sel === "td[data-label]:not(.none)") w.__fits++;
        return orig.call(this, sel);
      };
    });
    await page.goto(app.url + "/devices");
    await page.locator("table[data-table=devices] .pwr").first().waitFor({ state: "attached", timeout: 20_000 });
    await page.waitForTimeout(1_000); // the fit after the fonts and the first reports
    const fits = await page.evaluate((s) => {
      const w = window as unknown as { __fits: number };
      w.__fits = 0;
      const el = document.querySelector(s)!;
      for (let i = 0; i < 200; i++) el.dispatchEvent(new CustomEvent("htmx:afterSettle", { bubbles: true }));
      return new Promise<number>((done) => setTimeout(() => done(w.__fits), 600));
    }, cell);
    expect(fits, `${what}: fits for 200 swaps`).toBeLessThanOrEqual(2);
  });
}
