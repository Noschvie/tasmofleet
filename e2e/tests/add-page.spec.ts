import { connect } from "node:net";
import { join } from "node:path";
import { test, expect } from "../support/fixtures";
import { expectNoOverflow } from "../support/layout";
import { App, Devenv, startApp, startDevenv } from "../support/stack";

// #410: Devices -> Add is one page with four boxes (Discovered + Network scan wide, Add by address +
// Import narrow), no tabs. Every way of adding a device still works from its box.
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
let app: App;
let env: Devenv;

const seeded = [
  { mac: "aa:bb:cc:00:02:01", name: "Shed light", topic: "shed_light", host: "shed-light-1a2b", ip: "127.0.0.21", seen: 600 },
  { mac: "aa:bb:cc:00:02:02", name: "Porch plug", topic: "porch_plug", host: "porch-plug-3c4d", ip: "127.0.0.22", seen: 60 },
];

function publishRetained(url: string, topic: string, payload: string): Promise<void> {
  const { hostname, port } = new URL(url);
  const str = (s: string) => Buffer.concat([Buffer.from([s.length >> 8, s.length & 255]), Buffer.from(s)]);
  const packet = (head: number, body: Buffer) => Buffer.concat([Buffer.from([head, body.length]), body]);
  return new Promise((ok, fail) => {
    const sock = connect(Number(port), hostname, () => {
      sock.write(packet(0x10, Buffer.concat([str("MQTT"), Buffer.from([4, 2, 0, 60]), str("e2e-add-page")])));
    });
    sock.once("data", () => {
      sock.write(packet(0x31, Buffer.concat([str(topic), Buffer.from(payload)])));
      sock.end(Buffer.from([0xe0, 0]), () => ok());
    });
    sock.once("error", fail);
  });
}

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
  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
  try {
    db.exec("PRAGMA busy_timeout=5000");
    const now = Date.now();
    for (const d of seeded) {
      db.prepare(
        `INSERT INTO discovery_proposals (mac, kind, via, reason, name, address, port, topic, full_topic, existing_id, dismissed, first_seen, seen_at, hostname)
         VALUES (?, 'new', 'mqtt', '', ?, ?, 80, ?, 'tasmota/%topic%/%prefix%/', 0, 0, ?, ?, ?)`,
      ).run(d.mac, d.name, d.ip, d.topic, now - 3600_000, now - d.seen * 1000, d.host);
    }
  } finally {
    db.close();
  }
  for (const d of seeded) await publishRetained(env.vars.TASMOFLEET_MQTT_URL, `tele/${d.topic}/LWT`, "Offline");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});
test.beforeEach(async ({ page }) => {
  await page.setExtraHTTPHeaders(adam);
});

const box = (page: import("@playwright/test").Page, id: string) => page.locator(`#card-${id}`);

// Why: the four ways to add devices are one page, so there are fewer clicks and fewer menus: Discovered and
// Network scan in the wide column on the left, Add by address and Import in the narrow one on the right, no
// tabs. Add devices is the List's button and no menu entry (#1106), so the sub-navigation says List · Policy ·
// Device backups with List lit, and the trail names the page under List.
test("four boxes in two columns, no tabs, the menu says List, Policy, Device backups", { tag: "@core" }, async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/devices/new");
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Add devices");
  await expect(page.locator("nav.utabs")).toHaveCount(0);
  const sub = page.getByRole("navigation", { name: "Devices" });
  await expect(sub.getByRole("link")).toHaveText([/^List$/, /^Policy$/, /^Device backups$/]);
  await expect(sub.getByRole("link", { name: "List" })).toHaveAttribute("aria-current", "page");
  await expect(page.locator("#crumbs")).toHaveText(/^\s*Devices\s*\/\s*List\s*\/\s*Add devices\s*$/);
  const b = async (id: string) => (await box(page, id).boundingBox())!;
  const [disc, scan, manual, imp] = [await b("discovered"), await b("scan"), await b("manual"), await b("import")];
  expect(disc.x).toBeLessThan(manual.x);
  expect(Math.abs(scan.x - disc.x), "Network scan under Discovered").toBeLessThan(2);
  expect(Math.abs(imp.x - manual.x), "Import under Add by address").toBeLessThan(2);
  expect(scan.y).toBeGreaterThan(disc.y + disc.height - 1);
  expect(imp.y).toBeGreaterThan(manual.y + manual.height - 1);
  expect(disc.width, "the Discovered box is the wide one").toBeGreaterThan(manual.width * 1.7);
  // the Discovered table has no MAC, Hostname or First seen column any more
  for (const gone of ["Hostname", "MAC", "First seen"]) await expect(box(page, "discovered").getByRole("columnheader", { name: gone })).toHaveCount(0);
  await expectNoOverflow(page, "Add devices");
});

