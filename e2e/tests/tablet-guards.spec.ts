import { test, expect } from "../support/fixtures";
import { cardStackGaps, expectLayout } from "../support/layout";
import { pageList, settled } from "../support/page-list";
import { realisticDevenvArgs, realisticPolicyRules, seedRealisticData } from "../support/seed";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// #592: three #419 conversions (#588, #591) broke between 720 and 1199 px, where the card stacks
// become two half-width columns, and no test saw it: the card-width guard measures 1280/1920 and
// card-stacks covers only some pages. This runs the layout guards (overlap, a word broken across
// lines, cut-off cells, text over text) and the card-stack gap check on every page the card-width
// guard lists, at the two tablet widths, light theme (FULL tier).
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
let app: App;
let env: Devenv;
let lamp = "";
let txn = "";

test.beforeAll(async ({ browser }) => {
  test.setTimeout(300_000);
  env = await startDevenv(realisticDevenvArgs);
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_GITHUB_URL,
      TASMOFLEET_POLICY_RULES: realisticPolicyRules,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
  const p = await browser.newPage({ extraHTTPHeaders: adam });
  try {
    await seedRealisticData(p, app, env);
    await p.goto(app.url + "/devices");
    lamp = (await p.getByRole("link", { name: "lamp-1m", exact: true }).first().getAttribute("href"))!.replace(app.url, "");
    await p.goto(app.url + lamp);
    await p.getByRole("button", { name: "Back up", exact: true }).click();
    await expect(p.getByText(/backed up|Backup (taken|saved)/i).first()).toBeVisible({ timeout: 30_000 });
    // the policy judges devices from live reads. Since reads go over MQTT first (#456), none had arrived
    // when the deviations page was checked (empty state), and the background reads are spread over ten
    // minutes: run the live check once, as a person would, and check the page with its verdict
    await p.goto(app.url + "/policy/deviations");
    await p.getByRole("button", { name: "Run now" }).click();
    await expect(p.locator("#deviations [data-dev-group]").first()).toBeVisible({ timeout: 60_000 });
    await p.goto(app.url + "/transactions");
    txn = (await p.locator("a[href*='/transactions/']").first().getAttribute("href"))!.replace(app.url, "");
  } finally {
    await p.close();
  }
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const pages = pageList({ lamp: () => lamp, txn: () => txn, fwVersion: () => env.vars.DEVENV_FW_VERSION });

for (const width of [768, 1024]) {
  for (const [name, path] of pages) {
    test(`${name}: layout guards and card stacks at ${width}px`, async ({ page }) => {
      await page.setExtraHTTPHeaders(adam);
      await page.setViewportSize({ width, height: 900 });
      await page.emulateMedia({ colorScheme: "light" });
      const resp = await page.goto(app.url + path());
      expect(resp?.status(), `GET ${path()}`).toBeLessThan(400);
      await settled(page);
      await page.waitForTimeout(800);
      await expectLayout(page, `${name} ${width}px`);
      // the policy pages keep their own card gap (--card-gap, 20 to 24 px), see card-stacks.spec.ts
      expect(await cardStackGaps(page, name.startsWith("policy") ? 25 : 17), `${name}: card stacks`).toEqual([]);
    });
  }
}
