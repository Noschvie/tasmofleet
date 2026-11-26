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
// the status bar (#307) and with a banner, first "Reconnecting", then "Offline" with Retry now, and says
// "Live" again once the stream is back.
test("a broken event stream shows Reconnecting, then Offline, and Live once it is back", async ({
  page,
}) => {
  test.setTimeout(90_000);
  await page.setExtraHTTPHeaders(admin);
  let refuse = true;
  await page.route("**/events?since=*", (route) =>
    refuse ? route.fulfill({ status: 503, body: "down" }) : route.fallback(),
  );
  await page.goto(app.url + "/about");
  const re = page.locator(".conn-banner.conn-re");
  const off = page.locator(".conn-banner.conn-off");
  await expect(re).toBeVisible();
  await expect(re).toContainText("Reconnecting…");
  await expect(page.locator(".sb-wide .sb-l-re")).toBeVisible();
  await expect(page.locator(".sb-wide .sb-l-ok")).toBeHidden();
  // the extension retries after 0.5, 1, 2, 4 s: the fifth failure in a row is "Offline"
  await expect(off).toBeVisible({ timeout: 20_000 });
  await expect(re).toBeHidden();
  await expect(off).toContainText("Connection to the server lost.");
  await expect(page.locator(".sb-wide .sb-l-off")).toBeVisible();
  // Retry now reloads the page when the server answers; the stream is still refused, so it
  // starts over with "Reconnecting"
  await page.getByRole("button", { name: "Retry now" }).click();
  await page.waitForLoadState("domcontentloaded");
  await expect(re).toBeVisible();
  refuse = false;
  await expect.poll(() => liveState(page), { timeout: 20_000 }).toBe("live");
  await expect(re).toBeHidden();
  await expect(off).toBeHidden();
  await expect(page.locator(".sb-wide .sb-l-ok")).toBeVisible();
});

// Why (v0.11.2, live banner; #1089): a stream that drops and comes straight back (a proxy closing it, a
// server restart) loses nothing worth telling the user about; flashing "Reconnecting" for every such
// drop trains them to ignore it. Only a stream that stays down past the grace period (or several failed
// reconnects) is news. And when it shows, the banner sits in the page flow: the page below it moves down
// once when the banner shows and once when it goes (it used to float over the title), and never between
// Reconnecting and Offline, where the banners share one reserved space.
test("a quick reconnect is silent; a banner pushes the page down once and never covers the title", async ({
  page,
}) => {
  test.setTimeout(60_000);
  await page.setExtraHTTPHeaders(admin);
  await page.clock.install();
  await page.goto(app.url + "/about");
  await expect.poll(() => liveState(page)).toBe("live");
  const re = page.locator(".conn-banner.conn-re");
  const off = page.locator(".conn-banner.conn-off");
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
  const sseOpen = () =>
    page.evaluate(() =>
      document.body.dispatchEvent(new CustomEvent("htmx:sseOpen", { bubbles: true })),
    );
  // one drop, back within the grace period: nothing shows, nothing moves
  await sseError();
  await page.clock.runFor(10_000);
  await expect(re).toBeHidden();
  expect(await liveState(page)).toBe("live");
  await sseOpen();
  await page.clock.runFor(20_000);
  await expect(re).toBeHidden();
  expect(await top()).toBe(before);
  // a drop that lasts past the grace period shows the banner above the title, which moves down
  await sseError();
  await page.clock.runFor(16_000);
  await expect(re).toBeVisible();
  expect(await liveState(page)).toBe("reconnecting");
  const bannerBox = (await re.boundingBox())!;
  const reconnecting = await top();
  expect(reconnecting).toBeGreaterThanOrEqual(bannerBox.y + bannerBox.height);
  // Retry now belongs to the other banner: it must not be reachable while this one shows
  await expect(off.getByRole("button", { name: "Retry now" })).toBeHidden();
  // Offline: the other banner takes the same space, the title stays where it is
  for (let i = 0; i < 4; i++) await sseError();
  await expect.poll(() => liveState(page)).toBe("offline");
  await expect(off).toBeVisible();
  await expect(re).toBeHidden();
  expect(await top()).toBe(reconnecting);
  // back: hidden again, the title is where it was before
  await sseOpen();
  await expect(off).toBeHidden();
  expect(await top()).toBe(before);
});

