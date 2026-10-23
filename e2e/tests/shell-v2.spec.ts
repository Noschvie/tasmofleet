import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// Design v2 shell (WP F3, design/v2/README.md): the live indicator and the
// connection banners follow the real event stream, the navigation works without JavaScript, the
// theme choice sticks, and the icons are the TasmoFleet mark.

let app: App;
test.beforeAll(async () => {
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
    },
  });
});
test.afterAll(async () => app?.stop());

const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

async function liveState(page: Page): Promise<string | null> {
  return page.evaluate(() =>
    document.documentElement.getAttribute("data-live"),
  );
}

// Why: when the stream breaks, values on the page silently go stale; the prototype says so in
// the top bar and with a banner, first "Reconnecting", then "Offline" with Retry now, and says
// "Live" again once the stream is back.
test("a broken event stream shows Reconnecting, then Offline, and Live once it is back", async ({
  page,
}) => {
  test.setTimeout(90_000);
  await page.setExtraHTTPHeaders(admin);
  let refuse = true;
  await page.route("**/events", (route) =>
    refuse ? route.fulfill({ status: 503, body: "down" }) : route.fallback(),
  );
  await page.goto(app.url + "/about");
  const re = page.locator(".conn-banner.conn-re");
  const off = page.locator(".conn-banner.conn-off");
  await expect(re).toBeVisible();
  await expect(re).toContainText("Live updates interrupted.");
  await expect(page.locator(".tb .live-re")).toBeVisible();
  await expect(page.locator(".tb .live-ok")).toBeHidden();
  // the extension retries after 0.5, 1, 2, 4 s: the fifth failure in a row is "Offline"
  await expect(off).toBeVisible({ timeout: 20_000 });
  await expect(re).toBeHidden();
  await expect(off).toContainText("Connection to the server lost.");
  await expect(page.locator(".tb .live-off")).toBeVisible();
  // Retry now reloads the page when the server answers; the stream is still refused, so it
  // starts over with "Reconnecting"
  await page.getByRole("button", { name: "Retry now" }).click();
  await page.waitForLoadState("domcontentloaded");
  await expect(re).toBeVisible();
  refuse = false;
  await expect.poll(() => liveState(page), { timeout: 20_000 }).toBe("live");
  await expect(re).toBeHidden();
  await expect(off).toBeHidden();
  await expect(page.locator(".tb .live-ok")).toBeVisible();
});

// Why (v0.11.2, live banner): a stream that drops and comes straight back (a proxy closing it, a
// server restart) loses nothing worth telling the user about; flashing "Live updates interrupted"
// for every such drop trains them to ignore it. Only a stream that stays down past the grace period
// (or several failed reconnects) is news. And when it shows, the banner floats over the page: the
// table under it must not jump ("no layout jumps").
test("a quick reconnect is silent and the banner never moves the page", async ({
  page,
}) => {
  test.setTimeout(60_000);
  await page.setExtraHTTPHeaders(admin);
  await page.clock.install();
  await page.goto(app.url + "/about");
  await expect.poll(() => liveState(page)).toBe("live");
  const re = page.locator(".conn-banner.conn-re");
  const heading = page.locator("#main h1").first();
  const top = async () => (await heading.boundingBox())!.y;
  const before = await top();
  const sseError = () =>
    page.evaluate(() =>
      document.body.dispatchEvent(
        new CustomEvent("htmx:sseError", {
          bubbles: true,
          detail: { source: { readyState: 0 } },
        }),
      ),
    );
  // one drop, back within the grace period: nothing shows
  await sseError();
  await page.clock.runFor(10_000);
  await expect(re).toBeHidden();
  expect(await liveState(page)).toBe("live");
  await page.evaluate(() =>
    document.body.dispatchEvent(new CustomEvent("htmx:sseOpen", { bubbles: true })),
  );
  await page.clock.runFor(20_000);
  await expect(re).toBeHidden();
  // a drop that lasts past the grace period shows the banner, over the page
  await sseError();
  await page.clock.runFor(16_000);
  await expect(re).toBeVisible();
  expect(await liveState(page)).toBe("reconnecting");
  expect(await top()).toBe(before);
  // back: hidden again, still no shift
  await page.evaluate(() =>
    document.body.dispatchEvent(new CustomEvent("htmx:sseOpen", { bubbles: true })),
  );
  await expect(re).toBeHidden();
  expect(await top()).toBe(before);
});

