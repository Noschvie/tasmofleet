import { test, expect } from "../support/fixtures";
import { contrast } from "../support/contrast";
import { menuFloatProblem } from "../support/layout";
import { hitBox } from "../support/target";
import { pickForUpdate, txnState } from "../support/updates";
import type { Page } from "@playwright/test";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Updates and transactions: the behaviour of the pages that only a browser shows
// (script hints, the device list on a phone, the open log while the page refreshes).
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv(["-releases", "3"]); // three releases: the oldest folds under "Older releases"
  const fwPort = await freePort(); // the transaction pages come with the firmware server
  app = await startApp({
    env: {
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_GITHUB_URL,
    },
  });
  expect(app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)])).toContain("created: plug-4m-http");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

test("push where possible with an ESP8266 selected: the form says it downloads instead", { tag: "@core" }, async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await pickForUpdate(page, app.url, []);
  const hint = page.locator("[data-push-hint]");
  const delivery = page.locator("select[name=delivery]");
  await expect(hint).toBeHidden();
  await page.getByLabel("Select plug-4m-http").check();
  await expect(hint).toBeHidden(); // download delivery: every chip family can take it
  await delivery.selectOption("push");
  await expect(hint).toBeVisible();
  await expect(hint).toContainText("ESP32 devices only");
  await expect(hint).toContainText("1 selected device is an ESP8266");
  await expect(hint).toContainText("downloads its file instead");
  await delivery.selectOption("");
  await expect(hint).toBeHidden();
  // an ESP32 alone is fine for a push
  await delivery.selectOption("push");
  await page.getByLabel("Select plug-4m-http").uncheck();
  await page.getByLabel("Select esp32-dimmer").check();
  await expect(hint).toBeHidden();
});

// Why: the device list scrolls inside its card (at most 340 px, as in the prototype), so the
// release, the options and "Review update" stay within reach on a phone however large the fleet is.
test("the update picker's device list scrolls in its own card on a phone", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/updates/new");
  const list = page.locator(".upd-pick-list");
  const box = await list.evaluate((el) => ({ max: getComputedStyle(el).maxHeight, overflow: getComputedStyle(el).overflowY, h: el.getBoundingClientRect().height }));
  expect(box).toMatchObject({ max: "340px", overflow: "auto" });
  expect(box.h).toBeLessThanOrEqual(340);
  // a row is one tap target: the whole row ticks its device
  const row = page.locator("label.upd-pick-row", { hasText: "esp32-dimmer" });
  const before = await row.locator("input").isChecked();
  await row.locator(".upd-pick-name").click();
  expect(await row.locator("input").isChecked()).toBe(!before);
});

// Why: the "Select" quick selection is a menu like the row menus. In the preview review
//  it opened in the flow of the "1 · Devices" head, which grew to the menu's
// height and pushed the list down. It must float over the list instead, whole and on top (the
// list's card clips its overflow), on a desktop and on a phone.
for (const vp of [{ width: 1280, height: 800 }, { width: 375, height: 812 }]) {
  test(`the update picker's Select menu floats over the device list at ${vp.width} px`, async ({ page }) => {
    await page.setViewportSize(vp);
    await page.setExtraHTTPHeaders(admin);
    await page.goto(app.url + "/updates/new");
    const summary = page.locator(".upd-quick > summary");
    expect(await menuFloatProblem(summary)).toBeNull();
    const head = page.locator(".upd-pick .ulist-head");
    const h = (await head.boundingBox())!.height;
    await summary.click();
    const none = page.locator(".upd-quick .menu-item", { hasText: "None" });
    await expect(none).toBeVisible();
    expect((await head.boundingBox())!.height).toBe(h);
    // on top: the menu's last item is what a click there hits, not the list below it
    const b = (await none.boundingBox())!;
    expect(await page.evaluate(([x, y]) => !!document.elementFromPoint(x, y)?.closest(".upd-quick .menu-item"), [b.x + b.width / 2, b.y + b.height / 2])).toBe(true);
    await none.click();
    await expect(page.locator("[data-select-n]")).toHaveText("0 selected");
  });
}

