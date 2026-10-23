// Opens the dialogs of links and buttons with data-dialog (changelogs, delete
// confirmations). Without this file such a link goes to its page: the release
// shown inline, or the confirmation page of the action.
(function () {
  "use strict";
  // A dialog shared by many rows (the device list) is filled from the data-dlg-*
  // attributes of the link that opens it: title, text, the form's action and the
  // confirming button's verb; data-dlg-danger makes that button red; data-dlg-dismiss
  // names the dismissing button when "Cancel" would be ambiguous ("Keep running").
  function fill(dlg, opener) {
    if (!opener.hasAttribute("data-dlg-title")) {
      return;
    }
    var set = function (name, fn) {
      dlg.querySelectorAll('[data-fill="' + name + '"]').forEach(fn);
    };
    set("title", function (el) {
      el.textContent = opener.getAttribute("data-dlg-title");
    });
    set("text", function (el) {
      el.textContent = opener.getAttribute("data-dlg-text") || "";
    });
    set("form", function (el) {
      el.setAttribute("action", opener.getAttribute("data-dlg-action") || "");
    });
    set("dismiss", function (el) {
      el.textContent = opener.getAttribute("data-dlg-dismiss") || "Cancel";
    });
    set("verb", function (el) {
      el.textContent = opener.getAttribute("data-dlg-verb") || "";
      el.classList.toggle("danger", opener.hasAttribute("data-dlg-danger"));
    });
  }
  document.addEventListener("click", function (e) {
    var t = e.target instanceof Element ? e.target : null;
    if (!t) {
      return;
    }
    var opener = t.closest("[data-dialog]");
    if (opener) {
      var dlg = document.getElementById(opener.getAttribute("data-dialog"));
      if (dlg && typeof dlg.showModal === "function") {
        e.preventDefault();
        // The menu the trigger sat in (a row menu, the user menu) must not stay open
        // behind the modal; the style guide's static menus stay as they are.
        document
          .querySelectorAll(
            "details.rowmenu[open]:not([data-static]), details.navgroup[open]:not([data-static])",
          )
          .forEach(function (m) {
            m.open = false;
          });
        fill(dlg, opener);
        dlg.showModal();
      }
      return;
    }
    if (t.closest("[data-close]")) {
      var d = t.closest("dialog");
      if (d) {
        d.close();
      }
      return;
    }
    if (t instanceof HTMLDialogElement && t.open) {
      t.close();
    } // click on the backdrop
  });
  // A dialog the server sends open (data-autoopen: the answer to a refused action, such as
  // "Confirm it's you") becomes a modal: focus inside, the page behind it inert. Without this
  // file it stays open in the page.
  document
    .querySelectorAll("dialog[data-autoopen][open]")
    .forEach(function (d) {
      if (typeof d.showModal === "function") {
        d.close();
        d.showModal();
      }
    });
})();
