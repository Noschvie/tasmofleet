import { join } from "node:path";
import { test, expect } from "../support/fixtures";
import { insertProposals, publishRetained } from "../support/discovery-seed";
import { App, Devenv, startApp, startDevenv } from "../support/stack";

// #1110: a discovered row has one Add. It opens the device it added (as Add by address does), and for a device
// behind a web password it opens the address form for that password instead of adding a device TasmoFleet cannot
// log in to.
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
let app: App;
let env: Devenv;

const offline = { mac: "aa:bb:cc:00:11:01", name: "Cellar fan", topic: "cellar_fan", host: "cellar-fan-1a2b", ip: "127.0.0.61", seen: 60 };
const lockedMAC = "aa:bb:cc:00:11:02";

test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
    },
  });
  await insertProposals(app, [offline]);
  await publishRetained(env.vars.TASMOFLEET_MQTT_URL, `tele/${offline.topic}/LWT`, "Offline");
  // A scan sighting of the simulator behind a web password, at its real port: the MAC check of Add asks it without
  // credentials and gets 401.
  const [host, port] = env.devices["locked-plug"].addr.split(":");
  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
  try {
    db.exec("PRAGMA busy_timeout=5000");
    const now = Date.now();
    db.prepare(
      `INSERT INTO discovery_proposals (mac, kind, via, reason, name, address, port, topic, full_topic, existing_id, dismissed, first_seen, seen_at, hostname)
       VALUES (?, 'new', 'scan', '', 'Locked plug', ?, ?, '', '', 0, 0, ?, ?, 'locked-plug')`,
    ).run(lockedMAC, host, Number(port), now - 3600_000, now - 30_000);
  } finally {
    db.close();
  }
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});
test.beforeEach(async ({ page }) => {
  await page.setExtraHTTPHeaders(adam);
  await page.setViewportSize({ width: 1280, height: 900 });
});

const row = (page: import("@playwright/test").Page, name: string) => page.locator(".add-disc-table").getByRole("row").filter({ hasText: name });

// Why (A3-016): after Add the person stayed on the list with a toast and had to look for the device; Add by address
// already opened it. Both ways end on the device now, where the next step (a name, a backup) is.
test("Add on a discovered device opens the device it added", async ({ page }) => {
  await page.goto(app.url + "/devices/new");
  await expect(row(page, offline.name).getByRole("button")).toHaveText(["Ignore", "Add"]);
  await row(page, offline.name).getByRole("button", { name: "Add", exact: true }).click();
  await expect(page).toHaveURL(/\/devices\/\d+\?ok=added$/);
  await expect(page.getByRole("heading", { level: 1 })).toContainText(offline.name);
});

// Why (#1110, decision 9): the device answers the MAC check with 401, so it is behind a web password. Add must not
// create a device that every poll would be refused at; it opens the address form filled from the device, says what
// is missing at the password field, and with the password typed there the device is added and opened.
test("Add on a device behind a web password opens the form for it, and the password adds it", async ({ page }) => {
  await page.goto(app.url + "/devices/new");
  await row(page, "Locked plug").getByRole("button", { name: "Add", exact: true }).click();
  await expect(page).toHaveURL(/\/devices\/new\?proposal=\d+&need=password$/);
  const manual = page.locator("#card-manual");
  const pw = manual.getByLabel("Web password");
  await expect(manual).toContainText("This device needs its web password.");
  await expect(pw).toHaveAttribute("aria-invalid", "true");
  await expect(pw).toBeFocused();
  await expect(manual.locator("input[name=address]")).toHaveValue(env.devices["locked-plug"].addr.split(":")[0]);
  await expect(row(page, "Locked plug"), "nothing was added: the device is still offered").toHaveCount(1);
  await pw.fill("devenv-fake-password");
  await manual.getByRole("button", { name: "Add device" }).click();
  await expect(page).toHaveURL(/\/devices\/\d+\?ok=added$/);
});
