import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { designAdmin } from "../support/seed";
import { startVisualStack, type VisualStack } from "../support/visual-stack";

// v0.10.1 guarantee, kept at the default layout: a pinned column never covers the content of a
// column that scrolls. docs/media/devices.png showed Power's toggles lying over the Uptime text
// (and a two-relay device's second toggle pushed out of its cell). The fleet is the 60-device
// design fixture, the same the README screenshot is taken from, so multi-relay devices are in it.
// One fleet for all four checks (one worker): seeding it takes a minute or more on a loaded runner.
test.describe.configure({ mode: "serial" });
let stack: VisualStack;
test.beforeAll(async ({ browser }) => {
  test.setTimeout(600_000);
  stack = await startVisualStack("fleet", browser);
});
test.afterAll(async () => {
  await stack?.stop();
});

/**
 * overlaps lists what breaks the guarantee: a pinned cell whose content sticks out of it (a column
 * narrower than its content), and a scrolling cell whose content lies under a pinned column. A
 * table wider than its frame scrolls, and what has not scrolled in yet lies under the sticky
 * columns: that is scrolling, not overlap. A pinned group sits in its natural place at its own end
 * of the scroll range (left pins at the start, right pins at the end), so there nothing may lie under it.
 */
async function overlaps(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const t = document.querySelector<HTMLTableElement>("table[data-table=devices]")!;
    const scroller = t.closest<HTMLElement>(".tablescroll")!;
    const side = new Map<number, string>();
    for (const c of t.classList) {
      const m = /^p([lr])(\d+)$/.exec(c);
      if (m) side.set(parseInt(m[2], 10) - 1, m[1]);
    }
    const hidden = (c: HTMLElement) => c.offsetParent === null;
    // the box of what a cell shows: text and elements, without the resize handle and the folded part of a closed menu
    const content = (c: HTMLElement) => {
      let l = Infinity,
        r = -Infinity,
        top = Infinity,
        b = -Infinity;
      const add = (q: DOMRect) => {
        if (q.width <= 0 || q.height <= 0) return;
        l = Math.min(l, q.left);
        r = Math.max(r, q.right);
        top = Math.min(top, q.top);
        b = Math.max(b, q.bottom);
      };
      const walk = (n: Node) => {
        if (n.nodeType === 3) {
          if (!n.textContent!.trim()) return;
          const g = document.createRange();
          g.selectNodeContents(n);
          add(g.getBoundingClientRect());
        } else if (n instanceof HTMLElement) {
          if (n.classList.contains("colhandle") || n.classList.contains("sr-only") || n.closest("details:not([open]) > :not(summary)")) return;
          if (!n.childNodes.length || n.matches("svg,input,button,label,.sw")) add(n.getBoundingClientRect());
          n.childNodes.forEach(walk);
        }
      };
      c.childNodes.forEach(walk);
      return isFinite(l) ? new DOMRect(l, top, r - l, b - top) : new DOMRect();
    };
    const hit = (a: DOMRect, b: DOMRect) => a.width > 0 && a.left < b.right - 0.5 && b.left < a.right - 0.5;
    const out: string[] = [];
    for (const end of ["l", "r"]) {
      scroller.scrollLeft = end === "l" ? 0 : scroller.scrollWidth;
      for (const row of t.querySelectorAll<HTMLTableRowElement>("thead tr, tbody tr")) {
        const cells = [...row.cells] as HTMLElement[];
        if (cells.length < side.size) continue;
        cells.forEach((c, i) => {
          if (hidden(c)) return;
          const k = content(c);
          if (side.has(i)) {
            const own = c.getBoundingClientRect();
            if (end === "l" && k.width > 0 && (k.left < own.left - 0.5 || k.right > own.right + 0.5))
              out.push(`pinned ${c.dataset.label ?? i} shows ${Math.round(k.left)}-${Math.round(k.right)} in a cell of ${Math.round(own.left)}-${Math.round(own.right)}`);
            return;
          }
          cells.forEach((p, j) => {
            if (side.get(j) !== end || hidden(p)) return;
            const b = p.getBoundingClientRect();
            if (hit(k, b)) out.push(`${row.parentElement!.tagName.toLowerCase()} ${c.dataset.label ?? c.textContent!.trim()} ${Math.round(k.left)}-${Math.round(k.right)} under pinned ${p.dataset.label ?? j} ${Math.round(b.left)}-${Math.round(b.right)}`);
          });
        });
      }
    }
    scroller.scrollLeft = 0;
    return [...new Set(out)];
  });
}

for (const theme of ["light", "dark"]) {
  for (const width of [1280, 1440]) {
    test(`default layout, ${theme}, ${width} px: pinned columns show all their content and nothing lies under them`, async ({ page }) => {
      await page.setViewportSize({ width, height: 900 });
      await page.setExtraHTTPHeaders(designAdmin);
      await page.goto(stack.app.url + "/devices");
      await page.evaluate((t) => document.documentElement.setAttribute("data-theme", t), theme);
      await page.locator("table[data-table=devices] tbody tr").first().waitFor();
      await page.locator(".pwr").first().waitFor({ state: "attached", timeout: 20_000 });
      await page.evaluate(() => document.fonts.ready);
      // the default layout: a clean saved layout, scrolled to the start
      await page.evaluate(() => {
        const s = document.querySelector(".tablewrap[data-key=devices] .tablescroll");
        if (s) s.scrollLeft = 0;
      });
      await expect.poll(() => overlaps(page), { timeout: 5_000, message: "pinned columns cover or are overrun by other cells" }).toEqual([]);
    });
  }
}
