import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "@playwright/test";
import { entries } from "../support/visual-entries";
import { designAdmin, designOperator } from "../support/seed";
import { capture, defaultMasks, type Theme, type Width } from "../support/visual";
import { isSelected, stackKey, stacksFile } from "../support/visual-select";

// Parity run (plan 4.2, WP F1): a package that must not change how a page looks (the templ
// toolchain and bridge) proves it by rendering every page twice, once with the binaries of the
// base commit and once with the new ones (E2E_BIN_DIR), and comparing the two sets pixel by pixel
// with tolerance 0. This spec writes one set; e2e/visual-parity.mjs compares two. Pages are the
// entries of the V0 table that need no clicks, at 1280 and 375 px, light and dark, captured the
// way the references are. Values the app invents per run (times, addresses) are masked, as in
// the comparison with the prototype.
//
//   make visual-parity BASE=<commit>      (the Makefile target drives both runs)

const out = process.env.PARITY_OUT;
const widths: Width[] = [1280, 375];
const themes: Theme[] = ["light", "dark"];

interface RunningStack {
  url: string;
  placeholders: Record<string, string>;
}

const seen = new Set<string>();
const pages = entries.filter(isSelected).filter((e) => {
  const key = `${stackKey(e)}|${e.as ?? "admin"}|${e.route}`;
  if ((e.steps?.length ?? 0) > 0 || e.mutates || seen.has(key)) return false;
  seen.add(key);
  return true;
});

for (const e of pages) {
  for (const width of widths) {
    for (const theme of themes) {
      const slug = `${stackKey(e)}-${e.as ?? "admin"}-${e.route.replace(/[^\w{}]+/g, "_")}-${width}-${theme}`;
      test(slug, async ({ browser }) => {
        if (!out) throw new Error("PARITY_OUT is not set: run through make visual-parity");
        const stack = (JSON.parse(readFileSync(stacksFile, "utf8")) as Record<string, RunningStack>)[stackKey(e)];
        if (!stack) throw new Error(`stack ${stackKey(e)} was not started`);
        const route = e.route.replace(/\{(\w+)\}/g, (_, k: string) => stack.placeholders[k] ?? "");
        const headers = e.stack === "local" || e.stack === "setup" ? undefined : e.as === "operator" ? designOperator : designAdmin;
        const context = await browser.newContext({
          viewport: { width, height: 900 },
          deviceScaleFactor: 1,
          colorScheme: theme,
          reducedMotion: "reduce",
          extraHTTPHeaders: headers,
          ignoreHTTPSErrors: true,
          bypassCSP: true, // the mask style below is injected by the test, not served by the app
        });
        try {
          const page = await context.newPage();
          await page.goto(stack.url + route);
          // masks are painted over the page itself, so a changed value never counts
          await page.addStyleTag({ content: defaultMasks.map((s) => `${s}{visibility:hidden !important}`).join(",") });
          const png = await capture(page, width, e.mode ?? "full");
          mkdirSync(out, { recursive: true });
          writeFileSync(join(out, `${slug}.png`), png);
        } finally {
          await context.close();
        }
      });
    }
  }
}
