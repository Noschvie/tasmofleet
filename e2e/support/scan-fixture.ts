import { readFileSync } from "node:fs";
import { join } from "node:path";
import type { Page } from "@playwright/test";
import { expect } from "@playwright/test";
import { root } from "./stack";

// A scan only finds devices on port 80, which no e2e stack can serve, so the Network scan result page is
// loaded as the real binary serves it (shell, CSS, scripts) and the finished scan's markup is put into it:
// testdata/scan/*.html, which TestScanResultFixtures (internal/web) keeps equal to what the templates render.
// SCAN_FIXTURE_DIR points a run at other markup (a before/after comparison).
const fixtureDir = process.env.SCAN_FIXTURE_DIR ?? join(root, "testdata/scan");

/** putScanResult replaces the scan box of the open /discovery/scan page with a finished scan's markup. */
export async function putScanResult(page: Page, fixture: "known" | "mixed"): Promise<void> {
  const html = readFileSync(join(fixtureDir, fixture + ".html"), "utf8");
  await page.evaluate((h) => {
    document.querySelector("#scan")!.outerHTML = h;
  }, html);
  await page.evaluate(() => document.fonts.ready);
  const width = page.viewportSize()?.width ?? 1280;
  await expect(page.locator(width >= 720 ? ".add-scan-table tbody tr" : ".add-scan-card").first()).toBeVisible();
}

/** openScan loads /discovery/scan of the app as the given header user and puts a finished scan's result into it. */
export async function openScan(page: Page, appUrl: string, headers: Record<string, string>, fixture: "known" | "mixed", scheme: "light" | "dark" = "light", width = 1280): Promise<void> {
  await page.setExtraHTTPHeaders(headers);
  await page.setViewportSize({ width, height: 900 });
  await page.emulateMedia({ colorScheme: scheme });
  await page.goto(appUrl + "/discovery/scan");
  await putScanResult(page, fixture);
}
