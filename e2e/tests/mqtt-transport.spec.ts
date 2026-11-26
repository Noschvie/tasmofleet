import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { quickPick } from "../support/updates";
import { expectNoOverflow } from "../support/layout";
import { ACL_LEAD, aclNotice, aclShots } from "../support/aclshots";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Console and "Make compliant" over MQTT (0.20.0, #79, #256): mq-esp32 is a simulated
// device whose HTTP port refuses every connection but which is on the broker. TasmoFleet reaches it
// only through MQTT (the second device, mq-esp8266-1m, is the same kind).
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv(["-only-mqtt-transport"]);
  const fwPort = await freePort(); // the transaction pages come with the firmware server
  app = await startApp({
    env: {
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_URL,
      TASMOFLEET_POLICY_RULES: JSON.stringify([{ command: "PowerOnState", want: "2", note: "e2e target" }]),
    },
  });
  // two devices: the Columns menu (used to see when the group topics are read) needs a second row
  const only = {
    "mq-esp32": env.devices["mq-esp32"],
    "mq-esp8266-1m": env.devices["mq-esp8266-1m"],
    "mq-fw-blocked": env.devices["mq-fw-blocked"],
  };
  expect(app.cli(["import", "tasmoadmin", writeTasmoAdminDir(only)])).toContain("created: mq-esp32");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

test("console over MQTT on a device HTTP cannot reach", { tag: "@core" }, async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  await expect(page.getByRole("row").filter({ hasText: "mq-esp32" }).getByText("Online", { exact: true })).toBeVisible({ timeout: 30_000 });
  await page.getByRole("link", { name: "mq-esp32", exact: true }).click();
  await page.getByRole("link", { name: "Console" }).click();
  const box = page.getByRole("textbox", { name: "Command" });
  // The device becomes ours on the broker a moment after the import: the first send may still fail.
  await expect(async () => {
    await box.fill("Status 2");
    await page.getByRole("button", { name: "Send" }).click();
    await expect(page.getByText("StatusFWR")).toBeVisible({ timeout: 3_000 });
  }).toPass({ timeout: 60_000 });

  // A command that runs other commands could send Status 0 on our behalf: not over MQTT.
  await box.fill('Json {"Status":0}');
  await page.getByRole("button", { name: "Send" }).click();
  // Json hides its arguments, so its confirmation names the transport (#826): HTTP, the one the router tries while
  // the group topics are unread (then MQTT refuses it, and nothing is sent: no confirmation). Either way the answer is
  // the error text.
  const json = page.locator("section.devconfirm");
  await expect(json.or(page.getByRole("alert")).first()).toBeVisible();
  if (await json.isVisible()) {
    await expect(json).toContainText("The command goes to the device over HTTP, unencrypted.");
    await json.getByRole("button", { name: "Confirm and send" }).click();
  }
  await expect(page.getByRole("alert")).toBeVisible();
  await expect(page.getByText("StatusMEM")).toHaveCount(0); // what a Status 0 reply would have shown

  // Changing the topic of a device that only MQTT reaches can cut it off for good: it asks once.
  await box.fill("Topic somewhere-else");
  await page.getByRole("button", { name: "Send" }).click();
  await expect(page.getByText("reached over MQTT only")).toBeVisible();
});

