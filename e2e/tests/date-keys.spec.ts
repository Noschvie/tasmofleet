import { test, expect } from "../support/fixtures";
import { designAdmin } from "../support/seed";
import { startVisualStack, type VisualStack } from "../support/visual-stack";
import type { Browser, Page } from "@playwright/test";

// #1104 (UI audit DS-005, A2-006, A7-015, A4-021): one instant was written five ways, by whichever of twelve
// keys the page picked: "11/20/2026, 02:17:25 PM" in one table and "20 Nov, 14:17" in the next. Now every visible
// date is a <time data-ts> with one of a few named keys, and the keys agree on how a date and a clock time look.
//
// Nothing here knows ICU's spelling: the texts of the browser under test are compared with each other (the
// "shape" of a text: digit runs, letter runs), never with a hard-coded string or a copy of time.js's options.

/** the keys time.js knows (static/time.js SPECS, ui/stamp.go); stamp_test.go keeps those two lists equal */
const KEYS = ["date", "date-time", "month-day", "relative", "time", "clock-24", "start", "weekday-time", "title"];
/** the keys whose text ends in a clock time of the browser's locale (12 or 24 hour). clock-24 is a log column that is 24-hour on purpose, title has no text of its own. */
const WITH_CLOCK = ["date-time", "relative", "time", "start", "weekday-time"];

/** shape is a text with every run of digits written 9 and every run of letters (with an optional dot) written a: "Nov 20, 2026, 2:17 PM" and "Dec 3, 2025, 11:05 AM" have one shape. */
const shape = (text: string) =>
  text
    .replace(/\s+/g, " ")
    .trim()
    .replace(/\d+/g, "9")
    .replace(/\p{L}+\.?/gu, "a");

/** clockOf splits a text into what comes before its clock time and the clock time ("2:17 PM", "14:17"); null when it has none. */
function splitClock(text: string): { before: string; clock: string } | null {
  const flat = text.replace(/\s+/g, " ").trim();
  const m = /(\d{1,2}[:.]\d{2}(?: \p{L}+\.?)?)$/u.exec(flat);
  return m ? { before: flat.slice(0, m.index).replace(/[,\s]+$/, ""), clock: m[1] } : null;
}

interface Stamp {
  key: string;
  text: string;
  iso: string;
  where: string;
}

let fleet: VisualStack;
test.describe.configure({ mode: "serial" });
test.beforeAll(async ({ browser }) => {
  test.setTimeout(600_000);
  fleet = await startVisualStack("fleet", browser);
});
test.afterAll(async () => fleet?.stop());

const locales = ["en-US", "de-DE"] as const;

async function stampsOf(page: Page, where: string): Promise<Stamp[]> {
  // time.js marks every <time data-ts> it has handled; until then the server's fallback text is hidden
  await page.waitForFunction(() => document.querySelectorAll("time[data-ts]:not([data-painted])").length === 0);
  return page.$$eval(
    "time[data-ts]",
    (els, w) => els.map((e) => ({ key: e.getAttribute("data-ts") ?? "", text: e.textContent ?? "", iso: e.getAttribute("datetime") ?? "", where: w })),
    where,
  );
}

async function openIn(browser: Browser, locale: string) {
  const ctx = await browser.newContext({ locale, timezoneId: "Europe/Berlin", viewport: { width: 1280, height: 900 }, extraHTTPHeaders: designAdmin });
  return { ctx, page: await ctx.newPage() };
}

/** the pages whose stamps the audit named: the device overview, Backups, Firmware, the audit log and a user's page */
const pages = (d2: string): [string, string][] => [
  ["device overview", `/devices/${d2}`],
  ["device backups", `/devices/${d2}/backups`],
  ["firmware", "/firmware"],
  ["audit", "/audit"],
  ["user detail", "/users/principal/maren"],
];

