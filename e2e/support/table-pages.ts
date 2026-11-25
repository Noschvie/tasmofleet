import { readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";
import { expect, type Page } from "@playwright/test";
import { expectLayout, expectTableGuards } from "./layout";
import { settled } from "./page-list";
import { putScanResult } from "./scan-fixture";
import { root } from "./stack";

// #821: every page that shows a table has the table layout guards (layout.ts: cutCellProblems and
// rowCoverProblems). The list of tables is read from the templates, so a new table that no page claims
// fails the coverage test (tablet-guards.spec.ts) instead of going unguarded.
//
// A table site is one table of the web UI: a <table> in a .templ file, named by the templ component
// that renders it, or a table the Go code builds with ui.TableData (the Devices list). What a site looks
// like in the browser is read from the same text: its data-table key, or the static header labels.

export interface TableSite {
  /** the component (templ) or function (Go) that draws the table, with the file: "about.templ aboutConfigTable" */
  id: string;
  file: string;
  /** the static data-table key of the table, "" when it has none */
  dataTable: string;
  /** the static header labels, in order (what the page shows in its <th>s; template expressions are left out) */
  labels: string[];
}

const dirs = ["internal", "cmd"];

// A <table> that is not a site of the web UI, with the reason. Everything else that has a <table> must
// be listed in tablePages.
const notSites: Record<string, string> = {
  "internal/web/ui/table.templ": "ui.Table, the frame component: its callers are the sites (the Go files below)",
  "internal/web/styleguide.templ": "the style guide's samples of the components, not pages of the app (its table is the one ui.Table draws)",
  "internal/auth/pages.go": "the auth package's plain fallback pages; the app serves the users page from accountpages.templ",
};

function* files(dir: string): Generator<string> {
  for (const e of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, e.name);
    if (e.isDirectory()) {
      if (e.name !== "testdata" && e.name !== "node_modules") yield* files(p);
    } else yield p;
  }
}

