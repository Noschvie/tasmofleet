import { test, expect } from "../support/fixtures";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Power toggle in the device list and the per-device console, against the
// simulator fleet. No firmware source is configured: OTA is not involved.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
    },
  });
  const out = app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
  expect(out).toContain("created: plug-4m-http");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const operator = { "X-authentik-username": "olivia", "X-authentik-groups": "ops" };
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

// Was quarantined (#206): a health poll that read the device before the toggle put the old state back.
test("switch a relay on and off from the device list, and follow outside changes live (HTTP-only device)", async ({ page, request }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/devices");
  const row = page.getByRole("row").filter({ hasText: "plug-4m-http" });
  await expect(row.getByText("Online", { exact: true })).toBeVisible({ timeout: 30_000 });
  const button = row.getByRole("switch", { name: /relay 1$/ });
  await expect(button).toBeVisible({ timeout: 30_000 });
  const before = (await button.getAttribute("aria-checked"))!; // the switch: "true" is on
  const flipped = before === "true" ? "false" : "true";
  await button.click();
  await expect(row.getByRole("switch", { name: /relay 1$/ })).toHaveAttribute("aria-checked", flipped);
  await row.getByRole("switch", { name: /relay 1$/ }).click();
  await expect(row.getByRole("switch", { name: /relay 1$/ })).toHaveAttribute("aria-checked", before);

  // A change made at the device itself shows up without reloading the page
  // (the HTTP poll feeds the same SSE stream).
  const addr = env.devices["plug-4m-http"].addr;
  const outside = before === "true" ? "false" : "true";
  await request.get(`http://${addr}/cm?cmnd=Power%20${outside === "true" ? "ON" : "OFF"}`);
  await expect(row.getByRole("switch", { name: /relay 1$/ })).toHaveAttribute("aria-checked", outside, { timeout: 30_000 });
});

// #463: the list reloads itself on device events (the first probes of a fresh import end there). A reload that the
// server rendered before a toggle finished carries the old relay state; applied after the toggle's answer it put
// that state back, and nothing corrected it before the next poll. The test holds such a reload (rendered now,
// delivered after the toggle) and wants the toggled state to survive it, and the reload to be asked for again. The
// reload that is asked for again is judged like any other: it is held too, toggled over and must not win either.
test("a list reload rendered before a toggle does not bring the old relay state back", async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/devices");
  const row = page.getByRole("row").filter({ hasText: "plug-4m-http" });
  const toggle = () => row.getByRole("switch", { name: /relay 1$/ });
  await expect(toggle()).toBeVisible({ timeout: 30_000 });
  const before = (await toggle().getAttribute("aria-checked"))!;
  const flipped = before === "true" ? "false" : "true";

  // Every table reload is rendered when it is asked for and delivered when its hold is released.
  const hold = () => {
    let release!: () => void;
    let rendered!: () => void;
    return {
      gate: new Promise<void>((ok) => (release = ok)),
      release: () => release(),
      renderedByServer: new Promise<void>((ok) => (rendered = ok)),
      rendered: () => rendered(),
    };
  };
  let current = hold();
  await page.route("**/devices/table*", async (route) => {
    const mine = current;
    const res = await route.fetch();
    mine.rendered();
    await mine.gate;
    await route.fulfill({ response: res });
  });
  // The page's own decision about a reload (htmx fires beforeSwap whether it swaps or not), and the next one that is applied.
  const nextDecision = () =>
    page.evaluate(
      () => new Promise<void>((ok) => document.body.addEventListener("htmx:beforeSwap", (e) => (e as CustomEvent).detail.target?.id === "devtable" && ok(), { once: false })),
    );
  const nextSettled = () =>
    page.evaluate(
      () => new Promise<void>((ok) => document.body.addEventListener("htmx:afterSettle", (e) => (e as CustomEvent).detail.target?.id === "devtable" && ok())),
    );

  // 1. A reload rendered with the old state, a toggle, then the reload arrives: dropped.
  const first = current;
  const decidedFirst = nextDecision();
  await page.evaluate(() => (window as any).htmx.trigger("#devtable", "sse:resync"));
  await first.renderedByServer;
  await toggle().click();
  await expect(toggle()).toHaveAttribute("aria-checked", flipped);

  // 2. The dropped reload is asked for again (rendered now, with the toggled state); a second toggle follows
  //    before it arrives, so this one is stale as well and must be dropped.
  current = hold();
  const second = current;
  first.release();
  await decidedFirst;
  await expect(toggle()).toHaveAttribute("aria-checked", flipped, { timeout: 1000 });
  await second.renderedByServer;
  const decidedSecond = nextDecision();
  await toggle().click();
  await expect(toggle()).toHaveAttribute("aria-checked", before);
  current = hold(); // the next one is not held for long: it is released at once below
  const third = current;
  second.release();
  await decidedSecond;
  await expect(toggle()).toHaveAttribute("aria-checked", before, { timeout: 1000 });

  // 3. Asked for again once more, rendered after both toggles: applied, and it agrees.
  const settled = nextSettled();
  third.release();
  await settled;
  await expect(toggle()).toHaveAttribute("aria-checked", before, { timeout: 1000 });
});

test("switch a relay of an MQTT device", async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/devices");
  const row = page.getByRole("row").filter({ hasText: "lamp-1m" });
  await expect(row.getByText("Online", { exact: true })).toBeVisible({ timeout: 30_000 });
  const button = row.getByRole("switch", { name: /relay 1$/ });
  await expect(button).toBeVisible({ timeout: 30_000 });
  const before = (await button.getAttribute("aria-checked"))!;
  await button.click();
  await expect(row.getByRole("switch", { name: /relay 1$/ })).not.toHaveAttribute("aria-checked", before);
});

