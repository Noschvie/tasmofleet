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
