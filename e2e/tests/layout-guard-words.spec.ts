import { test, expect } from "../support/fixtures";
import { layoutGuards } from "../support/layout";

// Guard "w" (a word that fits on a line is never cut across two lines) on built pages. A dot between
// letters is no break point for a browser, so a dotted host name is one word (#511): the guard must
// not blame a label that the browser had to cut because the host was wider than its line, but it must
// still catch a plain word cut in a column squeezed narrower than the word. (A dotted host in a box
// narrower than itself cannot be told from a forced cut and is not judged.)
const column = (width: number, body: string) =>
  `<!doctype html><main><div style="width:${width}px;font:13px/1.4 sans-serif;overflow-wrap:anywhere">${body}</div></main>`;
const findings = async (page: import("@playwright/test").Page, html: string) => {
  await page.setViewportSize({ width: 800, height: 600 });
  await page.setContent(html);
  return (await layoutGuards(page, ["w"])).flatMap((r) => r.offenders);
};
const host = "livingroom-readinglamp.example.internal";

test("w: a host wider than its line is cut inside a label by force, not reported", async ({ page }) => {
  // "readinglamp.example.internal" is about 190 px wide: unbroken it does not fit a 120 px line
  expect(await findings(page, column(120, `Used by ${host}`))).toEqual([]);
});

test("w: a <wbr> after each dot lets the host break at its labels, nothing is cut", async ({ page }) => {
  expect(await findings(page, column(120, "Used by livingroom-<wbr>readinglamp.<wbr>example.<wbr>internal"))).toEqual([]);
});

test("w: a word that fits the line but is cut in a squeezed column is still reported", async ({ page }) => {
  // "offline" is about 45 px wide and has a 30 px box: it was squeezed, not too long for any line
  const found = await findings(page, `<!doctype html><main><div style="display:flex"><span style="width:30px;overflow-wrap:anywhere;font:13px sans-serif">offline</span></div></main>`);
  expect(found.join("\n")).toContain('"offline" is cut over');
});
