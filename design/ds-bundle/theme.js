// Theme switch for the preview pages. "system" is resolved here to
// data-theme="light|dark" (load in <head> without defer, so there is no flash);
// without JS, data-theme-auto on <html> follows prefers-color-scheme.
// Buttons with data-theme-set="light|dark|system" set the theme; "#theme=dark" in the URL does the same (used by
// the screenshot runs). The choice is remembered per browser when storage works.
(function () {
  "use strict";
  var KEY = "tfds-theme";
  var root = document.documentElement;

  var mq = window.matchMedia("(prefers-color-scheme: dark)");
  var current = "system";

  function apply(mode) {
    if (mode !== "light" && mode !== "dark") {
      mode = "system";
    }
    current = mode;
    root.removeAttribute("data-theme-auto");
    root.setAttribute(
      "data-theme",
      mode === "system" ? (mq.matches ? "dark" : "light") : mode,
    );
    var btns = document.querySelectorAll("[data-theme-set]");
    for (var i = 0; i < btns.length; i++) {
      btns[i].setAttribute(
        "aria-pressed",
        String(btns[i].getAttribute("data-theme-set") === mode),
      );
    }
  }

  function stored() {
    try {
      return window.localStorage.getItem(KEY);
    } catch (e) {
      return null;
    }
  }
  function store(mode) {
    try {
      window.localStorage.setItem(KEY, mode);
    } catch (e) {
      /* storage blocked: no memory */
    }
  }

  var hash = /theme=(light|dark|system)/.exec(window.location.hash);
  apply(hash ? hash[1] : stored());

  document.addEventListener("click", function (ev) {
    var btn = ev.target.closest && ev.target.closest("[data-theme-set]");
    if (!btn) {
      return;
    }
    var mode = btn.getAttribute("data-theme-set");
    apply(mode);
    store(mode);
  });
  mq.addEventListener("change", function () {
    if (current === "system") {
      apply("system");
    }
  });
  document.addEventListener("DOMContentLoaded", function () {
    apply(current);
  });
})();