/** textOf is the text a <th> shows: its content without tags and without template expressions. */
const textOf = (html: string) => {
  const noExpr = html
    .replace(/\b(?:else if|if|for|switch|case)\b[^{<>]*\{/g, "") // a templ if/for block opens with a brace that has no partner in the text
    .replace(/\{[^}]*\}/g, "")
    .replace(/[{}]/g, "");
  // tags are dropped one character at a time: a replace of "<...>" can leave a new tag behind ("<<b>b>")
  let text = "";
  let inTag = false;
  for (const ch of noExpr) {
    if (ch === "<") inTag = true;
    else if (ch === ">" && inTag) inTag = false;
    else if (!inTag) text += ch;
  }
  return text.replace(/\s+/g, " ").trim();
};

/** bracedBody is the text between the brace at `open` and its partner (string literals are skipped). */
function bracedBody(src: string, open: number): string {
  let depth = 0;
  for (let i = open; i < src.length; i++) {
    const c = src[i];
    if (c === '"' || c === "`") {
      for (i++; i < src.length && src[i] !== c; i++) if (c === '"' && src[i] === "\\") i++;
    } else if (c === "{") depth++;
    else if (c === "}" && --depth === 0) return src.slice(open + 1, i);
  }
  throw new Error("a { is never closed");
}

/**
 * dataSites reads the tables a source builds from ui.TableData (drawn by ui.Table): the function or templ
 * component that builds one is the site. Every literal is read, wherever its Key stands, and one without a
 * literal Key is an error, so a table cannot go unnamed.
 */
export function dataSites(file: string, src: string, isTempl: boolean): TableSite[] {
  const out: TableSite[] = [];
  for (const m of src.matchAll(/\bui\.TableData\{/g)) {
    const body = bracedBody(src, m.index! + m[0].length - 1);
    const key = /\bKey:\s*"([a-z_]+)"/.exec(body);
    // a table without a Key is no registry table (an ordered list, the policy rules): its header labels name it, as for a <table>
    const labels = key ? [] : [...body.matchAll(/\bLabel:\s*"([^"]+)"/g)].map((l) => l[1]);
    if (!key && labels.length < 2) throw new Error(`${file}: a ui.TableData has no literal Key and fewer than two literal column Labels, so the table cannot be named (e2e/support/table-pages.ts)`);
    const fn = [...src.slice(0, m.index).matchAll(isTempl ? /^templ\s+(\w+)\(/gm : /^func\s+(?:\([^)]*\)\s*)?(\w+)\(/gm)].pop();
    if (!fn) throw new Error(`${file}: a ui.TableData is not inside a function or component`);
    out.push({ id: `${file} ${fn[1]}`, file, dataTable: key ? key[1] : "", labels });
  }
  return out;
}

/** checkTableCalls throws for a templ that calls ui.Table with anything but a literal ui.TableData: nobody could name that table. */
export function checkTableCalls(file: string, src: string): void {
  for (const m of src.matchAll(/@ui\.Table\((?!\s*ui\.TableData\{)/g)) {
    throw new Error(`${file}: @ui.Table at offset ${m.index} is not given a literal ui.TableData, so the table cannot be named (e2e/support/table-pages.ts)`);
  }
}

/** tableSites reads every table the web UI can draw. It throws when it finds one it cannot name. */
export function tableSites(): TableSite[] {
  const sites: TableSite[] = [];
  const seen = new Set<string>();
  for (const dir of dirs) {
    for (const abs of files(join(root, dir))) {
      const file = relative(root, abs).split("\\").join("/");
      const isTempl = file.endsWith(".templ");
      const isGo = file.endsWith(".go") && !file.endsWith("_test.go") && !file.endsWith("_templ.go");
      if (!isTempl && !isGo) continue;
      let src = readFileSync(abs, "utf8");
      // Go comments talk about <table> a lot; only code and string literals count
      if (isGo) src = src.replace(/^\s*\/\/.*$/gm, "");
      if (!notSites[file]) sites.push(...dataSites(file, src, isTempl));
      if (/ui\.TableData\{/.test(src)) seen.add(file);
      if (isTempl && !notSites[file]) checkTableCalls(file, src);
      // in Go code only a literal table (<table> followed by its head or rows) counts: tableprefs.go
      // rewrites the tags of tables the templates drew, and talks about them
      const tableRe = isGo ? /<table\b([^>]*)>\s*(?=<thead|<tr|<tbody|<colgroup)/g : /<table\b([^>]*)>/g;
      for (const m of src.matchAll(tableRe)) {
        if (notSites[file]) {
          seen.add(file);
          continue;
        }
        if (!isTempl) throw new Error(`${file}: has a <table> in Go code that is neither a templ component nor listed in notSites (e2e/support/table-pages.ts)`);
        const at = m.index!;
        const comp = [...src.slice(0, at).matchAll(/^templ\s+(\w+)\(/gm)].pop();
        if (!comp) throw new Error(`${file}: a <table> at offset ${at} is not inside a templ component`);
        const end = src.indexOf("</table>", at);
        if (end < 0) throw new Error(`${file}: the <table> of ${comp[1]} is never closed`);
        const block = src.slice(at, end);
        const head = /<thead\b[\s\S]*?<\/thead>/.exec(block)?.[0] ?? "";
        // a header is a <th>, or a component call that draws one: @sortTh(p, "name", "Device name") (its last string)
        const labels = [...head.matchAll(/<th\b[^>]*>([\s\S]*?)<\/th>|@\w*Th\(([^)]*)\)/g)]
          .map((h) => (h[1] !== undefined ? textOf(h[1]) : ([...h[2].matchAll(/"([^"]*)"/g)].pop()?.[1] ?? "")))
          .filter(Boolean);
        if (!/\bdata-table="/.test(m[1]) && labels.length < 2) throw new Error(`${file}: the table of ${comp[1]} has no data-table key and fewer than two static header labels, so a page cannot be checked against it`);
        const id = `${file} ${comp[1]}`;
        if (sites.some((s) => s.id === id)) throw new Error(`${file}: ${comp[1]} draws two tables; name them apart in tableSites`);
        sites.push({ id, file, dataTable: /\bdata-table="([a-z_]+)"/.exec(m[1])?.[1] ?? "", labels });
        seen.add(file);
      }
    }
  }
  for (const f of Object.keys(notSites)) if (!seen.has(f)) throw new Error(`notSites lists ${f}, which has no <table> any more: remove the entry`);
  return sites;
}

/** A page with tables: how to reach it and which sites it shows. */
export interface TablePage {
  name: string;
  /** which stack shows it: the realistic fleet of tablet-guards.spec.ts, or the GPIO template shapes of policy-templates.spec.ts */
  stack: "fleet" | "templates";
  /** the path, a function where it depends on the seeded fleet */
  path: () => string;
  /** the sites (TableSite.id) that are visible on this page once `prepare` has run */
  sites: string[];
  /** the widths at which the page is a table; the default is both. Say why in `note`. */
  widths?: number[];
  note?: string;
  /**
   * header labels of columns whose text is as long as the data makes it (the audit summary): guard s does not
   * judge them. Say why in `note`; no other cell is exempt, the longest cell of a row included.
   */
  freeText?: string[];
  /** steps after the page is loaded (open a fold, press a button) that bring the tables up */
  prepare?: (page: Page) => Promise<void>;
}

/**
 * siteVisible reports whether a table of the site is on screen: the data-table key when the site has
 * one, else a table whose header shows the site's labels in order. Five sites share class="dense" and
 * two have no class at all, so labels are what tells them apart; two tables with the same labels
 * (the two configuration tables, Key and Value) are told apart by the page that claims them.
 */
export async function siteVisible(page: Page, site: TableSite): Promise<boolean> {
  return page.evaluate((s) => {
    const shown = (e: Element) => e.checkVisibility({ visibilityProperty: true }) && !e.closest("dialog:not([open]), [hidden]");
    for (const t of document.querySelectorAll<HTMLTableElement>("table")) {
      if (!shown(t) || !t.tBodies[0]?.rows.length) continue;
      if (s.dataTable) {
        if (t.dataset.table === s.dataTable) return true;
        continue;
      }
      const heads = [...(t.tHead?.rows[0]?.cells ?? [])].map((c) => (c.textContent ?? "").replace(/\s+/g, " ").trim()).filter(Boolean);
      let at = 0;
      for (const l of s.labels) {
        const i = heads.indexOf(l, at);
        if (i < 0) {
          at = -1;
          break;
        }
        at = i + 1;
      }
      if (at >= 0 && s.labels.length) return true;
    }
    return false;
  }, site);
}

const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

export interface TableContext {
  lamp: () => string; // device path of the seeded "lamp-1m"
}

/**
 * tablePages is every page that shows a table, with the sites it shows. The coverage test
 * (table-pages.spec.ts) fails for a site no page claims and for a claim that is no site, and each
 * page's guard run fails when a claimed table is not on screen. A page that needs state to show its table
 * (a result, a preview, proposals) brings it up in `prepare`.
 */
export function tablePages(ctx: TableContext): TablePage[] {
  const all = tableSites();
  const site = (file: string, name: string) => {
    const found = all.find((s) => s.id === `internal/web/${file} ${name}`);
    if (!found) throw new Error(`no table site ${file} ${name}: the page list names a table that is not in the templates`);
    return found.id;
  };
  const retry = site("messagepage.templ", "retryPageView");
  return [
    {
      name: "devices list",
      stack: "fleet",
      path: () => "/devices",
      sites: [site("devicelist.go", "deviceTableData")],
      widths: [1280],
      note: "below 45rem the list is phone cards (devicelist.go), which have no columns; ui-pages.spec.ts checks them at 375 px",
    },
    {
      name: "add devices: discovered",
      stack: "fleet",
      path: () => "/devices/new",
      sites: [site("adddevices.templ", "discoveredBody")],
      widths: [1280],
      note: "the proposals the spec seeds are shown as cards below 45rem",
    },
    {
      name: "add devices: network scan",
      stack: "fleet",
      path: () => "/discovery/scan",
      sites: [site("adddevices.templ", "scanLive")],
      widths: [1280],
      note: "no stack can serve port 80, so the finished scan's markup is put in (support/scan-fixture.ts); cards below 45rem",
      prepare: (page) => putScanResult(page, "mixed"),
    },
    {
      name: "device settings: result",
      stack: "fleet",
      path: () => `${ctx.lamp()}/settings`,
      sites: [site("devicetabs.templ", "settingsBodyInner")],
      // a changed value gives the Result table; an unchanged form only a notice
      prepare: async (page) => {
        // another value per width: a form saved unchanged gives a notice, not the Result table
        await page.locator("input[name=TelePeriod]").fill(String((page.viewportSize()?.width ?? 1280) / 5));
        await page.getByRole("button", { name: "Save to device" }).click();
        await expect(page.getByRole("heading", { name: "Result" })).toBeVisible({ timeout: 30_000 });
      },
    },
    { name: "about", stack: "fleet", path: () => "/about", sites: [site("about.templ", "aboutConfigTable")] },
    { name: "system settings: advanced", stack: "fleet", path: () => "/settings/system?tab=advanced", sites: [site("syssettings.templ", "sysContainerTable")] },
    { name: "devices backups", stack: "fleet", path: () => "/devices/backups", sites: [site("fleetbackups.templ", "fleetDevices")] },
    {
      name: "audit",
      stack: "fleet",
      path: () => "/audit?since=all",
      sites: [site("audit.templ", "auditView")],
      freeText: ["Summary"],
      note: "the summary of an entry is a sentence of whatever the action did: it is as long as the data, so guard s does not judge that column",
    },
    { name: "policy overview", stack: "fleet", path: () => "/policy", sites: [site("policy_overview.templ", "policyTable")] },
    { name: "policy deviations", stack: "fleet", path: () => "/policy/deviations", sites: [site("policydev.templ", "policyDevTable")] },
    { name: "policy rule", stack: "fleet", path: () => "/policy/rules/r1", sites: [site("policyrule.templ", "ruleDevicesCard")] },
    {
      name: "transaction retry",
      stack: "fleet",
      path: () => "/transactions",
      sites: [retry],
      // the retry page of a transaction with failed devices; the seeded fleet has one among its transactions
      prepare: async (page) => {
        const hrefs = await page.locator("a[href*='/transactions/']").evaluateAll((a) => [...new Set(a.map((e) => e.getAttribute("href")!.replace(/^https?:\/\/[^/]+/, "")))]);
        const base = new URL(page.url()).origin;
        for (const h of hrefs.reverse()) {
          await page.goto(base + h + "/retry");
          if (await siteVisible(page, all.find((s) => s.id === retry)!)) return;
        }
        throw new Error(`no transaction of ${hrefs.length} offers a retry with a table`);
      },
    },
    {
      name: "policy templates: groups",
      stack: "templates",
      path: () => "/policy/templates",
      sites: [site("gputemplates.templ", "gtRow")],
      prepare: async (page) => {
        await page.getByRole("button", { name: "Read again" }).click();
        await expect(page.locator(".gt-card", { hasText: "tpl-nous-dev" })).toBeVisible({ timeout: 60_000 });
        // the pin tables sit in closed folds; a person opens them to see the differences
        await page.locator("details.gt-pins").evaluateAll((d) => d.forEach((x) => ((x as HTMLDetailsElement).open = true)));
      },
    },
    {
      name: "policy templates: preview",
      stack: "templates",
      path: () => "/policy/templates",
      sites: [site("gputemplates.templ", "gtReferenceCard")],
      prepare: async (page) => {
        await page.getByRole("button", { name: "Read again" }).click();
        const card = page.locator(".gt-card", { hasText: "tpl-nous-dev" });
        await expect(card).toBeVisible({ timeout: 60_000 });
        await card.getByRole("button", { name: /^Apply reference to \d/ }).click();
        await expect(page.getByRole("heading", { level: 1 })).toContainText("template to");
      },
    },
  ];
}

/** tablePageCases is the pages of one stack as the widths they are tables at. */
export function tablePageCases(ctx: TableContext, stack: TablePage["stack"]): { page: TablePage; width: number }[] {
  return tablePages(ctx)
    .filter((p) => p.stack === stack)
    .flatMap((page) => (page.widths ?? [1280, 375]).map((width) => ({ page, width })));
}

/**
 * checkTablePage opens the page as an admin at the width, brings its tables up, checks that every claimed
 * site is on screen, and runs the layout guards (expectLayout) and the table guards (s and r) on it.
 */
export async function checkTablePage(page: Page, base: string, tp: TablePage, width: number): Promise<void> {
  await page.setExtraHTTPHeaders(adam);
  await page.setViewportSize({ width, height: 900 });
  await page.emulateMedia({ colorScheme: "light" });
  const resp = await page.goto(base + tp.path());
  expect(resp?.status(), `GET ${tp.path()}`).toBeLessThan(400);
  await settled(page);
  await tp.prepare?.(page);
  await settled(page);
  await page.waitForTimeout(800);
  const all = tableSites();
  for (const id of tp.sites) {
    const s = all.find((x) => x.id === id)!;
    expect(await siteVisible(page, s), `${tp.name}: no table that looks like ${id} is on screen at ${width} px (labels ${JSON.stringify(s.labels)}, data-table "${s.dataTable}")`).toBe(true);
  }
  await expectLayout(page, `${tp.name} ${width}px`);
  await expectTableGuards(page, `${tp.name} ${width}px`, tp.freeText);
}
