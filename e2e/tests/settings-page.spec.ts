import { mkdirSync } from "node:fs";
import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";
import { openSchedule } from "../support/nav";
import { hitBox } from "../support/target";

// The Settings page (issue #275): eight tabs in one form, a card grid, info popovers, one save bar.
// Header authentication: header principals cannot confirm, so only settings without a step-up are
// changed here (the step-up paths are unit tested).
let app: App;
test.beforeAll(async () => {
  app = await startApp({
    env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32", TASMOFLEET_ADMIN_GROUPS: "adm" },
  });
});
test.afterAll(async () => {
  await app?.stop();
});
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
const tabs = (page: import("@playwright/test").Page) => page.getByRole("navigation", { name: "Settings sections" });

// Why: a person who edits one tab and looks at another must not lose the edit, and must be told
// which tabs hold unsaved changes. One Save then writes every tab.
test("edits survive a tab change, the bar lists the tabs with changes, one Save writes them all", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=general");
  const bar = page.locator("[data-sys-bar]");
  await expect(bar).toBeHidden(); // nothing edited yet (#990)

  await page.locator("[data-key='METRICS_DEVICE_LABELS'] .swtrack").click();
  await expect(bar).toContainText("Unsaved changes:");
  await tabs(page).getByRole("link", { name: "Firmware & updates" }).click();
  await expect(page).toHaveURL(/tab=firmware/);
  await page.locator("input[name='v.FIRMWARE_KEEP_RELEASES']").fill("7");
  await expect(bar.locator(".sbar-chip")).toHaveText(["General", "Firmware & updates"]);
  await expect(tabs(page).getByRole("link", { name: "General" })).toHaveClass(/is-dirty/);

  await tabs(page).getByRole("link", { name: "General" }).click();
  await expect(page.locator("input[name='v.METRICS_DEVICE_LABELS']")).toBeChecked(); // still there

  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Saved.")).toBeVisible();
  await page.goto(app.url + "/settings/system?tab=firmware");
  await expect(page.locator("input[name='v.FIRMWARE_KEEP_RELEASES']")).toHaveValue("7");
  await page.goto(app.url + "/settings/system?tab=general");
  await expect(page.locator("input[name='v.METRICS_DEVICE_LABELS']")).toBeChecked();
  await expect(page.locator("[data-sys-bar]")).toBeHidden();
});

// Why: touch and keyboard users cannot hover. The info button opens a popover; Escape closes it;
// opening another one closes the first.
test("info popovers open on click, close on Escape and show one at a time", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=network");
  const first = page.getByRole("button", { name: "About Device networks" });
  const second = page.getByRole("button", { name: "About Telemetry period" });
  await first.click();
  await expect(page.locator("#info-DEVICE_NETWORKS")).toBeVisible();
  await expect(first).toHaveAttribute("aria-expanded", "true");
  await second.click();
  await expect(page.locator("#info-TELEPERIOD")).toBeVisible();
  await expect(page.locator("#info-DEVICE_NETWORKS")).toBeHidden();
  await page.keyboard.press("Escape");
  await expect(page.locator("#info-TELEPERIOD")).toBeHidden();
  await expect(second).toHaveAttribute("aria-expanded", "false");
});

