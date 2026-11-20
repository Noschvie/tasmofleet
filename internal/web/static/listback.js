// Back to the device list as it was left (#127). On the list, remember its path and query
// (filter, search, sort) in sessionStorage, which is per browser tab; on a device page, point
// the crumb marked data-keep-query ("Devices") at that remembered list. The navigation's own
// "Devices" link is not marked and still opens the unfiltered list; without JavaScript, or in
// a tab that never saw the list, the crumb stays the plain list link. No inline script: CSP
// forbids it.
(function () {
  "use strict";
  var KEY = "tasmofleet.devicelist";

  function marked() {
    return document.querySelectorAll("nav.crumbs a[data-keep-query]");
  }
  function remember() {
    if (!document.querySelector('[data-table="devices"]')) return; // only the list itself
    try {
      sessionStorage.setItem(KEY, location.pathname + location.search);
    } catch (e) {
      /* storage blocked: the crumb stays plain */
    }
  }
  function restore() {
    var saved = "";
    try {
      saved = sessionStorage.getItem(KEY) || "";
    } catch (e) {
      return;
    }
    var q = saved.indexOf("?");
    if (q < 0) return;
    marked().forEach(function (a) {
      // only a query saved for this very list (same base path) is applied
      if (saved.slice(0, q) === a.pathname) a.search = saved.slice(q);
    });
  }
  function run() {
    remember();
    restore();
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", run);
  } else {
    run();
  }
  // The list's table and the crumb are all that run reads; a live cell of the list swaps about 90 times a
  // second and needs neither (#1059). A swap of the body (history restore) holds both.
  document.body.addEventListener("htmx:afterSettle", function (e) {
    var t = e.target;
    var sel = '[data-table="devices"], nav.crumbs a[data-keep-query]';
    if (t instanceof Element && (t.matches(sel) || t.querySelector(sel))) {
      run();
    }
  });
  window.addEventListener("pageshow", run); // back/forward cache
})();
