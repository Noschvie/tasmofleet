import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { BrowserContext, Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";
import { scheduleSnapshotSoon } from "../support/snapshot-schedule";

// Settings, Notifications, Browser card (issue #540): desktop notifications while a TasmoFleet tab
// is open. The event is a real one: a scheduled database snapshot that cannot be written (a file
// sits where the snapshot folder should be), so it goes the whole way: settings, dispatcher, hub,
// event stream, notify.js. The Notification constructor is wrapped to count what a browser would show.
// A snapshot fails once per schedule, so a test that needs its own event starts its own app.
async function newApp(): Promise<App> {
  const a = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
    },
  });
  a.cli(
    ["user", "passwd", "--create", "--role", "admin", "noticer"],
    "e2e-fake-password-1\n",
  );
  return a;
}
let app: App;
test.beforeAll(async () => {
  app = await newApp();
});
test.afterAll(async () => {
  await app?.stop();
});

type Shown = { title: string; body: string; tag: string };

/**
 * record counts every Notification the pages of the context construct. Headless Chromium reports
 * Notification.permission "denied" even after the permission was granted (requestPermission
 * answers "granted"; a real browser then reports it too), so the permission is kept here, in
 * localStorage like the browser keeps it per site, and answers as a real browser would.
 */
async function record(context: BrowserContext): Promise<Shown[]> {
  const shown: Shown[] = [];
  await context.exposeBinding(
    "tfRecordNotification",
    (_src, s: Shown) => void shown.push(s),
  );
  await context.addInitScript(() => {
    const w = window as unknown as {
      tfRecordNotification: (s: unknown) => void;
    };
    const perm = () => localStorage.getItem("e2e-permission") ?? "default";
    const Orig = window.Notification;
    window.Notification = new Proxy(Orig, {
      construct(target, args: [string, NotificationOptions?]) {
        w.tfRecordNotification({
          title: args[0],
          body: args[1]?.body ?? "",
          tag: args[1]?.tag ?? "",
        });
        return Reflect.construct(target, args);
      },
      get(target, key) {
        if (key === "permission") return perm();
        if (key === "requestPermission")
          return async () => {
            const answer = await target.requestPermission();
            localStorage.setItem("e2e-permission", answer);
            return answer;
          };
        return Reflect.get(target, key);
      },
    });
  });
  return shown;
}

async function signIn(page: Page, a: App = app) {
  await page.goto(a.url + "/login");
  await page.getByLabel("Username").fill("noticer");
  await page.getByLabel("Password").fill("e2e-fake-password-1");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(
    page.getByRole("heading", { name: "Fleet overview" }),
  ).toBeVisible();
}

const card = (page: Page) => page.locator("#card-notify-browser");
const openCard = async (page: Page, a: App = app) => {
  await page.goto(a.url + "/settings/system?tab=notifications");
  await expect(card(page)).toBeVisible();
};

// failSnapshot makes the next scheduled snapshot fail (a file where the snapshot folder belongs). A
// snapshot never runs because the schedule was saved (#602), so the schedule is saved with a time 20 to
// 80 seconds ahead; the snapshot fails then, from a new tab of the context on the Snapshots page, a page
// without live data. Callers wait for the event up to eventWait.
const eventWait = 120_000;
async function failSnapshot(context: BrowserContext, a: App = app) {
  writeFileSync(join(a.dataDir, "snapshots"), "in the way");
  const settings = await context.newPage();
  await scheduleSnapshotSoon(settings, a.url);
}

// Why: one event must give one notification, however many tabs are open: the person would
// otherwise get the same message three times. The tabs share one browser, so one opt-in and one
// permission serve all of them, and only the permission click happens in a tab.
test(
  "one notification per event across several open tabs",
  { tag: "@core" },
  async ({ page, context }) => {
    test.setTimeout(240_000); // the snapshot is taken at its scheduled time, up to 80 s after the save (#602)
    await context.grantPermissions(["notifications"]);
    const shown = await record(context);
    await signIn(page);
    await openCard(page);
    await expect(card(page)).toContainText("Off in this browser");
    await card(page)
      .getByRole("button", { name: "Enable browser notifications" })
      .click();
    await expect(card(page)).toContainText("On in this browser");

    const second = await context.newPage();
    await second.goto(app.url + "/devices");
    const third = await context.newPage();
    await third.goto(app.url + "/devices");
    await expect(second.locator("html")).toHaveAttribute("data-live", "live");
    await expect(third.locator("html")).toHaveAttribute("data-live", "live");

    await failSnapshot(context);

    await expect
      .poll(() => shown.length, { timeout: eventWait })
      .toBeGreaterThan(0);
    await page.waitForTimeout(1500); // a second copy would arrive within this
    expect(shown).toHaveLength(1);
    expect(shown[0].title).toBe("Database snapshot failed");
    expect(shown[0].body).toContain("could not be written");
    expect(shown[0].tag).toMatch(/^tasmofleet-/);
  },
);

