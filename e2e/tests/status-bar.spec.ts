import { test, expect } from "../support/fixtures";
import { designAdmin } from "../support/seed";
import { App, startApp } from "../support/stack";
import { startVisualStack, type VisualStack } from "../support/visual-stack";

// #307: the status bar is fixed to the bottom of every page. It never scrolls away, nothing hides
// behind it, the phone shows one 44 px line that opens into a sheet, and the version it carries
// opens What's new.
let app: App;
test.beforeAll(async () => {
  app = await startApp({
    env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32", TASMOFLEET_ADMIN_GROUPS: "adm" },
  });
});
test.afterAll(async () => app?.stop());
test.use({ extraHTTPHeaders: { "X-authentik-username": "adam", "X-authentik-groups": "adm" } });

for (const [width, line] of [[1280, 32], [375, 44]] as const) {
  test(`the bar stays at the window's bottom edge while the page scrolls (${width}px)`, { tag: "@core" }, async ({ page }) => {
    await page.setViewportSize({ width, height: 600 });
    await page.goto(app.url + "/styleguide"); // a long page
    const bar = page.locator("footer.sb");
    await expect(bar).toBeVisible();
    const edge = async () => (await bar.boundingBox())!;
    const before = await edge();
    await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
    const after = await edge();
    expect(after.y).toBeCloseTo(before.y, 0);
    expect(after.y + after.height).toBeCloseTo(600, 0);
    expect(after.height).toBeGreaterThanOrEqual(line);
    // the last thing on the page ends above the bar
    const last = await page.evaluate(() => document.querySelector("main")!.getBoundingClientRect().bottom);
    expect(last).toBeLessThanOrEqual(after.y + 0.5);
  });
}

// #773: main kept a 120 px bottom padding from before the status bar existed, on top of the room the
// body reserves for the bar, so every page scrolled ~120 px past its content and ended in an empty
// strip. The room is the body's alone: at the end of the scroll the content ends one normal gap
// (--sp-16) above the bar. The Devices list with the 60-device fleet is taller than any window.
test.describe("the end of a long page", () => {
  test.describe.configure({ mode: "serial" });
  let fleet: VisualStack;
  test.beforeAll(async ({ browser }) => {
    test.setTimeout(600_000);
    fleet = await startVisualStack("fleet", browser);
  });
  test.afterAll(async () => fleet?.stop());

  for (const [width, height] of [[1280, 800], [375, 700]] as const) {
    test(`the content ends one gap above the status bar at the end of the scroll (${width}px)`, async ({ page }) => {
      await page.setViewportSize({ width, height });
      await page.setExtraHTTPHeaders(designAdmin);
      await page.goto(fleet.app.url + "/devices");
      // a phone shows cards, the table is in the page but hidden
      await page.locator("table[data-table=devices] tbody tr, li.dcard").filter({ visible: true }).first().waitFor();
      await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
      const m = await page.evaluate(() => {
        const bar = document.querySelector("footer.sb")!.getBoundingClientRect();
        // the last thing the page shows in main (not a fixed bar, not something hidden)
        const shown = [...document.querySelector("main")!.children].filter((c) => {
          const cs = getComputedStyle(c);
          return cs.display !== "none" && cs.position !== "fixed" && c.getBoundingClientRect().height > 0;
        });
        const probe = document.createElement("div");
        probe.style.height = "var(--sp-16)";
        document.body.append(probe);
        const gap = probe.getBoundingClientRect().height;
        probe.remove();
        return {
          tall: document.documentElement.scrollHeight > innerHeight + 200,
          reach: Math.round(window.scrollY + innerHeight) >= document.documentElement.scrollHeight - 1,
          gap,
          between: bar.top - shown[shown.length - 1].getBoundingClientRect().bottom,
        };
      });
      expect(m.tall, "the page has to be taller than the window for this to mean anything").toBe(true);
      expect(m.reach, "the scroll reached the end").toBe(true);
      expect(m.gap).toBe(16);
      expect(Math.abs(m.between - m.gap), `${m.between}px between the content and the bar, one gap is ${m.gap}px`).toBeLessThanOrEqual(2);
    });
  }
});

