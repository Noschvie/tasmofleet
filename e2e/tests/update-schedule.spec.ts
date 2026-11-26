import { test, expect } from "../support/fixtures";
import { cardStackGaps, expectLayout } from "../support/layout";
import { pickForUpdate } from "../support/updates";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";
import type { Page } from "@playwright/test";

// Planned updates in Transactions (#604): the Scheduled section, the page of a plan, Cancel and "Plan again…".
// The plan is made through the update preview, so this also proves that what the person planned is what the list
// shows. Nothing may start on the device: the plan is for tomorrow night.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
      TZ: "UTC",
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_URL,
    },
  });
  expect(app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)])).toContain("created: lamp-1m");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

/** plan plans an update of lamp-1m to Tasmota 16.1.0 for tomorrow night and returns the transaction page's path. */
async function plan(page: Page): Promise<string> {
  await page.setExtraHTTPHeaders(admin);
  await expect(async () => {
    await page.goto(app.url + "/updates/new");
    await expect(page.getByLabel("Select lamp-1m", { exact: true })).toBeEnabled({ timeout: 1000 });
  }).toPass({ timeout: 30_000 });
  await pickForUpdate(page, app.url, ["lamp-1m"]);
  await page.locator("label.upd-relopt", { hasText: "Tasmota 16.1.0" }).click();
  await page.getByRole("button", { name: "Review update" }).click();
  await page.getByLabel("Start at a chosen time").check();
  await page.getByText("Tomorrow night 03:00").click();
  await page.locator("button[form=start]").click();
  await expect(page).toHaveURL(/\/transactions\/\d+$/);
  return new URL(page.url()).pathname;
}

test.use({ timezoneId: "Asia/Kolkata", locale: "en-US" });

/**
 * clockAt is how the browser under test writes hour:00 (time.js key time: no leading zero in a 12-hour
 * locale, "3:00 AM"), asked of its own Intl so that neither ICU's spelling nor a hard-coded "03:00 AM" is
 * in this file. It is a pattern: any white space in it also matches the no-break spaces the page writes.
 */
async function clockAt(page: Page, hour: number): Promise<string> {
  const text = await page.evaluate((h) => new Intl.DateTimeFormat(undefined, { hour: "numeric", minute: "2-digit" }).format(new Date(2026, 0, 1, h, 0)), hour);
  return text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&").replace(/\s+/g, "\\s");
}

