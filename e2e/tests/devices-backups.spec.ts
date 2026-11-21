import { join } from "node:path";
import type { Locator, Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { layoutGuards, guardReport } from "../support/layout";
import { openSchedule } from "../support/nav";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Why (#409): Devices -> Backups is the one page that says how the fleet is backed up: the last
// scheduled run, a row per device, the storage and the schedule. It lives in the Devices group,
// uses the Dashboard's width, and an operator sees it without anything that starts a fleet run.
let app: App;
let env: Devenv;
// A scheduled run never starts at TasmoFleet start (#601), so the one the last test waits for is
// scheduled two minutes ahead, every day, on the clock the app runs on.
const soon = new Date(Date.now() + 2 * 60_000).toTimeString().slice(0, 5);
test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL, // the status bar's Broker item (#934)
      TASMOFLEET_BACKUP_DAYS: "mon,tue,wed,thu,fri,sat,sun",
      TASMOFLEET_BACKUP_TIME: soon,
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
const operator = { "X-authentik-username": "olga", "X-authentik-groups": "ops" };

test("the Devices menu has Backups between Policy and Add, and the page lists every device", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  const total = await page.locator("table[data-table=devices] tbody tr").count();
  await page.getByRole("navigation", { name: "Devices" }).getByRole("link", { name: "Backups" }).click();
  await expect(page).toHaveURL(/\/devices\/backups$/);
  await expect(page.getByRole("heading", { name: "Backups", level: 1 })).toBeVisible();
  await expect(page.locator(".bk-table tbody tr")).toHaveCount(total);
  await expect(page.locator(".dv-chip").first()).toContainText(String(total));
  // the chips count what they show
  for (const chip of await page.locator(".dv-chip").all()) {
    const n = Number((await chip.locator(".dv-n").textContent())!);
    await chip.click();
    await expect(page.locator(".bk-table tbody tr")).toHaveCount(n);
  }
});

test("the page is as wide as the Dashboard's content and never scrolls sideways", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  for (const width of [1280, 1920, 375]) {
    await page.setViewportSize({ width, height: 900 });
    await page.goto(app.url + "/");
    const dash = (await page.locator("main").boundingBox())!.width;
    await page.goto(app.url + "/devices/backups");
    const main = (await page.locator("main").boundingBox())!.width;
    expect(main, `main width at ${width}`).toBeCloseTo(dash, 0);
    const cards = await page.locator(".syspage .cstacks").boundingBox();
    expect(cards!.width, "the card stacks are not capped at the Settings width").toBeGreaterThan(width >= 1920 ? 1300 : main - 80);
    const sw = await page.evaluate(() => document.documentElement.scrollWidth);
    expect(sw, `no sideways scroll at ${width}`).toBeLessThanOrEqual(width);
  }
});

test("on a phone the filter chips scroll sideways inside their row", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 375, height: 800 });
  await page.goto(app.url + "/devices/backups?filter=skipped");
  const row = await page.locator(".dv-chips").evaluate((el) => ({ sc: el.scrollWidth, w: el.clientWidth, ox: getComputedStyle(el).overflowX }));
  expect(row.sc).toBeGreaterThan(row.w);
  expect(["auto", "scroll"]).toContain(row.ox);
});

test("Back up all now leads to the bulk backup page with the devices it would try", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices/backups");
  await expect(page.getByRole("button", { name: /Back up selected/ })).toBeDisabled();
  await page.locator(".bk-table tbody input[type=checkbox]").first().check();
  await expect(page.getByRole("button", { name: /Back up selected \(1\)/ })).toBeEnabled();
  await page.getByRole("button", { name: "Back up all now" }).click();
  await expect(page).toHaveURL(/\/devices\/bulk\/backup/);
});

