import type { Locator, Page } from "@playwright/test";
import { expect, test } from "@playwright/test";

/**
 * expectNoOverflow fails when anything is wider than the visible page, naming
 * the widest culprits. Zero tolerance, measured the way a user sees it:
 *
 * - against documentElement.clientWidth, the width WITHOUT the scrollbar.
 *   `scrollWidth - innerWidth` (what this check used before) is short by the
 *   scrollbar width on pages with a visible scrollbar, so 9 to 15 px
 *   of real overflow passed unnoticed;
 * - after document.fonts.ready, so text is measured in its final font;
 * - every element, plus the navigation row's own scroll width (a row that
 *   scrolls inside itself is wider than the screen even if the page is not).
 */
export async function expectNoOverflow(page: Page, name: string): Promise<void> {
  const problem = await overflowProblem(page, name);
  expect(problem, problem ?? "").toBeNull();
}

/** overflowProblem is expectNoOverflow without the assertion: a description of the overflow, or null. */
export async function overflowProblem(page: Page, name: string): Promise<string | null> {
  await page.evaluate(() => document.fonts.ready);
  const { overflow, culprits } = await page.evaluate(() => {
    const w = document.documentElement.clientWidth;
    const eps = 0.01; // sub-pixel rounding of a box that ends exactly at the edge
    const wider = (e: Element) => e.getBoundingClientRect().right > w + eps;
    const bad = [...document.querySelectorAll("body *")]
      // closed dialogs, [hidden] and the folded part of a closed <details> are not on screen
      .filter((e) => !e.closest("dialog:not([open])") && !e.closest("[hidden]") && !e.closest("details:not([open]) > :not(summary)") && wider(e))
      .filter((e) => ![...e.children].some(wider)) // innermost only
      .sort((a, b) => b.getBoundingClientRect().right - a.getBoundingClientRect().right)
      .slice(0, 5)
      .map((e) => {
        const label = (e.closest("td") as HTMLElement | null)?.dataset.label;
        return `${e.tagName.toLowerCase()}${e.className ? "." + String(e.className).split(" ").join(".") : ""}${label ? ` in td[${label}]` : ""} right=${Math.round(e.getBoundingClientRect().right)} (visible width ${w})`;
      });
    const nav = document.querySelector("nav.mainnav");
    // only a row that really scrolls (overflow auto or scroll) counts: an open menu hanging out of the bar is not overflow
    const navScrolls = nav ? ["auto", "scroll"].includes(getComputedStyle(nav).overflowX) : false;
    const navOver = nav && navScrolls ? nav.scrollWidth - nav.clientWidth : 0;
    if (navOver > 0) bad.push(`nav.mainnav scrolls inside itself by ${navOver}px`);
    return { overflow: Math.max(document.documentElement.scrollWidth - w, navOver), culprits: bad };
  });
  return overflow > 0 ? `${name} is wider than the visible page by ${overflow}px; widest elements: ${culprits.join("; ")}` : null;
}

/**
 * smallTargets lists the touch targets of the page smaller than min px in either
 * direction: buttons, summaries, links in rows and menus, checkbox hit areas,
 * form fields. Prose links (inside a paragraph, list item or note) are exempt
 * (WCAG 2.5.8 "inline" exception); hidden, closed-fold and screen-reader-only
 * elements are not on screen.
 */
export async function smallTargets(page: Page, min = 44): Promise<string[]> {
  return page.evaluate((min) => {
    const out: string[] = [];
    const visible = (e: Element) => {
      const r = e.getBoundingClientRect();
      if (r.width === 0 || r.height === 0) return false;
      if (e.closest("dialog:not([open]), [hidden], .sr-only, details:not([open]) > :not(summary), thead, .tip")) return false;
      return getComputedStyle(e).visibility !== "hidden";
    };
    const describe = (e: Element, r: DOMRect) =>
      `${e.tagName.toLowerCase()}${e.className ? "." + String(e.className).trim().split(/\s+/).join(".") : ""} "${(e.textContent || (e as HTMLInputElement).ariaLabel || "").trim().slice(0, 24)}" ${Math.round(r.width)}x${Math.round(r.height)}`;
    // A control drawn smaller than its target (the prototype's 32 px buttons) gets its hit area from
    // an absolutely positioned ::after that reaches past its box; clicks on it go to the control.
    const hitBox = (e: Element): DOMRect => {
      const r = e.getBoundingClientRect();
      const a = getComputedStyle(e, "::after");
      if (a.content === "none" || a.position !== "absolute" || getComputedStyle(e).position === "static") return r;
      const px = (v: string) => (v.endsWith("px") ? Math.min(0, parseFloat(v)) : 0);
      const [t, rt, b, l] = [px(a.top), px(a.right), px(a.bottom), px(a.left)];
      return new DOMRect(r.x + l, r.y + t, r.width - l - rt, r.height - t - b);
    };
    const check = (e: Element, target: Element = e) => {
      if (!visible(e)) return;
      const r = hitBox(target);
      if (r.height < min - 0.5 || r.width < min - 0.5) out.push(describe(target, r));
    };
    // the way back and links beside a button act like buttons: they are targets, not prose
    const prose = (e: Element) => !!e.closest("p, li, small, dd, .notice, .lead, .crumb, .tag, .chip, h1, h2, h3, summary, label") && !e.matches(".button, .menu a, td a, .brand, .crumb a, .actions > a");
    for (const e of document.querySelectorAll("button, .button")) check(e);
    // a fold title can get its hit area from a pseudo-element, which getBoundingClientRect does not see
    for (const e of document.querySelectorAll("summary:not(.card > details.fold-h > summary)")) check(e);
    // a device name in a list is one link of a wrapping run of 24px targets, 8px apart (WCAG 2.5.8 spacing exception, QA #84)
    for (const e of document.querySelectorAll("a")) if (!e.matches(".button") && !prose(e) && !e.closest(".pager-gap")) { const r = e.getBoundingClientRect(); if (visible(e) && hitBox(e).height < (e.closest(".dev") ? 24 : min) - 0.5) out.push(describe(e, r)); }
    for (const e of document.querySelectorAll("input[type=checkbox], input[type=radio]")) check(e, e.closest("label") ?? e);
    for (const e of document.querySelectorAll("select, input:not([type=hidden]):not([type=checkbox]):not([type=radio]):not([type=file])")) {
      if (visible(e) && e.getBoundingClientRect().height < min - 0.5) out.push(describe(e, e.getBoundingClientRect()));
    }
    return [...new Set(out)];
  }, min);
}

// ---------------------------------------------------------------------------
// Generic layout guards (visual QA of v0.8.x, the visual QA).
//
// Each guard walks the page the way a person sees it and returns offenders: tag,
// classes, trimmed text, the boxes involved and the nearest table cell or card.
// An empty list is a pass. Elements that are not on screen (closed dialog,
// [hidden], the folded part of a closed <details>, .sr-only, visibility hidden,
// zero size, the off-screen thead of a phone card table) are skipped. Tolerance
// is 0.5 px unless a guard says otherwise.
// ---------------------------------------------------------------------------

export type GuardId = "a" | "b" | "c" | "d" | "e" | "f" | "g" | "h" | "w" | "o" | "i";

export interface GuardResult {
  guard: GuardId;
  what: string;
  offenders: string[];
}

export const allGuards: GuardId[] = ["a", "b", "c", "d", "e", "f", "g", "h", "w", "o", "i"];

const guardNames: Record<GuardId, string> = {
  a: "controls and badges stay inside their row, cell and card",
  b: "no two visible items of one row overlap",
  c: "labels and row controls are vertically centred",
  d: "a collapsed section shows no empty body or frame",
  e: "buttons that sit together have one height",
  f: "card content starts below the legend title",
  g: "open overlays have a border and a shadow",
  h: "an open menu floats: its container keeps its height and nothing clips it",
  w: "no word that fits on a line is broken across two lines",
  o: "text stays inside its own box and never overlaps other text",
  i: "table cell content is not cut off (v0.10.1: columns never narrower than their content)",
};

/**
 * layoutGuards runs the guards on the page as it is now and returns the ones with
 * offenders. Guard g looks at whatever overlay is open at that moment: open the
 * menu, dialog or tooltip first (see openOverlays).
 */
