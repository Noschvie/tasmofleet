// Tab strips (nav.utabs) scroll sideways on a phone. Bring the current tab into
// view on load so the active tab is never the one cut off at the edge. Only the
// strip itself moves (scrollLeft), never the page. No inline script: CSP forbids it.
(function () {
  "use strict";
  function reveal() {
    document.querySelectorAll("nav.utabs").forEach(function (strip) {
      var cur = strip.querySelector('[aria-current="page"]');
      if (!cur || strip.scrollWidth <= strip.clientWidth) return;
      var s = strip.getBoundingClientRect();
      var c = cur.getBoundingClientRect();
      // centre the current tab; the browser clamps at both ends
      strip.scrollLeft += c.left - s.left - (s.width - c.width) / 2;
    });
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", reveal);
  } else {
    reveal();
  }
  // fonts change tab widths; measure again once they are in
  if (document.fonts && document.fonts.ready) document.fonts.ready.then(reveal);
  document.body.addEventListener("htmx:afterSwap", reveal);
})();
