import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// What's new (design v2, N5): the avatar's dot says the changelog has releases the user has not
// seen; opening the page shows what was new and clears the dot.

let app: App;
test.beforeAll(async () => {
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
    },
  });
});
test.afterAll(async () => app?.stop());

// Why: a user who never opened What's new gets one hint; the page still marks the release that
// was new on this visit, and afterwards the hint is gone everywhere until the next release.
test("the avatar dot shows on the first visit and goes once What's new was opened", async ({ page }) => {
  await page.setExtraHTTPHeaders({ "X-authentik-username": "olivia", "X-authentik-groups": "ops" });
  await page.goto(app.url + "/devices");
  await expect(page.locator(".tb .avatar-dot")).toBeVisible();
  await page.locator(".tb summary.um-btn").click();
  const item = page.locator(".um-menu .um-news");
  await expect(item).toContainText("1 new since last visit");
  await item.click();
  await expect(page).toHaveURL(/\/whats-new$/);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("What’s new in TasmoFleet");
  await expect(page.locator(".wn-rel").first()).toContainText("New since your last visit");
  await expect(page.locator(".tb .avatar-dot")).toHaveCount(0);

  await page.goto(app.url + "/devices");
  await expect(page.locator(".tb .avatar-dot")).toHaveCount(0);
  await page.goto(app.url + "/whats-new");
  await expect(page.getByText("New since your last visit")).toHaveCount(0);
});

// Why: About's "What's new in x.y.z" and its Changelog link open the changelog of this binary in
// the app instead of the repository's newest one on GitHub.
test("About leads to What's new", async ({ page }) => {
  await page.setExtraHTTPHeaders({ "X-authentik-username": "adam", "X-authentik-groups": "adm" });
  await page.goto(app.url + "/about");
  await page.getByRole("link", { name: /^What’s new in / }).click();
  await expect(page).toHaveURL(/\/whats-new$/);
  await expect(page.locator(".crumbs")).toContainText("About");
});

// Why (#419-S): below 1200 px the stacks are two half columns, which set 0.28.0 beside 0.11.x and put
// "You are on" after the last release. The page is one column there, the running version first.
for (const width of [768, 1024]) {
  test(`What's new is one column with the running version first at ${width}px`, async ({ page }) => {
    await page.setExtraHTTPHeaders({ "X-authentik-username": "olivia", "X-authentik-groups": "ops" });
    await page.setViewportSize({ width, height: 900 });
    await page.goto(app.url + "/whats-new");
    const m = await page.evaluate(() => {
      const r = (e: Element) => e.getBoundingClientRect();
      return {
        you: r(document.querySelector(".wn-you")!),
        jump: r(document.querySelector(".wn-jump")!),
        rels: [...document.querySelectorAll(".wn-rel")].map((e) => ({ top: r(e).top, width: r(e).width })),
        column: r(document.querySelector(".wn-stacks .cstacks")!).width,
      };
    });
    expect(m.rels.length).toBeGreaterThan(2);
    for (let i = 1; i < m.rels.length; i++) expect(m.rels[i].top, `release ${i} lies below release ${i - 1}`).toBeGreaterThan(m.rels[i - 1].top);
    expect(m.you.top, "You are on comes first").toBeLessThan(m.jump.top);
    expect(m.jump.top, "the jump list comes before the releases").toBeLessThan(m.rels[0].top);
    // one column: every card as wide as the column, not as its content
    expect([m.you.width, m.jump.width, m.rels[0].width]).toEqual([m.column, m.column, m.column]);
  });
}

// Why: a jump to a release scrolls it under the sticky header, and the sticky narrow stack starts
// under that header too: neither may be hidden by it.
test("a jump to an older release and the sticky stack stay below the header", async ({ page }) => {
  await page.setExtraHTTPHeaders({ "X-authentik-username": "olivia", "X-authentik-groups": "ops" });
  await page.setViewportSize({ width: 1280, height: 720 });
  await page.goto(app.url + "/whats-new");
  const target = page.locator(".wn-jump a").nth(5);
  const id = (await target.getAttribute("href"))!;
  await target.click();
  await page.waitForTimeout(500);
  const m = await page.evaluate((sel) => ({
    header: document.querySelector("header.top")!.getBoundingClientRect().bottom,
    target: document.getElementById(sel.slice(1))!.getBoundingClientRect().top,
    you: document.querySelector(".wn-you")!.getBoundingClientRect().top,
  }), id);
  expect(m.target, "the release is under the header").toBeGreaterThanOrEqual(m.header);
  expect(m.you, "the sticky stack is under the header").toBeGreaterThanOrEqual(m.header);
});