export async function layoutGuards(page: Page, which: GuardId[] = allGuards): Promise<GuardResult[]> {
  await page.evaluate(() => document.fonts.ready);
  const found = await page.evaluate((which) => {
    const out: Record<string, string[]> = {};
    const TOL = 0.5;
    const add = (g: string, msg: string) => void (out[g] ??= []).push(msg);

    // ---- helpers ----
    const rectOf = (e: Element) => e.getBoundingClientRect();
    const OVERLAY = ".rowmenu-body, details.navgroup > .um-menu, .dr, .colpanel, .tooltip, .tip";
    const visible = (e: Element) => {
      const r = rectOf(e);
      if (r.width === 0 || r.height === 0) return false;
      // checkVisibility is false for display:none, content-visibility:hidden (the folded part of a closed
      // <details> on a phone, which still reports stale boxes) and visibility:hidden. On wide screens a
      // closed fold is shown open (::details-content content-visibility: visible), and then it counts.
      if (!e.checkVisibility({ visibilityProperty: true })) return false;
      return !e.closest("dialog:not([open]), [hidden], .sr-only, thead");
    };
    const cls = (e: Element) => (typeof e.className === "string" && e.className.trim() ? "." + e.className.trim().split(/\s+/).join(".") : "");
    const box = (r: DOMRect) => `[${Math.round(r.left)},${Math.round(r.top)} ${Math.round(r.width)}x${Math.round(r.height)}]`;
    const textOf = (e: Element) => ((e as HTMLElement).innerText ?? e.textContent ?? "").replace(/\s+/g, " ").trim().slice(0, 28);
    const where = (e: Element) => {
      const td = e.closest("td[data-label]") as HTMLElement | null;
      if (td) return ` in td[${td.dataset.label}]`;
      const sec = e.closest("section[aria-labelledby]");
      return sec ? ` in section#${sec.getAttribute("aria-labelledby")}` : "";
    };
    const describe = (e: Element, extra = "") => `${e.tagName.toLowerCase()}${cls(e)} "${textOf(e)}" ${box(rectOf(e))}${where(e)}${extra ? " " + extra : ""}`;
    const inOverlay = (e: Element) => !!e.closest(OVERLAY);
    const within = (inner: DOMRect, outer: DOMRect) =>
      inner.left >= outer.left - TOL && inner.right <= outer.right + TOL && inner.top >= outer.top - TOL && inner.bottom <= outer.bottom + TOL;
    const intersect = (a: DOMRect, b: DOMRect): DOMRect | null => {
      const l = Math.max(a.left, b.left), t = Math.max(a.top, b.top), r = Math.min(a.right, b.right), bt = Math.min(a.bottom, b.bottom);
      return r > l && bt > t ? new DOMRect(l, t, r - l, bt - t) : null;
    };
    // the box of an element's own text (all its visible text nodes), or null
    const textRect = (e: Element): DOMRect | null => {
      const w = document.createTreeWalker(e, NodeFilter.SHOW_TEXT);
      let l = Infinity, t = Infinity, r = -Infinity, b = -Infinity;
      for (let n = w.nextNode(); n; n = w.nextNode()) {
        if (!n.textContent || !n.textContent.trim() || (n.parentElement as Element).closest(".sr-only")) continue;
        const rg = document.createRange();
        rg.selectNodeContents(n);
        for (const q of rg.getClientRects()) {
          if (q.width === 0 || q.height === 0) continue;
          l = Math.min(l, q.left); t = Math.min(t, q.top); r = Math.max(r, q.right); b = Math.max(b, q.bottom);
        }
      }
      return l === Infinity ? null : new DOMRect(l, t, r - l, b - t);
    };

    // scrolled out of sight inside a box that clips or scrolls its content (a chip list with a scrollbar): not on screen
    const scrolledAway = (e: Element) => {
      const r = rectOf(e);
      for (let p = e.parentElement; p; p = p.parentElement) {
        const cs = getComputedStyle(p);
        if ((cs.overflowX !== "visible" || cs.overflowY !== "visible") && p !== document.documentElement && p !== document.body && !intersect(r, rectOf(p))) return true;
      }
      return false;
    };
    const CONTROLS = "button, a.button, summary.button, label.button, .badge, input[type=checkbox], input[type=radio]";
    const controls = [...document.querySelectorAll(CONTROLS)].filter((e) => visible(e) && !scrolledAway(e));

    // ---- (a) controls and badges inside their row, cell and card ----
    if (which.includes("a")) {
      for (const e of controls) {
        if (inOverlay(e)) continue;
        const r = rectOf(e);
        const tr = e.closest("tr");
        if (tr && visible(tr)) {
          const td = e.closest("td, th");
          if (td && getComputedStyle(td).display !== "contents" && rectOf(td).width > 0 && !within(r, rectOf(td))) add("a", describe(e, `outside its cell ${box(rectOf(td))}`));
          else if (!within(r, rectOf(tr))) add("a", describe(e, `outside its row ${box(rectOf(tr))}`));
          const tbl = e.closest("table");
          if (tbl && !within(r, rectOf(tbl))) add("a", describe(e, `outside its table ${box(rectOf(tbl))}`));
        }
        const card = e.closest(".card");
        if (card && !e.closest(".actionbar, dialog") && !within(r, rectOf(card))) add("a", describe(e, `outside its card ${box(rectOf(card))}`));
      }
    }

    // the text of a tag, badge or count sits inside its own box: an inherited negative text-indent
    // (the console history is a <pre> with a hanging indent) once pushed "Proxy" out of its tag
    // onto the name before it ("adaProxy")
    if (which.includes("a")) {
      for (const e of document.querySelectorAll(".tag, .badge, .count")) {
        if (!visible(e) || inOverlay(e) || scrolledAway(e)) continue;
        const t = textRect(e);
        if (t && (t.left < rectOf(e).left - TOL || t.right > rectOf(e).right + TOL)) add("a", describe(e, `text ${box(t)} leaves its own box`));
      }
    }

    // fixed and sticky bars (the bulk bar, the top bar) lie fully inside the viewport: a bar that is
    // clipped at the right or bottom edge hides its last button
    if (which.includes("a")) {
      const w = document.documentElement.clientWidth;
      for (const e of document.querySelectorAll("body *")) {
        const pos = getComputedStyle(e).position;
        if ((pos !== "fixed" && pos !== "sticky") || !visible(e) || inOverlay(e) || e.closest("dialog")) continue;
        const r = rectOf(e);
        if (r.left < -TOL || r.right > w + TOL || (pos === "fixed" && (r.top < -TOL || r.bottom > innerHeight + TOL))) {
          add("a", describe(e, `${pos} bar outside the viewport (${w}x${innerHeight})`));
        }
      }
    }

    // ---- (b) no two visible items of one row overlap ----
    if (which.includes("b")) {
      type Item = { el: Element; r: DOMRect };
      const CONTAINERS = "tr, .page-head, .toolbar, .bulkbar";
      // controls and media are items of their own; plain text is an item per text node
      const OWN = "button, a.button, summary, label.button, .badge, .tag, input, select, textarea, svg, img, .sig";
      // a box clipped by an overflow:hidden ancestor (an ellipsis cell) only counts where it is seen
      const clip = (r: DOMRect, from: Element | null, stop: Element): DOMRect | null => {
        let cur: DOMRect | null = r;
        for (let p = from; p && cur; p = p.parentElement === stop.parentElement ? null : p.parentElement) {
          const cs = getComputedStyle(p);
          if (cs.overflowX !== "visible" || cs.overflowY !== "visible") cur = intersect(cur, rectOf(p));
        }
        return cur;
      };
      for (const cont of document.querySelectorAll(CONTAINERS)) {
        if (!visible(cont) || inOverlay(cont)) continue;
        const items: Item[] = [];
        const w = document.createTreeWalker(cont, NodeFilter.SHOW_ELEMENT | NodeFilter.SHOW_TEXT);
        for (let n = w.nextNode(); n; n = w.nextNode()) {
          if (n.nodeType === Node.ELEMENT_NODE) {
            const e = n as Element;
            if (!e.matches(OWN) || !visible(e) || inOverlay(e) || e.closest(CONTAINERS) !== cont) continue;
            if (e.parentElement?.closest(OWN)) continue; // part of a control
            if (e.matches("input[type=hidden]") || (e.matches("svg") && e.closest("a.ext"))) continue;
            const r = clip(rectOf(e), e.parentElement, cont);
            if (r) items.push({ el: e, r });
          } else {
            const pe = (n as Text).parentElement;
            if (!pe || !n.textContent?.trim() || pe.closest(OWN) || pe.closest(".sr-only") || pe.closest(CONTAINERS) !== cont || inOverlay(pe) || !visible(pe)) continue;
            const rg = document.createRange();
            rg.selectNodeContents(n);
            for (const q of rg.getClientRects()) {
              if (q.width < 1 || q.height < 1) continue;
              const r = clip(q, pe, cont);
              if (r) items.push({ el: pe, r });
            }
          }
        }
        // a pinned (sticky) table cell paints over the columns that scroll under it (D19)
        const cellOf = (e: Element) => e.closest("td, th");
        const stuck = (e: Element) => { const c = cellOf(e); return !!c && getComputedStyle(c).position === "sticky"; };
        for (let i = 0; i < items.length; i++) {
          for (let j = i + 1; j < items.length; j++) {
            const A = items[i], B = items[j];
            if (A.el === B.el || A.el.contains(B.el) || B.el.contains(A.el)) continue;
            if (cellOf(A.el) !== cellOf(B.el) && stuck(A.el) !== stuck(B.el)) continue;
            const x = intersect(A.r, B.r);
            if (x && x.width * x.height > 1) add("b", `${describe(A.el)} overlaps ${describe(B.el)} by ${Math.round(x.width)}x${Math.round(x.height)}`);
          }
        }
      }
    }

    // ---- (c) labels and row controls vertically centred (2 px) ----
    if (which.includes("c")) {
      for (const e of document.querySelectorAll("button, a.button, summary.button, label.button, .badge, .navtop, .relay-btn")) {
        if (!visible(e) || inOverlay(e)) continue;
        const tr = textRect(e);
        if (!tr) continue;
        const r = rectOf(e);
        const cs = getComputedStyle(e);
        const top = r.top + parseFloat(cs.borderTopWidth), bottom = r.bottom - parseFloat(cs.borderBottomWidth);
        const gap = tr.top - top - (bottom - tr.bottom); // space above minus space below
        if (Math.abs(gap) / 2 > 2) add("c", describe(e, `label sits ${(Math.abs(gap) / 2).toFixed(1)}px ${gap < 0 ? "high" : "low"} in its box`));
      }
      // a checkbox or a toggle sits on the middle of its row; so does a lone row button of a table
      for (const e of controls) {
        if (!e.matches("input[type=checkbox], button, a.button, summary.button") || inOverlay(e) || e.closest("dialog")) continue;
        const tr = e.closest("tr"), td = e.closest("td");
        if (!tr || !td || !visible(tr)) continue;
        const dense = getComputedStyle(tr).display === "flex"; // a phone list row: two lines, the checkbox spans both
        const stacked = !dense && getComputedStyle(td).display !== "table-cell"; // a phone card: every cell its own line
        const toggle = e.matches(".relay-btn");
        const lone = (td.textContent ?? "").replace(/\s+/g, " ").trim() === (e.closest("label") ?? e).textContent!.replace(/\s+/g, " ").trim() && td.querySelectorAll(CONTROLS).length === 1;
        if (stacked || (dense ? !(e.matches("input[type=checkbox]") || toggle) : !(e.matches("input[type=checkbox]") || toggle || lone))) continue;
        // a phone list row keeps the checkbox and the toggle on the first line (QA #67): that line is the reference
        const p1 = dense ? tr.querySelector("td.p1") : null;
        const p1r = p1 ? rectOf(p1) : null; // the first line of the name cell (its min-height, the phone touch target), not the whole cell
        const rr = p1r ? new DOMRect(p1r.left, p1r.top, p1r.width, Math.min(p1r.height, parseFloat(getComputedStyle(p1!).minHeight) || p1r.height)) : rectOf(tr), er = rectOf(e);
        const off = (er.top + er.bottom) / 2 - (rr.top + rr.bottom) / 2;
        if (Math.abs(off) > 2) add("c", describe(e, `centre is ${off.toFixed(1)}px off the centre of its row ${box(rr)}`));
      }
    }

    // ---- (d) collapsed means header only ----
    if (which.includes("d")) {
      for (const d of document.querySelectorAll("details:not([open])")) {
        if (d.matches(".navgroup, .rowmenu, .colmenu") || d.closest(".audittable") || !visible(d)) continue;
        const sum = d.querySelector(":scope > summary");
        if (!sum) continue;
        if (getComputedStyle(d, "::details-content").contentVisibility === "visible") continue; // shown open on wide screens: not collapsed
        const dr = rectOf(d), sr = rectOf(sum);
        // a summary that is a legend on the card border takes no room of its own: whatever the card
        // measures below the frame line is empty
        const inFlow = getComputedStyle(sum).position !== "absolute";
        const header = inFlow ? sr.height : 0;
        const dcs = getComputedStyle(d);
        const own = ["paddingTop", "paddingBottom", "borderTopWidth", "borderBottomWidth"].reduce((n, k) => n + parseFloat(dcs[k as "paddingTop"]), 0); // the fold's own padding and rules are not a body
        if (inFlow && dr.height - sr.height - own > 2.5) add("d", describe(d, `collapsed but ${Math.round(dr.height - sr.height - own)}px taller than its summary`));
        const card = d.closest(".card");
        if (card && card.children.length === 1 && card.firstElementChild === d && rectOf(card).height > header + 2.5) {
          add("d", describe(card, `empty frame: the card is ${Math.round(rectOf(card).height)}px high around a ${inFlow ? "header of " + Math.round(sr.height) + "px" : "legend and nothing else"}`));
        }
      }
    }

    // ---- (e) buttons that sit together have one height (1 px) ----
    if (which.includes("e")) {
      // one group per card, dialog head or foot (the small Close and the foot buttons are two bars), toolbar-like bar, or table (its row buttons); a row button is compared with the
      // other row buttons of its table, not with the toolbar above it
      const GROUPS = ".card, .dlg-head, .dlg-foot, .udlg-foot, dialog, .toolbar, .actions, .bulkbar, .page-head, .savebar, .pager, .summary-bar";
      const groups = new Map<Element, Element[]>();
      for (const e of document.querySelectorAll("button, a.button, summary.button, label.button")) {
        if (!visible(e) || e.matches(".colbtn") || inOverlay(e) || e.closest(".bulkbar, .actionbar")) continue;
        const g = e.closest("tr") ? e.closest("table")! : (e.closest(GROUPS) ?? e.parentElement!);
        const list = groups.get(g) ?? [];
        list.push(e);
        groups.set(g, list);
      }
      for (const [g, list] of groups) {
        if (list.length < 2) continue;
        const hs = list.map((e) => rectOf(e).height);
        if (Math.max(...hs) - Math.min(...hs) > 1) add("e", `${describe(g)}: heights ${list.map((e, i) => `"${textOf(e)}" ${hs[i].toFixed(0)}`).join(", ")}`);
      }
    }

    // ---- (f) card content starts 8 px below the legend title ----
    if (which.includes("f")) {
      for (const card of document.querySelectorAll(".card")) {
        if (!visible(card)) continue;
        const first = card.firstElementChild;
        if (!first) continue;
        let title: Element | null = null;
        let rest: Element | null = null;
        if (first.matches("h2, .card-head")) {
          title = first;
          rest = first.nextElementSibling;
        } else if (first.matches("details.fold-h")) {
          title = first.querySelector(":scope > summary");
          rest = title?.nextElementSibling ?? null;
        }
        if (!title || getComputedStyle(title).position !== "absolute" || !visible(title)) continue; // a title in the flow takes its own room
        while (rest && !visible(rest)) rest = rest.nextElementSibling;
        if (!rest) continue;
        const L = rectOf(title).bottom;
        if (rectOf(rest).top < L + 8 - TOL) add("f", `${describe(rest)} starts ${(rectOf(rest).top - L).toFixed(1)}px below the legend "${textOf(title)}" (needs 8)`);
      }
    }

    // ---- (g) open overlays have a border and a shadow, and the border is seen ----
    if (which.includes("g")) {
      // Design v2 (prototype, plan decision): an overlay is framed by --color-border-strong (#c2c9d0 on white,
      // 1.67:1) and carries --shadow-lg; the shadow does the rest of the separation. v1 asked for 3:1 on the
      // border alone. 1.3:1 still catches a border that vanished into its surround (the old failures were
      // border = background, ratio 1:1) without forcing a colour the prototype does not have.
      const MIN_OVERLAY_EDGE = 1.3;
      const lum = (c: string) => {
        const m = c.match(/rgba?\(([^)]+)\)/);
        if (!m) return null;
        const [r, g, b, a] = m[1].split(/[ ,/]+/).filter(Boolean).map(parseFloat);
        const f = (v: number) => ((v /= 255) <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4));
        return { l: 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b), a: a === undefined || Number.isNaN(a) ? 1 : a };
      };
      const behind = (e: Element) => {
        for (let p = e.parentElement; p; p = p.parentElement) {
          const c = lum(getComputedStyle(p).backgroundColor);
          if (c && c.a > 0.5) return c.l;
        }
        return 1;
      };
      const open = [...document.querySelectorAll("details[open] > .rowmenu-body, details.navgroup[open] > .um-menu, details.colmenu[open] > .colpanel, dialog[open], .tooltip:not([hidden]), .audittable td.detail:hover .tip, .audittable td.detail:focus-within .tip")].filter(visible);
      for (const o of open) {
        const cs = getComputedStyle(o);
        // the phone drawer is a sheet on the left edge: its one free edge is the right border, and
        // the backdrop (not a shadow) sets it off from the page, as in the prototype
        const sheet = o.matches("dialog.drawer");
        const edge = sheet ? "Right" : "Top";
        const hasBorder = parseFloat(cs[`border${edge}Width`]) >= 1 && cs[`border${edge}Style`] !== "none";
        const hasShadow = sheet || (cs.boxShadow !== "none" && cs.boxShadow !== "");
        if (!hasBorder || !hasShadow) {
          add("g", describe(o, `needs a 1px border and a shadow (border ${hasBorder}, shadow ${hasShadow})`));
          continue;
        }
        const b = lum(cs[`border${edge}Color`]), p = behind(o);
        if (b && !sheet) {
          const ratio = (Math.max(b.l, p) + 0.05) / (Math.min(b.l, p) + 0.05);
          if (ratio < MIN_OVERLAY_EDGE) add("g", describe(o, `border contrast ${ratio.toFixed(2)}:1 against what is behind it, needs ${MIN_OVERLAY_EDGE}:1`));
        }
      }
    }
    // ---- (w) no word that fits on a line is cut across two lines ----
    // A browser only breaks inside a word when the word is wider than its box. So a word of 4+ letters
    // that is no wider than MAX_WORD px unbroken but still occupies two lines sits in a box squeezed
    // narrower than a short word (a column that gave its width away, break-all, overflow-wrap:anywhere
    // on a flex item), the "garage-plug-offl / ine" and "ESP8266E / X" of QA v2. Longer tokens (a
    // 63-letter device name, a JSON reply) have to break somewhere and are not judged. A word is a run
    // of letters and digits: hyphens, dots, slashes and commas end it. A browser has no break point at a
    // dot between letters, so a page that shows a host name in a narrow box puts a <wbr> after each dot
    // (ui.Soft); without one the host is cut inside a label, and that cut is reported (#511).
    if (which.includes("w")) {
      const MAX_WORD = 200;
      const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
      for (let n = walker.nextNode(); n; n = walker.nextNode()) {
        const el = n.parentElement;
        if (!el || !visible(el) || el.closest("script, style, textarea, select, option")) continue;
        const text = n.textContent ?? "";
        for (const m of text.matchAll(/[\p{L}\p{N}]{4,}/gu)) {
          const range = document.createRange();
          range.setStart(n, m.index!);
          range.setEnd(n, m.index! + m[0].length);
          const rects = [...range.getClientRects()].filter((r) => r.width > 0);
          const lines = new Set(rects.map((r) => Math.round(r.top / 4)));
          if (lines.size < 2) continue;
          const width = rects.reduce((sum, r) => sum + r.width, 0);
          if (width <= MAX_WORD) add("w", `"${m[0]}" is cut over ${lines.size} lines (${Math.round(width)}px unbroken) in ${describe(el)}`);
        }
      }
    }

    // ---- (o) text stays inside its own box and never overlaps other text ----
    // Catches a label squeezed into a one-word column (flex/grid shrink): its text then runs out of the
    // bottom of the box and is drawn over whatever follows.
    if (which.includes("o")) {
      const SPILL = 3; // px a text block may exceed its own box (line-height and descender slack)
      const holders = [...document.querySelectorAll("main label, main button, main summary, main p, main li, main h1, main h2, main h3, main h4, main dt, main dd, dialog[open] label, dialog[open] p, dialog[open] li")]
        .filter((e) => visible(e) && !scrolledAway(e) && !inOverlay(e));
      const own = new Map<Element, DOMRect>(); // box of the element's own direct text
      const lines = new Map<Element, DOMRect[]>(); // and its line boxes: text that wraps leaves room between its lines for other text
      // an open menu or popover floats over the page on purpose
      // the status bar (footer.sb, fixed to every page) is judged too: its nowrap items shrink when the row is full (#934)
      const all = [...document.querySelectorAll("main *, dialog[open] *, footer.sb *")].filter((e) => visible(e) && !scrolledAway(e) && !inOverlay(e) && !e.closest(".menu-panel, [popover]"));
      for (const e of all) {
        const w = document.createTreeWalker(e, NodeFilter.SHOW_TEXT, { acceptNode: (n) => (n.parentNode === e && n.textContent!.trim() ? NodeFilter.FILTER_ACCEPT : NodeFilter.FILTER_REJECT) });
        let l = Infinity, t = Infinity, r = -Infinity, b = -Infinity;
        const qs: DOMRect[] = [];
        for (let n = w.nextNode(); n; n = w.nextNode()) {
          const rg = document.createRange();
          rg.selectNodeContents(n);
          for (const q of rg.getClientRects()) {
            if (q.width < 1 || q.height < 1) continue;
            qs.push(q);
            l = Math.min(l, q.left); t = Math.min(t, q.top); r = Math.max(r, q.right); b = Math.max(b, q.bottom);
          }
        }
        if (l === Infinity || e.matches("textarea, input, select, option, pre, code, script, style")) continue;
        let tr: DOMRect | null = new DOMRect(l, t, r - l, b - t);
        let ls: (DOMRect | null)[] = qs;
        // text that the element or an ancestor clips (ellipsis) is not drawn outside its box
        for (let p: Element | null = e; p && tr && p !== document.body; p = p.parentElement) {
          if (getComputedStyle(p).overflowX !== "visible") {
            const pr = rectOf(p);
            tr = intersect(tr, pr);
            ls = ls.map((q) => (q ? intersect(q, pr) : null));
          }
        }
        if (tr) {
          own.set(e, tr);
          lines.set(e, ls.filter((q): q is DOMRect => !!q));
        }
      }
      for (const e of holders) {
        const cs = getComputedStyle(e);
        if (cs.overflowY !== "visible" || cs.display === "inline" || cs.display === "contents") continue;
        const tr = own.get(e), er = rectOf(e); // only the element's own text: a card may legitimately hold taller children
        if (tr && tr.bottom > er.bottom + SPILL) add("o", describe(e, `text runs ${(tr.bottom - er.bottom).toFixed(1)}px out of the bottom of its box`));
      }
      // a sticky or fixed bar (bulk bar, pinned actions column) is drawn over what scrolls under it on purpose
      const floating = (e: Element) => {
        for (let p: Element | null = e; p; p = p.parentElement) {
          const pos = getComputedStyle(p).position;
          if (pos === "sticky" || pos === "fixed") return p;
        }
        return null;
      };
      const items = [...own.entries()].map(([e, r]) => [e, r, floating(e)] as const);
      for (let i = 0; i < items.length; i++) {
        for (let j = i + 1; j < items.length; j++) {
          const [a, ra, fa] = items[i], [b, rb, fb] = items[j];
          if (a.contains(b) || b.contains(a) || a.closest("dialog") !== b.closest("dialog") || fa !== fb) continue;
          // box against box first (cheap), then line against line: the union box of a wrapped text covers the lines of its neighbours
          if (!intersect(ra, rb)) continue;
          const hit = (lines.get(a) ?? []).some((qa) => (lines.get(b) ?? []).some((qb) => { const x = intersect(qa, qb); return !!x && x.width > 2 && x.height > 4; }));
          if (hit) add("o", `${describe(a)} overlaps ${describe(b)}`);
        }
      }
    }
    // ---- (i) a table cell does not hide part of its content ----
    // The text of a cell ends inside the cell's content box at the default widths: a nowrap value
    // that runs into the padding or under the next column (or is clipped there, with or without an
    // ellipsis) is cut off, as the MQTT · HTTP state was. A cell that ellipsises on purpose says
    // so with a title (the tooltip shows the full text); one that scrolls is not a clip. Text
    // that wraps stays inside by itself.
    if (which.includes("i")) {
      for (const td of document.querySelectorAll("td")) {
        if (!visible(td) || inOverlay(td) || scrolledAway(td) || td.closest("[title]") || td.colSpan > 1) continue;
        const t = textRect(td);
        if (!t) continue;
        const cs = getComputedStyle(td);
        const limit = rectOf(td).right - parseFloat(cs.paddingRight);
        if (t.right > limit + 1) add("i", describe(td, `text ends ${Math.round(t.right - limit)}px past the cell's content box`));
      }
    }
    return out;
  }, which);
  return (Object.keys(found) as GuardId[]).sort().map((g) => ({ guard: g, what: guardNames[g], offenders: [...new Set(found[g])].slice(0, 12) }));
}

