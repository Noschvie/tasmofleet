import { test, expect } from "../support/fixtures";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// #1047: the write gate's topic rule made visible. plug-4m's GroupTopic2 is set to lamp-1m's Topic through the
// console; once the reader has read plug-4m's group topics again, the Topic check card on System > Settings > MQTT
// lists the collision with both devices, and lamp-1m's Identity card says its MQTT writes are held back.
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
    },
  });
  const only = { "lamp-1m": env.devices["lamp-1m"], "plug-4m": env.devices["plug-4m"] };
  expect(app.cli(["import", "tasmoadmin", writeTasmoAdminDir(only)])).toContain("created: lamp-1m");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

test("a GroupTopic equal to another device's Topic shows on the Topic check card and the Identity card", async ({ page }) => {
  test.setTimeout(240_000);
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  const idOf = async (name: string) => (await page.getByRole("link", { name, exact: true }).first().getAttribute("href"))!.match(/devices\/(\d+)/)![1];
  const lamp = await idOf("lamp-1m");
  const plug = await idOf("plug-4m");
  const lampTopic = env.devices["lamp-1m"].topic;

  const status = page.locator("#topiccheck-status b");
  // Both devices' group topics are read by the background reader; until then the card says so, never "unique".
  await expect(async () => {
    await page.goto(app.url + "/settings/system?tab=mqtt");
    await expect(status).toHaveText("All device topics are unique", { timeout: 2_000 });
  }).toPass({ timeout: 90_000 });

  await page.goto(`${app.url}/devices/${plug}/console`);
  await page.getByRole("textbox", { name: "Command" }).fill(`GroupTopic2 ${lampTopic}`);
  await page.getByRole("button", { name: "Send" }).click();
  // the device's answer (all four group topics) lands in the console's log
  const confirm = page.getByRole("button", { name: "Confirm and send" });
  const sent = page.getByRole("log", { name: "Console output" }).getByText(`"GroupTopic2":"${lampTopic}"`);
  await expect(confirm.or(sent).first()).toBeVisible({ timeout: 30_000 });
  if (await confirm.isVisible()) await confirm.click();
  await expect(sent.first()).toBeVisible({ timeout: 30_000 });

  const card = page.locator("#card-topiccheck");
  await expect(async () => {
    await page.goto(app.url + "/settings/system?tab=mqtt");
    await expect(status).toHaveText("1 collision", { timeout: 2_000 });
  }).toPass({ timeout: 120_000 });
  const collisions = card.locator("#topiccheck-collisions");
  await expect(collisions.getByText(lampTopic, { exact: true }).first()).toBeVisible();
  await expect(collisions.getByRole("link", { name: "lamp-1m", exact: true })).toHaveAttribute("href", new RegExp(`/devices/${lamp}$`));
  await expect(collisions.getByRole("link", { name: "plug-4m", exact: true })).toHaveAttribute("href", new RegExp(`/devices/${plug}$`));
  await expect(collisions.getByText("one of GroupTopic2 to 4")).toBeVisible();

  await page.goto(`${app.url}/devices/${lamp}`);
  const hint = page.locator("#identity #topic-hint");
  await expect(hint).toContainText(`MQTT writes are held back: topic ${lampTopic} is one of GroupTopic2 to 4 of plug-4m.`);
  await expect(hint.getByRole("link", { name: "plug-4m" })).toHaveAttribute("href", new RegExp(`/devices/${plug}$`));
  await page.goto(`${app.url}/devices/${plug}`);
  await expect(page.locator("#identity")).toBeVisible();
  await expect(page.locator("#topic-hint")).toHaveCount(0); // plug-4m's own writes are not held back
});
