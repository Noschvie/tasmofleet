// Copy buttons: <button data-copy-text="value" hidden> copies the value;
// <button data-copy-from="#id" hidden> copies the text of that element (a
// secret shown once must not be in the markup twice). The buttons are hidden
// in the markup and shown here, so without this file the page has no dead
// button (the full value is in the title of its neighbour, or on screen).
// No inline script: CSP forbids that.
(function () {
  "use strict";
  var HIDDEN = "button[data-copy-text][hidden], button[data-copy-from][hidden]";
  // show reveals the hidden copy buttons of root (the document, or the element a swap settled) and root itself.
  function show(root) {
    if (root.matches(HIDDEN)) {
      root.hidden = false;
    }
    root.querySelectorAll(HIDDEN).forEach(function (b) {
      b.hidden = false;
    });
  }
  // Plain http on a LAN has no navigator.clipboard; fall back to a selection.
  function write(text) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text);
    }
    return new Promise(function (resolve, reject) {
      var ta = document.createElement("textarea");
      ta.value = text;
      ta.setAttribute("readonly", "");
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      var ok = false;
      try {
        ok = document.execCommand("copy");
      } catch (err) {
        ok = false;
      }
      document.body.removeChild(ta);
      if (ok) {
        resolve();
      } else {
        reject(new Error("copy failed"));
      }
    });
  }
  document.addEventListener("click", function (e) {
    var b =
      e.target instanceof Element
        ? e.target.closest("button[data-copy-text], button[data-copy-from]")
        : null;
    if (!b) {
      return;
    }
    var label = b.getAttribute("data-label") || b.textContent;
    b.setAttribute("data-label", label);
    var from = b.getAttribute("data-copy-from");
    var src = from ? document.querySelector(from) : null;
    write(src ? src.textContent : b.getAttribute("data-copy-text") || "")
      .then(
        function () {
          b.textContent = "Copied";
        },
        function () {
          b.textContent = "Copy failed";
        },
      )
      .then(function () {
        setTimeout(function () {
          b.textContent = label;
        }, 1500);
      });
  });
  function showAll() {
    show(document.documentElement);
  }
  document.addEventListener("DOMContentLoaded", showAll);
  window.addEventListener("pageshow", showAll);
  // a swap only needs its own element: a document-wide walk per live cell swap is what #726 and #1059 removed
  document.addEventListener("htmx:afterSettle", function (e) {
    if (e.target instanceof Element) {
      show(e.target);
    }
  });
})();
