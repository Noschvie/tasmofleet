import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// #307: the status bar is fixed to the bottom of every page. It never scrolls away, nothing hides
// behind it, the phone shows one 44 px line that opens into a sheet, and the version it carries
// opens What's new.
let app: App;
test.beforeAll(async () => {
  app = await startApp({
    env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32", TASMOFLEET_ADMIN_GROUPS: "adm" },
  });
});
test.afterAll(async () => app?.stop());
test.use({ extraHTTPHeaders: { "X-authentik-username": "adam", "X-authentik-groups": "adm" } });

for (const [width, line] of [[1280, 32], [375, 44]] as const) {
  test(`the bar stays at the window's bottom edge while the page scrolls (${width}px)`, { tag: "@core" }, async ({ page }) => {
    await page.setViewportSize({ width, height: 600 });
    await page.goto(app.url + "/styleguide"); // a long page
    const bar = page.locator("footer.sb");
    await expect(bar).toBeVisible();
    const edge = async () => (await bar.boundingBox())!;
    const before = await edge();
    await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
    const after = await edge();
    expect(after.y).toBeCloseTo(before.y, 0);
    expect(after.y + after.height).toBeCloseTo(600, 0);
    expect(after.height).toBeGreaterThanOrEqual(line);
    // the last thing on the page ends above the bar
    const last = await page.evaluate(() => document.querySelector("main")!.getBoundingClientRect().bottom);
    expect(last).toBeLessThanOrEqual(after.y + 0.5);
  });
}

test("on a phone the bar is one line that opens into a sheet", { tag: "@core" }, async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 700 });
  await page.goto(app.url + "/devices");
  await expect(page.locator(".sb-wide")).toBeHidden();
  const sum = page.locator(".sb-narrow summary");
  await expect(sum).toContainText("All good");
  await sum.click();
  await expect(page.locator(".sb-sheet")).toContainText("Version");
  await page.locator(".sb-sheet").getByRole("link", { name: "What’s new" }).click();
  await expect(page).toHaveURL(/\/whats-new$/);
});

// Why: a page without a stream is a snapshot; the bar then makes no claim of being live.
test("a page without a stream has no Live item, a live page does", async ({ page }) => {
  await page.goto(app.url + "/users");
  await expect(page.locator("footer.sb .sb-live")).toHaveCount(0);
  await page.goto(app.url + "/devices");
  await expect(page.locator("footer.sb .sb-wide .sb-l-ok")).toBeVisible();
});

// Why: htmx's own throttle runs the first request of a burst and drops the rest, so the last state
// of a bulk command (its final "done") never showed and the bar pulsed "11 of 12" for hours. The
// refresh is leading and trailing: at most one per 2 s, and one after the last event.
test("a burst of job events ends with the bar showing the final state", { tag: "@core" }, async ({ page }) => {
  let n = 0;
  await page.route("**/statusbar", (route) => {
    n++;
    return route.fulfill({
      contentType: "text/html",
      body: `<div hx-swap-oob="innerHTML:#sb-wide"><span id="state">state ${n}</span></div>`,
    });
  });
  await page.goto(app.url + "/devices");
  await expect(page.locator("footer.sb[hx-get]")).toBeVisible();
  const sent: number[] = [];
  await page.route("**/statusbar", (route) => { sent.push(Date.now()); return route.fallback(); });
  // 12 events in under a second, as the stream delivers them for a bulk command
  await page.evaluate(async () => {
    const bar = document.querySelector("footer.sb")!;
    for (let i = 0; i < 12; i++) {
      bar.dispatchEvent(new CustomEvent("htmx:sseBeforeMessage", { bubbles: true, cancelable: true, detail: { type: "job" } }));
      await new Promise((r) => setTimeout(r, 60));
    }
  });
  const lastEvent = Date.now();
  // the trailing refresh arrives after the last event, so the final server state is what shows
  await expect(page.locator("#sb-wide #state")).toHaveText("state 2", { timeout: 5000 });
  expect(n).toBe(2); // one at once, one after the burst: never one per event
  expect(sent[sent.length - 1]).toBeGreaterThanOrEqual(lastEvent - 100);
});

test("a landscape phone's open sheet stays inside the window", async ({ page }) => {
  await page.setViewportSize({ width: 667, height: 375 });
  await page.goto(app.url + "/devices");
  await page.locator(".sb-narrow summary").click();
  const sheet = (await page.locator(".sb-sheet").boundingBox())!;
  expect(sheet.y).toBeGreaterThanOrEqual(0);
  if (process.env.SHOT_DIR) await page.screenshot({ path: `${process.env.SHOT_DIR}/landscape-sheet-667x375.png` });
});
