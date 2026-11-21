import type { Locator } from "@playwright/test";

/**
 * hitBox probes what a pointer actually hits around an element: the corners and edge midpoints of a square of
 * `size` px centred on it (11.5 px from the centre for 24 px, a hair inside the edge so sub-pixel rounding cannot
 * flake). Every probe must land on the element or inside it, which counts a transparent ::after as part of the
 * element, as a finger does. It returns what missed, empty when the element reaches a size x size target (WCAG 2.5.8;
 * #1075). An element that is 16 px drawn but 24 px hit-testable passes; a 15 px tall label does not.
 */
export async function hitBox(loc: Locator, size = 24): Promise<string[]> {
  // centred, so a sticky bar at the bottom of the page does not cover it
  await loc.evaluate((el) => el.scrollIntoView({ block: "center" }));
  return loc.evaluate((el, n) => {
    const r = el.getBoundingClientRect();
    const cx = r.x + r.width / 2;
    const cy = r.y + r.height / 2;
    const d = n / 2 - 0.5;
    const misses: string[] = [];
    for (const [dx, dy] of [[-d, -d], [0, -d], [d, -d], [-d, 0], [d, 0], [-d, d], [0, d], [d, d]]) {
      const hit = document.elementFromPoint(cx + dx, cy + dy);
      if (!hit || !el.contains(hit)) {
        const what = hit ? `${hit.tagName.toLowerCase()}${hit.className ? "." + String(hit.className).trim().split(/\s+/).join(".") : ""}` : "nothing";
        misses.push(`${dx},${dy} hits ${what}`);
      }
    }
    const label = `${el.tagName.toLowerCase()}${el.className ? "." + String(el.className).trim().split(/\s+/).join(".") : ""} (${Math.round(r.width)} x ${Math.round(r.height)} px drawn)`;
    return misses.length ? [`${label}: ${misses.join("; ")}`] : [];
  }, size);
}
