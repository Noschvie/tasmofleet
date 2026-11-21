import { test, expect } from "../support/fixtures";
import { cardStackGaps, expectLayout } from "../support/layout";
import { pickForUpdate } from "../support/updates";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";
import type { Page } from "@playwright/test";

// The "5 · When" card of the update preview (#604): start now or at a chosen time, the plan's Starts row and
// the rule, in the browser's zone, and the server's refusal of a time that is not in the future.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
      TZ: "UTC", // the server's zone is behind the browsers below: the same digits are a different instant
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

/** preview opens the preview of an update of one device to Tasmota 16.1.0. */
async function preview(page: Page): Promise<void> {
  await page.setExtraHTTPHeaders(admin);
  // the picker offers the device once the first health poll has seen it
  await expect(async () => {
    await page.goto(app.url + "/updates/new");
    await expect(page.getByLabel("Select lamp-1m", { exact: true })).toBeEnabled({ timeout: 1000 });
  }).toPass({ timeout: 30_000 });
  await pickForUpdate(page, app.url, ["lamp-1m"]);
  await page.locator("label.upd-relopt", { hasText: "Tasmota 16.1.0" }).click();
  await page.getByRole("button", { name: "Review update" }).click();
  await expect(page.getByRole("heading", { name: "5 · When" })).toBeVisible();
  await expect(goButton(page)).toBeVisible(); // the device goes ahead: there is something to start
}

const startsRow = (page: Page) => page.locator("[data-starts]");
const goButton = (page: Page) => page.locator("button[form=start]");
const dateField = (page: Page) => page.locator("input[name=at_date]");
const timeField = (page: Page) => page.locator("input[name=at_time]");
const rule = (page: Page) => page.locator("[data-starts-rule]");

// Why: the person asked for an update, not for a plan. Start now is the default, and only choosing a time
// changes the plan, the rule and the button. The time is the one on the person's clock, whatever the zone of
// the server: a pick posts the instant with the browser's offset.
test.describe("in a browser at UTC+5:30", () => {
  test.use({ timezoneId: "Asia/Kolkata", locale: "en-US" });

  test("Start now is the default; a pick plans the update in the browser's zone", { tag: "@core" }, async ({ page }) => {
    await preview(page);
    await expect(page.getByLabel("Start now")).toBeChecked();
    await expect(startsRow(page)).toHaveText("Now · when you press Start update");
    await expect(goButton(page)).toHaveText("Start update");
    await expect(rule(page)).toBeHidden();
    await expect(page.locator(".upd-when-at")).toBeHidden();

    await page.getByLabel("Start at a chosen time").check();
    await expect(page.locator(".upd-when-at")).toBeVisible();
    await expect(startsRow(page)).toContainText("Not set yet: choose a quick pick or a time");
    await page.getByText("Tonight 03:00").click();
    await expect(startsRow(page)).toHaveText(/^\w{3}, \w{3} \d+ at 03:00 · in \d+( h| min| d \d+ h)$/);
    await expect(goButton(page)).toHaveText("Schedule update");
    await expect(rule(page)).toContainText("If TasmoFleet is down at 03:00, the update starts as soon as TasmoFleet is back, but only until 04:00 (60 minutes after the start time).");
    await expect(page.locator("[data-tz]")).toContainText("Times are in your time zone (");
    await expect(page.locator("[data-tz]")).toContainText("UTC+5:30");
    // the instant that goes to the server carries the browser's offset
    await expect(page.locator("input[name=start_at]")).toHaveValue(/T03:00:00\+05:30$/);

    await page.getByText("Tomorrow night 03:00").click();
    await expect(page.locator("input[name=start_at]")).toHaveValue(/T03:00:00\+05:30$/);
    await page.getByLabel("Start now").check();
    await expect(startsRow(page)).toHaveText("Now · when you press Start update");
    await expect(goButton(page)).toHaveText("Start update");
    await expect(rule(page)).toBeHidden();
    await expect(page.locator("input[name=start_at]")).toHaveValue("");
  });

  // Why: this is the point of the card. A person who picks a time and presses "Schedule update" must end up with
  // a job that waits for that time, not one that starts at once on the device (a firmware update nobody wanted
  // yet), and the instant must be the one on their clock, not the server's (UTC here, five and a half hours
  // behind). The engine holds the device until the start; the transaction page shows it as planned.
  test("Schedule update plans the job for the picked time and nothing starts on the device", { tag: "@core" }, async ({ page }) => {
    await preview(page);
    await page.getByLabel("Start at a chosen time").check();
    await page.getByText("Tomorrow night 03:00").click();
    const instant = await page.locator("input[name=start_at]").inputValue();
    expect(instant).toMatch(/T03:00:00\+05:30$/);
    await expect(goButton(page)).toHaveText("Schedule update");

    await goButton(page).click();
    await expect(page).toHaveURL(/\/transactions\/\d+$/);
    const txnUrl = page.url();
    const row = page.locator(".upd-row").filter({ hasText: "lamp-1m" });
    await expect(row.locator(".upd-row-state")).toContainText("Planned"); // the job waits; it is not "Queued" or "Checking"
    await page.waitForTimeout(3000); // an update that was started at once would have left "Planned" by now
    await expect(row.locator(".upd-row-state")).toContainText("Planned");

    // the audit entry records the planned start, in UTC, as the instant the browser sent
    await page.goto(app.url + "/audit");
    const entry = page.locator("table.audittable tbody tr", { hasText: "Start update" }).first();
    await entry.locator("summary").click();
    const planned = new Date(instant).toISOString().replace(".000Z", "Z");
    await expect(entry.locator("xpath=following-sibling::tr[1]")).toContainText(`"start_at": "${planned}"`);

    // Why (#796): the plan holds lamp-1m, and the picker blocks a planned device. The tests below share this app
    // and need lamp-1m, so this one leaves nothing planned: it cancels its own plan.
    await page.goto(txnUrl);
    await page.getByRole("link", { name: /^Cancel TX-/ }).click();
    await page.locator("#dlg-txn").getByRole("button", { name: "Cancel schedule" }).click();
    await expect(page).toHaveURL(txnUrl);
    await expect(row.locator(".upd-row-state")).toHaveText("Cancelled");
  });

  // Why: the page calls a typed time past, so the server must refuse it too. If the page posted nothing for it,
  // the server would read the typed digits in its own zone, which is behind this browser's: an hour ago here is
  // hours in the future there, and a firmware update would be planned for an hour nobody chose.
  test("a time the page calls past is refused by the server, not read in its zone", async ({ page }) => {
    await preview(page);
    await page.getByLabel("Start at a chosen time").check();
    await page.getByText("Other time").click();
    const hourAgo = await page.evaluate(() => {
      const d = new Date(Date.now() - 3600_000);
      const p = (n: number) => String(n).padStart(2, "0");
      return { date: `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`, time: `${p(d.getHours())}:${p(d.getMinutes())}` };
    });
    await dateField(page).fill(hourAgo.date);
    await timeField(page).fill(hourAgo.time);
    await expect(page.locator("#when-err")).toContainText("Choose a time in the future. It is now ");
    await expect(page.locator("input[name=start_at]")).toHaveValue(/\+05:30$/); // the instant, though it is past

    await goButton(page).click();
    await expect(page).toHaveURL(/\/updates$/);
    await expect(page.locator("#when-err")).toContainText("Choose a time in the future. It is now ");
    await expect(startsRow(page)).toContainText("Not set: the time is in the past");
    await expect(dateField(page)).toHaveValue(hourAgo.date);
    await expect(timeField(page)).toHaveValue(hourAgo.time);
  });
});