// Why (#1075): a 16 px info button and a 15 px "Reset to default" are hard to hit with a finger or a tremor
// (WCAG 2.5.8: 24 x 24). The drawing stays small; what a pointer hits must not. hitBox probes the points
// around the control with elementFromPoint, so the transparent hit area counts and a 16 px box does not.
test("every info button and reset control on the settings pages is a 24 x 24 px target at 1280", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.setExtraHTTPHeaders(admin);
  // stored values that differ from their defaults, so the reset controls show (two narrow fields on Backups; no
  // other test of this file stores those, and this file's tests run one after the other against one app)
  await page.goto(app.url + "/devices/backups");
  await openSchedule(page);
  await page.locator("input[name='v.BACKUP_KEEP']").fill("5");
  await page.locator("input[name='v.BACKUP_MAX_AGE']").fill("48h");
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.waitForLoadState("load");

  let infos = 0;
  let resets = 0;
  const missed: string[] = [];
  try {
    await page.goto(app.url + "/settings/system?tab=general");
    const hrefs = await tabs(page).getByRole("link").evaluateAll((a) => a.map((l) => (l as HTMLAnchorElement).getAttribute("href")!));
    expect(hrefs.length).toBeGreaterThanOrEqual(7);
    const pages = [...hrefs, "/devices/backups"];
    for (const href of pages) {
      await page.goto(app.url + href);
      if (href === "/devices/backups") await openSchedule(page);
      for (const sel of [".uinfo", "label.srst"]) {
        const all = page.locator(sel);
        for (let i = 0; i < (await all.count()); i++) {
          const el = all.nth(i);
          if (!(await el.isVisible())) continue;
          if (await el.evaluate((e) => e.closest("[inert]") !== null)) continue; // a switched-off region takes no pointer at all
          if (sel === ".uinfo") infos++;
          else resets++;
          for (const m of await hitBox(el)) missed.push(`${href}: ${m}`);
        }
      }
    }
  } finally {
    // put the stored values back: the file's other tests expect the Backups fields untouched (one app, tests run serially)
    await page.goto(app.url + "/devices/backups");
    await openSchedule(page);
    await page.locator("[data-key='BACKUP_KEEP'] label.srst").click();
    await page.locator("[data-key='BACKUP_MAX_AGE'] label.srst").click();
    await page.getByRole("button", { name: "Save settings" }).click();
    await page.waitForLoadState("load");
    await page.goto(app.url + "/devices/backups");
    await expect(page.locator("input[name='clear.BACKUP_KEEP']")).toHaveCount(0);
  }

  expect(infos, "the pages show info buttons").toBeGreaterThan(5);
  expect(resets, "the stored values show reset controls").toBeGreaterThanOrEqual(2);
  expect(missed.slice(0, 6).concat(missed.length > 6 ? [`${missed.length} misses in all`] : [])).toEqual([]);
});

// Why (#1075): the info button's hit box reaches past its drawing, and a click there must do what a click on
// the drawing does (open the popover), not fall through to the label.
test("a click on the edge of an info button's hit box opens its popover", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=network");
  const btn = page.getByRole("button", { name: "About Device networks" });
  const r = (await btn.boundingBox())!;
  await page.mouse.click(r.x - 3, r.y - 3); // 3 px outside the drawn 16 px box, inside the 24 px hit box
  await expect(page.locator("#info-DEVICE_NETWORKS")).toBeVisible();
  await expect(btn).toHaveAttribute("aria-expanded", "true");
});

// Why: scan interval and networks do nothing while the scan is off; they are dimmed (and not
// focusable) until the switch is on.
test("dependent settings follow their switch", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=discovery");
  const dep = page.locator("[data-depends='v.DISCOVERY_SCAN']");
  await expect(dep).toHaveClass(/is-off/);
  await page.locator("[data-key='DISCOVERY_SCAN'] .swtrack").click();
  await expect(dep).not.toHaveClass(/is-off/);
  await dep.locator("input[name='v.DISCOVERY_SCAN_INTERVAL']").fill("2h");

});

// Why (#334): a field that depends on a switch is an ordinary field of the card: no indent, no rule.
test("dependent settings are not drawn as an indented block", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=discovery");
  const dep = page.locator("[data-depends='v.DISCOVERY_SCAN']");
  const css = await dep.evaluate((el) => { const c = getComputedStyle(el); return [c.borderLeftWidth, c.marginLeft, c.paddingLeft]; });
  expect(css).toEqual(["0px", "0px", "0px"]);
  const box = await dep.boundingBox();
  const card = await page.locator("#card-scan .sbody").boundingBox();
  expect(Math.abs(box!.x - card!.x)).toBeLessThan(1);
});