/** guardReport formats the results of layoutGuards (and of the table guards s and r) for an assertion message. */
export function guardReport(name: string, results: { guard: string; what: string; offenders: string[] }[]): string {
  return results.map((r) => `${name}: guard ${r.guard} (${r.what}):\n  - ${r.offenders.join("\n  - ")}`).join("\n");
}

/**
 * withFoldsOpen opens every closed disclosure of the page that is not a menu (so the
 * guards also see the folded content of a phone page), runs fn, and closes them
 * again so the test goes on with the page as it was.
 */
export async function withFoldsOpen<T>(page: Page, fn: () => Promise<T>): Promise<T> {
  await page.evaluate(() => {
    for (const d of document.querySelectorAll<HTMLDetailsElement>("details:not([open]):not(.navgroup):not(.rowmenu):not(.colmenu)")) {
      if (d.closest(".audittable")) continue;
      d.setAttribute("data-guard-opened", "");
      d.open = true;
    }
  });
  try {
    return await fn();
  } finally {
    await page.evaluate(() => {
      for (const d of document.querySelectorAll<HTMLDetailsElement>("details[data-guard-opened]")) {
        d.open = false;
        d.removeAttribute("data-guard-opened");
      }
    });
  }
}

/** The table guards of #821 (expectTableGuards), which knownLayoutIssues can name beside the guards of layoutGuards. */
export type TableGuardId = "s" | "r";

