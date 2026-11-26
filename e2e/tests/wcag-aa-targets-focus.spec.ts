import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// WCAG 2.2 AA failures of the layout that #1170 measured: the 24 px target of a sort link (2.5.8, #1272), the full
// name of a device under the text-spacing override (1.4.12, #1273) and focus that ends under a fixed bar (2.4.11,
// #1274). Focus under a pinned table column (#1275) is not here: scroll-padding-inline made every Tab onto a pinned
// cell scroll the table, see the pull request. Each check reads what the page does with the keyboard, so a
// stylesheet that stops doing it fails here whatever rule it moved. No timing of a live swap is waited for: the poll
// interval is an hour, so nothing replaces a row while a walk is under way.
let app: App;
let env: Devenv;
const adm = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

test.beforeAll(async () => {
  test.setTimeout(240_000);
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "1h",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_GITHUB_URL,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
  // the first poll fills the rows (status, Wi-Fi, firmware); the next one is an hour away
  const total = String(Object.keys(env.devices).length);
  await expect
    .poll(
      async () => {
        const html = await (await fetch(app.url + "/devices", { headers: adm })).text();
        return /Online<\/span>\s*<span class="chip-n">(\d+)<\/span>/.exec(html)?.[1];
      },
      { timeout: 120_000, intervals: [1000] },
    )
    .toBe(total);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const widths = [
  { name: "1280", width: 1280, height: 800 },
  { name: "375", width: 375, height: 667 },
];

async function settle(page: Page) {
  await page.evaluate(() => document.fonts.ready);
  await page.waitForTimeout(300);
}

/** sortLinks measures every sort link a header shows on screen. */
async function sortLinks(page: Page): Promise<{ label: string; w: number; h: number }[]> {
  return page.evaluate(() =>
    [...document.querySelectorAll<HTMLElement>("th .sortlink")]
      .filter((a) => a.getClientRects().length > 0)
      .map((a) => {
        const r = a.getBoundingClientRect();
        return { label: (a.textContent ?? "").trim(), w: Math.round(r.width * 10) / 10, h: Math.round(r.height * 10) / 10 };
      }),
  );
}

for (const vp of widths) {
  test(`sort links of a table header are 24 x 24 targets at ${vp.name} px (WCAG 2.5.8, #1272)`, { tag: "@core" }, async ({ page }) => {
    await page.setExtraHTTPHeaders(adm);
    await page.setViewportSize({ width: vp.width, height: vp.height });
    const seen: { label: string; w: number; h: number }[] = [];
    for (const path of ["/devices", "/audit"]) {
      await page.goto(app.url + path);
      await settle(page);
      seen.push(...(await sortLinks(page)));
    }
    // Below 45rem the device list is cards and a dense table hides its header too (the audit log's links are in the
    // markup, display none), so a phone has no sort link on screen to measure. A wide screen must have some, so a
    // page that lost its links cannot pass by having nothing to measure.
    if (vp.width >= 720) expect(seen.length, "sort links on screen").toBeGreaterThan(0);
    const small = seen.filter((l) => l.w < 24 || l.h < 24);
    expect(small, "sort links under 24 x 24 px").toEqual([]);
  });
}

/**
 * focusTarget is what the person sees for the focused element. A switch is a visually hidden checkbox under its
 * track, so the box itself is not what the eye finds: its stand-in is the track next to it, or the label it belongs
 * to. Every other element stands for itself.
 */
const FOCUS_TARGET = `
  (a) => {
    if (!(a instanceof HTMLInputElement)) return a;
    const r = a.getBoundingClientRect();
    if (getComputedStyle(a).opacity !== "0" && r.width >= 8 && r.height >= 8) return a;
    const next = a.nextElementSibling;
    if (next && next.matches(".sw") && next.getBoundingClientRect().width > 0) return next;
    return (a.labels && a.labels[0]) || a;
  }
`;

/**
 * walk presses Tab (Shift+Tab with back) `steps` times, or until the focus leaves the page, and lists every focused
 * element whose centre is not itself on top: a fixed bar or a pinned column lies over it (WCAG 2.4.11), or it is
 * outside the window. An element with no box (a skip link while hidden) is not judged.
 */
async function walk(page: Page, steps: number, back = false): Promise<string[]> {
  const problems: string[] = [];
  for (let i = 0; i < steps; i++) {
    await page.keyboard.press(back ? "Shift+Tab" : "Tab");
    const found = await page.evaluate((src) => {
      const a = document.activeElement as HTMLElement | null;
      if (!a || a === document.body || a === document.documentElement) return { stop: true as const };
      const pick = (0, eval)(src) as (e: Element) => Element;
      const t = pick(a) as HTMLElement;
      const r = t.getBoundingClientRect();
      const desc = `${a.tagName.toLowerCase()}${a.id ? "#" + a.id : ""}${a.className && typeof a.className === "string" ? "." + a.className.trim().split(/\s+/).join(".") : ""} "${(a.textContent ?? "").trim().slice(0, 24)}"`;
      if (r.width < 2 || r.height < 2) return { stop: false as const, skip: true as const, desc };
      const cx = r.left + r.width / 2;
      const cy = r.top + r.height / 2;
      if (cx < 0 || cy < 0 || cx > innerWidth || cy > innerHeight) return { stop: false as const, bad: `${desc} lies outside the window (${Math.round(cx)}, ${Math.round(cy)})` };
      const hit = document.elementFromPoint(cx, cy);
      const label = a instanceof HTMLInputElement ? a.labels?.[0] : null;
      // the top element is the target, something inside it, or (a switch's track ignores the pointer) its label, or
      // a box around it (a control that takes no pointer, like a blocked button, hands the point to its wrapper):
      // none of these covers it; only a different element on top does
      if (hit && (hit === t || t.contains(hit) || a.contains(hit) || hit.contains(t) || (label && label.contains(hit)))) return { stop: false as const };
      const over = hit ? `${hit.tagName.toLowerCase()}${hit.id ? "#" + hit.id : ""}${hit.className && typeof hit.className === "string" ? "." + hit.className.trim().split(/\s+/).join(".") : ""}` : "nothing";
      // a table part over a cell of the table (a pinned column) or the scroller's own edge is #1275, not a bar
      if (t.closest(".tablescroll") && !(hit && hit.closest("header.top, .ph, .sb"))) return { stop: false as const };
      return { stop: false as const, bad: `${desc} is under ${over} at (${Math.round(cx)}, ${Math.round(cy)}), scrollY ${Math.round(scrollY)}` };
    }, FOCUS_TARGET);
    if (found.stop) break;
    if ("bad" in found && found.bad) problems.push(found.bad);
  }
  return problems;
}

for (const vp of widths) {
  for (const path of ["/styleguide", "/devices"]) {
    test(`keyboard focus on ${path} is never under a fixed bar at ${vp.name} px (WCAG 2.4.11, #1274)`, async ({ page }) => {
      test.setTimeout(180_000);
      await page.setExtraHTTPHeaders(adm);
      await page.setViewportSize({ width: vp.width, height: vp.height });
      await page.goto(app.url + path);
      await settle(page);
      // forward from the top: the focus walks down and each new control lands at the lower edge, over the status bar
      const down = await walk(page, 300);
      // and back up from where it stopped: each lands at the upper edge, under the sticky header
      const up = await walk(page, 300, true);
      expect([...down, ...up], "focused controls covered by a bar").toEqual([]);
    });
  }
}

/**
 * A phone shows the filter chips of /devices as one strip that scrolls sideways, and fades the edge that has more
 * (#1133, #1079). Chromium scrolls a focused element into view only when none of it shows, so Tab onto a chip that
 * is half in the strip left it where it was: the person saw a few pixels of it, inside the fade, and the Filters
 * button next to the strip lay over its centre (the nightly walk above found that). So a chip that takes the
 * keyboard focus must lie whole inside the strip and clear of the faded edge (WCAG 2.4.11). The walk above only
 * asks for an uncovered centre; the clearance from the fade is what this adds, so it is not a duplicate of it.
 */
test("a chip that takes keyboard focus on /devices at 375 px lies whole inside its strip, clear of the fade (WCAG 2.4.11, #1274)", async ({ page }) => {
  test.setTimeout(120_000);
  await page.setExtraHTTPHeaders(adm);
  await page.setViewportSize({ width: 375, height: 667 });
  await page.goto(app.url + "/devices");
  await settle(page);
  const strip = page.locator(".dv-list .dv-chips");
  await expect(strip).toBeVisible();
  const chips = await strip.locator("a.chip, button.chip").count();
  expect(chips, "chips in the strip").toBeGreaterThan(4);
  // the strip must really scroll here, or there is nothing to test
  expect(await strip.evaluate((s) => s.scrollWidth > s.clientWidth + 1), "the strip scrolls sideways").toBe(true);

  /** reach is how far the focused chip lies over the strip's edge or into the fade of an edge that has more. */
  const reach = () =>
    page.evaluate(() => {
      const a = document.activeElement as HTMLElement | null;
      const s = document.querySelector<HTMLElement>(".dv-list .dv-chips");
      if (!a || !s || !s.contains(a)) return { inStrip: false as const };
      // the width of the fade is the token itself, measured the way the stylesheet reads it
      const probe = document.createElement("div");
      probe.style.width = "var(--sp-32)";
      document.body.append(probe);
      const fade = probe.getBoundingClientRect().width;
      probe.remove();
      const c = a.getBoundingClientRect();
      const r = s.getBoundingClientRect();
      const left = s.hasAttribute("data-more-start") ? fade : 0;
      const right = s.hasAttribute("data-more-end") ? fade : 0;
      return {
        inStrip: true as const,
        label: (a.textContent ?? "").trim().slice(0, 24),
        reach: Math.max(0, Math.round(r.left + left - c.left), Math.round(c.right - (r.right - right))),
        pageX: Math.round(document.scrollingElement!.scrollLeft),
      };
    });

  // Tab from the top of the page into the strip and on through every chip, then Shift+Tab back through all of them.
  // The key press is what makes the focus visible by keyboard (:focus-visible); a script's focus() is not the test.
  let key = "Tab";
  let inside = 0;
  for (let step = 0; step < 60; step++) {
    await page.keyboard.press(key);
    const now = await reach();
    if (!now.inStrip) {
      if (inside === 0) continue; // still on the way to the strip
      if (key === "Tab") {
        key = "Shift+Tab"; // left the strip at its end: walk back through it
        inside = 0;
        continue;
      }
      break;
    }
    inside++;
    // the focus lands at once (no smooth scrolling), but the strip's own scroll event updates the fade attributes a
    // frame later: poll the DOM state, never a timer
    await expect.poll(reach, { message: `chip ${now.label} (${key})`, timeout: 5_000 }).toMatchObject({ inStrip: true, reach: 0, pageX: 0 });
  }
  expect(inside, "chips walked through on the way back").toBe(chips);
});

test("the name of a device is shown in full under the text-spacing override (WCAG 1.4.12, #1273)", async ({ page }) => {
  await page.setExtraHTTPHeaders(adm);
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto(app.url + "/devices");
  await settle(page);
  const link = page.locator("table.devtable td.dv-name > a").first();
  await expect(link).toBeVisible();
  // the four values of 1.4.12, on everything (the paragraph spacing has no paragraph here), and a name that needs more than two lines
  // (a constructed sheet: the page's CSP refuses an inline <style>)
  await page.evaluate(() => {
    const sheet = new CSSStyleSheet();
    sheet.replaceSync("* { line-height: 1.5 !important; letter-spacing: 0.12em !important; word-spacing: 0.16em !important; }");
    document.adoptedStyleSheets = [...document.adoptedStyleSheets, sheet];
  });
  await link.evaluate((a) => {
    a.textContent = "Workshop ceiling lights north east corner relays, west wall spotlight controller and the long cable run to the garage";
  });
  // scrollHeight is the height of all the text, also of what a clamp or overflow hides
  const box = await link.evaluate((a) => ({ text: a.textContent!.length, scroll: a.scrollHeight, client: a.clientHeight, lines: Math.round(a.scrollHeight / parseFloat(getComputedStyle(a).lineHeight)) }));
  expect(box.text, "a long name").toBeGreaterThan(48);
  expect(box.lines, "the spacing makes the name longer than the two lines the clamp showed").toBeGreaterThanOrEqual(3);
  expect(box.scroll, "no part of the name is cut off").toBeLessThanOrEqual(box.client);
});
