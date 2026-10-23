import { execFileSync } from "node:child_process";
import { test, expect } from "@playwright/test";
import { PNG } from "pngjs";
import { diffImages, judge, loadManifest, manifestIds, shifted, tolerance, type Rect } from "../support/visual";
import { entries, excluded } from "../support/visual-entries";
import { root } from "../support/stack";

// Tests of the pixel tool that need neither the references (they are outside the repository) nor
// a running app, so CI runs them: the comparison logic, the entry table's coverage of the
// reference manifest, and the design fixture's freshness. The comparison against the real
// references is `make visual` (visual-selftest.spec.ts and visual-v2.spec.ts).

/** page draws a synthetic screen: white, a header band, text-like blocks and a few timestamps. */
function page(w = 400, h = 600): PNG {
  const img = new PNG({ width: w, height: h });
  img.data.fill(255);
  const fill = (x0: number, y0: number, x1: number, y1: number, v: number) => {
    for (let y = y0; y < y1; y++) for (let x = x0; x < x1; x++) img.data.set([v, v, v, 255], (y * w + x) * 4);
  };
  fill(0, 0, w, 48, 30);
  for (let row = 0; row < 12; row++) fill(16, 72 + row * 40, 16 + 120 + (row % 4) * 60, 72 + row * 40 + 14, 90);
  return img;
}

test.describe("comparison logic", () => {
  test("identical images differ in no pixel", () => {
    const r = diffImages(page(), page());
    expect(r.diffPixels).toBe(0);
    expect(r.ratio).toBe(0);
  });

  test("a page moved by a few pixels exceeds the page tolerance (the tool can fail)", () => {
    // Why: a layout shift of one gutter is exactly what the port must be caught on.
    const r = diffImages(shifted(page(), 4, 4), page());
    expect(r.ratio).toBeGreaterThan(tolerance("page", 1280));
  });

  test("masked areas do not count, on either side, and leave out of the denominator", () => {
    // Why: the prototype's invented timestamps and ours differ by design (plan 5.2).
    const a = page();
    const b = page();
    const mask: Rect = { x: 16, y: 72, w: 200, h: 14 };
    for (let y = 72; y < 86; y++) for (let x = 16; x < 216; x++) b.data.set([200, 0, 0, 255], (y * 400 + x) * 4);
    expect(diffImages(a, b).diffPixels).toBeGreaterThan(0);
    const masked = diffImages(a, b, [mask]);
    expect(masked.diffPixels).toBe(0);
    expect(masked.unmasked).toBe(400 * 600 - 200 * 14);
  });

  test("a difference next to a mask still counts", () => {
    const a = page();
    const b = page();
    for (let y = 72; y < 86; y++) b.data.set([200, 0, 0, 255], (y * 400 + 300) * 4);
    expect(diffImages(a, b, [{ x: 16, y: 72, w: 200, h: 14 }]).diffPixels).toBeGreaterThan(0);
  });

  const meta = { id: "x", width: 1280 as const, theme: "light" as const, tolerance: 0.01, files: { actual: "a", expected: "e" } };

  test("a different size fails even when every shared pixel is equal", () => {
    // Why: plan 5.3, height difference 0 px: a page that is 3 px too tall has shifted everything below it.
    const r = judge(page(400, 603), page(400, 600), [], meta);
    expect(r.status).toBe("size");
    expect(r.message).toContain("400x603");
    expect(r.message).toContain("400x600");
  });

  test("a documented height difference compares the shared rows", () => {
    const r = judge(page(400, 603), page(400, 600), [], { ...meta, heightDifference: "prototype pads the last card" });
    expect(r.status).toBe("pass");
  });

  test("the diff ratio is judged against the tolerance of the tier", () => {
    const a = page();
    const b = page();
    for (let y = 100; y < 160; y++) for (let x = 100; x < 200; x++) b.data.set([200, 0, 0, 255], (y * 400 + x) * 4); // 6000 of 240000 px = 2.5 %
    expect(judge(a, b, [], { ...meta, tolerance: 0.01 }).status).toBe("fail");
    expect(judge(a, b, [], { ...meta, tolerance: 0.03 }).status).toBe("pass");
  });

  test("tolerances follow plan 5.3", () => {
    expect([tolerance("foundation", 1280), tolerance("foundation", 375), tolerance("page", 1440), tolerance("page", 375), tolerance("structural", 1280)]).toEqual([0.002, 0.003, 0.01, 0.015, 0.015]);
  });
});

test.describe("entry table", () => {
  const manifest = loadManifest();
  const ids = manifestIds(manifest);

  test("the manifest pins the 1312 references of the v6 render", () => {
    expect(manifest.size).toBe(1312);
  });

  test("every reference id has an entry, except the loading skeletons (D12)", () => {
    // Why: a screen without an entry would never be compared and the port could drift unnoticed.
    const known = new Set(entries.map((e) => e.id));
    expect([...ids.keys()].filter((id) => !known.has(id) && !excluded(id))).toEqual([]);
    expect([...ids.keys()].filter((id) => excluded(id) && known.has(id))).toEqual([]);
  });

  test("every entry points at a reference, once", () => {
    const seen = new Set<string>();
    for (const e of entries) {
      expect(ids.has(e.id), `${e.id} is not in the manifest`).toBe(true);
      expect(seen.has(e.id), `${e.id} is listed twice`).toBe(false);
      seen.add(e.id);
    }
  });

  test("routes are absolute paths and placeholders are device ids, {txn}, {backup} or the setup stack's {setuptoken}", () => {
    // Why: an unknown placeholder resolves to "" and silently captures the wrong page; only the
    // setup stack knows the setup link's token (N12).
    for (const e of entries) {
      expect(e.route.startsWith("/"), e.id).toBe(true);
      for (const [, key] of e.route.matchAll(/\{(\w+)\}/g)) {
        if (key === "setuptoken") expect(e.stack, `${e.id}: {setuptoken} outside the setup stack`).toBe("setup");
        else expect(key, `${e.id}: {${key}}`).toMatch(/^(d[1-9]\d?|txn|backup)$/);
      }
    }
  });

  test("overlay entries are the dialogs, toasts and results the references capture 900 px high", () => {
    // Why: a dialog is anchored to the viewport; a grown viewport would move it (plan 5.1).
    for (const e of entries) {
      const overlay = /dialog|toast|makecompliant-result/.test(e.id);
      expect(e.mode === "overlay", `${e.id}: mode`).toBe(overlay);
    }
  });

  test("owners are work packages of the plan", () => {
    const known = /^(F[1-5]|P([1-9]|10)|N([1-9]|1[0-9]|2[01])|V0|Z1)$/;
    for (const e of entries) expect(e.owner, e.id).toMatch(known);
  });
});

test("the design fixture is what design/tools/v2-fixture.mjs generates from the prototype", () => {
  // Why: the fixture must not drift from design/v2/template.html (plan 5.2); regenerate instead of editing it.
  expect(() => execFileSync("node", ["design/tools/v2-fixture.mjs", "--check"], { cwd: root, stdio: "pipe" })).not.toThrow();
});