/**
 * knownLayoutIssues lists the pages whose guards are tolerated red for a named finding
 * (the visual QA). Keyed by the page name `expectLayout`, `expectTableGuards` and the specs that
 * name a case use; the value names the findings (an issue reference each) and the guards they trip.
 * A page in this list is reported as an annotation, not a failure; an entry is removed as soon as its
 * finding is fixed (the issue's acceptance criteria say so), and a guard that no longer fires for an
 * entry is reported as an annotation ("clean now"). There is no blanket switch.
 */
export const knownLayoutIssues: Record<string, { findings: string; guards: (GuardId | TableGuardId)[] }> = {
  // red on main when the table guards of #821 landed: each finding has its own issue, the guard stays on
  "devices list 1280px": { findings: "#906 (Address is 14 px too narrow for a 15-character address)", guards: ["s"] },
  "policy deviations 375px": { findings: "#907 (a host name breaks inside a label)", guards: ["w"] },
  "devices list saved layout 1290px": { findings: "#912 (Last restart cuts the date by 2-3 px beside spare room)", guards: ["s"] },
  "devices list saved layout 1290px bubbles": { findings: "#908 (the OTA URL bubble covers the row above)", guards: ["r"] },
};

/**
 * withKnownIssues returns the results that are red for the page `name` after the guards listed for it in
 * knownLayoutIssues are taken out; a tolerated result and a listed guard of `ran` that did not fire are
 * annotations of the test, so a tracked state is visible in the report and an entry that is no longer
 * needed is told.
 */