test("a console command whose arguments the audit hides names its transport before it is sent", { tag: "@core" }, async ({ page }) => {
  test.setTimeout(180_000);
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: "mq-esp32", exact: true }).click();
  const devURL = page.url();
  // Changes go over MQTT once the group topics are read (the strict rule); until then the route is not MQTT.
  await expect(async () => {
    await page.goto(devURL);
    await expect(page.locator("#devpaths-actions")).toContainText(/changing\s*MQTT/, { timeout: 2_000 });
  }).toPass({ timeout: 120_000, intervals: [3_000] });
  await page.goto(devURL + "/console");
  const box = page.getByRole("textbox", { name: "Command" });
  const confirm = page.locator("section.devconfirm");
  // #826: the devenv's broker URL is tcp://, so the plain sentence; Cancel sends nothing.
  await box.fill("PulseTime1 10");
  await page.getByRole("button", { name: "Send" }).click();
  await aclShots(page, "console-confirm");
  await expect(confirm).toContainText("The command goes to the device over the MQTT broker, unencrypted; the broker also delivers it to every client subscribed to this device's commands, and without a broker ACL that is every client of the broker.");
  // #840, #1078: and the confirmation shows the broker ACL notice, the short warn banner of every page
  await expect(aclNotice(confirm)).toBeVisible();
  await expect(aclNotice(confirm)).toHaveClass(/\bt-warn\b/);
  await expect(aclNotice(confirm).locator("b")).toHaveText(ACL_LEAD);
  await confirm.getByRole("link", { name: "Cancel" }).click();
  await expect(confirm).toHaveCount(0);
  await expect(page.getByRole("log", { name: "Console output" }).getByText("PulseTime1")).toHaveCount(0);
  // Confirmed: it goes over MQTT, and the audit says so.
  await box.fill("PulseTime1 10");
  await page.getByRole("button", { name: "Send" }).click();
  await confirm.getByRole("button", { name: "Confirm and send" }).click();
  await expect(confirm).toHaveCount(0);
  await page.goto(app.url + "/audit");
  await expect(page.locator("table.audittable tr.detail-row", { hasText: "PulseTime1" }).filter({ hasText: /via\W+mqtt/ }).first()).toBeAttached();
});

test("Make compliant over MQTT", { tag: "@core" }, async ({ page }) => {
  test.setTimeout(300_000);
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  await expect(page.getByRole("row").filter({ hasText: "mq-esp32" }).getByText("Online", { exact: true })).toBeVisible({ timeout: 30_000 });
  // A write over MQTT needs every device's group topic (a message to a shared topic would reach other
  // devices). TasmoFleet reads them by itself shortly after the devices are known, and a change before
  // that is refused with the reason. So: try, and try again until the reads are in.
  const fixedRow = () => page.locator("table.audittable tr", { hasText: "Make compliant (device)" }).filter({ hasText: "PowerOnState" }).filter({ hasNotText: /refused|fail|unreachable/i });
  await expect(async () => {
    await page.goto(app.url + "/audit"); // a round may have fixed the device and failed on the other one
    if ((await fixedRow().count()) > 0) return;
    await page.goto(app.url + "/policy");
    await page.getByRole("button", { name: "Run now" }).click();
    await expect(page.locator(".banner", { hasText: /deviates? from a required rule/ })).toBeVisible({ timeout: 20_000 });
    // the banner's one action, Show, leads to the Deviating devices tab, where Make compliant is (#1091)
    await page.goto(app.url + "/policy/deviations");
    await page.getByRole("button", { name: /^Make compliant…/ }).click();
    // The devices take the DeviceName they report after a while, so the plan is not matched by name.
    await expect(page.locator(".pol-fix-cmd .mono").first()).toHaveText("PowerOnState 2");
    await page.getByRole("button", { name: /^Run \d+ changes?$/ }).click();
    await expect(page).toHaveURL(/\/transactions\/\d+/);
    await expect(page.locator(".upd-dev").getByText("Verified", { exact: true }).first()).toBeVisible({ timeout: 15_000 });
  }).toPass({ timeout: 240_000, intervals: [5_000] });
  await page.goto(app.url + "/audit");
  // The fix was written and succeeded; the audit says it travelled over MQTT (the device's HTTP port
  // refuses every connection).
  await expect(fixedRow().first()).toBeVisible();
  const detail = page.locator("table.audittable tr.detail-row", { hasText: "PowerOnState" }).filter({ hasText: /via\W+mqtt/ });
  expect(await detail.count()).toBeGreaterThan(0);
});

