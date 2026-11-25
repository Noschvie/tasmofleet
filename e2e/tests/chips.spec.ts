import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// #1095: a filter chip is one thing, ui.Chip, and a tag another, ui.Tag. Eight families of CSS used to draw them
// (34 px or 28 px, a 7 px radius or a pill, a selected look that was inverted on some and tinted on others), so the
// same filter looked different on every page. Here the chips of three pages, found through their strip, must be
// one height (28 px; 44 px on a phone, where a thumb hits them), pills, and tinted rather than inverted when
// selected; the tags of the audit trail must be the outlined 5 px label of the design system. A chip that grows
// when it is ticked would move the chips after it, so the weekday chips must not change their width.
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
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

type Look = { h: number; w: number; radius: string; bg: string; fg: string; selected: boolean };

/** looks reads what a chip is drawn like, by the strip it sits in (the class names are not what is under test). */
async function looks(page: Page, selector: string): Promise<Look[]> {
  return page.locator(selector).evaluateAll((els) =>
    els.map((el) => {
      const r = el.getBoundingClientRect();
      const s = getComputedStyle(el);
      const input = el.querySelector("input");
      return {
        h: Math.round(r.height * 10) / 10,
        w: Math.round(r.width * 10) / 10,
        radius: s.borderTopLeftRadius,
        bg: s.backgroundColor,
        fg: s.color,
        selected: input ? input.checked : el.hasAttribute("aria-current"),
      };
    }),
  );
}

/** drawn checks the rules every chip keeps: the height, the pill and a selected look that is not inverted. */
function drawn(what: string, chips: Look[], height: number) {
  expect(chips.length, `${what}: the page shows chips`).toBeGreaterThan(1);
  for (const c of chips) {
    expect(c.h, `${what}: a chip is ${height} px high`).toBeCloseTo(height, 0);
    expect(c.radius, `${what}: a chip is a pill`).toBe("999px");
  }
  const plain = chips.find((c) => !c.selected);
  expect(plain, `${what}: a chip that is not selected to compare with`).toBeTruthy();
  for (const c of chips.filter((c) => c.selected)) {
    expect(c.bg, `${what}: a selected chip has its own tint`).not.toBe(plain!.bg);
    expect(c.bg, `${what}: a selected chip is not inverted (its fill is not the text colour of the others)`).not.toBe(plain!.fg);
  }
}

for (const scheme of ["light", "dark"] as const) {
  for (const [width, height] of [
    [1280, 28],
    [375, 44],
  ] as const) {
    test(`chips are ${height} px pills, tinted when selected (${width}px, ${scheme})`, async ({ page }) => {
      await page.emulateMedia({ colorScheme: scheme });
      await page.setViewportSize({ width, height: 900 });
      await page.setExtraHTTPHeaders(adam);

      // a filter that changes the address: the device list (a link per chip, the current one aria-current)
      await page.goto(app.url + "/devices");
      await expect(page.locator(".dv-chips > a[aria-current]")).toHaveCount(1);
      drawn("device list", await looks(page, ".dv-chips > a"), height);

      // a chip around an input: the weekdays of the snapshot schedule
      await page.goto(app.url + "/settings/system/snapshots");
      await expect(page.locator(".cks > label")).toHaveCount(7);
      await page.locator("input[name='v.SNAPSHOT_DAYS'][value='wed']").check();
      const before = (await looks(page, ".cks > label")).map((c) => c.w);
      drawn("weekdays", await looks(page, ".cks > label"), height);
      // ticking a chip must not move the chips after it: nothing gets bolder or wider
      await page.locator("input[name='v.SNAPSHOT_DAYS'][value='thu']").check();
      const after = (await looks(page, ".cks > label")).map((c) => c.w);
      expect(after.slice(0, 2), "the chips before the ticked one did not move").toEqual(before.slice(0, 2));
      expect(after[3], "a chip keeps its width when it is ticked").toBe(before[3]);

      // the policy rule's pills: the chip families of the scope
      await page.goto(app.url + "/policy/rules/new?command=Timezone");
      const families = "[data-scope] [aria-labelledby=fam-l] > label";
      await page.locator(families, { hasText: "ESP8266" }).locator("input").check();
      drawn("rule scope", await looks(page, families), height);
    });
  }
}

// An audit tag is the kind or the source of who acted (Proxy, CLI): outlined, radius 5, 500 11.5px, never a state.
test("audit tags are the outlined 5 px label of the design system", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/audit?since=all");
  const tag = page.locator(".who .tag").first();
  await expect(tag).toBeVisible();
  const m = await tag.evaluate((el) => {
    const s = getComputedStyle(el);
    return { radius: s.borderTopLeftRadius, weight: s.fontWeight, size: s.fontSize, border: s.borderTopWidth, style: s.borderTopStyle };
  });
  expect(m).toEqual({ radius: "5px", weight: "500", size: "11.5px", border: "1px", style: "solid" });
});
