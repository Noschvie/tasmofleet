import { test, expect } from "../support/fixtures";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// #538: an action that takes longer than a moment shows at once that it runs, and a second click
// does nothing meanwhile. One shared script (form.js) does it for every plain POST form and every
// htmx control, so these tests cover one of each plus the loading placeholder.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv();
  app = await startApp({
    env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32", TASMOFLEET_ADMIN_GROUPS: "adm" },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

async function devicePath(page: import("@playwright/test").Page): Promise<string> {
  await page.goto(app.url + "/devices");
  // by id, not by name: "Refresh identity" renames the device after what it reports
  return (await page.locator("a[href^='/devices/']").evaluateAll((as) => as.map((a) => a.getAttribute("href")!).find((h) => /^\/devices\/\d+$/.test(h))))!;
}

// Why: "Refresh identity" is an htmx button. Without a busy state the person clicks again or
// assumes nothing happened while the device is read.
test("an htmx button is busy at once and ignores a second click", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  const href = await devicePath(page);
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  let posts = 0;
  await page.route("**/probe", async (route) => {
    posts++;
    await gate;
    await route.continue();
  });
  await page.goto(app.url + href);
  const btn = page.getByRole("button", { name: "Refresh identity" });
  await btn.click();
  await expect(btn).toBeDisabled();
  await expect(btn).toHaveAttribute("aria-busy", "true");
  await expect(btn).toHaveClass(/htmx-request/);
  release();
  await expect(page.getByText("Identity refreshed.")).toBeVisible();
  expect(posts).toBe(1);
});

// Why: a plain POST form leaves the page; until the next page arrives the submitter must look
// busy, and a second submit (Enter in a field) must not send the form again. The page cannot be
// inspected while its navigation is held, so a probe in the page writes what it saw to
// sessionStorage, and the test reads it on the page that follows.
test("a plain POST form marks its submitter busy and is not sent twice", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  const href = await devicePath(page);
  let posts = 0;
  await page.route("**" + href, async (route) => {
    if (route.request().method() !== "POST") return route.continue();
    posts++;
    await new Promise((r) => setTimeout(r, 1500));
    await route.continue();
  });
  await page.goto(app.url + href + "/edit");
  await page.evaluate(() => {
    const btn = [...document.querySelectorAll("button")].find((b) => b.textContent?.trim() === "Save changes") as HTMLButtonElement;
    const form = btn.form as HTMLFormElement;
    let second: boolean | null = null;
    window.addEventListener("submit", (e) => {
      if (second === null && btn.disabled === false) setTimeout(() => form.requestSubmit(), 100);
      else if (second === null) second = e.defaultPrevented;
    });
    form.addEventListener("submit", () => {
      setTimeout(() => {
        sessionStorage.setItem("probe", JSON.stringify({ disabled: btn.disabled, busy: btn.getAttribute("aria-busy"), second }));
      }, 300);
    }, { once: true });
  });
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  const probe = JSON.parse((await page.evaluate(() => sessionStorage.getItem("probe"))) ?? "null");
  expect(probe).toMatchObject({ disabled: true, busy: "true", second: true });
  expect(posts).toBe(1);
});

// Why: content that loads from the device says so, and after about 10 s says it is still working,
// so a slow device is not mistaken for a hung page.
test("the settings placeholder is busy and says it is still working after 10 s", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  const href = await devicePath(page);
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  await page.route("**/settings/form", async (route) => {
    await gate;
    await route.continue();
  });
  await page.goto(app.url + href + "/settings");
  const ph = page.locator("#settings-body");
  await expect(ph).toHaveAttribute("aria-busy", "true");
  await expect(ph).toContainText("Reading the settings from the device");
  await expect(page.locator("#toasts")).not.toContainText("Still working");
  await expect(page.locator("#toasts")).toContainText("Still working", { timeout: 15_000 });
  release();
  await expect(page.locator("#settings-body")).toHaveCount(0); // replaced by the form
  await expect(page.locator("form[data-track-changes]")).toBeVisible();
});
