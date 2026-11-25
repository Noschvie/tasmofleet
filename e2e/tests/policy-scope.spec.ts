import { join } from "node:path";
import type { Locator } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// The rule form's "Applies to" section (#235): always open and full width, with a device search over
// a fixed-height list, tags with their counts, and exceptions from the same names. Editing the scope
// moves nothing on the page, and the tiles and the line under the title follow the draft.
let app: App;
let env: Devenv;

// chipShape is a chip's parts without its text or values: element, classes and, for the input, its type, name and
// whether it carries data-n. The server (ui.Chip) and policy.js draw the same chip; this tells when they stop doing so.
async function chipShape(chip: Locator): Promise<string> {
  return chip.evaluate((root) => {
    const shape = (el: Element): string => {
      const input = el instanceof HTMLInputElement ? `[${el.type}|${el.name}|${el.hasAttribute("data-n")}]` : "";
      const cls = [...el.classList].sort().join(".");
      return `${el.tagName.toLowerCase()}.${cls}${input}(${[...el.children].map(shape).join(",")})`;
    };
    return shape(root);
  });
}
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
    },
  });
  expect(app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)])).toContain("created:");
  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
  try {
    db.exec("PRAGMA busy_timeout=5000");
    const set = db.prepare("UPDATE devices SET tags = ? WHERE name = ?");
    const names = Object.keys(env.devices);
    names.forEach((n, i) => set.run(JSON.stringify(i % 2 === 0 ? ["lights"] : ["plugs"]), n));
  } finally {
    db.close();
  }
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

