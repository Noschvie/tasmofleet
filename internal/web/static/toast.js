// Toasts (design v2): short confirmations bottom right. A success or info toast fades after
// 4.5 s; a warning or failure stays until dismissed. Two sources feed them: the server's flash
// after a redirect (rendered as an inline banner, [data-flash], which is all a browser without
// JavaScript shows) and the htmx event "toast" from an HX-Trigger response header
// ({"toast": {"text": "...", "tone": "ok|info|warn|danger"}}), and buttons with data-toast.
(function () {
  "use strict";
  var FADE_MS = 4500;

  function region() {
    return document.getElementById("toasts");
  }

  function show(text, tone) {
    var r = region();
    if (!r || !text) {
      return;
    }
    tone = tone || "ok";
    var t = document.createElement("div");
    t.className = "toast tone-" + tone;
    var dot = document.createElement("span");
    dot.className = "toast-dot";
    var txt = document.createElement("span");
    txt.className = "toast-text";
    txt.textContent = text;
    t.appendChild(dot);
    t.appendChild(txt);
    if (tone === "warn" || tone === "danger") {
      t.classList.add("has-x");
      var x = document.createElement("button");
      x.type = "button";
      x.className = "toast-x";
      x.setAttribute("aria-label", "Dismiss");
      x.textContent = "×";
      x.addEventListener("click", function () {
        t.remove();
      });
      t.appendChild(x);
    } else {
      window.setTimeout(function () {
        t.remove();
      }, FADE_MS);
    }
    r.appendChild(t);
  }
  window.tfToast = show;

  function fromFlash() {
    document.querySelectorAll("[data-flash]").forEach(function (f) {
      var tone = f.classList.contains("tone-warn")
        ? "warn"
        : f.classList.contains("tone-danger")
          ? "danger"
          : "ok";
      show(f.textContent.trim(), tone);
      f.remove();
    });
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", fromFlash);
  } else {
    fromFlash();
  }

  // A button with data-toast shows that text as a toast (data-toast-tone: ok, info, warn,
  // danger): the style guide's "Show success" and friends.
  document.addEventListener("click", function (e) {
    var b =
      e.target instanceof Element ? e.target.closest("[data-toast]") : null;
    if (b) {
      show(b.getAttribute("data-toast"), b.getAttribute("data-toast-tone"));
    }
  });

  document.addEventListener("toast", function (e) {
    var d = e.detail || {};
    if (typeof d.value === "string") {
      show(d.value, "ok");
      return;
    }
    show(d.text || "", d.tone);
  });
})();