export function withKnownIssues<T extends { guard: string }>(name: string, ran: (GuardId | TableGuardId)[], results: T[]): T[] {
  const known = knownLayoutIssues[name];
  if (!known) return results;
  const tolerated = results.filter((r) => known.guards.includes(r.guard as GuardId));
  if (tolerated.length) {
    test.info().annotations.push({ type: "known layout issue", description: `${name}: guards ${tolerated.map((r) => r.guard).join(",")} (finding ${known.findings})` });
  }
  const clean = known.guards.filter((g) => ran.includes(g) && !results.some((r) => r.guard === g));
  if (clean.length) {
    test.info().annotations.push({ type: "known layout issue is clean", description: `${name}: guards ${clean.join(",")} no longer fire: remove the entry from knownLayoutIssues (finding ${known.findings})` });
  }
  return results.filter((r) => !known.guards.includes(r.guard as GuardId));
}

/**
 * menuFloatProblem opens the <details> menu of the given summary (by script, as overlayProblems
 * does) and returns what is wrong, or null. An open menu is a popover over the page: the element
 * the menu sits in keeps its height (a panel in the flow grew the "1 · Devices" head of
 * /updates/new to the menu's height), and no ancestor that
 * clips its overflow cuts the panel off. Guard h; a menu drawn open on purpose (data-static, the
 * style guide) is skipped.
 */
export async function menuFloatProblem(summary: Locator): Promise<string | null> {
  return summary.evaluate((s) => {
    const d = s.parentElement as HTMLDetailsElement;
    if (d.hasAttribute("data-static")) return null;
    const box = d.parentElement!;
    const wasOpen = d.open;
    d.open = false;
    const before = box.getBoundingClientRect().height;
    d.open = true;
    const after = box.getBoundingClientRect().height;
    const panel = [...d.children].find((c) => c.tagName !== "SUMMARY");
    let clip: string | null = null;
    if (panel) {
      const p = panel.getBoundingClientRect();
      for (let a = d.parentElement; a && a !== document.body; a = a.parentElement) {
        const cs = getComputedStyle(a);
        if (cs.overflowX === "visible" && cs.overflowY === "visible") continue;
        const r = a.getBoundingClientRect();
        if (p.top < r.top - 0.5 || p.bottom > r.bottom + 0.5 || p.left < r.left - 0.5 || p.right > r.right + 0.5) {
          clip = `${a.tagName.toLowerCase()}.${[...a.classList].join(".")} (overflow ${cs.overflowX}/${cs.overflowY})`;
          break;
        }
      }
    }
    d.open = wasOpen;
    const name = `details.${[...d.classList].join(".")} "${s.textContent!.trim()}"`;
    if (Math.abs(after - before) > 0.5) return `${name}: opening it changes ${box.tagName.toLowerCase()}.${[...box.classList].join(".")} from ${Math.round(before)} to ${Math.round(after)} px high`;
    if (clip) return `${name}: its panel is clipped by ${clip}`;
    return null;
  });
}

/**
 * overlayProblems opens every overlay the page has, one at a time (navigation and account menus,
 * the first row menu and every other kind of menu (guard h too), the Columns menu, a changelog dialog, the audit tooltip, a tooltip), runs
 * guard g on each, and closes it again.
 */
export async function overlayProblems(page: Page): Promise<GuardResult[]> {
  const found: GuardResult[] = [];
  const check = async (what: string) => {
    for (const r of await layoutGuards(page, ["g"])) found.push({ ...r, offenders: r.offenders.map((o) => `${what}: ${o}`) });
  };
  if (await page.locator("dialog[open]").count()) { // a modal dialog is open: nothing behind it can be clicked, the dialog is the overlay
    await check("dialog");
    return found;
  }
  const toggles = page.locator("summary.navtop:visible, .user summary:visible, details.navpanel > summary:visible, details.rowmenu > summary:visible, details.umenu > summary:visible, details.colmenu > summary:visible");
  const n = await toggles.count();
  const seen = new Set<string>();
  for (let i = 0; i < n; i++) {
    const s = toggles.nth(i);
    const kind = await s.evaluate((e) => (e.matches(".navtop, .who") ? e.textContent!.trim() : e.parentElement!.className));
    if (seen.has(kind)) continue; // one of each kind of menu is enough
    seen.add(kind);
    const wasOpen = await s.evaluate((e) => (e.parentElement as HTMLDetailsElement).open);
    if (!wasOpen) await s.evaluate((e) => ((e.parentElement as HTMLDetailsElement).open = true)); // opened by script: a neighbouring open menu may cover the trigger
    await check(`menu "${kind}"`);
    if (await s.evaluate((e) => e.parentElement!.matches("details.umenu, details.colmenu"))) {
      const p = await menuFloatProblem(s);
      if (p) found.push({ guard: "h", what: guardNames.h, offenders: [p] });
    }
    if (!wasOpen) await s.evaluate((e) => ((e.parentElement as HTMLDetailsElement).open = false)); // close without a second click: an open menu may cover its own trigger
  }
  const opener = page.locator("[data-dialog]:visible").first();
  if (await opener.count()) {
    await opener.click();
    await check("dialog");
    await page.keyboard.press("Escape");
  }
  const detail = page.locator(".audittable td.detail:visible").first();
  if (await detail.count()) {
    await detail.hover();
    await check("audit tooltip");
    await page.mouse.move(0, 0);
  }
  // a tooltip: put the pointer on the first titled element that is really under it (a card-wide link can cover the rest)
  const spot = await page.evaluate(() => {
    for (const e of document.querySelectorAll("main [title], main [data-tip]")) {
      const r = e.getBoundingClientRect();
      if (r.width < 4 || r.height < 4 || r.top < 0 || r.bottom > innerHeight) continue;
      const x = r.left + r.width / 2, y = r.top + r.height / 2;
      const top = document.elementFromPoint(x, y);
      if (top && (top === e || e.contains(top))) return { x, y };
    }
    return null;
  });
  if (spot) {
    await page.mouse.move(spot.x - 3, spot.y);
    await page.mouse.move(spot.x, spot.y);
    const shown = await page.locator(".tooltip:not([hidden])").waitFor({ timeout: 1500 }).then(() => true, () => false);
    if (shown) await check("tooltip");
    await page.mouse.move(0, 0);
  }
  return found;
}

/**
 * expectLayout runs the guards on the page as loaded, again with every fold open, and on every
 * overlay; it fails with a report unless the guard is listed for the page in knownLayoutIssues.
 */
export async function expectLayout(page: Page, name: string): Promise<void> {
  const loaded = await layoutGuards(page, ["a", "b", "c", "d", "e", "f", "w", "o", "i"]);
  const open = await withFoldsOpen(page, () => layoutGuards(page, ["a", "b", "c", "d", "e", "f", "w", "o", "i"]));
  const overlays = await overlayProblems(page);
  const byGuard = new Map<GuardId, GuardResult>();
  for (const r of [...loaded, ...open, ...overlays]) {
    const cur = byGuard.get(r.guard);
    if (cur) cur.offenders = [...new Set([...cur.offenders, ...r.offenders])].slice(0, 12);
    else byGuard.set(r.guard, { ...r });
  }
  const red = withKnownIssues(name, ["a", "b", "c", "d", "e", "f", "w", "o", "i", "g", "h"], [...byGuard.values()]);
  expect(red.length, guardReport(name, red)).toBe(0);
}

/**
 * nativeControlProblems checks what screenshots never show: the browser draws the popups and
 * pickers of native controls (select lists, date/time pickers, number spinners, file buttons,
 * checkboxes, scrollbars, autofill) from the control's color-scheme and the options' own colours,
 * not from the page's tokens. A dark page whose select popup came up white with light option text
 * (v0.11.1 feedback 3) passed every visual check. So, for the page as it is and once more with the
 * opposite explicit theme (Light or Dark chosen over the system setting):
 *
 * - the root and every input, select, textarea and button resolve color-scheme to the theme in force
 *   for them (the nearest data-theme, else the system scheme when the page follows it);
 * - every option and optgroup paints an opaque ground on the theme's side (dark in dark, light in
 *   light) with text of at least 4.5:1 on it;
 * - a placeholder on an opaque field ground has at least 4.5:1.
 */
