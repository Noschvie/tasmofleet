// Phone navigation as a <select>: tabs and section lists are plain links that
// work without this file (on a phone they wrap into chips). With the script
// the page also carries a <select data-jump> of the same links; this file
// shows it and goes to the chosen address. CSS then swaps links for select on
// phones only. No inline script: CSP forbids that.
(function () {
  "use strict";
  function show() {
    document.querySelectorAll(".jump[hidden]").forEach(function (j) { j.hidden = false; });
  }
  document.addEventListener("change", function (e) {
    var t = e.target;
    if (t instanceof HTMLSelectElement && t.hasAttribute("data-jump") && t.value) {
      window.location.assign(t.value);
    }
  });
  document.addEventListener("DOMContentLoaded", show);
  window.addEventListener("pageshow", function (e) {
    // back/forward restores the old selection; put it on the current page again
    document.querySelectorAll("select[data-jump]").forEach(function (s) {
      for (var i = 0; i < s.options.length; i++) { s.options[i].selected = s.options[i].defaultSelected; }
    });
    if (e.persisted) { show(); }
  });
})();
