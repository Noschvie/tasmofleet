import { mkdirSync } from "node:fs";
import { test, expect } from "../support/fixtures";
import { expectNoOverflow } from "../support/layout";
import {
  App,
  Devenv,
  startApp,
  startDevenv,
  writeTasmoAdminDir,
} from "../support/stack";

// #257: the device edit form stores the device's MQTT password (needed for backup over MQTT). It is
// write-only: never in the page, an empty field keeps the stored one. SHOT_DIR keeps screenshots.
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
const secret = "e2e-secret-mqtt-pw-31337";
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
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

test("the MQTT password is stored write-only and a blank field keeps it", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices/1/edit");
  const mode = page.getByLabel("MQTT password source");
  await expect(mode).toHaveValue("");
  await expect(mode.locator("option")).toHaveText([
    "Not set",
    "Same as TasmoFleet's broker password",
    "Own password",
  ]);

  await mode.selectOption("own");
  await page.getByLabel("Own MQTT password").fill(secret);
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(page).toHaveURL(/ok=saved/);

  await page.goto(app.url + "/devices/1/edit");
  await expect(mode).toHaveValue("own");
  const pw = page.getByLabel("Own MQTT password");
  await expect(pw).toHaveValue("");
  await expect(pw).toHaveAttribute("placeholder", "Unchanged");
  expect(await page.content()).not.toContain(secret);

  // an unrelated edit with the field blank keeps mode and value
  await page.locator("textarea[name=notes]").fill("touched");
  await page.getByRole("button", { name: "Save changes" }).click();
  await page.goto(app.url + "/devices/1/edit");
  await expect(mode).toHaveValue("own");
  await expect(pw).toHaveAttribute("placeholder", "Unchanged");

  // Own without any password is refused, naming the field
  await mode.selectOption("");
  await page.getByRole("button", { name: "Save changes" }).click();
  await page.goto(app.url + "/devices/1/edit");
  await mode.selectOption("own");
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(page.locator("#err-mqtt_password")).toContainText(
    "Enter the device's MQTT password.",
  );

  // the broker option stores no value of its own
  await mode.selectOption("broker");
  await page.getByRole("button", { name: "Save changes" }).click();
  await page.goto(app.url + "/devices/1/edit");
  await expect(mode).toHaveValue("broker");
  await expect(pw).not.toHaveAttribute("placeholder", "Unchanged");
});

for (const width of [1280, 375]) {
  for (const scheme of ["light", "dark"] as const) {
    test(`edit form at ${width}px ${scheme}: no overflow`, async ({ page }) => {
      await page.setExtraHTTPHeaders(adam);
      await page.setViewportSize({ width, height: 1500 });
      await page.emulateMedia({ colorScheme: scheme });
      await page.goto(app.url + "/devices/1/edit");
      await expect(page.getByLabel("MQTT password source")).toBeVisible();
      if (process.env.SHOT_DIR) {
        mkdirSync(process.env.SHOT_DIR, { recursive: true });
        await page.screenshot({
          path: `${process.env.SHOT_DIR}/edit-${width}-${scheme}.png`,
          fullPage: true,
        });
      }
      await expectNoOverflow(page, "device edit");
    });
  }
}
