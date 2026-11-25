// Opens the dialogs of links and buttons with data-dialog (changelogs, delete
// confirmations). Without this file such a link goes to its page: the release
// shown inline, or the confirmation page of the action.
(function () {
  "use strict";
  // A dialog shared by many rows (the device list) is filled from the data-dlg-*
  // attributes of the link that opens it: title, text, the form's action and the
  // confirming button's verb; data-dlg-danger makes that button danger-solid (and its absence
  // puts it back to primary, so a red verb never carries over to the next opener);
  // data-dlg-dismiss names the dismissing button when "Cancel" would be ambiguous ("Keep
  // running"); data-dlg-field-<name> fills the hidden input[data-fill-field="<name>"] (the rule
  // a shared delete dialog removes), and an opener without it empties the field.
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
      var text = opener.getAttribute("data-dlg-text") || "";
      // data-dlg-at (RFC 3339) and data-dlg-ts (format key) name a time written into the text
      // in the server's zone; time.js says the same instant in the browser's zone
      var at = opener.getAttribute("data-dlg-at");
      var shown = opener.getAttribute("data-dlg-shown");
      if (at && shown && window.tasmofleetFormatTime) {
        var local = window.tasmofleetFormatTime(at, opener.getAttribute("data-dlg-ts"));
        if (local) text = text.split(shown).join(local);
      }
      el.textContent = text;
    });
    set("form", function (el) {
      el.setAttribute("action", opener.getAttribute("data-dlg-action") || "");
    });
    set("dismiss", function (el) {
      el.textContent = opener.getAttribute("data-dlg-dismiss") || "Cancel";
    });
    set("verb", function (el) {
      var danger = opener.hasAttribute("data-dlg-danger");
      el.textContent = opener.getAttribute("data-dlg-verb") || "";
      el.classList.toggle("btn-danger-solid", danger);
      el.classList.toggle("btn-primary", !danger);
    });
    dlg.querySelectorAll("input[data-fill-field]").forEach(function (el) {
      el.value = opener.getAttribute("data-dlg-field-" + el.getAttribute("data-fill-field")) || "";
    });
  }
  // A dialog whose confirming button is danger-solid opens on its Cancel (the first element with
  // data-close), not on the first focusable thing in it (the Copy button of a file's checksum):
  // Enter or Space on a dialog that just opened must not run the destructive action.
  function focusCancel(dlg) {
    if (!dlg.querySelector('button[type="submit"].btn-danger-solid')) {
      return;
    }
    var cancel = dlg.querySelector("[data-close]");
    if (cancel) {
      cancel.focus();
    }
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
        focusCancel(dlg);
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
        focusCancel(d);
      }
    });
})();
