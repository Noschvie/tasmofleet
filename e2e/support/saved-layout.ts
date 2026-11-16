import { expect, type Page } from "@playwright/test";

// #821: a saved column layout of the Devices list that is wider than the frame, as a standard case. The
// default layout is the one a person never has, and none of the table defects the owner found right after
// 0.36.0 (#805: a 1 px scroll and a table that grew with every show, #806: a list reload that threw the
// scroll back, #811) showed in it: they need a layout that is saved, wider than the frame, in a window of
// about 1290 px (the owner's). Specs that guard the list's layout use this one fixture at that width.

/**
 * savedLayoutUser is the header user the fixture saves the layout for. Not one of the users the other
 * tests of a spec use: they keep the default layout, and a failed test leaves this layout only in the
 * spec's own throwaway database.
 */
export const savedLayoutUser = { "X-authentik-username": "lena", "X-authentik-groups": "adm" };

/** the window width of the owner, where the saved layout is wider than the frame (the frame is about 1210 px) */
export const ownerWindow = 1290;

/**
 * ownerLayout is the maintainer's layout from the live database as 0.36.0 stored it (no "b", no "c": the
 * widths the browser drew, in tenths of pixels). Every version since accepts this document; an older one
 * refuses the fields b and c, so a check against an older commit needs this form.
 */
export const ownerLayout = {
  w: { actions: 5.7, address: 13.3, family: 9.8, firmware: 9.3, name: 22.8, power: 7.5, reach: 14.2, status: 10.5, uptime: 11.5, wifi: 11.9 },
  h: ["restart"],
  o: ["status", "wifi", "reach", "firmware", "family", "address", "uptime", "restart", "otaurl", "grouptopic", "power"],
};

/**
 * ownerShown is the owner's layout after showing Last restart, OTA URL and Group topic at 1920 px, as 0.36.0
 * stored it: the widths the browser drew, stretched to that window, about 1900 px of columns. In a window of
 * 1290 px (a frame of about 1210 px) the table scrolls inside its frame.
 */
export const ownerShown = {
  w: { name: 28.7, status: 13.2, wifi: 15, reach: 17.9, firmware: 11.7, family: 12.4, address: 16.7, uptime: 14.5, restart: 14.7, otaurl: 14.7, grouptopic: 14.7, power: 9.4, actions: 7.2 },
  v: ["grouptopic", "otaurl"],
  o: ownerLayout.o,
};

/**
 * ownerWide is the document the fixture saves: ownerShown with the current format's mark "c" (the widths are content
 * pixels, #828), which is how this version keeps a layout and what a person who saved one since 0.36.1 has.
 * Without the mark the server reads the widths of a 0.36.0 document as stretched shares and lays the table out
 * from its content (data-legacy), and at 1290 px that table is no wider than the frame. The documents differ by
 * one field on purpose: a commit before #828 refuses the field c, and one before #805 refuses b too, so the
 * one-time check of #821 against the commits before #805, #806 and #811 saves the document that commit knows:
 * SAVED_LAYOUT_FORMAT=0.36.0 (ownerShown as it is, before #805) or =b (the pixel widths of #805 to #827).
 */
const format = process.env.SAVED_LAYOUT_FORMAT;
export const ownerWide: Record<string, unknown> = format === "0.36.0" ? ownerShown : format === "b" ? { ...ownerShown, b: true } : { ...ownerShown, c: true };

/** postPref sends a preference the way the page script does: same origin, token from the page. */
export async function postPref(page: Page, key: string, doc: unknown): Promise<number> {
  return page.evaluate(
    async ([k, d]) => {
      const token = JSON.parse(document.body.getAttribute("hx-headers") ?? "{}")["X-CSRF-Token"];
      const r = await fetch(`${document.body.getAttribute("data-prefs")}/${k}`, { method: "POST", headers: { "Content-Type": "application/json", "X-CSRF-Token": token }, body: JSON.stringify(d) });
      return r.status;
    },
    [key, doc] as const,
  );
}

/**
 * openSavedLayoutList saves `doc` (default ownerWide) as the table.devices layout of the user in `headers`
 * (default savedLayoutUser), opens the Devices list at `width`, waits until the fleet has filled its cells and
 * the header widths stay put, and fails when the layout is not wider than the frame (the case would mean nothing).
 */
export async function openSavedLayoutList(page: Page, appUrl: string, opts: { width?: number; doc?: unknown; headers?: Record<string, string> } = {}): Promise<void> {
  await page.setViewportSize({ width: opts.width ?? ownerWindow, height: 600 });
  await page.setExtraHTTPHeaders(opts.headers ?? savedLayoutUser);
  await page.goto(appUrl + "/devices");
  await expect(page.locator("#devtable tbody tr").first()).toBeAttached({ timeout: 40_000 });
  expect(await postPref(page, "table.devices", opts.doc ?? ownerWide), "the layout is saved").toBe(204);
  await page.goto(appUrl + "/devices");
  await expect(page.locator("#devtable tbody tr").first()).toBeAttached({ timeout: 40_000 });
  await page.evaluate(() => document.fonts.ready);
  // the fleet comes online and fills its cells: wait until the header widths stay put
  let last = "";
  await expect
    .poll(
      async () => {
        const now = await page.evaluate(() => [...document.querySelectorAll("#devtable table[data-table=devices] thead th")].map((th) => th.getBoundingClientRect().width.toFixed(1)).join(","));
        const same = now === last;
        last = now;
        return same;
      },
      { timeout: 30_000, intervals: [700] },
    )
    .toBe(true);
  const frame = await page.evaluate(() => {
    const f = document.querySelector<HTMLElement>("#devtable .tablescroll")!;
    return { scroll: f.scrollWidth, client: f.clientWidth };
  });
  expect(frame.scroll, "the saved layout is wider than the frame").toBeGreaterThan(frame.client + 300);
}
