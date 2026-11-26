// Toasts (design v2): short confirmations bottom right. A success or info toast fades after
// 4.5 s; a warning or failure stays until dismissed. Two sources feed them: the server's flash
// after a redirect (rendered as an inline banner, [data-flash], which is all a browser without
// JavaScript shows) and the htmx event "toast" from an HX-Trigger response header
// ({"toast": {"text": "...", "tone": "ok|accent|warn|danger|neutral"}}), and buttons with data-toast.
// The tone is one of the design system's five (.t-ok ... .t-accent); a name outside them is neutral,
// as ui.Tone.class() does on the server.
(function () {
  "use strict";
  var FADE_MS = 4500;
  var TONES = { ok: 1, warn: 1, danger: 1, neutral: 1, accent: 1 };

  function region() {
    return document.getElementById("toasts");
  }

  // show returns a function that closes the toast again (a no-op when nothing was shown), so a caller
  // that announced something for a while can take it back when that is over (#634).
  function show(text, tone) {
    var r = region();
    if (!r || !text) {
      return function () {};
    }
    tone = !tone ? "ok" : TONES[tone] === 1 ? tone : "neutral";
    var t = document.createElement("div");
    t.className = "toast t-" + tone;
    var dot = document.createElement("span");
    dot.className = "toast-dot";
    var txt = document.createElement("span");
    txt.className = "toast-text";
    txt.textContent = text;
    t.appendChild(dot);
    t.appendChild(txt);
    var timer = 0;
    function close() {
      window.clearTimeout(timer);
      t.remove();
    }
    if (tone === "warn" || tone === "danger") {
      t.classList.add("has-x");
      var x = document.createElement("button");
      x.type = "button";
      x.className = "toast-x";
      x.setAttribute("aria-label", "Dismiss");
      x.textContent = "×";
      x.addEventListener("click", close);
      t.appendChild(x);
    } else {
      timer = window.setTimeout(close, FADE_MS);
    }
    r.appendChild(t);
    return close;
  }
  window.tfToast = show;

  function fromFlash() {
    document.querySelectorAll("[data-flash]").forEach(function (f) {
      var tone = f.classList.contains("t-warn")
        ? "warn"
        : f.classList.contains("t-danger")
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

  // A button with data-toast shows that text as a toast (data-toast-tone: ok, accent, warn,
  // danger, neutral): the style guide's "Show success" and friends.
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
