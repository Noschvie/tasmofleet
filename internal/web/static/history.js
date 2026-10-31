// Command history for the console prompt and the bulk "Send command" box (#321): Arrow Up and
// Arrow Down walk through the last 50 commands, newest first, as in a shell. The list lives in
// this browser's localStorage and is shared by every device's console and by the bulk box.
// The server decides what may be kept: a page that just sent a harmless command carries it in
// data-history-add; a command with a secret or one the policy refused is never put there.
// Without storage (or without this file) the fields work as before. No inline script: CSP.
(function () {
  "use strict";
  var KEY = "tasmofleet.commandHistory";
  var MAX = 50;

  function read() {
    try {
      var list = JSON.parse(window.localStorage.getItem(KEY) || "[]");
      return Array.isArray(list)
        ? list.filter(function (s) {
            return typeof s === "string" && s !== "";
          })
        : [];
    } catch (err) {
      return [];
    }
  }
  function write(list) {
    try {
      if (list.length === 0) {
        window.localStorage.removeItem(KEY);
      } else {
        window.localStorage.setItem(KEY, JSON.stringify(list));
      }
    } catch (err) {
      // private window or blocked storage: the history just stays empty
    }
  }
  function add(cmd) {
    var list = read().filter(function (s) {
      return s !== cmd;
    });
    list.unshift(cmd);
    write(list.slice(0, MAX));
  }

  var added = document.querySelector("[data-history-add]");
  if (added) {
    add(added.getAttribute("data-history-add"));
  }

  document.querySelectorAll("[data-history-clear]").forEach(function (b) {
    b.hidden = false;
    b.addEventListener("click", function (e) {
      e.preventDefault(); // the bulk box's link is an anchor
      write([]);
    });
  });

  document.querySelectorAll("[data-history-input]").forEach(function (field) {
    var multi = field.tagName === "TEXTAREA";
    var pos = -1; // -1: not recalling; otherwise the index in the list
    var draft = "";
    // Editing a recalled line makes it your own text: the next Arrow Up starts from the newest again.
    field.addEventListener("input", function () {
      pos = -1;
    });
    function show(text) {
      field.value = text;
      field.setSelectionRange(text.length, text.length);
    }
    field.addEventListener("keydown", function (e) {
      if (e.altKey || e.ctrlKey || e.metaKey || e.shiftKey) {
        return;
      }
      if (e.key === "Escape") {
        if (pos >= 0) {
          pos = -1;
          show(draft);
          e.preventDefault();
        }
        return;
      }
      if (e.key !== "ArrowUp" && e.key !== "ArrowDown") {
        return;
      }
      // In the multi-line box the arrows move the caret between lines; recall only at its edge.
      if (multi) {
        var caret = field.selectionStart;
        if (e.key === "ArrowUp" && field.value.slice(0, caret).indexOf("\n") >= 0) {
          return;
        }
        if (e.key === "ArrowDown" && field.value.slice(caret).indexOf("\n") >= 0) {
          return;
        }
      }
      var list = read();
      if (e.key === "ArrowUp") {
        if (list.length === 0 || pos >= list.length - 1) {
          e.preventDefault();
          return;
        }
        if (pos < 0) {
          draft = field.value;
        }
        pos++;
        show(list[pos]);
        e.preventDefault();
      } else if (pos >= 0) {
        pos--;
        show(pos < 0 ? draft : list[pos]);
        e.preventDefault();
      }
    });
  });
})();
