import { expect, type Page } from "@playwright/test";

// The new update and transaction pages (design v2, WP P4) as the specs drive them.

/** pickForUpdate opens the new update with exactly these devices ticked (the page starts with every outdated one). */
export async function pickForUpdate(page: Page, base: string, names: string[]): Promise<void> {
  await page.goto(base + "/updates/new");
  await quickPick(page, "None");
  for (const n of names) await page.getByLabel(`Select ${n}`, { exact: true }).check();
}

/** startUpdate reviews the picked update and starts it; it ends on the new transaction's page. */
export async function startUpdate(page: Page): Promise<void> {
  await page.getByRole("button", { name: "Review update" }).click();
  await page.getByRole("button", { name: "Start update", exact: true }).click();
  await expect(page).toHaveURL(/\/transactions\/\d+$/);
}

/** txnState is the state badge of the open transaction: Running, Completed, Cancelled, "1 done · 1 failed", ... */
export const txnState = (page: Page) => page.locator("#txn-state");

/** quickPick chooses an entry of the new update's quick selection menu ("All that can be updated", "None", ...). */
export async function quickPick(page: Page, item: string | RegExp): Promise<void> {
  await page.locator("details.upd-quick > summary").click();
  await page.getByRole("button", { name: item, exact: typeof item === "string" }).click();
}
