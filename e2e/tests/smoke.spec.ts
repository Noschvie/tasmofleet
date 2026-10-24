import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// Smoke test: the binary starts without MQTT and without a firmware base URL
// (OTA disabled), answers its probes and serves the vendored HTMX build.
let app: App;
test.beforeAll(async () => {
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "Tasmofleet Users",
    },
  });
});
test.afterAll(async () => app.stop());

test("health and readiness probes", { tag: "@core" }, async ({ request }) => {
  const h = await request.get(`${app.url}/healthz`);
  expect(h.status()).toBe(200);
  expect(await h.text()).toBe("ok\n");
  expect((await request.get(`${app.url}/readyz`)).status()).toBe(200);
});

test("vendored htmx is served", { tag: "@core" }, async ({ page }) => {
  const res = await page.goto(`${app.url}/static/htmx-2.0.11.min.js`);
  expect(res?.status()).toBe(200);
  expect(await res?.text()).toContain("htmx");
});

test("metrics expose the contract", { tag: "@core" }, async ({ request }) => {
  const res = await request.get(`${app.url}/metrics`);
  expect(res.status()).toBe(200);
  expect(await res.text()).toContain("tasmofleet_build_info");
});
