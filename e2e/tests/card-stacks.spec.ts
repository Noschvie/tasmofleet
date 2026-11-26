import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { cardStackGaps } from "../support/layout";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Design rule (#391): in every card grid the cards stack directly under each other per column; a
// short card next to a tall one leaves no empty gap below it. The check is generic (any card with
// more than the grid gap above it in its column fails), so a page that goes back to a row-aligned
// grid fails here without a page-specific assertion.
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

const cardGap = 16;

// siblingCardGaps lists every card, and the tile row (.itiles), whose distance to the nearest box above it
// in its column is not the card gap. A column is the set of boxes that start at the same left edge.
async function siblingCardGaps(page: Page, gap = cardGap): Promise<string[]> {
  return page.evaluate((want) => {
    const boxes = [...document.querySelectorAll<HTMLElement>(".scard, .about-card, .devcard, .ulist, .wn-rel, .ucard, .itiles")].filter((c) => {
      const r = c.getBoundingClientRect();
      return r.width > 0 && r.height > 0 && !c.closest("[hidden], dialog:not([open])");
    });
    const name = (c: Element) => c.querySelector("h2")?.textContent?.trim() ?? (c.id || c.className);
    const out: string[] = [];
    for (const b of boxes) {
      const rb = b.getBoundingClientRect();
      const above = boxes
        .filter((a) => a !== b && !a.contains(b) && !b.contains(a) && Math.abs(a.getBoundingClientRect().left - rb.left) < 1.5 && a.getBoundingClientRect().bottom <= rb.top + 0.5)
        .map((a) => a.getBoundingClientRect().bottom);
      if (above.length === 0) continue;
      const got = rb.top - Math.max(...above);
      if (Math.abs(got - want) > 0.5) out.push(`"${name(b)}" is ${got.toFixed(1)}px under the box above it (want ${want})`);
    }
    return out;
  }, gap);
}

const tabs = ["general", "network", "mqtt", "discovery", "notifications", "firmware", "signin", "advanced"];
// [name, path, largest gap between stacked cards]: every page, the policy pages too, has the one card gap, 16 px (#1101)
const pages: [string, string, number][] = [
  ...tabs.map((t): [string, string, number] => [`settings tab ${t}`, `/settings/system?tab=${t}`, 17]),
  ["backups page", "/devices/backups", 17],
  ["about", "/about", 17],
  ["dashboard", "/", 17],
  ["users", "/users", 17],
  ["whats new", "/whats-new", 17],
  ["new policy rule", "/policy/rules/new", 17],
];

for (const width of [1280, 1000, 600, 375]) {
  for (const [name, path, maxGap] of pages) {
    test(`${name}: cards stack under each other at ${width}px`, async ({ page }) => {
      await page.setExtraHTTPHeaders(adam);
      await page.setViewportSize({ width, height: 900 });
      await page.goto(app.url + path);
      await expect(page.locator(".scard:visible, .about-card:visible, .devcard:visible, .ulist:visible, .wn-rel:visible, .ucard:visible").first()).toBeVisible();
      expect(await cardStackGaps(page, maxGap)).toEqual([]);
    });
  }
  test(`device overview: cards stack under each other at ${width}px`, async ({ page }) => {
    await page.setExtraHTTPHeaders(adam);
    await page.setViewportSize({ width, height: 900 });
    await page.goto(app.url + "/devices");
    const href = (await page.getByRole("link", { name: "esp32-dimmer", exact: true }).first().getAttribute("href"))!;
    await page.goto(app.url + href);
    await expect(page.locator("#identity")).toBeVisible();
    expect(await cardStackGaps(page)).toEqual([]);
  });
  // Why (#1101): the gap between two cards is one token (--card-gap, 16 px), not a number each container
  // writes for itself. Before it was 12 px from the tile row to the Relays card on the device overview and
  // 20 to 24 px on the policy pages, 16 px elsewhere; a page that writes its own value goes back to that.
  // cardStackGaps only sees a gap that is too large; this sees one that is too small as well. The
  // Dashboard and System settings already measured 16 px: they lock it, the overview and policy pages
  // are what fails without the change.
  const gapPages = [
    ["dashboard", "/"],
    ["system settings", "/settings/system?tab=general"],
    ["new policy rule", "/policy/rules/new"],
    ["policy rule draft", "/policy/rules/new?template=timezone"],
  ];
  // below 45rem (720 px) the System settings sections are a flat list divided by a rule, not cards with a gap
  for (const [name, path] of gapPages.filter(([n]) => !(n === "system settings" && width < 720))) {
    test(`${name}: the gap between sibling cards is the 16 px card gap at ${width}px`, async ({ page }) => {
      await page.setExtraHTTPHeaders(adam);
      await page.setViewportSize({ width, height: 900 });
      await page.goto(app.url + path);
      await expect(page.locator(".scard:visible, .devcard:visible, .ucard:visible, .cstacks:visible").first()).toBeVisible();
      expect(await siblingCardGaps(page)).toEqual([]);
    });
  }
  test(`device overview: the gap between sibling cards, the tile row included, is 16 px at ${width}px`, async ({ page }) => {
    await page.setExtraHTTPHeaders(adam);
    await page.setViewportSize({ width, height: 900 });
    await page.goto(app.url + "/devices");
    const href = (await page.getByRole("link", { name: "esp32-dimmer", exact: true }).first().getAttribute("href"))!;
    await page.goto(app.url + href);
    await expect(page.locator("#identity")).toBeVisible();
    await expect(page.locator(".itiles")).toBeVisible();
    expect(await siblingCardGaps(page)).toEqual([]);
  });
  test(`device settings tab: cards stack under each other at ${width}px`, async ({ page }) => {
    await page.setExtraHTTPHeaders(adam);
    await page.setViewportSize({ width, height: 900 });
    await page.goto(app.url + "/devices");
    const href = (await page.getByRole("link", { name: "esp32-dimmer", exact: true }).first().getAttribute("href"))!;
    await page.goto(app.url + href + "/settings");
    await expect(page.locator("section.gcard").first()).toBeVisible({ timeout: 30_000 });
    expect(await cardStackGaps(page)).toEqual([]);
  });
}
