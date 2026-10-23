import type { Locator, Page } from "@playwright/test";

// The device list renders every device twice (design v2, F5): as a table row from 45rem and as a
// phone card below it; CSS shows one. These helpers find the one on screen, whatever the width.

/** deviceEntry is the device's table row or phone card, whichever is shown. */
export function deviceEntry(page: Page, name: string | RegExp): Locator {
  return page.locator("table[data-table=devices] tbody tr, li.dcard").filter({ visible: true }).filter({ hasText: name });
}

/** deviceBox is the device's selection box in the row or card on screen. */
export function deviceBox(page: Page, name: string): Locator {
  return page.getByRole("checkbox", { name: `Select ${name}`, exact: true });
}

/** sendCommand is the bulk bar's command button: "Send command" on a wide screen, "Command" on a phone. */
export function sendCommand(page: Page): Locator {
  return page.locator("[data-bulkbar]").getByRole("button", { name: /^(Send command|Command)$/ });
}