for (const locale of locales) {
  // Why: a key that time.js does not know leaves the server's fallback on the page, so a stamp with a retired or
  // misspelt key is drawn in whatever layout Go happened to pick. Every stamp is one of the named keys, and two
  // stamps of one key are one pattern (same fields, same order), whichever page they sit on.
  test(`every <time data-ts> of the main pages has a named key, and one key is one pattern (${locale})`, async ({ browser }) => {
    const { ctx, page } = await openIn(browser, locale);
    const all: Stamp[] = [];
    for (const [name, path] of pages(fleet.placeholders.get("d2")!)) {
      await page.goto(fleet.app.url + path);
      const stamps = await stampsOf(page, name);
      expect(stamps.length, `${name} shows at least one date (else this test proves nothing there)`).toBeGreaterThan(0);
      all.push(...stamps);
    }
    for (const s of all) expect(KEYS, `${s.where}: "${s.text}" carries data-ts="${s.key}"`).toContain(s.key);

    // two stamps of one key render one pattern; "relative" is Today, Yesterday or a date (the year is only written when it is not this year), so it is grouped by those
    const group = (s: Stamp) => {
      if (s.key !== "relative") return s.key;
      const first = s.text.replace(/\s+/g, " ").trim().split(" ")[0];
      return first === "Today" || first === "Yesterday" ? `relative ${first}` : /\b\d{4}\b/.test(s.text) ? "relative dated-with-year" : "relative dated";
    };
    const byGroup = new Map<string, Stamp[]>();
    for (const s of all) if (s.key !== "title") byGroup.set(group(s), [...(byGroup.get(group(s)) ?? []), s]);
    for (const [g, list] of byGroup) {
      const shapes = new Map(list.map((s) => [shape(s.text), s.text]));
      expect([...shapes.values()], `${g}: stamps of one key must read the same way (${[...shapes.values()].join(" | ")})`).toHaveLength(1);
    }

    // a 12-hour clock never sits beside a 24-hour one: every clock time on these pages has one shape
    const clocks = new Map<string, string>();
    for (const s of all.filter((x) => WITH_CLOCK.includes(x.key))) {
      const c = splitClock(s.text);
      if (c) clocks.set(shape(c.clock), `"${c.clock}" in ${s.where}`);
    }
    expect([...clocks.values()], "one clock shape on the pages").toHaveLength(Math.min(clocks.size, 1));
    await ctx.close();
  });

  // Why: the pages above only hold the stamps the seed happens to have. This one writes every key at fixed
  // instants (today, yesterday, this year, an earlier year) through time.js itself and compares the keys with
  // each other: the date of a date-time is written like a date, an older relative day is written like a
  // date-time, and every clock time has the locale's one shape (no 02:17 PM beside 14:17 beside 2:17 PM).
  // It fails when the keys are merely renamed but keep their old, different fields.
  test(`the keys agree on how a date and a clock time look (${locale})`, async ({ browser }) => {
    const { ctx, page } = await openIn(browser, locale);
    // the browser's clock, so that "Today", "Yesterday" and "this year" are the same on every day the test runs
    await page.clock.setFixedTime(new Date("2026-11-26T12:00:00Z"));
    await page.goto(fleet.app.url + "/devices");
    await stampsOf(page, "devices");
    const instants: Record<string, string> = {
      today: "2026-11-26T09:41:00Z",
      yesterday: "2026-11-25T20:10:00Z",
      thisYear: "2026-08-19T18:02:00Z",
      earlier: "2025-08-19T18:02:00Z",
    };
    const text = await page.evaluate(
      ([keys, at]) => {
        const fmt = (window as unknown as { tasmofleetFormatTime(iso: string, key: string): string }).tasmofleetFormatTime;
        const out: Record<string, Record<string, string>> = {};
        for (const k of keys) out[k] = Object.fromEntries(Object.entries(at).map(([n, iso]) => [n, fmt(iso, k)]));
        return out;
      },
      [KEYS.filter((k) => k !== "title"), instants] as [string[], Record<string, string>],
    );
    for (const k of KEYS.filter((k) => k !== "title"))
      for (const [n, t] of Object.entries(text[k])) expect(t, `key ${k} at "${n}" has a text (an unknown key leaves the server's fallback)`).not.toBe("");

    // relative says Today and Yesterday, else it is the date-time (without the year in this year)
    expect(text.relative.today).toMatch(/^Today /);
    expect(text.relative.yesterday).toMatch(/^Yesterday /);
    expect(shape(text.relative.earlier), "an older day is a date-time").toBe(shape(text["date-time"].earlier));
    expect(text.relative.thisYear, "this year leaves the year out").not.toMatch(/2026/);
    expect(text["date-time"].thisYear, "a date-time always names its year").toMatch(/2026/);

    // the date of a date-time is a date, and a clock time is one shape everywhere
    const dateTime = splitClock(text["date-time"].earlier)!;
    expect(dateTime, "a date-time ends in a clock time").not.toBeNull();
    expect(shape(dateTime.before), "the date of a date-time is written like a date").toBe(shape(text.date.earlier));
    const clocks = new Map<string, string>();
    for (const k of WITH_CLOCK)
      for (const [n, t] of Object.entries(text[k])) {
        const c = splitClock(t);
        expect(c, `key ${k} at "${n}" ("${t}") ends in a clock time`).not.toBeNull();
        clocks.set(shape(c!.clock), `${k}: ${c!.clock}`);
      }
    expect([...clocks.values()], "every clock time is written one way").toHaveLength(1);

    // no seconds in a date-time; the log column keeps them
    expect(shape(text["clock-24"].today)).toBe(shape("09:41:00"));
    expect(splitClock(text["date-time"].today)!.clock).not.toMatch(/\d:\d\d:\d\d/);
    await ctx.close();
  });
}
