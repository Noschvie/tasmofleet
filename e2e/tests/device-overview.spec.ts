import { mkdirSync } from "node:fs";
import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { expectNoOverflow } from "../support/layout";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// The device Overview (#145) and its live refresh (#126): tiles, relay box, Identity | Connection,
// Activity; the page follows the device (a button pressed on it, a pulled plug) without a reload.
// UX_SHOTS=<dir> keeps screenshots (375 and 1280, light and dark).
const shots = process.env.UX_SHOTS;
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
let app: App;
let env: Devenv;

test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv(["-overview-shapes"]);
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

async function control(path: string) {
  const r = await fetch(env.vars.DEVENV_CONTROL_URL + path, { method: "POST" });
  expect(r.status, path).toBe(204);
}

async function shot(page: Page, name: string) {
  if (!shots) return;
  mkdirSync(shots, { recursive: true });
  // a full-page capture draws a sticky bar where the scroll position happens to be: park it at the top
  await page.evaluate(() => {
    for (const el of document.querySelectorAll<HTMLElement>("body *")) {
      const pos = getComputedStyle(el).position;
      if (pos === "sticky" || pos === "fixed") if (!el.closest("dialog")) el.style.position = "static";
    }
    window.scrollTo(0, 0);
  });
  await page.screenshot({ path: `${shots}/${name}.png`, fullPage: true });
}

// A probe renames an entry that still carries its imported name to the device's own DeviceName (#125),
// so a device is looked up by name once per worker and by address afterwards.
const hrefs = new Map<string, string>();

/** open the overview of a device once it has reported its relay state */
async function openDevice(page: Page, name: string, width = 1280) {
  await page.setExtraHTTPHeaders(adam);
  await page.setViewportSize({ width, height: 900 });
  await page.goto(app.url + "/devices");
  await page.locator("table[data-table=devices] .pwr:not([disabled])").first().waitFor({ state: "attached", timeout: 30_000 });
  const href = hrefs.get(name) ?? (await page.getByRole("link", { name, exact: true }).first().getAttribute("href"))!;
  hrefs.set(name, href);
  await page.goto(app.url + href);
  await expect(page.locator("#devlive .dev-relay, #devlive .dev-sensor").first()).toBeVisible({ timeout: 30_000 });
  return href;
}

/**
 * refreshIdentity clicks Refresh identity and waits for its answer before it looks for the notice (#1041). A device read
 * over MQTT is checked first with the window that hears every reply on its topic (1.5 s, #682/#804), then read one
 * command at a time 200 ms apart (about 3 s), and another check on the same topic that is under way delays it by its own
 * window: the answer takes 3 to 5 s and more on a loaded runner, so a notice awaited inside expect's 5 s fails there. The
 * notice is asserted once the answer is in: an answer without it, or a card the page does not swap in, fails.
 */
async function refreshIdentity(page: Page) {
  const answered = page.waitForResponse((r) => r.request().method() === "POST" && /\/devices\/\d+\/probe$/.test(new URL(r.url()).pathname));
  await page.getByRole("button", { name: "Refresh identity" }).click();
  await answered;
  await expect(page.getByText("Identity refreshed.")).toBeVisible();
}

for (const width of [375, 1280]) {
  for (const scheme of ["light", "dark"] as const) {
    test(`overview renders without overflow at ${width}px ${scheme}`, async ({ page }) => {
      await page.emulateMedia({ colorScheme: scheme });
      await openDevice(page, "plug-4m", width);
      await expect(page.locator(".dev-tile")).not.toHaveCount(0);
      await expect(page.locator("#identity")).toBeVisible();
      await expect(page.locator("#devconn")).toBeVisible();
      await expect(page.locator("#devact")).toBeVisible();
      await expectNoOverflow(page, "overview");
      await shot(page, `overview-online-${width}-${scheme}`);
    });
  }
}

// #126: a button pressed on the device shows on the open page without a reload.
test("a relay switched on the device follows live, without a reload", async ({ page }) => {
  await openDevice(page, "plug-4m");
  await page.evaluate(() => ((window as unknown as { __kept: number }).__kept = 1));
  const sw = page.locator("#devlive .dev-relay [role=switch]").first();
  const was = await sw.getAttribute("aria-checked");
  await control("/device/plug-4m/relay/1/" + (was === "true" ? "off" : "on"));
  await expect(sw).toHaveAttribute("aria-checked", was === "true" ? "false" : "true", { timeout: 20_000 });
  expect(await page.evaluate(() => (window as unknown as { __kept: number }).__kept)).toBe(1); // same document: no reload
});

