// Menus are <details> elements (the user menu, the row menus of ui.RowMenu), so
// they open with click, Enter or Space and work without this file. It only adds
// what <details> lacks: one menu open at a time, close on Escape and on a click
// elsewhere. Static menus (data-static, the style guide) are left alone.
(function () {
  "use strict";
  var MENUS = "details.navgroup, details.umenu";
  function groups() {
    return document.querySelectorAll(MENUS);
  }
  function closeAll(except) {
    groups().forEach(function (d) {
      if (d !== except && !d.hasAttribute("data-static")) {
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
        d.matches(MENUS) &&
        d.open &&
        !d.hasAttribute("data-static")
      ) {
        closeAll(d);
      }
    },
    true,
  );
  document.addEventListener("click", function (e) {
    var t = e.target instanceof Element ? e.target : null;
    var inside = t ? t.closest(MENUS) : null;
    if (!inside) {
      closeAll(null);
      return;
    }
    // an entry that acts in place (Back up now posts with htmx) closes its menu, as one that
    // navigates or opens a dialog does
    if (
      t.closest(".menu-item") &&
      inside.matches("details.umenu") &&
      !inside.hasAttribute("data-static")
    ) {
      inside.open = false;
    }
  });
  document.addEventListener("keydown", function (e) {
    if (e.key !== "Escape") {
      return;
    }
    var open = document.querySelector(
      "details.navgroup[open]:not([data-static]), details.umenu[open]:not([data-static])",
    );
    if (open) {
      closeAll(null);
      var s = open.querySelector("summary");
      if (s) {
        s.focus();
      }
    }
  });
})();

// Row action menus: the server marks the last two rows "up" as the fallback
// without JavaScript. With JavaScript the direction follows the viewport at the
// moment of opening, so a short table does not push the menu over the top bar.
(function () {
  "use strict";
  document.addEventListener(
    "toggle",
    function (e) {
      var d = e.target;
      if (
        !(d instanceof HTMLDetailsElement) ||
        !d.open ||
        !d.classList.contains("rowmenu") ||
        d.hasAttribute("data-static") ||
        d.classList.contains("bulk-more")
      ) {
        return;
      }
      var s = d.querySelector("summary"),
        b = d.querySelector(".rowmenu-body");
      if (!s || !b) {
        return;
      }
      d.classList.remove("up");
      var r = s.getBoundingClientRect(),
        need = b.getBoundingClientRect().height + 8;
      var below = window.innerHeight - r.bottom,
        above = r.top;
      if (below < need && above > below) {
        d.classList.add("up");
      }
    },
    true,
  );
})();
