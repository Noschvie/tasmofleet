import { join } from "node:path";
import { Page } from "@playwright/test";
import { deviceBox, deviceEntry } from "../support/devicelist";
import { test, expect } from "../support/fixtures";
import { expectNoOverflow, overflowProblem } from "../support/layout";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Why: a device name with no break opportunity (people really name devices like
// "AtticHeaterPlug...") is the longest unbreakable token the UI ever shows. In
// v0.10.0 QA it pushed the device header, the retry form, the bulk scope form,
// the row dialogs and the firmware release dialog past the screen (up to 802 px
// at 375). Text must wrap inside its container, whatever the page: every key page
// is measured at 375 with that name (the page must not scroll sideways), and
// dialogs are also measured against their own frame at 1280.
const LONG = "AtticHeaterPlugWithAReallyLongDeviceNameThatNeverHasABreakAtAll";
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

let app: App;
let env: Devenv;
let deviceURL = "";
let retryURL = "";

test.beforeAll(async ({ browser }) => {
  test.setTimeout(240_000);
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_URL,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
    },
  });
  const pick = ["lamp-1m", "plug-4m"];
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(Object.fromEntries(pick.map((n) => [n, env.devices[n]])))]);
  const page = await browser.newPage({ extraHTTPHeaders: admin, viewport: { width: 1280, height: 800 } });
  try {
    await page.goto(app.url + "/devices");
    for (const n of ["lamp-1m", "plug-4m"]) await expect(page.getByRole("row").filter({ hasText: n }).getByText("Online", { exact: true }).first()).toBeVisible({ timeout: 40_000 });
    // cached firmware used by the long-named device, for the release notes dialog
    await expect(async () => {
      await page.goto(app.url + "/firmware");
      await expect(page.locator(`#release-${env.vars.DEVENV_FW_VERSION.replaceAll(".", "\\.")}`)).toBeVisible();
    }).toPass({ timeout: 30_000 });
    await page.getByRole("button", { name: "Prefetch for my fleet" }).click();
    await expect(page.getByText(/State: finished\b/)).toBeVisible({ timeout: 90_000 });
    // the long name
    await page.goto(app.url + "/devices");
    const href = await page.getByRole("link", { name: "plug-4m", exact: true }).getAttribute("href");
    deviceURL = app.url + href;
    await page.goto(deviceURL + "/edit");
    await page.locator("input[name=name]").fill(LONG);
    await page.getByRole("button", { name: "Save changes" }).click();
    await expect(page.getByRole("heading", { level: 1, name: LONG })).toBeVisible();
    // a failed backup of the long-named device: its retry form shows the error text. The simulator cannot
    // fail a backup on demand, so the finished job is turned into the failure QA saw.
    await page.goto(app.url + "/devices");
    await page.getByRole("row").filter({ hasText: LONG }).getByRole("checkbox").check();
    await page.locator("[data-bulkbar]").getByRole("button", { name: "Back up" }).click();
    await page.getByRole("button", { name: "Back up 1 device", exact: true }).click();
    await expect(page.getByRole("heading", { name: "Backups finished", level: 1 })).toBeVisible({ timeout: 90_000 });
    const batch = page.url().match(/bulk\/result\/(\d+)/)![1];
    retryURL = app.url + "/transactions/" + batch + "/retry";
    const { DatabaseSync } = await import("node:sqlite");
    const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
    try {
      db.exec("PRAGMA busy_timeout=5000");
      db.prepare("UPDATE ota_jobs SET state='failed', last_error=? WHERE batch_id=?").run("backup: download config: tasmota: unparsable response: HTTP 404", Number(batch));
    } finally {
      db.close();
    }
  } finally {
    await page.close();
  }
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

/** dialogFits: nothing inside the open dialog sticks out of the dialog's own frame. */
async function dialogOverflow(page: Page): Promise<string | null> {
  return page.evaluate(() => {
    const d = document.querySelector("dialog[open]");
    if (!d) return "no open dialog";
    const box = d.getBoundingClientRect();
    const out = [...d.querySelectorAll("*")].filter((e) => {
      const r = e.getBoundingClientRect();
      // a table head that is visually hidden on phones sits far off screen on purpose
      return r.width > 0 && r.right > box.left && (r.right > box.right + 1 || r.left < box.left - 1);
    });
    return out.length ? `sticks out of the dialog: ${out.slice(0, 4).map((e) => e.tagName.toLowerCase() + "." + e.className + " " + Math.round(e.getBoundingClientRect().right) + ">" + Math.round(box.right)).join("; ")}` : null;
  });
}