// Why: the phone menu button is a link to /nav and the user menu is a <details>; without
// JavaScript both must still get the user to every page and to Sign out.
test("navigation works without JavaScript", async ({ browser }) => {
  const context = await browser.newContext({
    javaScriptEnabled: false,
    extraHTTPHeaders: admin,
    viewport: { width: 375, height: 800 },
  });
  const page = await context.newPage();
  try {
    await page.goto(app.url + "/about");
    await page.getByLabel("Open navigation", { exact: true }).click();
    await expect(page).toHaveURL(/\/nav$/);
    await page.locator("main").getByRole("link", { name: "Audit log" }).click();
    await expect(page).toHaveURL(/\/audit$/);
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.locator("nav.tb-groups a", { hasText: "System" }).click();
    await page
      .locator("nav.sn-in")
      .getByRole("link", { name: "About", exact: true })
      .click();
    await expect(page).toHaveURL(/\/about$/);
    await page.locator("details.usermenu > summary").click();
    await expect(
      page
        .locator("details.usermenu")
        .getByRole("button", { name: "Sign out" }),
    ).toBeVisible();
  } finally {
    await context.close();
  }
});

// Why: on a phone the drawer replaces the top bar's groups; it opens over the page and closes
// with Escape like any dialog.
test("the phone drawer opens from the menu button and closes with Escape", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 375, height: 800 });
  await page.goto(app.url + "/about");
  await expect(page.locator(".tb")).toBeHidden();
  await page.getByLabel("Open navigation", { exact: true }).click();
  const drawer = page.locator("dialog#nav-drawer");
  await expect(drawer).toBeVisible();
  await expect(drawer.getByRole("link", { name: "About" })).toHaveAttribute(
    "aria-current",
    "page",
  );
  await page.keyboard.press("Escape");
  await expect(drawer).toBeHidden();
});

// Why: D11: the choice applies at once, is stored per user and rendered by the server on the
// next page (no flash); Auto follows the system again.
test("the theme choice applies at once and sticks", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.emulateMedia({ colorScheme: "light" });
  await page.goto(app.url + "/about");
  await page.locator("details.usermenu > summary").click();
  const save = page.waitForResponse(
    (r) => r.url().endsWith("/prefs/theme") && r.request().method() === "POST",
  );
  await page.locator(".um-menu").getByRole("button", { name: "Dark" }).click();
  expect((await save).status()).toBe(204);
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await expect(
    page
      .locator(".um-menu")
      .getByRole("button", { name: "Dark", includeHidden: true }),
  ).toHaveAttribute("aria-pressed", "true");
  await page.locator("details.usermenu > summary").click();
  const back = page.waitForResponse((r) => r.url().endsWith("/prefs/theme"));
  await page.locator(".um-menu").getByRole("button", { name: "Auto" }).click();
  await back;
  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("data-theme-auto", "");
  await expect(page.locator("html")).not.toHaveAttribute("data-theme", /./);
});

/** pngSize reads width and height from a PNG's IHDR chunk. */
function pngSize(b: Buffer): [number, number] {
  expect(b.subarray(1, 4).toString()).toBe("PNG");
  return [b.readUInt32BE(16), b.readUInt32BE(20)];
}

// Why: D16: the favicon, the home-screen icon and the install icons are the TasmoFleet mark;
// browsers and home screens fetch them by type and size, without a session.
test("favicon, manifest and app icons are the TasmoFleet mark", async ({
  request,
}) => {
  const ico = await request.get(app.url + "/favicon.ico");
  expect(ico.status()).toBe(200);
  expect(ico.headers()["content-type"]).toBe("image/x-icon");
  const icoBody = await ico.body();
  expect(icoBody.readUInt16LE(2), "ICO type").toBe(1);
  expect(icoBody.readUInt16LE(4), "16, 32 and 48 px").toBe(3);
  const svg = await request.get(app.url + "/static/favicon.svg");
  expect(svg.headers()["content-type"]).toContain("image/svg+xml");
  expect(await svg.text()).toContain('fill="#1f63a8"');
  const touch = await request.get(app.url + "/static/apple-touch-icon.png");
  expect(touch.headers()["content-type"]).toBe("image/png");
  expect(pngSize(await touch.body())).toEqual([180, 180]);
  const man = await request.get(app.url + "/static/manifest.webmanifest");
  expect(man.headers()["content-type"]).toContain("application/manifest+json");
  const m = (await man.json()) as {
    name: string;
    icons: { src: string; sizes: string; type: string }[];
  };
  expect(m.name).toBe("TasmoFleet");
  for (const i of m.icons) {
    const r = await request.get(app.url + "/static/" + i.src);
    expect(r.status(), i.src).toBe(200);
    expect(r.headers()["content-type"]).toBe(i.type);
    const n = Number(i.sizes.split("x")[0]);
    expect(pngSize(await r.body()), i.src).toEqual([n, n]);
  }
});

// Why: D15: the top bar, the drawer and the tab title name the product "TasmoFleet".
test("the top bar names the product TasmoFleet", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/about");
  await expect(page.locator("header .brand")).toHaveText("TasmoFleet");
  await expect(page).toHaveTitle("About - TasmoFleet");
});