// Why (#335): the Discovery tab holds both ways of finding devices; the old tab=scan opens the first tab.
test("Discovery tab holds MQTT discovery and the network scan; tab=scan opens General", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=discovery");
  await expect(tabs(page).getByRole("link", { name: "Discovery", exact: true })).toHaveAttribute("aria-current", "page");
  await expect(page.locator("#card-mqttdiscovery")).toBeVisible();
  await expect(page.locator("#card-scan")).toBeVisible();
  await page.locator("#card-scan").getByRole("link", { name: "device networks" }).click();
  await expect(tabs(page).getByRole("link", { name: /Network/ })).toHaveAttribute("aria-current", "page");
  await expect(page.locator("#card-networks")).toBeVisible();
  await page.goto(app.url + "/settings/system?tab=scan");
  await expect(tabs(page).getByRole("link", { name: "General" })).toHaveAttribute("aria-current", "page");
});

// Why (#333): "keep the stored password" only matters while host or port differ from the stored
// value, so it shows up then and not before. The e2e stack stores no password, so the option is
// added to the served page.
test("the keep-password option appears only when host or port changed", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.route("**/settings/system?tab=mqtt", async (route) => {
    const res = await route.fetch();
    const html = (await res.text()).replace('<div class="sf w-tls', '</div><label class="ucheck sopt bkeep" data-broker-keep data-stored-host="" data-stored-port=""><input type="checkbox" name="reuse.MQTT_PASSWORD" value="1"/><span>Broker changed: keep the stored password for the new broker</span></label><div class="brow"><div class="sf w-tls');
    await route.fulfill({ response: res, body: html });
  });
  await page.goto(app.url + "/settings/system?tab=mqtt");
  const keep = page.locator("[data-broker-keep]");
  await expect(keep).toBeHidden();
  await page.locator("input[name='mqtt.port']").fill("1999");
  await expect(keep).toBeVisible();
  await page.locator("input[name='mqtt.port']").fill("");
  await expect(keep).toBeHidden();
  await page.locator("input[name='mqtt.host']").fill("other.example");
  await expect(keep).toBeVisible();
});

// Why (review of #333): after a refused save the page shows the new broker; the keep option must
// still be there, still ticked (the server wants a decision about the stored password), and
// compared with the SAVED broker, not with the broker the page was loaded with.
test("keep-password option survives a refused save with a new broker", async ({ page }) => {
  const own = await startApp({
    env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32", TASMOFLEET_ADMIN_GROUPS: "adm" },
  });
  try {
    await page.setExtraHTTPHeaders(admin);
    await page.goto(own.url + "/settings/system?tab=mqtt");
    await page.locator("input[name='mqtt.host']").fill("127.0.0.1");
    await page.locator("input[name='mqtt.port']").fill("1883");
    await page.locator("input[name='v.MQTT_PASSWORD']").fill("fake-e2e-password");
    await page.getByRole("button", { name: "Save settings" }).click();
    await expect(page.getByText("Saved.")).toBeVisible();
    const keep = page.locator("[data-broker-keep]");
    await expect(keep).toBeHidden(); // the broker is the saved one
    await page.locator("input[name='mqtt.host']").fill("other.example");
    await expect(keep).toBeVisible();
    await keep.locator("input").check();
    await page.locator("[data-sys-tab='firmware'] input[name='v.FIRMWARE_KEEP_RELEASES']").evaluate((el) => { (el as HTMLInputElement).type = "text"; (el as HTMLInputElement).value = "not-a-number"; });
    await page.getByRole("button", { name: "Save settings" }).click();
    await expect(page.locator("[data-key='FIRMWARE_KEEP_RELEASES'] .ufield-err")).toBeVisible();
    await tabs(page).getByRole("link", { name: "MQTT" }).click(); // the page opens the tab with the error
    await expect(page.locator("input[name='mqtt.host']")).toHaveValue("other.example");
    await expect(keep).toBeVisible();
    await expect(keep.locator("input")).toBeChecked();
    // going back to the saved broker is what hides it
    await page.locator("input[name='mqtt.host']").fill("127.0.0.1");
    await expect(keep).toBeVisible(); // ticked in the refused save: never hidden by the page
  } finally {
    await own.stop();
  }
});

