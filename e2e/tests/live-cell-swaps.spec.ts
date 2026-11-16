import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { openSavedLayoutList, ownerWide, ownerWindow } from "../support/saved-layout";
import {
  App,
  Devenv,
  freePort,
  startApp,
  startDevenv,
  writeTasmoAdminDir,
} from "../support/stack";

// #726: the Devices list got about 90 cell swaps a second with the 60-device fixture, and the page fell
// behind its server by 10 s and more. Every health event of a device sends six fragments (the phone
// cards repeat four of them), changed or not, and each swap runs the page's swap scripts. These guards
// count what the page does with the stream, not how long it takes, so they do not depend on the speed
// of the machine.
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
      TASMOFLEET_POLL_INTERVAL: "1s", // a health event per device and second: repeats come quickly
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

type Seen = { messages: number; changes: number; swaps: number };

test("a live cell is swapped only when its fragment differs from the last one it received", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.setExtraHTTPHeaders(adam);
  // The counters sit on window in the capture phase, so they see every message and swap whatever the
  // page's own listeners do with it. A "change" is a message whose fragment differs from the previous
  // one of the same element (the first one of an element counts: it has nothing to compare with).
  await page.addInitScript(() => {
    const seen: Seen = { messages: 0, changes: 0, swaps: 0 };
    const last = new WeakMap<Element, string>();
    (window as unknown as { __seen: Seen }).__seen = seen;
    window.addEventListener(
      "htmx:sseBeforeMessage",
      (e) => {
        const t = e.target as Element;
        if (!t.matches?.("table [sse-swap], li.dcard [sse-swap]")) return;
        const data = (e as CustomEvent).detail.data as string;
        seen.messages++;
        if (last.get(t) !== data) seen.changes++;
        last.set(t, data);
      },
      true,
    );
    window.addEventListener(
      "htmx:afterSwap",
      (e) => {
        const t = e.target as Element;
        if (t.matches?.("table [sse-swap], li.dcard [sse-swap]")) seen.swaps++;
      },
      true,
    );
  });
  await page.goto(app.url + "/devices");
  await expect(page.locator("#devtable tbody tr").first()).toBeAttached({
    timeout: 40_000,
  });
  // enough repeated fragments to tell "skipped" from "not seen yet"
  const read = () =>
    page.evaluate(() => (window as unknown as { __seen: Seen }).__seen);
  await expect
    .poll(
      async () => {
        const s = await read();
        return s.messages - s.changes;
      },
      { timeout: 60_000, intervals: [500] },
    )
    .toBeGreaterThanOrEqual(40);
  const s = await read();
  expect(
    s.swaps,
    `${s.messages} messages, ${s.changes} of them differ from the previous one of their cell`,
  ).toBeLessThanOrEqual(s.changes);

  // The memo must not hide a cell that was changed by something else: a swap of the element (a power
  // toggle's response lands in the same cell) drops it, and the next fragment of the stream puts the
  // real content back.
  const cell = page.locator("#devtable tbody tr td.pcell").first();
  // read in the same task as the swap: with a health event a second the stream may put the content back at once
  const swapped = await cell.evaluate((el) => {
    // @ts-expect-error htmx is a page global
    window.htmx.swap(el, "<span>stale</span>", { swapStyle: "innerHTML" });
    return el.textContent;
  });
  expect(swapped).toContain("stale");
  await expect(cell).not.toContainText("stale", { timeout: 15_000 });
});

