import { Locator, Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import {
  App,
  Devenv,
  freePort,
  startApp,
  startDevenv,
  writeTasmoAdminDir,
} from "../support/stack";

// Why: a saved column layout puts the table into the fixed layout (tfix), where
// every column gets a percentage share and every cell cuts off what does not
// fit. In v0.10.0 that cut the row "Actions" button of the devices list in half
// (the 7 % share of the Actions column is 84 px at a 1701 px window, the button
// needs 99 px) and hid the opened row menu completely: the menu is positioned
// inside the cell, and the cell clipped it, so a click did nothing visible. The
// row actions are the only way to reach Console, Restart, Back up and Delete, so
// under any saved layout and window width the button must be whole and the
// opened menu must be on top and clickable.
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

let app: App;
let env: Devenv;

test.beforeAll(async () => {
  test.setTimeout(240_000);
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_URL,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
    },
  });
  const pick = ["lamp-1m", "plug-4m"];
  app.cli([
    "import",
    "tasmoadmin",
    writeTasmoAdminDir(
      Object.fromEntries(pick.map((n) => [n, env.devices[n]])),
    ),
  ]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

async function postPref(
  page: Page,
  key: string,
  doc: unknown,
): Promise<number> {
  return page.evaluate(
    async ([k, d]) => {
      const token = JSON.parse(
        document.body.getAttribute("hx-headers") ?? "{}",
      )["X-CSRF-Token"];
      const r = await fetch(
        `${document.body.getAttribute("data-prefs")}/${k}`,
        {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            "X-CSRF-Token": token,
          },
          body: JSON.stringify(d),
        },
      );
      return r.status;
    },
    [key, doc] as const,
  );
}

/** The summary lies wholly inside its cell (nothing cut off) and is what a click at its centre hits. */
async function expectWhole(summary: Locator) {
  const r = await summary.evaluate((s) => {
    const b = s.getBoundingClientRect(),
      c = s.closest("td")!.getBoundingClientRect();
    const hit = document.elementFromPoint(
      b.left + b.width / 2,
      b.top + b.height / 2,
    );
    return {
      left: b.left - c.left,
      right: c.right - b.right,
      w: b.width,
      hits: !!hit && s.contains(hit),
    };
  });
  expect(r.w, "control has a width").toBeGreaterThan(10);
  expect(r.left, "control starts inside its cell").toBeGreaterThanOrEqual(0);
  expect(r.right, "control ends inside its cell").toBeGreaterThanOrEqual(0);
  expect(r.hits, "a click at the control's centre reaches it").toBe(true);
}

/** Opening the menu shows its items on top of everything: a click at an item's centre reaches that item. */
async function expectMenuUsable(summary: Locator, item: string) {
  await summary.click();
  const details = summary.locator("xpath=..");
  await expect(details).toHaveAttribute("open", "");
  const el = details.locator(".rowmenu-body").getByText(item, { exact: true });
  await expect(el).toBeVisible();
  const hit = await el.evaluate((e) => {
    const b = e.getBoundingClientRect();
    const x = b.left + b.width / 2,
      y = b.top + b.height / 2;
    const at = document.elementFromPoint(x, y);
    return {
      inView: x >= 0 && y >= 0 && x <= innerWidth && y <= innerHeight,
      hits: !!at && (e === at || e.contains(at)),
    };
  });
  expect(hit.inView, "menu item is inside the window").toBe(true);
  expect(
    hit.hits,
    "a click at the menu item reaches it (not cut off by the cell)",
  ).toBe(true);
  await summary.click();
}

// A live layout of v0.10.0 (the default shares, saved), and the narrowest
// layout the server accepts for the pinned columns (3 %).
const layouts: Record<string, Record<string, number>> = {
  live: {
    name: 19,
    status: 17,
    firmware: 12,
    family: 8,
    address: 13,
    uptime: 9,
    wifi: 9,
    power: 11,
    actions: 7,
  },
  narrowest: {
    name: 40,
    status: 17,
    firmware: 12,
    family: 8,
    address: 13,
    uptime: 9,
    wifi: 9,
    power: 3,
    actions: 3,
  },
};

for (const width of [1700, 1280]) {
  for (const [name, w] of Object.entries(layouts)) {
    test(`devices: the row Actions button is whole and its menu usable at ${width} px with the ${name} saved layout`, async ({
      page,
    }) => {
      await page.setViewportSize({ width, height: 900 });
      await page.setExtraHTTPHeaders(admin);
      await page.goto(app.url + "/devices");
      expect(await postPref(page, "table.devices", { w })).toBe(204);
      await page.goto(app.url + "/devices");
      const table = page.locator("table[data-table=devices]");
      await expect(table).toHaveClass(/\btfix\b/);
      const row = page.getByRole("row").filter({ hasText: "lamp-1m" });
      await expect(row.getByText("Online", { exact: true })).toBeVisible({
        timeout: 40_000,
      });
      // Power: the relay button is whole too
      const relay = row.locator("td.p2 .pwr").first();
      await expect(relay).toBeVisible();
      await expectWhole(relay);
      const summary = row.locator("td.rowacts summary");
      await expectWhole(summary);
      await expectMenuUsable(summary, "Open console");
      // the checkbox column keeps its box whole as well
      await expectWhole(row.locator("td.sel input[type=checkbox]"));
    });
  }
}
