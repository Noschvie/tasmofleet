// Live connection state (design v2): the top bar's indicator and the two banners. The htmx SSE
// extension keeps the stream on <body> open; this file listens to its events and sets
// data-live on <html> ("live", "reconnecting", "offline"); the stylesheet shows the matching
// label and banner, this file fills in the numbers. Nothing here changes how updates arrive.
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

  function fill() {
    var left = Math.max(0, Math.ceil((nextAt - Date.now()) / 1000));
    document.querySelectorAll("[data-live-next]").forEach(function (el) {
      el.textContent = String(left);
    });
    document.querySelectorAll("[data-live-attempt]").forEach(function (el) {
      el.textContent = String(attempts);
    });
    document.querySelectorAll("[data-live-since]").forEach(function (el) {
      el.textContent =
        two(lastData.getHours()) + ":" + two(lastData.getMinutes());
      el.setAttribute("datetime", lastData.toISOString());
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
    if (attempts >= SHOW_AFTER || state === "reconnecting" || state === "offline") {
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