// Why: a plan nobody can see is a plan nobody can cancel. The list shows it with its start in the browser's
// zone, Cancel asks first and says nothing on the devices changes, and after the cancel the plan is in the
// history as Cancelled with a way to plan it again. All along the status bar says nothing is running: the
// plan waits, it does not work.
test("a plan is listed, shown as planned, cancelled, and planned again", { tag: "@core" }, async ({ page }) => {
  const txn = await plan(page);
  const id = "TX-" + txn.split("/").pop()!.padStart(4, "0");

  // the page of the plan
  await expect(page.locator("#txn-state")).toHaveText("Scheduled");
  const at3 = await clockAt(page, 3);
  const at4 = await clockAt(page, 4);
  await expect(page.locator(".upd-when-big")).toContainText(new RegExp(`^Starts \\w{3}, \\w{3} \\d+ at ${at3} · in \\d+`));
  await expect(page.locator(".upd-rule")).toContainText(new RegExp(`If TasmoFleet is down at ${at3}, the update starts as soon as TasmoFleet is back, but only until ${at4} \\(60 minutes after the start time\\)\\.`));
  await expect(page.locator(".upd-row-state")).toHaveText("Planned");
  await expect(page.getByRole("link", { name: "Retry failed" })).toHaveCount(0);
  await expect(page.locator("#sb-wide")).toContainText("No jobs running"); // a plan is not a running job
  await expect(page.locator("#sb-wide")).not.toContainText(id);
  await expect(page.locator("#sb-wide")).toContainText(new RegExp(`1 scheduled\\s*·\\s*next \\w{3}, \\w{3} \\d+ at ${at3}`)); // #707

  // the list
  await page.goto(app.url + "/transactions");
  const section = page.locator("section.upd-sched");
  await expect(section.getByRole("heading")).toHaveText("Scheduled · 1");
  const row = section.locator(".upd-srow");
  await expect(row).toContainText(id);
  await expect(row.locator(".upd-sstart")).toContainText(new RegExp(`^Starts \\w{3} ${at3}`));
  await expect(page.locator("section.upd-hist")).toHaveCount(0); // nothing else happened yet
  await expect(page.locator(".upd-run")).toHaveCount(0); // not a running transaction
  await expect(page.locator("#sb-wide")).toContainText("No jobs running");
  await page.goto(app.url + "/");
  await expect(page.locator("main")).not.toContainText(id); // a plan is no transaction of the card, only a line above it
  const line = page.locator(".dash-plans");
  await expect(line).toContainText(new RegExp(`1 scheduled\\s*·\\s*next \\w{3}, \\w{3} \\d+ at ${at3}\\s*·\\s*in \\d+`));
  await expect(page.locator("#sb-wide")).toContainText("1 scheduled");
  await line.getByRole("link", { name: "1 scheduled" }).click(); // leads to the Scheduled section of Transactions (#707)
  await expect(page).toHaveURL(/\/transactions#scheduled$/);
  await expect(page.locator("#scheduled")).toBeVisible();

  // Cancel asks, and Keep schedule changes nothing
  await page.goto(app.url + "/transactions");
  await row.getByRole("link", { name: "Cancel" }).click();
  const dlg = page.locator("#dlg-txn");
  await expect(dlg).toBeVisible();
  await expect(dlg).toContainText(`Cancel ${id}?`);
  await expect(dlg).toContainText("It will not start and nothing on the devices changes.");
  await expect(dlg).toContainText(new RegExp(`is planned for \\w{3}, \\w{3} \\d+ at ${at3}\\.`)); // the browser's zone, not the server's
  await dlg.getByRole("button", { name: "Keep schedule" }).click();
  await expect(row).toBeVisible();

  await row.getByRole("link", { name: "Cancel" }).click();
  await dlg.getByRole("button", { name: "Cancel schedule" }).click();
  await expect(page).toHaveURL(new RegExp(txn + "$"));
  await expect(page.locator("#txn-state")).toHaveText("Cancelled before start");
  await expect(page.locator(".upd-row-state")).toHaveText("Cancelled");
  await expect(page.getByRole("link", { name: "Retry failed" })).toHaveCount(0);

  // listed as Cancelled, no longer scheduled
  await page.goto(app.url + "/transactions");
  await expect(page.locator("section.upd-sched")).toHaveCount(0);
  const hist = page.locator("section.upd-hist tbody tr", { hasText: id });
  await expect(hist.locator(".bdg")).toHaveText("Cancelled");
  await expect(hist).toContainText("was due");

  // a cancelled plan is gone from the Dashboard and the bar (#707)
  await expect(page.locator("#sb-wide")).not.toContainText("scheduled");
  await page.goto(app.url + "/");
  await expect(page.locator(".dash-plans")).toHaveCount(0);
  await expect(page.locator("#sb-wide")).not.toContainText("scheduled");

  // Plan again opens the preview with the same device and release and the time choice of the plan, never on
  // "Start now": its main button would start the update at once (#825)
  await page.goto(app.url + txn);
  await page.getByRole("button", { name: "Plan again…" }).click();
  await expect(page.getByRole("heading", { name: "5 · When" })).toBeVisible();
  await expect(page.getByLabel("Start now")).not.toBeChecked();
  await expect(page.getByLabel("Start at a chosen time")).toBeChecked();
  await expect(page.getByLabel("Other time")).toBeChecked();
  await expect(page.getByLabel("Time", { exact: true })).toHaveValue("03:00"); // the plan's 03:00, in the browser's zone
  await expect(page.locator("input[name=release]").first()).toHaveValue("16.1.0");
  await expect(page.locator("form#start input[name=id]")).toHaveValue(/\d+/);
  await expect(page.locator("button[form=start]")).toHaveText("Schedule update");
  await expect(page.locator("[data-starts]")).toContainText(/\w{3}, \w{3} \d+ at 03:00 · in \d+/);
});

// Why (#592): the nightly layout guards open pages by GET at 768 and 1024 px; the plan's pages exist only after
// planning, so they are checked here at both tablet widths and on a phone.
for (const width of [375, 768, 1024]) {
  test(`the Scheduled section and the page of a plan pass the layout guards at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    const txn = await plan(page);
    await expectLayout(page, `plan page ${width}px`);
    expect(await cardStackGaps(page), "card stacks").toEqual([]);
    await page.goto(app.url + "/transactions");
    await expect(page.locator("section.upd-sched")).toBeVisible();
    await expectLayout(page, `transactions with a plan ${width}px`);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    // leave nothing planned for the next test
    await page.goto(app.url + txn);
    await page.getByRole("link", { name: /^Cancel TX-/ }).click();
    await page.locator("#dlg-txn").getByRole("button", { name: "Cancel schedule" }).click();
    await expect(page.locator("#txn-state")).toHaveText("Cancelled before start");
  });
}