/** openTxnLog sends a command to two devices and opens the log of the second one on the finished transaction's page. */
async function openTxnLog(page: Page): Promise<string> {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  for (const name of ["plug-4m-http", "plug-4m"]) {
    const row = page.getByRole("row").filter({ hasText: new RegExp(`${name}(\\s|$)`) });
    await expect(row.getByText("Online", { exact: true })).toBeVisible({ timeout: 30_000 });
    await row.getByRole("checkbox").check();
  }
  await page.locator("[data-bulkbar]").getByRole("button", { name: /Send command/ }).click();
  await page.getByRole("textbox", { name: "Command" }).fill("Status 2");
  await page.getByRole("button", { name: "Send to 2 devices" }).click();
  await expect(page.getByRole("heading", { name: "Send Status 2 to 2 devices?" })).toBeVisible();
  await page.getByRole("button", { name: "Send to 2 devices" }).click();
  await expect(page).toHaveURL(/\/devices\/bulk\/result\/\d+/);
  await page.goto(page.url().replace(/\/devices\/bulk\/result\/(\d+).*$/, "/transactions/$1")); // the transaction page keeps the logs
  await expect(txnState(page)).toHaveText("Succeeded", { timeout: 60_000 });

  const second = page.locator(".upd-row").nth(1);
  const name = (await second.locator(".upd-row-name").innerText()).trim();
  await second.click();
  await expect(page).toHaveURL(/\?job=\d+$/);
  await expect(page.locator("#log-h")).toHaveText(`Log · ${name}`);
  return name;
}

// Why: the page re-reads itself on every job event and every 3 s while it runs; the reader's open
// log must stay on the device they picked, also when two refreshes are in flight at once.
test("the open log stays on its device while the page refreshes itself", async ({ page }) => {
  const name = await openTxnLog(page);
  await page.evaluate(async () => {
    const htmx = (window as unknown as { htmx: { ajax: (m: string, u: string, o: object) => Promise<unknown> } }).htmx;
    const url = document.getElementById("txn")!.getAttribute("hx-get") ?? "";
    await Promise.all([1, 2].map(() => htmx.ajax("GET", url, { target: "#txn", swap: "innerHTML" })));
  });
  await page.waitForTimeout(300); // the settle step runs after the swap
  await expect(page.locator("#log-h")).toHaveText(`Log · ${name}`);
  await expect(page.locator(".upd-row[aria-current=true] .upd-row-name")).toHaveText(name);
});

// Why (#1075): the quick selection and "Older releases" are the design system's link buttons (32 px target), and
// the log's links reach 24 px: they were 18, 24 and 15 px tall, too small for a finger or a tremor (WCAG 2.5.8).
test("the new update's links are real targets at 1280 px", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(admin);
  const older = page.locator(".upd-older > summary");
  await expect(async () => {
    await page.goto(app.url + "/updates/new");
    await expect(older).toBeVisible({ timeout: 1000 }); // the releases come from the fake upstream once it is fetched
  }).toPass({ timeout: 30_000 });
  const quick = page.locator(".upd-quick > summary");
  const missed = [...(await hitBox(quick)), ...(await hitBox(older))];
  expect(missed).toEqual([]);
  for (const [what, el] of [["Select", quick], ["Older releases", older]] as const) {
    const h = (await el.boundingBox())!.height;
    expect(h, `${what} is a link button: 32 px high`).toBeGreaterThanOrEqual(32);
  }
  // making them taller must not make the head taller
  const head = (await page.locator("#dev-h").evaluate((e) => e.closest(".ulist-head")!.getBoundingClientRect().height));
  expect(head).toBeLessThan(50);
});

test("the open log's links are 24 px targets at 1280 px", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await openTxnLog(page);
  const links = page.locator(".upd-log-acts a, .upd-log-acts button");
  expect(await links.count()).toBeGreaterThan(0);
  const missed: string[] = [];
  for (let i = 0; i < (await links.count()); i++) missed.push(...(await hitBox(links.nth(i))));
  expect(missed).toEqual([]);
});

