// Policy page (design v2, N3): drag a rule row to change the order, which is the precedence.
// On drop the new order (the old positions, comma separated) is posted with the version the
// page was drawn on; the server answers 409 when someone changed the rules meanwhile. Without
// this file the rule menu's "Move up" / "Move down" do the same, one place at a time.
(function () {
  "use strict";
  // The editor's command field: a refused save marks it invalid; typing another command ends that
  // (the server clears the reason text when the change reaches it).
  var cmd = document.getElementById("rule-command");
  if (cmd) {
    cmd.addEventListener("input", function () {
      cmd.removeAttribute("aria-invalid");
    });
  }
  // Rule form, "Applies to" (#235). The section is plain form fields (family, tag, device, except),
  // so it works without this file; this file adds the chips of the picked devices, the device
  // search, tags that move between "used" and the chips, and exceptions as chips. The search
  // fields never reach the live preview while you type: only changes of the scope do.
  var scope = document.querySelector("[data-scope]");
  if (scope) {
    var fire = function (el) {
      el.dispatchEvent(new Event("change", { bubbles: true }));
    };
    var chipX = function (label) {
      var b = document.createElement("button");
      b.type = "button";
      b.className = "pol-chip-x";
      b.setAttribute("aria-label", label);
      b.textContent = "\u00d7";
      return b;
    };
    var hush = function (el, types) {
      types.forEach(function (t) {
        el.addEventListener(t, function (e) {
          e.stopPropagation();
        });
      });
    };
    // devices: chips of the ticked rows, and the search over the rows
    var rows = Array.prototype.slice.call(scope.querySelectorAll("[data-scope-list] .pol-row"));
    var devChips = scope.querySelector("[data-dev-chips]");
    var devSearch = scope.querySelector("[data-scope-search]");
    var count = scope.querySelector("[data-scope-count]");
    var none = scope.querySelector("[data-scope-none]");
    var countText = count ? count.textContent : "";
    var drawDevChips = function () {
      devChips.textContent = "";
      rows.forEach(function (row) {
        var box = row.querySelector("input");
        if (!box.checked) {
          return;
        }
        var chip = document.createElement("span");
        chip.className = "pol-chip";
        chip.appendChild(document.createTextNode(box.value));
        var x = chipX("Remove " + box.value);
        x.addEventListener("click", function () {
          box.checked = false;
          drawDevChips();
          fire(box);
        });
        chip.appendChild(x);
        devChips.appendChild(chip);
      });
    };
    var filter = function () {
      var needle = devSearch.value.trim().toLowerCase();
      var shown = 0;
      rows.forEach(function (row) {
        var hit = needle === "" || row.getAttribute("data-search").indexOf(needle) >= 0;
        row.hidden = !hit;
        if (hit) {
          shown++;
        }
      });
      none.hidden = shown > 0;
      count.textContent =
        needle === ""
          ? countText
          : shown + " of " + rows.length + " devices match \u201c" + devSearch.value.trim() +
            "\u201d. Searches name, IP and tag; the list never grows past this box.";
    };
    drawDevChips();
    devSearch.addEventListener("input", filter);
    hush(devSearch, ["input", "change"]);
    scope.querySelector("[data-scope-list]").addEventListener("change", drawDevChips);
    // tags: a ticked tag is a chip, an unticked one a pill with its count (an unused one is gone)
    var tagChips = scope.querySelector("[data-tag-chips]");
    var tagUsed = scope.querySelector("[data-tag-used]");
    var placeTag = function (label) {
      var box = label.querySelector("input");
      if (box.checked) {
        tagChips.appendChild(label);
      } else if (label.getAttribute("data-n") === "0") {
        label.remove();
      } else {
        tagUsed.appendChild(label);
      }
    };
    var tagLabels = function () {
      return Array.prototype.slice.call(scope.querySelectorAll("label.pol-tag"));
    };
    scope.addEventListener("change", function (e) {
      var label = e.target instanceof Element ? e.target.closest("label.pol-tag") : null;
      if (label) {
        placeTag(label);
      }
    });
    var addTag = function (name) {
      var found = tagLabels().filter(function (l) {
        return l.querySelector("input").value === name;
      })[0];
      if (!found) {
        found = document.createElement("label");
        found.className = "pol-pill pol-tag";
        found.setAttribute("data-n", "0");
        var box = document.createElement("input");
        box.type = "checkbox";
        box.name = "tag";
        box.value = name;
        var text = document.createElement("span");
        text.textContent = name;
        var x = document.createElement("span");
        x.className = "pol-chip-x";
        x.setAttribute("aria-hidden", "true");
        x.textContent = "\u00d7";
        found.append(box, text, x);
        tagChips.appendChild(found);
      }
      var cb = found.querySelector("input");
      if (!cb.checked) {
        cb.checked = true;
        placeTag(found);
        fire(cb);
      }
    };
    // a text field that turns what you type (comma or Enter) into chips; "known" lets a pick from
    // the suggestion list count at once
    var commitField = function (input, add, known) {
      var commit = function () {
        input.value
          .split(",")
          .map(function (v) {
            return v.trim();
          })
          .filter(Boolean)
          .forEach(add);
        input.value = "";
      };
      input.addEventListener("keydown", function (e) {
        if (e.key === "Enter" || e.key === ",") {
          e.preventDefault();
          commit();
        }
      });
      input.addEventListener("change", commit);
      input.addEventListener("input", function (e) {
        if (known && known(input.value.trim()) && !(e instanceof InputEvent && e.inputType !== "insertReplacementText")) {
          commit();
        }
      });
      hush(input, ["input"]);
    };
    commitField(scope.querySelector("[data-tag-add]"), addTag, null);
    // exceptions: devices or tags; each chip is a ticked checkbox (unticking drops it)
    var exChips = scope.querySelector("[data-except-chips]");
    var exNames = Array.prototype.slice.call(scope.querySelectorAll("#scope-names option")).map(function (o) {
      return o.value;
    });
    exChips.addEventListener("change", function (e) {
      var label = e.target instanceof Element ? e.target.closest("label.pol-exc") : null;
      if (label && !e.target.checked) {
        label.remove();
      }
    });
    var addExcept = function (name) {
      var have = exChips.querySelectorAll("input");
      for (var i = 0; i < have.length; i++) {
        if (have[i].value === name) {
          return;
        }
      }
      var label = document.createElement("label");
      label.className = "pol-pill pol-exc";
      var box = document.createElement("input");
      box.type = "checkbox";
      box.name = "except";
      box.value = name;
      box.checked = true;
      var text = document.createElement("span");
      text.textContent = name;
      var x = document.createElement("span");
      x.className = "pol-chip-x";
      x.setAttribute("aria-hidden", "true");
      x.textContent = "\u00d7";
      label.append(box, text, x);
      exChips.appendChild(label);
      fire(box);
    };
    commitField(scope.querySelector("[data-except-add]"), addExcept, function (v) {
      return exNames.indexOf(v) >= 0;
    });
  }
  // New rule: the template search filters the cards as you type. Without this file the search
  // is a plain GET (?q=) that the server answers the same way.
  var search = document.querySelector("[data-tpl-search]");
  if (search) {
    var tplCards = Array.prototype.slice.call(
      document.querySelectorAll("[data-tpl-card]"),
    );
    var tplGroups = Array.prototype.slice.call(
      document.querySelectorAll("[data-tpl-group]"),
    );
    search.addEventListener("input", function () {
      var needle = search.value.trim().toLowerCase();
      tplCards.forEach(function (c) {
        c.hidden =
          needle !== "" && c.textContent.toLowerCase().indexOf(needle) < 0;
      });
      tplGroups.forEach(function (g) {
        g.hidden = !g.querySelector("[data-tpl-card]:not([hidden])");
      });
    });
  }
  // Make compliant preview: ticking an Advisory box changes the plan, so Run is disabled until
  // "Update plan" shows the new one. Without this file the hint beside the boxes says the same.
  var planBoxes = document.querySelectorAll('input[type="checkbox"][form="fix-select"]');
  if (planBoxes.length > 0) {
    var planChanged = function () {
      document.querySelectorAll("[data-plan-run]").forEach(function (b) {
        b.disabled = true;
      });
      document.querySelectorAll("[data-plan-stale]").forEach(function (m) {
        m.hidden = false;
      });
    };
    planBoxes.forEach(function (b) {
      b.addEventListener("change", planChanged);
    });
  }
  // Deviating devices: the boxes of Make compliant. The button counts the ticked rows, "Select all"
  // (shown only with this file) ticks every box, and the filter field narrows the groups as you type.
  // Without this file the button posts what is ticked and the filter is a plain GET (?q=).
  var pickForm = document.getElementById("dev-pick");
  if (pickForm) {
    var pickBoxes = function () {
      return Array.prototype.slice.call(
        pickForm.querySelectorAll('input[type="checkbox"][name="pick"]'),
      );
    };
    var pickSync = function () {
      var boxes = pickBoxes();
      var n = boxes.filter(function (b) {
        return b.checked;
      }).length;
      document.querySelectorAll("[data-pick-label]").forEach(function (c) {
        c.textContent = "Make compliant\u2026 (" + n + " selected)";
      });
      pickForm.querySelectorAll("[data-pick-all]").forEach(function (all) {
        all.checked = boxes.length > 0 && n === boxes.length;
        all.indeterminate = n > 0 && n < boxes.length;
      });
    };
    pickForm.querySelectorAll("[data-pick-all]").forEach(function (all) {
      all.hidden = false;
      all.addEventListener("change", function () {
        pickBoxes().forEach(function (b) {
          b.checked = all.checked;
        });
        pickSync();
      });
    });
    pickForm.addEventListener("change", pickSync);
    pickSync();
    var devFilter = document.querySelector("[data-dev-filter]");
    if (devFilter) {
      devFilter.addEventListener("input", function () {
        var needle = devFilter.value.trim().toLowerCase();
        pickForm.querySelectorAll("[data-dev-group]").forEach(function (g) {
          g.hidden =
            needle !== "" && g.textContent.toLowerCase().indexOf(needle) < 0;
        });
      });
    }
  }
  var list = document.querySelector("[data-rule-list]");
  if (!list) {
    return;
  }
  var form = document.getElementById(list.getAttribute("data-order-form"));
  if (!form) {
    return;
  }
  var dragged = null;
  function cards() {
    return Array.prototype.slice.call(list.querySelectorAll("[data-rule-idx]"));
  }
  list.addEventListener("dragstart", function (e) {
    var c =
      e.target instanceof Element ? e.target.closest("[data-rule-idx]") : null;
    if (!c || !c.draggable) {
      return;
    }
    dragged = c;
    c.classList.add("is-drag");
    if (e.dataTransfer) {
      e.dataTransfer.effectAllowed = "move";
      e.dataTransfer.setData("text/plain", c.getAttribute("data-rule-idx"));
    }
  });
  list.addEventListener("dragover", function (e) {
    if (!dragged) {
      return;
    }
    var over =
      e.target instanceof Element ? e.target.closest("[data-rule-idx]") : null;
    if (!over || over === dragged) {
      return;
    }
    e.preventDefault();
    var box = over.getBoundingClientRect();
    var after = e.clientY > box.top + box.height / 2;
    list.insertBefore(dragged, after ? over.nextSibling : over);
  });
  list.addEventListener("drop", function (e) {
    if (dragged) {
      e.preventDefault();
    }
  });
  list.addEventListener("dragend", function () {
    if (!dragged) {
      return;
    }
    dragged.classList.remove("is-drag");
    dragged = null;
    var order = cards().map(function (c) {
      return c.getAttribute("data-rule-idx");
    });
    var same = order.every(function (v, i) {
      return v === String(i);
    });
    if (same) {
      return;
    }
    form.querySelector('[name="order"]').value = order.join(",");
    form.submit();
  });
})();

