import { mkdirSync } from "node:fs";
import { join, resolve } from "node:path";
import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// make styleguide-docs (plan D18): one screenshot per /styleguide section, desktop and phone, light
// and dark, into docs/design/styleguide/<section>-<viewport>-<theme>.png, so the style guide is
// readable in the repository without running the app. docs/spec/design-system.md embeds them, and
// styleguide_doc_test.go fails when a section has no doc entry or no screenshot. Not part of CI: the
// images are committed. Rendered like the visual comparison (same flags, reduced motion, no stored
// theme) so a second run writes the same files.
const out = resolve(process.env.STYLEGUIDE_DOCS_DIR ?? join(__dirname, "..", "..", "docs", "design", "styleguide"));
const admin = { "X-authentik-username": "maren", "X-authentik-groups": "adm" };

let app: App;
test.beforeAll(async () => {
  app = await startApp({
    env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32", TASMOFLEET_ADMIN_GROUPS: "adm" },
  });
  mkdirSync(out, { recursive: true });
});
test.afterAll(async () => app.stop());

const viewports = { desktop: { width: 1280, height: 900 }, phone: { width: 375, height: 812 } } as const;

for (const [vp, size] of Object.entries(viewports)) {
  for (const theme of ["light", "dark"] as const) {
    test(`styleguide sections ${vp} ${theme}`, async ({ page }) => {
      await page.setExtraHTTPHeaders(admin);
      await page.setViewportSize(size);
      await page.emulateMedia({ colorScheme: theme, reducedMotion: "reduce" });
      await page.goto(app.url + "/styleguide");
      await page.evaluate(() => document.fonts.ready);
      // the whole page in one viewport (like the visual comparison): no section is scrolled under the
      // sticky header while it is captured
      const height = await page.evaluate(() => document.documentElement.scrollHeight);
      await page.setViewportSize({ width: size.width, height: Math.min(height, 16000) });
      const ids = await page.locator("main section.sg-sec[id^='sg-']").evaluateAll((els) => els.map((e) => e.id));
      expect(ids.length, "the style guide has sections").toBeGreaterThan(10);
      for (const id of ids) {
        await page.locator("#" + id).screenshot({
          path: join(out, `${id.replace(/^sg-/, "")}-${vp}-${theme}.png`),
          animations: "disabled",
          caret: "hide",
        });
      }
    });
  }
}