// Why: on a phone the order is the one a person reads: Discovered, Add by address, Network scan, Import.
test("phone: Discovered, Add by address, Network scan, Import, one column", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 800 });
  await page.goto(app.url + "/devices/new");
  const ys: number[] = [];
  for (const id of ["discovered", "manual", "scan", "import"]) {
    const r = (await box(page, id).boundingBox())!;
    expect(r.width, `${id} fills the phone`).toBeGreaterThan(330);
    ys.push(r.y);
  }
  expect(ys).toEqual([...ys].sort((x, y) => x - y));
  await expectNoOverflow(page, "Add devices phone");
});

// Why: Ignore, Show ignored and Show again of a discovered device must keep working from the box, and the row keeps
// the two actions that differ: Ignore and Add (the second "Add…" is gone, #1110; add-devices-accept.spec.ts adds).
test("Discovered: ignore, show ignored, show again, and the row offers Ignore and Add", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/devices/new");
  const row = (name: string) => box(page, "discovered").getByRole("row").filter({ hasText: name });
  await row("Porch plug").getByRole("button", { name: "Ignore", exact: true }).click();
  await expect(page.getByText("Ignored.", { exact: true })).toBeVisible();
  await expect(row("Porch plug")).toHaveCount(0);
  await box(page, "discovered").getByRole("checkbox", { name: /Show ignored/ }).click();
  await expect(box(page, "discovered").getByRole("checkbox", { name: /Show ignored/ })).toBeChecked();
  await row("Porch plug").getByRole("button", { name: "Show again" }).click();
  await expect(page.getByText("Shown again.", { exact: true })).toBeVisible();
  await expect(row("Porch plug").getByRole("button")).toHaveText(["Ignore", "Add"]);
  await expect(row("Porch plug").getByRole("link")).toHaveCount(0);
});

// Why (A3-003, #1081): Show ignored was a link with role=checkbox: a screen reader announced a checkbox that Space
// could not toggle (on a link Space only scrolls). It is a real checkbox: it has the checkbox role, Space toggles it
// and the list follows at once, and the URL carries what it sends.
test("Discovered: Show ignored is a checkbox, Space toggles it and the list follows", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/devices/new");
  const disc = box(page, "discovered");
  const showIgn = () => disc.getByRole("checkbox", { name: /Show ignored/ });
  await expect(showIgn()).not.toBeChecked();
  await expect(disc.locator("a[role=checkbox]")).toHaveCount(0);
  await showIgn().focus();
  await page.keyboard.press("Space");
  await expect(page).toHaveURL(/ignored=on/);
  await expect(showIgn()).toBeChecked();
  await showIgn().focus();
  await page.keyboard.press("Space");
  await expect(page).not.toHaveURL(/ignored=/);
  await expect(showIgn()).not.toBeChecked();
});

// Why: the filter must work without JavaScript too: the form has an Apply button for that, so a person whose
// script is blocked ticks the box, presses Apply and sees the ignored devices (and Ignore and Show again, plain
// form posts, work as well).
test("Discovered: without JavaScript, ticking Show ignored and pressing Apply shows the ignored devices", async ({ browser }) => {
  const ctx = await browser.newContext({ javaScriptEnabled: false, extraHTTPHeaders: adam, viewport: { width: 1280, height: 900 } });
  try {
    const page = await ctx.newPage();
    await page.goto(app.url + "/devices/new");
    const disc = box(page, "discovered");
    const row = (name: string) => disc.getByRole("row").filter({ hasText: name });
    await row("Shed light").getByRole("button", { name: "Ignore", exact: true }).click();
    await expect(row("Shed light")).toHaveCount(0);
    await disc.getByRole("checkbox", { name: /Show ignored/ }).check();
    await disc.getByRole("button", { name: "Apply" }).click();
    await expect(page).toHaveURL(/ignored=on/);
    await row("Shed light").getByRole("button", { name: "Show again" }).click();
    await expect(row("Shed light")).toHaveCount(1);
  } finally {
    await ctx.close();
  }
});

