import { test, expect } from "../support/fixtures";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// #289: the Identity card shows the StatusFWR fields of Status 2 under Tasmota's names, apart from
// TasmoFleet's own reading, and the header names the hostname. #308: the Settings tab comes at once
// and fetches the device's settings into itself.
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

// The first test refreshes the identity, which may rename the device, so the id is looked up once
// by its imported name and reused.
let deviceId = "";
async function openDevice(page: import("@playwright/test").Page): Promise<string> {
  await page.setExtraHTTPHeaders(adam);
  if (deviceId) {
    await page.goto(`${app.url}/devices/${deviceId}`);
    return deviceId;
  }
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: "plug-4m-http", exact: true }).click();
  deviceId = page.url().match(/devices\/(\d+)/)![1];
  return deviceId;
}

test("identity card: StatusFWR under Tasmota's names, TasmoFleet's reading apart", { tag: "@core" }, async ({ page }) => {
  await openDevice(page);
  await page.getByRole("button", { name: "Refresh identity" }).click();
  const card = page.locator("#identity");
  await expect(card.getByText("Identity refreshed.")).toBeVisible();
  for (const name of ["Version", "BuildDateTime", "Core", "SDK", "CpuFrequency", "Hardware", "CR"]) {
    await expect(card.locator("dt", { hasText: new RegExp(`^${name}\\b`) })).toBeVisible();
  }
  await expect(card.locator("button.uinfo")).toHaveCount(7);
  // the explanation sits in a popover next to its icon (#680), not in a title attribute
  await expect(card.locator("dt", { hasText: /^CR/ }).locator(".uinfo-pop")).toHaveText(/settings text area/i);
  await expect(card.locator("dt", { hasText: /^CR/ }).locator(".uinfo-pop")).toBeHidden();
  const reading = card.getByRole("heading", { name: /TasmoFleet's reading/ });
  await expect(reading).toBeVisible();
  await expect(card.locator("dt", { hasText: /^Variant/ })).toBeVisible();
  // the header names the hostname the device reported
  await expect(page.locator(".devhd-meta")).toContainText(/\S+/);
});

test("settings tab: the tab comes at once with a loading state, then the form", { tag: "@core" }, async ({ page }) => {
  const id = await openDevice(page);
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  await page.route(`**/devices/${id}/settings/form`, async (route) => {
    await gate; // a slow device
    await route.continue();
  });
  await page.getByRole("link", { name: "Settings", exact: true }).click();
  await expect(page.getByRole("link", { name: "Settings", exact: true })).toHaveAttribute("aria-current", "page");
  await expect(page.getByRole("heading", { name: /Reading the settings/ })).toBeVisible();
  await expect(page.locator("input[name=TelePeriod]")).toHaveCount(0);
  release();
  await expect(page.locator("input[name=TelePeriod]")).toBeVisible();
  await expect(page.getByRole("heading", { name: /Reading the settings/ })).toHaveCount(0);
});

// #680: the info icons of "Reported by the device" sat below their label and a tap on them showed
// nothing (the text was a title attribute, and the tooltip script reacts to a mouse or keyboard
// focus only). On a phone there was no way to read the explanation. Widths: 375 and 412 are the
// phones of the report; 1280 checks that the desktop alignment holds as well.
async function touchDevice(browser: import("@playwright/test").Browser, width: number) {
  const context = await browser.newContext({
    viewport: { width, height: 900 },
    hasTouch: width < 1000,
    isMobile: width < 1000,
    extraHTTPHeaders: adam,
  });
  const page = await context.newPage();
  // Known limitation, not covered here: the page refreshes its live region when the event stream is up and
  // again a few seconds later; that swaps the Stability and Activity cards, moves the layout and scrolls
  // the page, and ui.Info closes an open popover on any scroll (it is fixed to the viewport). A popover
  // opened in the first seconds after load can therefore close by itself. These tests check the icons,
  // not that, so the page keeps its first state: the refresh requests are refused.
  await page.route(/\/devices\/\d+\/live\?/, (route) => route.abort());
  await page.goto(`${app.url}/devices/${deviceId}`);
  // the identity is refreshed by the first test; read it here if this test runs alone
  if ((await page.locator("#identity dt", { hasText: /^CR\b/ }).count()) === 0) {
    await page.getByRole("button", { name: "Refresh identity" }).click();
    await expect(page.locator("#identity dt", { hasText: /^CR\b/ })).toBeVisible();
  }
  return { context, page };
}

for (const width of [375, 412]) {
  test(`identity card at ${width}: a tap on an info icon shows its text, a tap elsewhere hides it`, async ({ browser, page: plain }) => {
    await openDevice(plain);
    const { context, page } = await touchDevice(browser, width);
    try {
      const card = page.locator("#identity");
      const icon = (name: RegExp) => card.locator("dt", { hasText: name }).getByRole("button");
      const cr = page.getByText("Characters used / available in the settings text area");
      const core = page.getByText("Version of the Arduino core");
      await expect(cr).toBeHidden();
      await icon(/^CR\b/).tap();
      await expect(cr, "the explanation of CR is shown after a tap").toBeVisible();
      // the open icon is drawn as a 16px circle; a filled touch box would cover the end of its label ("CR")
      await expect(icon(/^CR\b/)).toHaveAttribute("aria-expanded", "true");
      await expect(icon(/^CR\b/), "the touch box of an open icon is not filled").toHaveCSS("background-color", "rgba(0, 0, 0, 0)");
      // a second tap on the same icon closes it
      await icon(/^CR\b/).tap();
      await expect(cr).toBeHidden();
      // a tap on another icon closes the first and opens its own
      await icon(/^CR\b/).tap();
      await expect(cr).toBeVisible();
      await icon(/^Core\b/).tap();
      await expect(core).toBeVisible();
      await expect(cr, "one popover at a time").toBeHidden();
      // a tap elsewhere closes it
      await card.getByRole("heading", { name: "Identity" }).tap();
      await expect(core).toBeHidden();
    } finally {
      await context.close();
    }
  });
}

for (const width of [375, 412, 1280]) {
  test(`identity card at ${width}: each info icon is centred on the first line of its label`, async ({ browser, page: plain }) => {
    await openDevice(plain);
    const { context, page } = await touchDevice(browser, width);
    try {
      const rows = await page.locator("#identity dt").evaluateAll((dts) =>
        dts
          .map((dt) => {
            const btn = dt.querySelector("button");
            const label = [...dt.childNodes].find((n) => n.nodeType === Node.TEXT_NODE && n.textContent!.trim() !== "");
            if (!btn || !label) return null;
            const range = document.createRange();
            range.selectNodeContents(label);
            const line = range.getClientRects()[0];
            const b = btn.getBoundingClientRect();
            return { name: label.textContent!.trim(), line: line.top + line.height / 2, icon: b.top + b.height / 2 };
          })
          .filter((r) => r !== null),
      );
      expect(rows.length, "one icon per StatusFWR field").toBe(7);
      for (const r of rows) {
        expect(Math.abs(r!.icon - r!.line), `${r!.name}: icon centre ${r!.icon.toFixed(1)} vs label line centre ${r!.line.toFixed(1)}`).toBeLessThanOrEqual(1);
      }
    } finally {
      await context.close();
    }
  });
}