// Why (#409): the Backups tab moved to Devices -> Backups; Settings has eight tabs and no Backups.
test("Settings has no Backups tab any more", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=general");
  await expect(tabs(page).getByRole("link", { name: "Backups" })).toHaveCount(0);
  await expect(page.locator("#card-storage")).toHaveCount(0);
  await page.goto(app.url + "/settings/system?tab=backups");
  await expect(tabs(page).locator("[aria-current=page]")).toHaveText("General");
});

// Why (#337): a page that was just loaded has no unsaved changes, on any tab.
test("a freshly loaded page reports no unsaved changes on any tab", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  for (const tab of ["general", "network", "mqtt", "discovery", "firmware", "signin", "advanced"]) {
    await page.goto(app.url + "/settings/system?tab=" + tab);
    await expect(page.locator("[data-sys-bar]"), tab).toBeHidden(); // the bar shows only for an edit (#990)
  }
});

// Why (#601): ticking a backup day makes the Backups tab dirty, unticking it again leaves it clean.
test("a backup day ticked and unticked again leaves the tab clean", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices/backups");
  await openSchedule(page);
  const bar = page.locator("[data-sys-bar]");
  const mon = page.locator("input[name='v.BACKUP_DAYS'][value='mon']");
  await mon.check();
  await expect(bar).toContainText("Unsaved changes:");
  await mon.uncheck();
  await expect(bar).toBeHidden();
});

// Why (#336, #338): "Kept per device" and "Never prune younger than" sit side by side as small
// inputs; the label stays on one line with its info icon right behind it, and the deleted devices' card
// and the schedule card (#410, #684: the schedule is the last card of the narrow column) stack without a gap.
test("backups: schedule fields side by side, label and icon together, narrow cards without a gap", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/devices/backups");
  await openSchedule(page);
  const keep = page.locator("[data-key='BACKUP_KEEP']");
  const prune = page.locator("[data-key='BACKUP_MAX_AGE']");
  const k = (await keep.boundingBox())!, p = (await prune.boundingBox())!;
  expect(Math.abs(k.y - p.y)).toBeLessThan(2);
  expect((await prune.locator("input").boundingBox())!.width).toBeLessThan(140);
  const lbl = (await prune.locator(".lbl").boundingBox())!, info = (await prune.locator(".uinfo").boundingBox())!;
  expect(lbl.height).toBeLessThan(24);
  expect(info.x - (lbl.x + lbl.width)).toBeLessThan(12);
  const a = (await page.locator("#card-orphans").boundingBox())!, b = (await page.locator("#card-schedule").boundingBox())!;
  expect(b.y - (a.y + a.height)).toBeLessThan(24);
  expect(Math.abs(b.x - a.x), "same column").toBeLessThan(2);
  expect((await page.locator("#card-devices").boundingBox())!.x, "the device table keeps the wide column").toBeLessThan(a.x);
});

// Why (#601): the schedule is weekday chips and a time of day. The default is manual only (no chip
// on); ticked days and the time save and come back; unticking every day saves manual only again.
test("backup days and time save, and no day ticked is manual only", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices/backups");
  await openSchedule(page);
  const day = (d: string) => page.locator(`input[name='v.BACKUP_DAYS'][value='${d}']`);
  await expect(day("mon")).not.toBeChecked(); // default: manual only
  await day("mon").check();
  await day("wed").check();
  await page.locator("input[name='v.BACKUP_TIME']").fill("04:30");
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Saved.")).toBeVisible();
  await expect(day("mon")).toBeChecked();
  await expect(day("tue")).not.toBeChecked();
  await expect(day("wed")).toBeChecked();
  await expect(page.locator("input[name='v.BACKUP_TIME']")).toHaveValue("04:30");
  await expect(page.locator("#card-lastrun")).toContainText("Mon, Wed at 04:30");
  await openSchedule(page); // the page loaded again after the save, the fold is closed
  await day("mon").uncheck();
  await day("wed").uncheck();
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Saved.")).toBeVisible();
  await expect(day("mon")).not.toBeChecked();
  await expect(day("wed")).not.toBeChecked();
  await expect(page.locator("#card-lastrun")).toContainText("No schedule");
});

