import { existsSync } from "node:fs";
import { join } from "node:path";
import { test, expect } from "../support/fixtures";
import { App, Devenv, startApp, startDevenv } from "../support/stack";

// A fresh container with no configuration at all: self-signed TLS, the first admin set up through
// the one-time link in the container log (N12), then the setup: device networks (applied live, no
// restart), the network scan, and adding a device. The simulator listens on a random port, so the
// scan (port 80 only) finds nothing; the device is added by hand, which only works because the
// networks were applied without a restart. (The generated-password path: standalone.spec.ts.)
test.use({ ignoreHTTPSErrors: true });
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv();
  app = await startApp({ bare: true, scheme: "https" });
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

// #1079 (A7-020): the three step labels sat in three flex items of a 335 px card with nowrap and an ellipsis, so a
// phone read "Setup ...", "Admin ..." and "Basic se...". At 375 px the row keeps the three numbers and shows only the
// current step's label, under the row and never cut; the other labels stay readable to a screen reader. A person who
// cannot see the colour of the current number still reads which step it is. On the first page (no token) and on the
// password page (step 2), where the link in the container log opens it.
test("phone: the step indicator shows the current step's label whole, the others only to a screen reader", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto(app.url + "/setup/account");
  const label = async () =>
    page.locator(".setup-steps .setup-label").evaluateAll((els) =>
      els.map((e) => {
        const r = e.getBoundingClientRect();
        const li = e.closest("li")!;
        return {
          text: e.textContent!.trim(),
          current: li.classList.contains("is-current"),
          shown: r.width > 2 && r.height > 2 && getComputedStyle(e).visibility !== "hidden",
          cut: e.scrollWidth > e.clientWidth + 1 || getComputedStyle(e).textOverflow === "ellipsis",
        };
      }),
    );
  let got = await label();
  expect(got.map((l) => l.text)).toEqual(["Setup token", "Admin account", "Basic settings"]);
  expect(got.filter((l) => l.shown).map((l) => l.text), "exactly the current step's label is shown").toEqual(["Setup token"]);
  expect(got.filter((l) => l.shown && l.cut), "no shown label is cut with an ellipsis").toEqual([]);
  // the page itself does not scroll sideways
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBe(true);
  // step 2, through the setup link
  const link = new URL(app.setupLink(), app.url);
  await page.goto(app.url + link.pathname + link.search);
  got = await label();
  expect(got.filter((l) => l.shown).map((l) => l.text)).toEqual(["Admin account"]);
  expect(got.filter((l) => l.shown && l.cut)).toEqual([]);
  expect(await page.locator(".setup-steps .is-current .setup-label").evaluate((e) => e.getBoundingClientRect().bottom <= e.closest("ol")!.getBoundingClientRect().bottom + 1)).toBe(true);
  await page.setViewportSize({ width: 1280, height: 900 });
  expect((await label()).filter((l) => l.shown).length, "a wide screen shows all three labels").toBe(3);
});

test("zero configuration: setup link, first admin, setup, live networks, scan, device list", { tag: "@core" }, async ({ page }) => {
  // Why (N12): only the person who can read the container log may set the first password. Without
  // the link the page says where it is and offers no form; a wrong token is refused.
  await page.goto(app.url + "/setup/account");
  await expect(page.getByRole("heading", { name: "Open the setup link" })).toBeVisible();
  await expect(page.locator("form")).toHaveCount(0);
  expect((await page.goto(app.url + "/setup/account?token=not-the-token"))?.status()).toBe(403);
  await expect(page.getByText("That link isn’t valid.")).toBeVisible();

  const link = new URL(app.setupLink(), app.url);
  expect(link.pathname, "the first start prints the setup link to its log").toBe("/setup/account");
  await page.goto(app.url + link.pathname + link.search);
  await expect(page.getByRole("heading", { name: "Create the first admin account" })).toBeVisible();
  const pw = "e2e-setup-pass-1";
  await page.getByLabel("Display name").fill("Erna Admin");
  await page.locator('input[name="password"]').fill(pw);
  await page.locator('input[name="confirm"]').fill(pw + "x");
  await page.getByRole("button", { name: "Create account and continue" }).click();
  // a refused form keeps the link usable and focuses the refused field
  // one refused field shows its own message and no summary banner (#1098)
  await expect(page.locator(".ufield-err")).toHaveCount(1);
  await expect(page.locator(".err-summary")).toHaveCount(0);
  await expect(page.locator('input[name="confirm"]')).toBeFocused();
  await page.locator('input[name="password"]').fill(pw);
  await page.locator('input[name="confirm"]').fill(pw);
  await page.getByRole("button", { name: "Create account and continue" }).click();
  // signed in at once, on the next step; the link is spent and the generated password file gone
  await expect(page).toHaveURL(/\/setup$/);
  expect(existsSync(join(app.dataDir, "initial-admin-password")), "the generated password stops working").toBe(false);
  expect((await page.request.get(app.url + link.pathname + link.search, { maxRedirects: 0 })).status(), "a spent link leads to the sign-in").toBe(303);

  // The first visit leads into the setup (the last of its three steps); the banner says that no
  // device is contacted yet.
  await page.goto(app.url + "/");
  await expect(page.getByRole("heading", { name: "Basic settings" })).toBeVisible();
  await expect(page.getByRole("list", { name: "Setup steps" })).toBeVisible();
  // UX review #37: the banner is a warning (not the green success notice) and does not link to the page it is on
  await expect(page.getByText("No device networks are configured")).toHaveCount(0);
  await page.goto(app.url + "/devices");
  const banner = page.locator(".banner[role=status]", { hasText: "No device networks are configured" });
  await expect(banner).toBeVisible();
  await expect(banner).toHaveClass(/\bt-warn\b/);
  await expect(banner).not.toHaveClass(/\bt-ok\b/);
  await page.goto(app.url + "/setup");

  // The suggestions are only a proposal; nothing is configured until the admin finishes.
  // Finish setup saves the networks (live, no restart) and scans them because the box is ticked
  // (the simulators do not listen on port 80, so the scan finds nothing).
  await page.getByLabel("Device networks").fill("127.0.0.1/32");
  await expect(page.getByLabel(/Scan these networks/)).toBeChecked();
  await page.getByRole("button", { name: "Finish setup" }).click();
  await expect(page.getByText("Scan finished.")).toBeVisible({ timeout: 30_000 });
  await expect(page.getByText("1 address · 0 Tasmota devices")).toBeVisible();
  await page.goto(app.url + "/devices");
  await expect(page.getByText("No device networks are configured")).toHaveCount(0);

  // Adding a device works at once: the networks were applied without a restart.
  const [name, d] = Object.entries(env.devices)[0];
  const [host, port] = d.addr.split(":");
  await page.goto(app.url + "/devices/new");
  await page.locator("input[name=address]").fill(host);
  await page.getByText("More options").click(); // name and port are folded away (design v2)
  await page.locator("input[name=name]").fill(name);
  await page.locator("input[name=port]").fill(port);
  await page.getByRole("button", { name: "Add device" }).click();
  await page.goto(app.url + "/devices");
  await expect(page.getByRole("link", { name, exact: true })).toBeVisible();

  // The setup is finished: the dashboard opens instead of the setup.
  await page.goto(app.url + "/");
  await expect(page.getByRole("heading", { name: "Fleet overview" })).toBeVisible();
});