test("console: operator reads Status 2, admin commands need the admin role", { tag: "@core" }, async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: "plug-4m-http", exact: true }).click();
  await page.getByRole("link", { name: "Console" }).click();
  await page.getByRole("textbox", { name: "Command" }).fill("Status 2");
  await page.getByRole("button", { name: "Send" }).click();
  await expect(page.getByText("StatusFWR")).toBeVisible();
  await expect(page.getByRole("log", { name: "Console output" }).getByText("Status 2")).toBeVisible();

  await page.getByRole("textbox", { name: "Command" }).fill("TelePeriod 120");
  const res = page.waitForResponse((r) => r.url().endsWith("/console") && r.request().method() === "POST");
  await page.getByRole("button", { name: "Send" }).click();
  expect((await res).status()).toBe(403);
  await expect(page.getByRole("alert").filter({ hasText: "needs the admin role" })).toBeVisible();
});

test("console: a routine admin command is sent at once; a destructive one names its consequence first", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: "plug-4m-http", exact: true }).click();
  await page.getByRole("link", { name: "Console" }).click();
  await page.getByRole("textbox", { name: "Command" }).fill("TelePeriod 120");
  await page.getByRole("button", { name: "Send" }).click();
  await expect(page.getByRole("heading", { name: "Confirm" })).toHaveCount(0); // no second screen for a routine setting
  await expect(page.getByRole("log", { name: "Console output" }).getByText("TelePeriod 120").first()).toBeVisible();

  await page.getByRole("textbox", { name: "Command" }).fill("Reset 1");
  await page.getByRole("button", { name: "Send" }).click();
  await expect(page.getByRole("heading", { name: "Confirm" })).toBeVisible();
  await expect(page.getByRole("status").filter({ hasText: "erases the device configuration" })).toBeVisible();
  await expect(page.locator("input[type=password]")).toHaveCount(0); // nothing to retype
  await page.getByRole("link", { name: "Cancel" }).click(); // the simulator device stays as it is
  await expect(page.getByRole("log", { name: "Console output" }).getByText("Reset")).toHaveCount(0);
});

// Why (#1074): the switch is named by its row (aria-labelledby), and the toast of a click that never reached the
// server must name the same thing, "<device> relay 1", not the bare "Toggle relay 1" it said before. The name is
// whole words: a device called "Attic, left" is not cut at its comma.
test("a switch that cannot be sent says which device and relay it is", async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/devices");
  const row = page.getByRole("row").filter({ hasText: "plug-4m-http" });
  const toggle = row.getByRole("switch", { name: /relay 1$/ });
  await expect(toggle).toBeVisible({ timeout: 30_000 });
  await page.route(/\/power\/\d+$/, (route) => route.abort("failed"));
  await toggle.click();
  const toast = page.locator("#toasts .toast.t-danger");
  await expect(toast).toContainText("plug-4m-http relay 1 was not sent: no connection");
  await expect(toggle).toBeEnabled();
  // renamed and clicked in one task, so a live reload of the table cannot put the name back in between
  await toggle.evaluate((b) => {
    document.getElementById(b.getAttribute("aria-labelledby")!.split(" ")[0])!.textContent = "Attic, left";
    b.click();
  });
  await expect(toast.filter({ hasText: "Attic, left relay 1 was not sent: no connection" })).toHaveCount(1);
  // a long device name is cut, the relay is kept: the toast still names the switch instead of "The request"
  await toggle.evaluate((b) => {
    document.getElementById(b.getAttribute("aria-labelledby")!.split(" ")[0])!.textContent =
      "workshop-ceiling-spotlight-controller-north";
    b.click();
  });
  await expect(toast.filter({ hasText: "workshop-ceiling-spotlight-cont… relay 1 was not sent: no connection" })).toHaveCount(1);
});

// Why: a person who toggles a relay must learn that the command never left the browser, also when a live health
// update redraws the switch's cell while the click is in flight. htmx reports the failed request on the element
// that sent it; that element is gone after the redraw, so without a listener on the request itself the switch
// silently stays as it was (the nightly failed on this race, #634/#1074). The route handler redraws the cell
// first, the way a live swap does, and aborts after it: no wait on live timing.
test("a switch redrawn by a live update while its click fails still says it was not sent", async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/devices");
  const row = page.getByRole("row").filter({ hasText: "plug-4m-http" });
  const toggle = row.getByRole("switch", { name: /relay 1$/ });
  await expect(toggle).toBeVisible({ timeout: 30_000 });
  await page.route(/\/power\/\d+$/, async (route) => {
    const gone = await toggle.evaluate((btn) => {
      const cell = btn.closest(".pcell");
      if (!cell) return false;
      // the redraw a live swap does: the cell's content is replaced, the old button leaves the page
      (window as any).htmx.swap(cell, cell.innerHTML, { swapStyle: "innerHTML" });
      return !btn.isConnected;
    });
    expect(gone).toBe(true);
    await route.abort("failed");
  });
  await toggle.click();
  const toast = page.locator("#toasts .toast.t-danger").filter({ hasText: "relay 1 was not sent: no connection" });
  await expect(toast).toHaveCount(1);
  await expect(toast).toContainText("plug-4m-http relay 1");
  await expect(row.getByRole("switch", { name: /relay 1$/ })).toBeEnabled();
});