// Why: on a phone eight tabs scroll sideways; a picker replaces the strip, the cards lose their
// frame, nothing overflows, and Save is in a bar that stays in view.
test("phone: section picker, no horizontal overflow, save bar in view", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto(app.url + "/settings/system?tab=signin");
  await expect(page.locator("[data-sys-picker]")).toBeVisible();
  await expect(tabs(page)).toBeHidden();
  await page.locator("[data-sys-picker] select").selectOption("mqtt");
  await expect(page.locator("#sec-mqtt")).toBeVisible();
  await expect(page.locator("#sec-signin")).toBeHidden();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  // the bar shows with the first edit (#990), and Save is in view
  const save = page.getByRole("button", { name: "Save settings" });
  await expect(save).toBeHidden();
  await page.locator("input[name='mqtt.port']").fill("1999");
  await expect(save).toBeInViewport();
});

// Why (#1087): behind a proxy the mode is fixed by the environment, so the page has no mode control for the script to
// read. The script used to switch the identity provider card on whenever it found no control, so the card that does
// nothing in this mode was drawn live, and the field above it said "header". The server now draws the card inactive
// and names the mode; the roles card stays live because the proxy's groups give the roles in this mode.
test("sign-in tab behind a proxy: the identity provider card is inactive, the mode is named", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=signin");
  await expect(page.locator("[data-key='AUTH_MODE']")).toContainText("Sign-in is handled by your proxy (header)");
  await expect(page.locator("#card-idp")).toHaveClass(/is-off/);
  await expect(page.locator("#card-idp")).toHaveAttribute("inert", "");
  await expect(page.locator("#card-roles")).not.toHaveClass(/is-off/);
  await expect(page.locator("#card-mode")).not.toContainText("Asks you to confirm");
});