// syncAll (select.js) walks the whole document: it shows the select-all controls and fills the bulk bar
// (#594). It runs when a swap brings those controls in (the list, the scan results), not for the single
// cells of the live stream, which settle one by one (htmx:afterSettle still runs it for them).
test("a live cell swap does not run the document-wide select sync, a list reload does", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.setExtraHTTPHeaders(adam);
  type Sync = { runs: number; cell: number[]; list: number[] };
  await page.addInitScript(() => {
    // syncAll asks the document for the select-all controls once per run; nothing else does.
    const w = window as unknown as { __sync: Sync };
    w.__sync = { runs: 0, cell: [], list: [] };
    const qsa = document.querySelectorAll.bind(document);
    document.querySelectorAll = ((sel: string) => {
      if (sel === "[data-select-all]") w.__sync.runs++;
      return qsa(sel);
    }) as typeof document.querySelectorAll;
  });
  await page.goto(app.url + "/devices");
  await expect(
    page.locator("#devtable [data-select-all]").first(),
  ).toBeAttached({ timeout: 40_000 });
  // Registered now, so it runs after the page's own document listeners (like swap-no-flash.spec.ts):
  // the capture listener on window notes the run count before any of them, this one after all of them.
  await page.evaluate(() => {
    const w = window as unknown as { __sync: Sync };
    let before = 0;
    window.addEventListener(
      "htmx:afterSwap",
      () => (before = w.__sync.runs),
      true,
    );
    document.addEventListener("htmx:afterSwap", (e) => {
      const t = e.target as Element;
      const ran = w.__sync.runs - before;
      if (t.matches?.("table [sse-swap], li.dcard [sse-swap]"))
        w.__sync.cell.push(ran);
      else if (t.id === "devtable") w.__sync.list.push(ran);
    });
    // The reload makes new cells, and the first fragment of a new cell is always swapped.
    // @ts-expect-error htmx is a page global
    window.htmx.trigger("#devtable", "sse:resync");
  });
  const read = () =>
    page.evaluate(() => (window as unknown as { __sync: Sync }).__sync);
  await expect
    .poll(async () => (await read()).list.length, { timeout: 15_000 })
    .toBeGreaterThan(0);
  await expect
    .poll(async () => (await read()).cell.length, {
      timeout: 60_000,
      intervals: [500],
    })
    .toBeGreaterThanOrEqual(5);
  const got = await read();
  expect(
    got.cell.filter((n) => n > 0),
    "syncAll runs during the afterSwap of live cells",
  ).toEqual([]);
  expect(
    got.list.every((n) => n > 0),
    "the list reload still syncs at afterSwap (#594)",
  ).toBe(true);
});

// #806: a device event reloads the whole list (#devtable, 0.5 s after a burst). With a saved layout wider
// than the frame the person scrolls the table sideways to reach the columns on the right, and every reload
// threw the scroll back to 0 (a new scroll box) and drew the pinned columns at other widths for a frame
// (the fit of the pinned columns measured the new table before it had its minimum width, so a second fit
// at settle put them back): the owner saw a table that "refreshes constantly". A reload brings new values,
// never a new layout: what the person looks at stays where it is.
// the owner's layout from the issue, with Last restart, OTA URL and Group topic shown, saved as the layout's
// own pixel widths (b, tenths of a pixel in w): about 1700 px of columns, so at 1300 px the table scrolls
// inside its frame
const wideLayout = {
  b: true,
  w: { actions: 5.7, address: 13.3, family: 9.8, firmware: 9.3, name: 22.8, power: 7.5, reach: 14.2, status: 10.5, uptime: 11.5, wifi: 11.9, restart: 18, otaurl: 24, grouptopic: 14 },
  h: [],
  v: ["otaurl", "grouptopic"],
  o: ["status", "wifi", "reach", "firmware", "family", "address", "uptime", "restart", "otaurl", "grouptopic", "power"],
};

type Frame = { table: number; widths: string; pins: string; left: number; y: number; edges: string };

// The saved layout is the shared fixture's (support/saved-layout.ts), at two windows: 1300 px with the layout above
// (current format "b"), and the owner's 1290 px with the document the fixture saves (#821): the table scrolls
// inside its frame in both, and every case below must hold in both.
const wideCases = [
  { width: 1300, doc: wideLayout },
  { width: ownerWindow, doc: ownerWide },
];
type WideCase = (typeof wideCases)[number];
const openWideList = (page: Page, c: WideCase) => openSavedLayoutList(page, app.url, c);

