import test from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { readFileSync } from "node:fs";
import { webcrypto } from "node:crypto";
import vm from "node:vm";

// Web Push in the browser (#611): the pure parts of internal/web/static/push.js (which state the card says,
// how this browser finds its own row) and the service worker sw.js, run in a sandbox that stands in for
// the worker's global scope.
const require = createRequire(import.meta.url);
const push = require("../internal/web/static/push.js");
const notify = require("../internal/web/static/notify.js");
const read = (f) =>
  readFileSync(new URL("../internal/web/static/" + f, import.meta.url), "utf8");

const env = (o) => ({
  secure: true,
  serviceWorker: true,
  pushManager: true,
  ios: false,
  standalone: false,
  permission: "default",
  local: null,
  ...o,
});

// Why: a page that cannot subscribe must say why before anything else, in the order the approved mockup
// shows: on plain HTTP the person needs HTTPS, not "unsupported" or a button that cannot work.
test("state: insecure comes before unsupported, iOS before unsupported, denied before subscribed", () => {
  assert.equal(
    push.stateOf(
      env({ secure: false, serviceWorker: false, pushManager: false }),
    ),
    "insecure",
  );
  assert.equal(
    push.stateOf(
      env({
        secure: false,
        permission: "denied",
        local: { known: true, keyMatches: true },
      }),
    ),
    "insecure",
  );
  assert.equal(push.stateOf(env({ pushManager: false, ios: true })), "ios");
  assert.equal(
    push.stateOf(env({ serviceWorker: false, ios: true, standalone: true })),
    "unsupported",
  );
  assert.equal(push.stateOf(env({ pushManager: false })), "unsupported");
  assert.equal(
    push.stateOf(
      env({ permission: "denied", local: { known: true, keyMatches: true } }),
    ),
    "denied",
  );
});

// Why: a subscription the server does not list, or one made for another key, receives nothing. The card must
// not say "subscribed" for it, or push stops without a sign (docs/security/web-push.md, W2).
test("state: gone on a missing row or a key mismatch, subscribed only when both hold", () => {
  assert.equal(push.stateOf(env()), "off");
  assert.equal(
    push.stateOf(env({ local: { known: true, keyMatches: true } })),
    "subscribed",
  );
  assert.equal(
    push.stateOf(env({ local: { known: false, keyMatches: true } })),
    "gone",
  );
  assert.equal(
    push.stateOf(env({ local: { known: true, keyMatches: false } })),
    "gone",
  );
});

test("every state has a tone, a title and a sentence; the insecure one names the address and the way out", () => {
  for (const s of [
    "insecure",
    "ios",
    "unsupported",
    "denied",
    "gone",
    "subscribed",
    "off",
  ]) {
    const [tone, title, text] = push.describe(s, "http://tasmofleet.lan:8080");
    assert.match(tone, /^(ok|warn|danger|neutral)$/, s);
    assert.ok(title.length > 0 && text.length > 0, s);
  }
  const text = push.describe("insecure", "http://tasmofleet.lan:8080")[2];
  assert.match(text, /http:\/\/tasmofleet\.lan:8080/);
  assert.match(text, /HTTPS/);
  assert.match(text, /localhost/);
});

test("iOS: iPhone and iPad, also an iPad that reports itself as a Mac", () => {
  assert.equal(
    push.isIOS(
      "Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X)",
      "iPhone",
      5,
    ),
    true,
  );
  assert.equal(
    push.isIOS(
      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)",
      "MacIntel",
      5,
    ),
    true,
  );
  assert.equal(
    push.isIOS(
      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)",
      "MacIntel",
      0,
    ),
    false,
  );
  assert.equal(
    push.isIOS("Mozilla/5.0 (X11; Linux x86_64)", "Linux x86_64", 0),
    false,
  );
});

// Why: the page never carries a subscription's endpoint (its path is the browser's capability); the row is
// named by a hash, and the browser finds its own row by hashing its endpoint the same way. This vector is the
// one internal/notify TestSubscriptionIDIsSHA256OfEndpoint checks in Go, so the two sides cannot drift.
test("id: base64url SHA-256 of the endpoint, the vector the Go side checks", async () => {
  assert.equal(
    await push.idOf("https://push.example/send/abc123", webcrypto.subtle),
    "qB6dm6Gh1VhQw65vM85YGzBtiCPxGKw9rJkTClwwb6E",
  );
});

