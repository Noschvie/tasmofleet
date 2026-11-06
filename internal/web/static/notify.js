// Browser notifications (#540): while a TasmoFleet tab is open, a finished update and a failed
// scheduled backup or snapshot show as desktop notifications. The server sends the same events
// as for the webhook and e-mail ("notice" on the page's event stream, only the ones the person's
// event settings ask for, to admins only); this file shows them with the Notification API. Opt-in
// per browser: the permission is asked for only after a click on "Enable browser notifications"
// in Settings, Notifications, and the choice lives in this browser's localStorage, not in the
// database. No service worker (Web Push is a separate issue): nothing shows while no tab is open.
//
// With several tabs open, every tab with live data hears the event. One tab (the one that holds a
// Web Lock) shows it; the others pass what they heard on to it over a BroadcastChannel, so the
// notification still comes if the leader's own stream is down. On a page without live data the
// leader opens a stream of the notices alone (follow). The event id and the notification tag fold
// repeats.
(function (root) {
  "use strict";

  var KEY = "tf.browser-notifications";
  var LOCK = "tfleet-browser-notifications";
  var CHANNEL = "tfleet-browser-notifications";

  // stateOf: what the Settings card says. env: {secure, api, permission, optedIn}.
  function stateOf(env) {
    if (!env.secure) {
      return "insecure";
    }
    if (!env.api) {
      return "unsupported";
    }
    if (env.permission === "denied") {
      return "denied";
    }
    return env.permission === "granted" && env.optedIn ? "enabled" : "off";
  }

  var TEXT = {
    insecure: [
      "neutral",
      "Not available on this connection",
      "Browsers allow notifications only on a secure page: open TasmoFleet over HTTPS (or as localhost).",
    ],
    unsupported: [
      "neutral",
      "Not supported",
      "This browser has no notification support.",
    ],
    denied: [
      "danger",
      "Blocked by this browser",
      "Allow notifications for this site in the browser's site settings, then reload this page.",
    ],
    off: [
      "neutral",
      "Off in this browser",
      "Enable them to get a desktop notification while a TasmoFleet tab is open.",
    ],
    enabled: [
      "ok",
      "On in this browser",
      "Shows while a TasmoFleet tab is open, for the events selected above. Closing every tab stops them.",
    ],
  };

  // toNotification: the event the server sent -> {id, title, options}, or null when it lacks what
  // a notification needs. tag folds a repeat of the same event into one notification.
  function toNotification(raw) {
    if (
      !raw ||
      typeof raw.id !== "string" ||
      raw.id === "" ||
      typeof raw.title !== "string" ||
      raw.title === ""
    ) {
      return null;
    }
    var problem = raw.result === "failed" || raw.result === "partial";
    return {
      id: raw.id,
      title: raw.title,
      options: {
        body: typeof raw.text === "string" ? raw.text : "",
        tag: "tasmofleet-" + raw.id,
        requireInteraction: problem,
        data: { path: typeof raw.path === "string" ? raw.path : "" },
      },
    };
  }

  // newSeen: first(id) is true once per id (the last max ids are remembered).
  function newSeen(max) {
    var ids = [];
    return function first(id) {
      if (ids.indexOf(id) >= 0) {
        return false;
      }
      ids.push(id);
      if (ids.length > max) {
        ids.shift();
      }
      return true;
    };
  }

  var api = {
    stateOf: stateOf,
    toNotification: toNotification,
    newSeen: newSeen,
    TEXT: TEXT,
  };
  if (typeof module === "object" && module.exports) {
    module.exports = api;
  }
  if (typeof document === "undefined") {
    return;
  }

  function optedIn() {
    try {
      return root.localStorage.getItem(KEY) === "1";
    } catch (e) {
      return false;
    }
  }
  function setOptedIn(on) {
    try {
      if (on) {
        root.localStorage.setItem(KEY, "1");
      } else {
        root.localStorage.removeItem(KEY);
      }
    } catch (e) {
      // storage blocked: the choice cannot be kept, the state stays "off"
    }
  }
  function state() {
    var has = "Notification" in root;
    return stateOf({
      secure: root.isSecureContext === true,
      api: has,
      permission: has ? root.Notification.permission : "default",
      optedIn: optedIn(),
    });
  }

  function show(title, options) {
    try {
      var n = new root.Notification(title, options);
      n.onclick = function () {
        root.focus();
        n.close();
        var path = options.data && options.data.path;
        if (path && root.location.pathname + root.location.search !== path) {
          root.location.href = path;
        }
      };
    } catch (e) {
      // some browsers refuse the constructor (they want a service worker); there is nothing to fall back to
    }
  }

  // Which tab shows: the one that holds the lock. Without the Locks API every tab would show;
  // the tag then folds the copies.
  var leader = !root.navigator || !root.navigator.locks;
  if (!leader) {
    root.navigator.locks.request(LOCK, function () {
      leader = true;
      follow();
      return new Promise(function () {}); // held until this tab closes
    });
  }
  var seen = newSeen(100);
  var own = null; // the leader's stream of the notices alone, on a page without live data
  var channel = null;
  try {
    channel = new root.BroadcastChannel(CHANNEL);
    channel.onmessage = function (e) {
      receive(e.data);
    };
  } catch (e) {
    channel = null;
  }

  function receive(raw) {
    var n = toNotification(raw);
    if (!n || !seen(n.id) || !leader || state() !== "enabled") {
      return;
    }
    show(n.title, n.options);
  }

  function hear(data) {
    var raw = null;
    try {
      raw = JSON.parse(data);
    } catch (err) {
      return;
    }
    if (channel) {
      channel.postMessage(raw);
    }
    receive(raw);
  }

  document.addEventListener("htmx:sseBeforeMessage", function (e) {
    var t = e.target;
    if (!(t instanceof Element) || !t.matches("[data-bn-sink]")) {
      return;
    }
    e.preventDefault();
    hear(e.detail.data);
  });

  // A page without live data (Settings, say) has no event stream. While notifications are on, its
  // leader opens one for the notices alone, so they also come while only such tabs are open.
  function follow() {
    var src = document.querySelector("[data-bn-events]");
    var want = leader && src !== null && state() === "enabled";
    if (want && !own) {
      var es = new root.EventSource(src.getAttribute("data-bn-events"));
      es.addEventListener("notice", function (e) {
        hear(e.data);
      });
      es.onerror = function () {
        // refused (an error status, a proxy while TasmoFleet restarts): the browser gives up on
        // such a stream, so try again later
        if (own === es && es.readyState === 2) {
          own = null;
          root.setTimeout(follow, 30000);
        }
      };
      own = es;
    } else if (!want && own) {
      own.close();
      own = null;
    }
  }

  // The Settings card.
  function render() {
    var s = state();
    document.querySelectorAll("[data-bn]").forEach(function (card) {
      var t = TEXT[s];
      card.querySelector("[data-bn-dot]").className =
        "sys-status-dot t-" + t[0];
      card.querySelector("[data-bn-title]").textContent = t[1];
      card.querySelector("[data-bn-text]").textContent = t[2];
      card.setAttribute("data-bn-state", s);
      card.querySelector("[data-bn-enable]").hidden = s !== "off";
      card.querySelector("[data-bn-test]").hidden = s !== "enabled";
      card.querySelector("[data-bn-disable]").hidden = s !== "enabled";
    });
  }
  // refresh follows a change of the state: the card, and the stream of a page without live data.
  function refresh() {
    render();
    follow();
  }

  document.addEventListener("click", function (e) {
    var b =
      e.target instanceof Element ? e.target.closest("[data-bn] button") : null;
    if (!b) {
      return;
    }
    if (b.hasAttribute("data-bn-enable")) {
      // Asked only here, after the click.
      var done = function (p) {
        if (p === "granted") {
          setOptedIn(true);
        }
        refresh();
      };
      var r = root.Notification.requestPermission(done); // old browsers take a callback, new ones return a promise
      if (r && typeof r.then === "function") {
        r.then(done);
      }
    } else if (b.hasAttribute("data-bn-disable")) {
      setOptedIn(false);
      refresh();
    } else if (b.hasAttribute("data-bn-test")) {
      show("TasmoFleet test notification", {
        body: "If you can read this, browser notifications work.",
        tag: "tasmofleet-test",
      });
    }
  });

  document.addEventListener("htmx:afterSettle", render); // the card came back from a swap
  root.addEventListener("storage", refresh); // another tab turned it on or off
  try {
    root.navigator.permissions
      .query({ name: "notifications" })
      .then(function (p) {
        p.onchange = refresh;
      });
  } catch (e) {
    // no Permissions API: the card reads the state when the page loads
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", refresh);
  } else {
    refresh();
  }
})(typeof window !== "undefined" ? window : this);