// Local accounts (the default): the Sign-in tab shows the identity provider cards, which wait for
// single sign-on to be selected.
test.describe("with local accounts", () => {
  let local: App;
  test.beforeAll(async () => {
    local = await startApp({ env: { TASMOFLEET_ALLOW_INSECURE_HTTP: "true" } });
    local.cli(["user", "passwd", "--create", "--role", "admin", "settings-admin"], "e2e-fake-password-1\n");
  });
  test.afterAll(async () => {
    await local?.stop();
  });
  async function login(page: import("@playwright/test").Page) {
    await page.setExtraHTTPHeaders({});
    await page.goto(local.url + "/login");
    await page.getByLabel("Username").fill("settings-admin");
    await page.getByLabel("Password").fill("e2e-fake-password-1");
    await page.getByRole("button", { name: "Sign in" }).click();
    await expect(page.getByRole("heading", { name: "Fleet overview" })).toBeVisible();
  }

  // Why: the identity provider does nothing in local mode, so its cards are dimmed until single
  // sign-on is chosen; the choice itself counts as an unsaved change of the Sign-in tab.
  test("the identity provider cards wait for single sign-on", async ({ page }) => {
    await login(page);
    await page.goto(local.url + "/settings/system?tab=signin");
    await expect(page.locator("#card-idp")).toHaveClass(/is-off/);
    await expect(page.locator("#card-roles")).toHaveClass(/is-off/);
    await expect(page.locator("#card-mode")).toContainText("In use now: Local accounts.");
    await page.locator("[data-key='AUTH_MODE'] label.rcard", { hasText: "Single sign-on" }).click();
    await expect(page.locator("#card-idp")).not.toHaveClass(/is-off/);
    await expect(page.locator("#card-roles")).not.toHaveClass(/is-off/);
    await expect(page.locator("[data-sys-bar] .sbar-chip")).toHaveText(["Sign-in"]);
    // The line names what runs, not what is picked: it stays until the choice is saved and restarted.
    await expect(page.locator("#card-mode")).toContainText("In use now: Local accounts.");
  });

  // Why (#705): "Test discovery" posts with htmx and swaps its own row, but the server answered with
  // the complete settings page, so a second copy of the page was drawn inside the Identity
  // provider card on every click. Only the result line may change: one page, one form, the typed
  // issuer still in its field, and the button must work again (a second click is answered too).
  // The check refuses loopback addresses, so a loopback issuer is a real failure without a network.
  async function chooseSingleSignOn(page: import("@playwright/test").Page) {
    await login(page);
    await page.goto(local.url + "/settings/system?tab=signin");
    await page.locator("[data-key='AUTH_MODE'] label.rcard", { hasText: "Single sign-on" }).click();
  }
  async function expectOnlyTheRowChanged(page: import("@playwright/test").Page) {
    await expect(page.locator("h1")).toHaveCount(1);
    await expect(page.locator("#sysform")).toHaveCount(1);
    await expect(page.locator("#card-idp .sys-test")).toHaveCount(1);
    await expect(page.locator("#card-idp form, #card-idp header, #card-idp [data-sys-bar]")).toHaveCount(0);
  }

  test("Test discovery answers in its own row: no issuer, a refused issuer, and a second click", async ({ page }) => {
    await chooseSingleSignOn(page);
    const issuer = page.locator("input[name='v.OIDC_ISSUER']");
    const button = page.locator("#card-idp").getByRole("button", { name: "Test discovery" });
    const result = page.locator("#card-idp .sys-test-res");

    await issuer.fill("");
    await button.click();
    await expect(result).toHaveText("SSO check: failed: enter an issuer URL to test");
    await expect(result).toHaveClass(/t-danger/);
    await expectOnlyTheRowChanged(page);

    await issuer.fill("http://127.0.0.1:9/");
    await button.click();
    await expect(result).toHaveText(/^SSO check: failed: no usable OIDC provider at 127\.0\.0\.1:9/);
    await expect(result).toHaveClass(/t-danger/);
    await expect(result).toHaveAttribute("role", "alert");
    await expectOnlyTheRowChanged(page);
    await expect(issuer).toHaveValue("http://127.0.0.1:9/"); // the form was not redrawn

    await button.click(); // the swapped button still works; the service allows one test per few seconds
    await expect(result).toHaveText("SSO check: failed: wait a few seconds between connection tests");
    await expectOnlyTheRowChanged(page);
    await expect(issuer).toHaveValue("http://127.0.0.1:9/");
  });

  // The answer is stubbed here: the check cannot reach a provider on loopback, so a working
  // discovery is covered by the Go test. This only shows that a success row swaps in the same way.
  test("Test discovery: a success row swaps in as a status", async ({ page }) => {
    await chooseSingleSignOn(page);
    await page.route("**/settings/system/sso-test*", (route) =>
      route.fulfill({
        status: 200,
        contentType: "text/html; charset=utf-8",
        body: '<div class="sys-test"><button type="submit" class="btn" formaction="/settings/system/sso-test?tab=signin" formnovalidate hx-post="/settings/system/sso-test?tab=signin" hx-include="#sysform" hx-target="closest .sys-test" hx-swap="outerHTML">Test discovery</button><span class="sys-test-res t-ok" role="status">SSO check: discovery OK: authorization at idp.example, token endpoint at idp.example</span></div>',
      }),
    );
    await page.locator("input[name='v.OIDC_ISSUER']").fill("https://idp.example/");
    await page.locator("#card-idp").getByRole("button", { name: "Test discovery" }).click();
    await expect(page.locator("#card-idp .sys-test-res")).toHaveText(/^SSO check: discovery OK/);
    await expect(page.locator("#card-idp .sys-test-res")).toHaveAttribute("role", "status");
    await expectOnlyTheRowChanged(page);
  });

  // Screenshots of every tab for review: SETTINGS_SHOTS=<dir>.
  test("screenshots of every tab", async ({ page }) => {
    test.skip(!process.env.SETTINGS_SHOTS, "set SETTINGS_SHOTS=<dir> to write screenshots");
    test.setTimeout(240_000);
    const dir = process.env.SETTINGS_SHOTS!;
    mkdirSync(dir, { recursive: true });
    await login(page);
    for (const scheme of ["light", "dark"] as const) {
      await page.emulateMedia({ colorScheme: scheme });
      for (const [size, w, h] of [["desktop", 1280, 900], ["phone", 375, 812]] as const) {
        await page.setViewportSize({ width: w, height: h });
        for (const tab of ["general", "network", "mqtt", "discovery", "firmware", "signin", "advanced"]) {
          await page.goto(local.url + "/settings/system?tab=" + tab);
          await page.screenshot({ path: `${dir}/settings-${tab}-${size}-${scheme}.png`, fullPage: true });
        }
      }
    }
    // the Sign-in tab as three cards side by side on a wide screen (#337)
    await page.emulateMedia({ colorScheme: "light" });
    await page.setViewportSize({ width: 1600, height: 900 });
    await page.goto(local.url + "/settings/system?tab=signin");
    await page.screenshot({ path: `${dir}/settings-signin-wide-1600-light.png`, fullPage: true });
  });
});

