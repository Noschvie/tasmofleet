import {
  appendFileSync,
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import { createHash } from "node:crypto";
import { basename, join, resolve } from "node:path";
import type { Page } from "@playwright/test";
import pixelmatch from "pixelmatch";
import { PNG } from "pngjs";
import { root } from "./stack";

// Pixel comparison against the Claude Design prototype (design/v2/README.md,
// section 5). The references live outside the repository (PROTO_REF_DIR, ~150 MB); the repository
// pins them with design/v2/ref-manifest.sha256. Capture mode is the one the references were made
// with (ref/HOWTO.md): device scale factor 1, reduced motion, theme by prefers-color-scheme, the
// viewport grown to the document height for full-page ids and 900 px high for overlay ids.

/**
 * Width of a reference: 1280, 1440 and 2080 desktop, 375 phone. 2080 is only rendered for the main
 * screens (the layout caps content at 1640 px). 0 marks a reference with no width in its name: the
 * 6x crops of the header brand (logo-light.png, logo-dark.png).
 */
export type Width = 1280 | 1440 | 2080 | 375 | 0;
export type Theme = "light" | "dark";
export type Tier = "foundation" | "page" | "structural";

export interface Rect {
  x: number;
  y: number;
  w: number;
  h: number;
}

export const widths: Width[] = [1280, 1440, 2080, 375];
export const themes: Theme[] = ["light", "dark"];

/** baseHeight is the height every capture starts at and the minimum of a full-page capture (HOWTO.md: min 900). */
export const baseHeight = 900;
/** maxHeight is the cap of a full-page capture (HOWTO.md: max 14000, Chromium's screenshot limit is 16384). */
export const maxHeight = 14000;

/** maskColour is painted over every masked rectangle on both images, so differences inside it never count. */
const maskColour = [255, 0, 255, 255] as const;

/**
 * defaultMasks are the values the prototype invents and the app cannot reproduce (plan 5.2): all
 * timestamps (rendered as <time>), device addresses, MACs, uptime, Wi-Fi signal, power and
 * energy, transaction ids and sha256 values. Table cells are found by their data-label (the phone
 * layout prints the same label); anything else opts in with data-visual-mask on the element.
 */
export const defaultMasks: string[] = [
  "time",
  "[data-visual-mask]",
  ...[
    "Address",
    "MAC",
    "Uptime",
    "Wi-Fi",
    "Signal",
    "Power",
    "Energy",
    "Transaction",
    "SHA-256",
  ].map((l) => `td[data-label="${l}"]`),
];

/** tolerances are the plan's 5.3 limits as fractions of the unmasked pixels. */
export function tolerance(tier: Tier, width: Width): number {
  const desktop = width !== 375;
  switch (tier) {
    case "foundation":
      return desktop ? 0.002 : 0.003;
    case "page":
      return desktop ? 0.01 : 0.015;
    case "structural":
      return 0.015;
  }
}

export interface DiffResult {
  /** pixels that differ outside the masks */
  diffPixels: number;
  /** pixels outside the masks */
  unmasked: number;
  /** diffPixels / unmasked */
  ratio: number;
  /** red marks on the actual image, masks in magenta */
  diff: PNG;
}

/**
 * diffImages compares two images of the same size. Masks are painted on copies of both before
 * pixelmatch runs (threshold 0.1, anti-aliasing ignored), so the prototype's invented values and
 * ours can differ inside them.
 */
export function diffImages(
  actual: PNG,
  expected: PNG,
  masks: Rect[] = [],
): DiffResult {
  if (actual.width !== expected.width || actual.height !== expected.height) {
    throw new Error(
      `diffImages needs equal sizes, got ${actual.width}x${actual.height} and ${expected.width}x${expected.height}`,
    );
  }
  const a = paint(actual, masks);
  const e = paint(expected, masks);
  const diff = new PNG({ width: a.width, height: a.height });
  const total = a.width * a.height;
  const maskedPixels = maskedCount(a.width, a.height, masks);
  // Masked pixels are identical on both images, so pixelmatch never counts them.
  const diffPixels = pixelmatch(a.data, e.data, diff.data, a.width, a.height, {
    threshold: 0.1,
    includeAA: false,
  });
  const unmasked = total - maskedPixels;
  return {
    diffPixels,
    unmasked,
    ratio: unmasked > 0 ? diffPixels / unmasked : 0,
    diff,
  };
}

function paint(img: PNG, masks: Rect[]): PNG {
  const out = new PNG({ width: img.width, height: img.height });
  img.data.copy(out.data);
  for (const m of masks) {
    const x0 = Math.max(0, Math.floor(m.x));
    const y0 = Math.max(0, Math.floor(m.y));
    const x1 = Math.min(img.width, Math.ceil(m.x + m.w));
    const y1 = Math.min(img.height, Math.ceil(m.y + m.h));
    for (let y = y0; y < y1; y++) {
      for (let x = x0; x < x1; x++)
        out.data.set(maskColour, (y * img.width + x) * 4);
    }
  }
  return out;
}

function maskedCount(w: number, h: number, masks: Rect[]): number {
  if (masks.length === 0) return 0;
  const seen = new Uint8Array(w * h);
  let n = 0;
  for (const m of masks) {
    const x0 = Math.max(0, Math.floor(m.x));
    const y0 = Math.max(0, Math.floor(m.y));
    const x1 = Math.min(w, Math.ceil(m.x + m.w));
    const y1 = Math.min(h, Math.ceil(m.y + m.h));
    for (let y = y0; y < y1; y++) {
      for (let x = x0; x < x1; x++) {
        if (!seen[y * w + x]) {
          seen[y * w + x] = 1;
          n++;
        }
      }
    }
  }
  return n;
}

/** shifted returns img moved by dx, dy pixels (the vacated edge is white): the "deliberately shifted page" of the tool's self-test. */
export function shifted(img: PNG, dx: number, dy: number): PNG {
  const out = new PNG({ width: img.width, height: img.height });
  out.data.fill(255);
  for (let y = 0; y < img.height; y++) {
    const sy = y - dy;
    if (sy < 0 || sy >= img.height) continue;
    for (let x = 0; x < img.width; x++) {
      const sx = x - dx;
      if (sx < 0 || sx >= img.width) continue;
      img.data.copy(
        out.data,
        (y * img.width + x) * 4,
        (sy * img.width + sx) * 4,
        (sy * img.width + sx) * 4 + 4,
      );
    }
  }
  return out;
}

export function readPng(file: string): PNG {
  return PNG.sync.read(readFileSync(file));
}

/** refName is the file name of a reference: `<id>-<width>-<theme>.png`. */
export function refName(id: string, width: Width, theme: Theme): string {
  return width === 0 ? `${id}-${theme}.png` : `${id}-${width}-${theme}.png`;
}

/** manifestFile pins the reference set (sha256sum format), see design/v2/README.md. */
export const manifestFile = join(root, "design/v2/ref-manifest.sha256");

/** loadManifest returns file name -> sha256 of the reference set. */
export function loadManifest(file = manifestFile): Map<string, string> {
  const m = new Map<string, string>();
  for (const line of readFileSync(file, "utf8").split("\n")) {
    const hit = /^([0-9a-f]{64}) [ *](\S+\.png)$/.exec(line);
    if (hit) m.set(hit[2], hit[1]);
  }
  return m;
}

const refPattern = /^(.+?)(?:-(1280|1440|2080|375))?-(light|dark)\.png$/;

/** manifestIds groups the manifest by id: which widths and themes exist for it. */
export function manifestIds(
  manifest: Map<string, string>,
): Map<string, { width: Width; theme: Theme }[]> {
  const ids = new Map<string, { width: Width; theme: Theme }[]>();
  for (const name of manifest.keys()) {
    const hit = refPattern.exec(name);
    if (!hit)
      throw new Error(`manifest entry ${name} is not <id>-<width>-<theme>.png`);
    const list = ids.get(hit[1]) ?? [];
    list.push({ width: Number(hit[2] ?? 0) as Width, theme: hit[3] as Theme });
    ids.set(hit[1], list);
  }
  return ids;
}

/** refDir is where the reference PNGs are (PROTO_REF_DIR). */
export function refDir(): string {
  const dir = process.env.PROTO_REF_DIR;
  if (!dir)
    throw new Error(
      "PROTO_REF_DIR is not set: point it at the prototype's ref/ directory (make visual sets a default)",
    );
  return resolve(dir);
}

/**
 * verifyReference loads a reference PNG and fails when it is not the pinned one: a re-rendered
 * set must be pinned in the same change (design/v2/ref-manifest.sha256), so a comparison never
 * silently runs against different references.
 */
export function verifyReference(
  name: string,
  dir = refDir(),
  manifest = loadManifest(),
): Buffer {
  const want = manifest.get(name);
  if (!want) throw new Error(`${name} is not in design/v2/ref-manifest.sha256`);
  const file = join(dir, name);
  if (!existsSync(file))
    throw new Error(`reference ${file} is missing (PROTO_REF_DIR=${dir})`);
  const buf = readFileSync(file);
  const have = createHash("sha256").update(buf).digest("hex");
  if (have !== want)
    throw new Error(
      `reference ${name} differs from design/v2/ref-manifest.sha256 (have ${have.slice(0, 12)}, pinned ${want.slice(0, 12)}); pin the new set in the same change`,
    );
  return buf;
}

export interface CompareOptions {
  /** reference id without width and theme, e.g. "devices-colmenu" */
  id: string;
  /** work package that owns the screen, for the report */
  owner?: string;
  width: Width;
  theme: Theme;
  /** "full" grows the viewport to the document height (min 900, max 14000); "overlay" stays 900 px high */
  mode: "full" | "overlay";
  tier: Tier;
  /** CSS selectors of our page whose boxes are masked on both images, in addition to defaultMasks */
  masks?: string[];
  /** replaces the tier's tolerance (a fraction) */
  tolerance?: number;
  /** a height difference is accepted when set (the reason is printed in the report) */
  heightDifference?: string;
  /** the page is the prototype itself (self-test): its height is that of its inner scroller */
  prototype?: boolean;
  /** where actual, expected and diff PNGs and results.jsonl go (default e2e/test-results/visual) */
  outDir?: string;
  /** "shell": compare only the band from the top to the end of the header and crumbs (plan 6, F3) */
  region?: "shell";
  /** an image taken by the caller instead of capture() (the logo crop) */
  actual?: Buffer;
}

export interface CompareResult {
  id: string;
  width: Width;
  theme: Theme;
  /** "error": the page could not be brought into the state (HTTP status, a missing control, a step that does not exist yet) */
  status: "pass" | "fail" | "size" | "error";
  ratio: number;
  diffPixels: number;
  unmasked: number;
  tolerance: number;
  actualSize: string;
  expectedSize: string;
  /** diff over the area both images share, reported when the sizes differ */
  overlapRatio?: number;
  masked: number;
  files: { actual: string; expected: string; diff?: string };
  message: string;
}

export const resultsDir = join(root, "e2e/test-results/visual");

/**
 * settle freezes the page for a capture: web fonts loaded, animations off (the screenshot option
 * does it for CSS), caret hidden, two frames rendered.
 */
async function settle(page: Page): Promise<void> {
  await page.evaluate(() => document.fonts.ready);
  await page.evaluate(
    () =>
      new Promise<void>((ok) =>
        requestAnimationFrame(() => requestAnimationFrame(() => ok())),
      ),
  );
}

/**
 * maskRects returns the boxes (page coordinates) of every element matching the selectors. Boxes
 * are read after the final viewport size is set, so they match the screenshot.
 */
export async function maskRects(
  page: Page,
  selectors: string[],
): Promise<Rect[]> {
  return page.evaluate((sels) => {
    const out: { x: number; y: number; w: number; h: number }[] = [];
    for (const sel of sels) {
      for (const el of document.querySelectorAll(sel)) {
        const b = el.getBoundingClientRect();
        if (b.width > 0 && b.height > 0)
          out.push({
            x: b.left + scrollX,
            y: b.top + scrollY,
            w: b.width,
            h: b.height,
          });
      }
    }
    return out;
  }, selectors);
}

/**
 * capture takes the screenshot the way the references were made. Full-page ids: the page is
 * loaded at 900 px height, then the viewport is set to the document's scrollHeight (min 900, max
 * 14000) and a viewport screenshot is taken, never fullPage: true, because fixed elements (bulk
 * bar, toasts) must sit where they sit in the prototype, which grows its viewport the same way.
 */
export async function capture(
  page: Page,
  width: Width,
  mode: "full" | "overlay",
  prototype = false,
): Promise<Buffer> {
  await page.setViewportSize({ width, height: baseHeight });
  await settle(page);
  if (mode === "full") {
    // The prototype scrolls inside an absolutely positioned container under a 100vh root
    // (HOWTO.md), our pages scroll the document.
    const sh = await page.evaluate((inner) => (inner ? (document.querySelector("[data-theme]")?.firstElementChild?.scrollHeight ?? 0) : document.documentElement.scrollHeight), prototype);
    const h = Math.min(maxHeight, Math.max(baseHeight, sh));
    if (h !== baseHeight) {
      await page.setViewportSize({ width, height: h });
      await settle(page);
    }
  }
  return page.screenshot({
    animations: "disabled",
    caret: "hide",
    fullPage: false,
  });
}

/** shellBand is the height of the shell's band on our page: the header and the crumbs below it. */
async function shellBand(page: Page): Promise<number> {
  return page.evaluate(() => {
    const bottom = (sel: string) => {
      const el = document.querySelector(sel);
      return el ? el.getBoundingClientRect().bottom + scrollY : 0;
    };
    return Math.ceil(Math.max(bottom("header.top"), bottom("main nav.crumbs"), bottom("main .crumb.trail")));
  });
}

/** cropTop keeps the top h rows of a PNG (all of it when it is shorter). */
function cropTop(buf: Buffer, h: number): Buffer {
  const img = PNG.sync.read(buf);
  if (img.height <= h) return buf;
  const out = new PNG({ width: img.width, height: h });
  img.data.copy(out.data, 0, 0, img.width * h * 4);
  return PNG.sync.write(out);
}

/**
 * compareToRef captures the page and compares it with its reference. It never throws for a
 * mismatch: the caller asserts on the result, and every call appends to results.jsonl for the
 * report.
 */
export async function compareToRef(
  page: Page,
  opts: CompareOptions,
): Promise<CompareResult> {
  const out = opts.outDir ?? resultsDir;
  mkdirSync(out, { recursive: true });
  const name = refName(opts.id, opts.width, opts.theme);
  const stem = name.replace(/\.png$/, "");
  const tol = opts.tolerance ?? tolerance(opts.tier, opts.width);
  const expectedBuf = verifyReference(name);
  let actualBuf = opts.actual ?? (await capture(page, opts.width, opts.mode, opts.prototype));
  let expectedBuf2 = expectedBuf;
  if (opts.region === "shell") {
    const band = await shellBand(page);
    actualBuf = cropTop(actualBuf, band);
    expectedBuf2 = cropTop(expectedBuf, band);
  }
  const selectors = [...defaultMasks, ...(opts.masks ?? [])];
  const masks = opts.actual ? [] : await maskRects(page, selectors);
  const files = {
    actual: join(out, `${stem}.actual.png`),
    expected: join(out, `${stem}.expected.png`),
    diff: join(out, `${stem}.diff.png`),
  };
  writeFileSync(files.actual, actualBuf);
  writeFileSync(files.expected, expectedBuf2);
  const result = judge(
    PNG.sync.read(actualBuf),
    PNG.sync.read(expectedBuf2),
    masks,
    {
      id: opts.id,
      width: opts.width,
      theme: opts.theme,
      tolerance: tol,
      heightDifference: opts.heightDifference,
      files,
    },
  );
  if (result.diffImage)
    writeFileSync(files.diff, PNG.sync.write(result.diffImage));
  const { diffImage: _drop, ...row } = result;
  appendFileSync(
    join(out, "results.jsonl"),
    JSON.stringify({ ...row, owner: opts.owner, heightDifference: opts.heightDifference }) + "\n",
  );
  return row;
}

/** judge turns two decoded images into a result; it is the part of compareToRef that needs no browser. */
export function judge(
  actual: PNG,
  expected: PNG,
  masks: Rect[],
  meta: {
    id: string;
    width: Width;
    theme: Theme;
    tolerance: number;
    heightDifference?: string;
    files: CompareResult["files"];
  },
): CompareResult & { diffImage?: PNG } {
  const base = {
    id: meta.id,
    width: meta.width,
    theme: meta.theme,
    tolerance: meta.tolerance,
    actualSize: `${actual.width}x${actual.height}`,
    expectedSize: `${expected.width}x${expected.height}`,
    masked: masks.length,
    files: meta.files,
  };
  const sameWidth = actual.width === expected.width;
  const heightOk =
    actual.height === expected.height ||
    (meta.heightDifference !== undefined && sameWidth);
  if (!sameWidth || !heightOk) {
    // Report the share of differing pixels over the common top-left area anyway: it says whether
    // the page is close (a few rows of height) or far (a different layout).
    const w = Math.min(actual.width, expected.width);
    const h = Math.min(actual.height, expected.height);
    const overlap = diffImages(
      crop(actual, w, h),
      crop(expected, w, h),
      masks.filter((m) => m.x < w && m.y < h),
    );
    return {
      ...base,
      status: "size",
      ratio: 1,
      diffPixels: overlap.diffPixels,
      unmasked: overlap.unmasked,
      overlapRatio: overlap.ratio,
      message: `size ${base.actualSize}, reference ${base.expectedSize} (pixels differ on ${(overlap.ratio * 100).toFixed(2)} % of the common ${w}x${h} area)`,
      diffImage: overlap.diff,
    };
  }
  const h = Math.min(actual.height, expected.height);
  const d = diffImages(
    crop(actual, actual.width, h),
    crop(expected, expected.width, h),
    masks,
  );
  const pass = d.ratio <= meta.tolerance;
  return {
    ...base,
    status: pass ? "pass" : "fail",
    ratio: d.ratio,
    diffPixels: d.diffPixels,
    unmasked: d.unmasked,
    message: `${(d.ratio * 100).toFixed(3)} % differ (${d.diffPixels} of ${d.unmasked} unmasked pixels), limit ${(meta.tolerance * 100).toFixed(1)} %`,
    diffImage: d.diff,
  };
}

function crop(img: PNG, w: number, h: number): PNG {
  if (img.width === w && img.height === h) return img;
  const out = new PNG({ width: w, height: h });
  PNG.bitblt(img, out, 0, 0, w, h, 0, 0);
  return out;
}

/** prototypeBundle is the self-contained prototype export (artifact-*.html plus assets/). It is not in the
 * repository (it bundles a third-party runtime and fonts); PROTO_BUNDLE_DIR names the directory that holds it. */
const bundleDir = process.env.PROTO_BUNDLE_DIR || join(root, "design/v2");
export const prototypeBundle = join(bundleDir, readdirSync(bundleDir).find((f) => /^artifact-.*\.html$/.test(f)) ?? "artifact-missing.html");
/** The component name handed to __dcSetProps is derived from the bundle's file name (HOWTO.md), not from its content. */
const prototypeRoot = basename(prototypeBundle, ".html");

/**
 * openPrototype renders the prototype itself with the props of a reference (ref/HOWTO.md): the
 * props are handed over before the runtime boots, live:false stops its simulation timers, and the
 * "Made with Claude Design" pill is hidden. It is how the tool checks its own capture pipeline: a
 * reference and the same screen captured by capture() must be identical.
 */
export async function openPrototype(page: Page, width: Width, props: Record<string, string | number | boolean>): Promise<void> {
  const all = { live: false, layout: width < 720 ? "phone" : "desktop", ...props };
  await page.addInitScript(
    ({ props, name }) => {
      let f: ((n: string, p: unknown) => void) | undefined;
      Object.defineProperty(window, "__dcSetProps", {
        configurable: true,
        get: () => f,
        set(v) {
          f = v;
          v(name, props);
        },
      });
    },
    { props: all, name: prototypeRoot },
  );
  await page.goto("file://" + prototypeBundle);
  await page.waitForSelector("[data-theme]", { timeout: 15_000 });
  await page.addStyleTag({ content: "#__claude_design_branding{display:none!important} *{caret-color:transparent!important}" });
  await page.evaluate(() => document.fonts.ready);
  await page.waitForTimeout(500);
}

/**
 * recordError writes a result row for a reference whose page could not be captured (it answered
 * with an unexpected HTTP status, a control to click is missing, a step does not exist yet), so the
 * report counts every reference and a page that is simply missing never looks like a pass.
 */
export function recordError(opts: Pick<CompareOptions, "id" | "owner" | "width" | "theme" | "outDir">, message: string): CompareResult {
  const out = opts.outDir ?? resultsDir;
  mkdirSync(out, { recursive: true });
  const row: CompareResult = {
    id: opts.id,
    width: opts.width,
    theme: opts.theme,
    status: "error",
    ratio: 1,
    diffPixels: 0,
    unmasked: 0,
    tolerance: 0,
    actualSize: "",
    expectedSize: "",
    masked: 0,
    files: { actual: "", expected: "" },
    message,
  };
  appendFileSync(join(out, "results.jsonl"), JSON.stringify({ ...row, owner: opts.owner }) + "\n");
  return row;
}
