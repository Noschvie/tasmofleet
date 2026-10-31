import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Command history (#321): Arrow Up / Down in the console prompt and the bulk "Send command" box,
// kept in this browser's localStorage. Why it matters: a command typed once is repeated on the
// next device in two keystrokes, but a password must never land in localStorage and a command the
// policy refused is not offered again.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`, // the bulk transaction pages come with the firmware server
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_POLL_INTERVAL: "5s",
    },
  });
  expect(app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)])).toContain("created: plug-4m-http");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const KEY = "tasmofleet.commandHistory";
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
const operator = { "X-authentik-username": "olivia", "X-authentik-groups": "ops" };

async function consoleOf(page: import("@playwright/test").Page) {
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: "plug-4m-http", exact: true }).click();
  await page.getByRole("link", { name: "Console" }).click();
  return page.getByRole("textbox", { name: "Command" });
}
async function send(page: import("@playwright/test").Page, box: import("@playwright/test").Locator, cmd: string) {
  await box.fill(cmd);
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await expect(page.getByRole("log", { name: "Console output" })).toContainText(cmd.replace(/ \*+$/, ""));
}
const stored = (page: import("@playwright/test").Page) => page.evaluate((k) => JSON.parse(localStorage.getItem(k) ?? "[]"), KEY);

test("console: Arrow Up/Down recall, edit sends the edited text, Escape restores, newest first without duplicates", { tag: "@core" }, async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  let box = await consoleOf(page);
  await send(page, box, "Status 2");
  box = page.getByRole("textbox", { name: "Command" });
  await send(page, box, "Status 5");
  box = page.getByRole("textbox", { name: "Command" });
  await send(page, box, "Status 2"); // again: moves to the front, no second copy
  expect(await stored(page)).toEqual(["Status 2", "Status 5"]);

  box = page.getByRole("textbox", { name: "Command" });
  await box.focus();
  await box.pressSequentially("Sta");
  await box.press("ArrowUp");
  await expect(box).toHaveValue("Status 2");
  await box.press("ArrowUp");
  await expect(box).toHaveValue("Status 5");
  await box.press("ArrowUp"); // the oldest stays
  await expect(box).toHaveValue("Status 5");
  await box.press("ArrowDown");
  await expect(box).toHaveValue("Status 2");
  await box.press("Escape"); // back to what was typed
  await expect(box).toHaveValue("Sta");

  // The history is shared by every device's console (it is per browser): recall, edit, send the edit.
  await box.press("ArrowUp");
  await box.fill("Status 11");
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await expect(page.getByRole("log", { name: "Console output" })).toContainText("Status 11");
  expect((await stored(page))[0]).toBe("Status 11");

  await page.getByRole("button", { name: "Clear history" }).click();
  expect(await stored(page)).toEqual([]);
  await page.getByRole("textbox", { name: "Command" }).press("ArrowUp");
  await expect(page.getByRole("textbox", { name: "Command" })).toHaveValue("");
});

test("console: works without storage", async ({ page }) => {
  await page.addInitScript(() => {
    Object.defineProperty(window, "localStorage", {
      get() {
        throw new Error("storage blocked");
      },
    });
  });
  await page.setExtraHTTPHeaders(admin);
  const box = await consoleOf(page);
  await send(page, box, "Status 2");
  await page.getByRole("textbox", { name: "Command" }).press("ArrowUp");
  await expect(page.getByRole("textbox", { name: "Command" })).toHaveValue("");
  await page.getByRole("button", { name: "Clear history" }).click(); // does not throw
});

test("bulk Send command: the box recalls the same history", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  await page.evaluate((k) => localStorage.setItem(k, JSON.stringify(["Status 2", "Status 5"])), KEY);
  const row = page.getByRole("row").filter({ hasText: "plug-4m-http" });
  await expect(row.getByText("Online", { exact: true })).toBeVisible({ timeout: 30_000 });
  await row.getByRole("checkbox").check();
  await page.locator("[data-bulkbar]").getByRole("button", { name: /Send command/ }).click();
  const box = page.getByRole("textbox", { name: "Command" });
  await box.focus();
  await box.press("ArrowUp");
  await expect(box).toHaveValue("Status 2");
  await box.press("ArrowUp");
  await expect(box).toHaveValue("Status 5");
  await box.press("Escape");
  await expect(box).toHaveValue("");
  await box.press("ArrowUp");
  await box.press("ArrowUp");
  await page.getByRole("button", { name: "Send to 1 device" }).click();
  await page.getByRole("button", { name: "Send to 1 device" }).click();
  await expect(page).toHaveURL(/\/devices\/bulk\/result\/\d+/);
  expect((await page.evaluate((k) => JSON.parse(localStorage.getItem(k) ?? "[]"), KEY))[0]).toBe("Status 5");
});

// Last on purpose: MqttPassword on the simulator makes the device unreachable for the tests after it.
test("console: a command with a secret and a refused command are never stored", async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  let box = await consoleOf(page);
  await box.fill("TelePeriod 120"); // admin-only: refused for an operator
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await expect(page.getByRole("alert").filter({ hasText: "needs the admin role" })).toBeVisible();
  // one stored command proves that the page does write, so the empty rest is not a broken script
  await send(page, page.getByRole("textbox", { name: "Command" }), "Status 2");

  await page.setExtraHTTPHeaders(admin);
  box = await consoleOf(page);
  // Both reach the device (after the confirmation a credential command asks for): the audit log shows
  // them as "MqttPassword ***", and the history must not hold them either.
  for (const cmd of ["MqttPassword hunter2-never-stored", "Backlog Power ON; MqttPassword hunter2-never-stored"]) {
    await page.getByRole("textbox", { name: "Command" }).fill(cmd);
    await page.getByRole("button", { name: "Send", exact: true }).click();
    await page.getByRole("button", { name: "Confirm and send" }).click();
    await expect(page.getByRole("log", { name: "Console output" }).getByText(/MqttPassword \*\*\*/).first()).toBeVisible(); // sent, shown redacted
  }
  const all = JSON.stringify(await stored(page));
  expect(all).not.toContain("hunter2");
  expect(all).not.toContain("MqttPassword");
  expect(all).not.toContain("TelePeriod");
  expect(await stored(page)).toEqual(["Status 2"]);
});
