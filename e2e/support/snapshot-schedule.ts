import { expect, Page } from "@playwright/test";

const days = ["mon", "tue", "wed", "thu", "fri", "sat", "sun"];

/**
 * soonClock is a time of day (HH:MM) 20 to 80 seconds ahead, on the clock the app runs on (it inherits
 * this process's TZ). A snapshot never runs because the schedule was saved (#602), so a test that needs
 * one saves a schedule whose next time is this close and waits for it.
 */
export function soonClock(): string {
  return new Date(Date.now() + 80_000).toTimeString().slice(0, 5);
}

/** scheduleSnapshotSoon ticks every day on System, Snapshots, sets the time soonClock() and saves. */
export async function scheduleSnapshotSoon(page: Page, base: string): Promise<void> {
  await page.goto(base + "/settings/system/snapshots");
  for (const d of days) {
    await page.locator(`input[name='v.SNAPSHOT_DAYS'][value='${d}']`).check();
  }
  await page.locator("input[name='v.SNAPSHOT_TIME']").fill(soonClock());
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Saved.")).toBeVisible();
}