// #145: the switch of the relay box itself updates the box in place.
test("the relay box switch updates in place", async ({ page }) => {
  await openDevice(page, "lamp-1m");
  await page.evaluate(() => ((window as unknown as { __kept: number }).__kept = 1));
  const sw = page.locator("#devlive .dev-relay [role=switch]").first();
  const was = await sw.getAttribute("aria-checked");
  await sw.click();
  await expect(sw).toHaveAttribute("aria-checked", was === "true" ? "false" : "true", { timeout: 20_000 });
  expect(await page.evaluate(() => (window as unknown as { __kept: number }).__kept)).toBe(1);
});

// #145 mockup 8 and #126: a pulled plug turns the page offline by itself: banner, dimmed state,
// disabled switches and actions; Edit, Delete and Activity stay; the Activity list gets the new row.
test("a pulled plug shows the offline banner, dims and disables, and adds an Activity row live", async ({ page }) => {
  await openDevice(page, "esp32c3-sensor");
  await expect(page.locator("#devhd-off")).toHaveCount(0);
  const rows = await page.locator("#devact .dev-act li").count();
  await control("/device/esp32c3-sensor/off");
  await expect(page.locator("#devhd-off")).toContainText("Offline since", { timeout: 60_000 });
  await expect(page.locator("#devlive")).toHaveAttribute("data-off", "");
  await expect(page.locator("#identity")).toHaveClass(/is-dim/);
  await expect(page.locator("#devconn")).toHaveClass(/is-dim/);
  await expect(page.getByRole("button", { name: "Refresh identity" })).toBeDisabled();
  await expect(page.getByRole("button", { name: "Restart" })).toBeDisabled();
  await expect(page.getByRole("button", { name: "Back up" })).toBeDisabled();
  for (const sw of await page.locator("#devlive [role=switch]").all()) await expect(sw).toBeDisabled();
  await expect(page.getByRole("link", { name: "Edit" })).toBeEnabled();
  await expect(page.getByRole("link", { name: "Delete" })).toBeEnabled();
  await expect(page.locator("#devact")).not.toHaveClass(/is-dim/);
  await expect(page.locator("#devact .dev-act li")).toHaveCount(rows + 1, { timeout: 20_000 });
  await shot(page, "overview-offline-1280");
  await expectNoOverflow(page, "overview offline");
});

// #1059: syncDeviceDim (live.js) runs for a swap of #devlive, #identity or #devconn, no longer for every settled
// swap of the page. The Identity card is swapped on its own (#1003), and the server renders it without the dimmed
// look: a card that arrives while the device is offline has to be dimmed again, and its hidden Copy button
// (copy.js) has to be shown, or the person gets a card that looks live and a button that is missing.
test("an Identity card swapped in alone is dimmed for an offline device and keeps its Copy button (#1059)", async ({ page }) => {
  await openDevice(page, "plug-4m");
  await page.locator("#devlive").evaluate((el) => el.setAttribute("data-off", ""));
  await page.evaluate(async () => {
    const html = await (await fetch(location.href)).text();
    const fresh = new DOMParser().parseFromString(html, "text/html").querySelector("#identity")!.outerHTML;
    // @ts-expect-error htmx is a page global
    window.htmx.swap(document.querySelector("#identity")!, fresh, { swapStyle: "outerHTML" });
  });
  await expect(page.locator("#identity")).toHaveClass(/is-dim/);
  await expect(page.locator("#identity")).toHaveAttribute("aria-disabled", "true");
  await expect(page.locator("#identity").getByRole("button", { name: "Copy the TasmoFleet ID" })).toBeVisible();
});

// #145: the credentials of a device never reach the page
test("no credentials in the overview", async ({ page }) => {
  const href = await openDevice(page, "locked-plug");
  const html = await page.content();
  expect(html).not.toContain("devenv-fake-password");
  expect(await page.locator("#identity").innerText()).not.toMatch(/password/i);
  expect(href).toBeTruthy();
});