test("the schedule saves on this page and the row menu shows what the role may do", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices/backups");
  await openSchedule(page);
  await page.locator("input[name='v.BACKUP_KEEP']").fill("5");
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page).toHaveURL(/\/devices\/backups\?ok=saved/);
  await expect(page.locator("input[name='v.BACKUP_KEEP']")).toHaveValue("5");
  await page.locator(".bk-table tbody tr").first().locator("summary").click();
  await expect(page.locator(".bk-table tbody tr").first().getByRole("link", { name: "Open backups" })).toBeVisible();
});

// Why (#949): the page is mostly a status page with one small folded form. A save bar that is on screen
// all the time floats over the last rows of the device table while the page scrolls, with nothing edited.
// It shows only while the form has unsaved changes, and is gone again when the edit is undone.
for (const width of [1280, 375]) {
  test(`the save bar shows only while the schedule has unsaved changes and covers nothing at ${width}px`, async ({ page }) => {
    await page.setExtraHTTPHeaders(admin);
    await page.setViewportSize({ width, height: 800 });
    await page.goto(app.url + "/devices/backups");
    const bar = page.locator("form#sysform [data-sys-bar]");
    await expect(bar).toBeHidden();
    // Scroll the last device row to the bottom of the room above the status bar: that is where a sticky bar
    // would sit. scrollIntoView would put the row under the status bar, which covers it with or without the bar.
    const covered = async () =>
      page.evaluate(() => {
        const rows = document.querySelectorAll(".bk-table tbody tr");
        const row = rows[rows.length - 1];
        const status = document.querySelector("footer.sb")!.getBoundingClientRect();
        window.scrollBy(0, row.getBoundingClientRect().bottom - (status.top - 4));
        const r = row.getBoundingClientRect();
        const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
        return { onTop: !!hit && row.contains(hit), tag: hit ? hit.tagName + "." + hit.className : "none", bottom: r.bottom, top: status.top };
      });
    const clean = await covered();
    expect(clean.onTop, `the last row is covered by ${clean.tag} (row bottom ${clean.bottom}, status bar top ${clean.top})`).toBe(true);
    // ticking a device row belongs to another form: nothing in the schedule is changed
    await page.locator(".bk-table tbody tr").first().locator("input[type=checkbox]").check();
    await expect(bar).toBeHidden();
    await page.locator(".bk-table tbody tr").first().locator("input[type=checkbox]").uncheck();
    // an edit shows the bar, with Save in reach
    await page.evaluate(() => window.scrollTo(0, 0));
    await openSchedule(page);
    const keep = page.locator("input[name='v.BACKUP_KEEP']");
    const stored = await keep.inputValue();
    await keep.fill(String(Number(stored) + 1));
    await expect(bar).toBeVisible();
    await expect(bar.locator(".sbar-state")).toHaveText("Unsaved changes:");
    await expect(page.getByRole("button", { name: "Save settings" })).toBeInViewport();
    // the old value back: nothing is unsaved any more
    await keep.fill(stored);
    await expect(bar).toBeHidden();
  });
}

// Why (#684): the device table is the one long list of the page. On a phone every other card is short and
// sits above it, so the schedule and the deleted devices' backups are not 60 rows down.
test("on a phone every card ends above the device table", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 375, height: 800 });
  await page.goto(app.url + "/devices/backups");
  const table = (await page.locator("#card-devices").boundingBox())!;
  for (const id of ["lastrun", "storage", "orphans", "schedule"]) {
    const box = (await page.locator("#card-" + id).boundingBox())!;
    expect(box.y + box.height, `#card-${id} ends above the device table`).toBeLessThanOrEqual(table.y + 1);
  }
});