test("key: a subscription counts only for the server's current key", () => {
  const key = new Uint8Array([4, 1, 2, 3, 250, 251, 252]);
  const b64 = Buffer.from(key).toString("base64url");
  assert.equal(push.keyMatches(key.buffer, b64), true);
  assert.equal(
    push.keyMatches(new Uint8Array([4, 1, 2, 3, 250, 251, 253]).buffer, b64),
    false,
  );
  assert.equal(push.keyMatches(new Uint8Array([4, 1, 2]).buffer, b64), false);
  assert.equal(push.keyMatches(null, b64), false);
  assert.equal(push.keyMatches(key.buffer, ""), false);
});

// The worker: a sandbox with the globals a service worker has. importScripts runs the file in the same
// context, as in a browser, so the worker sees what notify.js leaves on the global scope.
function worker(base = "https://tf.example/tf/") {
  const handlers = {};
  const shown = [];
  const opened = [];
  const sandbox = {
    URL,
    JSON,
    Promise,
    self: null,
  };
  sandbox.self = sandbox;
  sandbox.location = { origin: "https://tf.example" };
  sandbox.registration = {
    scope: base,
    showNotification: (title, options) => (
      shown.push({ title, options }),
      Promise.resolve()
    ),
  };
  sandbox.clients = {
    matchAll: () => Promise.resolve(sandbox.windows ?? []),
    openWindow: (u) => (opened.push(u), Promise.resolve()),
  };
  sandbox.addEventListener = (type, fn) => (handlers[type] = fn);
  const imported = [];
  sandbox.importScripts = (url) => {
    imported.push(url);
    assert.equal(
      url,
      "static/notify.js",
      "the worker imports notify.js by its plain URL, never a ?v= one",
    );
    vm.runInContext(read("notify.js"), ctx, { filename: "notify.js" });
  };
  const ctx = vm.createContext(sandbox);
  vm.runInContext(read("sw.js"), ctx, { filename: "sw.js" });
  const fire = async (type, event) => {
    const waits = [];
    handlers[type]({ ...event, waitUntil: (p) => waits.push(p) });
    await Promise.all(waits);
  };
  return { sandbox, shown, opened, imported, fire };
}

// Why: a push and an open tab must show one notification for one event. That needs the same tag, which is
// the same function: the worker builds its notification with notify.js toNotification, not a copy.
test("sw: a push shows the notification toNotification builds (same tag as the tab)", async () => {
  const w = worker();
  assert.deepEqual(w.imported, ["static/notify.js"]);
  const payload = {
    id: "1f-7",
    name: "update.finished",
    result: "failed",
    title: "Update job #2: 1 failed",
    text: "1 device",
    path: "/tf/transactions/2",
  };
  await w.fire("push", { data: { json: () => payload } });
  const want = notify.toNotification(payload);
  assert.equal(w.shown.length, 1);
  assert.equal(w.shown[0].title, want.title);
  assert.deepEqual(
    JSON.parse(JSON.stringify(w.shown[0].options)),
    want.options,
  );
  assert.equal(w.shown[0].options.tag, "tasmofleet-1f-7");
});

// Why: Chrome requires every push to show a notification (userVisibleOnly), and shows its own text otherwise;
// an unreadable payload must still show ours, so the person sees that something arrived.
test("sw: a payload that is no notification still shows something", async () => {
  for (const data of [
    null,
    {
      json: () => {
        throw new Error("not json");
      },
    },
    { json: () => ({ id: "", title: "" }) },
  ]) {
    const w = worker();
    await w.fire("push", { data });
    assert.equal(w.shown.length, 1);
    assert.equal(w.shown[0].title, "TasmoFleet");
  }
});

// Why: the click opens the page the event is about (the path carries the base path); a path that is not on
// this origin is never opened, the app's root is instead. A window that already shows the page is focused.
test("sw: a click opens the event's page on this origin, or focuses it", async () => {
  const w = worker();
  const close = () => {};
  await w.fire("notificationclick", {
    notification: { close, data: { path: "/tf/transactions/2" } },
  });
  assert.deepEqual(w.opened, ["https://tf.example/tf/transactions/2"]);
  await w.fire("notificationclick", {
    notification: { close, data: { path: "https://evil.example/x" } },
  });
  assert.equal(
    w.opened[1],
    "https://tf.example/tf/",
    "a cross-origin path opens the root",
  );
  await w.fire("notificationclick", { notification: { close, data: {} } });
  assert.equal(w.opened[2], "https://tf.example/tf/");
  let focused = 0;
  w.sandbox.windows = [
    {
      url: "https://tf.example/tf/transactions/2",
      focus: () => (focused++, Promise.resolve()),
    },
  ];
  await w.fire("notificationclick", {
    notification: { close, data: { path: "/tf/transactions/2" } },
  });
  assert.equal(focused, 1);
  assert.equal(
    w.opened.length,
    3,
    "no new window when the page is already open",
  );
});