// Update over MQTT (0.20.0, #79, #259). The devices answer nothing over HTTP, so the preview probes
// them over MQTT; the language build is the locale ID of the Status 4 read there (#686); the update runs the
// same engine with the job pinned to MQTT. The backup before flashing runs over MQTT too (#257), so each
// device gets its MQTT password first (the devenv devices have the same one, the broker takes any).
// The devices take the DeviceName they report, so the picker is addressed by device id: the import
// creates them in the order of the object (mq-esp32 = 1, mq-esp8266-1m = 2, mq-fw-blocked = 3).
const deviceId = { "mq-esp32": 1, "mq-esp8266-1m": 2, "mq-fw-blocked": 3 } as const;
const withPassword = new Set<number>();
async function ensurePassword(page: import("@playwright/test").Page, id: number) {
  if (withPassword.has(id)) return;
  await page.goto(app.url + `/devices/${id}/edit`);
  await page.getByLabel("MQTT password source").selectOption("own");
  await page.getByLabel("Own MQTT password").fill("devenv-fake-mqtt-password");
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(page).toHaveURL(/ok=saved/);
  withPassword.add(id);
}

async function pickById(page: import("@playwright/test").Page, name: keyof typeof deviceId) {
  await ensurePassword(page, deviceId[name]);
  await page.goto(app.url + "/updates/new");
  await quickPick(page, "None");
  await page.locator(`input[name=id][value="${deviceId[name]}"]`).check();
}

async function reviewMQTTUpdate(page: import("@playwright/test").Page, name: keyof typeof deviceId) {
  const row = () => page.locator(".upd-prow");
  // HTTP counts as failing, and every device's group topic as read, only after the first health polls
  // and reads: review again until the device is probed over MQTT. A short action timeout lets one
  // failed round end (the picker lists a device as Offline until then) instead of eating the whole wait.
  page.setDefaultTimeout(8_000);
  await expect(async () => {
    await pickById(page, name);
    await page.locator("label.upd-relopt", { hasText: "Tasmota 16.1.0" }).click();
    await page.getByRole("button", { name: "Review update" }).click();
    await expect(row()).toBeVisible();
    await expect(row()).not.toContainText("probe failed", { timeout: 5_000 });
  }).toPass({ timeout: 150_000, intervals: [5_000] }).catch(async (e) => {
    throw new Error(`${e}\npicker/preview page: ${(await page.locator("main").innerText().catch(() => "?")).slice(0, 1500)}`);
  });
  page.setDefaultTimeout(30_000);
  return row();
}

test("update over MQTT (staged ESP8266) from the preview", { tag: "@core" }, async ({ page }) => {
  test.setTimeout(420_000);
  await page.setExtraHTTPHeaders(admin);
  const row = await reviewMQTTUpdate(page, "mq-esp8266-1m");
  // The language build is read over MQTT too (the Status 4 locale, #686): no choice is asked.
  await expect(row.locator("select")).toHaveCount(0);
  await expect(row).toContainText("Commands over MQTT, the image over HTTP from TasmoFleet");
  await expect(row).toContainText("Staged: minimal firmware first");
  await page.getByRole("button", { name: "Start update", exact: true }).click();
  await expect(page).toHaveURL(/\/transactions\/\d+$/);
  await expect(page.locator(".upd-row").getByText("Done", { exact: true })).toBeVisible({ timeout: 300_000 });
});

// FULL: a device that cannot reach the firmware server fails the update at once with that reason, and
// from then on the preview says there is no download path (until Forget or 24 h).
test("fw-blocked device shows no download path", async ({ page }) => {
  test.setTimeout(420_000);
  await page.setExtraHTTPHeaders(admin);
  await reviewMQTTUpdate(page, "mq-fw-blocked");
  await page.getByRole("button", { name: "Start update", exact: true }).click();
  await expect(page).toHaveURL(/\/transactions\/\d+$/);
  await expect(page.locator(".upd-row")).toContainText(/Failed|Done/, { timeout: 200_000 }).catch(async (e) => {
    throw new Error(`${e}\nlog: ${(await page.locator("#log-lines").innerText().catch(() => "?")).slice(0, 1500)}`);
  });
  await expect(page.locator("#log-lines"), "the job's log names the reason").toContainText("cannot reach the firmware server", { timeout: 5_000 });
  await pickById(page, "mq-fw-blocked");
  await page.locator("label.upd-relopt", { hasText: "Tasmota 16.1.0" }).click();
  await page.getByRole("button", { name: "Review update" }).click();
  await expect(page.locator(".upd-prow")).toContainText("no path");
  await expect(page.getByRole("button", { name: "Start update", exact: true })).toHaveCount(0);
});

