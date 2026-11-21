import type { Page } from "@playwright/test";

// WCAG 2.x contrast, shared by the specs that measure it (shell-css.spec.ts, contrast-dim.spec.ts).
// The browser resolves colours (any CSS syntax, the page's own opacity and background layers); the ratio is
// computed here, once.

type RGB = [number, number, number];

const lum = ([r, g, b]: RGB): number => {
  const f = (v: number) => ((v /= 255) <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4));
  return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b);
};

/** ratio is the WCAG contrast ratio of two opaque colours. */
export function ratio(a: RGB, b: RGB): number {
  const [hi, lo] = [lum(a), lum(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

/** contrast is the WCAG ratio of two CSS colours (any syntax the browser parses). */
export async function contrast(page: Page, fg: string, bg: string): Promise<number> {
  const [a, b] = await page.evaluate(
    (cols) => {
      const c = document.createElement("canvas").getContext("2d", { willReadFrequently: true })!;
      return cols.map((col) => {
        c.clearRect(0, 0, 1, 1);
        c.fillStyle = "#000";
        c.fillStyle = col;
        c.fillRect(0, 0, 1, 1);
        return [...c.getImageData(0, 0, 1, 1).data].slice(0, 3);
      });
    },
    [fg, bg],
  );
  return ratio(a as RGB, b as RGB);
}

/** One run of text as the page paints it: its colour and the ground behind it with every ancestor's opacity in. */
export interface TextRun {
  text: string;
  where: string;
  fg: RGB;
  bg: RGB;
  px: number;
  bold: boolean;
  /** the product of the opacity of the element and all its ancestors */
  opacity: number;
}

/**
 * textRuns measures every visible text node inside the elements `roots` matches. Text that belongs to a control that
 * cannot act (a disabled or aria-disabled button, input or switch, or one inside an inert region) is skipped: WCAG
 * exempts inactive components. The prose around such a control (labels, notes, headings) is not skipped, even in an
 * inert region: that text still has to be read. The colour is the text's own, blended onto the ground; the ground is
 * the stack of ancestor backgrounds, each group faded by its own opacity, so an opacity on a card counts for its text
 * and for the card's background alike.
 */
export async function textRuns(page: Page, roots: string): Promise<TextRun[]> {
  return page.evaluate((sel) => {
    const cv = document.createElement("canvas").getContext("2d", { willReadFrequently: true })!;
    type C = [number, number, number, number];
    const parse = (col: string): C => {
      cv.clearRect(0, 0, 1, 1);
      cv.fillStyle = "#000";
      cv.fillStyle = col;
      cv.fillRect(0, 0, 1, 1);
      const d = cv.getImageData(0, 0, 1, 1).data;
      // a colour with alpha is drawn on a transparent canvas: undo the premultiplication of getImageData
      return [d[0], d[1], d[2], d[3] / 255];
    };
    const over = (top: C, base: [number, number, number]): [number, number, number] => [
      top[0] * top[3] + base[0] * (1 - top[3]),
      top[1] * top[3] + base[1] * (1 - top[3]),
      top[2] * top[3] + base[2] * (1 - top[3]),
    ];
    const mix = (a: [number, number, number], b: [number, number, number], t: number): [number, number, number] => [
      a[0] * (1 - t) + b[0] * t,
      a[1] * (1 - t) + b[1] * t,
      a[2] * (1 - t) + b[2] * t,
    ];
    const CONTROL = 'button, input, select, textarea, [role="switch"], .menu-item, a.btn, .utab, .swtrack, .useg';
    const cannotAct = (el: Element): boolean => {
      const ctl = el.closest(CONTROL);
      if (!ctl) return false;
      return ctl.matches('[disabled], [aria-disabled="true"]') || ctl.closest("[inert]") !== null;
    };
    const describe = (el: Element): string => {
      const id = el.id ? `#${el.id}` : "";
      const cls = typeof el.className === "string" && el.className ? "." + el.className.trim().split(/\s+/).join(".") : "";
      return `${el.tagName.toLowerCase()}${id}${cls}`.slice(0, 80);
    };

    const out: {
      text: string;
      where: string;
      fg: [number, number, number];
      bg: [number, number, number];
      px: number;
      bold: boolean;
      opacity: number;
    }[] = [];
    for (const root of document.querySelectorAll(sel)) {
      const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
      for (let n = walker.nextNode(); n; n = walker.nextNode()) {
        const text = (n.textContent ?? "").replace(/\s+/g, " ").trim();
        const el = n.parentElement;
        if (!text || !el || ["SCRIPT", "STYLE"].includes(el.tagName)) continue;
        if (!el.checkVisibility({ visibilityProperty: true })) continue;
        const range = document.createRange();
        range.selectNodeContents(n);
        const box = range.getBoundingClientRect();
        if (box.width <= 1 || box.height <= 1) continue; // sr-only or collapsed
        if (cannotAct(el)) continue;

        // ancestors from the root down to the element
        const chain: Element[] = [];
        for (let e: Element | null = el; e; e = e.parentElement) chain.unshift(e);
        const cs = getComputedStyle(el);
        const textColour = parse(cs.color);
        const layers = chain.map((e) => {
          const s = getComputedStyle(e);
          return { bg: parse(s.backgroundColor), o: parseFloat(s.opacity) };
        });
        // group i draws its background and everything inside it, then fades the result by its opacity
        const paint = (i: number, backdrop: [number, number, number], withText: boolean): [number, number, number] => {
          if (i === layers.length) return withText ? over(textColour, backdrop) : backdrop;
          const L = layers[i];
          const inner = paint(i + 1, over(L.bg, backdrop), withText);
          return mix(backdrop, inner, L.o);
        };
        const canvas: [number, number, number] = [255, 255, 255];
        out.push({
          text: text.slice(0, 60),
          where: describe(el),
          fg: paint(0, canvas, true),
          bg: paint(0, canvas, false),
          px: parseFloat(cs.fontSize),
          bold: parseInt(cs.fontWeight, 10) >= 700,
          opacity: layers.reduce((o, l) => o * l.o, 1),
        });
      }
    }
    return out;
  }, roots);
}

/** lowContrast lists the runs below WCAG AA: 4.5:1, or 3:1 for large text (24 px, or 18.66 px bold). */
export function lowContrast(runs: TextRun[]): string[] {
  const out: string[] = [];
  for (const r of runs) {
    const large = r.px >= 24 || (r.px >= 18.66 && r.bold);
    const need = large ? 3 : 4.5;
    const got = ratio(r.fg, r.bg);
    if (got < need) out.push(`${r.where} "${r.text}" ${got.toFixed(2)}:1, needs ${need}:1`);
  }
  return out;
}

/** faded lists the runs drawn at less than full opacity: a state drawn by opacity on text instead of in words (#1071). */
export function faded(runs: TextRun[]): string[] {
  return runs.filter((r) => r.opacity < 1).map((r) => `${r.where} "${r.text}" at opacity ${r.opacity.toFixed(2)}`);
}