// Command field (#269): suggestions from the Tasmota command catalogue and the index box.
// An input with data-cmd-suggest asks the server for matching commands as you type and shows them
// in a listbox (arrow keys, Enter, Escape, mouse); picking one sets the input and fires "change",
// which asks for the value control of that command. For a command with an index range the field is
// a name and a number box; the hidden "command" input is kept in step here, before htmx reads it.
// Without this file the field is a plain text input and the server checks what is saved.
(function () {
  "use strict";
  var timer = null;
  var pending = null;
  var seq = 0;

  function boxFor(input) {
    var box = input.nextElementSibling;
    if (box && box.classList.contains("cmd-sug")) {
      return box;
    }
    box = document.createElement("div");
    box.className = "cmd-sug";
    box.id = "cmd-sug-" + ++seq;
    box.setAttribute("role", "listbox");
    box.setAttribute("aria-label", "Command suggestions");
    box.hidden = true;
    input.parentNode.classList.add("cmd-rel");
    input.parentNode.insertBefore(box, input.nextSibling);
    input.setAttribute("role", "combobox");
    input.setAttribute("aria-autocomplete", "list");
    input.setAttribute("aria-controls", box.id);
    return box;
  }

  function options(box) {
    return Array.prototype.slice.call(box.querySelectorAll("[data-name]"));
  }

  function close(input) {
    var box = boxFor(input);
    box.hidden = true;
    input.setAttribute("aria-expanded", "false");
    input.removeAttribute("aria-activedescendant");
  }

  function activate(input, box, n) {
    var opts = options(box);
    if (opts.length === 0) {
      return;
    }
    n = (n + opts.length) % opts.length;
    opts.forEach(function (o, i) {
      var on = i === n;
      o.classList.toggle("on", on);
      o.setAttribute("aria-selected", on ? "true" : "false");
      if (on) {
        o.id = o.id || box.id + "-o" + i;
        input.setAttribute("aria-activedescendant", o.id);
        o.scrollIntoView({ block: "nearest" });
      }
    });
  }

  function current(box) {
    return options(box).findIndex(function (o) {
      return o.classList.contains("on");
    });
  }

  function pick(input, name) {
    input.value = name;
    close(input);
    input.dispatchEvent(new Event("change", { bubbles: true }));
  }

  function ask(input) {
    var q = input.value.trim();
    var box = boxFor(input);
    if (q === "") {
      close(input);
      return;
    }
    if (pending) {
      pending.abort();
    }
    pending = window.AbortController ? new AbortController() : null;
    fetch(input.getAttribute("data-cmd-suggest") + "?command=" + encodeURIComponent(q), {
      headers: { "HX-Request": "true" },
      credentials: "same-origin",
      signal: pending ? pending.signal : undefined,
    })
      .then(function (r) {
        return r.ok ? r.text() : "";
      })
      .then(function (html) {
        if (html === "" || document.activeElement !== input) {
          return;
        }
        box.innerHTML = html;
        box.hidden = false;
        input.setAttribute("aria-expanded", "true");
        activate(input, box, 0);
      })
      .catch(function () {
        // aborted by a newer keystroke, or offline: the field stays a plain input
      });
  }

  document.addEventListener("input", function (e) {
    var t = e.target;
    if (t && t.hasAttribute && t.hasAttribute("data-cmd-suggest")) {
      clearTimeout(timer);
      timer = setTimeout(function () {
        ask(t);
      }, 150);
    }
  });

  document.addEventListener("keydown", function (e) {
    var t = e.target;
    if (!t || !t.hasAttribute || !t.hasAttribute("data-cmd-suggest")) {
      return;
    }
    var box = boxFor(t);
    if (box.hidden) {
      return;
    }
    var i = current(box);
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      activate(t, box, i + (e.key === "ArrowDown" ? 1 : -1));
    } else if (e.key === "Enter" && i >= 0) {
      e.preventDefault();
      pick(t, options(box)[i].getAttribute("data-name"));
    } else if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      close(t);
    }
  });

  // mousedown, not click: the input must not lose focus before the pick
  document.addEventListener("mousedown", function (e) {
    var o = e.target && e.target.closest ? e.target.closest(".cmd-sug [data-name]") : null;
    if (!o) {
      return;
    }
    e.preventDefault();
    var input = o.closest(".cmd-rel").querySelector("[data-cmd-suggest]");
    if (input) {
      pick(input, o.getAttribute("data-name"));
    }
  });

  document.addEventListener("focusout", function (e) {
    var t = e.target;
    if (t && t.hasAttribute && t.hasAttribute("data-cmd-suggest")) {
      close(t);
    }
  });

  // One change of the command per value: a pick fires "change" itself, and the browser fires another
  // when the field loses focus; the second would ask for the value control again and replace what
  // was typed into the value field in the meantime.
  document.addEventListener(
    "change",
    function (e) {
      var t = e.target;
      if (t && t.hasAttribute && t.hasAttribute("data-cmd-suggest")) {
        if (t.sent === t.value) {
          e.stopPropagation();
          return;
        }
        t.sent = t.value;
      }
    },
    true,
  );

  // The index box: the hidden command is the name and the number, set in the capture phase so the
  // preview and the value-control request (htmx, bubbling) read the new command.
  function syncIndex(e) {
    var t = e.target;
    if (!t || !t.hasAttribute || !t.hasAttribute("data-cmd-base")) {
      return;
    }
    var hidden = document.getElementById("rule-command");
    if (hidden) {
      hidden.value = t.getAttribute("data-cmd-base") + t.value.trim();
    }
  }
  document.addEventListener("input", syncIndex, true);
  document.addEventListener("change", syncIndex, true);
})();
