// Strips that scroll sideways on a phone: the tab rows (nav.utabs) and the filter chips (.dv-chips).
// Three things, all on the strip itself and never on the page (no inline script: CSP forbids it):
//  1. Bring the current tab or chip into view on load, so the one that is selected is never cut off
//     at the edge.
//  2. Centre the tab or chip that has keyboard focus in the strip: the browser's own focus scroll leaves
//     a control that shows a sliver at the edge where it is, its centre then lies under what follows the strip
//     (WCAG 2.4.11, the Filters button on the Devices list), and the fade of #1079 cuts its end.
//  3. Say which side has more: data-more-start / data-more-end on a strip that overflows and is not
//     at that end. app.css fades that edge (#1079), so a tab that is out of sight can be found. A strip
//     that does not scroll sideways (the Devices list's chips wrap from 45rem) gets neither attribute.
//  3. Keep a chip or tab that takes the keyboard focus whole inside the strip and clear of the fade
//     (WCAG 2.4.11, #1274): the browser scrolls a focused element into view only when none of it shows.
// Without JavaScript there is no fade; the strip still scrolls.
(function () {
  "use strict";
  var STRIPS = "nav.utabs, .dv-chips";
  // the selected one: aria-current="page" on a tab or a link chip, "true" on a button chip
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
  // Tab onto a chip that is half in the strip: the browser leaves the strip where it is, so the person sees a few
  // pixels of it inside the fade and the control next to the strip lies over its centre. scrollIntoView does the
  // scroll; app.css gives the strip scroll-padding-inline of the fade's width, so the chip lands clear of the
  // fade. :focus-visible only: a mouse press focuses a link too, and moving the strip under the pointer then could
  // lose the click. block "nearest" leaves the vertical position to the browser (the root's scroll-padding-block
  // keeps it clear of the bars).
  document.addEventListener("focusin", function (e) {
    var t = e.target;
    if (!(t instanceof Element) || !t.matches(":focus-visible")) return;
    var strip = t.closest(STRIPS);
    if (strip && scrolls(strip))
      t.scrollIntoView({ block: "nearest", inline: "nearest" });
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
  // Only keyboard focus (:focus-visible) acts, and only the strip's own scroll position changes: no state, no timer.
  document.addEventListener(
    "focusin",
    function (e) {
      var el = e.target;
      var strip = el instanceof Element && el.closest(STRIPS);
      if (!strip || !el.matches(":focus-visible") || !scrolls(strip)) return;
      var s = strip.getBoundingClientRect();
      var c = el.getBoundingClientRect();
      // Centre it like the current one: the browser clamps at both ends, so the control ends clear of the fade
      // that app.css puts on an end with more (no copy of its width here).
      strip.scrollLeft += c.left - s.left - (s.width - c.width) / 2;
    },
    true,
  );
})();
