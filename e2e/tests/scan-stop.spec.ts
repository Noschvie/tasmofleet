import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// #144: "Stop scan" was ignored, the bulk button flickered and ticked rows were cleared, because the
// event stream replaced the whole tab (Stop button included) on every progress tick. A real scan of
// the stack finishes within a second (loopback, at most 1024 addresses), too fast to click in, so
// the stream is faked here: the page markup is the one scanBody renders while a scan runs, the events
// are the ones the stream sends. What this guards is the browser side: nested sse-swap targets, the
// scan bar staying the same element, and the ticked rows surviving each swap.
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

const live = (
  done: number,
) => `<div class="add-progress" role="status"><span id="done">${done}</span></div>
<form method="post" action="/discovery/proposals/accept"><div class="add-chips"><div class="btn-row" data-bulkbar><button type="submit" class="btn btn-primary">Add <span data-bar-count></span> selected</button></div></div>
<div class="add-table"><table><thead><tr><th class="sel"><input type="checkbox" data-select-all hidden aria-label="Select all"/></th><th>Name</th></tr></thead><tbody><tr><td class="sel"><input type="checkbox" name="id" value="1:v1" aria-label="Select lamp"/></td><td>lamp</td></tr></tbody></table></div></form>`;

test("while a scan streams, the Stop button stays the same element and ticked rows stay ticked", async ({
  page,
}) => {
  let n = 0;
  await page.route("**/fake-stream", (route) => {
    n++;
    // one event per connection (the browser reconnects at once); the first answer is the initial state
    return route.fulfill({
      status: 200,
      headers: {
        "content-type": "text/event-stream",
        "cache-control": "no-cache",
      },
      body: `retry: 100\nevent: scan\ndata: ${live(n).replace(/\n/g, "\ndata: ")}\n\n`,
    });
  });
  await page.goto(app.url + "/discovery/scan");
  await page.evaluate((html) => {
    document.querySelector("#scan")!.outerHTML =
      `<div id="scan" class="add-scan" hx-ext="sse" sse-connect="/fake-stream" sse-swap="scan-done" hx-swap="outerHTML">
      <form class="add-scanbar" method="post"><button type="submit" class="btn" id="stop">Stop scan</button></form>
      <div id="scan-live" sse-swap="scan" hx-swap="innerHTML">${html}</div></div>`;
    (window as unknown as { htmx: { process(e: Element): void } }).htmx.process(
      document.querySelector("#scan")!,
    );
  }, live(0));
  const stop = await page.locator("#stop").elementHandle();
  await page.getByLabel("Select lamp").check();
  await expect(page.locator("[data-bulkbar]")).toBeVisible();
  const before = n;
  await expect.poll(() => n, { timeout: 10_000 }).toBeGreaterThan(before + 2); // several swaps happened
  await expect(page.locator("#done")).not.toHaveText("0");
  expect(
    await stop!.evaluate((e) => e.isConnected),
    "the swaps left the Stop button alone",
  ).toBe(true);
  await expect(page.getByLabel("Select lamp")).toBeChecked();
  await expect(page.locator("[data-bulkbar]")).toBeVisible();
  await expect(page.locator("[data-bar-count]")).toHaveText("1");
});
