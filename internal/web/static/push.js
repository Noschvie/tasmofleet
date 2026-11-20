// Web Push, this browser's side (#611): the state of this browser on Settings, Notifications, the Web Push
// card, and subscribing. The list of subscribed browsers and its Send test and Remove are the server's
// (they work without this file and without HTTPS); this file only knows what this browser holds: whether
// it can subscribe, whether it did, and whether the server still knows it.
//
// The service worker (sw.js) shows the pushes. Subscribing needs a secure page (HTTPS or localhost) and
// a click: the permission question comes only from the button.
(function (root) {
  "use strict";

  // stateOf: what the card says. env: {secure, serviceWorker, pushManager, ios, standalone, permission,
  // local}; local is null (this browser holds no push subscription) or {known, keyMatches}: whether the
  // server lists it and whether it was made for the server's current key. The order is the one of the
  // approved mockup: a page that cannot subscribe says why before anything else.
  function stateOf(env) {
    if (!env.secure) {
      return "insecure";
    }
    if (!env.serviceWorker || !env.pushManager) {
      return env.ios && !env.standalone ? "ios" : "unsupported";
    }
    if (env.permission === "denied") {
      return "denied";
    }
    if (env.local) {
      return env.local.known && env.local.keyMatches ? "subscribed" : "gone";
    }
    return "off";
  }

  // describe: [tone, title, sentence] of a state.
  function describe(state, origin) {
    switch (state) {
      case "insecure":
        return [
          "neutral",
          "Not available on this connection",
          "Browsers allow push only on a secure page, and this one is " +
            origin +
            ". Open TasmoFleet over HTTPS (through a reverse proxy) or as localhost. The list below still works.",
        ];
      case "ios":
        return [
          "neutral",
          "Add TasmoFleet to the Home Screen first",
          "On iPhone and iPad, Web Push works only for a web app on the Home Screen: tap Share, then Add to Home Screen, then open TasmoFleet from there and subscribe.",
        ];
      case "unsupported":
        return [
          "neutral",
          "Not supported",
          "This browser has no Web Push support. Notifications while a TasmoFleet tab is open (the Browser card) may still work.",
        ];
      case "denied":
        return [
          "danger",
          "Blocked by this browser",
          "Allow notifications for this site in the browser's site settings, then reload this page. The page cannot ask again.",
        ];
      case "gone":
        return [
          "warn",
          "Not subscribed any more",
          "The server no longer knows this browser's subscription: it was removed, its push service dropped it, or the server's key changed. Subscribe again to get pushes.",
        ];
      case "subscribed":
        return [
          "ok",
          "Subscribed in this browser",
          "Pushes arrive also with no TasmoFleet tab open, for the events selected above.",
        ];
    }
    return [
      "neutral",
      "Not subscribed",
      "Subscribe to get notifications also with no TasmoFleet tab open.",
    ];
  }

  // isIOS: iPhone and iPad (an iPad can report itself as a Mac, with a touch screen).
  function isIOS(ua, platform, touchPoints) {
    return (
      /iPhone|iPad|iPod/.test(ua || "") ||
      (platform === "MacIntel" && touchPoints > 1)
    );
  }

  function b64urlToBytes(s) {
    var b = s.replace(/-/g, "+").replace(/_/g, "/");
    while (b.length % 4) {
      b += "=";
    }
    var bin = atob(b);
    var out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) {
      out[i] = bin.charCodeAt(i);
    }
    return out;
  }

  function bytesToB64url(bytes) {
    var bin = "";
    for (var i = 0; i < bytes.length; i++) {
      bin += String.fromCharCode(bytes[i]);
    }
    return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }

  // idOf: the id the server gives a subscription (notify.SubscriptionID): the SHA-256 of the endpoint,
  // base64url. The page never carries the endpoint, so this is how the browser finds its own row.
  function idOf(endpoint, subtle) {
    return subtle
      .digest("SHA-256", new TextEncoder().encode(endpoint))
      .then(function (d) {
        return bytesToB64url(new Uint8Array(d));
      });
  }

  // keyMatches: whether a subscription was made for the server's key (applicationServerKey, base64url).
  // A subscription for another key is dead: the push services refuse the server's signature.
  function keyMatches(subKey, serverKey) {
    if (!subKey || !serverKey) {
      return false;
    }
    var a = new Uint8Array(subKey);
    var b = b64urlToBytes(serverKey);
    if (a.length !== b.length) {
      return false;
    }
    for (var i = 0; i < a.length; i++) {
      if (a[i] !== b[i]) {
        return false;
      }
    }
    return true;
  }

  var api = {
    stateOf: stateOf,
    describe: describe,
    isIOS: isIOS,
    idOf: idOf,
    keyMatches: keyMatches,
    b64urlToBytes: b64urlToBytes,
  };
  if (typeof module === "object" && module.exports) {
    module.exports = api;
  }
  if (typeof document === "undefined") {
    return;
  }

  var thisID = ""; // the id of this browser's subscription, once known (and the server lists it)

  function card() {
    return document.querySelector("[data-push]");
  }
  function csrf() {
    try {
      return (
        JSON.parse(document.body.getAttribute("hx-headers") || "{}")[
          "X-CSRF-Token"
        ] || ""
      );
    } catch (e) {
      return "";
    }
  }
  function swURL() {
    var c = card();
    return c ? c.getAttribute("data-push-sw") : "";
  }
  // scopeURL is the scope of the worker: the base path, where sw.js lives.
  function scopeURL() {
    return new URL("./", new URL(swURL(), root.location.href)).href;
  }
  function subscribeURL() {
    var c = card();
    return c ? c.getAttribute("data-push-subscribe") : "";
  }
  function row(id) {
    return id ? document.querySelector('[data-push-id="' + id + '"]') : null;
  }

  // local: what this browser holds, as stateOf wants it.
  function local() {
    var c = card();
    return navigator.serviceWorker
      .getRegistration(scopeURL())
      .then(function (reg) {
        return reg ? reg.pushManager.getSubscription() : null;
      })
      .then(function (sub) {
        if (!sub) {
          return null;
        }
        return idOf(sub.endpoint, root.crypto.subtle).then(function (id) {
          return {
            id: id,
            known: row(id) !== null,
            keyMatches: keyMatches(
              sub.options && sub.options.applicationServerKey,
              c.getAttribute("data-push-key"),
            ),
          };
        });
      });
  }

  function env() {
    var has = "Notification" in root;
    var secure = root.isSecureContext === true;
    var base = {
      secure: secure,
      serviceWorker: "serviceWorker" in navigator,
      pushManager: "PushManager" in root,
      ios: isIOS(
        navigator.userAgent,
        navigator.platform,
        navigator.maxTouchPoints,
      ),
      standalone:
        navigator.standalone === true ||
        (root.matchMedia &&
          root.matchMedia("(display-mode: standalone)").matches),
      permission: has ? root.Notification.permission : "default",
      local: null,
    };
    // the subscription is only read where it can exist (crypto.subtle is for secure pages too)
    if (!secure || !base.serviceWorker || !base.pushManager) {
      return Promise.resolve(base);
    }
    return local().then(
      function (l) {
        base.local = l;
        return base;
      },
      function () {
        return base; // unreadable: treated as not subscribed; subscribing says what is wrong
      },
    );
  }

  // mark shows "This browser" on the row of this browser's subscription and puts it first.
  function mark() {
    document.querySelectorAll("[data-push-this]").forEach(function (b) {
      b.hidden = true;
    });
    var r = row(thisID);
    if (!r) {
      return;
    }
    r.querySelector("[data-push-this]").hidden = false;
    if (r.parentNode && r.parentNode.firstElementChild !== r) {
      r.parentNode.insertBefore(r, r.parentNode.firstElementChild);
    }
  }

  function draw(state) {
    var c = card();
    if (!c) {
      return;
    }
    var t = describe(state, root.location.origin);
    c.querySelector("[data-push-dot]").className = "sys-status-dot t-" + t[0];
    c.querySelector("[data-push-title]").textContent = t[1];
    c.querySelector("[data-push-text]").textContent = t[2];
    c.setAttribute("data-push-state", state);
    c.querySelector("[data-push-subscribe]").hidden = state !== "off";
    c.querySelector("[data-push-again]").hidden = state !== "gone";
    c.querySelector("[data-push-test]").hidden = state !== "subscribed";
    mark();
  }

  function result(text) {
    var c = card();
    if (c) {
      c.querySelector("[data-push-result]").textContent = text;
    }
  }

  function render() {
    if (!card()) {
      return Promise.resolve();
    }
    return env().then(function (e) {
      thisID = e.local && e.local.known ? e.local.id : "";
      draw(stateOf(e));
    });
  }

  // ready waits for the worker of this page's scope to be active, but not forever: a worker that cannot
  // start must be an error on screen, not a button that never answers.
  function ready() {
    return Promise.race([
      navigator.serviceWorker.ready,
      new Promise(function (_, reject) {
        root.setTimeout(function () {
          reject(new Error("the service worker did not start"));
        }, 15000);
      }),
    ]);
  }

  function failure(res) {
    return res
      .json()
      .then(
        function (j) {
          return j.error || "HTTP " + res.status;
        },
        function () {
          return "HTTP " + res.status;
        },
      )
      .then(function (m) {
        throw new Error(m);
      });
  }

  // subscribe is the click on Subscribe this browser or Subscribe again. The permission is asked first, in
  // the click (browsers refuse it elsewhere); an old subscription (another key, or one the server dropped)
  // is replaced, never kept next to the new one.
  function subscribe(btn) {
    var c = card();
    var key = c.getAttribute("data-push-key");
    btn.disabled = true;
    result("Waiting for the browser's permission question…");
    var asked = root.Notification.requestPermission();
    return Promise.resolve(asked)
      .then(function (perm) {
        if (perm !== "granted") {
          throw new Error(
            perm === "denied"
              ? "the browser blocked notifications for this site"
              : "the permission was not given",
          );
        }
        return navigator.serviceWorker.register(swURL());
      })
      .then(function (reg) {
        return ready().then(function () {
          return reg.pushManager.getSubscription();
        });
      })
      .then(function (old) {
        return old ? old.unsubscribe() : true;
      })
      .then(function () {
        return navigator.serviceWorker.getRegistration(scopeURL());
      })
      .then(function (reg) {
        return reg.pushManager.subscribe({
          userVisibleOnly: true,
          applicationServerKey: b64urlToBytes(key),
        });
      })
      .then(function (sub) {
        return fetch(subscribeURL(), {
          method: "POST",
          credentials: "same-origin",
          headers: {
            "Content-Type": "application/json",
            "X-CSRF-Token": csrf(),
          },
          body: JSON.stringify(sub.toJSON()),
        }).then(function (res) {
          if (res.ok) {
            return;
          }
          // the server refused it: do not keep a subscription it does not know
          return sub.unsubscribe().then(
            function () {
              return failure(res);
            },
            function () {
              return failure(res);
            },
          );
        });
      })
      .then(function () {
        root.location.reload(); // the list is the server's: it shows this browser as soon as the page is read again
      })
      .catch(function (err) {
        btn.disabled = false;
        result(
          "Subscribing failed: " + (err && err.message ? err.message : err),
        );
        return render();
      });
  }

  document.addEventListener("click", function (e) {
    var b =
      e.target instanceof Element
        ? e.target.closest("[data-push] button")
        : null;
    if (!b) {
      return;
    }
    if (
      b.hasAttribute("data-push-subscribe") ||
      b.hasAttribute("data-push-again")
    ) {
      subscribe(b);
    } else if (b.hasAttribute("data-push-test")) {
      // the test goes through the server, like the one on the row: this browser's own row sends it
      var own = row(thisID);
      var send = own && own.querySelector("[data-push-row-test]");
      if (send) {
        result("The result is shown on this browser's row below.");
        send.click();
      } else {
        result("This browser is not in the list below; reload the page.");
      }
    }
  });

  // Removing this browser's own row also drops the subscription the browser holds: the dialog's form
  // posts to the server, and this runs first. Any other row is the server's business only.
  document.addEventListener(
    "submit",
    function (e) {
      var f = e.target;
      if (!(f instanceof HTMLFormElement) || !thisID) {
        return;
      }
      var m = /\/push\/remove\/([A-Za-z0-9_-]+)$/.exec(
        f.getAttribute("action") || "",
      );
      if (!m || m[1] !== thisID) {
        return;
      }
      e.preventDefault();
      var done = function () {
        f.submit();
      };
      navigator.serviceWorker
        .getRegistration(scopeURL())
        .then(function (reg) {
          return reg ? reg.pushManager.getSubscription() : null;
        })
        .then(function (sub) {
          return sub ? sub.unsubscribe() : true;
        })
        .then(done, done);
    },
    true,
  );

  // a row came back from a swap: only a swap that is or holds a row needs mark, which walks the document
  // (#1059; the live cells of the device list swap about 90 times a second)
  document.addEventListener("htmx:afterSettle", function (e) {
    var t = e.target;
    if (t instanceof Element && (t.matches("[data-push-this]") || t.querySelector("[data-push-this]"))) {
      mark();
    }
  });
  try {
    root.navigator.permissions
      .query({ name: "notifications" })
      .then(function (p) {
        p.onchange = render;
      });
  } catch (e) {
    // no Permissions API: the card reads the state when the page loads
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", render);
  } else {
    render();
  }
})(typeof window !== "undefined" ? window : this);
