import { test, expect } from "../support/fixtures";
import { pickForUpdate } from "../support/updates";
import { randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// M2-8: an admin uploads a synthetic image on the Firmware page, an operator
// updates one simulator device with it (ESP8266 1 MB: staged via minimal), and
// wrong images are refused. The synthetic image is the captured ESP8266 header
// plus filler and the simulator's version marker, like simulator.NewImage.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_URL,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
    },
  });
  const out = app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
  expect(out).toContain("created: lamp-1m");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const operator = { "X-authentik-username": "olivia", "X-authentik-groups": "ops" };
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
// The version of an upstream release on purpose: self-built images report the version they were built from.
const version = "16.1.0";

function header(name: string): Buffer {
  return readFileSync(join(__dirname, "..", "..", "testdata", "fixtures", "firmware-headers", name + ".hdr"));
}

/** image is a synthetic uncompressed image; pad > 0 adds incompressible bytes so the 1 MB device needs the staged flow. */
function image(hdr: string, pad: number): Buffer {
  return Buffer.concat([header(hdr), Buffer.alloc(2048, 0x55), Buffer.from(`TASMOFLEET-SIM-VERSION=${version}\0`), randomBytes(pad)]);
}

/** uploads lists the uploaded images among the cached files (an upload's label is its name there). */
const uploads = (page: import("@playwright/test").Page) => page.locator(".fw-crow", { hasText: "e2e build" });

async function upload(page: import("@playwright/test").Page, v: string, name: string, buffer: Buffer) {
  await page.goto(app.url + "/firmware");
  await page.getByRole("link", { name: "Upload custom firmware" }).click();
  await expect(page.locator("dialog[open]")).toBeVisible();
  await page.getByLabel("Label").fill("e2e build");
  await page.getByLabel("Version").fill(v);
  await page.locator('input[name="image"]').setInputFiles({ name, mimeType: "application/octet-stream", buffer });
  await page.getByRole("button", { name: "Upload" }).click();
}

test("operators see no upload form", { tag: "@core" }, async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/firmware");
  await expect(page.locator('input[name="image"]')).toHaveCount(0);
  await expect(page.getByRole("link", { name: "Upload custom firmware" })).toHaveCount(0);
});

// Why: the drop zone says "Drop a .bin or .bin.gz here"; a dropped file must land in the upload
// form, not be opened by the browser.
test("a file dropped on the drop zone opens the upload form with it", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/firmware");
  const transfer = await page.evaluateHandle(() => {
    const dt = new DataTransfer();
    dt.items.add(new File([new Uint8Array([0xe9, 1, 2, 3])], "dropped.bin", { type: "application/octet-stream" }));
    return dt;
  });
  await page.locator("[data-fw-drop]").dispatchEvent("dragover", { dataTransfer: transfer });
  await page.locator("[data-fw-drop]").dispatchEvent("drop", { dataTransfer: transfer });
  const dlg = page.locator("dialog[open]");
  await expect(dlg.getByRole("heading", { name: "Upload custom firmware" })).toBeVisible();
  expect(await dlg.locator('input[name="image"]').evaluate((i: HTMLInputElement) => i.files?.[0]?.name)).toBe("dropped.bin");
});

test("wrong images are refused with a reason and nothing is stored", { tag: "@core" }, async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await upload(page, version, "tasmota32-safeboot.bin", image("tasmota32-safeboot.bin", 0));
  await expect(page.locator("#custom-result")).toContainText("never an OTA target");
  await upload(page, version, "tasmota.bin.gz", Buffer.from("this is not firmware ".repeat(200)));
  await expect(page.locator("#custom-result")).toContainText("not a Tasmota/ESP firmware image");
  await page.goto(app.url + "/firmware");
  await expect(uploads(page)).toHaveCount(0);
});

test("upload a custom image, update an ESP8266 with it, delete it", async ({ page, request }) => {
  test.setTimeout(240_000);
  await page.setExtraHTTPHeaders(admin);
  await upload(page, version, "my-build.bin", image("tasmota.bin.gz", 400 * 1024));
  await expect(page.getByRole("heading", { name: "Firmware", exact: true })).toBeVisible();
  const row = uploads(page);
  await expect(row).toContainText(`custom ${version}`);
  await expect(row).toHaveAttribute("title", /ESP8266 \/ .*uploaded as my-build\.bin/);

  await pickForUpdate(page, app.url, ["lamp-1m"]);
  const custom = page.locator("label.upd-relopt", { hasText: "e2e build" });
  await expect(custom).toContainText(`Custom firmware ${version}`);
  await expect(custom.locator("input")).not.toBeChecked(); // never the default
  await custom.click();
  await page.getByRole("button", { name: "Review update" }).click();
  await expect(page.locator(".upd-prow").filter({ hasText: "lamp-1m" })).toContainText("Staged");
  await page.getByRole("button", { name: "Start update", exact: true }).click();
  await expect(page).toHaveURL(/\/transactions\/\d+$/);
  await expect(page.locator(".upd-row").filter({ hasText: "lamp-1m" }).getByText("Done", { exact: true })).toBeVisible({ timeout: 200_000 });

  const state = await (await request.get(`http://${env.devices["lamp-1m"].addr}/cm?cmnd=Status%202`)).json();
  expect(state.StatusFWR.Version).toContain(version);

  // upload, use and (below) delete are in the audit log
  await page.goto(app.url + "/audit");
  await expect(page.locator("table.audittable").getByText("Upload firmware").first()).toBeVisible();
  await expect(page.locator("table.audittable").getByText("Use uploaded firmware").first()).toBeVisible();

  await page.goto(app.url + "/firmware");
  await uploads(page).getByRole("link", { name: /^Delete/ }).click();
  await expect(page.locator("dialog[open]")).toContainText("cannot be downloaded again");
  await page.locator("dialog[open]").getByRole("button", { name: "Delete image" }).click();
  await expect(uploads(page)).toHaveCount(0);
});