// Why (#684): a device name that wraps makes its row two lines high, and a table of 60 rows grows by every
// one of them. At desktop width a row is one line; the full name is the title of its cell.
test("a long device name keeps its row as high as the others and stays readable in its title", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/devices/backups");
  const rows = page.locator(".bk-table tbody tr");
  const normal = (await rows.nth(1).boundingBox())!.height;
  const long = "a-very-long-device-name-with-no-room-in-the-column-" + "x".repeat(60);
  await rows.first().locator("td.dv-name").evaluate((td, name) => {
    td.setAttribute("title", name);
    td.querySelector("a")!.textContent = name;
  }, long);
  const tall = (await rows.first().boundingBox())!.height;
  expect(tall, "the row of the long name is as high as another row").toBeLessThanOrEqual(normal + 1);
  await expect(rows.first().locator("td.dv-name")).toHaveAttribute("title", long);
});

// Why (#684, #633): a title shows only for a mouse or the keyboard, so on a touch tablet a cut name or failure
// reason could not be read at all. Below desktop width the name wraps inside its cell instead of being cut.
test("a long device name wraps on a tablet instead of being cut", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 768, height: 1000 });
  await page.goto(app.url + "/devices/backups");
  const rows = page.locator(".bk-table tbody tr");
  const normal = (await rows.nth(1).boundingBox())!.height;
  const long = "a-very-long-device-name-with-no-room-in-the-column-" + "x".repeat(60);
  await rows.first().locator("td.dv-name a").evaluate((a, name) => {
    a.textContent = name;
  }, long);
  expect((await rows.first().boundingBox())!.height, "the long name takes more lines").toBeGreaterThan(normal + 1);
  const cell = rows.first().locator("td.dv-name");
  const [sw, cw] = await cell.evaluate((td) => [td.scrollWidth, td.clientWidth]);
  expect(sw, "the name is not cut off at its cell").toBeLessThanOrEqual(cw);
});

// Why (#684): the schedule fields sit in a fold. A draft that comes back after a step-up (?restore=1) must
// not come back hidden in it, and a field that fails its constraint in a closed fold must not make the save
// look dead: the browser cannot focus a hidden field, so the fold opens around it.
test("the schedule fold opens around a restored draft and around an invalid field", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices/backups");
  const fold = page.locator("#card-schedule details.fold");
  await expect(fold).not.toHaveAttribute("open", "");
  await page.evaluate(() => sessionStorage.setItem("tf-draft:backups", JSON.stringify({ "v.BACKUP_KEEP": "6" })));
  await page.goto(app.url + "/devices/backups?restore=1");
  await expect(fold).toHaveAttribute("open", "");
  await expect(page.locator("input[name='v.BACKUP_KEEP']")).toHaveValue("6");
  // a restored draft is an unsaved change (#949): the save bar is on screen for it
  await expect(page.locator("form#sysform [data-sys-bar]")).toBeVisible();
  // without ?restore=1 the draft is dropped and the fold stays closed
  await page.goto(app.url + "/devices/backups");
  await expect(fold).not.toHaveAttribute("open", "");
  await expect(page.locator("form#sysform [data-sys-bar]")).toBeHidden();
  // an empty time of day is refused by the browser: close the fold, press Save, and it opens
  await openSchedule(page);
  await page.locator("input[name='v.BACKUP_TIME']").fill("");
  await fold.locator("summary").click();
  await expect(fold).not.toHaveAttribute("open", "");
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(fold).toHaveAttribute("open", "");
  await expect(page).toHaveURL(/\/devices\/backups$/);
});

test("an operator sees the page read-only", async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/devices/backups");
  await expect(page.getByRole("heading", { name: "Backups", level: 1 })).toBeVisible();
  await expect(page.getByRole("button", { name: "Back up all now" })).toHaveCount(0);
  await expect(page.locator(".bk-table tbody input[type=checkbox]")).toHaveCount(0);
  await expect(page.locator("input[name='v.BACKUP_KEEP']")).toHaveCount(0);
  await expect(page.getByText("Admins change the schedule")).toBeVisible();
  await expect(page.getByRole("button", { name: "Save settings" })).toHaveCount(0);
});