// Why (#362): the yellow unsaved dots (tab, save bar) used to be added as inline content, so every
// neighbour jumped sideways the moment a value changed. Their space is reserved, so nothing moves.
// Since #990 the save bar itself is not there while the form is clean, so what must hold clean to dirty is
// the page content: the tab row, the label and the input stay where they are when the bar appears (the
// bar is sticky, so it does not push the form), and the dot and the state text are drawn whole.
test("the unsaved dots appear without moving the tab row or the field label", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  if (process.env.UI362_W) await page.setViewportSize({ width: +process.env.UI362_W, height: 800 });
  if (process.env.UI362_SCHEME) await page.emulateMedia({ colorScheme: process.env.UI362_SCHEME as "light" | "dark" });
  await page.goto(app.url + "/settings/system?tab=firmware");
  const bar = page.locator("[data-sys-bar]");
  await expect(bar).toBeHidden(); // clean: no bar at all (#990)
  const input = page.locator("input[name='v.FIRMWARE_KEEP_RELEASES']");
  const row = input.locator("xpath=ancestor::*[self::label or contains(@class,'kv-row') or @data-key][1]");
  const stripShown = await tabs(page).isVisible();
  const boxes = async () => {
    // Page coordinates: focusing the field may scroll the page, which is not a layout shift.
    const scrollY = await page.evaluate(() => window.scrollY);
    const rect = async (loc: import("@playwright/test").Locator) => {
      const b = await loc.boundingBox();
      expect(b).not.toBeNull();
      return { x: b!.x, y: b!.y + scrollY, width: b!.width, height: b!.height };
    };
    return {
      // On a phone the strip is replaced by the section picker (display: none), so it has no boxes.
      tabs: stripShown ? await Promise.all((await tabs(page).getByRole("link").all()).map(rect)) : [],
      row: await rect(row),
      input: await rect(input),
    };
  };
  const shot = process.env.UI362_DIR;
  const scheme = await page.evaluate(() => matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
  const w = page.viewportSize()!.width;
  if (shot) await page.screenshot({ path: `${shot}/${process.env.UI362_TAG}-${w}-${scheme}-clean.png` });
  const before = await boxes();
  await input.fill("9");
  await expect(bar).toBeVisible();
  await expect(bar).toContainText("Unsaved changes:");
  await expect(page.locator("a.utab[href*='tab=firmware']")).toHaveClass(/is-dirty/);
  if (shot) await page.screenshot({ path: `${shot}/${process.env.UI362_TAG}-${w}-${scheme}-dirty.png` });
  const after = await boxes();

  // The dots are really drawn (not just hidden placeholders).
  const dot = async (sel: string, pseudo: string) =>
    page.locator(sel).first().evaluate((el, p) => {
      const s = getComputedStyle(el, p);
      return { vis: s.visibility, bg: s.backgroundColor, w: s.width };
    }, pseudo);
  if (stripShown) {
    const tabDot = await dot("a.utab.is-dirty", "::after");
    expect(tabDot.vis).toBe("visible");
    expect(tabDot.w).toBe("6px");
    expect(tabDot.bg).not.toBe("rgba(0, 0, 0, 0)");
  }
  const barDot = await dot("[data-sys-bar].is-dirty", "::before");
  expect(barDot.vis).toBe("visible");
  expect(barDot.w).toBe("6px");

  const near = (a: number, b: number, what: string) => expect(Math.abs(a - b), what).toBeLessThanOrEqual(0.5);
  const same = (a: { x: number; y: number; width: number; height: number }, b: typeof a, what: string) => {
    for (const k of ["x", "y", "width", "height"] as const) near(a[k], b[k], `${what}.${k}`);
  };
  before.tabs.forEach((b, i) => same(b, after.tabs[i], `tab ${i}`));
  same(before.row, after.row, "label row");
  same(before.input, after.input, "input");
  // The state text is never cut off (the dot is not inline, so it takes no room from the text).
  const cut = await bar.locator("[data-sbar-state]").evaluate((e) => e.scrollWidth - e.clientWidth);
  expect(cut, "save-bar state text truncated").toBeLessThanOrEqual(0);
});