// Why (#713): a device that is on the broker but not offered as new has to be findable. The lookup of the
// Discovered box takes an IP address, a MAC or a topic and says where the device is; for something TasmoFleet
// has never seen it says so, and it asks nothing of devices (the page is a plain GET).
test("Discovered: the lookup says where a device is and what it has not seen", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/devices/new");
  const disc = box(page, "discovered");
  await expect(disc.locator("#why")).toHaveCount(0);
  await disc.getByLabel("Find a device by address").fill("127.0.0.21");
  await disc.getByRole("button", { name: "Look up" }).click();
  await expect(page).toHaveURL(/why=127\.0\.0\.21/);
  await expect(disc.locator("#why")).toContainText("Shed light");
  await expect(disc.locator("#why")).toContainText("is listed in the table");
  await disc.getByLabel("Find a device by address").fill("127.0.0.99");
  await disc.getByRole("button", { name: "Look up" }).click();
  await expect(disc.locator("#why")).toContainText("has not seen this on MQTT");
  await expectNoOverflow(page, "Add devices with a lookup");
});

// Why (#714, A3-005): Ignore is offered on an address change and on a conflict, so Show again must be too. Show ignored
// counts them and lists them as rows of the one table (never a section of their own) with Show again as the only action.
test("Discovered: an ignored address change and conflict are counted, listed and shown again", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
  const macs = ["aa:bb:cc:00:03:01", "aa:bb:cc:00:03:02"];
  try {
    db.exec("PRAGMA busy_timeout=5000");
    const now = Date.now();
    db.prepare(
      `INSERT INTO devices (id, name, address, port, mac, created_at, updated_at) VALUES (9714, 'Garage plug', '127.0.0.31', 80, ?, ?, ?)`,
    ).run(macs[0], now, now);
    for (const [mac, kind, ip] of [[macs[0], "address-change", "127.0.0.32"], [macs[1], "conflict", "127.0.0.33"]]) {
      db.prepare(
        `INSERT INTO discovery_proposals (mac, kind, via, name, address, port, existing_id, dismissed, first_seen, seen_at)
         VALUES (?, ?, 'mqtt', 'Claimed', ?, 80, 9714, 0, ?, ?)`,
      ).run(mac, kind, ip, now - 3600_000, now - 30_000);
    }
  } finally {
    db.close();
  }
  try {
    const disc = box(page, "discovered");
    const kindRow = (badge: string) => disc.locator(".add-disc-table").getByRole("row").filter({ hasText: badge });
    await page.goto(app.url + "/devices/new");
    await expect(disc.locator("section.add-moves[aria-label=\"Address changed\"]")).toHaveCount(0);
    await kindRow("New address").getByRole("button", { name: "Ignore" }).click();
    await kindRow("Conflict").getByRole("button", { name: "Ignore" }).click();
    await expect(kindRow("New address")).toHaveCount(0);
    await expect(kindRow("Conflict")).toHaveCount(0);
    await expect(disc.getByRole("checkbox", { name: "Show ignored (2)" })).toBeVisible();
    await disc.getByRole("checkbox", { name: /Show ignored/ }).click();
    for (const badge of ["New address", "Conflict"]) {
      await expect(kindRow(badge), `${badge} lists the ignored row`).toContainText("Ignored");
      await expect(kindRow(badge).getByRole("button", { name: "Show again" })).toHaveCount(1);
      await expect(kindRow(badge).getByRole("button", { name: /Ignore|Apply/ })).toHaveCount(0);
      await expect(kindRow(badge).getByRole("checkbox"), "no selection box on a known device's row").toHaveCount(0);
    }
    await kindRow("New address").getByRole("button", { name: "Show again" }).click();
    await expect(page.getByText("Shown again.", { exact: true })).toBeVisible();
    await expect(disc.getByRole("checkbox", { name: /Show ignored/ }), "Show again keeps the switch on").toBeChecked();
    await expect(kindRow("New address").getByRole("button", { name: "Apply" })).toBeVisible();
    await expect(kindRow("New address").getByRole("button", { name: "Ignore" })).toBeVisible();
    await kindRow("Conflict").getByRole("button", { name: "Show again" }).click();
    await expect(kindRow("Conflict").getByRole("button", { name: "Ignore" })).toBeVisible();
    await expect(disc.getByRole("checkbox", { name: "Show ignored (0)" })).toBeVisible();
    await expectNoOverflow(page, "Add devices with restored moves");
  } finally {
    const db2 = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
    try {
      db2.exec("PRAGMA busy_timeout=5000");
      db2.prepare("DELETE FROM discovery_proposals WHERE mac IN (?, ?)").run(macs[0], macs[1]);
      db2.prepare("DELETE FROM devices WHERE id = 9714").run();
    } finally {
      db2.close();
    }
  }
});