// Why: "while a TasmoFleet tab is open" means any tab. Settings, Snapshots, Firmware or Backups
// have no live data and so no event stream of their own; with only such tabs open the event must
// still show, once. Here the leader sits on Settings and the event is set off on Snapshots.
test(
  "with only pages without live data open, the event still shows once",
  { tag: "@core" },
  async ({ page, context }) => {
    test.setTimeout(240_000); // the snapshot is taken at its scheduled time, up to 80 s after the save (#602)
    const a = await newApp();
    try {
      await context.grantPermissions(["notifications"]);
      const shown = await record(context);
      await signIn(page, a);
      await openCard(page, a);
      // The stream opens on the click, and again on a page that loads with notifications on.
      let stream = page.waitForRequest(/\/events\?only=notice$/);
      await card(page)
        .getByRole("button", { name: "Enable browser notifications" })
        .click();
      await expect(card(page)).toContainText("On in this browser");
      await stream;
      stream = page.waitForRequest(/\/events\?only=notice$/);
      await page.reload();
      await stream;

      await failSnapshot(context, a);

      await expect
        .poll(() => shown.length, { timeout: eventWait })
        .toBeGreaterThan(0);
      await page.waitForTimeout(1500); // a second copy would arrive within this
      expect(shown).toHaveLength(1);
      expect(shown[0].title).toBe("Database snapshot failed");
    } finally {
      await a.stop();
    }
  },
);

// Why: permission alone must not start notifications: the person opts in per browser, and
// turning it off must stop them without touching the browser's permission. A real event follows
// the Turn off; a second browser that stays on proves it came.
test("nothing is shown before the opt-in, and Turn off stops it again", async ({
  page,
  context,
  browser,
}) => {
    test.setTimeout(240_000); // the snapshot is taken at its scheduled time, up to 80 s after the save (#602)
  const a = await newApp();
  try {
    await context.grantPermissions(["notifications"]); // granted for the site, but the person has not opted in here
    const shown = await record(context);
    await signIn(page, a);
    await openCard(page, a);
    await expect(card(page)).toContainText("Off in this browser");
    await expect(
      card(page).getByRole("button", { name: "Send test" }),
    ).toBeHidden();
    await card(page)
      .getByRole("button", { name: "Enable browser notifications" })
      .click();
    await card(page).getByRole("button", { name: "Send test" }).click();
    await expect.poll(() => shown.length).toBe(1);
    expect(shown[0].title).toBe("TasmoFleet test notification");
    await card(page).getByRole("button", { name: "Turn off" }).click();
    await expect(card(page)).toContainText("Off in this browser");
    await page.reload();
    await expect(card(page)).toContainText("Off in this browser"); // kept in this browser's storage, off
    await expect(
      card(page).getByRole("button", { name: "Enable browser notifications" }),
    ).toBeVisible();
    await page.goto(a.url + "/devices"); // a live page: the event reaches this browser
    await expect(page.locator("html")).toHaveAttribute("data-live", "live");

    const other = await browser.newContext();
    try {
      await other.grantPermissions(["notifications"]);
      const control = await record(other);
      const on = await other.newPage();
      await signIn(on, a);
      await openCard(on, a);
      await card(on)
        .getByRole("button", { name: "Enable browser notifications" })
        .click();
      await expect(card(on)).toContainText("On in this browser");
      await on.goto(a.url + "/devices");
      await expect(on.locator("html")).toHaveAttribute("data-live", "live");

      await failSnapshot(context, a);
      await expect.poll(() => control.length, { timeout: eventWait }).toBe(1);
      await page.waitForTimeout(1500); // the turned-off browser had the same time to show it
      expect(shown).toHaveLength(1);
    } finally {
      await other.close();
    }
  } finally {
    await a.stop();
  }
});

// Why: when the browser says no, a button that does nothing is the worst answer. The card names
// the cause and the way out, and offers no button.
test("denied: the card says the browser blocked it", async ({
  page,
  context,
}) => {
  await context.addInitScript(() =>
    Object.defineProperty(window.Notification, "permission", {
      get: () => "denied",
    }),
  );
  await signIn(page);
  await openCard(page);
  await expect(card(page)).toContainText("Blocked by this browser");
  await expect(card(page)).toContainText("site settings");
  await expect(card(page).getByRole("button")).toHaveCount(0);
});

// Why: the Notification API exists only on HTTPS or localhost. Someone who opens TasmoFleet by
// its LAN address over plain HTTP must be told that, not that the browser refused.
test("insecure page: the card says notifications need HTTPS or localhost", async ({
  page,
  context,
}) => {
  await context.addInitScript(() =>
    Object.defineProperty(window, "isSecureContext", { get: () => false }),
  );
  await signIn(page);
  await openCard(page);
  await expect(card(page)).toContainText("Not available on this connection");
  await expect(card(page)).toContainText("HTTPS");
  await expect(card(page).getByRole("button")).toHaveCount(0);
});
