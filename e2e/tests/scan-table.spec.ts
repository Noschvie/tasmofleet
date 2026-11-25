import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { layoutGuards, guardReport, hoverTip } from "../support/layout";
import { openScan as openScanOf } from "../support/scan-fixture";
import { App, startApp } from "../support/stack";

// #811: the result table of Devices -> Add -> Network scan cut Firmware off while the table had room
// to spare, showed a name bubble over the next row's name, kept an empty selection column when no row
// could be ticked, and hid known devices behind a line that was no control. A scan only finds devices
// on port 80, which no e2e stack can serve, so the page is loaded as the real binary serves it
// (shell, CSS, scripts) and the finished scan's markup is put into it (support/scan-fixture.ts).
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

let app: App;
test.beforeAll(async () => {
  app = await startApp({
    env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32", TASMOFLEET_ADMIN_GROUPS: "adm" },
  });
});
test.afterAll(async () => {
  await app?.stop();
});

const openScan = (page: Page, fixture: "known" | "mixed", scheme: "light" | "dark" = "light", width = 1280) => openScanOf(page, app.url, adam, fixture, scheme, width);

/** cutOffCells lists the cells of the table whose content is wider than the cell. */
async function cutOffCells(page: Page): Promise<string[]> {
  return page.evaluate(() =>
    [...document.querySelectorAll<HTMLElement>(".add-scan-table td, .add-scan-table th")]
      .filter((c) => c.scrollWidth > c.clientWidth + 1 || [...c.querySelectorAll<HTMLElement>("*")].some((x) => !x.closest(".sr-only") && x.scrollWidth > x.clientWidth + 1 && getComputedStyle(x).overflowX !== "visible"))
      .map((c) => `${c.tagName.toLowerCase()} "${(c.textContent ?? "").trim().slice(0, 30)}" content ${c.scrollWidth}px in ${c.clientWidth}px`),
  );
}

/** hoverAndMeasure hovers the cell of a row and returns the bubble (null when none shows) and what it covers of other rows' text. */
async function hoverAndMeasure(page: Page, row: number, cell: string) {
  const target = page.locator(".add-scan-table tbody tr").nth(row).locator(cell);
  return hoverTip(page, target, ".add-scan-table tbody tr", { x: 20, y: 18 });
}

for (const fixture of ["known", "mixed"] as const) {
  // Why: Name and Firmware get what they need before empty space is left over, so a table with room
  // to spare cuts nothing off ("16.0.0(relea…" beside a wide empty gap was the defect).
  test(`${fixture}: no cell text is cut off while the table has spare width`, async ({ page }) => {
    await openScan(page, fixture);
    expect(await cutOffCells(page)).toEqual([]);
    const frame = await page.locator(".add-scan-table").evaluate((f) => ({ scroll: f.scrollWidth, client: f.clientWidth }));
    expect(frame.scroll, "the table fits its frame at 1280 px").toBeLessThanOrEqual(frame.client);
    // the Firmware column shows the number only, as the Devices list does
    const fw = await page.evaluate(() => {
      const col = [...document.querySelectorAll(".add-scan-table thead th")].findIndex((th) => th.textContent?.trim() === "Firmware");
      return [...document.querySelectorAll(".add-scan-table tbody tr")].map((tr) => tr.children[col].textContent?.trim() ?? "");
    });
    for (const t of fw) expect(t, "firmware shows the number without its build suffix").toMatch(/^(\d+(\.\d+){2,3}|—)$/);
  });

  // Why: with the pointer on a row, a bubble lay over the name of the row below. A name that fits its
  // cell needs no bubble, and what a bubble shows never covers another row's text.
  test(`${fixture}: the pointer on a row covers no other row's text and shows no bubble for a name that fits`, async ({ page }) => {
    await openScan(page, fixture);
    const rows = await page.locator(".add-scan-table tbody tr").count();
    for (let i = 0; i < Math.min(rows, 5); i++) {
      for (const cell of ["td.name", "td.mono.add-t2", "td[title^=Tasmota]"]) {
        if (!(await page.locator(".add-scan-table tbody tr").nth(i).locator(cell).count())) continue;
        const m = await hoverAndMeasure(page, i, cell);
        expect(m.covers, `bubble ${JSON.stringify(m.tip)} on row ${i} ${cell}`).toEqual([]);
        if (cell === "td.name") expect(m.tip, `row ${i}: the name fits its cell`).toBeNull();
      }
    }
    await page.mouse.move(0, 0);
    expect(await layoutGuards(page, ["a", "b", "o", "w", "i"]).then((r) => guardReport(`scan ${fixture}`, r))).toBe("");
  });
}

// Why: a name that really is cut off keeps its bubble (the full name is the only way to read it), and
// even the longest one (160 characters, three lines) stays beside its own row.
test("a cut-off name shows its full text beside its row, over no other row", async ({ page }) => {
  await openScan(page, "known");
  const long = "Workshop ceiling spotlight controller south wing with the extension cable to the garden shed and the very long name of a test device number two";
  await page.locator(".add-scan-table tbody tr").nth(2).locator("td.name").evaluate((td, name) => {
    td.querySelector(".add-label")!.textContent = name;
    td.setAttribute("title", name);
  }, long);
  expect(await cutOffCells(page)).toHaveLength(1);
  const m = await hoverAndMeasure(page, 2, "td.name");
  expect(m.tip?.text, "the cut-off name is shown in full").toBe(long);
  expect(m.covers, "the bubble lies over other rows' text").toEqual([]);
  const own = await page.locator(".add-scan-table tbody tr").nth(2).boundingBox();
  expect(m.tip!.top, "the bubble starts beside its own row").toBeGreaterThanOrEqual(own!.y - 20);
  expect(m.tip!.bottom, "the bubble ends beside its own row").toBeLessThanOrEqual(own!.y + own!.height + 20);
});

// Why: when no row can be ticked the checkbox column is an empty strip in front of the names; with a
// new device in the scan it is the first column and holds its checkbox.
test("the selection column exists only while a row can be selected", async ({ page }) => {
  await openScan(page, "known");
  await expect(page.locator(".add-scan-table thead th").first()).toHaveText("Name");
  await expect(page.locator(".add-scan-table td.sel")).toHaveCount(0);
  await openScan(page, "mixed");
  await expect(page.locator(".add-scan-table thead th").first()).toHaveClass(/sel/);
  await expect(page.locator(".add-scan-table td.sel input[type=checkbox]")).toHaveCount(2); // the two new devices
});

// Why: the Known and All chips count 20 devices; the table lists 20 rows, and no line leaves devices out.
test("every device the chips count is a row", async ({ page }) => {
  await openScan(page, "known");
  await expect(page.locator(".add-chips .chip[aria-current]")).toContainText("20");
  await expect(page.locator(".add-scan-table tbody tr")).toHaveCount(20);
  await expect(page.getByText(/more known devices/)).toHaveCount(0);
});

// The card list of a phone (375 px): the same devices and the number of the version, no table.
test("phone: cards list every device with the plain version", async ({ page }) => {
  await openScan(page, "known", "light", 375);
  await expect(page.locator(".add-scan-table")).toBeHidden();
  await expect(page.locator(".add-scan-card")).toHaveCount(20);
  await expect(page.locator(".add-scan-card").first()).toContainText("16.0.0");
  await expect(page.locator(".add-scan-card").first()).not.toContainText("release");
  const wide = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  expect(wide, "no horizontal scroll on the phone").toBeLessThanOrEqual(0);
});
