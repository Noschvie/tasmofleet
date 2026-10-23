// Tooltips: a readable replacement for the browser's title bubble (narrow,
// normal line height, themed contrast). Server-rendered title attributes are
// the source; the first time the pointer or focus reaches an element, its
// title moves into data-tip, so the browser never shows its own bubble next
// to this one (pages without this script keep the native title). Texts are
// capped at 160 characters: tooltips explain, they do not list data.
// Mouse hover (after a short delay) and keyboard focus; never on touch.
(function () {
  "use strict";

  var DELAY = 350;
  var tip = null,
    owner = null,
    timer = 0;

  function node() {
    if (!tip) {
      tip = document.createElement("div");
      tip.className = "tooltip";
      tip.id = "tf-tooltip";
      tip.setAttribute("role", "tooltip");
      tip.hidden = true;
      document.body.appendChild(tip);
    }
    return tip;
  }

  var MAX = 160;

  function hide() {
    window.clearTimeout(timer);
    if (owner) {
      owner.removeAttribute("aria-describedby");
      owner = null;
    }
    if (tip) {
      tip.hidden = true;
    }
  }

  // adopt moves a native title into data-tip (once), so only one bubble shows.
  function adopt(el) {
    var t = el.getAttribute("title");
    if (t !== null) {
      if (!el.hasAttribute("data-tip") && t.trim()) {
        el.setAttribute("data-tip", t);
      }
      el.removeAttribute("title");
    }
  }

  function place(target) {
    var t = node(),
      r = target.getBoundingClientRect(),
      vw = document.documentElement.clientWidth;
    var tr = t.getBoundingClientRect();
    var left = Math.min(Math.max(8, r.left), Math.max(8, vw - tr.width - 8));
    var top = r.bottom + 6;
    if (top + tr.height > window.innerHeight - 8 && r.top - tr.height - 6 > 8) {
      top = r.top - tr.height - 6;
    }
    t.style.left = Math.round(left) + "px";
    t.style.top = Math.round(top) + "px";
  }

  // An open menu (details) and its trigger are never covered by a bubble.
  function overlayOpen(target) {
    return (
      !!(
        target.closest("details[open]:not([data-static])") &&
        target.matches("summary")
      ) ||
      !!document.querySelector(
        "details.rowmenu[open]:not([data-static]), details.colmenu[open]:not([data-static]), details.navgroup[open]:not([data-static])",
      )
    );
  }

  function show(target) {
    adopt(target);
    if (overlayOpen(target)) {
      return;
    }
    var text = target.getAttribute("data-tip");
    if (!text || !text.trim()) {
      return;
    }
    if (text.length > MAX) {
      text = text.slice(0, MAX - 1) + "\u2026";
    }
    hide();
    owner = target;
    owner.setAttribute("aria-describedby", "tf-tooltip");
    var t = node();
    t.textContent = text;
    t.hidden = false;
    place(target);
  }

  function titled(e) {
    return e.target instanceof Element
      ? e.target.closest("[title], [data-tip]")
      : null;
  }

  document.addEventListener("pointerover", function (e) {
    if (e.pointerType !== "mouse") {
      return;
    }
    var t = titled(e);
    if (!t || t === owner) {
      return;
    }
    adopt(t);
    window.clearTimeout(timer);
    timer = window.setTimeout(function () {
      if (t.isConnected) {
        show(t);
      }
    }, DELAY);
  });
  document.addEventListener("pointerout", function (e) {
    var t = titled(e);
    if (!t) {
      return;
    }
    var to = e.relatedTarget instanceof Element ? e.relatedTarget : null;
    if (to && t.contains(to)) {
      return;
    }
    hide();
  });
  // Keyboard focus only: a click on a summary or button focuses it too, and the bubble must not
  // open over the menu that click just opened.
  document.addEventListener("focusin", function (e) {
    var t = titled(e);
    if (t && t.matches(":focus-visible")) {
      show(t);
    }
  });
  document.addEventListener("focusout", hide);
  document.addEventListener("pointerdown", hide);
  document.addEventListener("scroll", hide, true);
  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape") {
      hide();
    }
  });
  document.addEventListener("htmx:beforeSwap", hide);
  document.addEventListener("toggle", hide, true); // a menu that opens takes the bubble down
})();
