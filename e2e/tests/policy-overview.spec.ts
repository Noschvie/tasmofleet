import { test, expect, type Page } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// The policy overview (#197 WP5): the rule table's order is the precedence. Drag, the menu's Move
// entries without JavaScript, the disabled row and the old links that are no longer a page.
let app: App;
test.beforeAll(async () => {
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_POLICY_RULES: JSON.stringify([
        { name: "Rule A", command: "SaveData", want: "0" },
        { name: "Rule B", command: "TelePeriod", want: "300", severity: "info" },
        { name: "Rule C", command: "SetOption0", want: "0" },
      ]),
    },
  });
});
test.afterAll(async () => {
  await app?.stop();
});

const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
const operator = { "X-authentik-username": "olga", "X-authentik-groups": "ops" };

const names = (page: Page) => page.locator("section#rules tbody td.p1 a").allTextContents();
const hrefOf = async (page: Page, name: string) => (await page.locator("td.p1 a", { hasText: name }).getAttribute("href"))!;

// Why: the order is the precedence, so dragging a row must store exactly the new order, and the
// rule keeps its id (a link to a rule follows the rule, not its slot).
test("drag a row to reorder: the order is stored and every rule keeps its id", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/policy");
  expect(await names(page)).toEqual(["Rule A", "Rule B", "Rule C"]);
  const idC = await hrefOf(page, "Rule C");
  const rows = page.locator("section#rules tbody tr");
  await rows.nth(2).locator(".pol-grip").dragTo(rows.nth(0), { targetPosition: { x: 20, y: 2 } });
  await expect(page.getByText("Rule saved")).toBeVisible();
  expect(await names(page)).toEqual(["Rule C", "Rule A", "Rule B"]);
  expect(await hrefOf(page, "Rule C"), "Rule C keeps its id when it moves to the top").toBe(idC);
});

// Why: Move up / Move down in the row menu are the way to reorder on a phone and without
// JavaScript (a <details> menu and plain form posts).
test("without JavaScript the row menu moves a rule down and back up", async ({ browser }) => {
  const ctx = await browser.newContext({ javaScriptEnabled: false, extraHTTPHeaders: admin });
  const page = await ctx.newPage();
  await page.goto(app.url + "/policy");
  const before = await names(page);
  const first = before[0];
  await page.locator("section#rules tbody tr").first().locator("summary").click();
  await page.getByRole("menuitem", { name: "Move down" }).first().click();
  expect(await names(page)).toEqual([before[1], first, ...before.slice(2)]);
  await page.locator("section#rules tbody tr").nth(1).locator("summary").click();
  await page.getByRole("menuitem", { name: "Move up" }).first().click();
  expect(await names(page)).toEqual(before);
  await ctx.close();
});

// Why: a switched-off rule is dimmed and says it is not checked; the tile counts it as disabled.
test("a disabled rule is dimmed, not checked and counted as disabled", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/policy");
  await page.getByLabel("Actions for Rule B").click();
  await page.getByRole("menuitem", { name: "Disable" }).click();
  const row = page.locator("section#rules tbody tr", { hasText: "Rule B" });
  await expect(row).toHaveClass(/is-dim/);
  await expect(row).toContainText("Disabled");
  await expect(row).toContainText("not checked while disabled");
  await expect(page.locator("[data-tile=rules]")).toContainText("2 enabled · 1 disabled");
  await page.getByLabel("Actions for Rule B").click();
  await page.getByRole("menuitem", { name: "Enable" }).click();
  await expect(row).not.toHaveClass(/is-dim/);
});

// Why: the editor left the overview. Old bookmarks are not redirected (no compatibility shims) but
// must show the overview, not an error.
test("old ?edit= and ?new= links show the overview", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  for (const q of ["?edit=1", "?new=1", "?v=abc"]) {
    const res = await page.goto(app.url + "/policy" + q);
    expect(res!.status()).toBe(200);
    await expect(page.locator("section#rules")).toBeVisible();
    await expect(page.locator("input[name=command]")).toHaveCount(0);
  }
});

// Why: operators look and run the check; they get no handle, no menu, no Pause and no New rule.
test("an operator sees the table without handle, menu, Pause or New rule", async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/policy");
  await expect(page.locator("section#rules tbody tr")).toHaveCount(3);
  await expect(page.locator(".pol-grip")).toHaveCount(0);
  await expect(page.getByLabel(/^Actions for/)).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Pause" })).toHaveCount(0);
  await expect(page.getByRole("link", { name: "New rule" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Run now" })).toBeVisible();
});

// Why: the first row's menu opens below its button, not over the tiles above (the menu's direction
// follows the room in the viewport at the moment of opening).
test("the first row's menu opens downwards", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/policy");
  const row = page.locator("section#rules tbody tr").first();
  await row.getByLabel(/^Actions for/).click();
  const btn = (await row.locator("summary").boundingBox())!;
  const body = (await row.locator(".rowmenu-body").boundingBox())!;
  expect(body.y, "the menu starts below its button").toBeGreaterThanOrEqual(btn.y + btn.height - 1);
});
