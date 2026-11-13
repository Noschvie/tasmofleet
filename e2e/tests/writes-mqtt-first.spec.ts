import { test, expect } from "../support/fixtures";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Changes over MQTT first (#605): a device on TasmoFleet's broker with its own topic takes console changes and
// switches over MQTT, also while HTTP reaches it, and the audit names the transport. Only lamp-1m is imported: a
// change over MQTT needs every device's group topic read, and the base fleet has devices whose group topic cannot
// be read (minimal firmware), which keep every change on HTTP (the strict rule, spec 5.x).
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
  expect(app.cli(["import", "tasmoadmin", writeTasmoAdminDir({ "lamp-1m": env.devices["lamp-1m"] })])).toContain("created: lamp-1m");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

test("a change and a switch of a device on TasmoFleet's broker go over MQTT first", { tag: "@core" }, async ({ page }) => {
  test.setTimeout(180_000);
  await page.setExtraHTTPHeaders(admin);
  // The group topic is read in the background shortly after the import; until then a change goes over HTTP.
  await expect(async () => {
    await page.goto(app.url + "/devices/1");
    await expect(page.locator("#devpaths-actions")).toContainText(/changing\s*MQTT/, { timeout: 2_000 });
  }).toPass({ timeout: 90_000, intervals: [3_000] });
  await page.goto(app.url + "/devices/1/console");
  await expect(page.getByText(/Console · MQTT \+ HTTP/)).toBeVisible();
  await page.getByRole("textbox", { name: "Command" }).fill("TelePeriod 130");
  await page.getByRole("button", { name: "Send" }).click();
  await expect(page.getByRole("log", { name: "Console output" }).getByText('"TelePeriod":130').first()).toBeVisible();

  await page.goto(app.url + "/devices");
  const row = page.getByRole("row").filter({ hasText: "lamp-1m" });
  const toggle = () => row.getByRole("button", { name: /Toggle relay 1/ });
  await expect(toggle()).toBeVisible({ timeout: 30_000 });
  const before = (await toggle().getAttribute("aria-pressed"))!;
  await toggle().click();
  await expect(toggle()).not.toHaveAttribute("aria-pressed", before);

  await page.goto(app.url + "/audit");
  const detail = (text: string) => page.locator("table.audittable tr.detail-row", { hasText: text });
  await expect(detail("TelePeriod 130").filter({ hasText: /via\W+mqtt/ })).toHaveCount(1);
  await expect(detail("TelePeriod 130").filter({ hasText: /via\W+http/ })).toHaveCount(0);
  await expect(detail('"relay"').filter({ hasText: /via\W+mqtt/ }).first()).toBeAttached();
});
