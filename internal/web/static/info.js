// Info popovers (ui.Info): the button opens a native popover with the longer help text. The
// browser handles click, Escape, light dismiss and "one open at a time"; this file places the
// popover under its button, keeps aria-expanded true, and closes it on scroll or resize (it is
// fixed, so it would drift). Without a native popover (older browsers) it opens and closes a
// class instead. Without this file the popover still opens, just not next to its button.
(function () {
  "use strict";

  var native =
    typeof HTMLElement !== "undefined" &&
    HTMLElement.prototype.hasOwnProperty("popover");

  function pops() {
    return Array.prototype.slice.call(document.querySelectorAll(".uinfo-pop"));
  }
  function buttonFor(p) {
    return document.querySelector('[popovertarget="' + p.id + '"]');
  }
  function isOpen(p) {
    return native ? p.matches(":popover-open") : p.classList.contains("open");
  }
  function place(p, b) {
    var r = b.getBoundingClientRect();
    var w = p.offsetWidth;
    var h = p.offsetHeight;
    var left = Math.max(8, Math.min(r.left - 10, window.innerWidth - w - 8));
    var top = r.bottom + 6;
    if (top + h > window.innerHeight - 8) {
      top = Math.max(8, r.top - h - 6);
    }
    p.style.left = left + "px";
    p.style.top = top + "px";
  }
  function close(p) {
    if (!isOpen(p)) {
      return;
    }
    if (native) {
      p.hidePopover();
    } else {
      p.classList.remove("open");
      buttonFor(p).setAttribute("aria-expanded", "false");
    }
  }
  function closeAll() {
    pops().forEach(close);
  }

  if (native) {
    document.addEventListener(
      "toggle",
      function (e) {
        var p = e.target;
        if (!(p instanceof Element) || !p.classList.contains("uinfo-pop")) {
          return;
        }
        var b = buttonFor(p);
        var open = e.newState === "open";
        if (b) {
          b.setAttribute("aria-expanded", open ? "true" : "false");
          if (open) {
            place(p, b);
          }
        }
      },
      true,
    );
  } else {
    document.addEventListener("click", function (e) {
      var t = e.target instanceof Element ? e.target : null;
      if (!t || t.closest(".uinfo-pop")) {
        return;
      }
      var b = t.closest(".uinfo[popovertarget]");
      if (!b) {
        closeAll();
        return;
      }
      var p = document.getElementById(b.getAttribute("popovertarget"));
      var was = isOpen(p);
      closeAll();
      if (!was) {
        p.classList.add("open");
        b.setAttribute("aria-expanded", "true");
        place(p, b);
      }
    });
    document.addEventListener("keydown", function (e) {
      if (e.key !== "Escape") {
        return;
      }
      var open = pops().filter(isOpen)[0];
      if (open) {
        close(open);
        buttonFor(open).focus();
      }
    });
  }
  window.addEventListener("scroll", closeAll, { capture: true, passive: true });
  window.addEventListener("resize", closeAll);
})();
