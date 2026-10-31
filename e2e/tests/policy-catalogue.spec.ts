import { join } from "node:path";
import { test, expect } from "../support/fixtures";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// The rule form's command field learns from the Tasmota command catalogues (#269): suggestions for
// the firmware versions in the fleet, an index box for indexed commands, a value field that follows
// the catalogue, and a check that warns about versions and features but only refuses a command no
// fleet version has.
let app: App;
let env: Devenv;
const versions: Record<string, [string, string]> = {
  "lamp-1m": ["14.4.1", "tasmota"],
  "plug-4m": ["14.4.1", "tasmota"],
  "plug-4m-http": ["13.4.0", "tasmota"],
  "lamp-minimal": ["13.4.0", "minimal"],
};

test.beforeAll(async () => {
  env = await startDevenv();
  app = await startApp({
    env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32", TASMOFLEET_ADMIN_GROUPS: "adm" },
  });
  expect(app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)])).toContain("created:");
  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
  try {
    db.exec("PRAGMA busy_timeout=5000");
    const set = db.prepare("UPDATE devices SET fw_version = ?, fw_variant = ?, chip_family = ? WHERE name = ?");
    for (const n of Object.keys(env.devices)) {
      const [v, variant] = versions[n] ?? ["16.0.0", "tasmota"];
      set.run(v, variant, n.startsWith("esp32") ? "esp32" : "esp8266", n);
    }
  } finally {
    db.close();
  }
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

// A shot resizes the window, which makes tables.js set inline widths that htmx copies on the next swap (a CSP
// violation the fixtures catch): take the shots last in a test.
// shots writes the page at both widths in both themes when UI_SHOTS_DIR is set (the paired screenshots of the PR).
async function shots(page: import("@playwright/test").Page, name: string) {
  const dir = process.env.UI_SHOTS_DIR;
  if (!dir) return;
  const size = page.viewportSize() ?? { width: 1280, height: 900 };
  for (const w of [1280, 375]) {
    for (const scheme of ["light", "dark"] as const) {
      await page.setViewportSize({ width: w, height: 800 });
      await page.emulateMedia({ colorScheme: scheme });
      await page.setViewportSize({ width: w, height: await page.evaluate(() => document.documentElement.scrollHeight) });
      await page.evaluate(() => window.scrollTo(0, 0));
      await page.screenshot({ path: `${dir}/${name}-${w}-${scheme}.png` });
    }
  }
  await page.setViewportSize(size);
  await page.emulateMedia({ colorScheme: null });
}

// htmx attaches its handlers to swapped-in elements after a short settle delay (20 ms); a test that
// types into a field the moment it appears would otherwise beat them (the change event then goes to the form).
async function settled(page: import("@playwright/test").Page) {
  await expect(page.locator(".htmx-added, .htmx-settling, .htmx-swapping")).toHaveCount(0);
}

async function draft(page: import("@playwright/test").Page, name: string) {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/policy/rules/new?command=Timezone");
  await page.locator("input[name='name']").fill(name);
}

test("suggestions come from the catalogues of the fleet's versions, with range and help", { tag: "@core" }, async ({ page }) => {
  await draft(page, "New rule");
  const cmd = page.locator("#rule-command");
  await cmd.fill("");
  await cmd.dispatchEvent("change");
  await expect(page.locator("#want")).toBeDisabled();
  await cmd.pressSequentially("SetOpt");
  const box = page.locator("#cmd-sug");
  await expect(box).toBeVisible();
  const first = box.getByRole("option").first();
  await expect(first).toContainText("SetOption");
  await expect(first).toContainText("0..177");
  await expect(first).toContainText("Switch one firmware behaviour on or off");
  await expect(box).toContainText("commands in Tasmota 13.4.0, 14.4.1, 16.0.0");
  await expect(box.getByRole("option", { name: /WCSetOptions/ })).toContainText("needs USE_WEBCAM");
  // Escape closes, keyboard picks
  await page.keyboard.press("Escape");
  await expect(box).toBeHidden();
  await cmd.pressSequentially("i"); // "SetOpti"
  await expect(box).toBeVisible();
  await page.keyboard.press("Enter");
  // SetOption has an index range: the field is now a name with a number box
  await expect(page.locator("#rule-index")).toBeFocused();
  await expect(page.locator(".cmd-idx-base")).toHaveText("SetOption");
  await page.getByRole("button", { name: "Change command" }).click();
  await expect(page.locator("input#rule-command[name='command']")).toBeVisible();
  await settled(page);
  await cmd.fill("");
  await cmd.dispatchEvent("change");
  await expect(page.locator("#want")).toBeDisabled();
  await cmd.pressSequentially("SetOpt");
  await expect(box).toBeVisible();
  await shots(page, "suggest");
});