// Why (#1089, WCAG 2.2.2 and 4.1.3): the Offline banner is an alert region; a countdown that changes
// every second inside it makes a screen reader read it out again every second. The countdown stays on
// screen but is aria-hidden, so over several seconds of ticking nothing the region announces changes.
test("the Offline banner's announced text does not tick", async ({ page }) => {
  test.setTimeout(60_000);
  await page.setExtraHTTPHeaders(admin);
  await page.clock.install();
  await page.goto(app.url + "/about");
  await expect.poll(() => liveState(page)).toBe("live");
  await page.evaluate(() => {
    for (let i = 0; i < 5; i++)
      document.body.dispatchEvent(
        new CustomEvent("htmx:sseError", {
          bubbles: true,
          detail: { source: { readyState: 0 } },
        }),
      );
  });
  await expect.poll(() => liveState(page)).toBe("offline");
  const countdown = page.locator(".conn-off [data-live-next]");
  await expect(countdown).toBeVisible();
  // record every change below the alert that an assistive technology would hear
  await page.evaluate(() => {
    const w = window as unknown as { heard: string[] };
    w.heard = [];
    const alert = document.querySelector(".conn-off")!;
    new MutationObserver((list) => {
      for (const m of list) {
        const at = m.target instanceof Element ? m.target : m.target.parentElement;
        if (!at?.closest('[aria-hidden="true"]')) w.heard.push(m.type + " " + (m.target.textContent ?? ""));
      }
    }).observe(alert, { subtree: true, childList: true, characterData: true, attributes: true });
  });
  const shown = async () => countdown.textContent();
  const first = await shown();
  await page.clock.runFor(3_000);
  expect(await shown()).not.toBe(first); // it does tick for the eye
  expect(await page.evaluate(() => (window as unknown as { heard: string[] }).heard)).toEqual([]);
  await expect(page.locator(".conn-off")).toContainText("Retrying automatically.");
});

// Why (#1089): the dashboard's subtitle said "state streams in live" as fixed text, so it claimed a live
// connection while the banner said the connection was lost. It counts devices only; while the stream is
// down it adds the time of the last values (the same time the status bar shows), and it is gone again
// when the stream is back.
test("the dashboard subtitle stops saying live while the stream is down", async ({ page }) => {
  test.setTimeout(60_000);
  await page.setExtraHTTPHeaders(admin);
  await page.clock.install();
  await page.goto(app.url + "/");
  await expect.poll(() => liveState(page)).toBe("live");
  const sub = page.locator("#main .pghead-sub");
  // what the person reads, so the hidden "Values from" part does not count
  const said = () => sub.innerText();
  await expect(sub).toContainText(/\d+ devices?/);
  expect(await said()).not.toMatch(/live/i);
  expect(await said()).not.toContain("Values from");
  await page.evaluate(() => {
    for (let i = 0; i < 3; i++)
      document.body.dispatchEvent(
        new CustomEvent("htmx:sseError", { bubbles: true, detail: { source: { readyState: 0 } } }),
      );
  });
  await expect(page.locator("html")).toHaveAttribute("data-live", "reconnecting");
  await expect(page.locator("#main .pghead-stale")).toBeVisible();
  expect(await said()).toMatch(/^\d+ devices? · Values from \d\d:\d\d$/);
  await page.evaluate(() =>
    document.body.dispatchEvent(new CustomEvent("htmx:sseOpen", { bubbles: true })),
  );
  await expect(page.locator("html")).toHaveAttribute("data-live", "live");
  await expect(page.locator("#main .pghead-stale")).toBeHidden();
  expect(await said()).not.toContain("Values from");
});

// Why (#1089): the banner floated over the page and covered the title and subtitle at 1280 px and the
// crumbs, title and subtitle at 375 px, exactly while the person needs to read what is wrong.
for (const width of [375, 1280]) {
  test(`dashboard at ${width} px: the connection banner does not cover the page header`, async ({
    page,
  }) => {
    test.setTimeout(60_000);
    await page.setViewportSize({ width, height: 800 });
    await page.setExtraHTTPHeaders(admin);
    await page.clock.install();
    await page.goto(app.url + "/");
    await expect.poll(() => liveState(page)).toBe("live");
    await page.evaluate(() => {
      for (let i = 0; i < 3; i++)
        document.body.dispatchEvent(
          new CustomEvent("htmx:sseError", { bubbles: true, detail: { source: { readyState: 0 } } }),
        );
    });
    await expect(page.locator("html")).toHaveAttribute("data-live", "reconnecting");
    const banner = (await page.locator(".conn-banner.conn-re").boundingBox())!;
    const header = (await page.locator("#main .pghead").boundingBox())!;
    expect(banner.y + banner.height, "the banner ends above the page header").toBeLessThanOrEqual(header.y);
    const title = (await page.locator("#main h1").boundingBox())!;
    expect(banner.y + banner.height, "the banner ends above the title").toBeLessThanOrEqual(title.y);
  });
}

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

// Why (#1077, A1-007): a keyboard user had to Tab through the brand, four groups, the sub-navigation and the account
// menu on every page before reaching the content (WCAG 2.4.1). The first Tab stop is a link that says so, and
// following it puts the focus into <main>, so the next Tab continues in the page, not back in the header.
test("the first Tab focuses Skip to content and Enter moves focus into main", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/about");
  await page.keyboard.press("Tab");
  const skip = page.getByRole("link", { name: "Skip to content" });
  await expect(skip).toBeFocused();
  const box = (await skip.boundingBox())!;
  expect(box.width, "the link is seen once it has focus").toBeGreaterThan(40);
  expect(box.y, "the link is on screen, over the header").toBeGreaterThanOrEqual(0);
  await page.keyboard.press("Enter");
  expect(await page.evaluate(() => document.activeElement?.id)).toBe("main");
  await page.keyboard.press("Tab");
  expect(
    await page.evaluate(() => !!document.activeElement?.closest("main")),
    "the next Tab continues inside the page content",
  ).toBe(true);
});

