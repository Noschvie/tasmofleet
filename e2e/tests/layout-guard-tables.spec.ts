import { readFileSync } from "node:fs";
import { join } from "node:path";
import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { cutCellProblems, headerStyleProblems, hoverRowProblems, layoutGuards, rowCoverProblems } from "../support/layout";
import { root } from "../support/stack";

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

// Why: pages with a live region (the Backups page, #950) swap their whole card, table included, when an event
// arrives. The hover pass named its table by an attribute set at runtime; the table that came in had none, so
// the pass lost the rest of the table, and when the swap fell between two of its calls it waited for the
// 10-minute test timeout (nightly 44571540336, #1015). The pass must go on with the table that replaced the
// first and point at its rows too. The swap here comes during the pause after the first hover; the rows
// the pointer reached are counted by the page, which a swap does not reset.
test("r: a table that a live region swaps during the pass is measured on, row by row", async ({ page }) => {
  test.setTimeout(60_000);
  const rows = `<tr><td title="first row">first row</td><td>a</td></tr><tr><td title="second row">second row</td><td>b</td></tr>`;
  const card = `<table style="width:400px"><thead><tr><th>Col 0</th><th>Col 1</th></tr></thead><tbody>${rows}</tbody></table>`;
  await page_(page, `<section id="live-card">${card}</section>`);
  await page.evaluate((card) => {
    const w = window as unknown as { __swaps: number; __rows: string[] };
    w.__swaps = 0;
    w.__rows = [];
    document.addEventListener("pointerover", (e) => {
      const tr = (e.target as Element).closest("tbody tr") as HTMLTableRowElement | null;
      if (!tr) return;
      const name = tr.cells[0].textContent ?? "";
      if (!w.__rows.includes(name)) w.__rows.push(name);
      if (w.__swaps) return;
      w.__swaps = 1; // the first pointer on a row swaps the whole card once, as the live region does on an event
      setTimeout(() => (document.getElementById("live-card")!.innerHTML = card), 0);
    });
  }, card);
  expect(await hoverRowProblems(page, 2, 2)).toEqual([]);
  const seen = await page.evaluate(() => ({ swaps: (window as unknown as { __swaps: number }).__swaps, rows: (window as unknown as { __rows: string[] }).__rows }));
  expect(seen.swaps, "the page swapped the table during the pass").toBe(1);
  expect(seen.rows, "the pointer reached the second row of the table that replaced the first").toContain("second row");
});

// Why: a form with a control named "id" answers form.id with that control, not with its id attribute; the
// add-devices pages have such forms, and the guard named their tables "#[object RadioNodeList] > ..." and
// measured nothing (found by the first run of tablet-guards on the new naming, #1015).
test("r: a table in a form that has a control named id is still named and measured", async ({ page }) => {
  test.setTimeout(60_000);
  const rows = `<tr><td title="first row">first row</td><td>a</td></tr><tr><td title="second row">second row</td><td>b</td></tr>`;
  await page_(page, `<form id="scan"><input name="id" type="hidden" value="1"><div><table style="width:400px"><thead><tr><th>Col 0</th><th>Col 1</th></tr></thead><tbody>${rows}</tbody></table></div></form>`);
  expect(await hoverRowProblems(page, 2, 2)).toEqual([]);
});

// Why (#1094): the design system has one header style for every data table. The audit found three (a band on
// the keyed tables, none on the plain ones, a bold 11.5 px header on the GPIO pin tables); each page was right by
// its own CSS and nothing compared them. Guard t compares every header cell with the base one.
test("t: a header that is uppercase, bold or without the band of the others is reported, one in the base style is not", async ({ page }) => {
  await page_(page, `<style>thead th{background:#eee;font:500 12px sans-serif;text-transform:none;letter-spacing:0}</style>
    <table id="ok" style="width:400px"><thead><tr><th>Name</th><th>State</th></tr></thead><tbody><tr><td>a</td><td>b</td></tr></tbody></table>
    <table id="loud" style="width:400px"><thead><tr><th style="text-transform:uppercase">Name</th><th style="font:600 11.5px sans-serif">State</th><th style="background:transparent">Plain</th></tr></thead><tbody><tr><td>a</td><td>b</td><td>c</td></tr></tbody></table>`);
  const found = (await headerStyleProblems(page)).join("\n");
  expect(found).toMatch(/#loud header "Name": text-transform uppercase/);
  expect(found).toMatch(/#loud header "State": font-size 11.5px/);
  expect(found).toMatch(/#loud header "Plain": background/);
  expect(found).not.toMatch(/#ok/);
});

// The real stylesheet with the classes the pages draw: the keyed table in its frame (the devices list, the audit), a
// plain table in a card (about, backups, the policy tables), the add-devices table and the GPIO pin table. On main
// the frame painted a band, the plain table none and the pin table a bold 11.5 px header.
test("t: the header cells of every table frame the app draws share one style in the real stylesheet", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  const head = `<thead><tr><th>Name</th><th>Status</th></tr></thead><tbody><tr><td>a</td><td>b</td></tr></tbody>`;
  await page.setContent(`<!doctype html><html><body><main>
    <div class="tablewrap"><div class="tablescroll"><table id="keyed" class="devtable dense" data-table="devices">${head}</table></div></div>
    <section class="devcard"><table id="plain" class="dense">${head}</table></section>
    <div class="add-table"><table id="add">${head}</table></div>
    <details class="gt-pins" open><table id="gpio" class="gt-ptab">${head}</table></details>
  </main></body></html>`);
  await page.addStyleTag({ content: readFileSync(join(root, "internal/web/static/app.css"), "utf8") });
  for (const scheme of ["light", "dark"] as const) {
    await page.evaluate((t) => document.documentElement.setAttribute("data-theme", t), scheme);
    expect(await headerStyleProblems(page), `${scheme}: one header style`).toEqual([]);
  }
});