// Why: a time that is not in the future would start the update at once. The page says so as soon as the
// time is typed, and the server refuses it anyway (a script, an old tab, the clock of the phone): the
// preview comes back with the choice kept and nothing started.
test("a time in the past is refused by the page and by the server", async ({ page }) => {
  await preview(page);
  await page.getByLabel("Start at a chosen time").check();
  await page.getByText("Other time").click();
  await dateField(page).fill("2020-01-02");
  await timeField(page).fill("08:00");
  await expect(page.locator("#when-err")).toContainText("Choose a time in the future. It is now ");
  await expect(startsRow(page)).toContainText("Not set: the time is in the past");
  await expect(dateField(page)).toHaveAttribute("aria-invalid", "true");

  await goButton(page).click();
  await expect(page).toHaveURL(/\/updates$/);
  await expect(page.getByRole("heading", { name: "5 · When" })).toBeVisible();
  await expect(page.locator("#when-err")).toBeVisible();
  await expect(page.locator("#when-err")).toContainText("Choose a time in the future. It is now ");
  await expect(startsRow(page)).toContainText("Not set: the time is in the past");
  await expect(dateField(page)).toHaveValue("2020-01-02"); // the input stays
  await expect(timeField(page)).toHaveValue("08:00");
  await expect(page.getByRole("heading", { name: /^4 · Preview/ })).toBeVisible(); // the devices are still there
});

// Why: on a phone the plan sits under the devices; the buttons are the last thing, and nothing runs off the
// page's side.
test("on a phone the order is preview, When, plan, buttons", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await preview(page);
  const top = async (sel: string) => (await page.locator(sel).first().boundingBox())!.y;
  const ys = [await top("#prev-h"), await top("#when-h"), await top("#what-h"), await top(".upd-go button[form=start]")];
  expect(ys).toEqual([...ys].sort((a, b) => a - b));
  expect(new Set(ys).size).toBe(4);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
});

