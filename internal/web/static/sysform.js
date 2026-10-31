// The Settings page (System -> Settings). All eight tabs are in one form; this file
//  - shows one tab at a time without a page load, so edits on another tab are not lost,
//  - tracks unsaved changes per tab (save bar, dot in the tab strip, leave warning),
//  - dims settings and cards whose switch or sign-in mode is off (data-depends),
//  - runs the phone section picker and the backup schedule switch.
// Without this file the tabs are links (a page load per tab) and everything is enabled.
(function () {
  "use strict";

  var form = document.getElementById("sysform");
  if (!form) {
    return;
  }
  var strip = document.querySelector(
    'nav.utabs[aria-label="Settings sections"]',
  );
  var picker = document.querySelector("[data-sys-picker]");
  var bar = form.querySelector("[data-sys-bar]");
  var leaving = false; // Save and Discard must not trigger the leave warning

  function panels() {
    return Array.prototype.slice.call(form.querySelectorAll("[data-sys-tab]"));
  }
  function slugOfHref(href) {
    var m = /[?&]tab=([a-z]+)/.exec(href || "");
    return m ? m[1] : "";
  }

  // ---- tabs ----
  function showTab(slug) {
    var known = false;
    document.querySelectorAll("[data-sys-tab]").forEach(function (el) {
      var on = el.getAttribute("data-sys-tab") === slug;
      known = known || on;
      el.hidden = !on;
    });
    if (!known) {
      return false;
    }
    if (strip) {
      strip.querySelectorAll("a.utab").forEach(function (a) {
        if (slugOfHref(a.getAttribute("href")) === slug) {
          a.setAttribute("aria-current", "page");
        } else {
          a.removeAttribute("aria-current");
        }
      });
    }
    if (picker) {
      picker.querySelector("select").value = slug;
    }
    // Save returns to the tab that was open, and Discard reloads it.
    form.setAttribute(
      "action",
      form.getAttribute("action").replace(/tab=[a-z]*/, "tab=" + slug),
    );
    var discard = bar && bar.querySelector("a.btn");
    if (discard) {
      discard.setAttribute(
        "href",
        discard.getAttribute("href").replace(/tab=[a-z]*/, "tab=" + slug),
      );
    }
    try {
      history.replaceState(null, "", "?tab=" + slug);
    } catch (e) {
      /* a sandboxed frame: the address stays */
    }
    return true;
  }
  if (strip) {
    strip.addEventListener("click", function (e) {
      var a = e.target instanceof Element ? e.target.closest("a.utab") : null;
      if (!a || e.ctrlKey || e.metaKey || e.shiftKey || e.button !== 0) {
        return;
      }
      if (showTab(slugOfHref(a.getAttribute("href")))) {
        e.preventDefault();
      }
    });
  }
  if (picker) {
    picker.hidden = false; // the phone CSS then hides the strip
    picker.querySelector("select").addEventListener("change", function (e) {
      showTab(e.target.value);
    });
  }

  // A link inside a tab to another tab (the network scan card names the device networks) switches
  // the tab without a page load, so unsaved edits stay.
  form.addEventListener("click", function (e) {
    var a = e.target instanceof Element ? e.target.closest("a[data-sys-goto]") : null;
    if (!a || e.ctrlKey || e.metaKey || e.shiftKey || e.button !== 0) {
      return;
    }
    if (showTab(a.getAttribute("data-sys-goto"))) {
      e.preventDefault();
      var card = document.getElementById("card-networks");
      if (card) {
        card.scrollIntoView();
      }
    }
  });

  // ---- broker changed: keep the stored password ----
  // The option only matters while host or port differ from the stored value (defaultValue).
  var keep = form.querySelector("[data-broker-keep]");
  function brokerKeep() {
    if (!keep) {
      return;
    }
    var host = form.elements["mqtt.host"];
    var port = form.elements["mqtt.port"];
    var box = keep.querySelector("input");
    // Compare with the broker that is SAVED (data attributes), not with what the page loaded with:
    // after a refused save the page shows the new broker, and the server still wants the choice.
    var changed =
      (host && host.value !== keep.getAttribute("data-stored-host")) ||
      (port && port.value !== keep.getAttribute("data-stored-port"));
    if (box.defaultChecked) {
      changed = true; // ticked in the save that is shown again: never hide or untick it
    }
    keep.hidden = !changed;
    if (!changed) {
      box.checked = false;
    }
  }

  // ---- unsaved changes ----
  function controls() {
    return Array.prototype.slice.call(form.elements).filter(function (c) {
      return (
        c.name &&
        c.name !== "csrf" &&
        c.type !== "hidden" &&
        c.type !== "submit" &&
        c.type !== "button"
      );
    });
  }
  function valueOf(c) {
    if (c.type === "checkbox" || c.type === "radio") {
      return c.checked ? "1" : "0";
    }
    if (c.name === "v.BACKUP_INTERVAL") {
      // Saved as 0 while the schedule switch is off, whatever the field holds.
      var sw = form.elements["sw.BACKUP_INTERVAL"];
      if (sw && !sw.checked) {
        return "0";
      }
    }
    return c.value;
  }
  var initial = new Map();
  controls().forEach(function (c) {
    initial.set(c, valueOf(c));
  });
  function dirtyTabs() {
    var out = [];
    panels().forEach(function (p) {
      var changed = controls().some(function (c) {
        return p.contains(c) && valueOf(c) !== initial.get(c);
      });
      if (changed) {
        out.push(p.getAttribute("data-sys-tab"));
      }
    });
    return out;
  }
  function tabLabel(slug) {
    var a = strip && strip.querySelector('a.utab[href*="tab=' + slug + '"]');
    return a ? a.textContent.trim().replace(/\s+$/, "") : slug;
  }
  function refresh() {
    var dirty = dirtyTabs();
    if (strip) {
      strip.querySelectorAll("a.utab").forEach(function (a) {
        a.classList.toggle(
          "is-dirty",
          dirty.indexOf(slugOfHref(a.getAttribute("href"))) >= 0,
        );
      });
    }
    if (!bar) {
      return;
    }
    bar.classList.toggle("is-dirty", dirty.length > 0);
    bar.querySelector("[data-sbar-state]").textContent = dirty.length
      ? "Unsaved changes:"
      : "No unsaved changes";
    var chips = bar.querySelector("[data-sbar-chips]");
    chips.textContent = "";
    dirty.forEach(function (slug) {
      var chip = document.createElement("span");
      chip.className = "sbar-chip";
      chip.textContent = tabLabel(slug);
      chips.appendChild(chip);
    });
    if (picker) {
      Array.prototype.forEach.call(
        picker.querySelectorAll("option"),
        function (o) {
          var base = o.textContent.replace(/ •$/, "");
          o.textContent = dirty.indexOf(o.value) >= 0 ? base + " •" : base;
        },
      );
    }
  }

  // ---- dependent settings ----
  // data-depends="NAME" (a switch is on) or "NAME=value" (a radio group has that value).
  function isOn(rule) {
    var parts = rule.split("=");
    var ctl = Array.prototype.slice.call(form.elements).filter(function (c) {
      return c.name === parts[0] && c.type !== "hidden";
    });
    if (ctl.length === 0) {
      return true;
    }
    if (parts.length > 1) {
      return ctl.some(function (c) {
        return c.checked && c.value === parts[1];
      });
    }
    return ctl.some(function (c) {
      return c.checked;
    });
  }
  function depends() {
    document.querySelectorAll("[data-depends]").forEach(function (el) {
      var on = isOn(el.getAttribute("data-depends"));
      el.classList.toggle("is-off", !on);
      if (on) {
        el.removeAttribute("inert");
      } else {
        el.setAttribute("inert", "");
      }
    });
  }

  function changed(e) {
    // Switching the backup schedule on with no interval fills a sensible one.
    var t = e.target;
    if (
      t instanceof HTMLInputElement &&
      t.name === "sw.BACKUP_INTERVAL" &&
      t.checked
    ) {
      var iv = form.elements["v.BACKUP_INTERVAL"];
      if (iv && (iv.value === "" || iv.value === "0" || iv.value === "0s")) {
        iv.setAttribute("data-before", iv.value); // what switching off puts back
        iv.value = "24h";
      }
    }
    // Switching off again takes back the proposed 24h, so on and off leaves nothing changed.
    if (
      t instanceof HTMLInputElement &&
      t.name === "sw.BACKUP_INTERVAL" &&
      !t.checked
    ) {
      var pv = form.elements["v.BACKUP_INTERVAL"];
      if (pv && pv.hasAttribute("data-before") && pv.value === "24h") {
        pv.value = pv.getAttribute("data-before");
      }
      if (pv) {
        pv.removeAttribute("data-before");
      }
    }
    depends();
    brokerKeep();
    refresh();
  }
  form.addEventListener("input", changed);
  form.addEventListener("change", changed);
  form.addEventListener("submit", function () {
    leaving = true;
  });
  if (bar) {
    bar.addEventListener("click", function (e) {
      if (e.target instanceof Element && e.target.closest("a.btn")) {
        leaving = true; // Discard throws the edits away on purpose
      }
    });
  }
  window.addEventListener("beforeunload", function (e) {
    if (!leaving && dirtyTabs().length > 0) {
      e.preventDefault();
      e.returnValue = "";
    }
  });
  window.addEventListener("pageshow", function () {
    leaving = false;
  });

  depends();
  brokerKeep();
  refresh();
  document.addEventListener("DOMContentLoaded", function () {
    // form.js puts back typed values after a step-up; they count as unsaved changes
    depends();
    brokerKeep();
    refresh();
  });
})();