// Why (#905, A3-001): the IP column was a fixed 110 px, so a 12-character address was cut and a 15-character one lost 30 px,
// shown whole only in a tooltip. The column takes the width of its longest address (at least 140 px) and the name
// column takes the slack and wraps, so neither a long address nor a long name is cut with an ellipsis.
test("Discovered: a 15-character address and a long name are shown whole", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  const { DatabaseSync } = await import("node:sqlite");
  const file = join(app.dataDir, "tasmofleet.db");
  const mac = "aa:bb:cc:00:05:01";
  const withDb = (fn: (db: import("node:sqlite").DatabaseSync) => void) => {
    const db = new DatabaseSync(file);
    try {
      db.exec("PRAGMA busy_timeout=5000");
      fn(db);
    } finally {
      db.close();
    }
  };
  const name = "Workshop ceiling spotlight controller south wing with the extension cable to the shed";
  const oldHost = "kitchen-strip.home.internal.lan";
  const moveMac = "aa:bb:cc:00:05:02";
  withDb((db) => {
    const now = Date.now();
    db.prepare(
      `INSERT INTO discovery_proposals (mac, kind, via, reason, name, address, port, topic, full_topic, existing_id, dismissed, first_seen, seen_at, hostname)
       VALUES (?, 'new', 'mqtt', '', ?, '127.100.100.100', 80, 'long_topic_name', '', 0, 0, ?, ?, '')`,
    ).run(mac, name, now - 3600_000, now - 5_000);
    // a known device stored under a long hostname that announces a new address: its "was <old address>" line
    // is a second line of the IP cell and must not widen the column (the frame would scroll, the sticky action
    // column would cover Source and Last seen: the #905 symptom through a secondary line)
    db.prepare(`INSERT INTO devices (id, name, address, port, mac, created_at, updated_at) VALUES (9716, 'Kitchen strip', ?, 80, ?, ?, ?)`).run(
      oldHost, moveMac, now, now,
    );
    db.prepare(
      `INSERT INTO discovery_proposals (mac, kind, via, name, address, port, existing_id, dismissed, first_seen, seen_at)
       VALUES (?, 'address-change', 'mqtt', 'Kitchen strip', '127.100.100.101', 80, 9716, 0, ?, ?)`,
    ).run(moveMac, now - 3600_000, now - 5_000);
  });
  try {
    await page.goto(app.url + "/devices/new");
    const frame = box(page, "discovered").locator(".add-disc-table");
    const sideways = await frame.evaluate((f) => f.scrollWidth - f.clientWidth);
    expect(sideways, "the table does not scroll sideways at 1280 px with a 30-character old hostname").toBeLessThanOrEqual(0);
    await expect(box(page, "discovered").getByRole("row").filter({ hasText: "was " + oldHost })).toHaveCount(1);
    const row = box(page, "discovered").getByRole("row").filter({ hasText: "127.100.100.100" });
    await expect(row).toHaveCount(1);
    const cut = await row.evaluate((tr) =>
      [...tr.querySelectorAll<HTMLElement>("td")]
        .filter((c) => c.scrollWidth > c.clientWidth + 1 || [...c.querySelectorAll<HTMLElement>("*")].some((x) => x.scrollWidth > x.clientWidth + 1 && getComputedStyle(x).overflowX !== "visible"))
        .map((c) => `${(c.textContent ?? "").trim().slice(0, 20)}: ${c.scrollWidth} in ${c.clientWidth}`),
    );
    expect(cut, "no cell of the row is cut off").toEqual([]);
    await expect(row.locator("td.mono")).toHaveText("127.100.100.100");
    await expect(row.locator("td.name")).toContainText(name);
    await expect(row.locator("td.name")).toContainText("long_topic_name");
    const ip = await row.locator("td.mono").evaluate((c) => c.clientWidth);
    expect(ip, "the address column is at least 140 px").toBeGreaterThanOrEqual(139);
    await expectNoOverflow(page, "Add devices with a long name");
  } finally {
    withDb((db) => {
      db.prepare("DELETE FROM discovery_proposals WHERE mac IN (?, ?)").run(mac, moveMac);
      db.prepare("DELETE FROM devices WHERE id = 9716").run();
    });
  }
});