test("on a phone the bar is one line that opens into a sheet", { tag: "@core" }, async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 700 });
  await page.goto(app.url + "/devices");
  await expect(page.locator(".sb-wide")).toBeHidden();
  const sum = page.locator(".sb-narrow summary");
  await expect(sum).toContainText("No devices yet"); // this app has no device: "All good" would claim a fleet that is not there (#1077)
  await expect(sum).not.toContainText("All good");
  await sum.click();
  await expect(page.locator(".sb-sheet")).toContainText("Version");
  await page.locator(".sb-sheet").getByRole("link", { name: "What’s new" }).click();
  await expect(page).toHaveURL(/\/whats-new$/);
});

// Why (#1077, A1-014): the phone bar was a dot, a sentence and a caret that screen readers skip: nothing named the
// control or said it opens. The summary says "Status", and the browser's own expanded state follows the sheet. That state
// is native (details/summary), not a server-rendered aria-expanded: a bar refresh swaps the summary's inside while the
// sheet is open, so an attribute written by the server would say "false" for an open sheet.
test("on a phone the status toggle is named Status and says whether the sheet is open", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 700 });
  await page.goto(app.url + "/devices");
  const sum = page.locator(".sb-narrow summary");
  await expect(sum).toContainText("Status");
  const cdp = await page.context().newCDPSession(page);
  const axState = async () => {
    const { root } = await cdp.send("DOM.getDocument", { depth: 0 });
    const { nodeId } = await cdp.send("DOM.querySelector", { nodeId: root.nodeId, selector: ".sb-narrow summary" });
    const { nodes } = await cdp.send("Accessibility.getPartialAXTree", { nodeId, fetchRelatives: false });
    const n = nodes[0];
    const prop = (name: string) => n.properties?.find((p) => p.name === name)?.value.value;
    return { name: String(n.name?.value ?? ""), expanded: prop("expanded") };
  };
  const closed = await axState();
  expect(closed.name).toContain("Status");
  expect(closed.expanded, "closed sheet").toBe(false);
  await sum.click();
  await expect(page.locator(".sb-sheet")).toBeVisible();
  expect((await axState()).expanded, "open sheet").toBe(true);
});

// Why: a page without a stream is a snapshot; the bar then makes no claim of being live.
test("a page without a stream has no Live item, a live page does", async ({ page }) => {
  await page.goto(app.url + "/users");
  await expect(page.locator("footer.sb .sb-live")).toHaveCount(0);
  await page.goto(app.url + "/devices");
  await expect(page.locator("footer.sb .sb-wide .sb-l-ok")).toBeVisible();
});

// Why: htmx's own throttle runs the first request of a burst and drops the rest, so the last state
// of a bulk command (its final "done") never showed and the bar pulsed "11 of 12" for hours. The
// refresh is leading and trailing: at most one per 2 s, and one after the last event.
test("a burst of job events ends with the bar showing the final state", { tag: "@core" }, async ({ page }) => {
  let n = 0;
  await page.route("**/statusbar", (route) => {
    n++;
    return route.fulfill({
      contentType: "text/html",
      body: `<div hx-swap-oob="innerHTML:#sb-wide"><span id="state">state ${n}</span></div>`,
    });
  });
  await page.goto(app.url + "/devices");
  await expect(page.locator("footer.sb[hx-get]")).toBeVisible();
  const sent: number[] = [];
  await page.route("**/statusbar", (route) => { sent.push(Date.now()); return route.fallback(); });
  // 12 events in under a second, as the stream delivers them for a bulk command
  await page.evaluate(async () => {
    const bar = document.querySelector("footer.sb")!;
    for (let i = 0; i < 12; i++) {
      bar.dispatchEvent(new CustomEvent("htmx:sseBeforeMessage", { bubbles: true, cancelable: true, detail: { type: "job" } }));
      await new Promise((r) => setTimeout(r, 60));
    }
  });
  const lastEvent = Date.now();
  // the trailing refresh arrives after the last event, so the final server state is what shows
  await expect(page.locator("#sb-wide #state")).toHaveText("state 2", { timeout: 5000 });
  expect(n).toBe(2); // one at once, one after the burst: never one per event
  expect(sent[sent.length - 1]).toBeGreaterThanOrEqual(lastEvent - 100);
});

test("a landscape phone's open sheet stays inside the window", async ({ page }) => {
  await page.setViewportSize({ width: 667, height: 375 });
  await page.goto(app.url + "/devices");
  await page.locator(".sb-narrow summary").click();
  const sheet = (await page.locator(".sb-sheet").boundingBox())!;
  expect(sheet.y).toBeGreaterThanOrEqual(0);
  if (process.env.SHOT_DIR) await page.screenshot({ path: `${process.env.SHOT_DIR}/landscape-sheet-667x375.png` });
});