// One device per mockup shape (devenv -overview-shapes). Each renders what its shape promises, and
// is looked at in 375/1280, light and dark (UX_SHOTS).
const shapes: { device: string; check: (page: Page) => Promise<void> }[] = [
  {
    device: "ov-metered-plug", // mockup 1
    check: async (page) => {
      await expect(page.locator("[data-tile=power]")).toContainText(/\d+ W/, { timeout: 30_000 });
      await expect(page.locator("[data-tile=energy]")).toContainText("kWh");
      await expect(page.locator("#devlive .dev-box-meta")).toHaveText("metered");
    },
  },
  {
    device: "ov-plain-plug", // mockup 2
    check: async (page) => {
      await expect(page.locator("[data-tile=power]")).toHaveCount(0);
      await expect(page.locator("[data-tile=energy]")).toHaveCount(0);
      await expect(page.locator("#devlive .dev-box-meta")).toHaveText("no energy meter");
    },
  },
  {
    device: "ov-double-switch", // mockup 3
    check: async (page) => {
      await expect(page.locator("#devlive .dev-relay")).toHaveCount(2);
      await expect(page.locator("#devlive .dev-box-meta")).toContainText("one meter for all", { timeout: 30_000 });
      await expect(page.locator("#devlive .dev-relay-s").first()).toHaveText(/^(On|Off)$/);
    },
  },
  {
    device: "ov-power-strip", // mockup 4
    check: async (page) => {
      await expect(page.locator("#devlive .dev-relay")).toHaveCount(4);
      await expect(page.locator("#devlive .dev-box-meta")).toContainText("of 4 on · metered per channel", { timeout: 30_000 });
      await expect(page.locator("#devlive .dev-relay-n").first()).toContainText("Monitor");
      await expect(page.locator("#devlive .dev-relay-s").first()).toHaveText(/\d+ W/);
      await expect(page.locator("[data-tile=power]")).toContainText("all channels");
    },
  },
  {
    device: "ov-ethernet-rack", // mockup 5
    check: async (page) => {
      // Ethernet is a fact the probe reads; a device that was only imported does not know it yet
      await refreshIdentity(page);
      await page.reload();
      await expect(page.locator("[data-tile=link]")).toContainText("Ethernet");
      await expect(page.locator("[data-tile=wifi]")).toHaveCount(0);
      await expect(page.getByRole("heading", { name: "Connection · Ethernet" })).toBeVisible();
      await expect(page.locator("#devconn")).toContainText("Ethernet MAC");
    },
  },
  {
    device: "ov-climate", // mockup 6
    check: async (page) => {
      await expect(page.getByRole("heading", { name: "Sensors" })).toBeVisible();
      await expect(page.locator("#devlive .dev-sensor", { hasText: "Temperature · BME280" })).toContainText("12.4 °C", { timeout: 30_000 });
      await expect(page.locator("#devlive .dev-relay")).toHaveCount(0);
    },
  },
  {
    device: "ov-http-only", // mockup 7
    check: async (page) => {
      // The HTTP-only simulator has an MqttHost of its own, a broker that is not the devenv's and that
      // no device is heard on (#580, #597): the tile names it instead of "MQTT not configured" (that wording stays for a device
      // without a broker host; unit-tested in TestReachTileNamesTheOtherBroker).
      await expect(page.locator("[data-tile=reach]")).toContainText("MQTT on another broker");
    },
  },
];

for (const shape of shapes) {
  for (const width of [375, 1280]) {
    for (const scheme of ["light", "dark"] as const) {
      test(`shape ${shape.device} at ${width}px ${scheme}`, async ({ page }) => {
        await page.emulateMedia({ colorScheme: scheme });
        await openDevice(page, shape.device, width);
        await shape.check(page);
        await expectNoOverflow(page, shape.device);
        await shot(page, `shape-${shape.device}-${width}-${scheme}`);
      });
    }
  }
}

