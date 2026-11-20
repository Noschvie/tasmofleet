import { mkdirSync } from "node:fs";
import type { Locator, Page } from "@playwright/test";

/**
 * aclShots keeps full-page screenshots of the page as it stands, at 1280 and 375 px in light and dark, in SHOT_DIR
 * (nothing without it). Review material for the broker ACL notice (#840); the viewport and colour scheme come back.
 */
export async function aclShots(page: Page, name: string): Promise<void> {
  const dir = process.env.SHOT_DIR;
  if (!dir) return;
  mkdirSync(dir, { recursive: true });
  const before = page.viewportSize();
  for (const width of [1280, 375]) {
    for (const scheme of ["light", "dark"] as const) {
      await page.setViewportSize({ width, height: 1000 });
      await page.emulateMedia({ colorScheme: scheme });
      await page.screenshot({ path: `${dir}/${name}-${width}-${scheme}.png`, fullPage: true });
    }
  }
  await page.emulateMedia({ colorScheme: null });
  if (before) await page.setViewportSize(before);
}

/** ACL_LEAD is the lead of the broker ACL notice (ui.BrokerACLLead), the same on every page that shows it (#1078). */
export const ACL_LEAD = "Your MQTT broker has no ACL.";

/** aclNotice is the broker ACL notice on the page (or inside a part of it): the banner that carries the shared lead. */
export function aclNotice(scope: Page | Locator): Locator {
  return scope.locator(".banner", { hasText: ACL_LEAD });
}
