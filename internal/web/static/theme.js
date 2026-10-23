// Theme choice (D11): the Light / Dark / Auto buttons of the user menu and the drawer apply the
// choice at once and store it per user (POST <base>/prefs/theme, the body a JSON string). Auto
// removes the choice: the page follows the system again (data-theme-auto on <html>). The server
// renders the stored choice, so there is no flash on the next page.
(function () {
  "use strict";
  var root = document.documentElement;
  var dark = window.matchMedia
    ? window.matchMedia("(prefers-color-scheme: dark)")
    : null;

  function current() {
    return root.getAttribute("data-theme") || "auto";
  }

  function paint() {
    var c = current();
    document.querySelectorAll("[data-theme-choice]").forEach(function (b) {
      b.setAttribute(
        "aria-pressed",
        b.getAttribute("data-theme-choice") === c ? "true" : "false",
      );
    });
    document.querySelectorAll("[data-theme-note]").forEach(function (n) {
      n.hidden = c !== "auto";
      n.textContent =
        "Auto follows your system (now " +
        (dark && dark.matches ? "dark" : "light") +
        ")";
    });
  }

  function apply(choice) {
    if (choice === "light" || choice === "dark") {
      root.setAttribute("data-theme", choice);
      root.removeAttribute("data-theme-auto");
    } else {
      root.removeAttribute("data-theme");
      root.setAttribute("data-theme-auto", "");
    }
    paint();
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

  function save(choice) {
    var url = document.body.getAttribute("data-prefs");
    if (!url) {
      return;
    }
    fetch(url + "/theme", {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf() },
      body: JSON.stringify(choice),
    })
      .then(function (r) {
        if (!r.ok && window.tfToast) {
          window.tfToast(
            "The theme was not saved: it applies to this page only.",
            "warn",
          );
        }
      })
      .catch(function () {
        if (window.tfToast) {
          window.tfToast(
            "The theme was not saved: it applies to this page only.",
            "warn",
          );
        }
      });
  }

  document.addEventListener("click", function (e) {
    var b =
      e.target instanceof Element
        ? e.target.closest("[data-theme-choice]")
        : null;
    if (!b) {
      return;
    }
    var choice = b.getAttribute("data-theme-choice");
    if (choice === current()) {
      return;
    }
    apply(choice);
    save(choice);
  });
  if (dark && dark.addEventListener) {
    dark.addEventListener("change", paint);
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", paint);
  } else {
    paint();
  }
})();
