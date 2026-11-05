import { test, expect } from "../support/fixtures";
import { layoutGuards } from "../support/layout";

// Guard "w" (a word that fits on a line is never cut across two lines) on built pages. A word is a run of
// letters and digits, so every label of a host name is a word of its own. A browser has no break point at
// a dot between letters: a host wider than its box is cut inside a label unless the page puts a <wbr>
// after each dot (ui.Soft, #511). The guard reports that cut, so every narrow place that shows host names
// gets the break points, and it keeps catching a word cut in a column squeezed narrower than the word.
const column = (width: number, body: string) =>
  `<!doctype html><main><div style="width:${width}px;font:13px/1.4 sans-serif;overflow-wrap:anywhere">${body}</div></main>`;
const squeezed = (width: number, word: string) =>
  `<!doctype html><main><div style="display:flex"><span style="width:${width}px;overflow-wrap:anywhere;font:13px sans-serif">${word}</span></div></main>`;
const findings = async (
  page: import("@playwright/test").Page,
  html: string,
) => {
  await page.setViewportSize({ width: 800, height: 600 });
  await page.setContent(html);
  return (await layoutGuards(page, ["w"])).flatMap((r) => r.offenders);
};

test("w: a host without break points that is wider than its line is reported: a label is cut", async ({
  page,
}) => {
  // "readinglamp.example.internal" is about 190 px wide: unbroken it does not fit a 120 px line
  const found = await findings(
    page,
    column(120, "Used by livingroom-readinglamp.example.internal"),
  );
  expect(found.join("\n")).toMatch(
    /"(readinglamp|example|internal)" is cut over/,
  );
});

test("w: a <wbr> after each dot lets the host break at its labels, nothing is cut", async ({
  page,
}) => {
  expect(
    await findings(
      page,
      column(
        120,
        "Used by livingroom-<wbr>readinglamp.<wbr>example.<wbr>internal",
      ),
    ),
  ).toEqual([]);
});

test("w: a word that fits the line but is cut in a squeezed column is still reported", async ({
  page,
}) => {
  // "offline" is about 45 px wide and has a 30 px box: it was squeezed, not too long for any line
  expect((await findings(page, squeezed(30, "offline"))).join("\n")).toContain(
    '"offline" is cut over',
  );
});

test("w: a dotted name cut in a squeezed column is reported too", async ({
  page,
}) => {
  // "lamp.local" is about 60 px wide in a 40 px box: its label "local" is cut
  expect(
    (await findings(page, squeezed(40, "lamp.local"))).join("\n"),
  ).toContain('"local" is cut over');
});