export async function nativeControlProblems(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const canvas = document.createElement("canvas");
    canvas.width = canvas.height = 1;
    const ctx = canvas.getContext("2d", { willReadFrequently: true })!;
    // rgba resolves any CSS colour (rgb, hex, oklch, color-mix) through the canvas
    const rgba = (c: string): [number, number, number, number] => {
      ctx.clearRect(0, 0, 1, 1);
      ctx.fillStyle = "rgba(0,0,0,0)";
      ctx.fillStyle = c;
      ctx.fillRect(0, 0, 1, 1);
      const d = ctx.getImageData(0, 0, 1, 1).data;
      return [d[0], d[1], d[2], d[3] / 255];
    };
    const lum = ([r, g, b]: number[]) => {
      const f = (v: number) => ((v /= 255) <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4);
      return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b);
    };
    const ratio = (a: number[], b: number[]) => {
      const [x, y] = [lum(a), lum(b)].sort((p, q) => q - p);
      return (x + 0.05) / (y + 0.05);
    };
    const root = document.documentElement;
    const sys = matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
    const themeOf = (e: Element) => {
      const t = e.closest("[data-theme]")?.getAttribute("data-theme");
      if (t === "light" || t === "dark") return t;
      return root.hasAttribute("data-theme-auto") ? sys : "light";
    };
    const label = (e: Element) => {
      const h = e as HTMLInputElement;
      return `${e.tagName.toLowerCase()}${h.name ? `[name=${h.name}]` : ""}${h.type && e.tagName === "INPUT" ? `[type=${h.type}]` : ""}${e.getAttribute("aria-label") ? `[aria-label="${e.getAttribute("aria-label")}"]` : ""}`;
    };
    const check = (mode: string) => {
      const out: string[] = [];
      const rootScheme = getComputedStyle(root).colorScheme.trim();
      if (rootScheme !== themeOf(root)) out.push(`${mode}: html color-scheme "${rootScheme}", want "${themeOf(root)}" (scrollbars)`);
      for (const e of document.querySelectorAll("input, select, textarea, button")) {
        const want = themeOf(e);
        const cs = getComputedStyle(e);
        if (cs.colorScheme.trim() !== want) out.push(`${mode}: ${label(e)} color-scheme "${cs.colorScheme}", want "${want}"`);
        if ((e.tagName === "INPUT" || e.tagName === "TEXTAREA") && (e as HTMLInputElement).placeholder) {
          const bg = rgba(cs.backgroundColor);
          const ph = rgba(getComputedStyle(e, "::placeholder").color);
          if (bg[3] === 1 && ratio(ph, bg) < 4.5) out.push(`${mode}: ${label(e)} placeholder contrast ${ratio(ph, bg).toFixed(2)}:1`);
        }
      }
      for (const o of document.querySelectorAll("select option, select optgroup")) {
        const want = themeOf(o);
        const cs = getComputedStyle(o);
        const bg = rgba(cs.backgroundColor);
        const fg = rgba(cs.color);
        const where = `${mode}: ${label(o.closest("select")!)} ${o.tagName.toLowerCase()} "${(o as HTMLOptionElement).label ?? ""}"`;
        if (bg[3] !== 1) {
          out.push(`${where} has no opaque ground (${cs.backgroundColor}): the popup falls back to the browser's`);
          continue;
        }
        if ((want === "dark") !== lum(bg) < 0.18) out.push(`${where} ground ${cs.backgroundColor} is not ${want}`);
        if (ratio(fg, bg) < 4.5) out.push(`${where} contrast ${ratio(fg, bg).toFixed(2)}:1 (${cs.color} on ${cs.backgroundColor})`);
      }
      return out;
    };
    const theme = root.getAttribute("data-theme");
    const auto = root.hasAttribute("data-theme-auto");
    const problems = check(theme ? `theme ${theme}` : `auto (system ${sys})`);
    // the opposite explicit choice beats the system setting
    root.setAttribute("data-theme", (theme ?? sys) === "dark" ? "light" : "dark");
    root.removeAttribute("data-theme-auto");
    problems.push(...check(`theme ${root.getAttribute("data-theme")} over system ${sys}`));
    if (theme) root.setAttribute("data-theme", theme);
    else root.removeAttribute("data-theme");
    if (auto) root.setAttribute("data-theme-auto", "");
    // one line per distinct problem: a table repeats the same select per row
    return [...new Set(problems)];
  });
}

/** expectNativeControlsFollowTheme fails with nativeControlProblems' list. */
export async function expectNativeControlsFollowTheme(page: Page, name: string): Promise<void> {
  const problems = await nativeControlProblems(page);
  expect(problems, `${name}: native controls do not follow the theme:\n${problems.join("\n")}`).toEqual([]);
}

/**
 * cardStackGaps (#391, design rule "card stacks") lists every card that has more empty space above
 * it than the grid gap, within its column. A column is the set of cards that start at the same left
 * edge; a card under a short neighbour of a taller row (a row-aligned grid) shows up here, a card
 * in a stack does not. Cards are `.scard`, `.about-card`, `.devcard`, `.ulist`, `.wn-rel` and `.ucard`; hidden ones are ignored.
 */
export async function cardStackGaps(page: Page, maxGap = 17): Promise<string[]> {
  return page.evaluate((limit) => {
    const cards = [...document.querySelectorAll<HTMLElement>(".scard, .about-card, .devcard, .ulist, .wn-rel, .ucard")].filter((c) => {
      const r = c.getBoundingClientRect();
      return r.width > 0 && r.height > 0 && !c.closest("[hidden], dialog:not([open])");
    });
    const name = (c: Element) => c.querySelector("h2")?.textContent?.trim() ?? c.id ?? "?";
    const out: string[] = [];
    for (const b of cards) {
      const rb = b.getBoundingClientRect();
      const above = cards
        .filter((a) => a !== b && Math.abs(a.getBoundingClientRect().left - rb.left) < 1.5 && a.getBoundingClientRect().bottom <= rb.top + 0.5)
        .map((a) => a.getBoundingClientRect().bottom);
      if (above.length === 0) continue;
      const gap = rb.top - Math.max(...above);
      if (gap > limit) out.push(`"${name(b)}" has ${gap.toFixed(1)}px above it in its column (limit ${limit})`);
    }
    return out;
  }, maxGap);
}

/**
 * wrappedUserMeta (#1080) lists every source / last sign-in cell of the users list that needs more than one
 * line. On a phone a user row is name and role on line 1, then source and last sign-in on line 2; a last
 * sign-in that wraps (a view-only admin's Details link used to squeeze it) makes the row three lines tall.
 */
export async function wrappedUserMeta(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const out: string[] = [];
    for (const row of document.querySelectorAll(".users-list .urow")) {
      const who = row.querySelector(".urow-name > span")?.textContent?.trim() ?? "?";
      for (const m of row.querySelectorAll<HTMLElement>(".urow-meta")) {
        const cs = getComputedStyle(m);
        const line = parseFloat(cs.lineHeight) || parseFloat(cs.fontSize) * 1.3;
        const h = m.getBoundingClientRect().height;
        if (h > line * 1.5) out.push(`${who}: "${m.textContent!.trim().replace(/\s+/g, " ")}" is ${Math.round(h)}px tall (one line is ${Math.round(line)}px)`);
      }
    }
    return out;
  });
}

/** The bubble the shared tooltip shows for an element, and which other rows' text it covers. */
export interface TipReport {
  tip: { text: string; top: number; bottom: number; left: number; right: number } | null;
  /** rows (by index) whose text lies under the bubble; the row of the hovered element is not listed */
  covers: string[];
}

/**
 * hoverTip puts the pointer on `target` (a Locator inside a row of `rowsSel`), waits for the bubble
 * (it comes after 350 ms) and reports what it covers of the text of the OTHER rows of that table: a
 * bubble beside its own row may cover that row, never its neighbours (#811). Text of a cell is
 * measured as the text the cell shows (cut to the cell), so a cut-off value's hidden tail is not text.
 */
export async function hoverTip(page: Page, target: Locator, rowsSel: string, position?: { x: number; y: number }, timeout?: number): Promise<TipReport> {
  await page.mouse.move(0, 0);
  // The box is taken before the hover: the first hover moves a title into data-tip, and a selector on
  // title no longer finds the element.
  // The page swaps rows live; an element that is replaced while it is scrolled to or hovered is looked up again.
  let y = -1;
  for (let attempt = 1; ; attempt++) {
    try {
      await target.scrollIntoViewIfNeeded({ timeout });
      const box = await target.boundingBox();
      y = box ? box.y + (position?.y ?? box.height / 2) : -1;
      await target.hover({ position, timeout });
      break;
    } catch (e) {
      // an element that is replaced is looked up again; one that could not be pointed at within the given
      // timeout is the caller's to skip, not to wait for three more times
      if (attempt >= 4 || (timeout !== undefined && /Timeout/.test(String(e))) || !/not attached|detached/i.test(String(e))) throw e;
    }
  }
  await page.waitForTimeout(800);
  await installRowTexts(page);
  return page.evaluate(
    ([sel, y]) => {
      const tip = document.querySelector<HTMLElement>(".tooltip:not([hidden])");
      if (!tip) return { tip: null, covers: [] as string[] };
      const t = tip.getBoundingClientRect();
      const covers: string[] = [];
      const rows = [...document.querySelectorAll(sel as string)];
      // the owner of the bubble marks itself with aria-describedby; when a live refresh replaced it, the
      // row under the pointer stands in. With neither the report cannot say which row is the element's own.
      const owner = document.querySelector("[aria-describedby=tf-tooltip]")?.closest(sel as string);
      let own = owner ? rows.indexOf(owner) : -1;
      if (own < 0) {
        own = rows.findIndex((tr) => {
          const b = tr.getBoundingClientRect();
          return b.top <= (y as number) && (y as number) < b.bottom;
        });
      }
      if (own < 0) return { tip: null, covers: [] as string[] };
      const texts = (window as unknown as { __tfRowTexts: (rows: Element[]) => Box[][] }).__tfRowTexts(rows);
      rows.forEach((tr, i) => {
        if (i === own) return;
        const name = (tr.querySelector("td")?.textContent ?? "").trim().slice(0, 30) || `row ${i}`;
        for (const q of texts[i]) {
          if (Math.min(q.right, t.right) - Math.max(q.left, t.left) > 1 && Math.min(q.bottom, t.bottom) - Math.max(q.top, t.top) > 1) covers.push(`row ${i} (${name})`);
        }
      });
      return { tip: { text: tip.textContent ?? "", top: t.top, bottom: t.bottom, left: t.left, right: t.right }, covers: [...new Set(covers)] };
    },
    [rowsSel, y] as const,
  );
}