test("an indexed command is a name and a number box, and Change gives the text field back", { tag: "@core" }, async ({ page }) => {
  await draft(page, "Home Assistant discovery");
  await page.locator("#rule-command").fill("SetOption19");
  await page.locator("#rule-command").dispatchEvent("change");
  const idx = page.locator("#rule-index");
  await expect(idx).toHaveValue("19");
  await expect(page.locator("#command-field")).toContainText("0 to 177");
  await expect(page.locator("#help-command")).toContainText("Home Assistant discovery");
  await expect(page.locator("#help-command a")).toHaveAttribute("href", "https://tasmota.github.io/docs/Commands/#setoption19");
  await expect(page.locator("#want-control")).toContainText("on / off");
  // the hidden command follows the box, so the preview and the save use SetOption20
  await idx.fill("20");
  await idx.dispatchEvent("change");
  await expect(page.locator("input[name='command']")).toHaveValue("SetOption20");
  await expect(page.locator("#cmd-sends")).toContainText("SetOption20");
  await page.getByRole("button", { name: "Change command" }).click();
  await expect(page.locator("input#rule-command[name='command']")).toHaveValue("SetOption20");
  await expect(page.locator("#rule-index")).toHaveCount(0);
  await settled(page);
  await page.locator("#rule-command").fill("SetOption19");
  await page.locator("#rule-command").dispatchEvent("change");
  await expect(page.locator("#rule-index")).toHaveValue("19");
  await expect(page.locator("#rule-line")).toContainText("SetOption19");
  await shots(page, "indexed");
});

test("an index out of range warns and the rule can still be saved", { tag: "@core" }, async ({ page }) => {
  await draft(page, "Far option");
  await page.locator("#rule-command").fill("SetOption200");
  await page.locator("#rule-command").dispatchEvent("change");
  await expect(page.locator("#command-check .banner")).toHaveClass(/tone-warn/);
  await page.locator("#want-control label").filter({ hasText: /^On$/ }).click();
  await page.getByRole("button", { name: "Save rule" }).click();
  await expect(page).toHaveURL(/\/policy(\?|$)/);
});

test("an index out of range is shown as a warning with the range", async ({ page }) => {
  await draft(page, "Far option");
  await page.locator("#rule-command").fill("SetOption200");
  await page.locator("#rule-command").dispatchEvent("change");
  const check = page.locator("#command-check");
  await expect(check).toContainText("index 200 is outside 0 to 177");
  await expect(page.locator("#rule-index")).toHaveValue("200");
  await shots(page, "index-bad");
});

test("a number value follows the catalogue: range, step, and a value outside it only warns", { tag: "@core" }, async ({ page }) => {
  await draft(page, "Blink time");
  // decimals where the handler answers a float
  await page.locator("#rule-command").fill("Latitude");
  await page.locator("#rule-command").dispatchEvent("change");
  await expect(page.locator("#want")).toHaveAttribute("step", "any");
  await page.locator("#rule-command").fill("BlinkTime");
  await page.locator("#rule-command").dispatchEvent("change");
  const want = page.locator("#want");
  await expect(want).toHaveAttribute("min", "2");
  await expect(want).toHaveAttribute("max", "3600");
  await expect(page.locator("#want-control .cmd-kind")).toHaveText("number");
  await want.fill("1");
  const warn = page.locator("#want-check");
  await expect(warn).toContainText("1 is below the minimum of 2.");
  await expect(warn.locator(".banner")).toHaveClass(/tone-warn/);
  await expect(warn).toContainText("You can still save");
  await want.fill("1.5");
  await expect(warn).toContainText("1.5 is not a whole number.");
  await want.fill("1");
  await expect(warn).toContainText("below the minimum");
  await shots(page, "number");
});

