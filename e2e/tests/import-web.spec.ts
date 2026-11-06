import { test, expect } from "../support/fixtures";
import { mkdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// M2-7: an admin uploads TasmoAdmin's devices.csv, sees the dry-run plan,
// confirms, and the devices appear in the inventory.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
    },
  });
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

// Why (#661): the plan counts lines. A row whose port cannot be used is imported on port 80: it is
// a create with a note on its own row, not a rejection and not a banner of its own; only the
// line that gave no row is a reject. UX_SHOTS=<dir> keeps screenshots (375 and 1280, light and dark).
test("the plan counts lines and notes a warning on the kept row", async ({ page }) => {
  const [first, second] = Object.entries(env.devices);
  const row = (i: number, name: string, addr: string, port: string) =>
    `${i},"${name}",${addr.split(":")[0]},,,bulb_1,1,1,0,0,1,${port},"${name}",0,${name},0`;
  const csv = [row(1, first[0], first[1].addr, "99999"), row(2, second[0], second[1].addr, second[1].addr.split(":")[1]), "x"].join("\n") + "\n";
  await page.setExtraHTTPHeaders({ "X-authentik-username": "alice", "X-authentik-groups": "adm" });
  await page.goto(app.url + "/devices/new");
  await page.setInputFiles('input[name="files"]', { name: "devices.csv", mimeType: "text/csv", buffer: Buffer.from(csv) });
  await expect(page.getByRole("heading", { name: "Import plan" })).toBeVisible();
  const counts = page.locator(".add-plan-counts");
  await expect(counts).toContainText("2 create");
  await expect(counts).toContainText("1 reject");
  await expect(page.locator(".add-improw", { hasText: first[0] })).toContainText("Unusable port, using 80");
  await expect(page.locator(".add-improw", { hasText: first[0] })).toContainText("Create");
  await expect(page.locator(".add-improw", { hasText: "devices.csv line 3" })).toContainText("Reject");
  await expect(page.locator(".add-improw", { hasText: "devices.csv line 3" })).toContainText("Row has no address");
  await expect(page.locator(".add-plan")).not.toContainText("Tasmoadmin");
  const shots = process.env.UX_SHOTS;
  if (shots) {
    mkdirSync(shots, { recursive: true });
    for (const w of [1280, 375]) {
      for (const scheme of ["light", "dark"] as const) {
        await page.setViewportSize({ width: w, height: 900 });
        await page.emulateMedia({ colorScheme: scheme });
        await page.locator(".add-plan").screenshot({ path: `${shots}/import-plan-${w}-${scheme}.png` });
      }
    }
  }
});

// Runs first: the next test applies its plan, and a device that exists is a merge, not a create.
test("upload devices.csv, review the plan, apply", async ({ page }) => {
  const dir = writeTasmoAdminDir(env.devices);
  const names = Object.keys(env.devices);
  await page.setExtraHTTPHeaders({ "X-authentik-username": "alice", "X-authentik-groups": "adm" });
  await page.goto(app.url + "/devices/new");
  await page.setInputFiles('input[name="files"]', {
    name: "devices.csv",
    mimeType: "text/csv",
    buffer: readFileSync(join(dir, "devices.csv")),
  });
  await expect(page.getByRole("heading", { name: "Import plan" })).toBeVisible();
  await expect(page.locator(".add-improw-name", { hasText: names[0] }).first()).toBeVisible();
  await page.getByRole("button", { name: /^Apply plan/ }).click();
  await expect(page.getByText("Import finished.")).toBeVisible();
  await page.goto(app.url + "/devices");
  await expect(page.getByRole("link", { name: names[0], exact: true })).toBeVisible();
});

test("operators cannot reach the import", { tag: "@core" }, async ({ page }) => {
  await page.setExtraHTTPHeaders({ "X-authentik-username": "olivia", "X-authentik-groups": "ops" });
  const r = await page.goto(app.url + "/devices/new");
  expect(r?.status()).toBe(403);
});
