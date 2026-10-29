import { mkdirSync } from "node:fs";
import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import {
  App,
  Devenv,
  startApp,
  startDevenv,
  writeTasmoAdminDir,
} from "../support/stack";

// #116: on a narrow screen the pill row scrolls sideways, and picking from it (the Chip dropdown
// sits at its far end) reloads the page, which used to put the row back to its start: every pill
// jumped. At desktop width the row fits and never scrolled. Every pill that was there before the selection must stay where it was
// (within 1 px), so the user's pointer keeps hitting the pill they meant. UX_SHOTS=<dir> keeps
// screenshots.
const shots = process.env.UX_SHOTS;
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
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
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

type Box = { x: number; y: number; w: number; h: number };

// the pills by their visible label (the count after it changes with the filter, the label does not)
async function pills(page: Page): Promise<Record<string, Box>> {
  return page.evaluate(() => {
    const out: Record<string, Box> = {};
    const add = (key: string, el: Element) => {
      const r = el.getBoundingClientRect();
      out[key] = { x: r.x, y: r.y, w: r.width, h: r.height };
    };
    for (const a of document.querySelectorAll(".dv-chips .dv-chip")) {
      const label = (a.firstChild?.textContent ?? "").trim();
      add(label, a);
    }
    const pick = document.querySelector(".dv-chips .dv-pick");
    if (pick) add("Chip dropdown", pick);
    return out;
  });
}

for (const scheme of ["light", "dark"] as const) {
  for (const width of [1280, 800, 375]) {
    test(`filter pills stay put when a chip is selected (${width}px, ${scheme})`, async ({
      page,
    }) => {
      await page.emulateMedia({ colorScheme: scheme });
      await page.setViewportSize({ width, height: 900 });
      await page.setExtraHTTPHeaders(adam);
      await page.goto(app.url + "/devices");
      await expect(page.locator(".dv-chips .dv-chip").first()).toBeVisible();
      // the CLI import runs after the server started; wait until the fleet is listed
      await expect(
        page.locator(".dv-chips .dv-chip", { hasText: "Online" }),
      ).toBeVisible();
      // The fleet comes online while the page is open, and the Online count (a digit more or less
      // changes the pill's width) is the fleet's, not the filter's. The toolbar is not live, so
      // reload until it shows the settled fleet: otherwise the pill moves because a device came
      // online between the two page loads, not because a chip was selected.
      await expect
        .poll(
          async () => {
            const n = await page
              .locator(".dv-chips .dv-chip", { hasText: "Online" })
              .locator(".dv-n")
              .textContent();
            if (n !== String(Object.keys(env.devices).length)) await page.reload();
            return n;
          },
          { timeout: 60_000, intervals: [1000] },
        )
        .toBe(String(Object.keys(env.devices).length));
      const family = page.locator("select[name=family]");
      const options = await family.locator("option").evaluateAll((os) =>
        (os as HTMLOptionElement[]).map((o) => o.value).filter((v) => v),
      );
      expect(options.length, "the fleet has chip families").toBeGreaterThan(0);

      // a narrow screen scrolls the pill row; the user picks from the far end (the Chip dropdown)
      await page.evaluate(() => {
        const c = document.querySelector(".dv-chips")!;
        c.scrollLeft = c.scrollWidth;
      });
      const before = await pills(page);
      if (shots) {
        mkdirSync(shots, { recursive: true });
        await page.screenshot({ path: `${shots}/pills-${width}-${scheme}-before.png` });
      }
      await Promise.all([
        page.waitForURL(/family=/),
        // force: Playwright would scroll the row to the select first, which is not what a thumb does
        family.selectOption(options[0], { force: true }),
      ]);
      await expect(page.locator("select[name=family]")).toHaveValue(options[0]);
      const after = await pills(page);
      if (shots) await page.screenshot({ path: `${shots}/pills-${width}-${scheme}-after.png` });
      if (process.env.PILLS_DUMP) console.log(JSON.stringify({ before, after }, null, 1));

      for (const [label, b] of Object.entries(before)) {
        const a = after[label];
        expect(a, `${label} is still there`).toBeTruthy();
        for (const k of ["x", "y", "w", "h"] as const)
          expect(
            Math.abs(a[k] - b[k]),
            `${label}.${k}: ${b[k]} -> ${a[k]}`,
          ).toBeLessThanOrEqual(1);
      }
    });
  }
}
