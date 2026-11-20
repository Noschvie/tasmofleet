import { createECDH, randomBytes } from "node:crypto";
import { BrowserContext, Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// Settings, Notifications, Web Push card (issue #611): subscribe, test and remove. A push service cannot
// be reached from a headless browser (and nothing here may reach the internet), so the browser's
// PushManager is stubbed: subscribe() answers with a subscription held in localStorage. Everything else is
// real: the page, push.js, the service worker (registered from <base>/sw.js under the page's CSP, which the
// fixture checks), the routes and the store. The address of the stub's push service is a name that never
// resolves (.invalid), so a test push fails at once without leaving the machine. That a real push service
// delivers is checked by hand in a real browser over HTTPS or localhost, not here.
const b64u = (b: Buffer) => b.toString("base64url");
function keys() {
  const ecdh = createECDH("prime256v1");
  ecdh.generateKeys();
  return { p256dh: b64u(ecdh.getPublicKey()), auth: b64u(randomBytes(16)) };
}
const host = "push.invalid";

let app: App;
test.beforeEach(async () => {
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
    },
  });
  app.cli(["user", "passwd", "--create", "--role", "admin", "pusher"], "e2e-fake-password-1\n");
});
test.afterEach(async () => {
  await app?.stop();
});

/** stubPush replaces what a browser gets from its push service: permission and PushManager. */
async function stubPush(context: BrowserContext, secure = true) {
  const k = keys();
  await context.addInitScript(
    (cfg) => {
      const perm = () => localStorage.getItem("e2e-permission") ?? "default";
      const Orig = window.Notification;
      window.Notification = new Proxy(Orig, {
        get(target, key) {
          if (key === "permission") return perm();
          if (key === "requestPermission")
            return async () => {
              localStorage.setItem("e2e-permission", "granted");
              return "granted";
            };
          return Reflect.get(target, key);
        },
      });
      if (!cfg.secure) Object.defineProperty(window, "isSecureContext", { value: false });
      const KEY = "e2e-push-subscription";
      const make = (endpoint: string, key: number[]) => ({
        endpoint,
        options: { applicationServerKey: new Uint8Array(key).buffer },
        toJSON: () => ({ endpoint, expirationTime: null, keys: { p256dh: cfg.p256dh, auth: cfg.auth } }),
        unsubscribe: async () => {
          localStorage.removeItem(KEY);
          return true;
        },
      });
      PushManager.prototype.getSubscription = async function () {
        const s = JSON.parse(localStorage.getItem(KEY) ?? "null");
        return s ? (make(s.endpoint, s.key) as unknown as PushSubscription) : null;
      };
      PushManager.prototype.subscribe = async function (o) {
        const key = Array.from(new Uint8Array(o!.applicationServerKey as ArrayBuffer));
        const endpoint = `https://${cfg.host}/send/${Math.random().toString(36).slice(2)}`;
        localStorage.setItem(KEY, JSON.stringify({ endpoint, key }));
        return make(endpoint, key) as unknown as PushSubscription;
      };
    },
    { ...k, host, secure },
  );
}

async function signIn(page: Page) {
  await page.goto(app.url + "/login");
  await page.getByLabel("Username").fill("pusher");
  await page.getByLabel("Password").fill("e2e-fake-password-1");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Fleet overview" })).toBeVisible();
}

const card = (page: Page) => page.locator("#card-notify-push");
async function openCard(page: Page) {
  await page.goto(app.url + "/settings/system?tab=notifications");
  await expect(card(page)).toBeVisible();
}

/** seed stores a subscription for a browser that is not this one, as that browser would. */
async function seed(page: Page, name: string) {
  const k = keys();
  const status = await page.evaluate(
    async ([url, endpoint, p256dh, auth]) => {
      const csrf = JSON.parse(document.body.getAttribute("hx-headers") ?? "{}")["X-CSRF-Token"];
      const r = await fetch(url, {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf },
        body: JSON.stringify({ endpoint, keys: { p256dh, auth } }),
      });
      return r.status;
    },
    [app.url.replace(/^https?:\/\/[^/]+/, "") + "/settings/system/push/subscription", `https://${host}/send/${name}`, k.p256dh, k.auth],
  );
  expect(status).toBe(204);
}

// Why: the person must be able to subscribe a browser, see it in the list as this browser, see that a test
// reaches the server's sender, and remove it again, which must also drop the subscription the browser
// holds (otherwise it stays subscribed in the browser, with nothing on the server).
test("subscribe this browser, test it, remove it", { tag: "@core" }, async ({ page, context }) => {
  await stubPush(context);
  await signIn(page);
  await openCard(page);
  await expect(card(page)).toContainText("Not subscribed");
  await expect(card(page)).toContainText("No browser is subscribed yet.");

  await card(page).getByRole("button", { name: "Subscribe this browser" }).click();
  await expect(card(page)).toContainText("Subscribed in this browser");
  await expect(card(page)).toContainText("Subscribed browsers (1)");
  const row = card(page).locator("[data-push-id]");
  await expect(row.locator("[data-push-this]")).toBeVisible();
  await expect(row).toContainText(/Chrome on (Linux|Windows)/); // the label comes from the User-Agent of the subscribing request
  await expect(row).toContainText(host);
  await expect(row).toContainText("nothing sent yet");
  const endpointInPage = await page.evaluate(() => document.documentElement.outerHTML.includes("/send/"));
  expect(endpointInPage, "the endpoint's path must never be in the page").toBe(false);

  // the push service of this stub does not exist: the test must fail visibly, on the row, with its reason
  await card(page).getByRole("button", { name: "Send test" }).first().click();
  await expect(row.getByRole("alert")).toContainText("Push test: failed", { timeout: 20_000 });
  await expect(row).toContainText("Last delivery failed");
  await expect(row).not.toContainText("/send/");

  await row.getByRole("link", { name: "Remove" }).click();
  const dlg = page.locator("dialog[open]");
  await expect(dlg).toContainText(/Remove Chrome on (Linux|Windows)\?/);
  await dlg.getByRole("button", { name: "Remove" }).click();
  await expect(card(page)).toContainText("No browser is subscribed yet.");
  await expect(card(page)).toContainText("Not subscribed");
  expect(await page.evaluate(() => localStorage.getItem("e2e-push-subscription"))).toBeNull();
});

