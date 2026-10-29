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
    const t = document.querySelector<HTMLTableElement>(
      "table[data-table=devices]",
    )!;
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
          if (
            n.classList.contains("colhandle") ||
            n.classList.contains("sr-only") ||
            n.closest("details:not([open]) > :not(summary)")
          )
            return;
          if (!n.childNodes.length || n.matches("svg,input,button,label,.sw"))
            add(n.getBoundingClientRect());
          n.childNodes.forEach(walk);
        }
      };
      c.childNodes.forEach(walk);
      return isFinite(l) ? new DOMRect(l, top, r - l, b - top) : new DOMRect();
    };
    const hit = (a: DOMRect, b: DOMRect) =>
      a.width > 0 && a.left < b.right - 0.5 && b.left < a.right - 0.5;
    const out: string[] = [];
    for (const end of ["l", "r"]) {
      scroller.scrollLeft = end === "l" ? 0 : scroller.scrollWidth;
      for (const row of t.querySelectorAll<HTMLTableRowElement>(
        "thead tr, tbody tr",
      )) {
        const cells = [...row.cells] as HTMLElement[];
        if (cells.length < side.size) continue;
        cells.forEach((c, i) => {
          if (hidden(c)) return;
          const k = content(c);
          if (side.has(i)) {
            // inside the cell's padding box: a switch flush on the border looks like it spills over
            const own = c.getBoundingClientRect(),
              cs = getComputedStyle(c),
              inL = own.left + parseFloat(cs.paddingLeft),
              inR = own.right - parseFloat(cs.paddingRight);
            if (
              end === "l" &&
              k.width > 0 &&
              (k.left < inL - 0.5 || k.right > inR + 0.5)
            )
              out.push(
                `pinned ${c.dataset.label ?? i} shows ${Math.round(k.left)}-${Math.round(k.right)} in a cell of ${Math.round(own.left)}-${Math.round(own.right)} with padding ${cs.paddingLeft}/${cs.paddingRight}`,
              );
            return;
          }
          cells.forEach((p, j) => {
            if (side.get(j) !== end || hidden(p)) return;
            const b = p.getBoundingClientRect();
            if (hit(k, b))
              out.push(
                `${row.parentElement!.tagName.toLowerCase()} ${c.dataset.label ?? c.textContent!.trim()} ${Math.round(k.left)}-${Math.round(k.right)} under pinned ${p.dataset.label ?? j} ${Math.round(b.left)}-${Math.round(b.right)}`,
              );
          });
        });
      }
    }
    // What the user sees first (scroll 0): the right pins lie over the columns that have not scrolled in
    // yet. A column may be wholly under them (the table scrolls, the rest is reachable) but never cut
    // in half: "seen 1" and a toggle beside half an Uptime value read as an overlap.
    scroller.scrollLeft = 0;
    const frame = scroller.getBoundingClientRect();
    const scrolls = scroller.scrollWidth > scroller.clientWidth + 1;
    for (const row of t.querySelectorAll<HTMLTableRowElement>(
      "thead tr, tbody tr",
    )) {
      const cells = [...row.cells] as HTMLElement[];
      if (cells.length < side.size) continue;
      // the pinned-right group begins at its leftmost cell and reaches the frame's edge
      const pins = cells.filter((_, j) => side.get(j) === "r");
      if (!pins.length) continue;
      const from = Math.min(...pins.map((p) => p.getBoundingClientRect().left));
      cells.forEach((c, i) => {
        if (side.has(i) || hidden(c)) return;
        const own = c.getBoundingClientRect();
        // only what lies inside the frame is seen at all
        const seen =
          Math.min(own.right, frame.right) - Math.max(own.left, frame.left);
        const under =
          Math.min(own.right, frame.right) - Math.max(own.left, from);
        if (seen <= 0.5 || under <= 0.5) return;
        const what = `${row.parentElement!.tagName.toLowerCase()} ${c.dataset.label ?? i} (${Math.round(own.left)}-${Math.round(own.right)}) under the pinned columns from ${Math.round(from)}`;
        if (!scrolls) out.push(`${what}, although the table does not scroll`);
        else if (under < seen - 0.5) out.push(`${what}: cut in half`);
      });
    }
    // every row of a pinned column has the same inner padding (a two-relay row's switches sat flush on the border)
    for (const [idx] of side) {
      const pads = new Set(
        [...t.tBodies[0].rows]
          .filter(
            (r) =>
              r.cells.length > idx && !(r.cells[idx] as HTMLElement).hidden,
          )
          .map((r) => {
            const cs = getComputedStyle(r.cells[idx]);
            return `${cs.paddingLeft}/${cs.paddingRight}`;
          }),
      );
      if (pads.size > 1)
        out.push(
          `pinned column ${idx} has different paddings in its rows: ${[...pads].join(" vs ")}`,
        );
    }
    // a column scrolled under the pinned ones must not show anything through them: the point at the
    // middle of each pinned header lies on that header, not on a resize handle of another column
    for (const [idx] of side) {
      const th = t.tHead!.rows[0].cells[idx];
      if (!th || hidden(th)) continue;
      const b = th.getBoundingClientRect();
      for (const y of [b.top + b.height * 0.3, b.top + b.height * 0.7]) {
        for (const x of [b.left + b.width - 4, b.left + 4]) {
          const at = document.elementFromPoint(x, y);
          if (at && !th.contains(at))
            out.push(
              `over the pinned header ${idx} at ${Math.round(x)},${Math.round(y)} lies ${at.tagName.toLowerCase()}.${String(at.className)}`,
            );
        }
      }
    }
    // columns pinned for good have no resize handle (a stray divider inside the last cell of the header)
    for (const h of t.querySelectorAll<HTMLElement>("thead th > .colhandle")) {
      const idx = (h.parentElement as HTMLTableCellElement).cellIndex;
      if (side.get(idx) === "r")
        out.push(`resize handle in the header of pinned column ${idx}`);
    }
    return [...new Set(out)];
  });
}

for (const theme of ["light", "dark"]) {
  for (const width of [1280, 1440]) {
    test(`default layout, ${theme}, ${width} px: pinned columns show all their content and nothing lies under them`, async ({
      page,
    }) => {
      await page.setViewportSize({ width, height: 900 });
      await page.setExtraHTTPHeaders(designAdmin);
      await page.goto(stack.app.url + "/devices");
      await page.evaluate(
        (t) => document.documentElement.setAttribute("data-theme", t),
        theme,
      );
      await page
        .locator("table[data-table=devices] tbody tr")
        .first()
        .waitFor();
      await page
        .locator(".pwr")
        .first()
        .waitFor({ state: "attached", timeout: 20_000 });
      await page.evaluate(() => document.fonts.ready);
      // the default layout: a clean saved layout, scrolled to the start
      await page.evaluate(() => {
        const s = document.querySelector(
          ".tablewrap[data-key=devices] .tablescroll",
        );
        if (s) s.scrollLeft = 0;
      });
      await expect
        .poll(() => overlaps(page), {
          timeout: 5_000,
          message: "pinned columns cover or are overrun by other cells",
        })
        .toEqual([]);
    });
  }
}
