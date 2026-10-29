// Table columns: drag or keyboard resize, double-click fit, show/hide, move,
// pin left or right, reset, and the saving of all that per user (POST
// <base>/prefs/table.<key>). The server renders the saved layout into the HTML
// (tableprefs.go), so nothing here is needed for the first paint. Pinned
// columns (classes plN/prN) stick to their edge when the table scrolls
// sideways; this file measures their offsets (--plN/--prN on the table) and
// keeps a pinned column at least as wide as its content. Also: a title with
// the full text for any value a cell cuts off. No inline script or style: CSP
// forbids both; widths go through the <col width="..%"> attribute, offsets
// through CSS custom properties set from here (CSSOM).
(function () {
  "use strict";

  var MIN_PX = 56; // narrowest a column may be dragged
  var NAME_MIN_PX = 160; // the column fixed left (the device name) never gets narrower (prototype)
  var SCROLL_MIN_PX = 96; // below this per column a table with a saved layout scrolls instead of squeezing
  var MIN_PCT = 3; // the server clamps to the same share
  var MAX_PCT = 90; // and to the same maximum
  var SELECT_PX = 44; // the checkbox column of a table with pixel widths (data-px)
  var SELECT_PCT = 3.5; // leading checkbox column, as on the server
  var desktop = window.matchMedia("(min-width: 45rem)"); // the table (not the phone list) shows from here
  var timers = {};
  var openKey = "";

  function body() {
    return document.body;
  }
  function prefsURL() {
    return body().getAttribute("data-prefs") || "";
  }
  function csrf() {
    try {
      return (
        JSON.parse(body().getAttribute("hx-headers") || "{}")["X-CSRF-Token"] ||
        ""
      );
    } catch (e) {
      return "";
    }
  }

  function notify(text) {
    if (window.tfToast) {
      window.tfToast(text, "danger");
    } // toast.js, loaded on every page
  }

  function post(key, doc) {
    var url = prefsURL();
    if (!url) {
      return Promise.resolve(false);
    }
    return fetch(url + "/" + key, {
      method: "POST",
      credentials: "same-origin",
      keepalive: true,
      headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf() },
      body: JSON.stringify(doc),
    })
      .then(function (r) {
        if (!r.ok) {
          throw new Error("HTTP " + r.status);
        }
        return true;
      })
      .catch(function () {
        notify(
          "Could not save your column settings. They apply until you reload.",
        );
        return false;
      });
  }

  // ---- model, read from the DOM each time (HTMX replaces tables) ----

  // The Columns menu sits in the table's wrapper or in the page's own toolbar.
  function menuOf(el) {
    return el.closest("details.colmenu");
  }
  function tableOf(el) {
    var m = menuOf(el);
    return m
      ? document.querySelector(
          'table[data-table="' + m.getAttribute("data-key") + '"]',
        )
      : null;
  }

  // id:default weight[:off][:l|:r][:fix]: off = hidden until shown, l/r = pinned to that side,
  // fix = pinned for good (cannot move or be unpinned). The selection column ("") is pinned left.
  function cols(table) {
    return (table.getAttribute("data-cols") || "").split(",").map(function (s) {
      var p = s.split(":");
      if (p.length < 2) {
        return { id: s, def: 0, off: false, side: "l", fix: true };
      }
      return {
        id: p[0],
        def: parseFloat(p[1]) || 1,
        off: p.indexOf("off", 2) > 0,
        side: p.indexOf("l", 2) > 0 ? "l" : p.indexOf("r", 2) > 0 ? "r" : "",
        fix: p.indexOf("fix", 2) > 0,
      };
    });
  }
  function hiddenIdx(table) {
    var set = {};
    table.className.split(/\s+/).forEach(function (c) {
      var m = /^hc(\d+)$/.exec(c);
      if (m) {
        set[parseInt(m[1], 10) - 1] = true;
      }
    });
    return set;
  }
  function headers(table) {
    var row = table.tHead && table.tHead.rows[0];
    return row ? Array.prototype.slice.call(row.cells) : [];
  }
  // visible data columns: {i, id, def, th, px}
  function visible(table) {
    var c = cols(table),
      hid = hiddenIdx(table),
      th = headers(table),
      out = [];
    for (var i = 0; i < c.length && i < th.length; i++) {
      if (c[i].id === "" || hid[i]) {
        continue;
      }
      out.push({
        i: i,
        id: c[i].id,
        def: c[i].def,
        side: c[i].side,
        fix: c[i].fix,
        th: th[i],
        px: th[i].getBoundingClientRect().width,
      });
    }
    return out;
  }
  function hasSelect(table) {
    return cols(table).some(function (c) {
      return c.id === "";
    });
  }

  // write: weights (any unit) -> classes, <colgroup>, handles' values
  function write(table, hidden, weight) {
    var c = cols(table);
    table.className = table.className
      .split(/\s+/)
      .filter(function (x) {
        return x && !/^hc\d+$/.test(x);
      })
      .join(" ");
    c.forEach(function (col, i) {
      if (hidden[i]) {
        table.classList.add("hc" + (i + 1));
      }
    });
    table.classList.add("tfix");
    // a cut-off value may fit now (or the other way round): ask again on hover
    table.querySelectorAll("[data-autotitle]").forEach(function (x) {
      x.removeAttribute("data-tip");
      x.removeAttribute("data-autotitle");
    });
    var fixed = hasSelect(table) ? SELECT_PCT : 0;
    var total = 0;
    c.forEach(function (col, i) {
      if (col.id !== "" && !hidden[i]) {
        total += weight[col.id];
      }
    });
    // a table with pixel widths keeps its size: the weights here are pixels (tableprefs.go: pxTables)
    if (table.hasAttribute("data-px")) {
      table.setAttribute(
        "data-basew",
        String(Math.round(total + (hasSelect(table) ? SELECT_PX : 0))),
      );
    }
    var cg = document.createElement("colgroup");
    c.forEach(function (col, i) {
      if (hidden[i]) {
        return;
      }
      var el = document.createElement("col");
      if (col.id === "") {
        el.setAttribute("width", SELECT_PCT + "%");
      } else {
        el.setAttribute("data-id", col.id);
        el.setAttribute(
          "width",
          ((weight[col.id] / total) * (100 - fixed)).toFixed(2) + "%",
        );
      }
      cg.appendChild(el);
    });
    var old = table.querySelector(":scope > colgroup");
    if (old) {
      old.remove();
    }
    table.insertBefore(cg, table.firstChild);
    fitPinned(table);
    stick(table);
    refreshHandles(table);
    mirror(table);
  }

  // A pinned column (the checkbox, Name, Power, row actions, any column the user pinned) must
  // show all of its content: a saved share that is too narrow for it at this window width would
  // cut a button off, and a pinned column is the one that stays in view. Such a column gets the
  // pixel width of its content at its narrowest (min-content: a wrapping name may wrap, a button
  // row may not) instead; the fixed table layout then shares the rest of the width among the
  // percentage columns. The share itself stays in the <col> (data-pct), so a wider window gives
  // the column its share back. The table's minimum width is the sum of what every column needs:
  // below it the table scrolls sideways inside its wrapper and the pinned columns stick.
  function fitPinned(table) {
    var cg = table.querySelector(":scope > colgroup");
    if (!cg) {
      table.style.minWidth = "";
      return;
    }
    if (!table.classList.contains("tfix")) {
      fitPinnedPx(table, cg);
      return;
    }
    var c = cols(table),
      hid = hiddenIdx(table),
      col = cg.children,
      n = 0,
      idx = [],
      all = [];
    for (var i = 0; i < c.length; i++) {
      if (hid[i]) {
        continue;
      }
      if (col[n]) {
        all.push([i, col[n]]);
        if (c[i].side) {
          idx.push([i, col[n]]);
        }
      }
      n++;
    }
    idx.forEach(function (p) {
      var el = p[1];
      if (!el.hasAttribute("data-pct")) {
        el.setAttribute("data-pct", el.getAttribute("width") || "");
      }
      el.setAttribute("width", el.getAttribute("data-pct"));
    });
    if (!desktop.matches) {
      table.style.minWidth = "";
      return;
    }
    var th = headers(table),
      rows = Array.prototype.filter.call(
        table.querySelectorAll(":scope > tbody > tr"),
        function (r) {
          return r.cells.length === c.length;
        },
      );
    // all cells of the pinned columns are measured in one go: one layout, not one per cell
    var cells = [];
    idx.forEach(function (p) {
      if (c[p[0]].id !== "") {
        cells.push(th[p[0]]);
      }
      rows.forEach(function (r) {
        cells.push(r.cells[p[0]]);
      });
    });
    var widths = minContents(cells);
    idx.forEach(function (p) {
      var w = 0;
      cells.forEach(function (cell, k) {
        if (cell && cell.cellIndex === p[0]) {
          w = Math.max(w, widths[k]);
        }
      });
      if (c[p[0]].id !== "" && c[p[0]].fix && c[p[0]].side === "l") {
        w = Math.max(w, NAME_MIN_PX);
      }
      p.push(w);
    });
    // twice: a column set to pixels takes width from the percentage columns, a pinned one among them too
    for (var pass = 0; pass < 2; pass++) {
      idx.forEach(function (p) {
        if (p[2] > th[p[0]].getBoundingClientRect().width) {
          p[1].setAttribute("width", String(Math.ceil(p[2])));
        }
      });
    }
    var need = 0;
    all.forEach(function (p) {
      var pinnedTo = idx.filter(function (q) {
        return q[0] === p[0];
      })[0];
      need += pinnedTo ? Math.ceil(pinnedTo[2]) : SCROLL_MIN_PX;
    });
    need = Math.max(need, Number(table.getAttribute("data-basew")) || 0);
    table.style.minWidth = need + "px";
  }

  // The default layout of a table with pixel column widths (no saved layout, so no tfix): a pinned
  // column still shows all of its content. A column whose CSS width is narrower than its content (a
  // device with two relays has two switches in Power) gets the content's width in pixels; the table,
  // fixed layout with a definite width, grows by that much and scrolls inside its frame. A column
  // already wide enough keeps its CSS width (the prototype's), so nothing else changes.
  function fitPinnedPx(table, cg) {
    var c = cols(table),
      col = cg.children,
      th = headers(table);
    for (var i = 0; i < col.length; i++) {
      col[i].style.width = "";
    }
    if (!desktop.matches) {
      return;
    }
    var rows = Array.prototype.filter.call(
      table.querySelectorAll(":scope > tbody > tr"),
      function (r) {
        return r.cells.length === c.length;
      },
    );
    var cells = [],
      at = [];
    c.forEach(function (cd, i) {
      if (!cd.side || !col[i] || !th[i]) {
        return;
      }
      rows.forEach(function (r) {
        cells.push(r.cells[i]);
        at.push(i);
      });
    });
    var widths = minContents(cells),
      need = {};
    widths.forEach(function (w, k) {
      need[at[k]] = Math.max(need[at[k]] || 0, w);
    });
    Object.keys(need).forEach(function (i) {
      if (need[i] > th[i].getBoundingClientRect().width) {
        col[i].style.width = Math.ceil(need[i]) + "px";
      }
    });
    cutAtPins(table, c, col, th);
  }

  // At scroll 0 the columns pinned right lie over the columns that have not scrolled in yet. One
  // column usually straddles their edge, half visible ("seen 1", an Uptime value touching the
  // switches). It is made whole instead: the wide columns before it give up the width it lacks (down
  // to a floor), so it ends where the pinned columns begin and the next one is wholly behind them.
  // When they have too little to give, the straddler is cut at the edge inside its own cell.
  function cutAtPins(table, c, col, th) {
    var frame = table.parentElement;
    if (
      !frame ||
      table.getBoundingClientRect().width <= frame.clientWidth + 1
    ) {
      return; // everything fits: nothing lies under a pinned column
    }
    var pinW = 0,
      first = -1;
    c.forEach(function (cd, i) {
      if (cd.side === "r" && th[i]) {
        pinW += th[i].getBoundingClientRect().width;
        if (first < 0) {
          first = i;
        }
      }
    });
    if (first < 0) {
      return;
    }
    var edge = frame.clientWidth - pinW,
      x0 = table.getBoundingClientRect().left,
      s = -1;
    for (var i = 0; i < first; i++) {
      var r = th[i].getBoundingClientRect();
      if (
        c[i].side !== "l" &&
        r.left - x0 < edge - 0.5 &&
        r.right - x0 > edge + 0.5
      ) {
        s = i;
        break;
      }
    }
    if (s < 0) {
      return;
    }
    var deficit = th[s].getBoundingClientRect().right - x0 - edge,
      give = [],
      cells = [],
      at = [];
    var rows = Array.prototype.filter.call(
      table.querySelectorAll(":scope > tbody > tr"),
      function (r) {
        return r.cells.length === c.length;
      },
    );
    // what a column can give: its width down to its content at its narrowest (the device name down to
    // the prototype's minimum, it wraps), never its pinned-for-good neighbours' or the checkbox column
    for (var j = 0; j < s; j++) {
      if (c[j].id === "" || (c[j].side && !(c[j].fix && c[j].side === "l"))) {
        continue;
      }
      at.push(j);
      rows.forEach(function (r) {
        cells.push(r.cells[j]);
      });
    }
    var mins = minContents(
      cells.concat(
        at.map(function (j) {
          return th[j];
        }),
      ),
    );
    at.forEach(function (j) {
      var floor = c[j].fix ? NAME_MIN_PX : 0;
      cells.forEach(function (cell, k) {
        if (cell && cell.cellIndex === j) {
          floor = Math.max(floor, mins[k]);
        }
      });
      floor = Math.max(floor, mins[cells.length + at.indexOf(j)]);
      var w = th[j].getBoundingClientRect().width;
      give.push([j, w, Math.max(0, w - Math.ceil(floor))]);
    });
    var slack = give.reduce(function (a, g) {
      return a + g[2];
    }, 0);
    if (slack >= deficit) {
      // the widest columns give first, so the narrow ones keep their size
      give.sort(function (a, b) {
        return b[1] - a[1];
      });
      give.forEach(function (g) {
        var take = Math.min(g[2], deficit);
        deficit -= take;
        if (take > 0) {
          col[g[0]].style.width = Math.floor(g[1] - take) + "px";
        }
      });
    } else {
      var w2 = th[s].getBoundingClientRect().width;
      col[s].style.width = Math.max(40, Math.floor(w2 - deficit)) + "px";
    }
  }

  // widths of cells' content at its narrowest (min-content), in each cell's own font and
  // white-space, plus the cell's padding. The probes are all added before the first is read.
  // A cell whose markup has not changed since it was measured keeps its width (minWidths): the
  // live cells are swapped one by one, and probing every row again for each swap was most of the
  // device list's main-thread time. The cache is dropped when the fonts arrive (other widths).
  var minWidths = new WeakMap();
  function minContents(cells) {
    var keys = cells.map(function (cell) {
      return cell ? cell.className + "|" + cell.innerHTML : "";
    });
    var probes = cells.map(function (cell, k) {
      if (!cell) {
        return null;
      }
      var known = minWidths.get(cell);
      if (known && known.html === keys[k]) {
        return null;
      }
      var probe = document.createElement("div");
      probe.style.position = "absolute";
      probe.style.visibility = "hidden";
      probe.style.left = "0";
      probe.style.top = "0";
      probe.style.width = "min-content";
      Array.prototype.forEach.call(cell.childNodes, function (n) {
        if (n.nodeType === 1 && n.classList.contains("colhandle")) {
          return;
        }
        probe.appendChild(n.cloneNode(true));
      });
      cell.appendChild(probe);
      return probe;
    });
    var out = probes.map(function (probe, k) {
      if (!probe) {
        var known = cells[k] && minWidths.get(cells[k]);
        return known ? known.w : 0;
      }
      var cs = getComputedStyle(cells[k]);
      var w =
        probe.getBoundingClientRect().width +
        parseFloat(cs.paddingLeft) +
        parseFloat(cs.paddingRight) +
        1;
      minWidths.set(cells[k], { html: keys[k], w: w });
      return w;
    });
    probes.forEach(function (probe) {
      if (probe) {
        probe.remove();
      }
    });
    return out;
  }

  // Sticky offsets of the pinned columns: a column pinned left starts where the pinned columns
  // left of it end, one pinned right ends where those right of it begin. The server marks them
  // (plN/prN, N = the column's place); the offsets depend on the rendered widths.
  function stick(table) {
    var th = headers(table);
    var classes = table.className.split(/\s+/);
    var left = [],
      right = [];
    classes.forEach(function (k) {
      var m = /^p([lr])(\d+)$/.exec(k);
      if (m) {
        (m[1] === "l" ? left : right).push(parseInt(m[2], 10));
      }
    });
    left.sort(function (a, b) {
      return a - b;
    });
    right.sort(function (a, b) {
      return b - a;
    });
    var at = 0;
    left.forEach(function (n) {
      table.style.setProperty("--pl" + n, at + "px");
      var cell = th[n - 1];
      at += cell ? cell.getBoundingClientRect().width : 0;
    });
    at = 0;
    right.forEach(function (n) {
      table.style.setProperty("--pr" + n, at + "px");
      var cell = th[n - 1];
      at += cell ? cell.getBoundingClientRect().width : 0;
    });
  }

  // A table that only mirrors this one (data-follows, e.g. the folded older releases) takes
  // its hidden columns and widths, so the two stay lined up while a column is dragged.
  function mirror(table) {
    var key = table.getAttribute("data-table");
    document
      .querySelectorAll('table[data-follows="' + key + '"]')
      .forEach(function (f) {
        f.className = f.className
          .split(/\s+/)
          .filter(function (x) {
            return x && !/^(hc\d+|tfix)$/.test(x);
          })
          .concat(
            table.className.split(/\s+/).filter(function (x) {
              return /^(hc\d+|tfix)$/.test(x);
            }),
          )
          .join(" ");
        var old = f.querySelector(":scope > colgroup");
        if (old) {
          old.remove();
        }
        var cg = table.querySelector(":scope > colgroup");
        if (cg) {
          f.insertBefore(cg.cloneNode(true), f.firstChild);
        }
      });
  }

  function snapshot(table) {
    var hidden = hiddenIdx(table),
      weight = {};
    visible(table).forEach(function (v) {
      weight[v.id] = v.px;
    });
    return { hidden: hidden, weight: weight };
  }

  function document_(table) {
    var c = cols(table),
      hid = hiddenIdx(table),
      vis = visible(table),
      sum = 0,
      w = {},
      h = [];
    vis.forEach(function (v) {
      sum += v.px;
    });
    vis.forEach(function (v) {
      w[v.id] = table.hasAttribute("data-px")
        ? Math.min(MAX_PCT, Math.max(MIN_PCT, Math.round(v.px) / 10)) // tenths of the pixels
        : Math.max(MIN_PCT, Math.round((v.px / sum) * 1000) / 10);
    });
    var v = [];
    c.forEach(function (col, i) {
      if (col.id === "") {
        return;
      }
      if (col.off) {
        if (!hid[i]) {
          v.push(col.id);
        }
      } // shown although hidden by default
      else if (hid[i]) {
        h.push(col.id);
      }
    });
    return { w: w, h: h, v: v, o: order(table), p: pins(table) };
  }

  // movable column ids in the user's order (data-order, the order of the Columns menu; the
  // table groups them by pin side)
  function order(table) {
    var o = table.getAttribute("data-order") || "";
    return o ? o.split(",") : [];
  }

  // the pin side of every movable column ("none" for an unpinned one; the server stores
  // only the sides that differ from the column's default)
  function pins(table) {
    var p = {};
    cols(table).forEach(function (c) {
      if (c.id !== "" && !c.fix) {
        p[c.id] = c.side === "l" ? "left" : c.side === "r" ? "right" : "none";
      }
    });
    return p;
  }

  function keyOf(table) {
    return "table." + table.getAttribute("data-table");
  }

  // The layout last set here, until the server has it. A list update (SSE) that is fetched
  // before the save lands brings the old layout; afterSwap puts this one back.
  var recent = {};
  // when each request still on its way left; a response is judged by its own start, not the
  // latest one's, and a layout is kept while an older request can still bring the old one
  var started = new Map();

  function save(table, now) {
    var key = keyOf(table);
    window.clearTimeout(timers[key]);
    var rec = { snap: snapshot(table), done: 0 };
    recent[key] = rec;
    var run = function () {
      post(key, document_(table)).then(function (ok) {
        rec.done = ok ? Date.now() : Infinity;
      });
    };
    if (now) {
      run();
    } else {
      timers[key] = window.setTimeout(run, 400);
    }
  }

  // A swap replaces the table and its resize handles: keep the layout the user just set
  // (unless the server already had it when the update was requested) and the handle focus.
  var heldHandle = null;
  document.addEventListener("htmx:beforeRequest", function (e) {
    if (e.detail && e.detail.xhr) {
      started.set(e.detail.xhr, Date.now());
    }
  });
  document.addEventListener("htmx:afterRequest", function (e) {
    if (e.detail && e.detail.xhr) {
      started.delete(e.detail.xhr);
    }
  });
  document.addEventListener("htmx:beforeSwap", function (e) {
    var a = document.activeElement,
      t = e.detail && e.detail.target;
    var table =
      a &&
      a.classList &&
      a.classList.contains("colhandle") &&
      t &&
      t.contains(a)
        ? a.closest("table[data-table]")
        : null;
    heldHandle = table
      ? { table: table.getAttribute("data-table"), n: a.getAttribute("data-n") }
      : null;
  });
  document.addEventListener("htmx:afterSwap", function (e) {
    var t = e.detail && e.detail.target;
    if (!t) {
      return;
    }
    var requested = (e.detail.xhr && started.get(e.detail.xhr)) || 0;
    t.querySelectorAll("table[data-table][data-cols]").forEach(
      function (table) {
        var key = keyOf(table),
          rec = recent[key];
        if (!rec) {
          return;
        }
        if (rec.done && rec.done < requested) {
          // this response was requested after the server had the layout: done with it,
          // unless an older request is still out and will bring the old layout
          var older = false;
          started.forEach(function (at, xhr) {
            if (xhr !== e.detail.xhr && at < rec.done) {
              older = true;
            }
          });
          if (!older) {
            delete recent[key];
          }
          return;
        }
        write(table, rec.snap.hidden, rec.snap.weight);
      },
    );
    restoreHandleFocus();
  });
  // The new table has no handles until refreshHandles runs, and until then the focus sits on
  // the page: arrow keys in that gap would go nowhere. So the handles are made and the focus
  // is put back as soon as the content is in (afterSwap), and again once it has settled.
  function restoreHandleFocus() {
    if (!heldHandle) {
      return;
    }
    var table = document.querySelector(
      'table[data-table="' + heldHandle.table + '"]',
    );
    if (!table) {
      return;
    }
    refreshHandles(table);
    var h = table.querySelector('.colhandle[data-n="' + heldHandle.n + '"]');
    if (h) {
      h.focus();
    }
  }
  document.addEventListener("htmx:afterSettle", function () {
    restoreHandleFocus();
    heldHandle = null;
  });

  // ---- autofit: double-click (or Enter on) a handle fits the column to its content ----

  var FIT_CAP_CH = 40; // the widest a fitted column becomes
  var lastDown = { h: null, t: 0 };

  // width of a cell's content on one line, measured out of flow in the cell's own font
  function contentWidth(cell, cs) {
    var probe = document.createElement("span");
    probe.style.position = "absolute";
    probe.style.visibility = "hidden";
    probe.style.whiteSpace = "nowrap";
    probe.style.left = "0";
    probe.style.top = "0";
    Array.prototype.forEach.call(cell.childNodes, function (n) {
      if (n.nodeType === 1 && n.classList.contains("colhandle")) {
        return;
      }
      probe.appendChild(n.cloneNode(true));
    });
    cell.appendChild(probe);
    var w = probe.getBoundingClientRect().width;
    cell.removeChild(probe);
    return w + parseFloat(cs.paddingLeft) + parseFloat(cs.paddingRight);
  }

  function fitColumn(table, idx) {
    var vis = visible(table),
      v = vis[idx];
    if (!v || vis.length < 2) {
      return;
    }
    var cell = v.th,
      n = cell.cellIndex;
    var ths = getComputedStyle(cell);
    var want = contentWidth(cell, ths) + 12; // 12: the resize handle
    Array.prototype.forEach.call(table.tBodies, function (tb) {
      Array.prototype.forEach.call(tb.rows, function (row) {
        var td = row.cells[n];
        if (!td || td.colSpan > 1 || row.cells.length <= n) {
          return;
        }
        want = Math.max(want, contentWidth(td, getComputedStyle(td)));
      });
    });
    var zero = document.createElement("span");
    zero.style.position = "absolute";
    zero.style.visibility = "hidden";
    zero.style.whiteSpace = "nowrap";
    zero.textContent = new Array(FIT_CAP_CH + 1).join("0");
    cell.appendChild(zero);
    var cap =
      zero.getBoundingClientRect().width +
      parseFloat(ths.paddingLeft) +
      parseFloat(ths.paddingRight);
    cell.removeChild(zero);
    var snap = takeSnap(table),
      total = 0;
    vis.forEach(function (x) {
      total += snap.px[x.id];
    });
    var limit = Math.min(cap, total - MIN_PX * (vis.length - 1));
    var target = Math.max(MIN_PX, Math.min(want, limit));
    var apply = function (t) {
      var others = total - snap.px[v.id],
        scale = (total - t) / others,
        weight = {};
      vis.forEach(function (x) {
        weight[x.id] = x === v ? t : snap.px[x.id] * scale;
      });
      write(table, hiddenIdx(table), weight);
    };
    apply(target);
    // The probe is an estimate (fonts, icons and padding differ between systems): check what the real
    // cells now clip and grow by that, up to the cap. All of it runs before the browser paints, so
    // there is no visible jump.
    for (var pass = 0; pass < 3 && target < limit; pass++) {
      var over = 0;
      Array.prototype.forEach.call(table.tBodies, function (tb) {
        Array.prototype.forEach.call(tb.rows, function (row) {
          var td = row.cells[n];
          if (td && td.colSpan === 1 && row.cells.length > n) {
            over = Math.max(over, td.scrollWidth - td.clientWidth);
          }
        });
      });
      if (over <= 1) {
        break;
      }
      target = Math.min(limit, target + Math.ceil(over));
      apply(target);
    }
    save(table, true);
  }

  // ---- resizing ----

  function setWidths(table, snap, idx, dx) {
    // idx: index in the visible data columns; the neighbour pays for the change
    var vis = visible(table);
    var a = vis[idx],
      b = vis[idx + 1] || vis[idx - 1];
    if (!a || !b) {
      return;
    }
    var wa = snap.px[a.id] + dx,
      wb = snap.px[b.id] - dx;
    if (wa < MIN_PX) {
      wb -= MIN_PX - wa;
      wa = MIN_PX;
    }
    if (wb < MIN_PX) {
      wa -= MIN_PX - wb;
      wb = MIN_PX;
    }
    var weight = {};
    vis.forEach(function (v) {
      weight[v.id] = snap.px[v.id];
    });
    weight[a.id] = wa;
    weight[b.id] = wb;
    write(table, hiddenIdx(table), weight);
  }
  function takeSnap(table) {
    var px = {};
    visible(table).forEach(function (v) {
      px[v.id] = v.px;
    });
    return { px: px };
  }

  function refreshHandles(table) {
    if (!desktop.matches) {
      return;
    }
    // No handle on the last column (it takes the remaining width) nor on the columns pinned
    // right (Power, row actions): they are as wide as their content.
    var all = visible(table),
      last = all.length - 1;
    while (last >= 0 && all[last].side === "r") {
      last--;
    }
    var vis = all.filter(function (v, n) {
      return n < last && v.side !== "r";
    });
    headers(table).forEach(function (th) {
      var h = th.querySelector(":scope > .colhandle");
      if (
        h &&
        !vis.some(function (v) {
          return v.th === th;
        })
      ) {
        h.remove();
      }
    });
    vis.forEach(function (v, n) {
      var h = v.th.querySelector(":scope > .colhandle");
      if (!h) {
        h = document.createElement("span");
        h.className = "colhandle";
        h.setAttribute("role", "separator");
        h.setAttribute("aria-orientation", "vertical");
        h.tabIndex = 0;
        v.th.appendChild(h);
      }
      h.setAttribute("aria-label", "Resize column " + v.th.textContent.trim());
      h.setAttribute("aria-valuemin", String(MIN_PX));
      h.setAttribute(
        "aria-valuenow",
        String(Math.round(v.th.getBoundingClientRect().width)),
      );
      h.setAttribute("data-n", String(n));
    });
  }

  document.addEventListener("pointerdown", function (e) {
    var h = e.target instanceof Element ? e.target.closest(".colhandle") : null;
    if (!h || e.button > 0) {
      return;
    }
    var table = h.closest("table[data-table]");
    if (!table) {
      return;
    }
    e.preventDefault();
    var idx = parseInt(h.getAttribute("data-n"), 10),
      x0 = e.clientX,
      snap = takeSnap(table);
    var now = Date.now();
    if (lastDown.h === h && now - lastDown.t < 400) {
      // double click: fit to content
      lastDown = { h: null, t: 0 };
      fitColumn(table, idx);
      return;
    }
    lastDown = { h: h, t: now };
    h.setPointerCapture(e.pointerId);
    h.classList.add("is-drag");
    body().classList.add("is-resizing");
    // A list update can replace the table and this handle mid-drag: the listeners sit on the
    // window (the removed handle gets no more events) and every step works on the live table.
    var name = table.getAttribute("data-table");
    var live = function () {
      return (
        document.querySelector('table[data-table="' + name + '"][data-cols]') ||
        table
      );
    };
    var move = function (ev) {
      if (ev.pointerId === e.pointerId) {
        setWidths(live(), snap, idx, ev.clientX - x0);
      }
    };
    var up = function (ev) {
      if (ev.pointerId !== e.pointerId) {
        return;
      }
      window.removeEventListener("pointermove", move);
      window.removeEventListener("pointerup", up);
      window.removeEventListener("pointercancel", up);
      h.classList.remove("is-drag");
      body().classList.remove("is-resizing");
      // a swap after the last move brought the server's widths: set the dragged ones again
      var t = live();
      setWidths(t, snap, idx, ev.clientX - x0);
      save(t, true);
    };
    window.addEventListener("pointermove", move);
    window.addEventListener("pointerup", up);
    window.addEventListener("pointercancel", up);
  });

  document.addEventListener("keydown", function (e) {
    var h = e.target instanceof Element ? e.target.closest(".colhandle") : null;
    if (h) {
      var table = h.closest("table[data-table]");
      var idx = parseInt(h.getAttribute("data-n"), 10);
      var step = e.shiftKey ? 40 : 10,
        dx = 0;
      if (e.key === "ArrowRight") {
        dx = step;
      } else if (e.key === "ArrowLeft") {
        dx = -step;
      }
      if (dx !== 0 && table) {
        e.preventDefault();
        setWidths(table, takeSnap(table), idx, dx);
        save(table, false);
        h.focus();
      } else if (e.key === "Enter" && table) {
        e.preventDefault();
        fitColumn(table, idx);
        h.focus();
      } else if (e.key === "Home" && table) {
        e.preventDefault();
        resetColumn(table, idx);
        save(table, false);
        h.focus();
      }
      return;
    }
    if (e.key === "Escape") {
      var open = document.querySelector(
        "details.colmenu[open]:not([data-static])",
      );
      if (open) {
        open.open = false;
        var s = open.querySelector("summary");
        if (s) {
          s.focus();
        }
      }
    }
  });

  // Home: this column goes back to its default share of the visible columns
  function resetColumn(table, idx) {
    var vis = visible(table);
    var v = vis[idx];
    if (!v) {
      return;
    }
    var sumPx = 0,
      sumDef = 0;
    vis.forEach(function (x) {
      sumPx += x.px;
      sumDef += x.def;
    });
    var weight = {};
    vis.forEach(function (x) {
      weight[x.id] = x.px;
    });
    var target = (v.def / sumDef) * sumPx;
    var others = sumPx - weight[v.id];
    var scale = (sumPx - target) / others;
    vis.forEach(function (x) {
      weight[x.id] = x === v ? target : x.px * scale;
    });
    write(table, hiddenIdx(table), weight);
  }

  // ---- reorder: move buttons in the Columns menu, drag a header ----
  // The server renders the saved order; a change is saved and the page reloads into it.

  function applyOrder(table, ids, reopen) {
    var doc = document_(table);
    doc.o = ids;
    applyDoc(table, doc, reopen);
  }

  // a change the server renders (order, pins): save, then reload into it, the menu open again
  function applyDoc(table, doc, reopen) {
    post(keyOf(table), doc).then(function (ok) {
      if (!ok) {
        return;
      }
      try {
        if (reopen) {
          window.sessionStorage.setItem(
            "tf-colmenu",
            table.getAttribute("data-table"),
          );
        }
      } catch (e) {
        /* no storage: the menu just stays closed */
      }
      window.location.reload();
    });
  }

  // put id before (or after) target in the list
  function moved(ids, id, target, after) {
    var rest = ids.filter(function (x) {
      return x !== id;
    });
    var at = rest.indexOf(target);
    if (at < 0) {
      return ids;
    }
    rest.splice(after ? at + 1 : at, 0, id);
    return rest;
  }

  function moveBy(table, id, dir) {
    var ids = order(table),
      hid = hiddenIdx(table),
      c = cols(table);
    var hiddenId = {};
    c.forEach(function (col, i) {
      if (hid[i]) {
        hiddenId[col.id] = true;
      }
    });
    var i = ids.indexOf(id),
      j = i + dir;
    while (j >= 0 && j < ids.length && hiddenId[ids[j]]) {
      j += dir;
    } // a hidden column is skipped: the move must show
    if (i < 0 || j < 0 || j >= ids.length) {
      return;
    }
    applyOrder(table, moved(ids, id, ids[j], dir > 0), true);
  }

  var dragging = null;
  function dragHeader(e) {
    return e.target instanceof Element
      ? e.target.closest("th[draggable=true]")
      : null;
  }
  function clearDrop(table) {
    table.querySelectorAll("th.drop-l, th.drop-r").forEach(function (t) {
      t.classList.remove("drop-l", "drop-r");
    });
  }
  document.addEventListener("dragstart", function (e) {
    var th = dragHeader(e);
    if (!th) {
      return;
    }
    if (body().classList.contains("is-resizing")) {
      e.preventDefault();
      return;
    } // a resize drag is no column move
    var table = th.closest("table[data-table]");
    dragging = { table: table, id: cols(table)[th.cellIndex].id };
    e.dataTransfer.effectAllowed = "move";
    e.dataTransfer.setData("text/plain", dragging.id);
  });
  document.addEventListener("dragover", function (e) {
    var th = e.target instanceof Element ? e.target.closest("th") : null;
    if (!dragging || !th || th.closest("table") !== dragging.table) {
      return;
    }
    var col = cols(dragging.table)[th.cellIndex];
    clearDrop(dragging.table);
    if (!col || col.id === "" || col.fix) {
      return;
    }
    e.preventDefault();
    var r = th.getBoundingClientRect();
    th.classList.add(e.clientX < r.left + r.width / 2 ? "drop-l" : "drop-r");
  });
  document.addEventListener("drop", function (e) {
    var th = e.target instanceof Element ? e.target.closest("th") : null;
    if (!dragging || !th || th.closest("table") !== dragging.table) {
      return;
    }
    var table = dragging.table,
      col = cols(table)[th.cellIndex],
      id = dragging.id;
    var r = th.getBoundingClientRect(),
      after = e.clientX >= r.left + r.width / 2;
    clearDrop(table);
    dragging = null;
    if (!col || col.id === "" || col.fix || col.id === id) {
      return;
    }
    e.preventDefault();
    applyOrder(table, moved(order(table), id, col.id, after), false);
  });
  document.addEventListener("dragend", function () {
    if (dragging) {
      clearDrop(dragging.table);
    }
    dragging = null;
  });

  // headers of movable columns can be dragged (wide screens only, like the resize handles)
  function refreshDrag(table) {
    var c = cols(table),
      th = headers(table);
    th.forEach(function (cell, i) {
      var col = c[i];
      var can = desktop.matches && col && col.id !== "" && !col.fix;
      if (can) {
        cell.setAttribute("draggable", "true");
        cell.querySelectorAll("a").forEach(function (a) {
          a.setAttribute("draggable", "false");
        });
      } else {
        cell.removeAttribute("draggable");
      }
    });
  }

  // ---- the Columns menu: drag a row by its grip (⋮⋮) to move the column ----

  var rowDrag = null;
  function colRow(e) {
    return e.target instanceof Element
      ? e.target.closest(".colrow[data-colrow]")
      : null;
  }
  function clearRowDrop(panel) {
    if (panel) {
      panel.querySelectorAll(".drop-t, .drop-b").forEach(function (r) {
        r.classList.remove("drop-t", "drop-b");
      });
    }
  }
  document.addEventListener("dragstart", function (e) {
    var row = colRow(e);
    if (!row) {
      return;
    }
    rowDrag = { table: tableOf(row), id: row.getAttribute("data-colrow") };
    e.dataTransfer.effectAllowed = "move";
    e.dataTransfer.setData("text/plain", rowDrag.id);
  });
  document.addEventListener("dragover", function (e) {
    var row = colRow(e);
    if (!rowDrag || !row || tableOf(row) !== rowDrag.table) {
      return;
    }
    e.preventDefault();
    clearRowDrop(row.parentNode);
    var r = row.getBoundingClientRect();
    row.classList.add(e.clientY < r.top + r.height / 2 ? "drop-t" : "drop-b");
  });
  document.addEventListener("drop", function (e) {
    var row = colRow(e);
    if (!rowDrag || !row || tableOf(row) !== rowDrag.table) {
      return;
    }
    e.preventDefault();
    var d = rowDrag,
      r = row.getBoundingClientRect(),
      target = row.getAttribute("data-colrow");
    rowDrag = null;
    clearRowDrop(row.parentNode);
    if (target !== d.id) {
      applyOrder(
        d.table,
        moved(order(d.table), d.id, target, e.clientY >= r.top + r.height / 2),
        true,
      );
    }
  });
  document.addEventListener("dragend", function () {
    if (rowDrag) {
      var m = document.querySelector(
        'details.colmenu[data-key="' +
          rowDrag.table.getAttribute("data-table") +
          '"]',
      );
      clearRowDrop(m);
    }
    rowDrag = null;
  });

  // ---- row menus inside a table that scrolls sideways ----
  // A scroll container clips what hangs out of it, so an opened row menu would be cut off at
  // the table's edge (v0.10.1: a menu must never be clipped). An open row menu inside a table
  // is therefore placed with position: fixed at its button, opening upward when there is no
  // room below, and follows the button while the page or the table scrolls. Without this file
  // the wrapper stops clipping while a menu is open (CSS: .tablescroll:not(.is-js)).
  function placeMenu(d) {
    var s = d.querySelector(":scope > summary"),
      b = d.querySelector(":scope > .rowmenu-body");
    if (!s || !b) {
      return;
    }
    var r = s.getBoundingClientRect();
    b.style.position = "fixed";
    b.style.left = "auto";
    b.style.right =
      Math.max(0, document.documentElement.clientWidth - r.right) + "px";
    var h = b.getBoundingClientRect().height,
      below = window.innerHeight - r.bottom;
    if (below < h + 8 && r.top > below) {
      b.style.top = "auto";
      b.style.bottom = window.innerHeight - r.top + 4 + "px";
    } else {
      b.style.bottom = "auto";
      b.style.top = r.bottom + 4 + "px";
    }
  }
  function releaseMenu(d) {
    var b = d.querySelector(":scope > .rowmenu-body");
    if (b) {
      ["position", "left", "right", "top", "bottom"].forEach(function (k) {
        b.style[k] = "";
      });
    }
  }
  // The open attribute is watched (not the toggle event, which comes a task later): the menu is
  // in place before anything can look at it.
  new MutationObserver(function (recs) {
    recs.forEach(function (r) {
      var d = r.target;
      if (
        d instanceof HTMLDetailsElement &&
        d.classList.contains("rowmenu") &&
        d.isConnected &&
        d.closest(".tablescroll")
      ) {
        if (d.open) {
          placeMenu(d);
        } else {
          releaseMenu(d);
        }
      }
    });
  }).observe(document.documentElement, {
    subtree: true,
    attributes: true,
    attributeFilter: ["open"],
  });
  function followMenus() {
    document
      .querySelectorAll(".tablescroll details.rowmenu[open]")
      .forEach(placeMenu);
  }
  window.addEventListener("scroll", followMenus, true);

  // ---- Pin left / Pin right in the Columns menu ----
  // A pressed button unpins the column; the other one moves it to that side. The server renders
  // the groups, so the change is saved and the page reloads into it with the menu open.
  document.addEventListener("click", function (e) {
    var b = e.target instanceof Element ? e.target.closest("[data-pin]") : null;
    if (!b) {
      return;
    }
    var table = tableOf(b);
    if (!table) {
      return;
    }
    var doc = document_(table);
    doc.p[b.getAttribute("data-pinid")] =
      b.getAttribute("aria-pressed") === "true"
        ? "none"
        : b.getAttribute("data-pin");
    applyDoc(table, doc, true);
  });

  // ---- show/hide and reset ----

  document.addEventListener("change", function (e) {
    var cb = e.target instanceof HTMLInputElement ? e.target : null;
    if (!cb || !cb.hasAttribute("data-colid")) {
      return;
    }
    var table = tableOf(cb);
    var c = cols(table),
      hidden = {},
      weight = {};
    var snap = snapshot(table);
    var vis = Object.keys(snap.weight),
      mean = 0;
    vis.forEach(function (id) {
      mean += snap.weight[id];
    });
    mean = vis.length ? mean / vis.length : 100;
    c.forEach(function (col, i) {
      if (col.id === "") {
        return;
      }
      var box = menuOf(cb).querySelector('[data-colid="' + col.id + '"]');
      hidden[i] = !!box && !box.checked;
      if (!hidden[i]) {
        weight[col.id] = snap.weight[col.id] || mean;
      }
    });
    write(table, hidden, weight);
    save(table, true);
  });

  document.addEventListener("click", function (e) {
    var t = e.target instanceof Element ? e.target : null;
    if (!t) {
      return;
    }
    var reset = t.closest("[data-cols-reset]");
    if (reset) {
      var table = tableOf(reset);
      post(keyOf(table), {}).then(function (ok) {
        if (ok) {
          window.location.reload();
        }
      });
      return;
    }
    var mv = t.closest("[data-move]");
    if (mv) {
      if (!mv.disabled) {
        moveBy(
          tableOf(mv),
          mv.getAttribute("data-moveid"),
          mv.getAttribute("data-move") === "up" ? -1 : 1,
        );
      }
      return;
    }
    // the page size links: remember the choice, then follow the link
    var size = t.closest(".pager-size a");
    if (size) {
      var m = /[?&]per=(\d+)/.exec(size.getAttribute("href") || "");
      var n = m ? parseInt(m[1], 10) : 0;
      if (n) {
        post("pager", { size: n });
      } else {
        post("pager", { size: parseInt(size.textContent, 10) });
      }
    }
    // close a Columns menu on a click elsewhere
    document
      .querySelectorAll("details.colmenu[open]:not([data-static])")
      .forEach(function (d) {
        if (!d.contains(t)) {
          d.open = false;
        }
      });
  });

  document.addEventListener(
    "toggle",
    function (e) {
      var d = e.target;
      if (d instanceof HTMLDetailsElement && d.classList.contains("colmenu")) {
        var key = d.getAttribute("data-key") || "";
        openKey = d.open ? key : openKey === key ? "" : openKey;
      }
    },
    true,
  );

  // ---- tooltips for cut-off values ----

  function cutOff(el) {
    return (
      el.scrollWidth > el.clientWidth + 1 ||
      el.scrollHeight > el.clientHeight + 1
    );
  }
  // Only a table cell (or an element inside one) gets the full text of a
  // cut-off value, never a container: a wrapper around the whole page would
  // otherwise get the page text. The text goes into data-tip for the tooltip
  // component (no native title, so the browser shows no second bubble).
  var TIP_MAX = 160;
  // Never on a cell that holds controls or a menu (a bubble with the run-together text of a
  // button row is noise, and it covered the open menu), never while a menu is open, and only
  // the text of a leaf: the element that is itself cut off.
  var CONTROLS =
    "button, details, summary, input, select, textarea, form, [role=menu]";
  function autoTitle(start) {
    if (!(start instanceof Element)) {
      return;
    }
    var cell = start.closest("td, th");
    if (!cell) {
      return;
    }
    if (
      document.querySelector(
        "details.rowmenu[open]:not([data-static]), details.colmenu[open]:not([data-static]), details.navgroup[open]:not([data-static])",
      )
    ) {
      return;
    }
    var el = start;
    for (var depth = 0; el && depth < 5; depth++) {
      if (el.title || el.hasAttribute("data-tip")) {
        return;
      }
      if (
        el.querySelector("table, td, th") ||
        el.matches(CONTROLS) ||
        el.closest("details.rowmenu") ||
        el.querySelector(CONTROLS)
      ) {
        return;
      }
      if (cutOff(el)) {
        var text = (el.textContent || "").replace(/\s+/g, " ").trim();
        if (text.length > TIP_MAX) {
          text = text.slice(0, TIP_MAX - 1) + "\u2026";
        }
        if (text) {
          el.setAttribute("data-tip", text);
          el.setAttribute("data-autotitle", "");
        }
        return;
      }
      if (el === cell) {
        return;
      }
      el = el.parentElement;
    }
  }
  // Capture phase: runs before the tooltip component's bubble listener.
  document.addEventListener(
    "pointerover",
    function (e) {
      autoTitle(e.target);
    },
    true,
  );
  document.addEventListener(
    "focusin",
    function (e) {
      autoTitle(e.target);
    },
    true,
  );

  // ---- setup ----

  // A cell with nothing in it prints no "Label:" line on a phone card (CSS hides td.none there).
  function markEmptyCells() {
    document
      .querySelectorAll("td[data-label]:not(.none)")
      .forEach(function (td) {
        if (
          !td.textContent.trim() &&
          !td.querySelector(
            "input, button, a, select, textarea, details, img, svg, .badge, form",
          )
        ) {
          td.classList.add("none");
        }
      });
  }

  function enhance() {
    markEmptyCells();
    document
      .querySelectorAll("table[data-table][data-cols]")
      .forEach(function (t) {
        fitPinned(t);
        stick(t);
        mirror(t);
        refreshHandles(t);
        refreshDrag(t);
      });
    document.querySelectorAll(".tablescroll").forEach(function (w) {
      w.classList.add("is-js"); // row menus are placed by followMenus, the wrapper may clip
    });
    try {
      var reopen = window.sessionStorage.getItem("tf-colmenu");
      if (reopen) {
        window.sessionStorage.removeItem("tf-colmenu");
        var menu = document.querySelector(
          'details.colmenu[data-key="' + reopen + '"]',
        );
        if (menu && desktop.matches) {
          menu.open = true;
        }
      }
    } catch (e) {
      /* no storage */
    }
    if (openKey) {
      var wrap = document.querySelector(
        'details.colmenu[data-key="' + openKey + '"]',
      );
      if (wrap && !wrap.open) {
        wrap.open = true;
      }
    }
  }
  document.addEventListener("DOMContentLoaded", enhance);
  document.body && enhance();
  // widths measured before the fonts are in are wrong (offsets of the pinned columns included)
  if (document.fonts && document.fonts.ready) {
    document.fonts.ready.then(function () {
      minWidths = new WeakMap();
      enhance();
    });
  }
  // A swap of a whole table is enhanced at once. The live cells (sse-swap: status, uptime, power,
  // ... of every device) settle one by one, several per device and second while a fleet comes
  // online, and each enhance measures every row: run per cell, that kept the device list's main
  // thread busy for minutes with 60 devices on a slow machine. Their swaps are coalesced into one
  // enhance at most every CELL_REFIT_MS (a throttle, not a debounce, so a steady stream of
  // updates cannot postpone it forever).
  var CELL_REFIT_MS = 250,
    refit = 0;
  document.addEventListener("htmx:afterSettle", function (e) {
    var t = e.target;
    if (
      t instanceof Element &&
      !t.querySelector("table[data-table]") &&
      t.closest("table[data-table]")
    ) {
      if (!refit) {
        refit = window.setTimeout(function () {
          refit = 0;
          enhance();
        }, CELL_REFIT_MS);
      }
      return;
    }
    enhance();
  });
  if (desktop.addEventListener) {
    desktop.addEventListener("change", enhance);
  }
  var fitting = 0;
  window.addEventListener("resize", function () {
    if (fitting) {
      return;
    }
    fitting = window.requestAnimationFrame(function () {
      fitting = 0;
      document
        .querySelectorAll("table[data-table][data-cols]")
        .forEach(function (t) {
          fitPinned(t);
          stick(t);
          mirror(t);
        });
      followMenus();
    });
  });
})();