// Ethernet facts come from a probe: Refresh identity reads subnet mask and DNS (WP0) and the card shows them (#1044).
// The simulator reports the same captured values on every read and the shape tests above already refreshed this device,
// so a refresh that read nothing would still show them. The test therefore changes what the device reports, to values
// that differ from what the page shows now and from the capture, and expects exactly those after the refresh. They are
// derived from the repetition (--repeat-each) and moved on until they differ from what an earlier run stored: the stack
// keeps the stored facts for the rest of the file.
const captured = { subnet: "255.255.255.0", dns: "172.31.13.1" };
const subnets = ["255.255.254.0", "255.255.252.0", "255.255.248.0", "255.255.240.0", "255.255.224.0", "255.255.192.0"];
test("an Ethernet device shows the subnet mask and DNS the refresh read", async ({ page }, testInfo) => {
  await openDevice(page, "ov-ethernet-rack");
  const dd = (label: string) => page.locator("#devconn").locator("dt", { hasText: label }).locator("xpath=following-sibling::dd");
  // read before the control call: the page that is open now was loaded before the values change, so it cannot have them;
  // a device nothing has been read from yet has no such row (the card is not the Ethernet card), which is no failure here
  const text = async (label: string) => ((await dd(label).count()) ? (await dd(label).textContent())?.trim() : undefined);
  const shown = { subnet: await text("Subnet mask"), dns: await text("DNS") };
  const subnet = subnets.map((_, i) => subnets[(testInfo.repeatEachIndex + i) % subnets.length]).find((v) => v !== shown.subnet && v !== captured.subnet)!;
  const dns = Array.from({ length: 254 }, (_, i) => `203.0.113.${1 + ((testInfo.repeatEachIndex + i) % 254)}`).find((v) => v !== shown.dns && v !== captured.dns)!;
  expect(shown.subnet).not.toBe(subnet);
  expect(shown.dns).not.toBe(dns);

  await control(`/device/ov-ethernet-rack/net?subnet=${subnet}&dns=${dns}`);
  await refreshIdentity(page);
  await page.reload();
  await expect(dd("Subnet mask")).toHaveText(subnet);
  await expect(dd("DNS")).toHaveText(dns);
});

// Mockup 8 in both schemes and sizes: an offline device with relays dimmed. One test, because the plug
// can only be pulled once.
test("an offline device with relays at 375/1280, light and dark", async ({ page }) => {
  await openDevice(page, "ov-offline");
  await control("/device/ov-offline/off");
  await expect(page.locator("#devhd-off")).toContainText("Offline since", { timeout: 60_000 });
  await openCards(page);
  // an HTTP answer from before the plug was pulled (the page's probe) is no "HTTP reachable" in the card either
  await expect(page.locator("[data-tile=reach]")).toContainText("HTTP no answer");
  await expect(page.locator("#devpaths-facts-sum")).not.toContainText("HTTP reachable");
  for (const width of [375, 1280]) {
    for (const scheme of ["light", "dark"] as const) {
      await page.emulateMedia({ colorScheme: scheme });
      await page.setViewportSize({ width, height: 900 });
      await expectNoOverflow(page, `offline ${width} ${scheme}`);
      await shot(page, `shape-ov-offline-${width}-${scheme}`);
    }
  }
});

// #447: a device whose HTTP API is switched off (SetOption128 0) still serves its web page. The Reachability tile,
// the Connection card and the device list say so instead of "Offline / HTTP no answer"; the device stays Online
// (a refusal is an answer), has no path for actions, and says why with one name.
async function openCards(page: Page) {
  for (const id of ["#devpaths-facts-sum", "#devpaths-actions-sum"]) {
    const el = page.locator(id);
    if ((await el.evaluate((e) => (e.parentElement as HTMLDetailsElement).open)) === false) await el.click();
  }
}