test("after a scheduled run the status bar links to the page and the last run is listed", async ({ page }) => {
  test.setTimeout(240_000);
  await page.setExtraHTTPHeaders(admin);
  // The page is loaded once: the run, started two minutes after the app, shows up on it by itself (#950)
  await page.goto(app.url + "/devices/backups");
  await expect(page.locator("#card-lastrun")).toContainText("Backed up", { timeout: 200_000 });
  await page.goto(app.url + "/");
  const item = page.locator(".sb-wide a.sb-i, footer .sb-wide a.sb-i").filter({ hasText: "Backup" }).first();
  await expect(item).toHaveAttribute("href", /\/devices\/backups$/);
});

// Why (#950): during a backup run the page is the place to watch it: the backup count of a device and the
// totals of the Storage card must follow a backup that is taken elsewhere, without a reload. A reload would
// also drop what the reader is doing, so the ticked row stays, the schedule form is not touched (its save
// bar stays away), and the swap waits while a row menu or the Storage fold is open and comes once it is
// closed. The tests below use other devices each (a manual backup of a device waits a minute before the next)
// that has a backup from the scheduled run above.
function liveRow(page: Page, name: string) {
  return page.locator(`.bk-table tbody tr:has(td.dv-name[title="${name}"])`);
}
async function backedUp(page: Page, n: number): Promise<{ name: string; id: string }> {
  const row = page.locator(".bk-table tbody tr").filter({ has: page.locator("td[data-label='Last result']", { hasText: /^\s*OK\s*$/ }) }).nth(n);
  const name = (await row.locator("td.dv-name").getAttribute("title"))!;
  const id = (await row.locator("input[name=id]").inputValue())!;
  return { name, id };
}
async function backUpElsewhere(page: Page, id: string) {
  const csrf = await page.locator("input[name=csrf]").first().inputValue();
  const took = await page.request.post(`${app.url}/devices/${id}/backup`, { headers: admin, form: { csrf }, maxRedirects: 0 });
  expect(took.status(), "backup taken").toBe(303);
}
const runsOf = (row: Locator) => row.locator("td[data-label=Runs]");
const storageBackups = (page: Page) => page.locator("#card-storage .sys-sum", { has: page.locator(".sys-sum-label", { hasText: /^Backups$/ }) }).locator(".sys-sum-value");
const liveReload = (page: Page) => page.waitForResponse((r) => r.url().includes("/devices/backups") && r.request().method() === "GET" && r.request().headers()["hx-request"] === "true");
// A mark inside the Devices card: a swap of the card's insides takes it away, whatever the new content is.
const markCard = (page: Page) => page.evaluate(() => document.querySelector("#card-devices .sbody")!.insertAdjacentHTML("beforeend", '<i id="swap-mark" hidden></i>'));

test("a backup taken elsewhere shows on the open page without a reload, the ticked row and the schedule stay (#950)", async ({ page }) => {
  test.setTimeout(90_000);
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices/backups");
  const consoleErrors: string[] = [];
  page.on("console", (m) => m.type() === "error" && consoleErrors.push(m.text()));
  const navigations: string[] = [];
  page.on("framenavigated", (f) => f === page.mainFrame() && navigations.push(f.url()));

  // two devices at once: the events come within one second, and the page must end with both (a throttle on the
  // trigger would show the first and drop the last)
  const target = await backedUp(page, 0);
  const second = await backedUp(page, 3);
  const other = page.locator(".bk-table tbody tr").filter({ hasNot: page.locator(`td.dv-name[title="${target.name}"], td.dv-name[title="${second.name}"]`) }).first();
  const otherID = await other.locator("input[name=id]").inputValue();
  await other.locator("input[name=id]").check();
  const runs = async (name: string) => Number((await runsOf(liveRow(page, name)).locator(".mono").innerText()).trim());
  const before = [await runs(target.name), await runs(second.name)];
  const totalBefore = Number(await storageBackups(page).innerText());

  await Promise.all([backUpElsewhere(page, target.id), backUpElsewhere(page, second.id)]);

  await expect(runsOf(liveRow(page, target.name)).locator(".mono")).toHaveText(String(before[0] + 1));
  await expect(runsOf(liveRow(page, second.name)).locator(".mono")).toHaveText(String(before[1] + 1));
  await expect(storageBackups(page)).toHaveText(String(totalBefore + 2));
  // the tick survived the swap, and the button that acts on it still counts it
  await expect(page.locator(`.bk-table input[name=id][value="${otherID}"]`)).toBeChecked();
  await expect(page.getByRole("button", { name: "Back up selected (1)" })).toBeEnabled();
  // the schedule form was not swapped: nothing is unsaved
  await expect(page.locator("form#sysform [data-sys-bar]")).toBeHidden();
  expect(navigations, "the page was not reloaded").toEqual([]);
  expect(consoleErrors, "htmx found every id it swaps").toEqual([]);
});

