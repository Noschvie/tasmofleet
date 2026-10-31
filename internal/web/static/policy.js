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
    // exceptions: devices or tags; each chip carries its own hidden field
    var exChips = scope.querySelector("[data-except-chips]");
    var exNames = Array.prototype.slice.call(scope.querySelectorAll("#scope-names option")).map(function (o) {
      return o.value;
    });
    var bindExRemove = function (chip) {
      chip.querySelector("[data-chip-remove]").addEventListener("click", function () {
        chip.remove();
        fire(exChips);
      });
    };
    Array.prototype.forEach.call(exChips.children, bindExRemove);
    var addExcept = function (name) {
      var have = exChips.querySelectorAll('input[type="hidden"]');
      for (var i = 0; i < have.length; i++) {
        if (have[i].value === name) {
          return;
        }
      }
      var chip = document.createElement("span");
      chip.className = "pol-chip pol-chip-ex";
      var h = document.createElement("input");
      h.type = "hidden";
      h.name = "except";
      h.value = name;
      var x = chipX("Remove exception " + name);
      x.setAttribute("data-chip-remove", "");
      chip.append(h, document.createTextNode(name), x);
      exChips.appendChild(chip);
      bindExRemove(chip);
      fire(exChips);
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
