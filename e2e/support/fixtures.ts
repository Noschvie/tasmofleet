import { test as base, expect } from "@playwright/test";

/**
 * test extends Playwright's test with a CSP check: the UI must not need inline
 * scripts or styles, so any securitypolicyviolation event fails the test.
 */
export const test = base.extend({
  page: async ({ page }, use) => {
    await page.addInitScript(() => {
      (window as unknown as { __csp: string[] }).__csp = [];
      document.addEventListener("securitypolicyviolation", (e) => {
        (window as unknown as { __csp: string[] }).__csp.push(`${e.violatedDirective} ${e.blockedURI}`);
      });
    });
    await use(page);
    const violations = await page.evaluate(() => (window as unknown as { __csp?: string[] }).__csp ?? []).catch(() => []);
    expect(violations, "CSP violations").toEqual([]);
  },
});

export { expect };
