import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { cutCellProblems, layoutGuards, rowCoverProblems } from "../support/layout";

// Guards s and r (table cells and rows, #821) and the line-wise guard o, on built pages: each guard must
// report the defect it exists for and stay quiet where the page is right. Without these a guard that
// never fires would pass every page.
const page_ = async (page: Page, html: string) => {
  await page.setViewportSize({ width: 800, height: 600 });
  await page.setContent(`<!doctype html><style>
    body{font:13px/1.4 sans-serif;margin:0} .sr-only{position:absolute;width:1px;height:1px;overflow:hidden;white-space:nowrap}
    table{border-collapse:collapse;table-layout:fixed} td,th{padding:4px 10px;text-align:left}
    td.cut{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
  </style><main>${html}</main>`);
};
const table = (cols: string[], rows: string[][]) =>
  `<table style="width:400px"><colgroup>${cols.map((c) => `<col style="width:${c}px">`).join("")}</colgroup><thead><tr>${cols.map((_, i) => `<th>Col ${i}</th>`).join("")}</tr></thead><tbody>${rows.map((r) => `<tr>${r.join("")}</tr>`).join("")}</tbody></table>`;

test("s: a titled cell that is cut while another column has room is reported (the Firmware cell of #811, the IP cell of the discovered list)", async ({ page }) => {
  // a 90 px IP column for a 12-character address, and a wide note column with more text than the address but room to spare
  await page_(page, table(["90", "310"], [[`<td class="cut" title="172.31.13.41">172.31.13.41 and more</td>`, `<td>a longer note, wider than the address</td>`], [`<td class="cut" title="x">x</td>`, `<td>ok</td>`]]));
  expect((await cutCellProblems(page)).join("\n")).toMatch(/Col 0 is cut off by \d+px .* while Col 1 \d+px spare/);
});

test("s: the same cut is no finding where no other column has room for it", async ({ page }) => {
  // every column is as full as the cut cell: nothing could have given it room
  await page_(page, table(["90", "310"], [[`<td class="cut" title="t">172.31.13.41 and more</td>`, `<td class="cut">${"w".repeat(60)}</td>`]]));
  expect(await cutCellProblems(page)).toEqual([]);
});

test("s: a free-text column that a page names is not judged, and only that column (the audit summary)", async ({ page }) => {
  // the summary is as long as the data makes it; whatever the Result column spares, the next row's text would not fit either
  await page_(page, table(["60", "160", "180"], [[`<td>ok</td>`, `<td class="cut" title="t">command=Status 2 reason=on our systems</td>`, `<td>done</td>`]]));
  expect(await cutCellProblems(page, ["Col 1"])).toEqual([]);
  // without the name the page is judged: nobody is exempt for being long
  expect((await cutCellProblems(page)).join("\n")).toMatch(/Col 1 is cut off/);
  // another column's cut is still a finding when a free-text column is named
  expect((await cutCellProblems(page, ["Col 0"])).join("\n")).toMatch(/Col 1 is cut off/);
});

test("s: a cut cell that holds the most text of its row is reported while another column has room (a name, a firmware string, a URL)", async ({ page }) => {
  // Name is the longest text of its row and cut at 150 px; Uptime is 250 px wide for 7 characters. No global rule may hide this.
  await page_(page, table(["150", "250"], [[`<td class="cut" title="AtticHeaterPlugLongName">AtticHeaterPlugLongName and a suffix</td>`, `<td>1h 16m</td>`]]));
  expect((await cutCellProblems(page)).join("\n")).toMatch(/Col 0 is cut off by \d+px .* while Col 1 \d+px spare/);
});

test("s: screen-reader text and a control that uses its cell's padding are no content that is cut", async ({ page }) => {
  // "(opens in a new window)" is read aloud, not drawn; a 36 px button in a 48 px cell with 10 px padding is inside its cell
  await page_(page, table(["140", "48", "212"], [[`<td class="cut" title="a">127.0.0.1:34567<span class="sr-only">(opens in a new window)</span></td>`, `<td><button style="width:36px;padding:0;margin:0 -4px">⋯</button></td>`, `<td>ok</td>`]]));
  expect(await cutCellProblems(page)).toEqual([]);
});

test("r: an element of one row that lies over the text of the next row is reported", async ({ page }) => {
  await page_(page, table(["200", "200"], [[`<td><span style="position:relative;top:18px">badge over the next row</span></td>`, `<td>a</td>`], [`<td>Next row text</td>`, `<td>b</td>`]]));
  expect((await rowCoverProblems(page)).join("\n")).toMatch(/lies over the text of row 1/);
});

test("r: rows that keep their elements inside them are clean", async ({ page }) => {
  await page_(page, table(["200", "200"], [[`<td><span>badge</span></td>`, `<td>a</td>`], [`<td>Next row text</td>`, `<td>b</td>`]]));
  expect(await rowCoverProblems(page)).toEqual([]);
});

test("o: a note that wraps in the middle of a date does not overlap the link on its first line", async ({ page }) => {
  // the date's text spans two lines; its union box covers the link, its lines do not (the reference card of the template preview)
  await page_(page, `<p style="width:250px;margin:0">Most common: <a href="#">tpl-nous-1</a>, read live <time>Nov 16, 2026, 01:02 PM</time></p>`);
  expect(await layoutGuards(page, ["o"])).toEqual([]);
});

test("o: two texts drawn on top of each other are still reported", async ({ page }) => {
  await page_(page, `<div style="position:relative;height:40px"><p style="position:absolute;top:0;left:0;margin:0">first line of text</p><p style="position:absolute;top:4px;left:10px;margin:0">second line of text</p></div>`);
  expect((await layoutGuards(page, ["o"])).flatMap((r) => r.offenders).join("\n")).toMatch(/overlaps/);
});
