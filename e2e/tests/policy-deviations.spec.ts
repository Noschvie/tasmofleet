import { test, expect, type Page } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// The Deviating devices tab (#197 WP6) against the simulator fleet: devices that cannot be judged
// yet are backed up from the tab, then a live check fills the table, and Make compliant writes exactly
// the rows that were ticked (a Required row is ticked from the start, an Advisory row never is).
let app: App;
let env: Devenv;
test.describe.configure({ mode: "serial" });
test.beforeAll(async () => {
  env = await startDevenv();
  const fwPort = await freePort(); // the transaction pages come with the firmware server
  app = await startApp({
    env: {
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_POLICY_LIVE_INTERVAL: "0", // no background reads: the first state is "nothing read yet"
      TASMOFLEET_POLICY_RULES: JSON.stringify([
        { name: "Power after outage", command: "PowerOnState", want: "2" },
        { name: "LED hint", command: "LedState", want: "3", severity: "info" },
      ]),
    },
  });
  expect(app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)])).toContain("created: lamp-1m");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const operator = { "X-authentik-username": "olivia", "X-authentik-groups": "ops" };
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

// UI_SHOTS_DIR set: every screenshot at 375 and 1280 px, light and dark.
async function shot(page: Page, name: string) {
  const dir = process.env.UI_SHOTS_DIR;
  if (!dir) return;
  const size = page.viewportSize() ?? { width: 1280, height: 720 };
  for (const [w, h] of [[375, 800], [1280, 900]]) {
    for (const scheme of ["light", "dark"] as const) {
      await page.setViewportSize({ width: w, height: h });
      await page.emulateMedia({ colorScheme: scheme });
      await page.screenshot({ path: `${dir}/${name}-${w}-${scheme}.png`, fullPage: true });
    }
  }
  await page.setViewportSize(size);
  await page.emulateMedia({ colorScheme: null });
}

const group = (page: Page, device: string) => page.locator("tbody[data-dev-group]", { has: page.locator("tr.pol-grp", { hasText: device }) });
const box = (page: Page, device: string, rule: string) => page.getByRole("checkbox", { name: `Select ${rule} on ${device}` });

// Why: before anything was read no device can be judged, so there is nothing to fix; the way out is
// a backup (for the settings it decodes). "Back up these N" must open the scope page with exactly the
// devices of the list, and running it starts a Back up transaction.
test("nothing read yet: the table is empty and Back up these N backs up exactly the listed devices", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/policy/deviations");
  await expect(page.getByText("No device deviates from a rule.")).toBeVisible();
  await expect(page.getByText(/\d+ devices cannot be judged/)).toBeVisible();
  await expect(page.getByRole("button", { name: /Make compliant/ })).toHaveCount(0);
  await page.locator("details#unjudged > summary").click();
  await shot(page, "empty");

  // the scope page leaves out a device the health tracker has not seen answer yet: retry until it has
  await expect(async () => {
    await page.goto(app.url + "/policy/deviations?q=lamp-1m");
    await page.locator("details#unjudged > summary").click();
    await expect(page.locator("details#unjudged .tag")).toHaveText(["lamp-1m"]);
    await page.getByRole("button", { name: "Back up these 1" }).click();
    await expect(page.getByRole("heading", { name: "Back up devices", level: 1 })).toBeVisible();
    await expect(page.locator(".bk-row")).toHaveCount(1);
    await expect(page.locator(".bk-row")).toContainText("lamp-1m");
    await expect(page.getByRole("button", { name: "Back up 1 device", exact: true })).toBeVisible({ timeout: 2_000 });
  }).toPass({ timeout: 90_000 });
  await page.getByRole("button", { name: "Back up 1 device", exact: true }).click();
  await expect(page.getByRole("heading", { name: /^Backups (running|finished)$/, level: 1 })).toBeVisible();
  await expect(page.locator(".bk-rrow", { hasText: "lamp-1m" })).toBeVisible();
});

test("operators read the deviations but have no boxes and no buttons", async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/policy/deviations");
  await page.getByRole("button", { name: "Run now" }).first().click(); // the live check is open to operators
  await expect(page.locator("tbody[data-dev-group]").first()).toBeVisible({ timeout: 60_000 });
  await expect(page.locator("input[name=pick]")).toHaveCount(0);
  await expect(page.getByRole("button", { name: /Make compliant|Back up/ })).toHaveCount(0);
  await shot(page, "operator");
});

