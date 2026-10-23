// Screenshot every preview page at 1280 and 375 px, light and dark, and report
// layout defects (horizontal page overflow, missing Archivo, console errors).
//
//   node tools/shots.mjs <out-dir> [filter]
//
// Playwright is borrowed from the tasmofleet e2e suite (no own node_modules).
import { createRequire } from "node:module";
import { readdirSync, mkdirSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import os from "node:os";

const require = createRequire(
  join(os.homedir(), "Projects/tasmofleet/e2e/package.json"),
);
const { chromium } = require("playwright");

const bundle = join(dirname(fileURLToPath(import.meta.url)), "..", "ds-bundle");
const out = process.argv[2];
const filter = process.argv[3] || "";
mkdirSync(out, { recursive: true });

const pages = ["thumbnail.html"];
for (const dir of ["foundations", "components", "templates"]) {
  for (const f of readdirSync(join(bundle, dir)).sort())
    if (f.endsWith(".html")) pages.push(`${dir}/${f}`);
}

const browser = await chromium.launch();
const problems = [];
for (const scheme of ["light", "dark"]) {
  for (const width of [1280, 375]) {
    const ctx = await browser.newContext({
      viewport: { width, height: 900 },
      colorScheme: scheme,
      deviceScaleFactor: 1,
    });
    for (const p of pages.filter((x) => x.includes(filter))) {
      const page = await ctx.newPage();
      const errors = [];
      page.on("console", (m) => m.type() === "error" && errors.push(m.text()));
      page.on("pageerror", (e) => errors.push(String(e)));
      await page.goto("file://" + join(bundle, p));
      await page.evaluate(() => document.fonts.ready);
      const info = await page.evaluate(() => ({
        overflow: document.documentElement.scrollWidth - window.innerWidth,
        archivo: document.fonts.check("14px Archivo"),
        loaded: [...document.fonts].filter((f) => f.status === "loaded").length,
        // guard (e) of the visual QA plan: buttons that sit together have one height
        heights: [...document.querySelectorAll(".toolbar, .actions, .bulkbar, .page-head, .dlg-head, .row-actions")]
          .map((g) => {
            const hs = [...g.querySelectorAll("button, a.button, summary.button")]
              .filter((b) => b.checkVisibility() && b.closest(".rowmenu-body, .menu") === null)
              .map((b) => b.getBoundingClientRect().height);
            return hs.length > 1 && Math.max(...hs) - Math.min(...hs) > 1 ? `${g.className}: ${hs.join("/")}` : null;
          })
          .filter(Boolean),
        // guard (d): a closed fold shows its header only
        folds: [...document.querySelectorAll("details:not([open])")]
          .filter((d) => d.checkVisibility())
          .map((d) => {
            const s = d.querySelector(":scope > summary");
            const extra = d.getBoundingClientRect().height - (s ? s.getBoundingClientRect().height : 0);
            return extra > 2 && !s?.matches(".button, .navtop, .who") ? `${d.className}: +${extra.toFixed(1)}px` : null;
          })
          .filter(Boolean),
      }));
      for (const h of info.heights) problems.push(`${p} ${width} ${scheme}: mixed button heights in ${h}`);
      for (const f of info.folds) problems.push(`${p} ${width} ${scheme}: closed fold not header-only: ${f}`);
      const name = `${p.replace(/\//g, "_").replace(".html", "")}-${width}-${scheme}.png`;
      await page.screenshot({ path: join(out, name), fullPage: true });
      if (info.overflow > 0)
        problems.push(`${name}: page overflows by ${info.overflow}px`);
      if (!info.loaded) problems.push(`${name}: no web font loaded`);
      for (const e of errors) problems.push(`${name}: ${e}`);
      await page.close();
    }
    await ctx.close();
  }
}
// explicit override: data-theme wins over the system preference
const ctx = await browser.newContext({
  viewport: { width: 1280, height: 900 },
  colorScheme: "dark",
});
const page = await ctx.newPage();
await page.goto(
  "file://" + join(bundle, "components/buttons.html") + "#theme=light",
);
const bg = await page.evaluate(
  () => getComputedStyle(document.body).backgroundColor,
);
await page.screenshot({
  path: join(out, "override-dark-system-light-theme.png"),
});
if (bg !== "rgb(244, 246, 246)")
  problems.push(`data-theme override failed: body background ${bg}`);
await browser.close();
console.log(problems.length ? problems.join("\n") : "no layout problems found");
