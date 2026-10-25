import { test, expect } from "../support/fixtures";
import { realisticPolicyRules } from "../support/seed";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// QA v0.9.0 follow-up, packages K and L: a phone fold (details.m-fold) is plain content from 45rem
// up, so a section title is a title and not a half-working toggle, and the policy pages read the
// same on a phone as on a desktop. CSS decides all of it, so it is checked on the computed styles.
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
      TASMOFLEET_POLICY_RULES: JSON.stringify([...JSON.parse(realisticPolicyRules), { command: "PowerOnState", want: "2", note: "e2e target" }]),
    },
  });
  expect(app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)])).toContain("created: lamp-1m");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});
test.use({ extraHTTPHeaders: { "X-authentik-username": "adam", "X-authentik-groups": "adm" } });

// The title of every card of a page: chevron shown or not, and how the card is painted.
async function cards(page: import("@playwright/test").Page) {
  return page.$$eval("section.card", (els) =>
    els.map((c) => {
      const s = c.querySelector(":scope > details > summary");
      const cs = getComputedStyle(c);
      return {
        title: (s ?? c.querySelector(":scope > h2"))?.textContent?.trim() ?? "",
        fold: !!s,
        chevron: s ? getComputedStyle(s, "::after").display !== "none" : false,
        background: cs.backgroundColor,
        border: cs.borderLeftColor,
      };
    }),
  );
}

// Why: the setup used to be four folds in two looks (framed and unframed, chevrons that
// contradicted the content). Design v2 makes it one card without folds (P8): nothing to open,
// nothing hidden, at every width.
test("Setup is one card without folds", async ({ page }) => {
  for (const width of [1280, 375]) {
    await page.setViewportSize({ width, height: 900 });
    await page.goto(app.url + "/setup");
    await expect(page.locator("main details")).toHaveCount(0);
    const bg = await page.locator(".auth-card").evaluate((c) => getComputedStyle(c).backgroundColor);
    expect(bg, "the card is not transparent").not.toBe("rgba(0, 0, 0, 0)");
    await expect(page.getByRole("button", { name: "Finish setup" })).toBeVisible();
  }
});

// Why: the policy page's sections are cards with a title and a surface (design v2, N3), not
// folds: nothing is hidden behind a chevron on a desktop.
test("desktop: policy sections are titled cards without folds", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/policy");
  for (const sel of [".pol-editor", ".pol-preview", ".pol-hist"]) {
    const sec = page.locator(sel);
    await expect(sec.locator("h2").first(), `${sel} title`).toBeVisible();
    const bg = await sec.evaluate((c) => getComputedStyle(c).backgroundColor);
    expect(bg, `${sel} background`).not.toBe("rgba(0, 0, 0, 0)");
  }
  await expect(page.locator("main details.m-fold")).toHaveCount(0);
});

// Why: on a phone the three columns stack (cards, editor, preview) and nothing scrolls sideways;
// each rule card keeps its name, its setting and its "⋯" menu on screen.
test("phone: the policy page stacks its columns and a rule card keeps its menu", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto(app.url + "/policy");
  const sw = await page.evaluate(() => document.documentElement.scrollWidth);
  expect(sw).toBeLessThanOrEqual(375);
  const card = page.locator(".pol-card").first();
  await expect(card.locator(".pol-card-name")).toHaveText("Keep power state");
  await expect(card.locator(".pol-card-kv")).toContainText("SetOption0");
  const box = (await card.boundingBox())!;
  const menu = (await card.getByLabel("Rule actions").boundingBox())!;
  expect(menu.x + menu.width).toBeLessThanOrEqual(box.x + box.width + 0.5);
  const ed = (await page.locator(".pol-editor").boundingBox())!;
  expect(ed.x, "the editor sits under the cards, not beside them").toBeLessThan(box.x + 1);
});

// Why: the Note had only a fold title as its label once; it is a labelled field at every width.
test("desktop: the rule editor's Note has a visible label", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/policy?new=1");
  const note = page.locator("#editor textarea[name=note]");
  await expect(note).toBeVisible();
  const label = note.locator("xpath=preceding-sibling::span[contains(@class,'ufield-label')]");
  await expect(label).toHaveText("Note");
  expect((await label.boundingBox())!.width, "visible label text").toBeGreaterThan(10);
});

// Why: "60 → 300" in the live preview is one value; it must not break between its parts while a
// long device name has room to wrap. (A value too long for the card wraps instead, see
// policy-long-value.spec.ts, so this checks the rendered height, not white-space: nowrap.)
test("desktop: a preview value stays on one line", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/policy");
  await page.getByRole("button", { name: "Run now" }).click();
  await page.goto(app.url + "/policy?edit=" + (await lastRuleIndex(page)));
  const val = page.locator(".pol-pv-val").first();
  await expect(val).toBeVisible({ timeout: 60_000 });
  const { height, font } = await val.evaluate((el) => ({ height: el.getBoundingClientRect().height, font: parseFloat(getComputedStyle(el).fontSize) }));
  expect(height, "a short value is one line (a second line would double the height)").toBeLessThan(font * 2);
});

// Why: the Make compliant plan (the page without JavaScript) keeps every card and its footer
// inside the screen on a phone.
test("phone: the make compliant plan stays inside the screen", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto(app.url + "/policy");
  await page.getByRole("button", { name: "Run now" }).click();
  await page.locator("form:has(button[data-dialog=dlg-fix])").evaluate((f: HTMLFormElement) => f.submit()); // the no-JS post
  await expect(page.locator(".pol-fix-card").first()).toBeVisible({ timeout: 15_000 });
  for (const b of await page.locator(".pol-fix-card, .pol-fix-foot").all()) {
    const r = (await b.boundingBox())!;
    expect(r.x).toBeGreaterThanOrEqual(0);
    expect(r.x + r.width).toBeLessThanOrEqual(375);
  }
});

/** lastRuleIndex is the position of the e2e target rule (PowerOnState 2, the last one). */
async function lastRuleIndex(page: import("@playwright/test").Page): Promise<number> {
  return (await page.locator(".pol-card").count()) - 1;
}
