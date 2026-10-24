// Static demo (make demo, published on GitHub Pages): the pages were saved from a running
// tasmofleet with a simulated fleet. Nothing here talks to a server: form submissions, links to
// pages that are not part of the demo and the scripts' POST requests are stopped with a toast.
// The theme switch of the user menu works and is remembered in this browser.
(function () {
  "use strict";
  var KEY = "tasmofleet-demo-theme";
  var root = document.documentElement;

  function note(text) {
    if (window.tfToast) {
      window.tfToast(text, "info");
    }
  }

  function applyTheme(choice) {
    if (choice === "light" || choice === "dark") {
      root.setAttribute("data-theme", choice);
      root.removeAttribute("data-theme-auto");
    } else {
      root.removeAttribute("data-theme");
      root.setAttribute("data-theme-auto", "");
    }
    document.querySelectorAll("[data-theme-choice]").forEach(function (b) {
      b.setAttribute("aria-pressed", b.getAttribute("data-theme-choice") === (choice || "auto") ? "true" : "false");
    });
  }

  try {
    var stored = window.localStorage.getItem(KEY);
    if (stored) {
      applyTheme(stored);
    }
  } catch (e) {
    /* storage blocked: the page keeps its light default */
  }

  document.addEventListener(
    "click",
    function (e) {
      var t = e.target instanceof Element ? e.target : null;
      if (!t) {
        return;
      }
      var choice = t.closest("[data-theme-choice]");
      if (choice) {
        try {
          window.localStorage.setItem(KEY, choice.getAttribute("data-theme-choice"));
        } catch (err) {
          /* not remembered, applies to this page only */
        }
        return;
      }
      if (t.closest("a[data-demo-off]")) {
        e.preventDefault();
        e.stopImmediatePropagation();
        note("This page is not part of the static demo.");
      }
    },
    true,
  );

  document.addEventListener(
    "submit",
    function (e) {
      e.preventDefault();
      e.stopImmediatePropagation();
      note("Static demo: actions are disabled.");
    },
    true,
  );

  // The page scripts save preferences (columns, theme) with POST requests: answer them locally.
  var realFetch = window.fetch;
  if (realFetch) {
    window.fetch = function (input, init) {
      var method = ((init && init.method) || "GET").toUpperCase();
      if (method !== "GET" && method !== "HEAD") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      return realFetch.apply(this, arguments);
    };
  }
})();
