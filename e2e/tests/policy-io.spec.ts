import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// Import / export is a tab of the policy shell (#1088). An import replaces ALL rules, so the preview lists
// every rule with its fate and the replace note is a warning ABOVE the button, also on a phone, where a note
// beside the button would wrap below it.
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

const saved = `[
  {"name":"Flash wear","command":"SaveData","expected":"0","level":"required"},
  {"name":"Quiet","command":"SetOption0","want":"1","severity":"info"},
  {"name":"Leaves","command":"SetOption1","expected":"1","level":"advisory"}
]`;
const file = `[
  {"name":"Flash wear","command":"SaveData","expected":"0","level":"required"},
  {"name":"Quiet","command":"SetOption0","expected":"0","level":"advisory"},
  {"name":"Fresh","command":"SetOption2","expected":"1"}
]`;

test("import: every rule has its fate, the warning sits above the button at 375 px, the export speaks the UI's words", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 375, height: 800 });
  await page.goto(app.url + "/policy/io?tab=import");
  // the first file is stored; the old and the new spelling are both read
  await page.locator("textarea[name=json]").fill(saved);
  await page.getByRole("button", { name: "Check import" }).click();
  await page.getByRole("button", { name: "Replace rules with these 3" }).click();
  await expect(page).toHaveURL(/\/policy\?ok=imported$/);

  // the export holds the words of the page, and the tab row is the policy shell's
  await page.goto(app.url + "/policy/io");
  const exported = page.locator(".pio-json");
  await expect(exported).toContainText('"level": "advisory"');
  await expect(exported).toContainText('"expected": "1"');
  await expect(exported).not.toContainText('"severity"');
  await expect(page.getByRole("navigation", { name: "Policy views" }).getByRole("link", { name: "Import / export" })).toHaveAttribute("aria-current", "page");
  await expect(page.getByRole("link", { name: "Back to policy" })).toHaveCount(0);

  // the second file changes one rule, keeps one, adds one and leaves one out
  await page.goto(app.url + "/policy/io?tab=import");
  await page.locator("textarea[name=json]").fill(file);
  await page.getByRole("button", { name: "Check import" }).click();
  const preview = page.locator(".pio-preview");
  const fate = (name: string) => preview.locator(".pio-row", { hasText: name });
  await expect(fate("Fresh")).toContainText("New");
  await expect(fate("Quiet")).toContainText("Changed");
  await expect(fate("Flash wear")).toContainText("Unchanged");
  await expect(fate("Leaves")).toContainText("Removed");

  const button = preview.getByRole("button", { name: "Replace rules with these 3" });
  const banner = preview.locator(".banner.t-warn");
  await expect(banner).toContainText("replaces all current rules");
  const b = (await banner.boundingBox())!;
  const c = (await button.boundingBox())!;
  expect(b.y + b.height, "the banner's bottom edge is above the button's top edge").toBeLessThanOrEqual(c.y);
});

test("operators only export: the tab is there, the Import card is not", async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/policy/io?tab=import");
  await expect(page.getByRole("link", { name: "Download .json" })).toBeVisible();
  await expect(page.locator("textarea[name=json]")).toHaveCount(0);
  await expect(page.getByRole("navigation", { name: "Policy views" }).getByRole("link", { name: "Import / export" })).toHaveAttribute("aria-current", "page");
});
