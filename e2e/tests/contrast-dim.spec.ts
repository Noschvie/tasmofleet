import { join } from "node:path";
import type { Page } from "@playwright/test";
import { faded, lowContrast, textRuns } from "../support/contrast";
import { test, expect } from "../support/fixtures";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// #1071: a state that is drawn only by opacity on the element that holds the text (an offline device, a rule that
// is not judged or disabled, a setting that is switched off) drops that text below the AA contrast of 4.5:1 (3:1
// for large text) and says nothing in words. These specs measure every visible text node of the regions that used to
// be dimmed, with every ancestor's opacity multiplied in, at both widths in both themes. Text that belongs to a
// control that cannot act (disabled, aria-disabled, inert) is exempt: WCAG exempts inactive components.
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
let app: App;
let env: Devenv;

test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv(["-overview-shapes"]);
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_POLICY_RULES: JSON.stringify([
        // firmware that does not know PowerLock cannot be judged: "Not judged" rows
        { name: "Power lock", command: "PowerLock", want: "0" },
        { name: "Flash wear", command: "SaveData", want: "0", disabled: true },
      ]),
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
  try {
    db.exec("PRAGMA busy_timeout=5000");
    // the minimal build and the old build are the two devices the catalogue says do not have PowerLock
    db.prepare("UPDATE devices SET fw_version = ?, fw_variant = ? WHERE name = ?").run("13.4.0", "minimal", "lamp-minimal");
    db.prepare("UPDATE devices SET fw_version = ?, fw_variant = ? WHERE name = ?").run("13.4.0", "tasmota", "plug-4m-http");
  } finally {
    db.close();
  }
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

async function control(path: string) {
  const r = await fetch(env.vars.DEVENV_CONTROL_URL + path, { method: "POST" });
  expect(r.status, path).toBe(204);
}

async function setup(page: Page, width: number, scheme: "light" | "dark") {
  await page.emulateMedia({ colorScheme: scheme });
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width, height: 900 });
}

/** offline opens the overview of a device the simulator has switched off, and waits for the banner. */
async function openOffline(page: Page, name: string) {
  await page.goto(app.url + "/devices");
  await page.locator("table[data-table=devices] .pwr:not([disabled])").first().waitFor({ state: "attached", timeout: 30_000 });
  const href = (await page.getByRole("link", { name, exact: true }).first().getAttribute("href"))!;
  await page.goto(app.url + href);
  await expect(page.locator("#devlive .dev-relay, #devlive .dev-sensor").first()).toBeVisible({ timeout: 30_000 });
  if ((await page.locator("#devhd-off").count()) === 0) {
    await control(`/device/${name}/off`);
  }
  await expect(page.locator("#devhd-off")).toContainText("Offline since", { timeout: 60_000 });
}

async function expectReadable(page: Page, roots: string) {
  const runs = await textRuns(page, roots);
  expect(runs.length, `no text found in ${roots}`).toBeGreaterThan(0);
  const low = lowContrast(runs);
  expect(low.length ? [...low.slice(0, 8), `${low.length} of ${runs.length} text runs in all`] : low, `text below AA in ${roots}`).toEqual([]);
}

for (const width of [1280, 375]) {
  for (const scheme of ["light", "dark"] as const) {
    const at = `${width}px ${scheme}`;

    for (const device of ["plug-4m", "esp32c3-sensor"]) {
      test(`an offline device's tiles and cards stay readable and say Last reported: ${device} at ${at}`, async ({ page }) => {
        test.setTimeout(120_000);
        await setup(page, width, scheme);
        await openOffline(page, device);
        await expectReadable(page, "#devlive, #identity, #devconn");
        // and the state is said in words
        await expect(page.locator("#devlive .dev-tile-l [data-stale]").first()).toBeVisible();
        await expect(page.locator("#identity [data-stale]")).toBeVisible();
        await expect(page.locator("#devconn [data-stale]")).toBeVisible();
      });
    }

    test(`a rule page's Not judged rows stay readable at ${at}`, async ({ page }) => {
      await setup(page, width, scheme);
      await page.goto(app.url + "/policy");
      await page.getByRole("link", { name: "Power lock" }).click();
      await expect(page.locator("#rule-devices tbody tr").first()).toBeVisible();
      await expect(page.locator("#rule-devices")).toContainText("not a command in 13.4.0");
      await expectReadable(page, "#rule-devices tbody");
    });

    // #1174: the count inside a filter chip was drawn at opacity 0.75 on the span that holds it. Contrast alone still
    // passes at that opacity (about 8:1), so the check is the #1071 rule itself: no text at less than full opacity,
    // and the current chip, drawn inverted, keeps a readable count too.
    for (const [row, path] of [
      ["the rule page's All / Deviate / Not judged", "/policy"],
      ["Devices", "/devices"],
      ["Backups", "/devices/backups"],
    ]) {
      test(`the chip counts of ${row} are not dimmed by opacity and stay readable at ${at}`, async ({ page }) => {
        await setup(page, width, scheme);
        await page.goto(app.url + path);
        if (path === "/policy") {
          await page.getByRole("link", { name: "Power lock" }).click();
          await expect(page.locator("#rule-devices tbody tr").first()).toBeVisible();
        }
        const chips = page.locator(".dv-chips .chip");
        expect(await chips.count(), "the page shows its chips").toBeGreaterThan(1);
        await expect(page.locator(".dv-chips .chip[aria-current] .chip-n").first(), "one chip is the current one").toBeVisible();
        const runs = await textRuns(page, ".dv-chips");
        expect(runs.some((r) => r.where.includes("chip-n")), "the counts were measured").toBe(true);
        expect(faded(runs), "no chip text is drawn at less than full opacity").toEqual([]);
        expect(lowContrast(runs), "chip text below AA").toEqual([]);
      });
    }

    test(`the Policy rules list keeps a disabled rule readable at ${at}`, async ({ page }) => {
      await setup(page, width, scheme);
      await page.goto(app.url + "/policy");
      const row = page.locator("section#rules tbody tr", { hasText: "Flash wear" });
      await expect(row).toContainText("Disabled");
      await expectReadable(page, "section#rules tbody");
    });

    for (const [tab, roots] of [
      ["notifications", "#sec-notifications .scard"],
      ["discovery", "#card-scan"],
    ]) {
      test(`switched-off settings stay readable: ${tab} at ${at}`, async ({ page }) => {
        await setup(page, width, scheme);
        await page.goto(app.url + `/settings/system?tab=${tab}`);
        // the region really holds something that is switched off (sysform.js marks it is-off)
        const first = roots.split(",")[0];
        await expect(page.locator(`${first}.is-off, ${first} .is-off`).first()).toBeAttached();
        await expectReadable(page, roots);
      });
    }
  }
}

// A segmented control inside a switched-off area is inert like the inputs around it, so it takes the same disabled
// look: left at full strength it reads as a control that works and does nothing when clicked.
test("the webhook Authentication segments look disabled while the webhook switch is off", async ({ page }) => {
  await setup(page, 1280, "light");
  await page.goto(app.url + "/settings/system?tab=notifications");
  const seg = page.locator("#sec-notifications .dep.is-off .useg").first();
  await expect(seg).toBeVisible();
  await expect(seg.locator('input[name$="NOTIFY_WEBHOOK_AUTH"]').first()).toBeAttached();
  expect(await seg.evaluate((el) => parseFloat(getComputedStyle(el).opacity))).toBeLessThan(1);
});
