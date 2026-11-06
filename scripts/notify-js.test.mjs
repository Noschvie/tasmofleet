import test from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

// The pure parts of internal/web/static/notify.js (#540): which state the Settings card shows,
// how a server event becomes a notification, and that one event is shown once.
const { stateOf, toNotification, newSeen } = createRequire(import.meta.url)("../internal/web/static/notify.js");

const env = (o) => ({ secure: true, api: true, permission: "default", optedIn: false, ...o });

// Why: the Notification API exists only on a secure page. The person must be told that, not shown
// a button that cannot work, and an insecure page must not read "denied" (the fix differs).
test("state: an insecure page is unavailable whatever the permission says", () => {
  assert.equal(stateOf(env({ secure: false, permission: "granted", optedIn: true })), "insecure");
  assert.equal(stateOf(env({ secure: false, api: false })), "insecure");
});

test("state: no API, denied, off, enabled", () => {
  assert.equal(stateOf(env({ api: false })), "unsupported");
  assert.equal(stateOf(env({ permission: "denied", optedIn: true })), "denied");
  assert.equal(stateOf(env()), "off");
  // Why: permission is per site, the opt-in per browser profile; both must hold to show anything.
  assert.equal(stateOf(env({ permission: "granted" })), "off");
  assert.equal(stateOf(env({ optedIn: true })), "off");
  assert.equal(stateOf(env({ permission: "granted", optedIn: true })), "enabled");
});

// Why: a failed backup or update must stay on screen until looked at; a successful one need not.
// The tag is what lets a browser fold a repeat into one notification.
test("mapping: title, body, tag, click target; problems stay until dismissed", () => {
  const n = toNotification({ id: "7-1", name: "update.finished", result: "partial", title: "Update job #2: 2 ok, 1 failed", text: "3 devices", path: "/tf/transactions/2" });
  assert.equal(n.id, "7-1");
  assert.equal(n.title, "Update job #2: 2 ok, 1 failed");
  assert.deepEqual(n.options, { body: "3 devices", tag: "tasmofleet-7-1", requireInteraction: true, data: { path: "/tf/transactions/2" } });
  const ok = toNotification({ id: "7-2", result: "ok", title: "Update job #3: 1 ok" });
  assert.equal(ok.options.requireInteraction, false);
  assert.equal(ok.options.body, "");
  assert.equal(ok.options.data.path, "");
});

test("mapping: an event without id or title is dropped, not shown blank", () => {
  assert.equal(toNotification(null), null);
  assert.equal(toNotification({ title: "x" }), null);
  assert.equal(toNotification({ id: "1", title: "" }), null);
  assert.equal(toNotification({ id: 1, title: "x" }), null);
});

// Why: every open tab hears the event, and the leader also gets it again over the BroadcastChannel;
// the person must see one notification.
test("dedupe: an id passes once; old ids are forgotten only beyond the limit", () => {
  const first = newSeen(2);
  assert.equal(first("a"), true);
  assert.equal(first("a"), false);
  assert.equal(first("b"), true);
  assert.equal(first("c"), true); // a falls out
  assert.equal(first("b"), false);
  assert.equal(first("a"), true);
});
