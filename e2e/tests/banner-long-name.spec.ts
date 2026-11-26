import { test, expect } from "../support/fixtures";
import { expectNoOverflow } from "../support/layout";
import { App, startApp } from "../support/stack";

// Why: a banner that names devices (the Run now note of #989 lists the devices the cooldown held back)
// meets the longest unbreakable token the UI ever shows, a device name with no break opportunity
// (see long-name-overflow.spec.ts). Without a wrap rule on .banner-text that name widens the banner and
// with it the tab strip above it, and the whole page scrolls sideways at 375 px (97 px in the final
// gates). The note only appears while a device is held back, which a spec cannot provoke on demand
// (Run now itself is rate limited to once a minute), so the markup is built here from the same
// classes and measured against the real stylesheet of the page.
const LONG = "AtticHeaterPlugWithAReallyLongDeviceNameThatNeverHasABreakAtAll";
let app: App;
test.beforeAll(async () => {
  app = await startApp({ env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32", TASMOFLEET_ADMIN_GROUPS: "adm" } });
});
test.afterAll(async () => {
  await app?.stop();
});
test.use({ extraHTTPHeaders: { "X-authentik-username": "adam", "X-authentik-groups": "adm" } });

test("a banner that names a device with an unbreakable name wraps inside the page at 375 px", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 800 });
  await page.goto(app.url + "/policy");
  await expectNoOverflow(page, "policy before the note");
  await page.evaluate((name) => {
    const note = document.createElement("div");
    note.className = "banner t-accent";
    note.setAttribute("role", "status");
    note.innerHTML = `<span class="banner-dot"></span><div class="banner-text">Read less than 5 minutes ago, not read again: ${name} (from 12:40), ${name} (from 12:41)</div>`;
    (document.querySelector("main") ?? document.body).prepend(note);
  }, LONG);
  await expectNoOverflow(page, "policy with a cooldown note naming " + LONG);
});
