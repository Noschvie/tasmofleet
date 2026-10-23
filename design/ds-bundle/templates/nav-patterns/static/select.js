// Device picker: "select all" header checkbox (with the indeterminate state)
// and the quick selectors. Without this file the same buttons are plain
// submit buttons that reload the page with the selection made on the server,
// so nothing depends on it. No inline script: CSP forbids that.
(function () {
  "use strict";

  function picker(el) {
    return el.closest("form") || document.getElementById(el.getAttribute("data-picker") || "");
  }
  function boxes(form) {
    return Array.prototype.slice.call(form.querySelectorAll('input[type="checkbox"][name="id"]:not(:disabled)'));
  }
  function sync(form) {
    var list = boxes(form);
    var n = list.filter(function (b) { return b.checked; }).length;
    var all = form.querySelector("[data-select-all]");
    if (all) {
      all.checked = list.length > 0 && n === list.length;
      all.indeterminate = n > 0 && n < list.length;
    }
    document.querySelectorAll("[data-select-count]").forEach(function (c) {
      c.textContent = n + " of " + list.length + " selected";
    });
    form.querySelectorAll("[data-bar-count]").forEach(function (c) { c.textContent = String(n); });
    // live summary of an update selection
    form.querySelectorAll("[data-summary]").forEach(function (el) {
      var rows = list.filter(function (b) { return b.checked; }).map(function (b) { return b.closest("tr"); });
      if (rows.length === 0) { el.textContent = "Select devices to see what will happen."; return; }
      var fam = {}, outdated = 0;
      rows.forEach(function (r) {
        var f = r ? r.getAttribute("data-family") || "unknown" : "unknown";
        fam[f] = (fam[f] || 0) + 1;
        if (r && r.getAttribute("data-update") === "1") { outdated++; }
      });
      var parts = Object.keys(fam).sort().map(function (k) { return fam[k] + " " + k; });
      el.textContent = rows.length + " selected: " + parts.join(", ") + "; " + outdated + " older than the release.";
    });
    // buttons that act on the selection stay off until something is selected
    form.querySelectorAll("[data-needs-selection]").forEach(function (b) {
      b.disabled = n === 0;
      b.title = n === 0 ? "Select devices first" : "";
    });
    // the bulk action bar shows as soon as something is selected
    form.querySelectorAll("[data-bulkbar]").forEach(function (b) { b.hidden = n === 0; });
  }
  function syncAll() {
    document.querySelectorAll("[data-select-all]").forEach(function (a) {
      a.hidden = false; // only useful with this script
      var f = picker(a);
      if (f) { sync(f); }
    });
  }
  // wants says whether the quick selector picks the row.
  function wants(mode, row) {
    if (mode === "all") { return true; }
    if (mode === "none") { return false; }
    if (mode === "unused") { return !!row && row.getAttribute("data-unused") === "1"; }
    if (mode === "update") { return !!row && row.getAttribute("data-update") === "1"; }
    if (mode.indexOf("family:") === 0) { return !!row && row.getAttribute("data-family") === mode.slice(7); }
    return false;
  }

  document.addEventListener("change", function (e) {
    var t = e.target;
    if (!(t instanceof HTMLInputElement)) { return; }
    var form = picker(t);
    if (!form) { return; }
    if (t.hasAttribute("data-select-all")) {
      boxes(form).forEach(function (b) { b.checked = t.checked; });
    }
    if (t.hasAttribute("data-select-all") || t.name === "id") { sync(form); }
  });

  document.addEventListener("click", function (e) {
    var btn = e.target instanceof Element ? e.target.closest("[data-pick]") : null;
    if (!btn) { return; }
    var form = picker(btn);
    if (!form) { return; }
    e.preventDefault(); // handled here; without the script the submit button does it on the server
    var mode = btn.getAttribute("data-pick");
    boxes(form).forEach(function (b) { b.checked = wants(mode, b.closest("tr")); });
    sync(form);
  });

  document.addEventListener("DOMContentLoaded", syncAll);
  // the browser restores checkbox states after back/forward, without a change event
  window.addEventListener("pageshow", syncAll);
  document.addEventListener("htmx:afterSettle", syncAll);
})();
