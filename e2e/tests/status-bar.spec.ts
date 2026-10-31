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