// Why (#592): the nightly layout guards only open pages by GET, and the preview is an answer to a POST, so
// the new card is checked here at the two tablet widths, with its choices open and with the refusal showing.
for (const width of [768, 1024]) {
  test(`the When card passes the layout guards and the card stacks at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await preview(page);
    await page.getByLabel("Start at a chosen time").check();
    await page.getByText("Tonight 03:00").click();
    await expectLayout(page, `preview, Tonight ${width}px`);
    await page.getByText("Other time").click();
    await dateField(page).fill("2020-01-02");
    await timeField(page).fill("08:00");
    await expect(page.locator("#when-err")).toBeVisible();
    await expectLayout(page, `preview, past time ${width}px`);
    expect(await cardStackGaps(page), "card stacks").toEqual([]);
  });
}

// Why (fail safe: nothing may start earlier than chosen): a recheck (a language or build choice, Change, Forget)
// posts a form of its own. The time the person chose must survive it, and a plan must never come back as "Start
// now" with the main button armed to flash at once.
test.describe("a recheck of the preview", () => {
  test.use({ timezoneId: "Asia/Kolkata", locale: "en-US" });

  test("keeps the chosen time, and keeps Start now when that was the choice", async ({ page }) => {
    await preview(page);
    await page.getByLabel("Start at a chosen time").check();
    await page.getByText("Other time").click();
    await dateField(page).fill("2999-01-02");
    await timeField(page).fill("04:15");
    await expect(page.locator("input[name=start_at]")).toHaveValue("2999-01-02T04:15:00+05:30");
    await page.evaluate(() => (document.getElementById("recheck") as HTMLFormElement).requestSubmit());
    await expect(page.getByLabel("Start at a chosen time")).toBeChecked();
    await expect(dateField(page)).toHaveValue("2999-01-02");
    await expect(timeField(page)).toHaveValue("04:15");
    await expect(goButton(page)).toHaveText("Schedule update");

    await page.getByLabel("Start now").check();
    await page.evaluate(() => (document.getElementById("recheck") as HTMLFormElement).requestSubmit());
    await expect(page.getByLabel("Start now")).toBeChecked();
    await expect(goButton(page)).toHaveText("Start update");
  });
});

// Why (fail safe: nothing may start earlier than chosen): on the night the clocks go back the wall clock 02:30
// happens twice, and parsing the date and time fields picks the earlier. A plan for the second 02:30 that
// "Plan again…" preselects must be planned for that instant when the person presses the button, not for the
// one an hour before it.
test.describe("in a browser where the clocks go back", () => {
  test.use({ timezoneId: "Europe/Berlin", locale: "en-US" });

  test("a preselected time of the repeated hour keeps its instant", async ({ page }) => {
    await page.clock.install({ time: new Date("2026-10-20T12:00:00Z") });
    const second = "2026-10-25T02:30:00+01:00"; // 01:30 UTC: the second 02:30 of that night
    await page.route("**/updates/preview", async (route) => {
      const resp = await route.fetch();
      const body = (await resp.text()).replace('aria-labelledby="when-h"', `aria-labelledby="when-h" data-preset="${second}"`);
      await route.fulfill({ response: resp, body });
    });
    await preview(page);
    await page.getByLabel("Start at a chosen time").check();
    await page.getByText("Other time").click();
    await expect(dateField(page)).toHaveValue("2026-10-25");
    await expect(timeField(page)).toHaveValue("02:30");
    await expect(page.locator("input[name=start_at]")).toHaveValue(second);
    // typing something else is read as the wall clock again
    await timeField(page).fill("03:30");
    await expect(page.locator("input[name=start_at]")).toHaveValue("2026-10-25T03:30:00+01:00");
  });
});

// Why: without JavaScript nothing recomputes the picks or reads the zone, so the page must say the fields
// are read in the server's zone, and the server must still refuse a time in the past.
test.describe("without JavaScript", () => {
  test.use({ javaScriptEnabled: false });

  test("the fields are the server's zone and a past time is refused", async ({ page }) => {
    await preview(page);
    await expect(page.locator("[data-tz]")).toContainText("Times are in the server's time zone (");
    await page.getByLabel("Start at a chosen time").check();
    await expect(page.locator(".upd-when-at")).toBeVisible(); // the radio alone shows the choices
    await page.getByText("Other time").click();
    await dateField(page).fill("2020-01-02");
    await timeField(page).fill("08:00");
    await expect(goButton(page)).toHaveText("Start update"); // nothing rewrites it without a script
    await goButton(page).click();
    await expect(page.locator("#when-err")).toContainText("Choose a time in the future. It is now ");
    await expect(startsRow(page)).toContainText("Not set: the time is in the past");
    await expect(goButton(page)).toHaveText("Schedule update");
  });
});