test("a row menu that is open holds the live update until it is closed (#950)", async ({ page }) => {
  test.setTimeout(90_000);
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices/backups");
  const target = await backedUp(page, 1);
  const row = liveRow(page, target.name);
  await row.locator("summary").click();
  await expect(row.locator("details[open]")).toHaveCount(1);
  await markCard(page);

  const answered = liveReload(page);
  await backUpElsewhere(page, target.id);
  await answered; // the reload was asked for and answered: only the swap waits
  await page.waitForTimeout(500);
  await expect(row.locator("details[open]"), "the menu was not swapped away").toHaveCount(1);
  await expect(page.locator("#swap-mark")).toHaveCount(1);

  await row.locator("summary").click(); // closed: the held reload comes, once
  await expect(page.locator("#swap-mark")).toHaveCount(0);
});

test("an open Storage fold holds the live update the same way (#950)", async ({ page }) => {
  test.setTimeout(90_000);
  await page.setExtraHTTPHeaders(admin);
  // one kept backup per device and none protected by its age: the first device has more, so the Storage card
  // lists it for pruning, in a fold
  await page.goto(app.url + "/devices/backups");
  await openSchedule(page);
  await page.locator("input[name='v.BACKUP_KEEP']").fill("1");
  await page.locator("input[name='v.BACKUP_MAX_AGE']").fill("0h");
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page).toHaveURL(/\/devices\/backups\?ok=saved/);
  const fold = page.locator("#card-storage details.fold");
  await expect(fold).toHaveCount(1);
  const target = await backedUp(page, 2);
  await fold.locator("summary").click();
  await expect(fold).toHaveAttribute("open", "");
  await markCard(page);

  const answered = liveReload(page);
  await backUpElsewhere(page, target.id);
  await answered;
  await page.waitForTimeout(500);
  await expect(fold, "the fold was not swapped away").toHaveAttribute("open", "");
  await expect(page.locator("#swap-mark")).toHaveCount(1);

  await fold.locator("summary").click();
  await expect(page.locator("#swap-mark")).toHaveCount(0);
});

