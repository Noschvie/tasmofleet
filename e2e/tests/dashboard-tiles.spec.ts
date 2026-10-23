import { test, expect } from "../support/fixtures";
import {
  App,
  Devenv,
  freePort,
  startApp,
  startDevenv,
  writeTasmoAdminDir,
} from "../support/stack";

// Every dashboard tile opens the device list narrowed to what it counts (?health=online,
// ?chip=down, ...). "All" must then be the way back to the whole fleet, with the filter gone from
// the URL too (v0.11.1 feedback 5: from the Online tile, All kept ?health=online and the list
// stayed narrowed, so a reload or a shared link showed a part of the fleet as "All").
let app: App;
let env: Devenv;
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE:
        env.vars.DEVENV_UPSTREAM_GITHUB_URL,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
      // a policy makes the "Policy deviations" tile (and the Not compliant chip) appear
      TASMOFLEET_POLICY_RULES: JSON.stringify([
        { command: "PowerOnState", want: "2", note: "e2e target" },
      ]),
    },
  });
  expect(
    app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]),
  ).toContain("created:");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const tiles = [
  "Online",
  "Offline",
  "Unknown",
  "Firmware behind",
  "Policy deviations",
];

for (const tile of tiles) {
  test(`from the ${tile} tile, All shows the whole fleet and drops the filter from the URL`, async ({
    page,
  }) => {
    const total = Object.keys(env.devices).length;
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.setExtraHTTPHeaders(adam);
    await page.goto(app.url + "/");
    await page.locator("a.stat-tile").filter({ hasText: tile }).click();
    // the tile's filter is in force: it is in the URL and "All" is not the selected chip
    await expect(page).toHaveURL(/[?&](health|chip)=/);
    const all = page.locator(".dv-chips a.dv-chip").filter({ hasText: /^All/ });
    await expect(all).not.toHaveAttribute("aria-current", "true");

    await all.click();
    await expect(page).not.toHaveURL(/[?&](health|chip)=/);
    await expect(all).toHaveAttribute("aria-current", "true");
    await expect(all.locator(".dv-n")).toHaveText(String(total));
    await expect(
      page.locator(
        "table[data-table=devices] tbody tr[data-id]",
      ),
    ).toHaveCount(total);
  });
}
