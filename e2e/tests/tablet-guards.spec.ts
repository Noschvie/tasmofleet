import { test, expect } from "../support/fixtures";
import { insertProposals } from "../support/discovery-seed";
import { cardStackGaps, expectLayout, expectTableGuards } from "../support/layout";
import { pageList, settled } from "../support/page-list";
import { openSavedLayoutList, ownerWindow } from "../support/saved-layout";
import { realisticDevenvArgs, realisticPolicyRules, seedRealisticData } from "../support/seed";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";
import { checkTablePage, tablePageCases } from "../support/table-pages";

// #592: three #419 conversions (#588, #591) broke between 720 and 1199 px, where the card stacks
// become two half-width columns, and no test saw it: the card-width guard measures 1280/1920 and
// card-stacks covers only some pages. This runs the layout guards (overlap, a word broken across
// lines, cut-off cells, text over text) and the card-stack gap check on every page the card-width
// guard lists, at the two tablet widths, light theme (FULL tier).
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
let app: App;
let env: Devenv;
let lamp = "";
let txn = "";

test.beforeAll(async ({ browser }) => {
  test.setTimeout(300_000);
  env = await startDevenv(realisticDevenvArgs);
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_GITHUB_URL,
      TASMOFLEET_POLICY_RULES: realisticPolicyRules,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
  // proposals for the New devices list of Devices -> Add (a table only a discovery fills)
  await insertProposals(app, [
    { mac: "aa:bb:cc:00:01:01", name: "Workshop lamp", topic: "workshop_lamp", host: "workshop-lamp-1a2b", ip: "127.0.0.11", seen: 600 },
    { mac: "aa:bb:cc:00:01:02", name: "", topic: "attic_fan", host: "tasmota-attic-3c4d", ip: "127.0.0.9", seen: 300 },
    { mac: "aa:bb:cc:00:01:03", name: "Balcony plug", topic: "balcony_plug", host: "balcony-plug-5e6f", ip: "127.0.0.100", seen: 60 },
  ]);
  const p = await browser.newPage({ extraHTTPHeaders: adam });
  try {
    await seedRealisticData(p, app, env);
    await p.goto(app.url + "/devices");
    lamp = (await p.getByRole("link", { name: "lamp-1m", exact: true }).first().getAttribute("href"))!.replace(app.url, "");
    await p.goto(app.url + lamp);
    await p.getByRole("button", { name: "Back up", exact: true }).click();
    await expect(p.getByText(/backed up|Backup (taken|saved)/i).first()).toBeVisible({ timeout: 30_000 });
    // the policy judges devices from live reads. Since reads go over MQTT first (#456), none had arrived
    // when the deviations page was checked (empty state), and the background reads are spread over ten
    // minutes: run the live check once, as a person would, and check the page with its verdict
    await p.goto(app.url + "/policy/deviations");
    await p.getByRole("button", { name: "Run now" }).click();
    await expect(p.locator("#deviations [data-dev-group]").first()).toBeVisible({ timeout: 60_000 });
    await p.goto(app.url + "/transactions");
    txn = (await p.locator("a[href*='/transactions/']").first().getAttribute("href"))!.replace(app.url, "");
  } finally {
    await p.close();
  }
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const pages = pageList({ lamp: () => lamp, txn: () => txn, fwVersion: () => env.vars.DEVENV_FW_VERSION });

for (const width of [768, 1024]) {
  for (const [name, path] of pages) {
    test(`${name}: layout guards and card stacks at ${width}px`, async ({ page }) => {
      await page.setExtraHTTPHeaders(adam);
      await page.setViewportSize({ width, height: 900 });
      await page.emulateMedia({ colorScheme: "light" });
      const resp = await page.goto(app.url + path());
      expect(resp?.status(), `GET ${path()}`).toBeLessThan(400);
      await settled(page);
      await page.waitForTimeout(800);
      await expectLayout(page, `${name} ${width}px`);
      // the policy pages keep their own card gap (--card-gap, 20 to 24 px), see card-stacks.spec.ts
      expect(await cardStackGaps(page, name.startsWith("policy") ? 25 : 17), `${name}: card stacks`).toEqual([]);
    });
  }
}

// #821: every page that shows a table has the table layout guards at the two widths people use. The pages
// come from support/table-pages.ts, which reads the templates: a table that no page shows fails
// table-pages.spec.ts. Besides the generic guards (overlap, text over text, cut-off cells with no title)
// each page runs guard s (a cell is not cut off while another column has room to spare, the Firmware cell
// of #811) and guard r (no element of one row over the text of another, as loaded and with the pointer
// on a row and on its titled elements, the bubble of #811). The two policy template pages need the
// template-shapes fleet and run in policy-templates.spec.ts.
for (const { page: tp, width } of tablePageCases({ lamp: () => lamp }, "fleet")) {
  test(`table page ${tp.name}: table guards at ${width}px`, async ({ page }) => {
    test.setTimeout(600_000); // the pointer pass waits for a bubble per hover
    await checkTablePage(page, app.url, tp, width);
  });
}

// #821: the same guards on the Devices list with a saved layout wider than the frame at the owner's window (1290 px),
// the case no earlier layout guard ran: the table scrolls, Power, Status and Actions are pinned, the optional columns
// are shown.
test(`devices list with a saved layout at ${ownerWindow}px: layout guards and table guards`, async ({ page }) => {
  test.setTimeout(600_000); // the pointer pass waits for a bubble per hover
  await page.emulateMedia({ colorScheme: "light" });
  await openSavedLayoutList(page, app.url, { width: ownerWindow });
  await page.waitForTimeout(800);
  await expectLayout(page, `devices list saved layout ${ownerWindow}px`);
  // Last restart is cut by 2-3 px beside spare room in other columns (#912, tolerated through knownLayoutIssues).
  await expectTableGuards(page, `devices list saved layout ${ownerWindow}px`);
});

// #727: a live reload of /devices/table (the page does one right after it connects and again after
// device events) replaces the whole table. The swapped-in table has no <colgroup> fit yet; tables.js
// used to fit it only when htmx settled (about 20 ms later), and until then the Power column
// overlapped the Actions summary in every row at 1024 px. A person sees that frame, and the nightly
// layout guard caught it by chance (a run that measured inside the window). So the table must be fitted
// in the very task that swaps it in: nothing is painted between the swap and the end of that task.
// The probe reads the layout in a microtask of the swap's own task, with no fixed wait.
const powerOverActions = () => {
  const offenders: string[] = [];
  const rows = [...document.querySelectorAll('table[data-table="devices"] tbody tr')].filter((r) => (r as HTMLElement).offsetParent !== null);
  for (const tr of rows) {
    const power = tr.querySelector('td[data-label="Power"]');
    const actions = tr.querySelector('td[data-label="Actions"]');
    if (!power || !actions) continue;
    // the controls that must stay clear of each other: the Power buttons and the Actions summary
    const a = [...power.querySelectorAll("button")].map((e) => e.getBoundingClientRect());
    const b = [...actions.querySelectorAll("summary")].map((e) => e.getBoundingClientRect());
    for (const x of a)
      for (const y of b) {
        const w = Math.min(x.right, y.right) - Math.max(x.left, y.left);
        const h = Math.min(x.bottom, y.bottom) - Math.max(x.top, y.top);
        if (w > 1 && h > 1) offenders.push(`${tr.querySelector("td a")?.textContent?.trim() ?? "row"}: ${Math.round(w)}x${Math.round(h)}`);
      }
  }
  return { rows: rows.length, offenders: [...new Set(offenders)] };
};

// The 1290 px case (#821) is the owner's window with a saved layout wider than the frame: the table scrolls, the
// pinned columns keep their places, and the swapped-in table must be fitted just as early.
for (const c of [{ width: 1024, saved: false }, { width: ownerWindow, saved: true }]) {
test(`devices list at ${c.width}px${c.saved ? " with a saved layout" : ""}: the table is fitted in the task that swaps it in after a live reload`, async ({ page }) => {
  await page.emulateMedia({ colorScheme: "light" });
  if (c.saved) {
    await openSavedLayoutList(page, app.url, { width: c.width });
  } else {
    await page.setExtraHTTPHeaders(adam);
    await page.setViewportSize({ width: c.width, height: 900 });
    await page.goto(app.url + "/devices");
    await settled(page);
  }
  await expect(page.locator('table[data-table="devices"] tbody tr').first()).toBeVisible();
  // the probe itself: on a list that has settled nothing overlaps (else it would flag the fit, not the window)
  await expect.poll(async () => (await page.evaluate(powerOverActions)).offenders, { timeout: 15_000 }).toEqual([]);

  // install the probe once; the swap handler below calls it
  await page.evaluate(`window.__powerOverActions = ${powerOverActions.toString()}`);
  const swapped = await page.evaluate(
    () =>
      new Promise<{ rows: number; offenders: string[] }>((resolve) => {
        // live cells swap all the time too: only the swap of the whole list counts
        const onSwap = (e: Event) => {
          if ((e.target as Element).id !== "devtable") return;
          document.removeEventListener("htmx:afterSwap", onSwap);
          // registered after tables.js's own listener, so this sees what that listener left behind
          queueMicrotask(() => resolve((window as any).__powerOverActions()));
        };
        document.addEventListener("htmx:afterSwap", onSwap);
        const t = document.getElementById("devtable")!;
        (window as any).htmx.ajax("GET", t.getAttribute("hx-get")!, { target: t, swap: "innerHTML" });
      }),
  );
  expect(swapped.rows, "the reloaded list has rows").toBeGreaterThan(10);
  expect(swapped.offenders, "Power buttons overlapping the Actions summary right after the swap").toEqual([]);
});
}
