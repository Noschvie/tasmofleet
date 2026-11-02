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
  const button = row.getByRole("button", { name: /Toggle relay 1/ });
  await expect(button).toBeVisible({ timeout: 30_000 });
  const before = (await button.getAttribute("aria-pressed"))!; // the switch: "true" is on
  const flipped = before === "true" ? "false" : "true";
  await button.click();
  await expect(row.getByRole("button", { name: /Toggle relay 1/ })).toHaveAttribute("aria-pressed", flipped);
  await row.getByRole("button", { name: /Toggle relay 1/ }).click();
  await expect(row.getByRole("button", { name: /Toggle relay 1/ })).toHaveAttribute("aria-pressed", before);

  // A change made at the device itself shows up without reloading the page
  // (the HTTP poll feeds the same SSE stream).
  const addr = env.devices["plug-4m-http"].addr;
  const outside = before === "true" ? "false" : "true";
  await request.get(`http://${addr}/cm?cmnd=Power%20${outside === "true" ? "ON" : "OFF"}`);
  await expect(row.getByRole("button", { name: /Toggle relay 1/ })).toHaveAttribute("aria-pressed", outside, { timeout: 30_000 });
});

// #463: the list reloads itself on device events (the first probes of a fresh import end there). A reload that the
// server rendered before the toggle finished carries the old relay state; applied after the toggle's answer it put
// that state back, and nothing corrected it before the next poll. The test holds such a reload (rendered now,
// delivered after the toggle) and wants the toggled state to survive it, and the reload to be asked for again.
test("a list reload rendered before a toggle does not bring the old relay state back", async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/devices");
  const row = page.getByRole("row").filter({ hasText: "plug-4m-http" });
  const toggle = () => row.getByRole("button", { name: /Toggle relay 1/ });
  await expect(toggle()).toBeVisible({ timeout: 30_000 });
  const before = (await toggle().getAttribute("aria-pressed"))!;
  const flipped = before === "true" ? "false" : "true";

  let release!: () => void;
  const held = new Promise<void>((ok) => (release = ok));
  let rendered!: () => void;
  const renderedByServer = new Promise<void>((ok) => (rendered = ok));
  await page.route("**/devices/table*", async (route) => {
    const res = await route.fetch(); // rendered now, with the old state
    rendered();
    await held;
    await route.fulfill({ response: res });
  });
  // The page's own decision about the held reload (htmx fires beforeSwap whether it swaps or not), and the next reload that is applied.
  const decided = page.evaluate(
    () => new Promise<void>((ok) => document.body.addEventListener("htmx:beforeSwap", (e) => (e as CustomEvent).detail.target?.id === "devtable" && ok())),
  );
  await page.evaluate(() => (window as any).htmx.trigger("#devtable", "sse:resync"));
  await renderedByServer;
  await toggle().click();
  await expect(toggle()).toHaveAttribute("aria-pressed", flipped);

  const reloaded = page.evaluate(
    () => new Promise<void>((ok) => document.body.addEventListener("htmx:afterSettle", (e) => (e as CustomEvent).detail.target?.id === "devtable" && ok())),
  );
  release();
  await decided;
  await expect(toggle()).toHaveAttribute("aria-pressed", flipped, { timeout: 1000 });
  await reloaded; // the dropped reload was asked for again, rendered after the toggle
  await expect(toggle()).toHaveAttribute("aria-pressed", flipped, { timeout: 1000 });
});

test("switch a relay of an MQTT device", async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/devices");
  const row = page.getByRole("row").filter({ hasText: "lamp-1m" });
  await expect(row.getByText("Online", { exact: true })).toBeVisible({ timeout: 30_000 });
  const button = row.getByRole("button", { name: /Toggle relay 1/ });
  await expect(button).toBeVisible({ timeout: 30_000 });
  const before = (await button.getAttribute("aria-pressed"))!;
  await button.click();
  await expect(row.getByRole("button", { name: /Toggle relay 1/ })).not.toHaveAttribute("aria-pressed", before);
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