// Why (#745): a row's number is reused after the row is deleted, so Ignore on a page loaded earlier acted on a
// device the person never saw. The button carries the version it was shown; the other device stays listed.
test("Discovered: Ignore on a stale page is refused and does not hide the device that took the number", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  const { DatabaseSync } = await import("node:sqlite");
  const file = join(app.dataDir, "tasmofleet.db");
  const macs = ["aa:bb:cc:00:04:01", "aa:bb:cc:00:04:02"];
  const withDb = (fn: (db: import("node:sqlite").DatabaseSync) => void) => {
    const db = new DatabaseSync(file);
    try {
      db.exec("PRAGMA busy_timeout=5000");
      fn(db);
    } finally {
      db.close();
    }
  };
  const insert = (mac: string, name: string, ip: string) =>
    withDb((db) => {
      const now = Date.now();
      db.prepare(
        `INSERT INTO discovery_proposals (mac, kind, via, reason, name, address, port, topic, full_topic, existing_id, dismissed, first_seen, seen_at, hostname)
         VALUES (?, 'new', 'mqtt', '', ?, ?, 80, '', '', 0, 0, ?, ?, '')`,
      ).run(mac, name, ip, now - 3600_000, now - 5_000);
    });
  try {
    insert(macs[0], "Stale lamp", "127.0.0.41");
    await page.goto(app.url + "/devices/new");
    const row = (name: string) => box(page, "discovered").getByRole("row").filter({ hasText: name });
    await expect(row("Stale lamp")).toHaveCount(1);
    // the row behind the button is deleted (Add, the overtaken cleanup, the 30-day prune) and another device takes its number
    withDb((db) => db.prepare("DELETE FROM discovery_proposals WHERE mac = ?").run(macs[0]));
    insert(macs[1], "Fresh lamp", "127.0.0.42");
    await row("Stale lamp").getByRole("button", { name: "Ignore", exact: true }).click();
    await expect(page.getByText("The form is out of date. Reload the page and try again.")).toBeVisible();
    await expect(row("Fresh lamp"), "the device that took the number is still listed").toHaveCount(1);
    await expect(box(page, "discovered").getByRole("checkbox", { name: "Show ignored (0)" })).toBeVisible();
  } finally {
    withDb((db) => db.prepare("DELETE FROM discovery_proposals WHERE mac IN (?, ?)").run(macs[0], macs[1]));
  }
});

// Why: Scan now stays one click, and the scan runs live where its results are: the results page. The box
// then says in one line what the last scan found, WHEN (a date stamp, not a bare clock time) and links all results.
test("Network scan: Scan now opens the live results, the box keeps a one-line summary with All results", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/devices/new");
  await box(page, "scan").getByLabel("Subnet").fill("127.0.0.0/30");
  await box(page, "scan").getByRole("button", { name: "Scan now" }).click();
  await expect(page).toHaveURL(/\/discovery\/scan\/[0-9a-f]+$/);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Network scan");
  await expect(page.getByText("Scan finished.")).toBeVisible({ timeout: 30_000 });
  await page.getByRole("link", { name: "Back to Add devices" }).click();
  await expect(page).toHaveURL(/\/devices\/new$/);
  const scan = box(page, "scan");
  await expect(scan).toContainText("Scan finished.");
  await expect(scan.locator("time[data-ts=relative]")).toBeVisible();
  await expect(scan).toContainText("(in Discovered)");
  await expect(scan.locator("table")).toHaveCount(0);
  await scan.getByRole("link", { name: "All results" }).click();
  await expect(page).toHaveURL(/\/discovery\/scan\/[0-9a-f]+$/);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Network scan");
});

// Why: the manual box tests a connection without storing anything and adds the device when asked.
test("Add by address: Test connection stores nothing, Add device does", async ({ page }) => {
  const [name, d] = Object.entries(env.devices)[0];
  const [host, port] = d.addr.split(":");
  await page.goto(app.url + "/devices/new");
  const manual = box(page, "manual");
  await manual.locator("input[name=address]").fill(host);
  await manual.getByText("More options").click();
  await manual.locator("input[name=port]").fill(port);
  await manual.locator("input[name=name]").fill(name);
  await manual.getByRole("button", { name: "Test connection" }).click();
  await expect(page.getByText("Nothing was added yet.")).toBeVisible();
  await manual.getByRole("button", { name: "Add device" }).click();
  await expect(page).toHaveURL(/\/devices\/\d+/); // the add went through before the list is opened
  await page.goto(app.url + "/devices");
  await expect(page.getByRole("link", { name, exact: true })).toBeVisible();
});
