// Marks the fields of a form[data-track-changes] that differ from the value the
// device reported (the hidden orig.<name> field next to each), and counts them.
// Without this file the form works the same; nothing is marked.
(function () {
  "use strict";

  function fields(form) {
    return Array.prototype.slice
      .call(form.querySelectorAll("input[name], select[name]"))
      .filter(function (f) {
        return (
          f.name.indexOf("orig.") !== 0 &&
          f.type !== "hidden" &&
          f.type !== "checkbox" &&
          form.elements["orig." + f.name]
        );
      });
  }
  function sync(form) {
    var n = 0;
    fields(form).forEach(function (f) {
      var changed =
        f.type === "password"
          ? f.value !== ""
          : f.value !== form.elements["orig." + f.name].value;
      var label = f.closest("label") || f.closest(".kv-row");
      if (label) {
        label.classList.toggle("is-changed", changed);
      }
      if (changed) {
        n++;
      }
    });
    form.querySelectorAll("[data-changed-count]").forEach(function (c) {
      c.textContent =
        n === 0
          ? "No changes yet"
          : n + (n === 1 ? " setting changed" : " settings changed");
    });
  }
  function all() {
    document.querySelectorAll("form[data-track-changes]").forEach(sync);
  }
  document.addEventListener("input", function (e) {
    var form =
      e.target instanceof Element
        ? e.target.closest("form[data-track-changes]")
        : null;
    if (form) {
      sync(form);
    }
  });
  document.addEventListener("change", function (e) {
    var form =
      e.target instanceof Element
        ? e.target.closest("form[data-track-changes]")
        : null;
    if (form) {
      sync(form);
    }
  });
  // Typed values survive a step-up. A form[data-keep-draft] saves its plain
  // fields to sessionStorage when it is submitted (passwords are never kept)
  // and puts them back when the page is opened with ?restore=1, the address the
  // step-up returns to. Any other visit drops the draft. Without storage the
  // form works as before, minus the restore.
  function draftKey(form) {
    return "tf-draft:" + form.getAttribute("data-keep-draft");
  }
  function draftForms() {
    return document.querySelectorAll("form[data-keep-draft]");
  }
  function draftRestore() {
    var want = /[?&]restore=1(&|$)/.test(location.search);
    draftForms().forEach(function (form) {
      try {
        var raw = sessionStorage.getItem(draftKey(form));
        sessionStorage.removeItem(draftKey(form));
        if (!want || !raw) {
          return;
        }
        var d = JSON.parse(raw);
        Object.keys(d).forEach(function (name) {
          var f = form.elements[name];
          if (!f || f.type === "password" || f.type === "hidden") {
            return;
          }
          if (f.type === "checkbox") {
            f.checked = d[name] === "1";
          } else {
            f.value = d[name];
          }
          // a restored value inside a closed fold would be hidden (the Backups schedule, #684)
          var el = f.closest ? f : f[0]; // several fields of one name come as a list
          var fold = el && el.closest ? el.closest("details") : null;
          if (fold) {
            fold.open = true;
          }
        });
      } catch (e) {
        /* storage unavailable or bad draft: nothing to restore */
      }
    });
  }
  // A field that fails its constraint (a time left empty) inside a closed fold cannot be focused, so the
  // browser would refuse the save without showing why. The fold opens around it. "invalid" does not
  // bubble, hence the capture phase.
  document.addEventListener(
    "invalid",
    function (e) {
      var fold =
        e.target instanceof Element ? e.target.closest("details") : null;
      if (fold && !fold.open) {
        fold.open = true;
      }
    },
    true,
  );
  document.addEventListener("submit", function (e) {
    var form = e.target;
    if (
      !(form instanceof HTMLFormElement) ||
      !form.hasAttribute("data-keep-draft")
    ) {
      return;
    }
    try {
      var d = {};
      Array.prototype.forEach.call(form.elements, function (f) {
        if (
          !f.name ||
          f.type === "password" ||
          f.type === "hidden" ||
          f.type === "submit" ||
          f.type === "button" ||
          (f.type === "radio" && !f.checked)
        ) {
          return;
        }
        d[f.name] = f.type === "checkbox" ? (f.checked ? "1" : "0") : f.value;
      });
      sessionStorage.setItem(draftKey(form), JSON.stringify(d));
    } catch (err) {
      /* storage unavailable: no restore */
    }
  });
  // A pill row that scrolls sideways on a narrow screen (Devices > Backups, the Policy pages; the device
  // list's row wraps instead since #678), and every filter choice loads the page again, which would put
  // the row back to its start and move every pill (#116). The scroll position is kept across that one
  // reload: saved when a pill or a select in the row is used, restored (once) on the next page.
  var chipsKey = "tf-chips-scroll";
  function saveChipsScroll(e) {
    var row = e.target instanceof Element && e.target.closest(".dv-chips");
    if (row) {
      try {
        sessionStorage.setItem(chipsKey, String(row.scrollLeft));
      } catch (err) {
        /* storage unavailable: the row starts at its beginning */
      }
    }
  }
  document.addEventListener("click", saveChipsScroll);
  document.addEventListener("change", saveChipsScroll);
  function restoreChipsScroll() {
    var row = document.querySelector(".dv-chips");
    var kept = false;
    try {
      var x = sessionStorage.getItem(chipsKey);
      sessionStorage.removeItem(chipsKey);
      if (row && x) {
        row.scrollLeft = Number(x);
        kept = true;
      }
    } catch (err) {
      /* storage unavailable */
    }
    // An active pill the row hides (a Backups or Policy tile opens the page filtered; on a phone the
    // row is wider than the screen) is scrolled into view, so the filter shows. Only the row
    // scrolls, never the page (#381). Not after the row's own reload: the person just used the row
    // where it was, and the default "All" pill is current whenever only Chip or Tag is chosen, so
    // scrolling it into view would throw the row back to its start (#116, #428).
    var on =
      row && !kept && row.querySelectorAll('.dv-chip[aria-current="true"]');
    if (on && on.length) {
      var chip = on[on.length - 1].getBoundingClientRect();
      var box = row.getBoundingClientRect();
      if (chip.right > box.right) {
        row.scrollLeft += chip.right - box.right;
      } else if (chip.left < box.left) {
        row.scrollLeft -= box.left - chip.left;
      }
    }
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", restoreChipsScroll);
  } else {
    restoreChipsScroll();
  }
  // A select with data-autosubmit applies its filter at once (the device list's Chip, Tag and
  // Sort); without this file the form's Filter button (noscript) submits it.
  document.addEventListener("change", function (e) {
    var t = e.target;
    if (
      t instanceof HTMLSelectElement &&
      t.hasAttribute("data-autosubmit") &&
      t.form
    ) {
      t.form.requestSubmit();
    }
  });
  // "Reset to default on save" (clear.KEY) wins over the field's own value: the
  // field is not sent next to its ticked box. Without this file the server lets
  // the ticked box win (#623).
  document.addEventListener("submit", function (e) {
    var form = e.target;
    if (!(form instanceof HTMLFormElement)) {
      return;
    }
    form
      .querySelectorAll('input[type="checkbox"][name^="clear."]:checked')
      .forEach(function (box) {
        var key = box.name.slice("clear.".length);
        form
          .querySelectorAll('[name="v.' + key + '"], [name="b.' + key + '"]')
          .forEach(function (f) {
            f.disabled = true;
          });
      });
  });
  // "Reset to default on save" shows its result at once (#623): ticking clear.KEY puts the default
  // (data-default on the box, from the server) into the field, so the page shows what Save will
  // store; unticking puts the old value back; typing in the field afterwards is the person's own
  // value and unticks the box. A switch, a choice or a segment is set the same way, and the input
  // and change events let the save bar and the settings that depend on a switch follow. The broker
  // row (MQTT_URL) is its host, port and TLS switch. Without this file the box is just posted and
  // the server stores the default.
  var resetPrev = new Map();
  var resetting = false;
  function resetFieldsOf(key) {
    var form = document.getElementById("sysform");
    if (!form) {
      return [];
    }
    var names =
      key === "MQTT_URL" ? ["mqtt.host", "mqtt.port", "mqtt.tls"] : ["v." + key];
    return Array.prototype.slice.call(form.elements).filter(function (f) {
      return names.indexOf(f.name) >= 0 && f.type !== "hidden";
    });
  }
  function resetValueFor(f, def, key) {
    if (key === "MQTT_URL") {
      return f.type === "checkbox" ? { checked: false } : { value: "" };
    }
    if (f.type === "checkbox") {
      return {
        checked:
          f.value === "true" ? def === "true" : def.split(",").indexOf(f.value) >= 0,
      };
    }
    if (f.type === "radio") {
      return { checked: f.value === def };
    }
    return { value: def };
  }
  document.addEventListener("change", function (e) {
    var box = e.target;
    if (
      resetting ||
      !(box instanceof HTMLInputElement) ||
      box.name.indexOf("clear.") !== 0 ||
      !box.hasAttribute("data-default")
    ) {
      return;
    }
    var key = box.name.slice("clear.".length);
    var def = box.getAttribute("data-default");
    var fields = resetFieldsOf(key);
    if (box.checked) {
      // Every field first: checking the default radio unchecks the stored one of the group.
      fields.forEach(function (f) {
        resetPrev.set(f, { value: f.value, checked: f.checked });
      });
    }
    resetting = true;
    fields.forEach(function (f) {
      var to = box.checked ? resetValueFor(f, def, key) : resetPrev.get(f);
      if (!to) {
        return;
      }
      if ("value" in to && f.type !== "checkbox" && f.type !== "radio") {
        f.value = to.value;
      } else {
        f.checked = to.checked;
      }
      f.dispatchEvent(new Event("input", { bubbles: true }));
      f.dispatchEvent(new Event("change", { bubbles: true }));
    });
    resetting = false;
  });
  function untickResetOnEdit(e) {
    var f = e.target;
    if (
      resetting ||
      !(
        f instanceof HTMLInputElement ||
        f instanceof HTMLSelectElement ||
        f instanceof HTMLTextAreaElement
      ) ||
      !f.form
    ) {
      return;
    }
    var key = /^v\.(.+)$/.exec(f.name);
    key = key ? key[1] : /^mqtt\.(host|port|tls)$/.test(f.name) ? "MQTT_URL" : "";
    var box = key && f.form.elements["clear." + key];
    if (box && box.checked) {
      resetting = true; // the typed value stays; only the save bar and the switches follow
      box.checked = false;
      resetPrev.delete(f);
      box.dispatchEvent(new Event("change", { bubbles: true }));
      resetting = false;
    }
  }
  document.addEventListener("input", untickResetOnEdit);
  document.addEventListener("change", untickResetOnEdit);
  // A filter form (form[data-autosubmit]: the audit filter, the backup comparison's pickers)
  // applies a changed select, checkbox or radio at once; its submit button
  // (data-autosubmit-hide) is for browsers without this file. Text fields wait for the submit.
  document.addEventListener("change", function (e) {
    var t = e.target;
    var choice =
      t instanceof HTMLSelectElement ||
      (t instanceof HTMLInputElement &&
        (t.type === "checkbox" || t.type === "radio"));
    if (!choice || t.hasAttribute("data-autosubmit")) {
      return; // a select with its own data-autosubmit: the rule above submits it
    }
    var form = t.closest("form[data-autosubmit]");
    if (form) {
      form.requestSubmit();
    }
  });
  document.addEventListener("DOMContentLoaded", function () {
    document
      .querySelectorAll("form[data-autosubmit] [data-autosubmit-hide]")
      .forEach(function (b) {
        b.hidden = true;
      });
  });
  document.addEventListener("DOMContentLoaded", draftRestore);
  document.addEventListener("DOMContentLoaded", all);
  window.addEventListener("pageshow", all);
})();

