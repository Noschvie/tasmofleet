import { test, expect } from "../support/fixtures";
import { designAdmin } from "../support/seed";
import { startVisualStack, type VisualStack } from "../support/visual-stack";

// #1109: on a phone the seven stat tiles took four rows of about 100 px and pushed "Needs attention", the card a person
// opens the dashboard for, below 640 px (738 px at 0.48.1 on the 60-device fleet). The tiles are compact now (label and
// value on one line), so the heading starts in the first screen. The card is also grouped by reason and each group is
// capped (#1247): no group lists more than five devices, and the heading counts what the card stands for.
// The page is measured after it loaded; no live swap is waited for.
let fleet: VisualStack;
test.describe.configure({ mode: "serial" });
test.beforeAll(async ({ browser }) => {
  test.setTimeout(600_000);
  fleet = await startVisualStack("fleet", browser);
});
test.afterAll(async () => fleet?.stop());
test.use({ extraHTTPHeaders: designAdmin });

test("at 375 px the Needs attention heading starts above 640 px with 60 devices", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto(fleet.app.url + "/");
  const heading = page.getByRole("heading", { name: /^Needs attention/ });
  await expect(heading).toBeVisible();
  const top = await heading.evaluate((h) => h.getBoundingClientRect().top + window.scrollY);
  expect(top, "the heading's distance from the top of the page").toBeLessThan(640);
});

test("Needs attention counts its entries and caps every group at five devices", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(fleet.app.url + "/");
  const heading = page.getByRole("heading", { name: /^Needs attention \(\d+\)$/ });
  await expect(heading).toBeVisible();
  const card = page.locator("section.ulist").filter({ has: heading });
  const groups = card.locator(".att-group");
  expect(await groups.count(), "the fleet has devices of several reasons").toBeGreaterThan(1);
  for (const g of await groups.all()) {
    expect(await g.locator("a.att-item").count(), "a group lists at most five rows").toBeLessThanOrEqual(5);
    await expect(g.getByRole("heading", { level: 3 })).toBeVisible();
  }
  // the count is the rows the card stands for: every device of every group is in it, so it is never below what is drawn
  const n = Number((await heading.textContent())!.match(/\((\d+)\)/)![1]);
  expect(n).toBeGreaterThanOrEqual(await card.locator(".att-row").count());
});