// Records every frame from now until 300 ms after the next settle of #devtable (after `act`): which table
// is on the page, the widths of all its header cells and of the pinned ones, the frame's sideways scroll and
// the page's vertical one, and which edges of the frame cast a shade. Read in requestAnimationFrame: the
// state each frame is painted with.
async function framesAcrossReload(page: Page, act: () => Promise<void>): Promise<Frame[]> {
  await page.evaluate(() => {
    const w = window as unknown as { __frames: Frame[]; __done: boolean; __ids: WeakMap<Element, number> };
    w.__frames = [];
    w.__done = false;
    w.__ids = new WeakMap();
    let n = 0;
    const sample = () => {
      const t = document.querySelector<HTMLTableElement>("#devtable table[data-table=devices]");
      if (t) {
        if (!w.__ids.has(t)) w.__ids.set(t, ++n);
        const pinned = new Set<number>();
        for (const c of t.classList) {
          const m = /^p[lr](\d+)$/.exec(c);
          if (m) pinned.add(parseInt(m[1], 10) - 1);
        }
        const ths = [...t.tHead!.rows[0].cells];
        const px = (c: Element) => c.getBoundingClientRect().width.toFixed(1);
        w.__frames.push({
          table: w.__ids.get(t)!,
          widths: ths.map(px).join(","),
          pins: ths.filter((_, i) => pinned.has(i)).map(px).join(","),
          left: t.parentElement!.scrollLeft,
          y: window.scrollY,
          // the shade of the pinned groups' inner edges (#790), which follows the scroll position
          edges: ["more-l", "more-r"].filter((c) => t.parentElement!.classList.contains(c)).join(" "),
        });
      }
      if (!w.__done) requestAnimationFrame(sample);
    };
    requestAnimationFrame(sample);
    // Only the settle of a swap that came after the first recorded frame ends the recording: a reload swapped
    // in just before the recording started settles 20 ms later, and ending on that settle stopped the
    // recording before the asked-for reload (#819).
    let request: unknown = null;
    document.addEventListener("htmx:afterSwap", function swapped(e) {
      if ((e.target as Element).id !== "devtable" || w.__frames.length === 0) return;
      document.removeEventListener("htmx:afterSwap", swapped);
      request = (e as CustomEvent<{ xhr: XMLHttpRequest }>).detail.xhr;
    });
    document.addEventListener("htmx:afterSettle", function settled(e) {
      if ((e.target as Element).id !== "devtable") return;
      if (request === null || (e as CustomEvent<{ xhr: XMLHttpRequest }>).detail.xhr !== request) return;
      document.removeEventListener("htmx:afterSettle", settled);
      setTimeout(() => (w.__done = true), 300);
    });
  });
  // The first frame is recorded before the reload is asked for: the reload's answer can be swapped in before
  // the next frame, and then no frame showed the old table ("the reload replaced the table" failed, #790).
  await page.waitForFunction(() => (window as unknown as { __frames: Frame[] }).__frames.length > 0);
  await act();
  await page.waitForFunction(() => (window as unknown as { __done: boolean }).__done, null, { timeout: 15_000 });
  return page.evaluate(() => (window as unknown as { __frames: Frame[] }).__frames);
}

const reload = (page: Page) =>
  page.evaluate(() => {
    // @ts-expect-error htmx is a page global
    window.htmx.trigger("#devtable", "sse:resync");
  });

