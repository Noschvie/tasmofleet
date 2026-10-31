import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { expectNoOverflow } from "../support/layout";
import {
  App,
  Devenv,
  freePort,
  startApp,
  startDevenv,
  writeTasmoAdminDir,
} from "../support/stack";

// Design v2, package F5 (D19): table columns can be pinned to the left or the right edge, and pinned
// columns stay in view while a table that is wider than the page scrolls sideways. Power and the row
// actions are the controls you want next to the row wherever the table scrolls, the name says which
// row it is. The 0.10.1 guarantees hold on both sides: a pinned column is never narrower than its
// content, and a row menu is never clipped. On a phone the device list is cards, whose selection
// drives the same bulk bar.
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
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE:
        env.vars.DEVENV_UPSTREAM_GITHUB_URL,
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

async function open(page: Page, width: number) {
  await page.setViewportSize({ width, height: 900 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  await expect(
    page
      .locator(width < 720 ? "li.dcard" : "table[data-table=devices] tbody tr")
      .first(),
  ).toBeVisible();
  await page
    .locator(".pwr")
    .first()
    .waitFor({ state: "attached", timeout: 20_000 });
}

async function postPref(page: Page, doc: unknown): Promise<number> {
  return page.evaluate(async (d) => {
    const token = JSON.parse(document.body.getAttribute("hx-headers") ?? "{}")[
      "X-CSRF-Token"
    ];
    const r = await fetch(
      `${document.body.getAttribute("data-prefs")}/table.devices`,
      {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-CSRF-Token": token },
        body: JSON.stringify(d),
      },
    );
    return r.status;
  }, doc);
}

const heads = (page: Page) =>
  page
    .locator("table[data-table=devices] thead th")
    .evaluateAll((t) =>
      t
        .filter((x) => (x as HTMLElement).offsetParent !== null)
        .map((x) => x.textContent!.trim())
        .filter((x) => x && x !== "Select" && x !== "Actions"),
    )
    .catch(() => [] as string[]);

const row = (page: Page, id: string) =>
  page.locator(`details.colmenu .colrow[data-colrow="${id}"]`);

test("Pin left and Pin right regroup the columns and survive a reload; Name and Actions stay fixed", async ({
  page,
}) => {
  await open(page, 1280);
  expect(await postPref(page, {})).toBe(204);
  await page.reload();
  const menu = page.locator("details.colmenu").first();
  await menu.locator("summary").click();
  // defaults: Name pinned left and Actions pinned right for good, Power pinned right
  await expect(
    menu.locator(".colrow.is-fixed", { hasText: "Name" }),
  ).toContainText("Pinned left · always");
  await expect(
    menu.locator(".colrow.is-fixed", { hasText: "Actions" }),
  ).toContainText("Pinned right · always");
  await expect(menu.locator(".colrow.is-fixed button")).toHaveCount(0); // nothing to pin, move or hide there
  await expect(
    row(page, "power").getByRole("button", { name: "Pin right" }),
  ).toHaveAttribute("aria-pressed", "true");

  // Status to the right: it joins Power before Actions; the page reloads with the menu open.
  // The reload is awaited itself (its load event): a column may already sit where the pin puts it,
  // so the order of the heads cannot tell the old page from the new one, and acting on the old
  // one races the reload.
  const reloaded = () => page.waitForEvent("load", { timeout: 20_000 });
  let saved = page.waitForResponse(
    (r) => r.url().endsWith("/prefs/table.devices") && r.status() === 204,
  );
  let loaded = reloaded();
  await row(page, "status").getByRole("button", { name: "Pin right" }).click();
  await saved;
  await loaded;
  await expect
    .poll(async () => (await heads(page)).slice(-2), { timeout: 20_000 })
    .toEqual(["Status", "Power"]);
  // the page opens the menu again once enhance() has fitted the table: that takes a while on a slow runner
  await expect(page.locator("details.colmenu[open]").first()).toBeVisible({
    timeout: 20_000,
  });
  // Wi-Fi to the left: right after Name
  saved = page.waitForResponse(
    (r) => r.url().endsWith("/prefs/table.devices") && r.status() === 204,
  );
  loaded = reloaded();
  await row(page, "wifi").getByRole("button", { name: "Pin left" }).click();
  await saved;
  await loaded;
  await expect
    .poll(async () => (await heads(page)).slice(0, 2), { timeout: 20_000 })
    .toEqual(["Name", "Wi-Fi"]);
  // the page that came out of the reload opens the menu again
  await expect(page.locator("details.colmenu[open]").first()).toBeVisible({
    timeout: 20_000,
  });
  await page.reload();
  // heads() reads once and answers [] while a navigation is under way: poll it, with the time a
  // slow runner needs to lay the table out again
  await expect
    .poll(async () => (await heads(page)).slice(0, 2), { timeout: 20_000 })
    .toEqual(["Name", "Wi-Fi"]);
  await expect
    .poll(async () => (await heads(page)).slice(-2), { timeout: 20_000 })
    .toEqual(["Status", "Power"]);
  await expect(page.locator("table[data-table=devices]")).toHaveClass(
    /\bpl3\b/,
    { timeout: 20_000 },
  );

  // pressing a pressed pin unpins: Wi-Fi goes back among the scrolling columns
  await page.locator("details.colmenu > summary").first().click();
  saved = page.waitForResponse(
    (r) => r.url().endsWith("/prefs/table.devices") && r.status() === 204,
  );
  await row(page, "wifi").getByRole("button", { name: "Pin left" }).click();
  await saved;
  await expect
    .poll(async () => (await heads(page))[1], { timeout: 20_000 })
    .not.toBe("Wi-Fi");

  // the server refuses to unpin or move Actions or Name, whatever the browser sends
  expect(await postPref(page, { p: { actions: "none" } })).toBe(400);
  expect(await postPref(page, { p: { name: "right" } })).toBe(400);
  expect(await postPref(page, {})).toBe(204);
});

// #273 (and #12 before it): a device event replaces the list, Columns menu included, with a closed
// one, and the page opens it again. It used to learn "this menu is open" from the toggle event,
// which comes a task after the menu opened; a swap that landed in between (a pin reloads the page
// with the menu open, and the first device events arrive just then) left the menu shut for good.
// The swap is done in the same task as the opening here, so the order does not depend on timing.
test("a list swap right after the Columns menu opened keeps it open", async ({
  page,
}) => {
  await open(page, 1280);
  const opened = await page.evaluate(() => {
    const live = document.querySelector("#devtable")!;
    const menu = live.querySelector<HTMLDetailsElement>("details.colmenu")!;
    menu.open = true;
    // what htmx does for a swap of #devtable, with no task between the opening and the swap
    live.dispatchEvent(
      new CustomEvent("htmx:beforeSwap", {
        bubbles: true,
        detail: { target: live },
      }),
    );
    const fresh = menu.cloneNode(true) as HTMLDetailsElement;
    fresh.open = false; // the server renders it closed
    menu.replaceWith(fresh);
    live.dispatchEvent(new CustomEvent("htmx:afterSettle", { bubbles: true }));
    return live.querySelector<HTMLDetailsElement>("details.colmenu")!.open;
  });
  expect(opened).toBe(true);
  await expect(page.locator("details.colmenu[open]").first()).toBeVisible();
});

// #273: a pin saves, asks for a reload and leaves a note for the NEXT page to open the Columns
// menu. The page that asked keeps running until the new one arrives (a slow runner: seconds), and
// a refit of it used to read the note, so the page going away opened the menu and the new one
// stayed shut. Here the note is left for another page and the running one refits: it must not
// open its menu; the page that starts afterwards does.
test("the reopen note of a pin is for the next page, not for the one that is about to go", async ({
  page,
}) => {
  await open(page, 1280);
  await page.evaluate(() => {
    sessionStorage.setItem("tf-colmenu", "devices");
    document
      .querySelector("#devtable")!
      .dispatchEvent(new CustomEvent("htmx:afterSettle", { bubbles: true }));
  });
  await expect(page.locator("details.colmenu").first()).not.toHaveAttribute(
    "open",
    "",
  );
  await page.reload();
  await expect(page.locator("details.colmenu[open]").first()).toBeVisible();
});

for (const saved of [false, true]) {
  test(`a table wider than the page scrolls inside its frame; pinned columns stay at their edge (${saved ? "saved layout" : "default layout"})`, async ({
    page,
  }) => {
    await open(page, 800);
    expect(
      await postPref(
        page,
        saved
          ? {
              w: {
                name: 19,
                status: 17,
                firmware: 12,
                family: 8,
                address: 13,
                uptime: 9,
                wifi: 9,
                power: 3,
                actions: 3,
              },
            }
          : {},
      ),
    ).toBe(204);
    await page.reload();
    await page
      .locator(".pwr")
      .first()
      .waitFor({ state: "attached", timeout: 20_000 });
    const scroller = page.locator(".tablewrap[data-key=devices] .tablescroll");
    // A device event reloads the list (and resets the scroll) at any time, about 1.6 s after
    // the page loads in practice (#99). The new table comes from the server without the widths
    // the script gives it, and the frame may keep its .is-js, so the class alone does not say the
    // table is ready: measured then it does not overflow yet (max 0). enhance() sets the pinned
    // offsets (--pl1, see stick() in tables.js) on every table it has fitted, so measure only a
    // frame with .is-js and a laid-out table that has them, and look and measure in one step.
    const geo = async (toEnd = false) => {
      const read = () =>
        scroller.evaluate((s, toEnd) => {
          const table = s.querySelector<HTMLElement>(
            "table[data-table=devices]",
          );
          if (
            !s.classList.contains("is-js") ||
            !table ||
            !table.style.getPropertyValue("--pl1")
          )
            return null;
          if (!s.clientWidth || !table.getBoundingClientRect().width)
            return null; // not laid out yet
          if (toEnd) s.scrollLeft = s.scrollWidth;
          const box = (sel: string) =>
            [...s.querySelectorAll("thead th")]
              .find((t) => t.textContent!.trim().startsWith(sel))!
              .getBoundingClientRect();
          const w = s.getBoundingClientRect();
          return {
            left: w.left,
            right: w.right,
            name: box("Name").left,
            powerRight: box("Power").right,
            scroll: s.scrollLeft,
            max: s.scrollWidth - s.clientWidth,
          };
        }, toEnd);
      let g: Awaited<ReturnType<typeof read>> = null;
      await expect
        .poll(async () => (g = await read()) !== null, { timeout: 15_000 })
        .toBe(true);
      return g!;
    };
    const start = await geo();
    expect(
      start.max,
      "the table is wider than the page at 800 px and scrolls",
    ).toBeGreaterThan(20);
    await expectNoOverflow(
      page,
      "devices at 800 px: only the table scrolls, never the page",
    );
    const end = await geo(true);
    expect(end.scroll).toBeGreaterThan(0);
    // Name stays at the left edge (after the checkbox), Power at the right edge before Actions
    expect(
      Math.abs(end.name - start.name),
      "Name keeps its place",
    ).toBeLessThan(1);
    expect(
      Math.abs(end.powerRight - start.powerRight),
      "Power keeps its place",
    ).toBeLessThan(1);
    expect(start.powerRight).toBeLessThanOrEqual(start.right);
    expect(end.name).toBeGreaterThanOrEqual(end.left);
    // the pinned cells paint over the scrolling ones: a click at Power's header reaches it
    // (scrolled and hit-tested in one task, retried if a reload replaced the table meanwhile)
    await expect
      .poll(
        () =>
          scroller.evaluate((s) => {
            s.scrollLeft = s.scrollWidth;
            const th = [...s.querySelectorAll("thead th")].find((t) =>
              t.textContent!.trim().startsWith("Power"),
            )!;
            const b = th.getBoundingClientRect();
            const at = document.elementFromPoint(
              b.left + b.width / 2,
              b.top + b.height / 2,
            );
            return s.scrollLeft > 0 && !!at && th.contains(at);
          }),
        { message: "Power's header lies on top while scrolled" },
      )
      .toBe(true);
    expect(await postPref(page, {})).toBe(204);
  });
}

test("pinned columns are never narrower than their content, on either side; row menus are never clipped", async ({
  page,
}) => {
  await open(page, 800);
  // the narrowest shares the server accepts, for columns pinned on both sides
  expect(
    await postPref(page, {
      w: {
        name: 3,
        status: 3,
        firmware: 30,
        family: 20,
        address: 20,
        uptime: 20,
        wifi: 20,
        power: 3,
        actions: 3,
      },
      p: { status: "left" },
    }),
  ).toBe(204);
  await page.reload();
  await page
    .locator(".pwr")
    .first()
    .waitFor({ state: "attached", timeout: 20_000 });
  await expect(page.locator("table[data-table=devices]")).toHaveClass(
    /\btfix\b/,
  );
  const clipped = () =>
    page.evaluate(() => {
      const t = document.querySelector("table[data-table=devices]")!;
      const pinned = [...t.classList]
        .map((c) => /^p[lr](\d+)$/.exec(c))
        .filter(Boolean)
        .map((m) => parseInt(m![1], 10));
      const out: string[] = [];
      for (const r of t.querySelectorAll("tbody tr")) {
        for (const n of pinned) {
          const c = (r as HTMLTableRowElement).cells[n - 1];
          if (c && c.scrollWidth > c.clientWidth + 1)
            out.push(`${c.dataset.label}: ${c.scrollWidth} > ${c.clientWidth}`);
        }
      }
      return out;
    });
  await expect.poll(clipped, { timeout: 5_000 }).toEqual([]);

  // scroll half way and open the last row's menu: it lies on top, inside the window, wholly clickable
  const scroller = page.locator(".tablewrap[data-key=devices] .tablescroll");
  await scroller.evaluate(
    (s) => (s.scrollLeft = (s.scrollWidth - s.clientWidth) / 2),
  );
  const last = page.locator("table[data-table=devices] tbody tr").last();
  await last.locator("details.rowmenu > summary").click();
  const item = last
    .locator(".rowmenu-body")
    .getByText("Back up now", { exact: true });
  await expect(item).toBeVisible();
  const hit = await item.evaluate((e) => {
    const b = e.getBoundingClientRect();
    const x = b.left + b.width / 2,
      y = b.top + b.height / 2;
    const at = document.elementFromPoint(x, y);
    const body = e.closest(".rowmenu-body")!.getBoundingClientRect();
    return {
      inView:
        body.left >= 0 &&
        body.top >= 0 &&
        body.right <= innerWidth &&
        body.bottom <= innerHeight,
      hits: !!at && (e === at || e.contains(at)),
    };
  });
  expect(hit.inView, "the open menu lies inside the window").toBe(true);
  expect(hit.hits, "a click at a menu item reaches it").toBe(true);
  await last.locator("details.rowmenu > summary").click();
  expect(await postPref(page, {})).toBe(204);
});

test("phone: the device cards' selection drives the bulk bar and is shared with the table", async ({
  page,
}) => {
  await open(page, 375);
  await expect(page.locator("table[data-table=devices]")).toBeHidden();
  const bar = page.getByRole("region", { name: "Selected devices" });
  await expect(bar).toBeHidden();
  const cards = page.locator("li.dcard");
  const n = await cards.count();
  expect(n).toBeGreaterThan(2);
  await cards.nth(0).getByRole("checkbox").check();
  await expect(bar).toBeVisible();
  await expect(bar).toContainText("1 device selected");
  // the phone bar: count and Clear, then the actions with their short labels, inside the window
  await expect(
    bar.getByRole("button", { name: "Command", exact: true }),
  ).toBeVisible();
  await expect(
    bar.getByRole("button", { name: "Power on", exact: true }),
  ).toBeVisible();
  const box = (await bar.boundingBox())!;
  expect(box.x).toBeGreaterThanOrEqual(0);
  expect(box.x + box.width).toBeLessThanOrEqual(375);
  expect(box.y + box.height).toBeLessThanOrEqual(900 + 0.5);
  // Select all N ticks every card
  await page.getByRole("checkbox", { name: "Select all devices" }).check();
  await expect(bar).toContainText(`${n} devices selected`);
  await bar.getByRole("button", { name: "Clear", exact: true }).click();
  await expect(bar).toBeHidden();
  await expect(page.locator("input[name=id]:checked")).toHaveCount(0);

  // a card's tick is its row's tick: the same selection on a wider screen
  await cards.nth(1).getByRole("checkbox").check();
  const id = await cards.nth(1).getAttribute("data-id");
  await page.setViewportSize({ width: 1280, height: 900 });
  await expect(
    page.locator(`table[data-table=devices] input[name=id][value="${id}"]`),
  ).toBeChecked();
  await expect(bar).toContainText("1 device selected");
  await expect(
    bar.getByRole("button", { name: "Send command", exact: true }),
  ).toBeVisible();
});
