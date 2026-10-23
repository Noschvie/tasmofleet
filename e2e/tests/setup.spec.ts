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

test("zero configuration: setup link, first admin, setup, live networks, scan, device list", async ({ page }) => {
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
  await expect(page.getByText("1 field needs attention.")).toBeVisible();
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
  const banner = page.locator("p[role=status]", { hasText: "No device networks are configured" });
  await expect(banner).toBeVisible();
  await expect(banner).toHaveClass(/\btone-warn\b/);
  await expect(banner).not.toHaveClass(/\btone-ok\b/);
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
