import { Page } from "@playwright/test";

/** openMenu opens a top-bar group ("Devices", "Updates", "System"): the group is a link to its first page, whose sub-navigation lists the group's pages. */
export async function openMenu(page: Page, group: string): Promise<void> {
  await page.locator("nav.tb-groups a", { hasText: group }).click();
  await page.waitForLoadState("domcontentloaded");
}

/** gotoNav opens a group and clicks one of its pages in the sub-navigation. */
export async function gotoNav(page: Page, group: string, link: string): Promise<void> {
  await openMenu(page, group);
  await page.locator("nav.sn-in").getByRole("link", { name: link, exact: true }).click();
}

/** openAccountMenu opens the user menu of the top bar (avatar button). */
export async function openAccountMenu(page: Page): Promise<void> {
  const menu = page.locator("details.usermenu:not([data-static])");
  if ((await menu.getAttribute("open")) === null) await menu.locator("summary").click();
}

/** signOut clicks Sign out in the user menu. */
export async function signOut(page: Page): Promise<void> {
  await openAccountMenu(page);
  await page.locator("details.usermenu:not([data-static])").getByRole("button", { name: "Sign out" }).click();
}
