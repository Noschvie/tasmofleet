import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { expectNoOverflow } from "../support/layout";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// A rule whose value is a long URL (OtaUrl rules, since the policy knows that setting) put the
// device name of the live preview and of the "Make compliant" plan on one letter per line and
// pushed the URL out of the card (horizontal scrollbar). Short values (0, 1, 99) never showed it.
// The generic overflow guards miss it: the page itself is not wider, the list scrolls inside itself.
// 64 characters: the longest "want" a rule accepts. With the device's own OtaUrl as "have" the pair is wider than any card.
const longUrl = "http://mirror.home.internal:8080/firmwares/tasmota/tasmota.bin";
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_POLICY_RULES: JSON.stringify([{ command: "OtaUrl", want: longUrl, note: "e2e long value" }]),
    },
  });
  expect(app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)])).toContain("created: lamp-1m");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});
test.use({ extraHTTPHeaders: { "X-authentik-username": "adam", "X-authentik-groups": "adm" } });

async function openPolicy(page: Page): Promise<void> {
  await page.goto(app.url + "/policy");
  await page.getByRole("button", { name: "Run now" }).click(); // the values are read live
  await expect(page.locator(".banner", { hasText: "deviate from a required rule" })).toBeVisible({ timeout: 60_000 }); // the banner has Show, to the tab that offers Make compliant
}

for (const vp of [
  { name: "phone", width: 375, height: 800 },
  { name: "desktop", width: 1280, height: 900 },
]) {
  test(`policy preview and make-compliant plan keep a long value inside, ${vp.name}`, async ({ page }) => {
    await page.setViewportSize({ width: vp.width, height: vp.height });
    // the rule page (env mode: the rule has the synthesised id r1): the long value sits in the
    // mono line, the device rows stay readable and the page does not scroll sideways
    await page.goto(app.url + "/policy/rules/r1");
    await page.getByRole("button", { name: "Check now" }).click();
    await expect(page.getByText(/Live check (finished|rate-limited)/)).toBeVisible({ timeout: 60_000 });
    await expect(page.locator(".pghead-sub")).toContainText(longUrl);
    const devRows = page.locator("#rule-devices tbody tr");
    // the long Actual value wraps inside its cell (the row grows); the name keeps its width and lines
    for (let i = 0; i < (await devRows.count()); i++) {
      const cell = await devRows.nth(i).locator("td.p1").boundingBox();
      const link = await devRows.nth(i).locator("td.p1 a").boundingBox();
      expect(cell!.width, `rule page device ${i}: the name column is squeezed`).toBeGreaterThanOrEqual(80);
      expect(link!.height, `rule page device ${i}: the name runs over ${link!.height}px`).toBeLessThanOrEqual(60);
    }
    const over = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(over, "the rule page scrolls sideways").toBeLessThanOrEqual(0);

    await openPolicy(page);

    // Make compliant is on the Deviating devices tab (the overview's banner has the one action Show, #1091): its plan page
    await page.goto(app.url + "/policy/deviations");
    await page.getByRole("button", { name: /^Make compliant…/ }).click();
    await expect(page.locator(".pol-fix-card").first()).toBeVisible();
    // Why: a name that is squeezed to a sliver is unreadable, a command that pokes out of its card hides the value it
    // exists to show. The name stays on a few lines at most, and every command stays inside its card.
    for (const [i, card] of (await page.locator(".pol-fix-card").all()).entries()) {
      const box = (await card.boundingBox())!;
      const name = (await card.locator("h2 a").boundingBox())!;
      expect(name.height, `plan card ${i}: the name runs over ${name.height}px of height`).toBeLessThanOrEqual(60);
      for (const cmd of await card.locator(".pol-fix-cmd").all()) {
        const c = (await cmd.boundingBox())!;
        expect(c.x + c.width, `plan card ${i}: a command pokes out of its card`).toBeLessThanOrEqual(box.x + box.width + 1);
      }
    }
    await expectNoOverflow(page, "the make compliant plan");
  });
}