// The import drop zone (Add devices, Import from TasmoAdmin): files dropped on a
// [data-dropzone] go into its file input, whose change uploads them (htmx,
// hx-trigger="change"). Without this file the "Choose files" button does the same.
(function () {
  "use strict";

  function zone(e) {
    return e.target instanceof Element
      ? e.target.closest("[data-dropzone]")
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
    if (z && !z.contains(e.relatedTarget)) {
      z.classList.remove("is-over");
    }
  });
  document.addEventListener("drop", function (e) {
    var z = zone(e);
    if (!z || !e.dataTransfer) {
      return;
    }
    e.preventDefault();
    z.classList.remove("is-over");
    var input = z.querySelector('input[type="file"]');
    if (input && e.dataTransfer.files.length > 0) {
      input.files = e.dataTransfer.files;
      input.dispatchEvent(new Event("change", { bubbles: true }));
    }
  });
})();

// Busy state of actions (#538). The control a person clicked is disabled, spins (the
// button.htmx-request[disabled] rule in app.css) and says aria-busy until the answer is there, so a
// second click does nothing; after about 10 s a toast says it is still working. Two sources:
// the submitter of a plain POST form (busy until the next page arrives, a form[data-no-busy] such
// as a file download opts out) and the htmx controls (busy until the request ends). A section that
// loads by itself (aria-busy="true" with hx-trigger="load") gets the same note, and the note goes
// away when its request ends. A control a person changes (a file, a field, a form with a live
// preview: htmx events change and input) says aria-busy and draws a ring (app.css), nothing more:
// it is never disabled and a newer request is never dropped, so the newest value is always sent.
// A request that never got an answer (htmx:sendError, htmx:timeout) says so in a toast naming the
// control (#634). A link to a page whose handler reads a device before it answers says aria-busy
// until the page replaces this one. Without this file everything works the same, minus the busy look.
(function () {
  "use strict";
  var NOTE_MS = 10000;
  var NOTE = "Still working. A slow device can take up to a minute.";
  var notes = new WeakMap(); // element -> its pending note: {timer, close} (close once it is shown)
  var failures = new WeakMap(); // element -> the function that closes its error toast

  // One timer per element, cancelled when its work ends (quiet): the timer of an earlier, finished
  // request on the same control must not speak for a later one that has only just started.
  function note(el, still) {
    quiet(el);
    var n = { timer: 0, close: null };
    n.timer = window.setTimeout(function () {
      if (still() && typeof window.tfToast === "function") {
        n.close = window.tfToast(NOTE, "info");
      }
    }, NOTE_MS);
    notes.set(el, n);
  }
  // the work ended: no note is due any more, and one that is showing goes away with it
  function quiet(el) {
    var n = notes.get(el);
    if (n) {
      window.clearTimeout(n.timer);
      if (n.close) {
        n.close();
      }
      notes.delete(el);
    }
  }
  function submitButtons(form) {
    var sel = "button[type=submit], button:not([type])";
    var list = Array.prototype.slice.call(form.querySelectorAll(sel));
    if (form.id) {
      list = list.concat(
        Array.prototype.slice.call(
          document.querySelectorAll(
            'button[form="' + form.id + '"]:not([type=button])',
          ),
        ),
      );
    }
    return list;
  }
  function on(btn) {
    btn.setAttribute("aria-busy", "true");
    btn.classList.add("htmx-request");
    btn.disabled = true;
  }
  function off(btn) {
    btn.removeAttribute("aria-busy");
    btn.classList.remove("htmx-request");
    btn.disabled = false;
  }
  function targets(el) {
    return el instanceof HTMLFormElement ? submitButtons(el) : [el];
  }

  document.addEventListener("submit", function (e) {
    var form = e.target;
    if (
      !(form instanceof HTMLFormElement) ||
      form.method !== "post" ||
      form.target ||
      form.hasAttribute("data-no-busy") ||
      form.hasAttribute("data-fw-check")
    ) {
      return;
    }
    if (form.hasAttribute("data-busy")) {
      e.preventDefault();
      return;
    }
    var sub = e.submitter;
    // after the submission has started (a disabled button would not be sent) and after the other
    // submit handlers had their say (a confirmation that cancels it must not leave the form busy;
    // htmx cancels the submit of a form it sends itself, which the htmx events below cover)
    window.setTimeout(function () {
      if (e.defaultPrevented) {
        return;
      }
      form.setAttribute("data-busy", "");
      var btns = sub ? [sub] : submitButtons(form).slice(0, 1);
      btns.forEach(on);
      note(form, function () {
        return form.isConnected && form.hasAttribute("data-busy");
      });
    }, 0);
  });
  // the page came back from the back/forward cache with its form still busy
  window.addEventListener("pageshow", function (e) {
    if (!e.persisted) {
      return;
    }
    document.querySelectorAll("form[data-busy]").forEach(function (f) {
      f.removeAttribute("data-busy");
      quiet(f);
    });
    document.querySelectorAll("button[aria-busy=true]").forEach(off);
    document.querySelectorAll("a[aria-busy=true]").forEach(function (a) {
      a.removeAttribute("aria-busy");
    });
  });

  // What a person did to start an htmx request: "act" (a click or a submit: the control is disabled
  // and ignores a second click), "edit" (a change or input: aria-busy only), "" for everything else
  // (a live swap announced by the server, polling, a load placeholder, htmx.ajax). The same answer
  // is used when the request starts and when it ends, so a control is never left busy.
  function trigger(e) {
    var t =
      e.detail &&
      e.detail.requestConfig &&
      e.detail.requestConfig.triggeringEvent;
    if (!t) {
      return "";
    }
    if (t.type === "click" || t.type === "submit") {
      return "act";
    }
    return t.type === "change" || t.type === "input" ? "edit" : "";
  }
  // an edited control sends its request to a target (the part of the page that changes); the target
  // is marked too, so the part that is being replaced can show it (the live rule preview)
  function edited(e, el) {
    var tg = e.detail.requestConfig.target;
    return tg instanceof Element && tg !== el ? [el, tg] : [el];
  }
  document.addEventListener("htmx:beforeRequest", function (e) {
    var el = e.detail && e.detail.elt;
    var k = el ? trigger(e) : "";
    if (!k) {
      return;
    }
    if (k === "edit") {
      // never preventDefault here: the newer value is the one that has to be sent
      edited(e, el).forEach(function (x) {
        x.setAttribute("aria-busy", "true");
      });
    } else {
      if (el.getAttribute("aria-busy") === "true") {
        e.preventDefault();
        return;
      }
      el.setAttribute("aria-busy", "true");
      targets(el).forEach(on);
    }
    note(el, function () {
      return el.isConnected && el.getAttribute("aria-busy") === "true";
    });
  });
  document.addEventListener("htmx:afterRequest", function (e) {
    // the element that sent the request: when its own swap removed it, htmx repeats this event on
    // the closest ancestor still in the page (detail.elt), which was never busy and stays untouched
    var cfg = e.detail && e.detail.requestConfig;
    var el = cfg && cfg.elt;
    if (!el) {
      return;
    }
    // the note belongs to the request that ended, whatever started it (a placeholder that loads by
    // itself has one too); the note of a plain form that is still being sent is not ours to close
    if (!el.hasAttribute("data-busy")) {
      quiet(el);
    }
    var k = trigger(e);
    if (!k) {
      return;
    }
    (k === "edit" ? edited(e, el) : [el]).forEach(function (x) {
      x.removeAttribute("aria-busy");
    });
    if (k === "act") {
      targets(el).forEach(off);
    }
  });

  // The name of a control for a message: what it says, else what it is labelled. "Toggle relay 1,
  // currently ON" is cut at the comma.
  function nameOf(el) {
    var src = el instanceof HTMLFormElement ? submitButtons(el)[0] : null;
    var s =
      el.getAttribute("data-busy-label") ||
      (src && src.textContent) ||
      el.getAttribute("aria-label") ||
      (el.labels && el.labels[0] && el.labels[0].textContent) ||
      (el instanceof HTMLButtonElement || el instanceof HTMLAnchorElement
        ? el.textContent
        : "") ||
      el.getAttribute("title") ||
      "";
    s = s.split(",")[0].replace(/\s+/g, " ").trim();
    return s && s.length <= 40 ? s : "The request";
  }
  // A person-started request that got no answer. One toast per control: a preview that fails on
  // every keystroke must not pile them up. A server error (4xx, 5xx) is an answer and is swapped in.
  function failed(e, what) {
    var cfg = e.detail && e.detail.requestConfig;
    var el = cfg && cfg.elt;
    if (!el || !trigger(e) || typeof window.tfToast !== "function") {
      return;
    }
    var prev = failures.get(el);
    if (prev) {
      prev();
    }
    failures.set(el, window.tfToast(nameOf(el) + " " + what, "danger"));
  }
  document.addEventListener("htmx:sendError", function (e) {
    failed(e, "was not sent: no connection.");
  });
  // htmx has no timeout of its own (shell.templ htmxConfig); this is for one that is set
  document.addEventListener("htmx:timeout", function (e) {
    failed(e, "timed out.");
  });

  // Pages whose handler reads a device before it answers (#634): the restore confirmation, the
  // drift comparison (a live read when the cached one is stale), the replace page (it asks every
  // discovered device) and its preview. Every other GET reads the
  // database or the cache. Until the page arrives, the link or the form button that was used says so.
  var DEVICE_PAGES = /\/(?:backups\/\d+\/restore|devices\/\d+\/(?:drift|replace(?:\/preview)?))$/;
  function devicePage(url) {
    try {
      var u = new URL(url);
      return u.origin === window.location.origin && DEVICE_PAGES.test(u.pathname);
    } catch (err) {
      return false; // not a URL: not ours
    }
  }
  document.addEventListener("click", function (e) {
    var a = e.target instanceof Element ? e.target.closest("a[href]") : null;
    if (
      !a ||
      e.button !== 0 ||
      e.ctrlKey ||
      e.metaKey ||
      e.shiftKey ||
      e.altKey ||
      (a.target && a.target !== "_self") ||
      a.hasAttribute("download") ||
      !devicePage(a.href)
    ) {
      return;
    }
    // after the other handlers had their say (a tab or a menu may take the click over)
    window.setTimeout(function () {
      if (!e.defaultPrevented) {
        a.setAttribute("aria-busy", "true");
      }
    }, 0);
  });
  document.addEventListener("submit", function (e) {
    var form = e.target;
    if (
      !(form instanceof HTMLFormElement) ||
      form.method !== "get" ||
      form.target ||
      !e.submitter ||
      !devicePage(form.action)
    ) {
      return;
    }
    var btn = e.submitter;
    window.setTimeout(function () {
      if (!e.defaultPrevented) {
        on(btn);
      }
    }, 0);
  });

  // a section that loads by itself says it is still working too
  var PLACEHOLDER = '[aria-busy=true][hx-trigger="load"]:not([data-busy-watch])';
  function watch(s) {
    s.setAttribute("data-busy-watch", "");
    note(s, function () {
      return s.isConnected && s.getAttribute("aria-busy") === "true";
    });
  }
  // watchPlaceholders looks at root (the document, or the element a swap settled) and its subtree
  function watchPlaceholders(root) {
    if (root.matches(PLACEHOLDER)) {
      watch(root);
    }
    root.querySelectorAll(PLACEHOLDER).forEach(watch);
  }
  document.addEventListener("DOMContentLoaded", function () {
    watchPlaceholders(document.documentElement);
  });
  // only the swapped element: a document-wide walk per live cell swap is what #726 and #1059 removed
  document.addEventListener("htmx:afterSettle", function (e) {
    if (e.target instanceof Element) {
      watchPlaceholders(e.target);
    }
  });
})();