for (const c of wideCases) {
  test(`a list reload keeps the sideways scroll and draws the pinned columns at their widths in every frame (#806, ${c.width} px)`, async ({ page }) => {
    await openWideList(page, c);
    // a page that scrolls down too: the list stays where the person is
    await page.evaluate(() => {
      document.querySelector<HTMLElement>("#devtable .tablescroll")!.scrollLeft = 200;
      window.scrollTo(0, 120);
    });
    const y = await page.evaluate(() => window.scrollY);
    expect(y, "the page scrolls down at this height").toBeGreaterThan(0);
    const frames = await framesAcrossReload(page, () => reload(page));
    const tables = new Set(frames.map((f) => f.table));
    expect(tables.size, "the reload replaced the table").toBeGreaterThan(1);
    const swapped = frames.filter((f) => f.table !== frames[0].table);
    expect(
      [...new Set(swapped.map((f) => f.left))],
      "the sideways scroll after the reload (every frame)",
    ).toEqual([200]);
    expect([...new Set(swapped.map((f) => f.y))], "the page's vertical scroll after the reload").toEqual([y]);
    expect([...new Set(frames.map((f) => f.edges))], "the shaded edges, the same in every frame").toHaveLength(1);
    expect(
      [...new Set(frames.map((f) => f.widths))],
      "distinct header widths from before the reload until after it settled",
    ).toHaveLength(1);
  });

  test(`the fit of the pinned columns gives the same widths when it runs again (#806, ${c.width} px)`, async ({ page }) => {
    await openWideList(page, c);
    // A swapped-in table has no minimum width yet; the fit sets it. Run twice from that state, the second fit
    // must find nothing to change: one painted frame per fit with other widths was the flicker.
    const runs = await page.evaluate(() => {
      const t = document.querySelector<HTMLTableElement>("#devtable table[data-table=devices]")!;
      const cols = () => [...t.querySelectorAll(":scope > colgroup > col")].map((c) => c.getAttribute("width")).join(",");
      const fit = () => document.body.dispatchEvent(new CustomEvent("htmx:afterSettle", { bubbles: true }));
      t.style.minWidth = "";
      fit();
      const once = { cols: cols(), min: t.style.minWidth };
      fit();
      return { once, twice: { cols: cols(), min: t.style.minWidth } };
    });
    expect(runs.twice).toEqual(runs.once);
  });

  test(`a list reload during a column drag keeps the sideways scroll and the pinned widths (#806, ${c.width} px)`, async ({ page }) => {
    await openWideList(page, c);
    await page.evaluate(() => {
      document.querySelector<HTMLElement>("#devtable .tablescroll")!.scrollLeft = 200;
    });
    // a handle in plain view: between the pinned groups
    const box = await page.evaluate(() => {
      const t = document.querySelector<HTMLTableElement>("#devtable table[data-table=devices]")!;
      const on = (side: string, th: HTMLTableCellElement) => t.classList.contains(`p${side}${th.cellIndex + 1}`);
      const ths = [...t.tHead!.rows[0].cells];
      const left = Math.max(...ths.filter((th) => on("l", th)).map((th) => th.getBoundingClientRect().right));
      const right = Math.min(...ths.filter((th) => on("r", th)).map((th) => th.getBoundingClientRect().left));
      const h = ths
        .filter((th) => !on("l", th) && !on("r", th))
        .map((th) => th.querySelector(".colhandle"))
        .map((el) => el && el.getBoundingClientRect())
        .find((r) => r && r.width > 0 && r.left > left + 20 && r.right < right - 60)!;
      return { x: h.x + h.width / 2, y: h.y + h.height / 2 };
    });
    await page.mouse.move(box.x, box.y);
    await page.mouse.down();
    await page.mouse.move(box.x + 30, box.y, { steps: 5 });
    await expect(page.locator("body")).toHaveClass(/is-resizing/);
    const left = await page.evaluate(() => document.querySelector<HTMLElement>("#devtable .tablescroll")!.scrollLeft);
    const frames = await framesAcrossReload(page, () => reload(page));
    await page.mouse.move(box.x + 40, box.y, { steps: 2 });
    await page.mouse.up();
    expect(new Set(frames.map((f) => f.table)).size, "the reload replaced the table").toBeGreaterThan(1);
    const swapped = frames.filter((f) => f.table !== frames[0].table);
    expect([...new Set(swapped.map((f) => f.left))], "the sideways scroll after the reload").toEqual([left]);
    expect([...new Set(frames.map((f) => f.edges))], "the shaded edges, the same in every frame").toHaveLength(1);
    expect([...new Set(frames.map((f) => f.pins))], "distinct widths of the pinned columns").toHaveLength(1);
    // the new table is drawn at the widths being dragged, not at the saved ones until the pointer moves again
    expect([...new Set(frames.map((f) => f.widths))], "distinct header widths, the dragged column's included").toHaveLength(1);
  });
}