// Why (#1077, A1-008): the phone drawer covers the page; on a phone there is no Escape key and nothing said that a tap
// on the scrim closes it. It has a close button in its head.
test("at 375 the drawer has a visible close button that closes it", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 375, height: 800 });
  await page.goto(app.url + "/about");
  await page.getByLabel("Open navigation", { exact: true }).click();
  const drawer = page.locator("dialog#nav-drawer");
  await expect(drawer).toBeVisible();
  const close = drawer.getByRole("button", { name: "Close navigation" });
  await expect(close).toBeVisible();
  const box = (await close.boundingBox())!;
  expect(Math.min(box.width, box.height), "a touch target").toBeGreaterThanOrEqual(44);
  await close.click();
  await expect(drawer).toBeHidden();
});

// Why (#1077, A1-008): Account, Theme and Sign out sat at the end of a long scrolling dialog, below the fold on a
// short phone. The foot stays at the bottom of the drawer while the list scrolls.
test("at 375 the drawer's Sign out stays in view with the list scrolled", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 375, height: 520 });
  await page.goto(app.url + "/about");
  await page.getByLabel("Open navigation", { exact: true }).click();
  const drawer = page.locator("dialog#nav-drawer");
  await expect(drawer).toBeVisible();
  const signOut = drawer.getByRole("button", { name: "Sign out" });
  await expect(signOut).toBeInViewport({ ratio: 1 });
  await drawer.evaluate((el) => (el.scrollTop = 0));
  await expect(signOut).toBeInViewport({ ratio: 1 });
  await drawer.evaluate((el) => (el.scrollTop = el.scrollHeight));
  await expect(signOut).toBeInViewport({ ratio: 1 });
});

// Why (#1077, A1-008, WCAG 2.4.11 Focus Not Obscured): the foot stays in view at the bottom of the drawer, so a link
// that keyboard focus scrolls to must end above it; a sticky foot laid over the scrolling dialog hid the last links.
// A short screen must not hide a link: the list keeps a floor of three rows and the dialog scrolls when even that
// does not fit (landscape phone, 400 % zoom); focus must land inside the list's visible box and above the foot.
for (const size of [
  { width: 375, height: 520 },
  { width: 568, height: 320 },
  { width: 320, height: 256 },
]) {
  test(`at ${size.width} x ${size.height} no focused drawer link is hidden by the foot or outside the list`, async ({ page }) => {
    await page.setExtraHTTPHeaders(admin);
    await page.setViewportSize(size);
    await page.goto(app.url + "/about");
    await page.getByLabel("Open navigation", { exact: true }).click();
    const drawer = page.locator("dialog#nav-drawer");
    await expect(drawer).toBeVisible();
    const links = await drawer.locator("nav a").count();
    expect(links, "the drawer lists the pages").toBeGreaterThan(8);
    const hidden: string[] = [];
    for (let i = 0; i < links + 1; i++) {
      const r = await page.evaluate(() => {
        const a = document.activeElement as HTMLElement | null;
        const foot = document.querySelector("dialog#nav-drawer .dr-foot");
        const nav = document.querySelector("dialog#nav-drawer .dr > nav");
        if (!a || !foot || !nav || !a.closest("nav")) return null;
        const ar = a.getBoundingClientRect();
        const nr = nav.getBoundingClientRect();
        return { name: a.textContent?.trim() ?? "", top: ar.top, bottom: ar.bottom, navTop: nr.top, navBottom: nr.bottom, footTop: foot.getBoundingClientRect().top };
      });
      if (r && (r.bottom > r.footTop + 0.5 || r.top < r.navTop - 0.5 || r.bottom > r.navBottom + 0.5)) hidden.push(r.name);
      await page.keyboard.press("Tab");
    }
    expect(hidden, "links focused under the foot or outside the list").toEqual([]);
  });
}

// Why (#1077, A1-009): the avatar was labelled "Account and theme" and opened the whole navigation, the same as the
// burger. It opens the account menu's content in a sheet and no navigation; the burger keeps the navigation.
test("at 375 the avatar opens the account sheet with Account, Theme and Sign out and no navigation link", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 375, height: 800 });
  await page.goto(app.url + "/about");
  await page.getByRole("link", { name: "Account", exact: true }).first().click();
  const sheet = page.locator("dialog#account-sheet");
  await expect(sheet).toBeVisible();
  await expect(page.locator("dialog#nav-drawer")).toBeHidden();
  await expect(sheet.getByRole("link", { name: "Account", exact: true })).toBeVisible();
  await expect(sheet.getByRole("group", { name: "Theme" })).toBeVisible();
  await expect(sheet.getByRole("button", { name: "Sign out" })).toBeVisible();
  await expect(sheet.locator(".um-role")).toHaveText("Admin");
  await expect(sheet.getByRole("link", { name: "Devices", exact: true })).toHaveCount(0);
  await expect(sheet.getByRole("navigation")).toHaveCount(0);
  await page.keyboard.press("Escape");
  await expect(sheet).toBeHidden();
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
