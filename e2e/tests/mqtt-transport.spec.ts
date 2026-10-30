import { test, expect } from "../support/fixtures";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Console over MQTT (0.20.0, #79, #256): mq-esp32 is a simulated
// device whose HTTP port refuses every connection but which is on the broker. TasmoFleet reaches it
// only through MQTT.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv(["-mqtt-transport"]);
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
  const only = { "mq-esp32": env.devices["mq-esp32"] };
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
  // Json is routine for an admin (no confirmation screen): the refusal is the device error text.
  await expect(page.getByRole("alert")).toBeVisible();
  await expect(page.getByText("StatusMEM")).toHaveCount(0); // what a Status 0 reply would have shown

  // Changing the topic of a device that only MQTT reaches can cut it off for good: it asks once.
  await box.fill("Topic somewhere-else");
  await page.getByRole("button", { name: "Send" }).click();
  await expect(page.getByText("reached over MQTT only")).toBeVisible();
});
