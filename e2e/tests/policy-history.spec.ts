import { test, expect, type Page } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// The History tab (/policy/history) and the history of one rule, #197 WP7: quick saves fold into one
// entry, each entry says what changed, a revert is a new version and lands back on the tab.
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
test.afterAll(async () => {
  await app?.stop();
});

const operator = { "X-authentik-username": "olivia", "X-authentik-groups": "ops" };
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

// UI_SHOTS_DIR set: every screenshot at 375 and 1280 px, light and dark.
async function shot(page: Page, name: string) {
  const shots = process.env.UI_SHOTS_DIR;
  if (!shots) return;
  const size = page.viewportSize() ?? { width: 1280, height: 720 };
  for (const [w, h] of [
    [375, 800],
    [1280, 900],
  ]) {
    for (const scheme of ["light", "dark"] as const) {
      await page.setViewportSize({ width: w, height: h });
      await page.emulateMedia({ colorScheme: scheme });
      await page.screenshot({ path: `${shots}/${name}-${w}-${scheme}.png`, fullPage: true });
    }
  }
  await page.setViewportSize(size);
  await page.emulateMedia({ colorScheme: null });
}

test("history: quick saves fold, the summary names the rule, revert lands on the tab; a rule page lists only its versions", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/policy/history");
  await expect(page.getByText("No versions yet")).toBeVisible();
  await shot(page, "history-empty");

  // v1: the timezone rule from its template
  await page.goto(app.url + "/policy/rules/new?template=timezone");
  await page.getByRole("button", { name: "Save rule" }).click();
  await expect(page).toHaveURL(/\/policy\/rules\/r[a-z0-9]+\?ok=saved$/);
  const timezoneRule = page.url().split("?")[0];
  // v2..v4: three quick edits of the expected value
  for (const want of ["1", "2", "3"]) {
    await page.locator("input[name='want']").fill(want);
    await page.getByRole("button", { name: "Save rule" }).click();
    await expect(page).toHaveURL(timezoneRule + "?ok=saved");
  }
  // another rule, so the first rule's own history has something to leave out (v5)
  await page.goto(app.url + "/policy/rules/new");
  await page.locator("#any-command").fill("LedState");
  await page.getByRole("button", { name: "Continue" }).first().click();
  await page.locator("input[name='want']").fill("3");
  await page.getByRole("button", { name: "Save rule" }).click();
  await expect(page).toHaveURL(/\/policy\/rules\/r[a-z0-9]+\?ok=saved$/);

  await page.goto(app.url + "/policy");
  await page.getByRole("link", { name: "History" }).first().click();
  await expect(page).toHaveURL(/\/policy\/history$/);
  const list = page.locator("#history");
  // the current version is alone; v1-4 are one fold, collapsed
  await expect(list.locator("details.pol-hfold")).toHaveCount(1);
  await expect(list.locator(".pol-hv").filter({ hasText: "Current" })).toHaveCount(1);
  const fold = list.locator("details.pol-hfold");
  await expect(fold).toContainText("4 quick saves");
  await expect(fold).toContainText("Added Local timezone");
  await expect(fold.locator("a.pol-hv").first()).toBeHidden();
  await fold.locator("summary").click();
  await expect(fold.locator("a.pol-hv")).toHaveCount(4);
  await shot(page, "history-folded");

  // the right card says what the newest version changed
  await expect(page.locator("#diff")).toContainText("Changes in v5");
  await expect(page.locator("#diff")).toContainText("LedState");

  // a rule page lists only the versions that touched it (v1..v4, not v5)
  await page.goto(timezoneRule);
  const own = page.locator("#rule-history");
  await own.locator("summary").click();
  await expect(own.locator("a.pol-hv")).toHaveCount(4);
  await expect(own).not.toContainText("v5");
  await own.getByRole("link", { name: /All policy history/ }).click();
  await expect(page).toHaveURL(/\/policy\/history$/);

  // revert to the version before the newest: a new version, back on the tab with the flash
  await page.getByRole("button", { name: "Revert to v4…" }).click();
  await shot(page, "history-revert-dialog");
  await page.locator("dialog[open]").getByRole("button", { name: "Revert to v4" }).click();
  await expect(page).toHaveURL(/\/policy\/history\?ok=reverted$/);
  await expect(page.getByText(/Reverted/)).toBeVisible();
  await expect(page.locator("#diff")).toContainText("Changes in v6");
  await expect(page.locator("#history .pol-hv").filter({ hasText: "Current" })).toContainText("v6");

  // an unknown version is a 404
  expect((await page.goto(app.url + "/policy/history?v=999"))?.status()).toBe(404);

  // operators read the history but cannot revert
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/policy/history");
  await expect(page.locator("#history")).toBeVisible();
  await expect(page.getByRole("button", { name: /^Revert to v/ })).toHaveCount(0);
});

test("history at 375 px: the list comes first, the diff below, nothing scrolls sideways", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 375, height: 800 });
  await page.goto(app.url + "/policy/rules/new?template=timezone");
  await page.getByRole("button", { name: "Save rule" }).click();
  await expect(page).toHaveURL(/\?ok=saved$/);
  await page.goto(app.url + "/policy/history?v=1");
  await shot(page, "history-phone");
  const lb = await page.locator(".pol-hist-list").boundingBox();
  const db = await page.locator("#diff").boundingBox();
  expect(lb && db && lb.y < db.y).toBeTruthy();
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(overflow).toBeLessThanOrEqual(0);
});
