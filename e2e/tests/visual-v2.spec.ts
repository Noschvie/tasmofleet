import { readFileSync } from "node:fs";
import { test, expect, chromium } from "@playwright/test";
import { entries } from "../support/visual-entries";
import { designAdmin, designOperator } from "../support/seed";
import { compareToRef, loadManifest, manifestIds, recordError, resultsDir, type Theme, type Width } from "../support/visual";
import { isSelected, stackKey, stacksFile } from "../support/visual-select";
import { runSteps } from "../support/visual-steps";

// Pixel comparison of our pages against the Claude Design prototype (plan section 5, WP V0).
// One test per reference: every entry of support/visual-entries.ts x the widths and themes the
// manifest lists for its id. Run with `make visual`; not part of CI (D7), and expected to fail
// for every screen whose work package has not landed yet. The app instances are started once by
// support/visual-global.ts and shared by all workers.
//
//   make visual                        all references
//   make visual SCREENS=P2,device-     entries whose owner or id contains one of the words
//   VISUAL_REGION=shell make visual    only the header and crumbs of every reference, at the
//                                      foundation tolerance (plan section 6, WP F3)

const shellOnly = process.env.VISUAL_REGION === "shell";

const manifest = loadManifest();
const ids = manifestIds(manifest);

interface RunningStack {
  url: string;
  placeholders: Record<string, string>;
}
let published: Record<string, RunningStack> | undefined;
/** stackOf reads what the global setup published, on first use (listing tests needs no running app). */
function stackOf(key: string): RunningStack {
  published ??= JSON.parse(readFileSync(stacksFile, "utf8")) as Record<string, RunningStack>;
  const s = published[key];
  if (!s) throw new Error(`stack ${key} was not started: run through make visual (VISUAL_STACKS=1)`);
  return s;
}

/** resolve fills {d2} / {txn} in a route from what the stack's seed created. */
function resolve(route: string, stack: RunningStack): string {
  return route.replace(/\{(\w+)\}/g, (_, key: string) => {
    const v = stack.placeholders[key];
    if (!v) throw new Error(`route ${route}: no value for {${key}} on its stack`);
    return v;
  });
}

for (const e of entries.filter(isSelected)) {
  const refs = ids.get(e.id);
  if (!refs) throw new Error(`entry ${e.id} has no reference in design/v2/ref-manifest.sha256`);
  for (const { width, theme } of refs.sort((a, b) => b.width - a.width || a.theme.localeCompare(b.theme))) {
    test(`${e.owner} ${e.id} ${width} ${theme}`, async ({ browser }) => {
      const stack = stackOf(stackKey(e));
      if (e.id === "logo") return compareLogo(stack.url + e.route, theme as Theme);
      const headers = e.stack === "local" || e.stack === "setup" ? undefined : e.as === "operator" ? designOperator : designAdmin;
      const context = await browser.newContext({
        viewport: { width: width || 1280, height: 900 },
        deviceScaleFactor: 1,
        colorScheme: theme as Theme,
        reducedMotion: "reduce",
        extraHTTPHeaders: headers,
        ignoreHTTPSErrors: true,
      });
      try {
        const page = await context.newPage();
        const outDir = shellOnly ? resultsDir + "-shell" : undefined; // errors land next to the band's results, which visual-report.mjs reads
        const who = { id: e.id, owner: e.owner, width: width as Width, theme: theme as Theme, outDir };
        try {
          const response = await page.goto(stack.url + resolve(e.route, stack));
          const want = e.status ?? 200;
          if (want !== "any" && response?.status() !== want) throw new Error(`${e.route} answered HTTP ${response?.status()}, the reference is a page that answers ${want}`);
          await runSteps(page, e.steps ?? [], e.owner, stack.placeholders);
        } catch (err) {
          recordError(who, (err as Error).message.split("\n")[0]);
          throw err;
        }
        const result = await compareToRef(page, {
          id: e.id, owner: e.owner, width: width as Width, theme: theme as Theme, mode: e.mode ?? "full", tier: shellOnly ? "foundation" : e.tier ?? "page", masks: e.masks,
          ...(shellOnly ? { region: "shell" as const, outDir } : {}),
        });
        expect(result.status, `${e.id} ${width} ${theme}: ${result.message}`).toBe("pass");
      } finally {
        await context.close();
      }
    });
  }
}

/**
 * compareLogo checks the header brand against logo-light/-dark.png: 6x crops of the prototype's
 * brand (133 x 42 CSS px, the brand's box plus 10 px on every side) at 1280 px. The references
 * were taken by a browser without the visual project's font flags, so this one is launched the
 * same way (probed: that recipe reproduces the reference at 0 pixels).
 */
async function compareLogo(url: string, theme: Theme): Promise<void> {
  const plain = await chromium.launch({ args: [] }); // an explicit empty list: the project's font flags would otherwise be applied
  try {
    const context = await plain.newContext({ viewport: { width: 1280, height: 900 }, deviceScaleFactor: 6, colorScheme: theme, reducedMotion: "reduce", extraHTTPHeaders: designAdmin });
    const page = await context.newPage();
    await page.goto(url);
    await page.evaluate(() => document.fonts.ready);
    const box = await page.locator("header .brand").boundingBox();
    if (!box) throw new Error("no brand in the top bar");
    const shot = await page.screenshot({ clip: { x: box.x - 10, y: box.y - 10, width: 133, height: 42 }, animations: "disabled", caret: "hide" });
    const result = await compareToRef(page, { id: "logo", owner: "F3", width: 0, theme, mode: "overlay", tier: "foundation", actual: shot });
    expect(result.status, `logo ${theme}: ${result.message}`).toBe("pass");
  } finally {
    await plain.close();
  }
}
