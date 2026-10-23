import { test, expect } from "../support/fixtures";
import { App, startApp, startProxy } from "../support/stack";
import type { Server } from "node:http";

// Header mode behind a tiny forward-auth proxy. The only trusted peer is
// 127.0.0.2, which is the proxy's source address; direct connections from
// 127.0.0.1 must have their identity headers ignored.
let app: App;
let proxyURL: string;
let proxy: Server;
test.beforeAll(async () => {
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.2/32",
      TASMOFLEET_OPERATOR_GROUPS: "Tasmofleet Users",
      TASMOFLEET_ADMIN_GROUPS: "Tasmofleet Admins",
    },
  });
  ({ url: proxyURL, server: proxy } = await startProxy(app.url));
});
test.afterAll(async () => {
  proxy.close();
  await app.stop();
});

test("operator sees the app but not admin pages", async ({ page }) => {
  await page.setExtraHTTPHeaders({ "X-Test-User": "olivia", "X-Test-Groups": "Tasmofleet Users" });
  await page.goto(proxyURL + "/");
  await expect(page.getByRole("heading", { name: "Fleet overview" })).toBeVisible();
  await expect(page.locator("summary.um-btn").getByText("olivia")).toBeVisible();
  await expect(page.getByRole("link", { name: "Audit" })).toHaveCount(0);
  const res = await page.goto(proxyURL + "/audit");
  expect(res?.status()).toBe(403);
});

test("admin sees the audit log", async ({ page }) => {
  await page.setExtraHTTPHeaders({ "X-Test-User": "adam", "X-Test-Groups": "Tasmofleet Users|Tasmofleet Admins" });
  await page.goto(proxyURL + "/audit");
  await expect(page.getByRole("heading", { name: "Audit log" })).toBeVisible();
});

test("a user without a mapped group is denied", async ({ page }) => {
  await page.setExtraHTTPHeaders({ "X-Test-User": "nina", "X-Test-Groups": "Unrelated" });
  const res = await page.goto(proxyURL + "/");
  expect(res?.status()).toBe(403);
});

test("forged identity headers on a direct connection are ignored", async ({ request }) => {
  const res = await request.get(app.url + "/devices", {
    headers: { "X-authentik-username": "mallory", "X-authentik-groups": "Tasmofleet Admins" },
    maxRedirects: 0,
  });
  expect(res.status()).not.toBe(200);
});

// Review F1: an anonymous client must not get its (multipart or huge) body parsed.
test("anonymous multipart and oversized posts are refused early", async ({ request }) => {
  const multipart = await request.post(app.url + "/devices/new", {
    multipart: { file: { name: "x.bin", mimeType: "application/octet-stream", buffer: Buffer.alloc(1024) } },
  });
  expect([403, 415]).toContain(multipart.status());
  const big = await request.post(app.url + "/devices/new", {
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    data: "a=" + "x".repeat(2 << 20),
  });
  expect(big.status()).toBe(403);
});
