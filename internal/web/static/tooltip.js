// Tooltips: a readable replacement for the browser's title bubble (narrow,
// normal line height, themed contrast). Server-rendered title attributes are
// the source; the first time the pointer or focus reaches an element, its
// title moves into data-tip, so the browser never shows its own bubble next
// to this one (pages without this script keep the native title). Texts are
// capped at 160 characters: tooltips explain, they do not list data.
// Mouse hover (after a short delay) and keyboard focus; never on touch.
// Two rules for table cells (#811). A title that only repeats the element's own text exists for the
// day the text is cut off, so it shows only then. And a bubble for an element inside a cell sits
// beside it, not below it: a cell fills its whole row, so "below" is the next row's text.
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

  function norm(s) {
    return (s || "").replace(/\s+/g, " ").trim();
  }

  // cutOff: the element, or something inside it, hides part of its content (overflow hidden or
  // clipped with an ellipsis).
  function cutOff(el) {
    var all = [el].concat(Array.prototype.slice.call(el.querySelectorAll("*")));
    return all.some(function (x) {
      if (
        x.scrollWidth <= x.clientWidth + 1 &&
        x.scrollHeight <= x.clientHeight + 1
      ) {
        return false;
      }
      var cs = window.getComputedStyle(x);
      return cs.overflowX !== "visible" || cs.overflowY !== "visible";
    });
  }

  // repeats: the tooltip says nothing but what the element already shows.
  function repeats(target, text) {
    var own = norm(target.textContent);
    return own !== "" && own.indexOf(norm(text)) >= 0;
  }

  // reveal: the tooltip is the full text of the element (it starts with what is shown).
  function reveal(target, text) {
    var own = norm(target.textContent);
    return own !== "" && norm(text).indexOf(own) === 0;
  }

  // textBox is the box of the element's text, cut to the element: the end of the text where it fits,
  // the edge of the element where it is cut off.
  function textBox(target) {
    var r = target.getBoundingClientRect(),
      rg = document.createRange();
    rg.selectNodeContents(target);
    var q = rg.getBoundingClientRect();
    if (!q.width || !q.height) {
      return r;
    }
    return {
      left: Math.max(q.left, r.left),
      right: Math.min(q.right, r.right),
      top: Math.max(q.top, r.top),
      bottom: Math.min(q.bottom, r.bottom),
    };
  }

  // placeBeside puts the bubble to the right of the element, centred on its line, or to the left
  // where the page ends. It returns false when neither side has room.
  function placeBeside(target, t) {
    var a = textBox(target),
      tr = t.getBoundingClientRect(),
      vw = document.documentElement.clientWidth,
      left = a.right + 8;
    if (left + tr.width > vw - 8) {
      left = a.left - tr.width - 8;
    }
    if (left < 8) {
      return false;
    }
    var top = (a.top + a.bottom) / 2 - tr.height / 2;
    top = Math.min(
      Math.max(8, top),
      Math.max(8, window.innerHeight - tr.height - 8),
    );
    t.style.left = Math.round(left) + "px";
    t.style.top = Math.round(top) + "px";
    return true;
  }

  function place(target) {
    var t = node(),
      r = target.getBoundingClientRect(),
      vw = document.documentElement.clientWidth;
    var tr = t.getBoundingClientRect();
    if (target.closest("td, th") && placeBeside(target, t)) {
      return;
    }
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
    // a title that repeats the element's own text matters only while that text is cut off
    // (a cell the table script found cut off carries data-autotitle: that was checked already)
    if (
      !target.hasAttribute("data-autotitle") &&
      repeats(target, text) &&
      !cutOff(target)
    ) {
      return;
    }
    hide();
    owner = target;
    owner.setAttribute("aria-describedby", "tf-tooltip");
    var t = node();
    t.textContent = text;
    // the full text of a cut-off value is read in one go: wider than an explanation, so that even
    // the longest (160 characters) is three lines and stays inside its own row
    t.classList.toggle(
      "is-wide",
      !!target.closest("td, th") && reveal(target, text),
    );
    // measured from the left edge: where the previous bubble stood, a fixed box is as wide as the room right of its
    // old left, so a bubble that wrapped there is measured taller than it ends up and is centred too high
    t.style.left = "0px";
    t.style.top = "0px";
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
