import { test, expect } from "../support/fixtures";
import { expectLayout, expectNoOverflow } from "../support/layout";
import { App, startApp } from "../support/stack";

// #510: a section badge ("Upgrade notes: action may be required" in 0.17.0) must wrap inside its own
// column instead of running over the first item of the list, and a path in code breaks after a "/"
// instead of inside a word ("tasmofl / eet"). The generic layout guards judge the whole page; the
// explicit check below names the case, so a failure says what broke.
let app: App;
test.beforeAll(async () => {
  app = await startApp({ env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32", TASMOFLEET_ADMIN_GROUPS: "adm" } });
});
test.afterAll(async () => {
  await app?.stop();
});
test.use({ extraHTTPHeaders: { "X-authentik-username": "adam", "X-authentik-groups": "adm" } });

for (const width of [1280, 375]) {
  for (const scheme of ["light", "dark"] as const) {
    test(`What's new at ${width}px ${scheme}: badges stay in their column, code breaks at slashes`, async ({ page }) => {
      await page.setViewportSize({ width, height: 900 });
      await page.emulateMedia({ colorScheme: scheme });
      await page.goto(app.url + "/whats-new");
      await expectNoOverflow(page, "whats-new");
      await expectLayout(page, `whats-new ${width} ${scheme}`);
      // every section badge ends before the list of its section starts
      const overlaps = await page.evaluate(() =>
        [...document.querySelectorAll(".wn-sec")].flatMap((sec) => {
          const b = sec.querySelector(".wn-lbl .bdg")!.getBoundingClientRect();
          const l = sec.querySelector("ul")!.getBoundingClientRect();
          const text = sec.querySelector(".wn-lbl .bdg")!.textContent;
          return b.right > l.left + 0.5 ? [`${text}: badge ends at ${Math.round(b.right)}, list starts at ${Math.round(l.left)}`] : [];
        }),
      );
      expect(overlaps).toEqual([]);
      // a code span never shows a word cut across lines: a path breaks after "/" or "-" or "_"
      const cut = await page.evaluate(() =>
        [...document.querySelectorAll(".wn-sec code")].flatMap((c) => {
          const out: string[] = [];
          const w = document.createTreeWalker(c, NodeFilter.SHOW_TEXT);
          for (let n = w.nextNode(); n; n = w.nextNode()) {
            for (const m of (n.textContent ?? "").matchAll(/[\p{L}\p{N}]{4,}/gu)) {
              const r = document.createRange();
              r.setStart(n, m.index!);
              r.setEnd(n, m.index! + m[0].length);
              const lines = new Set([...r.getClientRects()].filter((q) => q.width > 0).map((q) => Math.round(q.top / 4)));
              if (lines.size > 1) out.push(m[0]);
            }
          }
          return out;
        }),
      );
      expect(cut, "words of inline code cut across two lines").toEqual([]);
    });
  }
}