test("Applies to is an open section: search, chips, tags, exceptions, and nothing shifts", { tag: "@core" }, async ({ page }) => {
  const shots = process.env.UI_SHOTS_DIR;
  const shot = async (name: string) => {
    if (!shots) return;
    const size = page.viewportSize() ?? { width: 1280, height: 720 };
    for (const [w, h] of [[375, 800], [1280, 900]]) {
      for (const scheme of ["light", "dark"] as const) {
        await page.setViewportSize({ width: w, height: h });
        await page.emulateMedia({ colorScheme: scheme });
        // as tall as the page: a full-page capture would repeat the sticky header
        await page.setViewportSize({ width: w, height: await page.evaluate(() => document.documentElement.scrollHeight) });
        await page.evaluate(() => window.scrollTo(0, 0));
        await page.screenshot({ path: `${shots}/${name}-${w}-${scheme}.png` });
      }
    }
    await page.setViewportSize(size);
    await page.emulateMedia({ colorScheme: null });
  };
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/policy/rules/new?command=Timezone");
  await page.locator("input[name='want']").fill("99");
  await page.locator("input[name='name']").fill("Local timezone");

  // open without a click, after the severity and before the note
  const section = page.locator("[data-scope]");
  await expect(section).toBeVisible();
  await expect(section.getByRole("heading", { name: "Applies to" })).toBeVisible();
  const yOf = (sel: string) => page.locator(sel).first().evaluate((el) => el.getBoundingClientRect().top + window.scrollY);
  expect(await yOf("[data-scope]")).toBeGreaterThan(await yOf("#sev-l"));
  expect(await yOf("textarea[name='note']")).toBeGreaterThan(await yOf("[data-scope]"));

  const cmdY = await yOf("#rule-command");
  const list = section.locator("[data-scope-list]");
  const boxH = (await list.boundingBox())!.height;
  const tiles = page.locator("[data-tile='applies'] .dev-tile-v");
  const total = Object.keys(env.devices).length;
  await expect(tiles).toHaveText(String(total));
  await expect(section.locator("#scope-summary")).toContainText(`${total} devices`);

  // the search narrows the list by name, never past the box
  const first = Object.keys(env.devices)[0];
  await section.locator("[data-scope-search]").fill(first.slice(0, 5));
  await expect(section.locator("[data-scope-count]")).toContainText(`of ${total} devices match`);
  expect((await list.boundingBox())!.height).toBe(boxH);

  // a picked device is a chip, the tiles follow the draft (out-of-band), and the line too
  await list.locator(".pol-row:not([hidden]) input").first().check();
  await expect(section.locator("[data-dev-chips] .chip")).toHaveCount(1);
  await expect(tiles).toHaveText("1");
  await expect(page.locator("#rule-line")).toContainText("1 device");
  await expect(section.locator("#scope-summary")).toContainText("1 picked device");
  expect(await yOf("#rule-command")).toBe(cmdY);
  expect((await list.boundingBox())!.height).toBe(boxH);

  // tags in use carry their device count; ticking one turns it into a chip
  const pill = section.locator("[data-tag-chips] label.chip, [data-tag-used] label.chip", { hasText: "lights" });
  await expect(pill.locator(".chip-n")).toHaveText(String(Math.ceil(total / 2)));
  await pill.locator("input").check();
  await expect(section.locator("[data-tag-chips] label.chip")).toHaveCount(1);
  await expect(page.locator("#rule-line")).toContainText("tag lights");

  // the state of the approved mockup: ESP8266, tag lights, two picked devices, one exception
  await section.locator("label.chip", { hasText: "ESP8266" }).click();
  await section.locator("[data-scope-search]").fill("lamp");
  await list.locator(".pol-row:not([hidden]) input").nth(1).check();
  await expect(section.locator("[data-dev-chips] .chip")).toHaveCount(2);

  // exceptions come from the same names (devices or tags)
  const except = section.locator("[data-except-add]");
  const inventoryName = (await list.locator(".pol-row span").first().textContent())!; // a name the inventory really has
  await except.fill(inventoryName);
  await except.press("Enter");
  await expect(section.locator("[data-except-chips] label.chip")).toHaveCount(1);
  const exceptShape = await chipShape(section.locator("[data-except-chips] label.chip"));
  // the tile and the line under the title follow the exception, not only the section's own summary
  await expect(page.locator("[data-tile='applies'] .dev-tile-s")).toContainText("1 exception");
  await expect(page.locator("#rule-line")).toContainText("1 exception");
  await expect(page.locator("#rule-line")).toContainText("ESP8266, tag lights, 2 devices");
  await expect(section.locator("#scope-summary")).toContainText("ESP8266, tag “lights” or 2 picked devices, minus 1 exception");
  await shot("applies-to");

  // a new tag typed into the field becomes a chip
  await section.locator("[data-tag-add]").fill("garden");
  await section.locator("[data-tag-add]").press("Enter");
  await expect(section.locator("[data-tag-chips] label.chip")).toHaveCount(2);
  const gardenShape = await chipShape(section.locator("[data-tag-chips] label.chip", { hasText: "garden" }));

  // saved, the rule opens with the same form: family, tags, devices and the exception survive
  await page.getByRole("button", { name: "Save rule" }).click();
  await expect(page).toHaveURL(/\/policy\?ok=saved/);
  const ruleURL = app.url + "/policy/rules/" + new URL(page.url()).searchParams.get("rule");
  await page.goto(ruleURL);
  const saved = page.locator("[data-scope]");
  await expect(saved.locator("input[name='family'][value='esp8266']")).toBeChecked();
  await expect(saved.locator("[data-dev-chips] .chip")).toHaveCount(2);
  await expect(saved.locator("[data-tag-chips] label.chip")).toHaveCount(2);
  await expect(saved.locator("[data-except-chips] label.chip input")).toHaveValue(inventoryName);
  // policy.js draws these chips a second time in the browser: the parts must be the ones the server draws
  // (ui.Chip), or the two copies drift and a chip looks different before and after a save
  expect(await chipShape(saved.locator("[data-tag-chips] label.chip", { hasText: "garden" })), "a tag chip").toBe(gardenShape);
  expect(await chipShape(saved.locator("[data-except-chips] label.chip")), "an exception chip").toBe(exceptShape);
  await expect(saved.locator("#scope-summary")).toContainText("minus 1 exception");

  // unticking the exception removes its chip and, saved, the exception
  await saved.locator("[data-except-chips] label.chip").click();
  await expect(saved.locator("[data-except-chips] label.chip")).toHaveCount(0);
  await expect(page.locator("[data-tile='applies'] .dev-tile-s")).toContainText("0 exceptions");
  await page.getByRole("button", { name: "Save rule" }).click();
  await expect(page).toHaveURL(/\/policy\?ok=saved/);
  await page.goto(ruleURL);
  await expect(page.locator("[data-scope] [data-except-chips] label.chip")).toHaveCount(0);
});

test("without JavaScript the section is plain fields and still saves", async ({ browser }) => {
  const ctx = await browser.newContext({ javaScriptEnabled: false, extraHTTPHeaders: admin });
  const page = await ctx.newPage();
  await page.goto(app.url + "/policy/rules/new?command=Timezone");
  await page.locator("input[name='want']").fill("99");
  await page.locator("[data-scope] label.chip", { hasText: "ESP8266" }).click();
  await page.locator("[data-scope-list] input").first().check();
  await page.locator("input[name='except_extra']").fill("someone");
  await page.getByRole("button", { name: "Save rule" }).click();
  await expect(page).toHaveURL(/\/policy\?ok=saved/);
  await page.goto(app.url + "/policy/rules/" + new URL(page.url()).searchParams.get("rule"));
  // the saved exception is a ticked box: unticking it and saving removes it, with no script
  const exc = page.locator("input[name='except'][value='someone']");
  await expect(exc).toBeChecked();
  await exc.uncheck();
  await page.getByRole("button", { name: "Save rule" }).click();
  await expect(page).toHaveURL(/\/policy\?ok=saved/);
  await ctx.close();
});