test.describe("phone, long device name", () => {
  test.use({ viewport: { width: 375, height: 812 }, extraHTTPHeaders: admin });

  test("device pages do not scroll sideways", async ({ page }) => {
    for (const tab of ["", "/settings", "/console", "/backups", "/drift", "/replace", "/edit"]) {
      await page.goto(deviceURL + tab);
      await expectNoOverflow(page, `device${tab || " overview"}`);
    }
    // the title is readable in full, the back link does not run off the screen
    await page.goto(deviceURL);
    const h1 = await page.locator(".devhd h1").boundingBox();
    expect(h1!.width, "the title stays inside the screen").toBeLessThanOrEqual(375);
  });

  test("lists, bulk pages and the retry form do not scroll sideways", async ({ page }) => {
    await page.goto(app.url + "/devices");
    await expectNoOverflow(page, "devices");
    // the name is wrapped to its full length in the card, not clipped
    const link = page.getByRole("link", { name: LONG, exact: true });
    expect(await link.evaluate((a) => a.scrollWidth <= a.clientWidth + 1), "the name is not cut").toBe(true);
    await deviceBox(page, LONG).check();
    await deviceBox(page, "lamp-1m").check();
    await page.locator("[data-bulkbar]").getByRole("button", { name: "Restart" }).click();
    await expect(page.getByRole("heading", { name: "Restart devices", level: 1 })).toBeVisible();
    await expectNoOverflow(page, "bulk restart scope"); // the scope page is the confirmation
    await page.goto(app.url + "/devices");
    await deviceBox(page, LONG).check();
    await deviceBox(page, "lamp-1m").check();
    await page.locator("[data-bulkbar]").getByRole("button", { name: "Back up" }).click();
    await expect(page.getByRole("heading", { name: "Back up devices", level: 1 })).toBeVisible();
    await expectNoOverflow(page, "bulk backup scope");
    await page.goto(retryURL);
    await expect(page.getByRole("heading", { name: /Retry/ }).first()).toBeVisible();
    await expect(page.locator("table[data-table=transaction_retry] .reason").first(), "the retry form shows the error text").toBeVisible();
    await expectNoOverflow(page, "transaction retry");
    await page.goto(app.url + "/transactions");
    await expectNoOverflow(page, "transactions");
  });

  test("dialogs fit the screen", async ({ page }) => {
    await page.goto(app.url + "/devices");
    // Restart… opens the row dialog; Delete… leads to its confirmation page (design v2)
    await deviceEntry(page, LONG).locator("details.rowmenu summary").click(); // the phone card
    await deviceEntry(page, LONG).locator("details.rowmenu[open] a[data-dialog]").first().click();
    await expect(page.locator("dialog#dlg-row[open]")).toHaveCount(1);
    expect(await overflowProblem(page, "row dialog")).toBeNull();
    expect(await dialogOverflow(page)).toBeNull();
    await page.keyboard.press("Escape");
    await deviceEntry(page, LONG).locator("details.rowmenu summary").click();
    await deviceEntry(page, LONG).locator("details.rowmenu[open] a.danger").first().click();
    await expect(page).toHaveURL(/\/devices\/\d+\/delete$/);
    await expectNoOverflow(page, "delete page");
    await page.goto(app.url + "/devices");
    await page.goto(app.url + "/firmware");
    await page.getByRole("link", { name: "Release notes" }).click();
    await expect(page.locator("dialog[open]")).toBeVisible();
    expect(await overflowProblem(page, "firmware release notes dialog")).toBeNull();
    expect(await dialogOverflow(page)).toBeNull();
    // names are wrapped, not cut with an ellipsis
    const cut = await page.locator("dialog[open] .dev > a").evaluateAll((as) => as.filter((a) => a.scrollWidth > a.clientWidth + 1).map((a) => a.textContent));
    expect(cut, "device names in the release notes dialog are cut").toEqual([]);
    await page.keyboard.press("Escape");
    await page.goto(app.url + "/firmware?notes=1&release=" + env.vars.DEVENV_FW_VERSION);
    await expectNoOverflow(page, "firmware release notes in the page");
    const cutPage = await page.locator("main .dev > a").evaluateAll((as) => as.filter((a) => a.scrollWidth > a.clientWidth + 1).map((a) => a.textContent));
    expect(cutPage, "device names on the release page are cut").toEqual([]);
  });
});

test.describe("desktop, long device name", () => {
  test.use({ viewport: { width: 1280, height: 800 }, extraHTTPHeaders: admin });

  test("dialogs keep their title and Close inside the frame", async ({ page }) => {
    await page.goto(app.url + "/devices");
    await page.locator("tbody tr", { hasText: LONG }).locator("details.rowmenu summary").click();
    await page.locator("tbody tr", { hasText: LONG }).locator("details.rowmenu[open] a[data-dialog]").click();
    await expect(page.locator("dialog#dlg-row[open]")).toHaveCount(1);
    expect(await dialogOverflow(page)).toBeNull();
    await expect(page.locator("dialog#dlg-row h2")).toBeVisible();
    await expect(page.locator("dialog#dlg-row [data-close]").first()).toBeVisible();
    await page.keyboard.press("Escape");
    await page.goto(app.url + "/firmware");
    await page.getByRole("link", { name: "Release notes" }).click();
    await expect(page.locator("dialog[open]")).toBeVisible();
    expect(await dialogOverflow(page)).toBeNull();
    await expect(page.locator("dialog[open] .fw-dlg-head button")).toBeVisible();
  });

  test("device header and retry form", async ({ page }) => {
    await page.goto(deviceURL);
    await expectNoOverflow(page, "device header at 1280");
    await page.goto(retryURL);
    await expectNoOverflow(page, "retry at 1280");
  });
});
