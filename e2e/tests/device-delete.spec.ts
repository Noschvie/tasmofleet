import type { Browser, Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import {
  App,
  Devenv,
  freePort,
  startApp,
  startDevenv,
  writeTasmoAdminDir,
} from "../support/stack";

// Delete a device (design v2 screen "devdelete", N2): the device header's Delete and the row
// menu's Delete… lead to one page that lists the device's backups, asks what happens to them
// (Keep preselected) and needs the one acknowledgement. The page is a plain form, so it works
// without JavaScript too. Each test deletes its own device of this stack.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_URL,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

// deviceName is the n-th device of the stack (sorted, so every test picks its own).
function deviceName(n: number): string {
  return Object.keys(env.devices).sort()[n];
}

// openDevice opens a device's overview from the list and returns its URL.
async function openDevice(page: Page, name: string): Promise<string> {
  await page.goto(app.url + "/devices");
  await page
    .locator("table[data-table=devices] tbody")
    .getByRole("link", { name, exact: true })
    .click();
  await expect(page.locator("h1")).toHaveText(name);
  return page.url();
}

// backUp takes a manual backup of the open device (the header's Back up, a plain form).
async function backUp(page: Page) {
  await expect(
    page.getByRole("button", { name: "Back up", exact: true }),
  ).toBeEnabled({ timeout: 20_000 });
  await page.getByRole("button", { name: "Back up", exact: true }).click();
  await expect(
    page
      .locator(".toast, [role=status]")
      .filter({ hasText: /Backup/ })
      .first(),
  ).toBeVisible();
}

async function expectGone(page: Page, name: string) {
  await expect(page).toHaveURL(/\/devices\?ok=deleted$/);
  await expect(page.getByText("Device deleted.")).toBeVisible();
  await expect(
    page
      .locator("table[data-table=devices] tbody")
      .getByRole("link", { name, exact: true }),
  ).toHaveCount(0);
}

// Why: the header's Delete must lead to the page that names the backups, not to a dialog that
// cannot, and Keep (the choice that loses nothing) is what the admin gets without choosing.
test("delete from the device header keeps the backups by default", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders(admin);
  const name = deviceName(0);
  const deviceURL = await openDevice(page, name);
  await backUp(page);
  await page.getByRole("link", { name: "Delete", exact: true }).click();
  await expect(page).toHaveURL(deviceURL + "/delete");
  await expect(page.locator("h1")).toHaveText(`Delete ${name}?`);
  await expect(page.getByText("This device still has 1 backup")).toBeVisible();
  await expect(page.locator(".dd-row")).toHaveCount(1);
  await expect(
    page.getByRole("radio", { name: /Keep the backups/ }),
  ).toBeChecked();
  await page.getByRole("checkbox", { name: /I understand/ }).check();
  await page.getByRole("button", { name: "Delete device" }).click();
  await expectGone(page, name);
});

// Why: the acknowledgement is checked by the server; a post without it deletes nothing and
// answers with the page and the field error, keeping the admin's choice. Delete… in the row
// menu leads to the same page.
test("delete from the row menu: refused without the acknowledgement, then deletes the backups too", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders(admin);
  const name = deviceName(1);
  await page.goto(app.url + "/devices");
  const row = page
    .locator("table[data-table=devices] tbody tr")
    .filter({ has: page.getByRole("link", { name, exact: true }) });
  await row.locator("td.rowacts > details.rowmenu > summary").click();
  await row.locator('.rowmenu-body a.danger[href$="/delete"]').click();
  await expect(page).toHaveURL(/\/devices\/\d+\/delete$/);
  await expect(page.locator("h1")).toHaveText(`Delete ${name}?`);
  await expect(page.getByText("This device has no backups.")).toBeVisible();
  await page.getByRole("button", { name: "Delete device" }).click();
  await expect(page.getByText("Tick the box to confirm.")).toBeVisible();
  await expect(page.locator("h1")).toHaveText(`Delete ${name}?`);
  await page.goto(app.url + "/devices");
  await expect(
    page
      .locator("table[data-table=devices] tbody")
      .getByRole("link", { name, exact: true }),
    "a refused delete removed the device",
  ).toHaveCount(1);
  const deviceURL = await openDevice(page, name);
  await backUp(page);
  await page.goto(deviceURL + "/delete");
  await page.getByRole("radio", { name: /Delete the backups too/ }).check();
  await page.getByRole("button", { name: "Delete device" }).click(); // without the box: refused
  await expect(page.getByText("Tick the box to confirm.")).toBeVisible();
  await expect(
    page.getByRole("radio", { name: /Delete the backups too/ }),
    "the refused page keeps the choice",
  ).toBeChecked();
  await page.getByRole("checkbox", { name: /I understand/ }).check();
  await page.getByRole("button", { name: "Delete device" }).click();
  await expectGone(page, name);
});

// Why: deleting must work without JavaScript: the row menu is a <details>, the page a plain form.
test("delete works without JavaScript", async ({
  browser,
}: {
  browser: Browser;
}) => {
  const ctx = await browser.newContext({
    javaScriptEnabled: false,
    extraHTTPHeaders: admin,
  });
  const page = await ctx.newPage();
  const name = deviceName(2);
  await page.goto(app.url + "/devices");
  const row = page
    .locator("table[data-table=devices] tbody tr")
    .filter({ has: page.getByRole("link", { name, exact: true }) });
  await row.locator("td.rowacts > details.rowmenu > summary").click();
  await row.locator('.rowmenu-body a.danger[href$="/delete"]').click();
  await expect(page).toHaveURL(/\/devices\/\d+\/delete$/);
  await page.getByRole("checkbox", { name: /I understand/ }).check();
  await page.getByRole("button", { name: "Delete device" }).click();
  await expect(page).toHaveURL(/\/devices\?ok=deleted$/);
  await expect(
    page
      .locator("table[data-table=devices] tbody")
      .getByRole("link", { name, exact: true }),
  ).toHaveCount(0);
  await ctx.close();
});