// A refresh that fetches values by itself must not overwrite what the person is typing (#599). A
// container marked data-guard-swap that was edited refuses the swap; the element that asked for it
// (data-guard-text) says so in its data-guard-note and stops being busy. The rest of that line (the
// age of the values shown, the Stale marker) stays: the values on the page are still those.
(function () {
  "use strict";
  function guarded(el) {
    return el instanceof Element ? el.closest("[data-guard-swap]") : null;
  }
  function mark(e) {
    var box = guarded(e.target);
    if (box) {
      box.setAttribute("data-dirty", "");
    }
  }
  document.addEventListener("input", mark);
  document.addEventListener("change", mark);
  // Reset puts the values back, so there is nothing left to protect
  document.addEventListener("reset", function (e) {
    var box = guarded(e.target);
    if (box) {
      box.removeAttribute("data-dirty");
    }
  });
  document.addEventListener("htmx:beforeSwap", function (e) {
    var t = e.detail && e.detail.target;
    var cfg = e.detail && e.detail.requestConfig;
    var asker = cfg && cfg.elt;
    if (
      !(t instanceof Element) ||
      !t.hasAttribute("data-guard-swap") ||
      !t.hasAttribute("data-dirty") ||
      !(asker instanceof Element) ||
      !asker.hasAttribute("data-guard-text")
    ) {
      return;
    }
    e.detail.shouldSwap = false;
    asker.removeAttribute("aria-busy");
    (asker.querySelector("[data-guard-note]") || asker).textContent =
      asker.getAttribute("data-guard-text");
  });
})();
