import { test, expect } from "@playwright/test";
import { compareToRef, openPrototype, tolerance, verifyReference, loadManifest, refName, type Theme, type Width } from "../support/visual";

// Self-test of the pixel tool against the real references (make visual runs it first). Why: a
// harness that can only fail is as useless as one that cannot. The prototype itself, rendered
// through the tool's own capture pipeline (same flags, viewport growth, fonts), has to reproduce
// its reference exactly; the same render shifted by a few pixels has to fail the page tolerance.
// If the first half fails, every later page comparison is meaningless (wrong Chromium, fonts,
// viewport handling), so fix the tool before reading any page result.

const cases: { id: string; width: Width; theme: Theme; props: Record<string, string> }[] = [
  { id: "dashboard", width: 1280, theme: "light", props: { screen: "dashboard" } },
  { id: "devices", width: 375, theme: "dark", props: { screen: "devices" } },
  { id: "device-console", width: 1440, theme: "light", props: { screen: "device", deviceId: "d2", deviceTab: "console" } },
  { id: "devices", width: 2080, theme: "dark", props: { screen: "devices" } },
];

test.describe("tool self-test", () => {
  for (const c of cases) {
    test(`prototype render equals its reference: ${c.id} ${c.width} ${c.theme}`, async ({ page }) => {
      await openPrototype(page, c.width, { ...c.props, theme: c.theme });
      const r = await compareToRef(page, { id: c.id, width: c.width, theme: c.theme, mode: "full", tier: "foundation", tolerance: 0, prototype: true, outDir: "test-results/visual-selftest" });
      expect(r.message).toContain("0.000 %");
      expect(r.status).toBe("pass");
    });

    test(`a page shifted by 6 px fails: ${c.id} ${c.width} ${c.theme}`, async ({ page }) => {
      await openPrototype(page, c.width, { ...c.props, theme: c.theme });
      // The page moves right and down inside its viewport, the way a changed gutter or header height would.
      await page.addStyleTag({ content: "[data-theme] > :first-child { left: 6px !important; top: 6px !important; }" });
      const r = await compareToRef(page, { id: c.id, width: c.width, theme: c.theme, mode: "full", tier: "page", prototype: true, outDir: "test-results/visual-selftest" });
      expect(r.status, r.message).not.toBe("pass");
      expect(r.ratio).toBeGreaterThan(tolerance("page", c.width));
    });
  }

  test("a changed reference is refused", () => {
    const name = refName("dashboard", 1280, "light");
    const manifest = loadManifest();
    manifest.set(name, "0".repeat(64));
    expect(() => verifyReference(name, undefined, manifest)).toThrow(/differs from design\/v2\/ref-manifest.sha256/);
  });
});
