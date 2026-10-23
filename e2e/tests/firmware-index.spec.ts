import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv } from "../support/stack";

// H-2: the firmware index is refreshed by the app itself right after start,
// nobody clicks "Check for new releases now". The upstream is the devenv fake.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_URL,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
    },
  });
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

test("releases are listed right after start without clicking refresh", async ({ page }) => {
  await page.setExtraHTTPHeaders({ "X-authentik-username": "olivia", "X-authentik-groups": "ops" });
  await page.goto(app.url + "/firmware");
  // The first refresh runs asynchronously; the page may briefly say "Checking".
  await expect(async () => {
    await page.reload();
    await expect(page.locator("#release-16\\.1\\.0")).toBeVisible();
  }).toPass({ timeout: 30_000 });
  await expect(page.locator("[data-fw-check-state]")).toContainText("Last checked");
});