// Why (#990): the save bar is sticky, so it covers whatever scrolls behind it. On System > MQTT it covered the
// lower part of the Device MQTT passwords card with nothing edited. The bar shows only while the form has
// unsaved changes (the #949 mechanism, as on the Backups page): clean, the card's bottom edge is not covered;
// an edit shows the bar with the changed tab and Save in reach; the old value back hides it again.
for (const width of [1280, 375]) {
  test(`the save bar covers nothing on the MQTT tab while clean and shows with an edit at ${width}px`, async ({ page }) => {
    await page.setExtraHTTPHeaders(admin);
    await page.setViewportSize({ width, height: 800 });
    await page.goto(app.url + "/settings/system?tab=mqtt");
    const bar = page.locator("form#sysform [data-sys-bar]");
    await expect(bar).toBeHidden();
    // Scroll the bottom of the card to just above the status bar: that is where a sticky bar would sit.
    // scrollIntoView would put the card under the status bar, which covers it with or without the bar.
    const covered = async () =>
      page.evaluate(() => {
        const card = document.getElementById("card-devicepasswords")!;
        const status = document.querySelector("footer.sb")!.getBoundingClientRect();
        window.scrollBy(0, card.getBoundingClientRect().bottom - (status.top - 4));
        const r = card.getBoundingClientRect();
        const hit = document.elementFromPoint(r.left + r.width / 2, r.bottom - 6);
        return { onTop: !!hit && card.contains(hit), tag: hit ? hit.tagName + "." + hit.className : "none", bottom: r.bottom, top: status.top };
      });
    const clean = await covered();
    expect(clean.onTop, `the Device MQTT passwords card is covered by ${clean.tag} (card bottom ${clean.bottom}, status bar top ${clean.top})`).toBe(true);
    // an edit shows the bar with the MQTT chip, and Save is in reach
    const flip = page.locator("[data-key='MQTT_PASSWORD_FROM_BACKUPS'] .swtrack");
    await flip.click();
    await expect(bar).toBeVisible();
    await expect(bar.locator(".sbar-state")).toHaveText("Unsaved changes:");
    await expect(bar.locator(".sbar-chip")).toHaveText(["MQTT"]);
    await expect(page.getByRole("button", { name: "Save settings" })).toBeInViewport();
    // the old value back: nothing is unsaved any more
    await flip.click();
    await expect(bar).toBeHidden();
  });
}

// Why (#990): a draft that comes back after a step-up (?restore=1) is an unsaved change. The bar must be on
// screen for it, or the person would have no Save for the restored value.
test("a restored draft shows the save bar", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=firmware");
  await expect(page.locator("form#sysform [data-sys-bar]")).toBeHidden();
  await page.evaluate(() => sessionStorage.setItem("tf-draft:sysconfig", JSON.stringify({ "v.FIRMWARE_KEEP_RELEASES": "6" })));
  await page.goto(app.url + "/settings/system?tab=firmware&restore=1");
  await expect(page.locator("input[name='v.FIRMWARE_KEEP_RELEASES']")).toHaveValue("6");
  await expect(page.locator("form#sysform [data-sys-bar]")).toBeVisible();
  // without ?restore=1 the draft is dropped
  await page.goto(app.url + "/settings/system?tab=firmware");
  await expect(page.locator("form#sysform [data-sys-bar]")).toBeHidden();
});