// Why (#419-U review): between 45rem and 75rem the card stacks are two balanced columns, so the Devices
// card is only half the page wide there; a second column of rows squeezed the names to a letter per
// line. Two columns of rows only where the wide stack spans two columns (from 75rem).
for (const width of [800, 1024]) {
  test(`the update picker keeps device names readable at ${width} px`, async ({ page }) => {
    test.setTimeout(120_000);
    await page.setViewportSize({ width, height: 900 });
    await page.setExtraHTTPHeaders(admin);
    await page.goto(app.url + "/updates/new");
    // the longest row: a device whose login failed carries the reason beside its chip and version
    await expect(async () => {
      await page.reload();
      await expect(page.locator(".upd-pick-why", { hasText: "Login to the device failed" })).toBeVisible();
    }).toPass({ timeout: 60_000 });
    const squeezed = await page
      .locator(".upd-pick-name")
      .evaluateAll((els) => els.filter((e) => e.getBoundingClientRect().width < 96).map((e) => e.textContent?.trim()));
    expect(squeezed, "device names narrower than 6rem").toEqual([]);
  });
}

// Why (#419-U review): the update preview splits the free width 3:2 between the devices and "What
// happens" (flex 2 1 300px); the New update page's plan rule must not reach it and shrink it to 1 part.
test("the update preview keeps What happens at two parts of the width", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.setExtraHTTPHeaders(admin);
  await pickForUpdate(page, app.url, ["esp32-dimmer"]);
  await page.getByRole("button", { name: "Review update" }).click();
  const what = page.locator(".upd-what");
  await expect(what).toBeVisible();
  expect(await what.evaluate((e) => getComputedStyle(e).flexGrow)).toBe("2");
});

// Why (#1073): the progress bar drew each state in a tint of its hue, the running part at 55 % opacity: 2.2 to 2.7:1
// against the track (3:1 is the least a graphic needs), and done and running differed by hue alone, so a user who
// cannot tell green and blue apart could not read the progress. Every part now reaches 3:1 in both themes, and the
// states differ by shape: done is one solid bar, running is stripes with gaps, failed is a checker over two rows.
// The styleguide's transaction card has all three.
for (const scheme of ["light", "dark"] as const) {
  test(`the transaction bar's parts reach 3:1 against the track and differ by shape (${scheme})`, async ({ page }) => {
    await page.emulateMedia({ colorScheme: scheme });
    await page.setExtraHTTPHeaders(admin);
    await page.goto(app.url + "/styleguide");
    const bar = page.locator(".txn-card .seg-bar").first();
    await expect(bar).toBeVisible();
    const track = await bar.evaluate((el) => getComputedStyle(el).backgroundColor);
    const parts = await bar.locator("rect").evaluateAll((rs) =>
      rs.map((r) => {
        const cs = getComputedStyle(r);
        return { tone: [...r.classList].find((c) => c.startsWith("t-")) ?? "", fill: cs.fill, opacity: cs.opacity, y: r.getAttribute("y"), x: Number(r.getAttribute("x")), w: Number(r.getAttribute("width")) };
      }),
    );
    expect(parts.length).toBeGreaterThan(3);
    for (const p of parts) {
      expect(p.opacity, `${p.tone}: a dimmed part loses contrast`).toBe("1");
      expect(await contrast(page, p.fill, track), `${scheme}: ${p.tone} ${p.fill} on the track ${track}`).toBeGreaterThanOrEqual(3);
    }
    const of = (tone: string) => parts.filter((p) => p.tone === tone);
    expect(of("t-ok"), "done is one solid bar").toHaveLength(1);
    const run = of("t-accent");
    expect(run.length, "running is stripes").toBeGreaterThan(2);
    for (let i = 1; i < run.length; i++) expect(run[i].x, "a gap between two stripes").toBeGreaterThan(run[i - 1].x + run[i - 1].w);
    expect(new Set(of("t-danger").map((p) => p.y)).size, "failed is a checker over two rows").toBe(2);
  });
}