test("a value outside the catalogue's range is saved", { tag: "@core" }, async ({ page }) => {
  await draft(page, "Blink time");
  await page.locator("#rule-command").fill("BlinkTime");
  await page.locator("#rule-command").dispatchEvent("change");
  await page.locator("#want").fill("1");
  await expect(page.locator("#want-check")).toContainText("below the minimum");
  await page.getByRole("button", { name: "Save rule" }).click();
  await expect(page).toHaveURL(/\/policy(\?|$)/);
});

test("a text command shows its kind and no invented range", { tag: "@core" }, async ({ page }) => {
  await draft(page, "Syslog host");
  await page.locator("#rule-command").fill("LogHost");
  await page.locator("#rule-command").dispatchEvent("change");
  await expect(page.locator("#want-control .cmd-kind")).toHaveText("text");
  await expect(page.locator("#want-control")).toContainText("No range is known for this command.");
  await shots(page, "text");
});

test("a command missing in some versions warns, names the devices, and the devices card says why", { tag: "@core" }, async ({ page }) => {
  await draft(page, "Power lock");
  await page.locator("#rule-command").fill("PowerLock");
  await page.locator("#rule-command").dispatchEvent("change");
  const check = page.locator("#command-check");
  await expect(check).toContainText("PowerLock is not a command in Tasmota 13.4.0 (2 devices)");
  await expect(check.locator(".cmd-v")).toHaveCount(3);
  await expect(check.locator(".cmd-v.no")).toContainText("not a command in Tasmota 13.4.0");
  await expect(check).toContainText("lamp-minimal and plug-4m-http show as “Not judged”");
  await page.locator("#want").fill("0");
  await expect(page.locator("#rule-devices")).toContainText("not a command in 13.4.0");
  await shots(page, "check-version");
});

test("a version warning does not stop the save", { tag: "@core" }, async ({ page }) => {
  await draft(page, "Power lock");
  await page.locator("#rule-command").fill("PowerLock");
  await page.locator("#rule-command").dispatchEvent("change");
  await expect(page.locator("#command-check .banner")).toHaveClass(/tone-warn/);
  await page.locator("#want").fill("0");
  await page.getByRole("button", { name: "Save rule" }).click();
  await expect(page).toHaveURL(/\/policy(\?|$)/);
});

test("a command that needs a feature names the devices whose build lacks it", { tag: "@core" }, async ({ page }) => {
  await draft(page, "Device latitude");
  await page.locator("#rule-command").fill("Latitude");
  await page.locator("#rule-command").dispatchEvent("change");
  const check = page.locator("#command-check");
  await expect(check).toContainText("Latitude needs the feature");
  await expect(check).toContainText("USE_SUNRISE");
  await expect(check).toContainText("lamp-minimal");
  await shots(page, "check-feature");
});

test("the title, the trail and the notice follow the draft's name and command", { tag: "@core" }, async ({ page }) => {
  await draft(page, "Power lock");
  await page.locator("#rule-command").fill("PowerLock");
  await page.locator("#rule-command").dispatchEvent("change");
  await expect(page.locator("#rule-title")).toContainText("Power lock");
  await expect(page.locator(".crumb-here")).toHaveText("Power lock");
  await expect(page.locator("#ph-title")).toHaveText("Power lock");
  await expect(page.locator("#rule-source")).toContainText("A new rule for PowerLock.");
});

test("a command that no Tasmota version of the fleet has is refused with the versions named", { tag: "@core" }, async ({ page }) => {
  await draft(page, "Nothing");
  await page.locator("#rule-command").fill("NoSuchCmdAnywhere");
  await page.locator("#rule-command").dispatchEvent("change");
  await expect(page.locator("#command-check .banner")).toHaveClass(/tone-danger/);
  await page.locator("#want").fill("1");
  await page.getByRole("button", { name: "Save rule" }).click();
  await expect(page.locator("#err-command")).toContainText("NoSuchCmdAnywhere is not a command in any Tasmota version in your fleet (16.0.0, 14.4.1, 13.4.0).");
});