test("a device with its HTTP API off is not shown as offline", { tag: "@core" }, async ({ page }) => {
  const href = await openDevice(page, "ov-api-off");
  await control("/device/ov-api-off/api-off");
  // opening the page asks for an HTTP check; the refusal reaches the page through the live refresh
  await page.goto(app.url + href);
  const tile = page.locator("[data-tile=reach]");
  await expect(tile).toContainText("HTTP API off", { timeout: 60_000 });
  await expect(tile).toContainText("The web page answers");
  await expect(tile).not.toContainText("Offline");
  await expect(page.locator("#devpaths-facts-sum")).toContainText("HTTP API off");
  await openCards(page);
  await expect(page.locator("#devpaths-facts")).toContainText("SetOption128 1");
  await expect(page.locator("#devpaths-actions")).toContainText("HTTP API off (SetOption128 0)");
  await expect(page.locator(".devhd-state")).toHaveText("Online");
  // #645: nothing on the page tries and fails; the switches and Restart are disabled with the one reason
  await expect(page.locator("#dev-relays-why")).toContainText("HTTP API off (SetOption128 0)");
  await expect(page.locator("#devlive .devrelay-sw").first()).toBeDisabled();
  await expect(page.getByRole("button", { name: "Restart", exact: true })).toBeDisabled();
  for (const width of [375, 1280]) {
    for (const scheme of ["light", "dark"] as const) {
      await page.emulateMedia({ colorScheme: scheme });
      await page.setViewportSize({ width, height: 900 });
      await expectNoOverflow(page, `api-off ${width} ${scheme}`);
      await shot(page, `shape-ov-api-off-${width}-${scheme}`);
    }
  }
});

// #659: the live refresh never swaps the Identity card, so Refresh identity followed the state at page load only.
// It now follows the live state both ways: disabled with the reason once the refusal is found, enabled again when the
// live region stops saying so (the API answers), without a reload.
test("Refresh identity follows the live refusal both ways, without a reload", { tag: "@core" }, async ({ page }) => {
  await openDevice(page, "ov-api-flip");
  await page.evaluate(() => ((window as unknown as { __kept: number }).__kept = 1));
  const refresh = page.getByRole("button", { name: "Refresh identity" });
  await expect(refresh).toBeEnabled();
  await control("/device/ov-api-flip/api-off");
  await expect(page.locator("[data-tile=reach]")).toContainText("HTTP API off", { timeout: 60_000 });
  await expect(refresh).toBeDisabled();
  // #1062: the fix leads with the own address; SetOption128 1 (the Referer check off) comes second, with the web password
  await expect(refresh).toHaveAttribute("title", /HTTP API off \(SetOption128 0\)\. Add the device by its own IP or hostname;.*SetOption128 1.*web password/);
  // The API coming back is found by the health tracker only minutes later (a refused device is polled slowly), so the
  // way back is driven by the live region itself: what the server renders into it when it no longer says "off".
  const setReason = (reason: string | null) =>
    page.evaluate((r) => {
      const live = document.getElementById("devlive")!;
      if (r === null) live.removeAttribute("data-reads-off");
      else live.setAttribute("data-reads-off", r);
      document.dispatchEvent(new Event("htmx:afterSettle", { bubbles: true }));
    }, reason);
  const reason = (await page.locator("#devlive").getAttribute("data-reads-off"))!;
  expect(reason).toContain("HTTP API off (SetOption128 0)");
  await expect(async () => {
    // a live refresh may put the real attribute back between two steps: try again
    await setReason(null);
    await expect(refresh).toBeEnabled({ timeout: 1000 });
  }).toPass({ timeout: 15_000 });
  await expect(refresh).not.toHaveAttribute("title", /.+/);
  await expect(refresh).not.toHaveAttribute("data-refused", /.*/);
  await setReason(reason);
  await expect(refresh).toBeDisabled();
  expect(await page.evaluate(() => (window as unknown as { __kept: number }).__kept)).toBe(1); // same document: no reload
});

// The refusal is contact, not a failure: the header must not turn Offline after the polls that follow (the 3rd failed
// poll used to flip it), and the Back up button names the reason.
test("a device with its HTTP API off stays Online after more polls", { tag: "@full" }, async ({ page }) => {
  const href = await openDevice(page, "ov-api-off");
  await control("/device/ov-api-off/api-off");
  await page.goto(app.url + href);
  await expect(page.locator("[data-tile=reach]")).toContainText("HTTP API off", { timeout: 60_000 });
  await page.waitForTimeout(25_000); // a refusal counted as a failure would be polled every 5 s and turn Offline at the 3rd
  await page.goto(app.url + href);
  await expect(page.locator(".devhd-state")).toHaveText("Online");
  await expect(page.locator("[data-tile=reach]")).toContainText("HTTP API off");
  await expect(page.locator("#devhd-why")).toContainText("HTTP API off (SetOption128 0)");
});
