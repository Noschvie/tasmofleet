import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { hoverTip, withKnownIssues } from "../support/layout";
import { openSavedLayoutList, ownerWindow } from "../support/saved-layout";
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
  return page.evaluate(async () => {
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
      // what a cut-off value (overflow hidden, an ellipsis) does not show is not under anything: its
      // text range runs on past the box that clips it
      const clipped = (q: DOMRect, from: Element | null): DOMRect => {
        let l = q.left,
          r = q.right;
        for (let e = from; e && e !== c; e = e.parentElement) {
          if (getComputedStyle(e).overflowX === "visible") continue;
          const b = e.getBoundingClientRect();
          l = Math.max(l, b.left);
          r = Math.min(r, b.right);
        }
        return new DOMRect(l, q.top, Math.max(0, r - l), q.height);
      };
      const add = (q: DOMRect, from: Element | null) => {
        q = clipped(q, from);
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
          add(g.getBoundingClientRect(), n.parentElement);
        } else if (n instanceof HTMLElement) {
          if (
            n.classList.contains("colhandle") ||
            n.classList.contains("sr-only") ||
            n.closest("details:not([open]) > :not(summary)")
          )
            return;
          if (!n.childNodes.length || n.matches("svg,input,button,label,.sw"))
            add(n.getBoundingClientRect(), n);
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
    // yet. A column may be wholly under them (the table scrolls, the rest is reachable), or cut at their
    // edge when that edge shows it scrolls on (below); never cut in half by a bare edge: "seen 1" and a
    // toggle beside half an Uptime value read as an overlap.
    scroller.scrollLeft = 0;
    // the page marks which edges have columns under them on the scroll event, which comes before the next frame
    await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
    const frame = scroller.getBoundingClientRect();
    // #790: the pinned columns are the ends of the scroll area. A column that runs on under them is cut at
    // their edge as a column is cut at the edge of any box that scrolls, and that edge is drawn: a shade on
    // the scrolling side (as on the cut edge of a dialog or a log box). With it a column cut there reads as
    // one that scrolls on; without it, text that runs flush into the pin reads as covered, half a column.
    const shaded = (pin: HTMLElement) => {
      const b = getComputedStyle(pin, "::before");
      return (
        getComputedStyle(pin).overflowX === "visible" &&
        b.content !== "none" &&
        b.position === "absolute" &&
        /gradient/.test(b.backgroundImage) &&
        parseFloat(b.width) >= 4 &&
        Math.abs(parseFloat(b.right) - pin.clientWidth) < 1.5 // it lies left of the pin, over the scrolling cells
      );
    };
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
      const edge = pins.find((p) => p.getBoundingClientRect().left === from)!;
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
        else if (under < seen - 0.5 && !shaded(edge))
          out.push(`${what}: cut in half, and the pins' edge does not show that it scrolls on`);
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

// #773: the default layout is not the only one people see. Showing the optional columns (OTA URL, Group
// topic) saves a layout (class tfix) that is wider than the page. A saved layout is judged by the same
// rule as the default one: what lies under a pinned column is only ever a column that has not
// scrolled in yet, and at either end of the scroll range nothing lies under a pin.
//
// A column cut in half at scroll 0 is a finding here too (#790), unless the pins' edge shows that it scrolls
// on: the scroll area of the unpinned columns ends at the pins. A saved layout is not cut to the pins (its
// widths are the person's, and a fit that changed one fought every drag of it, see the next test), so at
// most window widths one column runs on under the pins, and that edge has to say so.
for (const width of [1280, 1440]) {
  test(`a saved layout wider than the page, ${width} px: no column lies under a pinned column at either end of the scroll`, async ({
    page,
  }) => {
    await page.setViewportSize({ width, height: 900 });
    await page.setExtraHTTPHeaders(designAdmin);
    await page.goto(stack.app.url + "/devices");
    await page.locator("table[data-table=devices] tbody tr").first().waitFor();
    await page.locator(".pwr").first().waitFor({ state: "attached", timeout: 20_000 });
    const table = page.locator("table[data-table=devices]");
    const menu = page.locator("details.colmenu").first();
    const saved = () =>
      page.waitForResponse(
        (r) =>
          r.url().endsWith("/prefs/table.devices") &&
          r.request().method() === "POST" &&
          r.status() === 204,
      );
    try {
      await menu.locator(":scope > summary").click();
      for (const name of ["OTA URL", "Group topic"]) {
        const done = saved();
        await menu.getByRole("checkbox", { name }).check();
        await done;
      }
      // the layout as the server sends it on the next visit, not as the first click built it
      await page.goto(stack.app.url + "/devices");
      await table.locator("tbody tr").first().waitFor();
      await page.locator(".pwr").first().waitFor({ state: "attached", timeout: 20_000 });
      await page.evaluate(() => document.fonts.ready);
      await expect(table).toHaveClass(/tfix/);
      for (const name of ["OTA URL", "Group topic"])
        await expect(table.locator("thead th", { hasText: name })).toBeVisible();
      const wider = await page.evaluate(() => {
        const s = document.querySelector<HTMLElement>(".tablewrap[data-key=devices] .tablescroll")!;
        return s.scrollWidth > s.clientWidth + 1;
      });
      expect(wider, "the saved layout has to be wider than the page for this case to mean anything").toBe(true);
      await expect
        .poll(() => overlaps(page), {
          timeout: 5_000,
          message: "a column of the saved layout lies under a pinned column",
        })
        .toEqual([]);
    } finally {
      // leave the shared fleet's layout as it was found
      await page.goto(stack.app.url + "/devices");
      await page.locator("details.colmenu > summary").first().click();
      await page.locator("details.colmenu [data-cols-reset]").first().click();
      await page.waitForLoadState("load");
    }
  });
}

// the saved layout of the case above (OTA URL and Group topic shown), as the server sends it on the next visit;
// the shared fleet's layout is reset afterwards
async function withSavedLayout(
  page: Page,
  width: number,
  body: () => Promise<void>,
): Promise<void> {
  await page.setViewportSize({ width, height: 900 });
  await page.setExtraHTTPHeaders(designAdmin);
  await page.goto(stack.app.url + "/devices");
  await page.locator("table[data-table=devices] tbody tr").first().waitFor();
  await page.locator(".pwr").first().waitFor({ state: "attached", timeout: 20_000 });
  const menu = page.locator("details.colmenu").first();
  try {
    await menu.locator(":scope > summary").click();
    for (const name of ["OTA URL", "Group topic"]) {
      const done = page.waitForResponse(
        (r) =>
          r.url().endsWith("/prefs/table.devices") &&
          r.request().method() === "POST" &&
          r.status() === 204,
      );
      await menu.getByRole("checkbox", { name }).check();
      await done;
    }
    await page.goto(stack.app.url + "/devices");
    await page.locator("table[data-table=devices] tbody tr").first().waitFor();
    await page.locator(".pwr").first().waitFor({ state: "attached", timeout: 20_000 });
    await page.evaluate(() => document.fonts.ready);
    await expect(page.locator("table[data-table=devices]")).toHaveClass(/tfix/);
    await body();
  } finally {
    await page.goto(stack.app.url + "/devices");
    await page.locator("details.colmenu > summary").first().click();
    await page.locator("details.colmenu [data-cols-reset]").first().click();
    await page.waitForLoadState("load");
  }
}

// #821: the owner's window. A layout saved by a person, wider than the frame at 1290 px, is not a layout the
// menu builds in these tests (their windows are 1280 and 1440 px): the shared fixture saves it as a person's
// layout and the same rule is checked on it.
test(`a saved layout wider than the frame, ${ownerWindow} px (fixture): no column lies under a pinned column at either end of the scroll`, async ({ page }) => {
  await openSavedLayoutList(page, stack.app.url, { width: ownerWindow });
  await page.locator(".pwr").first().waitFor({ state: "attached", timeout: 20_000 });
  await expect(page.locator("table[data-table=devices]")).toHaveClass(/tfix/);
  await expect.poll(() => overlaps(page), { timeout: 5_000, message: "a column of the saved layout lies under a pinned column" }).toEqual([]);
});

// #790: in a saved layout that scrolls, the last column seen whole before the right pins is the one a person
// most likely widens or narrows to see more. It must follow the pointer and the arrow keys like any other
// column. An earlier fix for the column under the pins widened this one back to the pins' edge on every
// step of a drag: the column stayed where it was while the pointer moved 40 px, and jumped on release.
for (const width of [1280, 1440]) {
  test(`a saved layout that scrolls: the last whole column before the pins follows a drag and a key step, ${width} px`, async ({
    page,
  }) => {
    await withSavedLayout(page, width, async () => {
      const at = await page.evaluate(() => {
        const t = document.querySelector<HTMLTableElement>("table[data-table=devices]")!;
        const s = t.closest<HTMLElement>(".tablescroll")!;
        s.scrollLeft = 0;
        const side = new Map<number, string>();
        for (const c of t.classList) {
          const m = /^p([lr])(\d+)$/.exec(c);
          if (m) side.set(parseInt(m[2], 10) - 1, m[1]);
        }
        const th = [...t.tHead!.rows[0].cells] as HTMLElement[];
        const edge = Math.min(
          ...th.filter((_, j) => side.get(j) === "r").map((p) => p.getBoundingClientRect().left),
        );
        let i = -1;
        th.forEach((h, j) => {
          if (side.has(j) || h.offsetParent === null) return;
          if (h.getBoundingClientRect().right <= edge + 0.5) i = j;
        });
        return { i, label: th[i]?.textContent!.trim() ?? "", scrolls: s.scrollWidth > s.clientWidth + 1 };
      });
      expect(at.scrolls, "the saved layout has to scroll for this case to mean anything").toBe(true);
      expect(at.i, "no scrolling column is seen whole at scroll 0").toBeGreaterThan(1);
      // Measured on the table the page shows, in one step: a locator resolves the cell in one call and measures
      // it in the next, and a live reload of the list in between left it measuring a removed cell (0 px).
      const shown = () =>
        page.evaluate((i) => {
          const cell = document.querySelector<HTMLTableElement>("table[data-table=devices]")!.tHead!.rows[0].cells[i];
          return {
            w: cell.getBoundingClientRect().width,
            now: Number(cell.querySelector(".colhandle")?.getAttribute("aria-valuenow")),
          };
        }, at.i);
      const w = async () => (await shown()).w;
      const handle = page.getByRole("separator", { name: `Resize column ${at.label}` });
      const w0 = await w();
      // the middle of the handle, read in one step for the same reason
      const { x, y } = await page.evaluate((i) => {
        const cell = document.querySelector<HTMLTableElement>("table[data-table=devices]")!.tHead!.rows[0].cells[i];
        const b = cell.querySelector(".colhandle")!.getBoundingClientRect();
        return { x: b.x + b.width / 2, y: b.y + b.height / 2 };
      }, at.i);
      await page.mouse.move(x, y);
      await page.mouse.down();
      for (const k of [1, 2, 3, 4]) await page.mouse.move(x - 10 * k, y);
      // While the pointer is held. Every step below is laid out in the event that makes it (pointermove,
      // pointerup, keydown), so each is read once, with no waiting: a poll's first read could take longer
      // than its timeout on a loaded machine.
      const moved = (await w()) - w0;
      expect(moved, `${at.label} while the pointer is held 40 px left of where it took the handle`).toBeLessThanOrEqual(-38);
      expect(moved, `${at.label} narrowed by more than the pointer moved`).toBeGreaterThanOrEqual(-42);
      // A device event reloads the whole list (#devtable) about once a second in a busy fleet, so a reload
      // lands while a person holds a handle. Until #806 the new table came with the saved widths and the
      // column jumped back to them until the pointer moved again (this test failed whenever a reload landed
      // in the second after the last move). The reload is forced here, so that case is always checked.
      // The settle that ends the wait is the one of a swap seen here (the same request): a device event's
      // reload swapped in just before this script ran settles 20 ms later, after the table was read as
      // "before", and ending on that settle measured no reload at all (#819).
      const swap = await page.evaluate(
        (i) =>
          new Promise<{ replaced: boolean; swapped: number; settled: number }>((done) => {
            const width = () => {
              const t = document.querySelector<HTMLTableElement>("table[data-table=devices]")!;
              return t.tHead!.rows[0].cells[i].getBoundingClientRect().width;
            };
            const before = document.querySelector("table[data-table=devices]");
            let swapped = -1;
            let request: unknown = null;
            const onSwap = (e: Event) => {
              if ((e.target as Element).id !== "devtable") return;
              request = (e as CustomEvent<{ xhr: XMLHttpRequest }>).detail.xhr;
              swapped = width(); // the first frame drawn after the swap
            };
            const onSettle = (e: Event) => {
              if ((e.target as Element).id !== "devtable") return;
              if (request === null || (e as CustomEvent<{ xhr: XMLHttpRequest }>).detail.xhr !== request) return;
              document.removeEventListener("htmx:afterSwap", onSwap);
              document.removeEventListener("htmx:afterSettle", onSettle);
              done({ replaced: document.querySelector("table[data-table=devices]") !== before, swapped, settled: width() });
            };
            document.addEventListener("htmx:afterSwap", onSwap);
            document.addEventListener("htmx:afterSettle", onSettle);
            (window as unknown as { htmx: { trigger(s: string, n: string): void } }).htmx.trigger("#devtable", "sse:resync");
          }),
        at.i,
      );
      expect(swap.replaced, "the reload replaced the table").toBe(true);
      for (const [when, got] of [
        ["right after", swap.swapped],
        ["once settled after", swap.settled],
      ] as const) {
        expect(got - w0, `${at.label} ${when} a reload of the list while the pointer is held`).toBeLessThanOrEqual(-38);
        expect(got - w0, `${at.label} ${when} a reload of the list while the pointer is held`).toBeGreaterThanOrEqual(-42);
      }
      const held = await w();
      const saved = page.waitForResponse(
        (r) => r.url().endsWith("/prefs/table.devices") && r.request().method() === "POST",
      );
      await page.mouse.up();
      await saved;
      expect(
        Math.abs((await w()) - held),
        `${at.label} on release against its width while held (${held.toFixed(1)} px)`,
      ).toBeLessThanOrEqual(1);
      // one step of the arrow keys: 10 px, and the handle announces the width the column has
      await handle.focus();
      const stepped = page.waitForResponse(
        (r) => r.url().endsWith("/prefs/table.devices") && r.request().method() === "POST",
      );
      await page.keyboard.press("ArrowLeft");
      await stepped;
      const now = await shown();
      expect(now.w - held, `${at.label} after one ArrowLeft`).toBeLessThanOrEqual(-8);
      expect(now.w - held, `${at.label} narrowed by more than one step`).toBeGreaterThanOrEqual(-12);
      expect(
        Math.abs(now.now - now.w),
        `aria-valuenow against the width ${at.label} shows (${now.w.toFixed(1)} px)`,
      ).toBeLessThanOrEqual(1);
    });
  });
}

// #811: the shared tooltip showed a bubble below the hovered cell, and a cell fills its whole row, so the
// bubble lay over the next row's text. The rule holds for every table, so it is checked on the Devices
// list too: with the pointer on any titled element of the first rows, in the scrolling cells and in
// the pinned columns (status pills, relay switches, row actions), the bubble covers no other row's text.
/** titledBubbles puts the pointer on every titled element of rows 1 and 2 of the open Devices list and returns the bubbles that cover another row's text. */
async function titledBubbles(page: Page): Promise<{ hovered: number; bad: string[] }> {
  const rows = page.locator("table[data-table=devices] tbody tr");
  await rows.first().waitFor();
  await page.locator(".pwr").first().waitFor({ state: "attached", timeout: 20_000 });
  await page.evaluate(() => document.fonts.ready);
  const bad: string[] = [];
  let hovered = 0;
  for (const row of [1, 2]) {
    const titled = rows.nth(row).locator("[title], [data-tip]");
    for (let i = 0, n = await titled.count(); i < n; i++) {
      const el = titled.nth(i);
      if (!(await el.isVisible())) continue;
      const what = await el.evaluate((e) => `${e.tagName.toLowerCase()}.${String(e.className).trim().split(" ")[0]} in ${(e.closest("td") as HTMLElement | null)?.dataset.label ?? "?"}`);
      // on a layout that scrolls, an element can sit under a pinned column or outside the frame, where a pointer
      // cannot reach it: it shows no bubble, so it is skipped (and listed), not waited for
      const m = await hoverTip(page, el, "table[data-table=devices] tbody tr", undefined, 10_000).catch((e) => (/Timeout/.test(String(e)) ? null : Promise.reject(e)));
      if (!m) {
        test.info().annotations.push({ type: "hover skipped", description: `row ${row} ${what}: could not be pointed at within 10 s` });
        continue;
      }
      hovered++;
      if (m.covers.length) bad.push(`row ${row} ${what}: bubble "${m.tip?.text.slice(0, 40)}" ${Math.round(m.tip!.top)}-${Math.round(m.tip!.bottom)} covers ${m.covers.join(", ")}`);
    }
  }
  return { hovered, bad };
}

test(`Devices list: the bubble of a titled cell or pinned control covers no other row's text`, async ({ page }) => {
  test.setTimeout(400_000); // about 30 hovers, each waits for its bubble
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.setExtraHTTPHeaders(designAdmin);
  await page.goto(stack.app.url + "/devices");
  const { hovered, bad } = await titledBubbles(page);
  expect(hovered, "the rows carry titled elements").toBeGreaterThan(6);
  expect(bad).toEqual([]);
});

// #821: the same on the saved layout of the owner's window, where the columns beyond the frame scroll in and
// the pinned ones keep their places.
test(`Devices list, a saved layout at ${ownerWindow} px (fixture): the bubble of a titled cell or pinned control covers no other row's text`, async ({ page }) => {
  test.setTimeout(400_000);
  await openSavedLayoutList(page, stack.app.url, { width: ownerWindow });
  const { hovered, bad } = await titledBubbles(page);
  expect(hovered, "the rows carry titled elements").toBeGreaterThan(6);
  // red on main: the bubble of the two-line OTA URL covers the row above (knownLayoutIssues names the issue)
  const red = withKnownIssues(`devices list saved layout ${ownerWindow}px bubbles`, ["r"], bad.length ? [{ guard: "r", bad }] : []);
  expect(red.flatMap((r) => r.bad)).toEqual([]);
});