// FULL: a device whose language build cannot be read (mq-esp32 reports a locale of no upstream build) is
// not guessed.
test("language choice required", async ({ page }) => {
  test.setTimeout(240_000);
  await page.setExtraHTTPHeaders(admin);
  const row = await reviewMQTTUpdate(page, "mq-esp32");
  // An ESP32 with the dual-core tasmota32 build has language builds too; whichever the row shows, it
  // must be a choice and not a pre-filled value.
  await expect(row).toContainText("Language build of this device");
  await expect(row.locator("select")).toHaveValue("");
});

// #257: backup over MQTT (FileDownload). The device's HTTP port refuses everything, so the backup can
// only come over MQTT, and only with the device's MQTT password (the devenv devices have one; the
// broker takes any).
test("backup over MQTT", async ({ page }) => {
  test.setTimeout(300_000);
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  // Either device will do (both are HTTP-dead, on the broker and have the devenv password); the earlier
  // tests may have renamed them to what they report, so the first row is taken, not a name.
  await expect(page.getByText("Online", { exact: true }).first()).toBeVisible({ timeout: 30_000 });
  await page.locator("table[data-table=devices] tbody tr a").first().click();
  const devicePath = new URL(page.url()).pathname;

  const backUp = page.getByRole("button", { name: "Back up", exact: true });
  await page.goto(app.url + devicePath + "/edit");
  await page.getByLabel("MQTT password source").selectOption("own");
  await page.getByLabel("Own MQTT password").fill("devenv-fake-mqtt-password");
  await page.getByRole("button", { name: "Save changes" }).click();

  // The group topics of both devices are read shortly after start; until then the backup is refused with the
  // reason, which starts only a 5 s floor, so retrying every 6 s is a bounded wait for those reads only.
  await expect(async () => {
    await page.goto(app.url + devicePath);
    await backUp.click();
    await expect(page.getByText(/Backup taken over MQTT/)).toBeVisible({ timeout: 5_000 });
  }).toPass({ timeout: 300_000, intervals: [6_000] });

  await page.goto(app.url + devicePath + "/backups");
  await expect(page.locator("section.bk-list")).toBeVisible();
  await expect(page.getByText("No backups yet")).toHaveCount(0);
  const audit = await page.request.get(app.url + "/audit", { headers: admin });
  expect(await audit.text()).not.toContain("devenv-fake-mqtt-password");
});
// The device page's connection card (#261): mq-esp32 refuses every HTTP connection, so the router
// remembers the failure and the card, the buttons and the list say how it is reached instead.
const olga = { "X-authentik-username": "olga", "X-authentik-groups": "ops" };

async function openDevicePage(page: Page, headers: Record<string, string>): Promise<void> {
  await page.setExtraHTTPHeaders(headers);
  // The devenv imports in a fixed order into a fresh database, so mq-esp32 is device 1; it takes the
  // name it reports after a while, so go by the id and check it is the one we mean (by address).
  await page.goto(app.url + "/devices/1");
  await expect(page.locator(".devhd-meta")).toContainText(env.devices["mq-esp32"].addr);
  // the failure is remembered after the first HTTP call that fails: reload until the card says so
  await expect(async () => {
    await page.reload();
    await expect(page.locator("#devpaths-facts-sum")).toContainText("HTTP not reachable", { timeout: 2_000 });
  }).toPass({ timeout: 60_000, intervals: [3_000] });
}

test("connection card explains paths", async ({ page }) => {
  await openDevicePage(page, admin);
  await page.locator("#devpaths-facts-sum").click();
  await page.locator("#devpaths-actions-sum").click();
  const card = page.locator("#devpaths");
  await expect(card.locator("#devpaths-facts").getByText("Not reachable")).toBeVisible();
  await expect(card.locator("#devpaths-facts").getByText("Experimental")).toBeVisible();
  // commands go over MQTT, and the list marks the device
  await expect(card.locator("#devpaths-actions").getByText("Over MQTT, because", { exact: false }).first()).toBeVisible();
  await page.goto(app.url + "/devices");
  await expect(page.locator("[data-via-mqtt]").first()).toBeVisible();
});

