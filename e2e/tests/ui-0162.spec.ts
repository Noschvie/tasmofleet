import { test, expect } from "../support/fixtures";
import { expectLayout, expectNoOverflow } from "../support/layout";
import { App, startApp } from "../support/stack";
import { mkdirSync } from "node:fs";

// 0.16.2 UI fixes: #143 (header version label on the brand's line), #131 (policy empty state:
// template cards inside their frame, below the intro). The generic layout guards decide; with
// SHOT_DIR set the pages are also saved as screenshots for a human look.
let app: App;
test.beforeAll(async () => {
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
    },
  });
});
test.afterAll(async () => {
  await app?.stop();
});
test.use({
  extraHTTPHeaders: {
    "X-authentik-username": "adam",
    "X-authentik-groups": "adm",
  },
});

for (const width of [1280, 375]) {
  for (const scheme of ["light", "dark"] as const) {
    for (const [name, path] of [
      ["policy-empty", "/policy"],
      ["dashboard", "/"],
    ] as const) {
      test(`${name} at ${width}px ${scheme}: layout guards and no overflow`, async ({
        page,
      }) => {
        await page.setViewportSize({ width, height: 900 });
        await page.emulateMedia({ colorScheme: scheme });
        await page.goto(app.url + path);
        if (name === "policy-empty")
          await expect(page.locator(".pol-tpl").first()).toBeVisible();
        if (process.env.SHOT_DIR) {
          mkdirSync(process.env.SHOT_DIR, { recursive: true });
          await page.screenshot({
            path: `${process.env.SHOT_DIR}/${name}-${width}-${scheme}.png`,
            fullPage: true,
          });
        }
        await expectNoOverflow(page, name);
        await expectLayout(page, `${name} ${width} ${scheme}`);
      });
    }
  }
}

// #143: the version label shares the vertical centre of the brand name (it sat a few px higher).
test("the header version label is centred on the brand name", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/");
  const centre = async (sel: string) =>
    page
      .locator(sel)
      .first()
      .evaluate((e) => {
        const r = e.getBoundingClientRect();
        return r.top + r.height / 2;
      });
  const brand = await centre("header.top .tb .brand");
  const ver = await centre("header.top .tb .ver");
  expect(Math.abs(brand - ver)).toBeLessThanOrEqual(1);
});