// Why: a subscription the server dropped (removed in the list, or the push service said 404/410) leaves the
// browser holding one that receives nothing. The card must say so and offer Subscribe again, not "subscribed".
test("a subscription the server no longer knows is offered again", { tag: "@core" }, async ({ page, context }) => {
  await stubPush(context);
  await signIn(page);
  await openCard(page);
  await card(page).getByRole("button", { name: "Subscribe this browser" }).click();
  await expect(card(page)).toContainText("Subscribed in this browser");
  const id = await card(page).locator("[data-push-id]").getAttribute("data-push-id");
  const csrf = await page.evaluate(() => JSON.parse(document.body.getAttribute("hx-headers") ?? "{}")["X-CSRF-Token"]);
  const gone = await page.request.post(`${app.url}/settings/system/push/remove/${id}`, { form: { csrf }, maxRedirects: 0 });
  expect(gone.status()).toBe(303);

  await openCard(page);
  await expect(card(page)).toContainText("Not subscribed any more");
  await card(page).getByRole("button", { name: "Subscribe again" }).click();
  await expect(card(page)).toContainText("Subscribed in this browser");
  await expect(card(page)).toContainText("Subscribed browsers (1)");
});

// Why (#611 acceptance): on plain HTTP a browser refuses push, so the page says so instead of showing a button
// that cannot work, and names the way out. The list does not depend on the browser's support: the server
// sends the test and removes the row, so both must still work.
test("plain HTTP explains instead of offering the button, and the list still works", async ({ page, context }) => {
  await stubPush(context, false);
  await signIn(page);
  await openCard(page);
  await expect(card(page)).toContainText("Not available on this connection");
  await expect(card(page)).toContainText("HTTPS");
  await expect(card(page)).toContainText("localhost");
  await expect(card(page).getByRole("button", { name: /Subscribe/ })).toBeHidden();
  await seed(page, "other-browser");
  await openCard(page);
  const row = card(page).locator("[data-push-id]");
  await expect(row).toHaveCount(1);
  await expect(row.locator("[data-push-this]")).toBeHidden();
  await row.getByRole("button", { name: "Send test" }).click();
  await expect(row.getByRole("alert")).toContainText("Push test: failed", { timeout: 20_000 });
  await row.getByRole("link", { name: "Remove" }).click();
  await page.locator("dialog[open]").getByRole("button", { name: "Remove" }).click();
  await expect(card(page)).toContainText("No browser is subscribed yet.");
});

// Why: without scripts Remove is a link to a confirmation page and Send test posts the page; both must work
// (the list is the server's), and opening the confirmation changes nothing.
test("without scripts: confirmation page for Remove, Send test reloads with the result", async ({ browser, page, context }) => {
  await stubPush(context);
  await signIn(page);
  await openCard(page);
  await seed(page, "no-script-browser");
  const plain = await browser.newContext({ javaScriptEnabled: false });
  try {
    const p = await plain.newPage();
    // the plain context signs in with the form too
    await p.goto(app.url + "/login");
    await p.getByLabel("Username").fill("pusher");
    await p.getByLabel("Password").fill("e2e-fake-password-1");
    await p.getByRole("button", { name: "Sign in" }).click();
    await p.goto(app.url + "/settings/system?tab=notifications");
    const row = p.locator("#card-notify-push [data-push-id]");
    await expect(row).toHaveCount(1);
    await row.getByRole("button", { name: "Send test" }).click();
    await expect(p.locator("#card-notify-push [data-push-id]")).toContainText("Push test: failed", { timeout: 20_000 });
    await p.locator("#card-notify-push [data-push-id]").getByRole("link", { name: "Remove" }).click();
    await expect(p.getByRole("heading", { name: "Remove this browser" })).toBeVisible();
    await p.getByRole("button", { name: "Remove" }).click();
    await expect(p.locator("#card-notify-push")).toContainText("No browser is subscribed yet.");
  } finally {
    await plain.close();
  }
});

// Why (#1059): a row that Send test swaps in comes from the server without the mark of the browser that
// is looking at it. push.js marks it and puts it first again, but only after a swap that brings a row, no
// longer after every settled swap of the page: the person must still see which row is this browser.
test("a row swapped by Send test is still marked as this browser and listed first (#1059)", async ({ page, context }) => {
  await stubPush(context);
  await signIn(page);
  await openCard(page);
  await seed(page, "other-browser");
  await openCard(page);
  await card(page).getByRole("button", { name: "Subscribe this browser" }).click();
  await expect(card(page)).toContainText("Subscribed browsers (2)");
  const mine = card(page).locator("[data-push-id]").filter({ has: page.locator("[data-push-this]:visible") });
  await expect(mine).toHaveCount(1);
  const id = await mine.getAttribute("data-push-id");
  await mine.getByRole("button", { name: "Send test" }).click();
  await expect(mine.getByRole("alert")).toContainText("Push test: failed", { timeout: 20_000 });
  const row = card(page).locator(`[data-push-id="${id}"]`);
  await expect(row.locator("[data-push-this]")).toBeVisible();
  await expect(card(page).locator("[data-push-id]").first()).toHaveAttribute("data-push-id", id!);
});
