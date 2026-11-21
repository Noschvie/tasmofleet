// Live connection state (design v2): the top bar's indicator and the two banners. The htmx SSE
// extension keeps the stream on <body> open; this file listens to its events and sets
// data-live on <html> ("live", "reconnecting", "offline"); the stylesheet shows the matching
// label and banner, this file fills in the time of the last update and the retry countdown.
// Nothing here changes how updates arrive.
(function () {
  "use strict";
  var root = document.documentElement;
  var sseRetryMs = 4000; // the stream's "retry:" field (events.go sseRetry): the browser's own reconnect
  var OFFLINE_AFTER = 5; // failed attempts in a row before "Reconnecting" becomes "Offline"
  // A drop the browser repairs by itself (a proxy or the server closing the stream, a restart)
  // stays silent: the indicator and banner change only when the stream is still down after
  // GRACE_MS or after SHOW_AFTER failed attempts in a row, whichever comes first.
  var GRACE_MS = 15000;
  var SHOW_AFTER = 3;
  var attempts = 0,
    closedRuns = 0,
    nextAt = 0,
    tick = null,
    grace = null;
  var lastData = new Date();

  function set(state) {
    root.setAttribute("data-live", state);
  }

  function two(n) {
    return (n < 10 ? "0" : "") + n;
  }

  // put writes only a value that changed: an element inside a live region (the banners, #1089) must
  // not be touched every second just because the timer ran.
  function put(el, text) {
    if (el.textContent !== text) {
      el.textContent = text;
    }
  }

  function fill() {
    var left = Math.max(0, Math.ceil((nextAt - Date.now()) / 1000));
    document.querySelectorAll("[data-live-next]").forEach(function (el) {
      put(el, String(left));
    });
    var since = two(lastData.getHours()) + ":" + two(lastData.getMinutes());
    var iso = lastData.toISOString();
    document.querySelectorAll("[data-live-since]").forEach(function (el) {
      put(el, since);
      if (el.getAttribute("datetime") !== iso) {
        el.setAttribute("datetime", iso);
      }
    });
  }

  document.addEventListener("htmx:sseOpen", function () {
    attempts = 0;
    closedRuns = 0;
    lastData = new Date();
    if (tick) {
      window.clearInterval(tick);
      tick = null;
    }
    if (grace) {
      window.clearTimeout(grace);
      grace = null;
    }
    set("live");
  });
  document.addEventListener("htmx:sseBeforeMessage", function () {
    lastData = new Date();
  });
  document.addEventListener("htmx:sseError", function (e) {
    var src = e.detail && e.detail.source;
    attempts++;
    var delay = sseRetryMs;
    // A stream the server refused (an error status) is closed for good; the extension reopens it
    // itself after 0.5 s, 1 s, 2 s ... up to 64 s.
    if (src && src.readyState === 2) {
      closedRuns++;
      delay = Math.min(500 * Math.pow(2, closedRuns - 1), 64000);
    }
    nextAt = Date.now() + delay;
    if (attempts === 1 && !grace) {
      grace = window.setTimeout(show, GRACE_MS);
    }
    var state = root.getAttribute("data-live");
    if (
      attempts >= SHOW_AFTER ||
      state === "reconnecting" ||
      state === "offline"
    ) {
      show();
    }
  });

  // show switches the indicator and banner to the failed state (still down, see GRACE_MS).
  function show() {
    set(attempts >= OFFLINE_AFTER ? "offline" : "reconnecting");
    fill();
    if (!tick) {
      tick = window.setInterval(fill, 1000);
    }
  }

  // Status bar (#307): job, backup, broker and resync events ask the bar to re-read itself. htmx's
  // throttle runs the first request of a burst and drops the rest, so the last state of a burst
  // (the final "done" of a bulk command) would never show. This is leading and trailing: at once
  // for the first event, then at most once per SB_EVERY_MS, and always once after the last event.
  // The bar's sse-swap only registers the stream's listeners; the swap is cancelled here.
  var SB_EVERY_MS = 2000;
  var sbLast = 0,
    sbTimer = null;
  function sbRefresh() {
    sbTimer = null;
    sbLast = Date.now();
    var bar = document.querySelector("footer.sb[hx-get]");
    if (bar) {
      htmx.trigger(bar, "sb-refresh");
    }
  }
  document.addEventListener("htmx:sseBeforeMessage", function (e) {
    var t = e.target;
    if (!(t instanceof Element) || !t.matches("footer.sb")) {
      return;
    }
    e.preventDefault();
    if (sbTimer) {
      return; // the pending refresh runs after this event
    }
    var wait = SB_EVERY_MS - (Date.now() - sbLast);
    if (wait <= 0) {
      sbRefresh();
    } else {
      sbTimer = window.setTimeout(sbRefresh, wait);
    }
  });

  // Live cells of the Devices list (#726). Every health event brings six fragments per device (the
  // phone cards repeat four of them), whether the cell changed or not: with 60 devices that is about 90
  // swaps a second, and each runs the page's swap scripts. A cell whose fragment is the one it was last
  // swapped to is left alone. The memo is dropped when anything else swaps that element (a form post, a
  // reload of the list makes new elements without one), so it never hides a cell that was changed by hand.
  var cellMemo = new WeakMap(),
    cellSent = new WeakMap();
  document.addEventListener("htmx:sseBeforeMessage", function (e) {
    var t = e.target;
    var data = e.detail && e.detail.data;
    if (
      !(t instanceof Element) ||
      typeof data !== "string" ||
      !t.closest("table[data-table], li.dcard")
    ) {
      return;
    }
    if (cellMemo.get(t) === data) {
      e.preventDefault();
      return;
    }
    cellSent.set(t, data);
  });
  document.addEventListener("htmx:afterSwap", function (e) {
    var t = e.target;
    if (!(t instanceof Element)) {
      return;
    }
    if (cellSent.has(t)) {
      cellMemo.set(t, cellSent.get(t));
      cellSent.delete(t);
    } else {
      cellMemo.delete(t);
    }
  });

  // Device overview (#145, #126): the page refreshes #devlive, the header and the Activity list
  // when the device changes, but never the Connection card and the Identity card only when what it
  // shows changed (#1003; both hold answers and notices). Those follow the offline state from here:
  // their "Last reported" label shown (a word, never an opacity: #1071), and Refresh identity blocked with its reason.
  function syncDeviceOffline() {
    var live = document.getElementById("devlive");
    if (!live) {
      return;
    }
    var off = live.hasAttribute("data-off");
    document
      .querySelectorAll("#identity [data-stale], #devconn [data-stale]")
      .forEach(function (el) {
        el.hidden = !off;
      });
    // Refresh identity cannot act while the device is offline, or while its HTTP API is off and nothing else reads it
    // (#659). The button stays focusable (aria-disabled, never disabled) and the reason is the line #ident-why
    // names, as text: the Identity card is not swapped for either, so both follow the live region from here (#1072).
    // The words come from the server: the offline sentence from the line's data-off-why, the API wording (with
    // its non-breaking markup) from the template the live region carries.
    var refused = live.hasAttribute("data-reads-off");
    var why = document.getElementById("ident-why");
    document
      .querySelectorAll("#identity [data-needs-device]")
      .forEach(function (b) {
        // a Refresh identity in flight is disabled by htmx and enabled again by htmx
        if (b.classList.contains("htmx-request")) {
          return;
        }
        // hx-disable as the page renders it: htmx sends nothing for a blocked button whatever the event, and
        // process binds the probe again once the device is back (the card is not swapped for that)
        if (refused || off) {
          b.setAttribute("aria-disabled", "true");
          b.setAttribute("hx-disable", "");
        } else if (b.hasAttribute("aria-disabled")) {
          b.removeAttribute("aria-disabled");
          b.removeAttribute("hx-disable");
          htmx.process(b);
        }
      });
    if (why) {
      var tpl = live.querySelector("template[data-ident-why]");
      if (refused && tpl) {
        why.replaceChildren(tpl.content.cloneNode(true));
      } else if (off) {
        why.textContent = why.getAttribute("data-off-why") || "";
      } else {
        why.replaceChildren();
      }
      why.hidden = !why.firstChild;
    }
  }
  // Only a swap of #devlive, #identity or #devconn, of something inside one of them or of something that holds
  // one changes what syncDeviceOffline reads or writes; a live cell of the device list is none of these (#1059).
  var DEVICE_CARDS = "#devlive, #identity, #devconn";
  document.addEventListener("htmx:afterSettle", function (e) {
    var t = e.target;
    if (
      t instanceof Element &&
      (t.closest(DEVICE_CARDS) || t.querySelector(DEVICE_CARDS))
    ) {
      syncDeviceOffline();
    }
  });

  // The live refresh carries the Identity card when what it shows changed (#1003), judged by the signature the
  // page sent with the request. The answer to a Refresh identity (its notice or error) is only in the card that
  // answer swapped in: a card that shows what the card on the page shows is not swapped again (the refresh was on its
  // way with the old signature), and a card that shows something new (the network facts a probe reads afterwards)
  // takes the answer over, so it stays until the page is loaded again, as before.
  document.addEventListener("htmx:oobBeforeSwap", function (e) {
    var d = e.detail;
    if (!d || !d.target || d.target.id !== "identity" || !d.fragment) {
      return;
    }
    var incoming = d.fragment.querySelector("#identsig");
    var current = d.target.querySelector("#identsig");
    // A Refresh identity (or a name answer) still on its way swaps in its own card, the newest, with its answer: the
    // card it targets must still be on the page then.
    if (
      d.target.querySelector(".htmx-request") ||
      (incoming && current && incoming.value === current.value)
    ) {
      d.shouldSwap = false;
      return;
    }
    var answer = d.target.querySelector("[data-ident-answer]");
    var card = d.fragment.querySelector("#identity");
    if (answer && card && !card.querySelector("[data-ident-answer]")) {
      card.appendChild(answer.cloneNode(true));
    }
  });

  // A refresh that was held back by an open dialog is asked for again when the dialog closes (the
  // close event does not bubble, hence the capture).
  document.addEventListener(
    "close",
    function (e) {
      if (
        e.target instanceof HTMLDialogElement &&
        document.getElementById("devlive")
      ) {
        htmx.trigger("#devlive", "sse:resync");
      }
    },
    true,
  );

  // A refresh never swaps the page under an open dialog (Restart): the next event refreshes it.
  document.addEventListener("htmx:beforeRequest", function (e) {
    var t = e.target;
    var cfg = e.detail && e.detail.requestConfig;
    if (
      t &&
      t.id === "devlive" &&
      cfg &&
      cfg.verb === "get" &&
      document.querySelector("dialog[open]")
    ) {
      e.preventDefault();
    }
  });

  // Retry now: reload the page when the server answers (fresh values and a fresh stream); while
  // it does not, the banner stays and says when the next try is.
  document.addEventListener("click", function (e) {
    var b =
      e.target instanceof Element
        ? e.target.closest("[data-live-retry]")
        : null;
    if (!b) {
      return;
    }
    b.disabled = true;
    fetch(window.location.href, {
      method: "HEAD",
      credentials: "same-origin",
      cache: "no-store",
    })
      .then(function (r) {
        if (r.ok) {
          window.location.reload();
        } else {
          b.disabled = false;
        }
      })
      .catch(function () {
        b.disabled = false;
      });
  });
})();
