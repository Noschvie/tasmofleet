import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// #594, same bug class as timestamps-no-flash.spec.ts: a script that fixes the swapped-in content up at
// htmx:afterSettle (~20 ms after the swap) lets the browser draw the server's version for a frame. Each
// test reads the state at htmx:afterSwap from a listener registered after the app's own scripts.
// (on document, like the scripts: listeners on one node run in registration order).
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
let app: App;
let env: Devenv;

test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_GITHUB_URL,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

// select.js hides the select-all controls in the server's HTML and shows them with script; after a list
// refresh they blinked away until afterSettle.
test("the select-all controls of a refreshed Devices list are shown at htmx:afterSwap", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  await expect(page.locator("#devtable [data-select-all]").first()).toBeAttached({ timeout: 40_000 });
  await expect(page.locator("#devtable [data-select-all]").first()).toBeVisible();
  await page.evaluate(() => {
    const w = window as unknown as { __sel: boolean[] };
    w.__sel = [];
    document.addEventListener("htmx:afterSwap", (e) => {
      const t = e.target as Element;
      if (t.id !== "devtable") return;
      t.querySelectorAll<HTMLElement>("[data-select-all], [data-select-all-row]").forEach((el) => w.__sel.push(el.hidden));
    });
    // @ts-expect-error htmx is a page global
    window.htmx.trigger("#devtable", "sse:resync");
  });
  await expect.poll(() => page.evaluate(() => (window as unknown as { __sel: boolean[] }).__sel.length), { timeout: 15_000 }).toBeGreaterThan(0);
  const hidden = await page.evaluate(() => (window as unknown as { __sel: boolean[] }).__sel);
  expect(hidden.every((h) => h === false), "no select-all control is hidden right after the swap").toBe(true);
});

// batch.js keeps the open log's scroll position over the refresh of #txn (every 3 s); put back at
// afterSettle, one frame was drawn with the log scrolled to the top.
test("a refreshed log keeps its scroll position at htmx:afterSwap", async ({ page }) => {
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/audit");
  const html = `<div id="log-lines" class="upd-log-body">${"<p>line</p>".repeat(100)}</div>`;
  await page.route("**/synthetic-log", (r) => r.fulfill({ contentType: "text/html", body: html }));
  const got = await page.evaluate(async (h) => {
    const host = document.createElement("div");
    host.innerHTML = h;
    document.body.appendChild(host);
    const log = () => host.querySelector("#log-lines") as HTMLElement;
    log().scrollTop = 150;
    let at = -1;
    document.addEventListener("htmx:afterSwap", () => {
      at = log().scrollTop;
    });
    // @ts-expect-error htmx is a page global
    await window.htmx.ajax("GET", "/synthetic-log", { target: host, swap: "innerHTML" });
    return { at, before: 150 };
  }, html);
  expect(got.at).toBe(got.before);
});
