import { test, expect } from "../support/fixtures";
import { measureCards } from "../support/card-widths";
import { pageList } from "../support/page-list";
import { realisticDevenvArgs, realisticPolicyRules, seedRealisticData } from "../support/seed";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Design rule (#419, design-system.md "Card widths follow their content"), measured at 1280 px:
//   1. a card that takes the full content width uses at least 60 % of it (its content wants that
//      much room), and
//   2. a page's cards span the Dashboard's content width.
// A page breaks a rule only through an entry below. Each entry says why, and an entry whose page
// now passes fails the test too ("remove it"), so the list shrinks as pages are converted: a
// group of the #419 conversion deletes its own block when it lands.
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
let app: App;
let env: Devenv;
let lamp = "";
let txn = "";
const REPORT = !!process.env.CARD_WIDTHS_REPORT; // print every finding instead of failing

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
    const hrefOf = async (n: string) => (await p.getByRole("link", { name: n, exact: true }).first().getAttribute("href"))!.replace(app.url, "");
    lamp = await hrefOf("lamp-1m");
    // a backup, so the Backups page has content
    await p.goto(app.url + lamp);
    await p.getByRole("button", { name: "Back up", exact: true }).click();
    await expect(p.getByText(/backed up|Backup (taken|saved)/i).first()).toBeVisible({ timeout: 30_000 });
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

type Exempt = { page: string; rule: "use" | "span" | "cap"; card?: string; borderline?: boolean };
const use = (page: string, card: string, borderline = false): Exempt => ({ page, rule: "use", card, borderline });
const span = (page: string): Exempt => ({ page, rule: "span" });
const cap = (page: string): Exempt => ({ page, rule: "cap" }); // spans 1204 px at 1280 but not the 1576 px at 1920

const exempt: Exempt[] = [
  // Deliberate exceptions. The card is wide for a reason the rule does not know.
  use("device console", "console"), // the terminal: a console is wide, its text is not
  use("device settings", "devform-foot"), // the save bar under the settings columns: one line
  use("system settings advanced", "sys-danger"), // the "Reset all settings" strip: one line
  span("policy io"), // import/export stays a narrow card
  use("policy rule", "History of this rule"), // a collapsed <details>: one line until opened
  use("transactions", "History", true), // 61 % today (#419 decision): a hair under the line must not fail the guard

  // Single-form and confirmation pages stay narrow (decision of 2026-11-05).
  span("device edit"),
];

// Not measured: the narrow single-form and confirmation pages other than device edit (device delete,
// ui.ConfirmPage, add user), sign-in and first-run setup, error pages, states that need data the devenv
// fleet does not have (an empty fleet, the Replace preview, which needs a spare board of the same chip
// family and an old device that no longer answers: replace.spec.ts measures it), Drift (a table without a card), the phone
// and tablet widths (card-stacks.spec.ts guards the stacks there) and pages of other roles.
const pages = pageList({ lamp: () => lamp, txn: () => txn, fwVersion: () => env.vars.DEVENV_FW_VERSION });

for (const [name, path] of pages) {
  test(`${name}: cards follow their content at 1280px`, async ({ page }) => {
    await page.setExtraHTTPHeaders(adam);
    await page.setViewportSize({ width: 1280, height: 900 });
    const resp = await page.goto(app.url + path());
    expect(resp?.status(), `GET ${path()}`).toBeLessThan(400);
    await page.waitForTimeout(800);
    const m = await measureCards(page);
    expect(m.cards.length, "the page has cards").toBeGreaterThan(0);
    const found: Exempt[] = [];
    let capSpan = "";
    for (const c of m.cards) if (c.fullWidth && c.usePct < 60) found.push(use(name, c.card));
    if (m.span < m.area * 0.95) found.push(span(name));
    else {
      // decision 9: on a large screen too the page fills the capped content width (1576 px at 1920),
      // so a max-width below it (a fixed max-width) shows here and nowhere at 1280
      await page.setViewportSize({ width: 1920, height: 900 });
      await page.goto(app.url + path());
      await page.waitForTimeout(800);
      const w = await measureCards(page);
      if (w.span < w.area * 0.95) {
        found.push(cap(name));
        capSpan = `page spans ${w.span} of ${w.area} px at 1920`;
      }
    }
    const key = (e: Exempt) => `${e.page}|${e.rule}|${e.card ?? ""}`;
    if (REPORT) {
      console.log("FINDINGS", name, JSON.stringify({ area: m.area, left: m.left, span: m.span, found: found.map(key), cards: m.cards.map((c) => `${c.card} ${c.width}px ${c.usePct}%${c.multiColumn ? " (columns)" : ""}`) }));
      return;
    }
    const exempted = new Set(exempt.filter((e) => e.page === name).map(key));
    const unlisted = found.filter((e) => !exempted.has(key(e)));
    expect(
      unlisted.map((e) => (e.rule === "span" ? `page spans ${m.span} of ${m.area} px` : e.rule === "cap" ? capSpan : `card "${e.card}" is full width but uses under 60 %`)),
      `${name}: breaks the card-width rule; convert it or add a reason to the exemption list`,
    ).toEqual([]);
    // a borderline entry may pass or fail the 60 % line, but not fall far below it
    for (const e of exempt.filter((x) => x.page === name && x.borderline)) {
      const c = m.cards.find((x) => x.card === e.card);
      expect(c?.usePct ?? 100, `${name}: "${e.card}" is borderline, not allowed under 50 %`).toBeGreaterThanOrEqual(50);
    }
    const borderline = new Set(exempt.filter((e) => e.page === name && e.borderline).map(key));
    const stale = [...exempted].filter((k) => !borderline.has(k) && !found.some((e) => key(e) === k));
    expect(stale, `${name}: exemption entries that no longer apply; delete them`).toEqual([]);
  });
}

// The metric itself: the max-content width of a list in columns is one column, so a card that is
// full of entries in three columns must not read as 33 % used (device Activity after #419-D). A
// list in one column that fills only the left of a full-width card still reads as unused.
test.describe("card use metric", () => {
  const html = (items: number, cols: number) => `<style>
    body{margin:0;background:#fff} main{display:block;padding:0} .card{border:1px solid #888;border-radius:10px;padding:16px;background:#fafafa}
    ul{list-style:none;margin:0;padding:0;display:grid;grid-template-columns:repeat(auto-fill,minmax(${cols === 1 ? "100%" : "22rem"},1fr));gap:8px 24px}
    li{white-space:nowrap}</style>
    <main><div class="card"><h2>Activity</h2><ul>${Array.from({ length: items }, (_, i) => `<li>Entry ${i} of the list with some words</li>`).join("")}</ul></div></main>`;
  test("a full list in three columns counts as used", async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.setContent(html(12, 3));
    const m = await measureCards(page);
    expect(m.cards[0]).toMatchObject({ fullWidth: true, multiColumn: true });
    expect(m.cards[0].maxContentPct, "max-content is one column").toBeLessThan(60);
    expect(m.cards[0].usePct).toBeGreaterThanOrEqual(60);
  });
  test("one row of equal cells is not a list in columns", async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.setContent(`<style>body{margin:0;background:#fff} main{display:block;padding:0} .card{border:1px solid #888;border-radius:10px;padding:16px;background:#fafafa}
      .row{display:grid;grid-template-columns:repeat(4,1fr);gap:8px}</style>
      <main><div class="card"><h2>Summary</h2><div class="row"><span>Size</span><span>Date</span><span>Hash</span><span>Used</span></div></div></main>`);
    const m = await measureCards(page);
    expect(m.cards[0]).toMatchObject({ fullWidth: true, multiColumn: false });
    expect(m.cards[0].usePct, "four short words stretched across the card use little of it").toBeLessThan(60);
  });
  test("a list in one column in a full-width card is unused width", async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.setContent(html(12, 1));
    const m = await measureCards(page);
    expect(m.cards[0]).toMatchObject({ fullWidth: true, multiColumn: false });
    expect(m.cards[0].usePct).toBeLessThan(60);
  });
});
