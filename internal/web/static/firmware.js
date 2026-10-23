// Firmware page (WP P5). "Check for new releases now" shows the checking state while its
// request runs, and a firmware file dropped on the drop zone opens the upload form with the
// file already chosen. Without this file the check is a plain form post and the zone's link
// opens the upload form.
(function () {
  "use strict";

  document.addEventListener("submit", function (e) {
    var form = e.target instanceof HTMLFormElement ? e.target : null;
    if (!form || !form.hasAttribute("data-fw-check")) {
      return;
    }
    var label = form.getAttribute("data-checking") || "Checking…";
    document.querySelectorAll("[data-fw-check-state]").forEach(function (s) {
      s.className = "fw-check t-accent";
      s.textContent = "";
      var dot = document.createElement("span");
      dot.className = "fw-check-dot pulse";
      dot.setAttribute("aria-hidden", "true");
      s.appendChild(dot);
      s.appendChild(document.createTextNode(label));
    });
    // after the submission has started: a disabled button would not be sent
    window.setTimeout(function () {
      document
        .querySelectorAll("form[data-fw-check] button[type=submit]")
        .forEach(function (b) {
          b.disabled = true;
          if (b.closest(".fw-check-form")) {
            b.textContent = "Checking…";
          }
        });
    }, 0);
  });

  function zone(e) {
    return e.target instanceof Element
      ? e.target.closest("[data-fw-drop]")
      : null;
  }
  document.addEventListener("dragover", function (e) {
    var z = zone(e);
    if (z) {
      e.preventDefault();
      z.classList.add("is-over");
    }
  });
  document.addEventListener("dragleave", function (e) {
    var z = zone(e);
    if (z) {
      z.classList.remove("is-over");
    }
  });
  document.addEventListener("drop", function (e) {
    var z = zone(e);
    if (!z || !e.dataTransfer || e.dataTransfer.files.length === 0) {
      return;
    }
    e.preventDefault();
    z.classList.remove("is-over");
    var dlg = document.getElementById("dlg-upload");
    var input = (dlg || document).querySelector("input[data-fw-file]");
    if (!input) {
      return;
    }
    input.files = e.dataTransfer.files;
    if (dlg && typeof dlg.showModal === "function" && !dlg.open) {
      dlg.showModal();
    }
  });
})();
