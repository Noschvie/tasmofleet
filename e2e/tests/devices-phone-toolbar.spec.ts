import { test, expect } from "../support/fixtures";
import { designAdmin } from "../support/seed";
import { startVisualStack, type VisualStack } from "../support/visual-stack";

// #1133 (A1-024): at 375 px the Devices toolbar was search, the chips on three rows, the Chip and Tag selects, a
// right-aligned Sort select and "Select all 60", and the first device started at y 550 on the 60-device fleet, below
// the fold of an 812 px screen. Now the chips are one strip that scrolls and fades, Chip, Tag and Sort are one Filters
// fold, and Select all is a button of the bulk bar, so the first device starts above 400 px. The page is measured after
// it loaded; no live swap is waited for.
let fleet: VisualStack;
test.describe.configure({ mode: "serial" });
test.beforeAll(async ({ browser }) => {
  test.setTimeout(600_000);
  fleet = await startVisualStack("fleet", browser);
});
test.afterAll(async () => fleet?.stop());
test.use({ extraHTTPHeaders: designAdmin });

test("at 375 px the first device card starts above 400 px with 60 devices", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto(fleet.app.url + "/devices");
  const first = page.locator("li.dcard").first();
  await expect(first).toBeVisible();
  const top = await first.evaluate((c) => c.getBoundingClientRect().top + window.scrollY);
  expect(top, "the first card's distance from the top of the page (550 px before #1133)").toBeLessThan(400);
  // nothing the person needs is lost for it: the strip scrolls and says so, and Filters opens to Chip, Tag and Sort
  const strip = page.locator(".dv-list .dv-chips");
  expect(await strip.evaluate((s) => s.scrollWidth - s.clientWidth), "the chip strip scrolls sideways").toBeGreaterThan(0);
  await expect(strip).toHaveAttribute("data-more-end", "");
  await expect(page.getByRole("combobox", { name: "Chip" })).toBeHidden();
  await page.getByText("Filters", { exact: true }).click();
  for (const name of ["Chip", "Tag", "Sort"]) await expect(page.getByRole("combobox", { name })).toBeVisible();
});

test("the Filters button counts the Chip and Tag filters that are on", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto(fleet.app.url + "/devices");
  await expect(page.locator(".dv-filters > summary")).toHaveText("Filters");
  await page.locator(".dv-filters > summary").click();
  const chip = page.getByRole("combobox", { name: "Chip" });
  const family = (await chip.locator("option").evaluateAll((os) => (os as HTMLOptionElement[]).map((o) => o.value).filter((v) => v)))[0];
  await Promise.all([page.waitForURL(/family=/), chip.selectOption(family)]);
  // a folded fold must still say the list is narrowed
  await expect(page.locator(".dv-filters > summary")).toHaveText("Filters (1)");
  await expect(page.locator(".dv-filters")).not.toHaveAttribute("open", "");
});

test("Select all N is a button of the bulk bar; a selection starts with a card's own box", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto(fleet.app.url + "/devices");
  const cards = page.locator("li.dcard");
  await expect(cards.first()).toBeVisible();
  const n = await cards.count();
  expect(n).toBeGreaterThan(30);
  const bar = page.getByRole("region", { name: "Selected devices" });
  await expect(bar).toBeHidden();
  await expect(page.getByText(/^Select all \d+$/)).toBeHidden(); // nothing above the cards
  await cards.first().getByRole("checkbox").check();
  await expect(bar).toBeVisible();
  await bar.getByRole("button", { name: `Select all ${n}`, exact: true }).click();
  await expect(bar).toContainText(`${n} devices selected`);
  await expect(page.locator("li.dcard input[name=id]:checked")).toHaveCount(n);
});

test("from 45rem the toolbar shows Chip and Tag without a Filters button", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(fleet.app.url + "/devices");
  await expect(page.getByRole("combobox", { name: "Chip" })).toBeVisible();
  await expect(page.getByRole("combobox", { name: "Tag" })).toBeVisible();
  await expect(page.locator(".dv-filters > summary")).toBeHidden();
  await expect(page.getByRole("combobox", { name: "Sort" })).toBeHidden(); // the table's headers sort
});