// #853: this device's backup goes over MQTT (its HTTP port refuses everything), so the notice stands by its Back up
// button, in the header of every tab of the device, once the device has the MQTT password a backup needs.
test("the broker ACL notice stands by Back up when the backup goes over MQTT", async ({ page }) => {
  test.setTimeout(120_000);
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices/1/edit");
  await page.getByLabel("MQTT password source").selectOption("own");
  await page.getByLabel("Own MQTT password").fill("devenv-fake-mqtt-password");
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(async () => {
    await page.goto(app.url + "/devices/1");
    await expect(page.getByRole("button", { name: "Back up", exact: true })).toBeEnabled({ timeout: 2_000 });
  }).toPass({ timeout: 90_000, intervals: [3_000] });
  const notice = aclNotice(page.locator("#devhd"));
  await expect(notice).toHaveCount(1);
  await expect(notice).toHaveClass(/\bt-warn\b/);
  // the notice is under the Back up button, not beside it, and nothing runs off the page
  const buttonBox = await page.getByRole("button", { name: "Back up", exact: true }).boundingBox();
  const noticeBox = await notice.boundingBox();
  expect(noticeBox!.y).toBeGreaterThanOrEqual(buttonBox!.y + buttonBox!.height);
  await aclShots(page, "device-back-up");
  for (const width of [1280, 768, 375]) {
    await page.setViewportSize({ width, height: 1000 });
    await expectNoOverflow(page, `device header at ${width}`);
  }
  await page.goto(app.url + "/devices/1/backups");
  await expect(aclNotice(page.locator("#devhd"))).toHaveCount(1);
  // #880, #962: the admin's hidden warning takes the notice from the header
  await page.goto(app.url + "/settings/system?tab=mqtt");
  await page.getByLabel("Hide the broker ACL warning").check();
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.goto(app.url + "/devices/1");
  await expect(page.getByRole("button", { name: "Back up", exact: true })).toBeVisible();
  await expect(aclNotice(page)).toHaveCount(0);
  await aclShots(page, "device-back-up-hidden");
});

test("disabled actions carry the reason", async ({ page }) => {
  await openDevicePage(page, admin);
  await page.locator("#devpaths-actions-sum").click();
  const row = page.locator("#devpaths-actions .ukv-row", { hasText: "Back up" }).first();
  const reason = (await row.locator(".devcard-note").innerText()).trim();
  const button = page.getByRole("button", { name: "Back up", exact: true });
  // without a way to back it up over MQTT the button says the same sentence as the card
  if (/^No path/.test(reason)) {
    await expect(button).toBeDisabled();
    // #1072: the sentence is text the button names, no title
    await expect(button).not.toHaveAttribute("title", /.+/);
    await expect(button).toHaveAttribute("aria-describedby", "devhd-why");
    await expect(page.locator("#devhd-why")).toContainText(reason);
  } else {
    await expect(button).toBeEnabled();
  }
});

test("operators see the card read-only", async ({ page }) => {
  await openDevicePage(page, olga);
  await page.locator("#devpaths-facts-sum").click();
  await expect(page.locator("#devpaths-facts")).toBeVisible();
  await expect(page.getByRole("button", { name: "Forget" })).toHaveCount(0);
});

test("an open fold stays open across a live refresh", async ({ page }) => {
  await openDevicePage(page, admin);
  const fold = page.locator("#devpaths details").first();
  await page.locator("#devpaths-facts-sum").click();
  await expect(fold).toHaveAttribute("open", "");
  const live = page.waitForResponse((r) => /\/devices\/\d+\/live(\?|$)/.test(r.url()));
  await page.evaluate(() => {
    const el = document.querySelector("#devlive")!;
    (window as unknown as { htmx: { trigger(e: Element, n: string): void } }).htmx.trigger(el, "sse:resync");
  });
  await live;
  await expect(fold).toHaveAttribute("open", "");
});

for (const width of [375, 1280]) {
  test(`connection card stacks at ${width}px without sideways scroll`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await openDevicePage(page, admin);
    await page.locator("#devpaths-facts-sum").click();
    await page.locator("#devpaths-actions-sum").click();
    await expectNoOverflow(page, "connection card");
  });
}
