import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// #1103 (UI audit DS-033): a link in a table cell looked three ways: the device name in the text colour and 600,
// the address link in text-2 and 400, an external link in 400, each turning accent only under the pointer. The
// reader could not tell at a glance what is clickable. One style now (design-system.md, Buttons, Inline links):
// accent, weight 500, no underline at rest, underline on hover. The test reads the computed style of every link
// in a table cell of the three pages that mix them (Devices, Policy rules, Audit) and expects exactly that one
// style, so a page that adds its own link rule fails here. Buttons, tags and row-menu items are not inline links
// and are left out; a closed <details> menu still has computed styles, so only what is visible counts.
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
      TASMOFLEET_POLICY_RULES: JSON.stringify([
        { name: "Power after outage", command: "PowerOnState", want: "2" },
        { name: "LED hint", command: "LedState", want: "3", severity: "info" },
      ]),
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

type Look = { text: string; color: string; weight: string; line: string };

/** cellLinks reads what every visible link of a table cell is drawn like, and what the accent text colour resolves to. */
async function cellLinks(page: Page): Promise<{ links: Look[]; accent: string }> {
  return page.evaluate(() => {
    const probe = document.createElement("span");
    probe.style.color = "var(--color-accent-fg)"; // CSSOM, not an attribute: the CSP forbids inline styles
    document.body.append(probe);
    const accent = getComputedStyle(probe).color;
    probe.remove();
    const links: Look[] = [];
    for (const a of document.querySelectorAll<HTMLAnchorElement>("td a")) {
      if (a.matches(".btn, .tag, details a") || a.getClientRects().length === 0) continue;
      const s = getComputedStyle(a);
      links.push({ text: (a.textContent ?? "").trim().slice(0, 40), color: s.color, weight: s.fontWeight, line: s.textDecorationLine });
    }
    return { links, accent };
  });
}

const pages: Array<[string, (page: Page) => Promise<void>]> = [
  ["Devices", (page) => page.goto(app.url + "/devices").then(() => undefined)],
  [
    "the Devices table of a policy rule",
    async (page) => {
      await page.goto(app.url + "/policy");
      await page.locator("td a[href*='/policy/rules/']").first().click();
      await expect(page.locator("#dc-devs")).toBeVisible();
    },
  ],
  ["Audit", (page) => page.goto(app.url + "/audit?since=all").then(() => undefined)],
  ["Policy rules", (page) => page.goto(app.url + "/policy").then(() => undefined)],
];

for (const scheme of ["light", "dark"] as const) {
  for (const [name, open] of pages) {
    test(`a link in a table cell is one style on ${name}, ${scheme}`, async ({ page }) => {
      await page.emulateMedia({ colorScheme: scheme });
      await page.setExtraHTTPHeaders(adam);
      await page.setViewportSize({ width: 1280, height: 900 });
      await open(page);
      await expect(page.locator("td").first()).toBeVisible();
      const { links, accent } = await cellLinks(page);
      expect(links.length, `${name}: the page has links in table cells to judge`).toBeGreaterThan(0);
      // one style, and it is the decided one: not just "all the same", or a page that was wrong everywhere would pass
      for (const l of links) {
        expect({ color: l.color, weight: l.weight, line: l.line }, `${name}: the link "${l.text}"`).toEqual({
          color: accent,
          weight: "500",
          line: "none",
        });
      }
    });
  }
}
