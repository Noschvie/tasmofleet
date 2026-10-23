// Opens the changelog dialogs of the firmware page. Without this file the
// "Changelog and fleet" links reload the page with the release shown inline.
(function () {
  "use strict";
  document.addEventListener("click", function (e) {
    var t = e.target instanceof Element ? e.target : null;
    if (!t) { return; }
    var opener = t.closest("[data-dialog]");
    if (opener) {
      var dlg = document.getElementById(opener.getAttribute("data-dialog"));
      if (dlg && typeof dlg.showModal === "function") {
        e.preventDefault();
        dlg.showModal();
      }
      return;
    }
    if (t.closest("[data-close]")) {
      var d = t.closest("dialog");
      if (d) { d.close(); }
      return;
    }
    if (t instanceof HTMLDialogElement && t.open) { t.close(); } // click on the backdrop
  });
})();