// Why: the device group shows its Required and Advisory deviation together; the filters narrow the
// table, and the boxes follow the rule: Required ticked, Advisory not.
test("deviations are grouped by device; Required rows start ticked, Advisory rows do not", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/policy/deviations");
  await page.getByRole("button", { name: "Run now" }).first().click();
  const g = group(page, "plug-4m-http");
  await expect(g).toBeVisible({ timeout: 60_000 });
  await expect(g.locator("tr.pol-grp")).toContainText("1 required · 1 advisory");
  await expect(g.locator("tr").nth(1)).toContainText("Power after outage");
  await expect(box(page, "plug-4m-http", "Power after outage")).toBeChecked();
  await expect(box(page, "plug-4m-http", "LED hint")).not.toBeChecked();
  await expect(g.getByRole("link", { name: "Device →" }).first()).toHaveAttribute("href", /\/devices\/\d+$/);
  await shot(page, "deviations");

  await page.getByRole("link", { name: /^Advisory/ }).click();
  await expect(page).toHaveURL(/show=advisory/);
  await expect(page.getByText("Power after outage")).toHaveCount(0);
  await expect(box(page, "plug-4m-http", "LED hint")).toBeVisible();

  await page.goto(app.url + "/policy/deviations?q=plug-4m-http");
  await expect(page.locator("tbody[data-dev-group]")).toHaveCount(1);
});

// Why: "Make compliant" must write exactly what is ticked. Only the PowerOnState row of one device is
// ticked: the preview lists that one command, the run ends with that deviation gone, and the
// device's Advisory row is still there (a hint is never written unless it was ticked).
test("select one row: the preview lists only that command and the run fixes only it", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/policy/deviations");
  await page.locator("input[data-pick-all]").check(); // all rows ...
  await page.locator("input[data-pick-all]").uncheck(); // ... then none
  await expect(page.locator("input[name=pick]:checked")).toHaveCount(0);
  await box(page, "plug-4m-http", "Power after outage").check();
  await expect(page.getByRole("button", { name: /Make compliant… \(1 selected\)/ })).toBeVisible();
  await page.getByRole("button", { name: /Make compliant…/ }).click();
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Make 1 device compliant");
  await expect(page.locator(".pol-fix-cmd .mono")).toHaveText(["PowerOnState 2"]);
  await shot(page, "preview");
  await page.getByRole("button", { name: /^Run 1 change$/ }).click();
  await expect(page).toHaveURL(/\/transactions\/\d+/);
  await expect(page.locator(".upd-dev", { hasText: "plug-4m-http" }).getByText("Applied", { exact: true }).first()).toBeVisible({ timeout: 60_000 });

  await page.goto(app.url + "/policy/deviations?q=plug-4m-http");
  await page.getByRole("button", { name: "Run now" }).first().click();
  const g = group(page, "plug-4m-http");
  await expect(g.locator("tr.pol-grp")).toContainText("1 advisory", { timeout: 60_000 });
  await expect(g.getByText("Power after outage")).toHaveCount(0);
  await expect(g.getByText("LED hint")).toBeVisible(); // never written: it was not ticked
});

// Why: an Advisory deviation can be written when the admin ticks it by hand, from this tab and
// from the preview of the other entry points (the device tile: unticked box, then Update plan).
test("an Advisory row ticked by hand is written; the device tile's preview offers it unticked", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/policy/deviations?q=plug-4m-http");
  await box(page, "plug-4m-http", "LED hint").check();
  await page.getByRole("button", { name: /Make compliant…/ }).click();
  await expect(page.locator(".pol-fix-cmd .mono")).toHaveText(["LedState 3"]);
  await page.getByRole("button", { name: /^Run 1 change$/ }).click();
  await expect(page.locator(".upd-dev", { hasText: "plug-4m-http" }).getByText("Applied", { exact: true }).first()).toBeVisible({ timeout: 60_000 });

  // another device, through the device page tile: Required only, the hint is offered unticked
  await page.goto(app.url + "/policy/deviations?q=plug-4m");
  const g = group(page, "plug-4m").first();
  await expect(g).toBeVisible();
  await g.getByRole("link", { name: "Device →" }).first().click();
  await page.getByRole("button", { name: "Make compliant…" }).click();
  await expect(page.locator(".pol-fix-cmd .mono")).toHaveText(["PowerOnState 2"]);
  const hint = page.getByRole("checkbox", { name: /LED hint/ });
  await expect(hint).not.toBeChecked();
  await hint.check();
  await page.getByRole("button", { name: "Update plan" }).click();
  await expect(page.locator(".pol-fix-cmd .mono")).toHaveText(["PowerOnState 2", "LedState 3"]);
  await expect(page.getByRole("checkbox", { name: /LED hint/ })).toBeChecked();
});
