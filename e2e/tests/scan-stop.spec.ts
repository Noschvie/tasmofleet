import { test, expect } from "../support/fixtures";
import { expectLayout, expectNoOverflow } from "../support/layout";
import { App, startApp } from "../support/stack";
import { mkdirSync } from "node:fs";

// #144: "Stop scan" was ignored, the bulk button flickered and ticked rows were cleared, because the
// event stream replaced the whole tab (Stop button included) on every progress tick. A real scan of
// the stack finishes within a second (loopback, at most 1024 addresses), too fast to click in, so
// the stream is faked here: the page markup follows scanBody while a scan runs, the events are the
// ones the stream sends ("scan-progress" every tick, "scan" when a device was found, "scan-done" at
// the end). What this guards is the browser side: nested sse-swap targets, the scan bar and the
// "Add selected" button staying the same elements across a progress tick, and the ticked rows
// surviving every swap, the last one included.
let app: App;
test.beforeAll(async () => {
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
    },
  });
});
test.afterAll(async () => {
  await app?.stop();
});
test.use({
  extraHTTPHeaders: {
    "X-authentik-username": "adam",
    "X-authentik-groups": "adm",
  },
});

const progress = (done: number) =>
  `<div class="add-progress" role="status"><div class="add-progress-head"><span class="add-progress-t"><span class="add-note-dot pulse" aria-hidden="true"></span><b>Scanning 192.168.20.0/24</b></span> <span class="mono add-t2"><span id="done">${done}</span> of 254 addresses</span></div><progress max="254" value="${done}"></progress><div class="add-progress-found">Found 1 Tasmota devices so far</div></div>`;
const row = (name: string, id: number) =>
  `<tr><td class="sel"><input type="checkbox" name="id" value="${id}:v${id}" aria-label="Select ${name}"/></td><td class="name">${name}</td><td class="act"><button type="submit" class="btn btn-ghost add-ign">Ignore</button> <a class="btn btn-sm" href="#">Add</a></td></tr>`;
const results = (rows: string) =>
  `<form method="post" action="/discovery/proposals/accept"><div class="add-chips"><a class="add-chip is-on" href="#">All<span class="add-chip-n">1</span></a><span class="add-gap"></span><div class="btn-row" data-bulkbar><button type="submit" class="btn btn-primary">Add <span data-bar-count></span> selected</button></div></div>
<div class="add-table add-scan-table"><table><thead><tr><th class="sel"><input type="checkbox" data-select-all hidden aria-label="Select all"/></th><th>Name</th><th class="act"></th></tr></thead><tbody>${rows}</tbody></table></div></form>`;
const scanbar = (stopped: boolean) =>
  `<form class="add-scanbar" method="post" action="/discovery/scan"><input class="mono add-subnet" name="cidrs" value="192.168.20.0/24" aria-label="Subnet"/>${stopped ? '<button type="submit" class="btn btn-primary" id="scan-now">Scan now</button>' : '<button type="submit" class="btn" id="stop">Stop scan</button>'}</form>`;

test("a running scan: progress ticks leave the scan bar and the table alone, ticked rows survive every swap, the end included", async ({
  page,
}) => {
  // each stream connection answers with the next event the test sends, then closes (the browser reconnects at once)
  const waiting: ((m: { event: string; html: string }) => void)[] = [];
  await page.route("**/fake-stream", (route) => {
    return new Promise<void>((done) => {
      waiting.push(async (m) => {
        await route.fulfill({
          status: 200,
          headers: {
            "content-type": "text/event-stream",
            "cache-control": "no-cache",
          },
          body: `retry: 100\nevent: ${m.event}\ndata: ${m.html.replace(/\n/g, "\ndata: ")}\n\n`,
        });
        done();
      });
    });
  });
  const send = async (event: string, html: string) => {
    await expect
      .poll(() => waiting.length, { timeout: 10_000 })
      .toBeGreaterThan(0);
    waiting.shift()!({ event, html });
  };

  await page.goto(app.url + "/discovery/scan");
  await page.evaluate(
    (parts) => {
      document.querySelector("#scan")!.outerHTML =
        `<div id="scan" class="add-scan" hx-ext="sse" sse-connect="/fake-stream" sse-swap="scan-done" hx-swap="outerHTML">${parts.bar}
      <div id="scan-progress" sse-swap="scan-progress" hx-swap="innerHTML">${parts.progress}</div>
      <div id="scan-live" sse-swap="scan" hx-swap="innerHTML">${parts.live}</div></div>`;
      (
        window as unknown as { htmx: { process(e: Element): void } }
      ).htmx.process(document.querySelector("#scan")!);
    },
    {
      bar: scanbar(false),
      progress: progress(1),
      live: results(row("lamp", 1)),
    },
  );
  const stop = await page.locator("#stop").elementHandle();
  const addSel = await page
    .getByRole("button", { name: /selected/ })
    .elementHandle();
  await page.getByLabel("Select lamp").check();
  await expect(page.locator("[data-bulkbar]")).toBeVisible();

  // a progress tick: only the progress changes; scan bar, bulk bar and table are the same elements
  await send("scan-progress", progress(2));
  await expect(page.locator("#done")).toHaveText("2");
  await send("scan-progress", progress(3));
  await expect(page.locator("#done")).toHaveText("3");
  expect(
    await stop!.evaluate((e) => e.isConnected),
    "a progress tick left Stop scan alone",
  ).toBe(true);
  expect(
    await addSel!.evaluate((e) => e.isConnected),
    "a progress tick left Add selected alone",
  ).toBe(true);
  await expect(page.getByLabel("Select lamp")).toBeChecked();

  // a device was found: the results are replaced, the ticked row stays ticked
  await send("scan", results(row("lamp", 1) + row("plug", 2)));
  await expect(page.getByLabel("Select plug")).toBeVisible();
  expect(
    await stop!.evaluate((e) => e.isConnected),
    "new results left Stop scan alone",
  ).toBe(true);
  await expect(page.getByLabel("Select lamp")).toBeChecked();
  await expect(page.locator("[data-bar-count]")).toHaveText("1");

  // the end replaces the whole tab: the ticked row stays ticked there too
  await send(
    "scan-done",
    `<div id="scan" class="add-scan">${scanbar(true)}<div id="scan-progress"><div class="add-note tone-ok" role="status"><span class="add-note-text"><b>Scan finished.</b></span></div></div><div id="scan-live">${results(row("lamp", 1) + row("plug", 2))}</div></div>`,
  );
  await expect(page.locator("#scan-now")).toBeVisible();
  await expect(page.getByLabel("Select lamp")).toBeChecked();
  await expect(page.getByLabel("Select plug")).not.toBeChecked();
  await expect(page.locator("[data-bar-count]")).toHaveText("1");
});