// Why (#934): the status bar is one nowrap row on every page. While a backup run is in progress it
// holds the Broker, the running job (progress and "as of" on a page without a stream), the last
// run's result and the version. When together they are wider than the window, the items must give
// way inside their own boxes: a Broker host that runs under the job text, or last-run counts that
// run under the version, are drawn over each other and the next item's background cuts a word in
// two. Items that shrink must clip (ellipsis), and what identifies an item (its dot and its bold
// label) must stay visible. The test runs last: the last run is in the bar (the test above waited
// for it), and it adds a frozen backup job that the engine never touches (it resumes active jobs
// only at its start). That job stands in for the scheduled run's own item (the in-memory run that
// the bar adds as a job without a link): sbJobs renders both the same way.
test("while a backup run is in progress the status bar keeps every item inside its own box (#934)", async ({ page }) => {
  test.setTimeout(120_000);
  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
  try {
    db.exec("PRAGMA busy_timeout=5000");
    const ids = (db.prepare("SELECT id FROM devices ORDER BY id LIMIT 3").all() as { id: number }[]).map((r) => r.id);
    expect(ids.length, "the imported fleet has three devices").toBe(3);
    const now = Date.now();
    db.prepare("INSERT INTO ota_batches (id, created_by, created_at) VALUES (900, 'scheduler', ?)").run(now);
    const job = db.prepare(
      `INSERT INTO ota_jobs (batch_id, device_id, kind, state, stage, target_release, target_variant, created_by, created_at, updated_at)
       VALUES (900, ?, 'backup', ?, 'final', '', '', 'scheduler', ?, ?)`,
    );
    ids.forEach((id, i) => job.run(id, i === 0 ? "succeeded" : "backup", now, now));
  } finally {
    db.close();
  }
  await page.setExtraHTTPHeaders(admin);
  const bar = async (url: string) => {
    await page.goto(app.url + url);
    await expect(page.locator("#sb-wide .sb-jobs")).toContainText("Backup");
    await expect(page.locator("#sb-wide a.sb-i[href$='/devices/backups']")).toContainText("Scheduled backup"); // the last run's item
    await page.evaluate(() => document.fonts.ready);
  };
  // the widest row is the narrowest wide window: 720 px is where the phone layout takes over
  for (const width of [1280, 1024, 768, 720]) {
    await page.setViewportSize({ width, height: 900 });
    for (const url of ["/devices/backups", "/devices", "/"]) {
      await bar(url);
      const name = `${url} at ${width}px`;
      const o = await layoutGuards(page, ["o"]);
      expect(o, guardReport(name, o)).toEqual([]);
      const problems = await page.evaluate(() => {
        const row = document.querySelector("#sb-wide")!;
        const out: string[] = [];
        // the row ends inside the window: its items give way, they do not push it wider
        if (row.scrollWidth > row.clientWidth) out.push(`the row is ${row.scrollWidth - row.clientWidth}px wider than the window`);
        for (const item of row.querySelectorAll(":scope > .sb-i")) {
          const box = item.getBoundingClientRect();
          // the dot and the bold label identify the item: they stay inside its box when the rest is cut
          for (const e of item.querySelectorAll(".sb-dot, b")) {
            if (e.getBoundingClientRect().right > box.right + 0.5) out.push(`"${(e.textContent ?? "").trim() || "dot"}" ends ${Math.round(e.getBoundingClientRect().right - box.right)}px past its item`);
          }
        }
        return out;
      });
      expect(problems, name).toEqual([]);
    }
  }
  // a phone: one line, and the open sheet stays inside the window with its text apart. On the Dashboard:
  // the Backups table has its own phone finding (a "Failed" badge over its reason), not the bar's
  await page.setViewportSize({ width: 375, height: 800 });
  await bar("/");
  const line = (await page.locator("#sb-narrow summary").boundingBox())!;
  expect(line.height, "the bar is one 44px line").toBeLessThanOrEqual(45);
  const lines = await page.locator("#sb-narrow .sb-sum").evaluate((e) => {
    const r = document.createRange();
    r.selectNodeContents(e);
    return new Set([...r.getClientRects()].filter((q) => q.width > 0).map((q) => Math.round(q.top / 8))).size;
  });
  expect(lines, "the summary text is one line").toBe(1);
  await page.locator("#sb-narrow summary").click();
  const sheet = (await page.locator("#sb-narrow .sb-sheet").boundingBox())!;
  expect(sheet.x).toBeGreaterThanOrEqual(0);
  expect(sheet.x + sheet.width).toBeLessThanOrEqual(375);
  const o = await layoutGuards(page, ["o"]);
  expect(o, guardReport("the phone sheet", o)).toEqual([]);
});
