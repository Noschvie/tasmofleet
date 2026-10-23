// Transaction page helpers: the open log keeps its scroll position when the page refreshes itself
// (and follows new lines for a reader who is at its end), and "Copy log" copies its lines. Without
// this file the page still works; the log just starts at the top after a refresh.
(function () {
  "use strict";
  var saved = null;

  document.addEventListener("htmx:beforeSwap", function (e) {
    var target = e.detail && e.detail.target;
    var log =
      target && target.querySelector && target.querySelector("#log-lines");
    saved = log
      ? {
          atBottom: log.scrollHeight - log.scrollTop - log.clientHeight < 8,
          top: log.scrollTop,
        }
      : null;
  });

  document.addEventListener("htmx:afterSettle", function (e) {
    var target = e.detail && e.detail.target;
    var log =
      target && target.querySelector && target.querySelector("#log-lines");
    if (!log || !saved) {
      return;
    }
    log.scrollTop = saved.atBottom ? log.scrollHeight : saved.top;
    saved = null;
  });

  document.addEventListener("click", function (e) {
    var el = e.target instanceof Element ? e.target : null;
    var copy = el && el.closest("[data-copy]");
    if (!copy) {
      return;
    }
    var box = document.querySelector(copy.getAttribute("data-copy"));
    if (!box || !navigator.clipboard) {
      return;
    }
    // one element per log line, its parts (time, text) apart: the copy keeps both and the line breaks
    var text = Array.prototype.map
      .call(box.querySelectorAll(".upd-log-line"), function (line) {
        return Array.prototype.map
          .call(line.children, function (c) {
            return c.textContent;
          })
          .join("  ");
      })
      .join("\n");
    navigator.clipboard.writeText(text).then(function () {
      copy.textContent = "Copied";
      setTimeout(function () {
        copy.textContent = "Copy log";
      }, 1500);
    });
  });
})();
