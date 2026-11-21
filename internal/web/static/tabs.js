// Strips that scroll sideways on a phone: the tab rows (nav.utabs) and the filter chips (.dv-chips).
// Two things, both on the strip itself and never on the page (no inline script: CSP forbids it):
//  1. Bring the current tab or chip into view on load, so the one that is selected is never cut off
//     at the edge.
//  2. Say which side has more: data-more-start / data-more-end on a strip that overflows and is not
//     at that end. app.css fades that edge (#1079), so a tab that is out of sight can be found. A strip
//     that does not scroll sideways (the Devices list's chips wrap) gets neither attribute.
// Without JavaScript there is no fade; the strip still scrolls.
(function () {
  "use strict";
  var STRIPS = "nav.utabs, .dv-chips";
  // the selected one: aria-current="page" on a tab, aria-current (true, or empty) on a chip
  var CURRENT = '[aria-current]:not([aria-current="false"])';

  function scrolls(strip) {
    var x = getComputedStyle(strip).overflowX;
    return (
      (x === "auto" || x === "scroll") &&
      strip.scrollWidth > strip.clientWidth + 1
    );
  }

  function strips(root) {
    var out = [];
    if (root.matches && root.matches(STRIPS)) out.push(root);
    if (root.querySelectorAll)
      out.push.apply(out, root.querySelectorAll(STRIPS));
    return out;
  }

  function flag(strip, name, on) {
    if (on) strip.setAttribute(name, "");
    else strip.removeAttribute(name);
  }

  // mark sets the two attributes from where the strip is scrolled to. 1 px of slack: scrollLeft is
  // fractional on a high-density screen and the end of the strip is then never reached exactly.
  function mark(strip) {
    var more = scrolls(strip);
    var max = strip.scrollWidth - strip.clientWidth;
    flag(strip, "data-more-start", more && strip.scrollLeft > 1);
    flag(strip, "data-more-end", more && strip.scrollLeft < max - 1);
  }

  function reveal(root) {
    strips(root).forEach(function (strip) {
      var cur = strip.querySelector(CURRENT);
      if (cur && scrolls(strip)) {
        var s = strip.getBoundingClientRect();
        var c = cur.getBoundingClientRect();
        // centre the current one; the browser clamps at both ends
        strip.scrollLeft += c.left - s.left - (s.width - c.width) / 2;
      }
      mark(strip);
    });
  }

  function all() {
    reveal(document);
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", all);
  } else {
    all();
  }
  // fonts change tab widths; measure again once they are in
  if (document.fonts && document.fonts.ready) document.fonts.ready.then(all);

  // a swap brings new strips (every swap, out of band ones included, fires htmx:load on what it
  // inserts); only a swap that is or holds a strip is worth the walk (#1059)
  document.body.addEventListener("htmx:load", function (e) {
    var t = e.target;
    if (
      t instanceof Element &&
      (t.matches(STRIPS) || t.querySelector(STRIPS))
    ) {
      reveal(t);
    }
  });

  var queued = false;
  function later(fn) {
    if (queued) return;
    queued = true;
    requestAnimationFrame(function () {
      queued = false;
      fn();
    });
  }
  // the width of the window, and with it what a strip hides, changes (rotating a phone)
  window.addEventListener("resize", function () {
    later(function () {
      strips(document).forEach(mark);
    });
  });
  // scroll does not bubble; capturing it on the document covers a strip that arrives later too
  document.addEventListener(
    "scroll",
    function (e) {
      var t = e.target;
      if (t instanceof Element && t.matches(STRIPS))
        later(function () {
          mark(t);
        });
    },
    true,
  );
})();