// ---------------------------------------------------------------------------
// Table guards (#821). Run on every table of a page, by tests/tablet-guards.spec.ts.
//
// s: a cell whose content is wider than its box, titled or not, while another column of the
//    same table has at least that much room to spare in every row. The generic guard i skips a
//    cell that has a title (an ellipsis on purpose), and that let Firmware be cut off beside an
//    empty gap (#811).
// r: no element of one row lies over the text of another row, as loaded and with the pointer on a
//    row and on its titled elements (the bubble of the shared tooltip is such an element, #811).
// ---------------------------------------------------------------------------

/** A box in viewport pixels, as getBoundingClientRect gives it. */
interface Box {
  left: number;
  right: number;
  top: number;
  bottom: number;
}

/**
 * rowTexts is the one place that says what "the text of a row" is: the boxes of the text nodes of the
 * row's cells, cut to their cell (a cut-off value's hidden tail is not text). It runs in the page and is
 * installed there as window.__tfRowTexts by installRowTexts, so hoverTip and the static row guard measure
 * the same thing and cannot drift apart.
 */
const rowTexts = (rows: Element[]): Box[][] =>
  rows.map((tr) => {
    const out: Box[] = [];
    for (const td of tr.querySelectorAll("td, th")) {
      const c = td.getBoundingClientRect();
      const w = document.createTreeWalker(td, NodeFilter.SHOW_TEXT);
      for (let n = w.nextNode(); n; n = w.nextNode()) {
        if (!n.textContent?.trim() || (n.parentElement as Element).closest(".sr-only, [hidden], .tip, .tooltip")) continue;
        const rg = document.createRange();
        rg.selectNodeContents(n);
        for (const q of rg.getClientRects()) {
          const l = Math.max(q.left, c.left), r = Math.min(q.right, c.right);
          if (q.width < 1 || q.height < 1 || r <= l) continue;
          out.push({ left: l, right: r, top: q.top, bottom: q.bottom });
        }
      }
    }
    return out;
  });

const installRowTexts = (page: Page) => page.evaluate(`window.__tfRowTexts = ${rowTexts.toString()}`);

/**
 * cutCellProblems is guard s for the tables of the page that are on screen as tables (the phone cards of
 * the Devices list are not). A cell is cut by `cut` px when its content (its own text, or a box inside it
 * that clips) is wider than its content box. That is a finding when some other column has at least `cut`
 * px of room in every row: the room was there, it sat in the wrong column.
 *
 * `freeText` names columns (by their header label) whose text is as long as the data makes it, such as the
 * audit summary: they are not judged. No cell is exempt for being the longest of its row, since that is
 * the cell most likely to be cut (a name, a firmware string, a URL); a page names its free-text columns
 * itself, with the reason (TablePage.freeText).
 */
export async function cutCellProblems(page: Page, freeText: string[] = []): Promise<string[]> {
  await page.evaluate(() => document.fonts.ready);
  return page.evaluate((freeText) => {
    const out: string[] = [];
    const px = (v: string) => parseFloat(v) || 0;
    for (const table of document.querySelectorAll("table")) {
      if (!table.checkVisibility({ visibilityProperty: true }) || table.closest("dialog:not([open]), [hidden]")) continue;
      const rows = [...table.tBodies].flatMap((b) => [...b.rows]).filter((tr) => tr.checkVisibility({ visibilityProperty: true }) && tr.getBoundingClientRect().height > 0);
      if (!rows.length) continue;
      // a phone card table: every cell is a line of its own, there are no columns to compare
      const first = rows[0].cells[0];
      if (!first || getComputedStyle(first).display !== "table-cell") continue;
      const heads = [...(table.tHead?.rows[0]?.cells ?? [])];
      const label = (i: number) => (heads[i]?.textContent ?? "").replace(/\s+/g, " ").trim() || `column ${i}`;
      // per column: the largest cut and the smallest room over the rows
      const cols = new Map<number, { cut: number; room: number; where: string }>();
      for (const tr of rows) {
        let idx = 0;
        const cells: { i: number; cut: number; room: number; avail: number; text: string }[] = [];
        for (const td of tr.cells) {
          const i = idx;
          idx += td.colSpan;
          if (td.colSpan > 1 || getComputedStyle(td).display !== "table-cell" || td.getBoundingClientRect().width === 0 || td.hidden) continue;
          // what a box shows to the right: its text (against its content box: an ellipsis cuts there) and its
          // controls and media (against its padding box: a 36 px menu button in a narrow cell may use the padding).
          // Screen-reader text is no content.
          const extent = (root: Element) => {
            let text = -Infinity, ctl = -Infinity;
            const walk = (n: Node) => {
              if (n.nodeType === Node.TEXT_NODE) {
                if (!n.textContent?.trim() || (n.parentElement as Element).closest(".sr-only")) return;
                const rg = document.createRange();
                rg.selectNodeContents(n);
                for (const q of rg.getClientRects()) if (q.width > 0 && q.height > 0) text = Math.max(text, q.right);
              } else if (n instanceof Element) {
                if (n.closest(".sr-only") || !n.checkVisibility({ visibilityProperty: true })) return;
                if (n.matches("svg, img, input, select, textarea, button, summary, .badge, .tag")) {
                  const q = n.getBoundingClientRect();
                  if (q.width > 0 && q.height > 0) ctl = Math.max(ctl, q.right);
                  return;
                }
                n.childNodes.forEach(walk);
              }
            };
            root.childNodes.forEach(walk);
            const r = root.getBoundingClientRect();
            const ccs = getComputedStyle(root);
            const pad = r.left + root.clientLeft + root.clientWidth;
            return { text, ctl, contentRight: pad - px(ccs.paddingRight), padRight: pad };
          };
          const cutOf = (x: Element) => {
            const e = extent(x);
            return Math.max(0, e.text - e.contentRight, e.ctl - e.padRight);
          };
          // the room the cell has to spare: how far its content stays short of the box it may use
          const own = extent(td);
          const avail = own.contentRight - (td.getBoundingClientRect().left + td.clientLeft + px(getComputedStyle(td).paddingLeft));
          const room = Math.max(0, Math.min(own.text === -Infinity ? Infinity : own.contentRight - own.text, own.ctl === -Infinity ? Infinity : own.padRight - own.ctl));
          // a box inside that clips its own content (an ellipsis) cuts it at its own edge
          let cut = cutOf(td);
          for (const x of td.querySelectorAll("*")) if (getComputedStyle(x).overflowX !== "visible" && !x.closest(".sr-only")) cut = Math.max(cut, cutOf(x));
          cells.push({ i, cut, room, avail, text: (td.textContent ?? "").replace(/\s+/g, " ").trim().slice(0, 30) });
        }
        for (const x of cells) {
          const c = cols.get(x.i) ?? { cut: 0, room: Infinity, where: "" };
          if (x.cut > c.cut && !freeText.includes(label(x.i))) {
            c.cut = x.cut;
            c.where = `"${x.text}" content ${Math.round(x.avail + x.cut)}px in ${Math.round(x.avail)}px`;
          }
          c.room = Math.min(c.room, x.room);
          cols.set(x.i, c);
        }
      }
      for (const [i, c] of cols) {
        if (c.cut <= 1.5) continue;
        const donors = [...cols].filter(([j, d]) => j !== i && d.room >= c.cut && d.room !== Infinity).map(([j, d]) => `${label(j)} ${Math.round(d.room)}px`);
        if (donors.length) out.push(`${label(i)} is cut off by ${Math.round(c.cut)}px (${c.where}) while ${donors.slice(0, 3).join(", ")} spare`);
      }
    }
    return [...new Set(out)];
  }, freeText);
}

/**
 * rowCoverProblems is guard r as loaded: for every visible table, the elements of a row that lie over the
 * text of another row (a badge, a switch, a wrapped chip that sticks out of its row), and rows whose boxes
 * overlap. The pointer part is hoverRowProblems.
 */
