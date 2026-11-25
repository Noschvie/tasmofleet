import { test, expect } from "../support/fixtures";
import { checkTableCalls, dataSites, tablePages, tableSites } from "../support/table-pages";

// #821: a table that no page of the guard list shows has no layout guard, and the nightly layout guards
// found three table defects (#805, #806, #811) only after a release because of such gaps. The list of
// tables is read from the templates (support/table-pages.ts), so a new table fails here until a page
// that shows it joins tablePages. No stack: it reads the sources.
const pages = tablePages({ lamp: () => "/devices/1" });

test("every table of the web UI is shown by a page that has the table guards", () => {
  const sites = tableSites().map((s) => s.id);
  const claimed = new Set(pages.flatMap((p) => p.sites));
  expect(
    sites.filter((id) => !claimed.has(id)),
    "tables in the templates that no page of tablePages shows: add a page to tablePages (support/table-pages.ts)",
  ).toEqual([]);
  expect(
    [...claimed].filter((id) => !sites.includes(id)),
    "tablePages claims a table that is not in the templates",
  ).toEqual([]);
});

test("a page that shows a table is listed once per width and says why a width is left out", () => {
  const names = pages.map((p) => `${p.stack}:${p.name}`);
  expect(new Set(names).size, "page names are unique per stack").toBe(names.length);
  for (const p of pages) if (p.widths) expect(p.note, `${p.name} leaves a width out: say why in note`).toBeTruthy();
});

// The coverage test is only as good as its reading of the sources: a table it cannot see is a table nobody guards.
test("a ui.TableData is found wherever its Key stands, and one without a literal Key is an error", () => {
  const go = (body: string) => `package web\n\nfunc buildIt(p page) ui.TableData {\n\tt := ${body}\n\treturn t\n}\n`;
  expect(dataSites("x.go", go(`ui.TableData{Class: "dense", SortBase: p.Base, Key: "things"}`), false)).toMatchObject([{ id: "x.go buildIt", dataTable: "things" }]);
  expect(dataSites("x.go", go(`ui.TableData{Class: "a {b}", Cols: []ui.TableCol{{Label: "L"}},\n\tKey: "later"}`), false)[0].dataTable).toBe("later");
  expect(() => dataSites("x.go", go(`ui.TableData{Class: "dense"}`), false)).toThrow(/no literal Key/);
  expect(() => dataSites("x.go", go(`ui.TableData{Key: name}`), false)).toThrow(/no literal Key/);
  // #1094: a table with no Key (an ordered list, no registry entry) is named by its literal column labels, two at least
  expect(dataSites("x.go", go(`ui.TableData{Class: "dense", Cols: []ui.TableCol{{Label: "Rule"}, {Label: "Setting", Col: "c"}}}`), false)).toMatchObject([{ id: "x.go buildIt", dataTable: "", labels: ["Rule", "Setting"] }]);
  expect(() => dataSites("x.go", go(`ui.TableData{Class: "dense", Cols: []ui.TableCol{{Label: "Rule"}}}`), false)).toThrow(/fewer than two/);
  expect(dataSites("y.templ", `package web\n\ntempl thingsView() {\n\t@ui.Table(ui.TableData{Key: "things_view"}) {}\n}\n`, true)).toMatchObject([{ id: "y.templ thingsView", dataTable: "things_view" }]);
});

test("an @ui.Table call in a templ must be given a literal ui.TableData", () => {
  expect(() => checkTableCalls("y.templ", `templ v() {\n\t@ui.Table(data) {}\n}`)).toThrow(/cannot be named/);
  expect(() => checkTableCalls("y.templ", `templ v() {\n\t@ui.Table( ui.TableData{Key: "k"}) {}\n}`)).not.toThrow();
});
