import type { Page } from "@playwright/test";

/**
 * Card width metrics for the card-width rule (#419, design-system.md "Card widths follow their
 * content"). Ported from the card audit used to find the offenders: for every card (top-most
 * only) the share of the content area it takes and how much of its own width its content wants
 * (max-content / actual).
 */
export interface CardWidth {
  card: string; // heading text, else aria-label, id or class
  width: number;
  area: number; // the page's content area (main without padding)
  fullWidth: boolean; // takes at least 95 % of the area
  maxContentPct: number; // max-content width / actual width, capped at 100
}

export interface PageWidth {
  cards: CardWidth[];
  area: number;
  left: number; // left edge of the leftmost card, relative to the area
  span: number; // distance from the leftmost card's left edge to the rightmost card's right edge
}

const CARD_SEL = [".card", ".scard", ".gcard", ".devcard", ".about-card", ".ulist", ".audit-card", ".txn-card", ".rp-card", ".pol-fix-card", ".fw-upload-card", ".confirm-card", ".otp-card", ".add-scan-card", ".stab-card", ".colpanel", ".upd-cards > *"].join(",");

/** measureCards measures the page as the user first sees it. */
export async function measureCards(page: Page): Promise<PageWidth> {
  await page.evaluate(() => document.fonts.ready);
  return page.evaluate((sel) => {
    const main = document.querySelector("main") ?? document.body;
    const mr = main.getBoundingClientRect();
    const ms = getComputedStyle(main);
    const areaL = mr.left + parseFloat(ms.paddingLeft);
    const area = mr.right - parseFloat(ms.paddingRight) - areaL;
    const vis = (e: Element) => {
      const r = e.getBoundingClientRect();
      const s = getComputedStyle(e);
      return r.width > 0 && r.height > 0 && s.visibility !== "hidden" && s.display !== "none";
    };
    const known = new Set(document.querySelectorAll(sel));
    // a surface that looks like a card (border or fill, radius) but has none of the known classes
    const generic = [...main.querySelectorAll("*")].filter((e) => {
      if (known.has(e) || e.closest("dialog,.menu-panel,svg") || !vis(e)) return false;
      const s = getComputedStyle(e);
      const r = e.getBoundingClientRect();
      if (r.width < 200 || r.height < 40) return false;
      const bw = parseFloat(s.borderTopWidth) + parseFloat(s.borderLeftWidth);
      const bg = s.backgroundColor !== "rgba(0, 0, 0, 0)" && s.backgroundColor !== "rgb(244, 245, 247)";
      return parseFloat(s.borderTopLeftRadius) >= 6 && (bw > 0 || bg) && !["BUTTON", "INPUT", "SELECT", "TEXTAREA", "A", "LABEL", "SUMMARY"].includes(e.tagName);
    });
    let cards = [...known, ...generic].filter((c) => main.contains(c) && vis(c));
    cards = cards.filter((c) => !cards.some((o) => o !== c && o.contains(c))); // top-most only
    const label = (c: Element) => {
      const h = c.querySelector("h1,h2,h3,summary,legend,.card-head");
      return ((h?.textContent ?? c.getAttribute("aria-label") ?? c.id) || c.className).replace(/\s+/g, " ").trim().slice(0, 50);
    };
    let minL = Infinity;
    let maxR = -Infinity;
    const out = cards.map((c) => {
      const el = c as HTMLElement;
      const r = c.getBoundingClientRect();
      minL = Math.min(minL, r.left);
      maxR = Math.max(maxR, r.right);
      // max-content: how wide the card wants to be when nothing stretches it
      const old = [el.style.width, el.style.maxWidth, el.style.justifySelf, el.style.alignSelf];
      el.style.width = "max-content";
      el.style.maxWidth = "none";
      el.style.justifySelf = "start";
      el.style.alignSelf = "start";
      const mc = c.getBoundingClientRect().width;
      [el.style.width, el.style.maxWidth, el.style.justifySelf, el.style.alignSelf] = old;
      return { card: label(c), width: Math.round(r.width), area: Math.round(area), fullWidth: r.width >= area * 0.95, maxContentPct: Math.round(Math.min(1, mc / r.width) * 100) };
    });
    return { cards: out, area: Math.round(area), left: Math.round(minL - areaL), span: Math.round(maxR - minL) };
  }, CARD_SEL);
}
