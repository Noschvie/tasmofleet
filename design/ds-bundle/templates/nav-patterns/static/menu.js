// Navigation menus are <details> elements, so they open with click, Enter or
// Space and work without this file. It only adds what <details> lacks: one
// menu open at a time, close on Escape and on a click elsewhere.
(function () {
  "use strict";
  // PROPOSED: + details.navpanel (the phone menu is a navgroup too)
  function groups() {
    return document.querySelectorAll(
      "nav.mainnav details, .user details, details.navpanel",
    );
  }
  function closeAll(except) {
    groups().forEach(function (d) {
      if (d !== except) {
        d.open = false;
      }
    });
  }
  document.addEventListener(
    "toggle",
    function (e) {
      var d = e.target;
      if (
        d instanceof HTMLDetailsElement &&
        d.classList.contains("navgroup") &&
        d.open
      ) {
        closeAll(d);
      }
    },
    true,
  );
  document.addEventListener("click", function (e) {
    var t = e.target instanceof Element ? e.target : null;
    if (!t || !t.closest("details.navgroup")) {
      closeAll(null);
    }
  });
  document.addEventListener("keydown", function (e) {
    if (e.key !== "Escape") {
      return;
    }
    var open = document.querySelector("details.navgroup[open]");
    if (open) {
      closeAll(null);
      var s = open.querySelector("summary");
      if (s) {
        s.focus();
      }
    }
  });
})();