export async function rowCoverProblems(page: Page): Promise<string[]> {
  await page.evaluate(() => document.fonts.ready);
  await installRowTexts(page);
  return page.evaluate(() => {
    const out: string[] = [];
    const rowTexts = (window as unknown as { __tfRowTexts: (rows: Element[]) => Box[][] }).__tfRowTexts;
    const name = (tr: Element, i: number) => ((tr.querySelector("td")?.textContent ?? "").replace(/\s+/g, " ").trim().slice(0, 30) || `row ${i}`);
    // what an element shows: its box cut to the boxes that clip it (an ellipsis cell)
    const shown = (e: Element, stop: Element): Box | null => {
      const r = e.getBoundingClientRect();
      let b: Box = { left: r.left, right: r.right, top: r.top, bottom: r.bottom };
      for (let p = e.parentElement; p && p !== stop.parentElement; p = p.parentElement) {
        const cs = getComputedStyle(p);
        if (cs.overflowX === "visible" && cs.overflowY === "visible") continue;
        const q = p.getBoundingClientRect();
        b = { left: Math.max(b.left, q.left), right: Math.min(b.right, q.right), top: Math.max(b.top, q.top), bottom: Math.min(b.bottom, q.bottom) };
      }
      return b.right - b.left > 1 && b.bottom - b.top > 1 ? b : null;
    };
    for (const table of document.querySelectorAll("table")) {
      if (!table.checkVisibility({ visibilityProperty: true }) || table.closest("dialog:not([open]), [hidden]")) continue;
      const rows = [...table.tBodies].flatMap((b) => [...b.rows]).filter((tr) => tr.checkVisibility({ visibilityProperty: true }) && tr.getBoundingClientRect().height > 0);
      if (rows.length < 2) continue;
      const texts = rowTexts(rows);
      const where = `table${table.dataset.table ? `[data-table=${table.dataset.table}]` : table.className ? "." + table.className.trim().split(/\s+/).join(".") : ""}`;
      // a row and the one below it do not overlap (a tall cell or a negative margin)
      for (let i = 0; i + 1 < rows.length; i++) {
        const a = rows[i].getBoundingClientRect(), b = rows[i + 1].getBoundingClientRect();
        if (a.bottom > b.top + 1 && a.top < b.bottom - 1 && getComputedStyle(rows[i]).display !== "contents") out.push(`${where}: row ${i} (${name(rows[i], i)}) overlaps row ${i + 1} (${name(rows[i + 1], i + 1)}) by ${Math.round(a.bottom - b.top)}px`);
      }
      // an element of row i that shows over the text of rows around it
      rows.forEach((tr, i) => {
        for (const e of tr.querySelectorAll("td *, th *")) {
          if (e.closest(".sr-only, [hidden], .tip, .tooltip, details:not([open]) > :not(summary)") || !e.checkVisibility({ visibilityProperty: true })) continue;
          const b = shown(e, tr);
          if (!b) continue;
          for (let j = Math.max(0, i - 2); j <= Math.min(rows.length - 1, i + 2); j++) {
            if (j === i) continue;
            for (const t of texts[j]) {
              const w = Math.min(b.right, t.right) - Math.max(b.left, t.left), h = Math.min(b.bottom, t.bottom) - Math.max(b.top, t.top);
              if (w > 1 && h > 1) out.push(`${where}: ${e.tagName.toLowerCase()}${typeof e.className === "string" && e.className.trim() ? "." + e.className.trim().split(/\s+/).join(".") : ""} in row ${i} (${name(tr, i)}) lies over the text of row ${j} (${name(rows[j], j)}) by ${Math.round(w)}x${Math.round(h)}`);
            }
          }
        }
      });
    }
    return [...new Set(out)].slice(0, 12);
  });
}

/**
 * hoverRowProblems is guard r with the pointer: on each of the first `rows` rows of every visible table and on
 * every titled element of those rows (up to `perRow`), the bubble of the shared tooltip covers no other
 * row's text. It uses hoverTip, the one routine that puts the pointer somewhere and reads the bubble.
 */
export async function hoverRowProblems(page: Page, rows = 2, perRow = 8): Promise<string[]> {
  const tables = page.locator("table:visible");
  const problems: string[] = [];
  const n = await tables.count();
  for (let t = 0; t < n; t++) {
    const table = tables.nth(t);
    // The table is named by where it sits in the page, never by an attribute set on the element: a live region
    // swaps its whole card (#950), and the table that comes in has no attribute, so a later lookup waited for
    // the test's timeout (#1015). The path starts at the nearest ancestor with an id (a live region's card) or
    // at body; the guard's own evaluate calls are bounded, so a swap fails in seconds, not minutes.
    const tableSel = await table.evaluate((e) => {
      const body = e.tBodies[0];
      if (!body || body.rows.length < 2 || getComputedStyle(body.rows[0].cells[0] ?? e).display !== "table-cell") return null;
      const path: string[] = [];
      let el: Element | null = e;
      for (; el && el !== document.body; el = el.parentElement) {
        const id = el.getAttribute("id"); // not el.id: a form with a control named "id" answers with that control
        if (id) {
          path.unshift(`#${CSS.escape(id)}`);
          break;
        }
        const same = [...(el.parentElement?.children ?? [])].filter((c) => c.tagName === el!.tagName);
        path.unshift(`${el.tagName.toLowerCase()}:nth-of-type(${same.indexOf(el) + 1})`);
      }
      if (el === document.body) path.unshift("body");
      const sel = path.join(" > ");
      // a name that does not find exactly this table would measure another one: that is a finding, not a skip
      return { sel: document.querySelectorAll(sel).length === 1 && document.querySelector(sel) === e ? sel : "" };
    }, undefined, { timeout: 10_000 });
    if (!tableSel) continue;
    if (!tableSel.sel) {
      problems.push(`table ${t}: the guard could not name this table by its place in the page, so the bubble was never measured`);
      continue;
    }
    let hovered = 0;
    try {
      const rowsSel = `${tableSel.sel} > tbody > tr`;
      const trs = page.locator(rowsSel);
      for (let i = 0; i < Math.min(rows, await trs.count()); i++) {
        const targets: Locator[] = [trs.nth(i)];
        const titled = trs.nth(i).locator("[title], [data-tip]");
        const m = await titled.count();
        for (let k = 0; k < m && targets.length <= perRow; k++) targets.push(titled.nth(k));
        for (const target of targets) {
          if (!(await target.isVisible())) continue;
          // a target the page swapped away and did not bring back is skipped, bounded like hoverTip, and not counted as hovered
          const what = await target
            .evaluate((e) => `${e.tagName.toLowerCase()}${typeof e.className === "string" && e.className.trim() ? "." + e.className.trim().split(/\s+/)[0] : ""} in ${(e.closest("td") as HTMLElement | null)?.dataset.label ?? "row"}`, undefined, { timeout: 10_000 })
            .catch((e) => (/Timeout/.test(String(e)) ? null : Promise.reject(e)));
          if (what === null) {
            test.info().annotations.push({ type: "hover skipped", description: `table ${t} row ${i}: the element was replaced by a live swap and did not come back within 10 s` });
            continue;
          }
          // an element something else covers cannot be pointed at, and shows no bubble: it is skipped, not waited for
          const r = await hoverTip(page, target, rowsSel, target === targets[0] ? { x: 20, y: 18 } : undefined, 10_000).catch((e) => (/Timeout/.test(String(e)) ? null : Promise.reject(e)));
          if (!r) {
            test.info().annotations.push({ type: "hover skipped", description: `table ${t} row ${i} ${what}: could not be pointed at within 10 s` });
            continue;
          }
          hovered++;
          if (r.covers.length) problems.push(`table ${t} row ${i} ${what}: the bubble "${r.tip?.text.slice(0, 40)}" ${Math.round(r.tip!.top)}-${Math.round(r.tip!.bottom)} covers ${r.covers.join(", ")}`);
        }
      }
      // a pass that pointed at nothing proved nothing
      if (hovered === 0) problems.push(`table ${t}: no row or titled element could be pointed at, so the bubble was never measured`);
    } finally {
      await page.mouse.move(0, 0);
    }
  }
  return problems;
}

/**
 * expectTableGuards runs guards s and r (as loaded and with the pointer) on every table of the page, with
 * `freeText` the labels of the columns guard s does not judge. A page and guard listed in knownLayoutIssues
 * is reported as an annotation, as expectLayout does.
 */
export async function expectTableGuards(page: Page, name: string, freeText: string[] = []): Promise<void> {
  const found: { guard: TableGuardId; what: string; offenders: string[] }[] = [];
  const s = await cutCellProblems(page, freeText);
  if (s.length) found.push({ guard: "s", what: "a cell is not cut off while another column has room to spare", offenders: s });
  const r = [...(await rowCoverProblems(page)), ...(await hoverRowProblems(page))];
  if (r.length) found.push({ guard: "r", what: "no element of one row over the text of another, also with the pointer on a row", offenders: r });
  const red = withKnownIssues(name, ["s", "r"], found);
  expect(red.length, guardReport(name, red)).toBe(0);
}