// The page while a scan runs, with the layout guards; with SHOT_DIR set also as screenshots. The
// markup is the running scanBody (see above) with the table columns of the real page.
for (const width of [1280, 375]) {
  for (const scheme of ["light", "dark"] as const) {
    test(`a running scan at ${width}px ${scheme}: layout guards`, async ({
      page,
    }) => {
      await page.setViewportSize({ width, height: 900 });
      await page.emulateMedia({ colorScheme: scheme });
      await page.goto(app.url + "/discovery/scan");
      const rows = [
        ["lamp-01", "192.168.20.31", "AA:BB:CC:DD:EE:01", "15.0.1", "New"],
        ["plug-kitchen", "192.168.20.32", "AA:BB:CC:DD:EE:02", "14.4.1", "New"],
        [
          "sonoff-basic",
          "192.168.20.40",
          "AA:BB:CC:DD:EE:03",
          "13.1.0",
          "Known",
        ],
      ];
      const tr = rows
        .map(
          ([n, ip, mac, fw, st]) =>
            `<tr><td class="sel">${st === "New" ? `<input type="checkbox" name="id" value="${n}:v" aria-label="Select ${n}"/>` : ""}</td><td class="name" title="${n}"><span class="add-label">${n}</span></td><td class="mono">${ip}</td><td class="mono add-t2">${mac}</td><td class="mono">${fw}</td><td><span class="bdg ${st === "New" ? "t-accent" : ""}">${st}</span></td><td class="act">${st === "New" ? '<button type="submit" class="btn btn-ghost add-ign">Ignore</button> <a class="btn btn-sm" href="#">Add</a>' : '<a class="btn btn-link" href="#">Open</a>'}</td></tr>`,
        )
        .join("");
      const cards = rows
        .map(
          ([n, ip, mac, , st]) =>
            `<div class="add-card add-scan-card"><div class="add-card-body"><span class="add-card-title"><span class="add-card-name">${n}</span><span class="bdg ${st === "New" ? "t-accent" : ""}">${st}</span></span><span class="add-card-meta">${ip}</span><span class="add-card-src mono">${mac}</span></div></div>`,
        )
        .join("");
      await page.evaluate(
        (p) => {
          document.querySelector("#scan")!.outerHTML =
            `<div id="scan" class="add-scan">${p.bar}<div id="scan-progress">${p.progress}</div><div id="scan-live"><form method="post"><div class="add-chips"><a class="add-chip is-on" href="#">All<span class="add-chip-n">3</span></a><a class="add-chip" href="#">New<span class="add-chip-n">2</span></a><a class="add-chip" href="#">Known<span class="add-chip-n">1</span></a><a class="add-chip" href="#">Ignored<span class="add-chip-n">0</span></a><span class="add-gap"></span><div class="btn-row" data-bulkbar><button type="submit" class="btn btn-primary">Add <span data-bar-count></span> selected</button></div></div>
<div class="add-table add-scan-table"><table><colgroup><col class="c-sel"/><col class="c-name"/><col class="c-ip"/><col class="c-mac"/><col class="c-fw"/><col class="c-st"/><col class="c-act"/></colgroup><thead><tr><th class="sel"><input type="checkbox" data-select-all hidden aria-label="Select all"/></th><th>Name</th><th>IP</th><th>MAC</th><th>Firmware</th><th>Status</th><th class="act"><span class="sr-only">Actions</span></th></tr></thead><tbody>${p.tr}</tbody></table></div><div class="add-cards">${p.cards}</div></form></div></div>`;
        },
        { bar: scanbar(false), progress: progress(120), tr, cards },
      );
      if (width > 600) await page.getByLabel("Select lamp-01").check(); // the phone shows cards, which have no tick box
      if (process.env.SHOT_DIR) {
        mkdirSync(process.env.SHOT_DIR, { recursive: true });
        await page.screenshot({
          path: `${process.env.SHOT_DIR}/scan-running-${width}-${scheme}.png`,
          fullPage: true,
        });
      }
      await expectNoOverflow(page, "scan running");
      await expectLayout(page, `scan running ${width} ${scheme}`);
    });
  }
}
