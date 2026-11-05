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
test("a plain POST form marks its submitter busy and is not sent twice", { tag: "@core" }, async ({ page }) => {
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

// Why: the busy state must start only after the browser has read the form, because a disabled
// button is left out of it. A quick command is a submit button whose name/value IS the command.
test("a named submitter still sends its value", { tag: "@core" }, async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  const href = await devicePath(page);
  await page.goto(app.url + href + "/console");
  const chip = page.locator("form.console-quick button.console-chip").first();
  const want = await chip.getAttribute("value");
  let body: string | null = null;
  await page.route("**" + href + "/console", async (route) => {
    if (route.request().method() === "POST") body = route.request().postData() ?? "";
    await route.continue();
  });
  await chip.click();
  await expect.poll(() => body).not.toBeNull();
  expect(new URLSearchParams(body!).get("command")).toBe(want);
});

// Why: a request that fails without an answer must not leave the control dead; the person must be
// able to try again. "Test connection" has no hx-disabled-elt, so only the busy pattern disables it.
test("a failed htmx request clears the busy state and the control works again", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  let posts = 0;
  await page.route("**/mqtt-test*", async (route) => {
    posts++;
    if (posts > 1) return route.continue();
    await gate;
    await route.abort("failed");
  });
  await page.goto(app.url + "/settings/system?tab=mqtt");
  const btn = page.getByRole("button", { name: "Test connection" });
  await btn.click();
  await expect(btn).toBeDisabled();
  await expect(btn).toHaveAttribute("aria-busy", "true");
  release();
  await expect(btn).toBeEnabled();
  await expect(btn).not.toHaveAttribute("aria-busy", "true");
  await btn.click();
  await expect.poll(() => posts).toBe(2);
});

// Why: "Save rule" is a plain POST although its form also carries hx-post (the live preview on
// change); it must be busy like every other plain form.
test("a plain form with an htmx preview is busy on its own submit", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/policy/rules/new?template=timezone");
  await page.route("**/policy/rules/save", async (route) => {
    await new Promise((r) => setTimeout(r, 1500));
    await route.continue();
  });
  const btn = page.getByRole("button", { name: "Save rule" });
  await btn.evaluate((b: HTMLButtonElement) => {
    b.form!.addEventListener("submit", () => setTimeout(() => sessionStorage.setItem("probe", JSON.stringify({ disabled: b.disabled, busy: b.getAttribute("aria-busy") })), 300), { once: true });
  });
  await btn.click();
  await expect(page).toHaveURL(/\/policy\?ok=saved/);
  const probe = JSON.parse((await page.evaluate(() => sessionStorage.getItem("probe"))) ?? "null");
  expect(probe).toEqual({ disabled: true, busy: "true" });
});

// Why: "still working" is about the request that is running now. The timer of an earlier request on
// the same control, which ended at once, must not announce it for a later one that has run 5 s.
test("the still-working note belongs to the running request, not an earlier one", async ({ page }) => {
  test.setTimeout(60_000);
  await page.setExtraHTTPHeaders(admin);
  const href = await devicePath(page);
  let posts = 0;
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  await page.route("**/probe", async (route) => {
    posts++;
    if (posts === 1) return route.abort("failed");
    await gate;
    await route.continue();
  });
  await page.goto(app.url + href);
  const btn = page.getByRole("button", { name: "Refresh identity" });
  const t0 = Date.now();
  await btn.click();
  await expect(btn).toBeEnabled();
  await page.waitForTimeout(5_000);
  await btn.click();
  await expect(btn).toHaveAttribute("aria-busy", "true");
  await page.waitForTimeout(Math.max(0, 11_000 - (Date.now() - t0)));
  // read once, not retried: a toast fades after 4.5 s, so a retrying check would just wait it out
  expect(await page.locator("#toasts").innerText()).not.toContain("Still working");
  release();
  await expect(page.getByText("Identity refreshed.")).toBeVisible();
});

// Why: the relay switch is the control people use most. Busy, it dims; a spinner ring inside its grid
// would push the knob to the top edge and hang below the switch.
test("a busy relay switch keeps its knob in place", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  // by id, not by name (Refresh identity renames devices): the first device with a switch it can use
  const hrefs = await page.locator("a[href^='/devices/']").evaluateAll((as) => [...new Set(as.map((a) => a.getAttribute("href")!).filter((h) => /^\/devices\/\d+$/.test(h)))]);
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  await page.route(/\/power\/\d+$/, async (route) => {
    await gate;
    await route.continue();
  });
  for (const h of hrefs) {
    await page.goto(app.url + h);
    if ((await page.locator("button.devrelay-sw:not([disabled])").count()) > 0) break;
  }
  const sw = page.locator("button.devrelay-sw").first();
  await expect(sw).toBeEnabled();
  const knob = () => sw.evaluate((b) => b.querySelector(".devsw")!.getBoundingClientRect().top - b.getBoundingClientRect().top);
  const idle = await knob();
  await sw.click();
  await expect(sw).toBeDisabled();
  await expect(sw).toHaveAttribute("aria-busy", "true");
  expect(await knob()).toBeCloseTo(idle, 0);
  release();
});

// Why (#538): the spinner has to turn (no ds-spin keyframes meant it never did) and must stand still
// for a person who asked for reduced motion.
test("the busy ring uses ds-spin and stops under reduced motion", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders(admin);
  const href = await devicePath(page);
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  await page.route("**/probe", async (route) => {
    await gate;
    await route.continue();
  });
  await page.goto(app.url + href);
  const btn = page.getByRole("button", { name: "Refresh identity" });
  await btn.click();
  await expect(btn).toHaveAttribute("aria-busy", "true");
  const style = () =>
    btn.evaluate((b) => ({
      content: getComputedStyle(b, "::after").content,
      anim: getComputedStyle(b, "::after").animationName,
    }));
  const normal = await style();
  await page.emulateMedia({ reducedMotion: "reduce" });
  const reduced = await style();
  release();
  expect(normal.anim).toBe("ds-spin");
  expect(reduced.anim).toBe("none");
});

// Why (#538): the list's power toggle is a switch; a ring next to its knob pushed the knob out of
// place, so a busy switch only dims.
test("a busy list power toggle has no ring and keeps its knob", async ({
  page,
}) => {
  await page.setExtraHTTPHeaders(admin);
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  await page.route(/\/power\/\d+$/, async (route) => {
    await gate;
    await route.continue();
  });
  await page.goto(app.url + "/devices");
  // pinned by its URL: once busy it is disabled, and a :not([disabled]) locator would move on
  const url = await page
    .locator("button.pwr:not([disabled]):visible")
    .first()
    .getAttribute("hx-post");
  const pwr = page.locator(`button.pwr[hx-post="${url}"]:visible`);
  await expect(pwr).toBeVisible();
  const look = () =>
    pwr.evaluate((b) => {
      const k = b.querySelector(".sw")!.getBoundingClientRect();
      const r = b.getBoundingClientRect();
      return {
        top: k.top - r.top,
        left: k.left - r.left,
        w: r.width,
        h: r.height,
        after: getComputedStyle(b, "::after").content,
      };
    });
  const idle = await look();
  await pwr.click();
  await expect(pwr).toHaveAttribute("aria-busy", "true");
  const busy = await look();
  release();
  expect(busy.after).toBe("none");
  expect(busy.top).toBeCloseTo(idle.top, 0);
  expect(busy.left).toBeCloseTo(idle.left, 0);
  expect(busy.w).toBeCloseTo(idle.w, 0);
  expect(busy.h).toBeCloseTo(idle.h, 0);
});
