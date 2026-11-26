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
  // The checkbox column of a table with pixel widths (data-px): the width of its checkbox
  // (tableprefs.go: selectPx).
  var SELECT_PX = 43;
  var SELECT_PCT = 3.5; // leading checkbox column, as on the server
  // A table with pixel widths (data-px, the device list) draws every column at the width the person set,
  // in pixels of content, and nothing else (#828): no frame stretches a column, a fit or a drag changes only
  // the column it acts on, and a frame wider than the table keeps what it has spare in an empty cell before
  // the columns pinned right (SPARE, a column of data-cols that is none of the registry's). Its row menu
  // column is as wide as its button and padding (ACT_PX, tableprefs.go menuPx) and has no saved width.
  var ACT_PX = 48;
  var HANDLE_PX = 12; // the resize handle in a header: what a fit adds to the header's own width (fitWidth, donateRoom)
  var SPARE = "~";
  var desktop = window.matchMedia("(min-width: 45rem)"); // the table (not the phone list) shows from here
  var timers = {};
  var openKey = "";
  // Whether the "Customize order" fold of that menu is open (#1133). Client state only: it ends with the menu (the
  // toggle handler below closes the fold when its menu closes), so a menu always opens on its checkboxes, and a
  // page load starts with it closed, except for the reload a pin, a move or a row drag asks for (reopen).
  var orderOpen = false;
  // The Columns menu a pin reloaded the page for (applyDoc), read once when this page starts. It
  // must not be read later: a page that has asked for its reload keeps running until the new one
  // arrives, and an enhance() of it (a coalesced cell refit, the fonts) used to take the flag, so
  // the page that was going away opened the menu and the new one never did.
  var reopen = "";
  try {
    reopen = window.sessionStorage.getItem("tf-colmenu") || "";
    window.sessionStorage.removeItem("tf-colmenu");
  } catch (e) {
    /* no storage: the menu just stays closed */
  }

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
      if (s === SPARE) {
        return {
          id: SPARE,
          def: 0,
          off: false,
          side: "",
          fix: true,
          spare: true,
        };
      }
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
      if (c[i].id === "" || c[i].spare || hid[i]) {
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

  function pxTable(table) {
    return table.hasAttribute("data-px");
  }
  // the row menu of a table with pixel widths: as wide as its button, never a saved width (#828)
  function isMenu(table, col) {
    return pxTable(table) && col.fix && col.side === "r";
  }

  // The widths of the layout, per visible data column ({px: id -> px}): the widths the browser draws.
  // A table with pixel widths draws every column at its own width (#828: no stretch to the frame, which a
  // save used to store, #805), and its row menu is no width of the layout. A table of shares (no data-px)
  // fills its frame and stores proportions, which the stretch does not change.
  function layoutWidths(table) {
    var px = {};
    visible(table).forEach(function (v) {
      if (!isMenu(table, v)) {
        px[v.id] = v.px;
      }
    });
    return { px: px };
  }

  // Whole pixels that add up to the rounded total: each column rounded on its own let the sum drift by
  // up to half a pixel per column, six pixels in a table of twelve columns (#805).
  function wholePixels(px) {
    var ids = Object.keys(px),
      out = {},
      total = 0,
      sum = 0;
    ids.forEach(function (id) {
      total += px[id];
      out[id] = Math.floor(px[id]);
      sum += out[id];
    });
    ids
      .slice()
      .sort(function (a, b) {
        return px[b] - out[b] - (px[a] - out[a]);
      })
      .slice(0, Math.round(total) - sum)
      .forEach(function (id) {
        out[id]++;
      });
    return out;
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
    var px = pxTable(table),
      fixed = hasSelect(table) ? SELECT_PCT : 0,
      total = 0,
      width = hasSelect(table) ? SELECT_PX : 0;
    var own = function (col) {
      // a weight of the layout; a column that has none yet (just shown) gets its default
      return weight[col.id] > 0 ? weight[col.id] : col.def * (px ? 10 : 1);
    };
    c.forEach(function (col, i) {
      if (col.id !== "" && !col.spare && !hidden[i]) {
        total += own(col);
        // whole pixels, as they are drawn and saved: the server computes the same number from them
        width += isMenu(table, col) ? ACT_PX : Math.round(own(col));
      }
    });
    // a table with pixel widths is as wide as its columns (tableprefs.go: pxTables); the weights are pixels
    if (px) {
      table.setAttribute("data-basew", String(width));
      table.removeAttribute("data-legacy"); // its widths are content widths from now on
    }
    var cg = document.createElement("colgroup");
    c.forEach(function (col, i) {
      if (hidden[i]) {
        return;
      }
      var el = document.createElement("col");
      if (col.spare) {
        el.className = "spare"; // no width: what a wider frame has beyond the columns
      } else if (col.id === "") {
        el.setAttribute("width", px ? String(SELECT_PX) : SELECT_PCT + "%");
      } else {
        el.setAttribute("data-id", col.id);
        el.setAttribute(
          "width",
          isMenu(table, col)
            ? String(ACT_PX)
            : px
              ? String(Math.round(own(col)))
              : ((own(col) / total) * (100 - fixed)).toFixed(2) + "%",
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
    if (pxTable(table)) {
      fitPixels(table, cg);
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
    // The minimum width first, then the pinned columns are measured against it (#806). A table that a
    // list update has just swapped in has none yet and lies at the frame's width: measured there, its
    // pinned shares looked too narrow and got pixel widths, the minimum width then made the table wider,
    // and the next fit (at settle) gave the shares back. Two layouts per update, one painted frame each.
    // Measured at the width it keeps, a second fit finds the same widths as the first.
    var need = 0;
    all.forEach(function (p) {
      var pinnedTo = idx.filter(function (q) {
        return q[0] === p[0];
      })[0];
      need += pinnedTo ? Math.ceil(pinnedTo[2]) : SCROLL_MIN_PX;
    });
    need = Math.max(need, Number(table.getAttribute("data-basew")) || 0);
    table.style.minWidth = need + "px";
    // twice: a column set to pixels takes width from the percentage columns, a pinned one among them too
    for (var pass = 0; pass < 2; pass++) {
      idx.forEach(function (p) {
        if (p[2] > th[p[0]].getBoundingClientRect().width) {
          p[1].setAttribute("width", String(Math.ceil(p[2])));
        }
      });
    }
  }

  // A saved layout of a table with pixel widths (#828): every column at its own width in pixels and the table
  // as wide as its columns; a wider frame gives the room it has spare to the spare cell, before the columns
  // pinned right. A pinned column still shows all of its content (it is the one that stays in view): one that
  // is narrower than its content at its narrowest (min-content) gets that width, the device name at least
  // NAME_MIN_PX, and the row menu is as wide as its button. Every fit starts from the widths the colgroup came
  // with (data-w), never from what an earlier fit made of them, so a second fit changes nothing (#806).
  function fitPixels(table, cg) {
    var c = cols(table),
      hid = hiddenIdx(table),
      kids = cg.children,
      col = [],
      n = 0;
    for (var k = 0; k < c.length; k++) {
      col.push(hid[k] ? null : kids[n++] || null);
    }
    col.forEach(function (el) {
      if (!el) {
        return;
      }
      el.removeAttribute("data-give"); // what donateRoom moved is drawn again by this fit
      if (!el.hasAttribute("data-w")) {
        el.setAttribute("data-w", el.getAttribute("width") || "");
      }
      if (el.getAttribute("data-w")) {
        el.setAttribute("width", el.getAttribute("data-w"));
      }
    });
    if (!desktop.matches) {
      table.style.minWidth = "";
      return;
    }
    if (table.hasAttribute("data-legacy")) {
      convertLegacy(table, c, col);
    }
    var th = headers(table),
      rows = Array.prototype.filter.call(
        table.querySelectorAll(":scope > tbody > tr"),
        function (r) {
          return r.cells.length === c.length;
        },
      ),
      cells = [],
      at = [];
    // all cells of the pinned columns are measured in one go: one layout, not one per cell
    c.forEach(function (cd, i) {
      if (!cd.side || !col[i]) {
        return;
      }
      if (cd.id !== "") {
        cells.push(th[i]);
        at.push(i);
      }
      rows.forEach(function (r) {
        cells.push(r.cells[i]);
        at.push(i);
      });
    });
    var need = {};
    minContents(cells).forEach(function (w, k) {
      need[at[k]] = Math.max(need[at[k]] || 0, w);
    });
    Object.keys(need).forEach(function (i) {
      var cd = c[i],
        w = need[i];
      if (isMenu(table, cd)) {
        w = Math.max(ACT_PX, w - 1); // its button and padding: a button does not wrap, no allowance for it
      } else if (cd.id !== "" && cd.fix && cd.side === "l") {
        w = Math.max(w, NAME_MIN_PX);
      }
      if (Math.ceil(w) > Number(col[i].getAttribute("width"))) {
        col[i].setAttribute("width", String(Math.ceil(w)));
      }
    });
    donateRoom(table, c, col, rows);
    // the table is as wide as its columns (the spare cell has no width of its own)
    var sum = 0;
    col.forEach(function (el, i) {
      if (el && !c[i].spare) {
        sum += Number(el.getAttribute("width")) || 0;
      }
    });
    table.style.minWidth = sum + "px";
  }

  // The columns the person resized in this page (drag, arrow keys, double-click fit, Home), per table key and
  // column id. It starts at that resize and ends with the page: a reload or a new visit starts empty, and it is
  // never stored (the same way `recent` and `resizing` below live in the page script; the pages are full
  // navigations, there is no hx-boost). A column in it is neither given room (donateRoom) nor asked for any, so
  // a drag follows the pointer even where it makes a column narrower than its content (#790).
  var resizedHere = {};
  function markResized(table, id) {
    var key = keyOf(table);
    (resizedHere[key] || (resizedHere[key] = {}))[id] = true;
  }

  // Room for a saved layout's cut cell (#912). A saved width stays what the person chose, with one exception: a
  // column too narrow for its own content (its cells run past their boxes) while other columns have room
  // beyond what their own content needs is not what the person chose, so the room moves. It is taken from
  // the columns that can spare it, down to the width of their content on one line, the measure of the
  // double-click fit (fitWidth: want), so no donor changes a line break. The table keeps its width. Only
  // columns that scroll with the table take part: a pinned column was fitted to its content already, the
  // selection checkbox and the row menu have a width of their own. A column is helped all or nothing, the
  // smallest shortfall first: a cell that is cut on purpose (a long URL, hundreds of pixels short) finds no
  // donor and stays as saved. Every fit starts from the saved widths (data-w), so a second fit changes
  // nothing. The room is drawn, not stored: each <col> a donation touched says by how much (data-give, negative for
  // a donor), and a save stores the widths without it (undonate), so hiding and showing a column, or any other
  // save, does not take room from the donors again each time.
  function donateRoom(table, c, col, rows) {
    var mine = resizedHere[keyOf(table)] || {},
      at = [];
    c.forEach(function (cd, i) {
      if (col[i] && cd.id !== "" && !cd.spare && !cd.side && !mine[cd.id]) {
        at.push(i);
      }
    });
    // one layout for all the cells: a column whose cells all hold within their boxes costs no probe. A cell can
    // seem cut with nothing cut (the Address link's hidden "opens in a new tab" text runs past the box): what a
    // column needs is measured below, and only a column that lacks width is cut.
    var cutAt = at.filter(function (i) {
      return rows.some(function (r) {
        return r.cells[i].scrollWidth > r.cells[i].clientWidth;
      });
    });
    if (!cutAt.length) {
      return;
    }
    var cells = [],
      of = [];
    cutAt.forEach(function (i) {
      rows.forEach(function (r) {
        cells.push(r.cells[i]);
        of.push(i);
      });
    });
    var need = {};
    minContents(cells).forEach(function (w, k) {
      need[of[k]] = Math.max(need[of[k]] || 0, w);
    });
    var lack = cutAt
      .map(function (i) {
        return [i, Math.ceil(need[i]) - Number(col[i].getAttribute("width"))];
      })
      .filter(function (l) {
        return l[1] > 0; // a cell that only seems cut (a hidden or clipped part) asks for nothing
      })
      .sort(function (a, b) {
        return a[1] - b[1];
      });
    if (!lack.length) {
      return;
    }
    // What a donor needs is the measure of the double-click fit (fitWidth: want): its widest content on one line,
    // header and resize handle included. Measured here for all the donors in one go, from the same cached probes
    // as the pinned columns', so a fit that lands on a layout that is cut costs no probe of its own.
    var donors = at.filter(function (i) {
        return !lack.some(function (l) {
          return l[0] === i;
        });
      }),
      th = headers(table),
      probe = [],
      owner = [],
      want = {};
    donors.forEach(function (i) {
      rows.forEach(function (r) {
        probe.push(r.cells[i]);
        owner.push(i);
      });
    });
    var body = probe.length;
    donors.forEach(function (i) {
      probe.push(th[i]);
      owner.push(i);
    });
    maxContents(probe).forEach(function (w, k) {
      want[owner[k]] = Math.max(want[owner[k]] || 0, k < body ? w : w + HANDLE_PX);
    });
    var give = [];
    donors.forEach(function (i) {
      var w = Number(col[i].getAttribute("width")),
        room = w - Math.ceil(want[i]);
      if (room > 0) {
        give.push([i, w, room]);
      }
    });
    lack.forEach(function (l) {
      var spare = give.reduce(function (a, g) {
        return a + g[2];
      }, 0);
      if (spare < l[1]) {
        return;
      }
      takeFromWidest(give, l[1]).forEach(function (t) {
        var g = give.filter(function (x) {
          return x[0] === t[0];
        })[0];
        g[2] -= g[1] - t[1];
        g[1] = t[1];
        moveWidth(col[t[0]], t[1] - Number(col[t[0]].getAttribute("width")));
      });
      moveWidth(col[l[0]], l[1]);
    });
  }

  // A column's width changes by `by` pixels in a donation: drawn now, and noted on its <col> (data-give).
  function moveWidth(el, by) {
    el.setAttribute("width", String(Number(el.getAttribute("width")) + by));
    el.setAttribute("data-give", String((Number(el.getAttribute("data-give")) || 0) + by));
  }

  // The widths of the visible columns without what donateRoom moved: the ones a person set, which a save stores
  // and the next fit donates from again.
  function undonate(table, px) {
    table.querySelectorAll(":scope > colgroup > col[data-give]").forEach(function (el) {
      var id = el.getAttribute("data-id");
      if (id in px) {
        px[id] -= Number(el.getAttribute("data-give"));
      }
    });
    return px;
  }

  // A layout saved before #828 has no content widths (tableprefs.go: tablePref.B): before 0.36.1 the widths a
  // frame had stretched (data-legacy="shares"), in 0.36.1 the widths scaled to the table's scroll floor ("px"):
  // Address came back 86 px wide for 156 px of content. It is laid out here with no column narrower than its
  // content, its floor: the width a fit gives it (the device name: NAME_MIN_PX, it wraps). The next save stores
  // what is drawn. "px" keeps every stored width that is wide enough. "shares" keeps every stored width,
  // raised to its floor, if the frame holds that (the room comes out of the spare room, no column pays for
  // another); else the proportions, at the layout's own total if the frame holds it, else at what the frame
  // holds, never below the floors; a frame that does not hold the floors gets the layout squeezed into it down
  // to SCROLL_MIN_PX a column (as #805 did), and below that the table scrolls. A column pinned right that is no
  // row menu (Power) gets its floor in both. The floors are measured once per page (legacyFloors), so a list
  // reload lays the table out as before.
  var legacyFloors = {};
  function convertLegacy(table, c, col) {
    var key = keyOf(table),
      at = [],
      raw = {},
      told = 0,
      menus = 0;
    c.forEach(function (cd, i) {
      if (!col[i] || cd.id === "" || cd.spare) {
        return;
      }
      if (isMenu(table, cd)) {
        menus++;
        return;
      }
      at.push(i);
      raw[i] = Number(col[i].getAttribute("width")) || cd.def * 10;
      told += raw[i];
    });
    var known = legacyFloors[key] || (legacyFloors[key] = {});
    at.forEach(function (i) {
      if (!(c[i].id in known)) {
        var f = fitWidth(table, i);
        known[c[i].id] =
          c[i].fix && c[i].side === "l"
            ? NAME_MIN_PX
            : Math.ceil(Math.min(f.want, f.cap));
      }
    });
    // A column pinned right (Power) has no handle and no fit: it gets its floor, never a share, so a stored
    // share of the old table cannot outlive the conversion (only Reset could narrow it).
    var floor = {},
      sumF = 0,
      w = {},
      pinned = 0;
    at.forEach(function (i) {
      floor[i] = known[c[i].id];
      if (c[i].side === "r") {
        w[i] = floor[i];
        pinned += floor[i];
        told -= raw[i];
      }
    });
    var all = at;
    at = at.filter(function (i) {
      return c[i].side !== "r";
    });
    at.forEach(function (i) {
      sumF += floor[i];
    });
    var put = function (got) {
      at.forEach(function (i) {
        w[i] = got[i];
      });
    };
    if (table.getAttribute("data-legacy") === "px") {
      at.forEach(function (i) {
        w[i] = Math.max(Math.round(raw[i]), floor[i]);
      });
    } else {
      var frame = table.parentElement ? table.parentElement.clientWidth : 0,
        avail =
          frame - (hasSelect(table) ? SELECT_PX : 0) - menus * ACT_PX - pinned,
        raised = {},
        sumR = 0;
      at.forEach(function (i) {
        raised[i] = Math.max(Math.round(raw[i]), floor[i]);
        sumR += raised[i];
      });
      if (sumR <= avail) {
        // the frame holds every column at its stored width, the narrow ones lifted to their floors: no
        // column gives up width for them, the room they need comes out of the spare room
        put(raised);
      } else if (sumF <= avail) {
        put(
          spread(at, raw, told, floor, Math.min(Math.max(told, sumF), avail)),
        );
      } else {
        sumF = 0;
        at.forEach(function (i) {
          floor[i] = Math.min(floor[i], SCROLL_MIN_PX);
          sumF += floor[i];
        });
        put(spread(at, raw, told, floor, Math.max(avail, sumF)));
      }
    }
    all.forEach(function (i) {
      col[i].setAttribute("width", String(w[i]));
    });
  }
  // Whole pixels for the columns at, in proportion to raw (adding up to total), none below its floor (whole
  // pixels), adding up to the whole pixels of size.
  function spread(at, raw, total, floor, size) {
    var lo = 0,
      hi = size,
      exact = {},
      whole = {},
      got = 0;
    var sum = function (t) {
      var s = 0;
      at.forEach(function (i) {
        s += Math.max(floor[i], total > 0 ? (raw[i] / total) * t : 0);
      });
      return s;
    };
    for (var k = 0; k < 40; k++) {
      var mid = (lo + hi) / 2;
      if (sum(mid) > size) {
        hi = mid;
      } else {
        lo = mid;
      }
    }
    at.forEach(function (i) {
      exact[i] = Math.max(floor[i], total > 0 ? (raw[i] / total) * lo : 0);
      whole[i] = Math.floor(exact[i]);
      got += whole[i];
    });
    at.slice()
      .sort(function (a, b) {
        return exact[b] - whole[b] - (exact[a] - whole[a]);
      })
      .slice(0, Math.max(0, Math.floor(size) - got))
      .forEach(function (i) {
        whole[i]++;
      });
    return whole;
  }

  // The default layout of a table with pixel column widths (no saved layout, so no tfix): a pinned
  // column still shows all of its content. A column whose CSS width is narrower than its content (a
  // device with two relays has two switches in Power) gets the content's width in pixels; the table,
  // fixed layout with a definite width, grows by that much and scrolls inside its frame. A column
  // already wide enough keeps its CSS width (the prototype's), so nothing else changes.
  function fitPinnedPx(table, cg) {
    var c = cols(table),
      kids = cg.children,
      th = headers(table);
    for (var i = 0; i < kids.length; i++) {
      kids[i].style.width = "";
    }
    // The colgroup has no <col> for a column that is off by default (the server drops it), so
    // its children are not indexed like the header cells: line them up (#239).
    var hid = hiddenIdx(table),
      col = [],
      n = 0;
    for (var k = 0; k < c.length; k++) {
      col.push(hid[k] ? null : kids[n++]);
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
      // the row menu is as wide as its button and padding: a button does not wrap, no allowance for it
      var w = isMenu(table, c[i]) ? need[i] - 1 : need[i];
      if (w > th[i].getBoundingClientRect().width) {
        col[i].style.width = Math.ceil(w) + "px";
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
        !c[i].spare &&
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
      if (
        c[j].id === "" ||
        c[j].spare ||
        (c[j].side && !(c[j].fix && c[j].side === "l"))
      ) {
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
      takeFromWidest(give, deficit).forEach(function (t) {
        col[t[0]].style.width = t[1] + "px";
      });
    } else {
      var w2 = th[s].getBoundingClientRect().width;
      col[s].style.width = Math.max(40, Math.floor(w2 - deficit)) + "px";
    }
  }

  // `give` lists [column, width, room] of the columns that can give up room: the widest ones give first, so the
  // narrow ones keep their size, none gives more than its room. Returns [column, new whole width] for each
  // column that gave, adding up to `deficit` (when the rooms hold it). Shared by cutAtPins and donateRoom.
  function takeFromWidest(give, deficit) {
    var out = [];
    give
      .slice()
      .sort(function (a, b) {
        return b[1] - a[1];
      })
      .forEach(function (g) {
        var take = Math.min(g[2], deficit);
        deficit -= take;
        if (take > 0) {
          out.push([g[0], Math.floor(g[1] - take)]);
        }
      });
    return out;
  }

  // widths of cells' content at its narrowest (min-content), in each cell's own font and
  // white-space, plus the cell's padding. The probes are all added before the first is read.
  // A cell whose markup has not changed since it was measured keeps its width (minWidths): the
  // live cells are swapped one by one, and probing every row again for each swap was most of the
  // device list's main-thread time. The cache is dropped when the fonts arrive (other widths).
  var minWidths = new WeakMap();
  var maxWidths = new WeakMap();
  function minContents(cells) {
    return contents(cells, minWidths, false);
  }
  // the same for the content on one line (max-content), without the allowance for a wrap: what a cell
  // that does not wrap needs to show all of it (#828)
  function maxContents(cells) {
    return contents(cells, maxWidths, true);
  }
  function contents(cells, cache, oneLine) {
    var keys = cells.map(function (cell) {
      return cell ? cell.className + "|" + cell.innerHTML : "";
    });
    var probes = cells.map(function (cell, k) {
      if (!cell) {
        return null;
      }
      var known = cache.get(cell);
      if (known && known.html === keys[k]) {
        return null;
      }
      var probe = document.createElement(oneLine ? "span" : "div");
      probe.style.position = "absolute";
      probe.style.visibility = "hidden";
      probe.style.left = "0";
      probe.style.top = "0";
      if (oneLine) {
        probe.style.whiteSpace = "nowrap";
      } else {
        probe.style.width = "min-content";
      }
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
        var known = cells[k] && cache.get(cells[k]);
        return known ? known.w : 0;
      }
      var cs = getComputedStyle(cells[k]);
      var w =
        probe.getBoundingClientRect().width +
        parseFloat(cs.paddingLeft) +
        parseFloat(cs.paddingRight) +
        (oneLine ? 0 : 1);
      cache.set(cells[k], { html: keys[k], w: w });
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
    // the pinned column at the inner edge of each group (a hidden one has no width): it draws that edge
    var inner = { l: 0, r: 0 };
    left.forEach(function (n) {
      if (th[n - 1] && th[n - 1].getBoundingClientRect().width > 0) {
        inner.l = n;
      }
    });
    right.forEach(function (n) {
      if (th[n - 1] && th[n - 1].getBoundingClientRect().width > 0) {
        inner.r = n;
      }
    });
    classes.forEach(function (k) {
      if (/^pe[lr]\d+$/.test(k)) {
        table.classList.remove(k);
      }
    });
    if (inner.l) {
      table.classList.add("pel" + inner.l);
    }
    if (inner.r) {
      table.classList.add("per" + inner.r);
    }
    scrollEdges(table.parentElement);
  }

  // The pinned columns are the ends of the scroll area (#790). A column that runs on under them is cut at
  // their edge like a column at the edge of any box that scrolls, and the edge says so: while columns lie
  // under a pinned group, its inner edge casts a shade over the scrolling side (CSS, classes more-l and
  // more-r on the scroll box), as the cut edge of a dialog or a log box does. Text that ran flush into a
  // pinned column read as covered, half a column. No column width is changed for this: the widths are
  // the person's, and a drag or an arrow key must move exactly the column it acts on.
  function scrollEdges(frame) {
    if (!frame || !frame.classList.contains("tablescroll")) {
      return;
    }
    var rest = frame.scrollWidth - frame.clientWidth - frame.scrollLeft;
    frame.classList.toggle("more-l", frame.scrollLeft > 1);
    frame.classList.toggle("more-r", rest > 1);
  }
  document.addEventListener(
    "scroll",
    function (e) {
      if (e.target instanceof Element) {
        scrollEdges(e.target);
      }
    },
    { capture: true, passive: true },
  );

  // Keyboard focus must not end under a pinned column (WCAG 2.4.11, #1275). The pinned cells are position:sticky and
  // lie over the scroll box, so a cell that has scrolled under them is still inside the box: the browser counts it
  // as visible and does not bring it out. This scrolls the box sideways just far enough that the focused element
  // lies between the pinned groups of its row, its focus ring included. A pinned cell is left alone: a sticky cell is measured at the place
  // it sticks to, so scroll-padding on the box moved the whole table on every Tab onto a pinned cell (#1290).
  // The edges are read from the live cells (the sticky ones of the row), not from a copy of the plN/prN rule. The
  // scroll is instant and sets no state; the scroll listener above updates the more-l/more-r edges. Only keyboard
  // focus (:focus-visible) acts, so a click on a cell that is half in view does not move the table. Below 45rem
  // no cell is sticky and nothing happens. A header is not sticky vertically (position is only for the columns), so
  // "sticky" means "pinned" for header cells too.
  document.addEventListener(
    "focusin",
    function (e) {
      var el = e.target;
      if (!(el instanceof Element) || !el.matches(":focus-visible")) {
        return;
      }
      var cell = el.closest("td, th");
      var row = cell && cell.parentElement;
      var table = row && row.closest("table");
      var frame = table && table.parentElement;
      if (!frame || !frame.classList.contains("tablescroll")) {
        return;
      }
      function pinned(c) {
        return getComputedStyle(c).position === "sticky";
      }
      if (pinned(cell)) {
        return;
      }
      var box = frame.getBoundingClientRect();
      var from = box.left + frame.clientLeft;
      var to = from + frame.clientWidth;
      var left = from;
      var right = to;
      var seen = false;
      Array.prototype.forEach.call(row.children, function (c) {
        if (c === cell) {
          seen = true;
          return;
        }
        if (!pinned(c)) {
          return;
        }
        var r = c.getBoundingClientRect();
        if (r.width === 0) {
          return; // a hidden pinned column
        }
        if (seen) {
          right = Math.min(right, r.left);
        } else {
          left = Math.max(left, r.right);
        }
      });
      // The ring (outline width plus offset) must be clear of the pinned cells as well as the box: read live, so a
      // change of the focus style needs no change here.
      var cs = getComputedStyle(el);
      var ring = Math.max(0, parseFloat(cs.outlineWidth) + parseFloat(cs.outlineOffset) || 0);
      var at = el.getBoundingClientRect();
      var lo = at.left - ring;
      var hi = at.right + ring;
      if (lo >= left && hi <= right) {
        return;
      }
      // Line up the cell itself when it fits between the groups: that keeps its padding and the ring clear, and
      // the text off the pinned divider. A cell wider than the gap lines up the control and its ring.
      var cr = cell.getBoundingClientRect();
      var a = Math.min(cr.left, lo);
      var b = Math.max(cr.right, hi);
      if (b - a > right - left) {
        a = lo;
        b = hi;
      }
      if (a < left) {
        frame.scrollBy({ left: a - left, behavior: "instant" });
      } else if (b > right) {
        // never past the point where its start goes under the left group
        frame.scrollBy({ left: Math.min(b - right, a - left), behavior: "instant" });
      }
    },
    true,
  );

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
    return { hidden: hiddenIdx(table), weight: undonate(table, layoutWidths(table).px) };
  }

  function document_(table) {
    var c = cols(table),
      hid = hiddenIdx(table),
      drawn = undonate(table, layoutWidths(table).px),
      px = pxTable(table) ? wholePixels(drawn) : null,
      sum = 0,
      w = {},
      h = [];
    Object.keys(drawn).forEach(function (id) {
      sum += drawn[id];
    });
    Object.keys(drawn).forEach(function (id) {
      w[id] = px
        ? Math.min(MAX_PCT, Math.max(MIN_PCT, px[id] / 10)) // tenths of the pixels
        : Math.max(MIN_PCT, Math.round((drawn[id] / sum) * 1000) / 10);
    });
    var v = [];
    c.forEach(function (col, i) {
      if (col.id === "" || col.spare) {
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
    var doc = { w: w, h: h, v: v, o: order(table), p: pins(table) };
    if (px) {
      doc.c = true; // content widths, drawn as they are (#828, tableprefs.go); no row menu among them
    }
    return doc;
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
    // Measured now, not when the timer runs: a list update can replace the table inside the
    // 400 ms, and the table this was called for then measures 0 px in every column, which was
    // saved as the minimum weight everywhere (the whole layout lost, #727).
    var doc = document_(table);
    var run = function () {
      post(key, doc).then(function (ok) {
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
  // the column drag in progress ({key, apply(table)}): a swap mid-drag lays the new table out with it
  var resizing = null;
  // beforeSend cannot be cancelled (beforeRequest can, and then no afterRequest follows); the
  // xhr's own loadend fires after the swap for load, error, abort and timeout alike, even when
  // the requesting element has left the page (afterRequest would not reach document then)
  document.addEventListener("htmx:beforeSend", function (e) {
    var xhr = e.detail && e.detail.xhr;
    if (xhr) {
      started.set(xhr, Date.now());
      xhr.addEventListener("loadend", function () {
        started.delete(xhr);
      });
    }
  });
  // A list update replaces the whole table, its scroll frame included, and a new frame starts at the
  // left edge: the columns the person had scrolled to were gone on every device event (#806). The
  // position is kept per table and put back once the new table has its widths (the frame's range).
  var keptScroll = null;
  document.addEventListener("htmx:beforeSwap", function (e) {
    var a = document.activeElement,
      t = e.detail && e.detail.target;
    keptScroll = null;
    if (t instanceof Element) {
      t.querySelectorAll(".tablescroll > table[data-table]").forEach(
        function (table) {
          if (table.parentElement.scrollLeft > 0) {
            keptScroll = keptScroll || { target: t, left: {} };
            keptScroll.left[table.getAttribute("data-table")] =
              table.parentElement.scrollLeft;
          }
        },
      );
    }
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
    var laidOut = [];
    t.querySelectorAll("table[data-table][data-cols]").forEach(
      function (table) {
        var key = keyOf(table),
          rec = recent[key];
        if (resizing && resizing.key === key) {
          // mid-drag: the new table is drawn at the widths being dragged, not the server's until the
          // pointer moves again (#806)
          resizing.apply(table);
          laidOut.push(table);
          return;
        }
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
        laidOut.push(table);
      },
    );
    // A whole table that was swapped in has no pinned-column fit yet. htmx settles ~20 ms after the
    // swap, enhance() fits it then, and until then the browser can paint it: the sticky Power column
    // sat on the Actions summary in every row (#727). Fit it in the swap's own task, before any
    // paint, and only the tables this swap brought (a live cell holds none, so it costs a query).
    t.querySelectorAll("table[data-table][data-cols]").forEach(
      function (table) {
        if (laidOut.indexOf(table) < 0) {
          fitSwapped(table);
        }
      },
    );
    restoreScroll(t);
    restoreHandleFocus();
  });
  // after the fit: the frame's scroll range is the table's width, which the fit sets
  function restoreScroll(t) {
    var kept = keptScroll;
    keptScroll = null;
    if (!kept || kept.target !== t) {
      return;
    }
    t.querySelectorAll(".tablescroll > table[data-table]").forEach(
      function (table) {
        var left = kept.left[table.getAttribute("data-table")];
        if (left) {
          table.parentElement.scrollLeft = left;
        }
      },
    );
  }
  function fitSwapped(table) {
    fitPinned(table);
    stick(table);
    mirror(table);
    refreshHandles(table);
    refreshDrag(table);
  }
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

  // What a fit makes of column n: its widest content on one line (header with the resize handle, every
  // visible cell, icons and badges included), and the cap a fit never goes beyond (FIT_CAP_CH).
  function fitWidth(table, n) {
    var cell = headers(table)[n],
      ths = getComputedStyle(cell),
      cells = [];
    var want = contentWidth(cell, ths) + HANDLE_PX;
    Array.prototype.forEach.call(table.tBodies, function (tb) {
      Array.prototype.forEach.call(tb.rows, function (row) {
        var td = row.cells[n];
        if (td && td.colSpan === 1 && row.cells.length > n) {
          cells.push(td);
        }
      });
    });
    maxContents(cells).forEach(function (w) {
      want = Math.max(want, w);
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
    return { want: want, cap: cap, cells: cells };
  }
  // how far the widest of these cells runs on past its box (0: every cell shows all of its content)
  function overrun(cells) {
    var over = 0;
    cells.forEach(function (td) {
      over = Math.max(over, td.scrollWidth - td.clientWidth);
    });
    return over;
  }

  function fitColumn(table, idx) {
    var vis = visible(table),
      v = vis[idx];
    if (!v || (vis.length < 2 && !pxTable(table))) {
      return;
    }
    var fit = fitWidth(table, v.th.cellIndex),
      snap = takeSnap(table),
      weight = {};
    markResized(table, v.id);
    Object.keys(snap.px).forEach(function (id) {
      weight[id] = snap.px[id];
    });
    var target, limit;
    var apply = function (t) {
      if (pxTable(table)) {
        weight[v.id] = t; // #828: only this column changes; the table grows, or the spare room does
      } else {
        // a table of shares fills its frame: the others give (or take) the difference in proportion
        var total = 0;
        vis.forEach(function (x) {
          total += snap.px[x.id];
        });
        var scale = (total - t) / (total - snap.px[v.id]);
        vis.forEach(function (x) {
          weight[x.id] = x === v ? t : snap.px[x.id] * scale;
        });
      }
      write(table, hiddenIdx(table), weight);
    };
    if (pxTable(table)) {
      limit = Math.ceil(fit.cap);
      target = Math.max(MIN_PX, Math.min(Math.ceil(fit.want), limit));
    } else {
      var total = 0;
      vis.forEach(function (x) {
        total += snap.px[x.id];
      });
      limit = Math.min(fit.cap, total - MIN_PX * (vis.length - 1));
      target = Math.max(MIN_PX, Math.min(fit.want, limit));
    }
    apply(target);
    // The probe is an estimate (fonts, icons and padding differ between systems): check what the real
    // cells now cut off and grow by that, up to the cap. A cell that is a fraction of a pixel short already
    // shows its ellipsis, so nothing is tolerated (#828: 155.9 px for 156 px cut every address). All of it
    // runs before the browser paints, so there is no visible jump.
    for (var pass = 0; pass < 3 && target < limit; pass++) {
      var over = overrun(fit.cells);
      if (over <= 0) {
        break;
      }
      target = Math.min(limit, target + Math.ceil(over));
      apply(target);
    }
    save(table, true);
  }

  // ---- resizing ----

  function setWidths(table, snap, idx, dx) {
    // idx: index in the visible data columns
    var vis = visible(table);
    var a = vis[idx],
      b = vis[idx + 1] || vis[idx - 1],
      weight = {};
    if (a && pxTable(table)) {
      // #828: only this column changes; the table grows and scrolls, or the spare room takes the difference
      Object.keys(snap.px).forEach(function (id) {
        weight[id] = snap.px[id];
      });
      weight[a.id] = Math.max(MIN_PX, snap.px[a.id] + dx);
      markResized(table, a.id);
      write(table, hiddenIdx(table), weight);
      return;
    }
    // a table of shares fills its frame: the neighbour pays for the change
    if (!a || !b) {
      return;
    }
    var d = dx,
      min = MIN_PX;
    var wa = snap.px[a.id] + d,
      wb = snap.px[b.id] - d;
    if (wa < min) {
      wb -= min - wa;
      wa = min;
    }
    if (wb < min) {
      wa -= min - wb;
      wb = min;
    }
    vis.forEach(function (v) {
      weight[v.id] = snap.px[v.id];
    });
    weight[a.id] = wa;
    weight[b.id] = wb;
    write(table, hiddenIdx(table), weight);
  }
  function takeSnap(table) {
    return layoutWidths(table);
  }

  function refreshHandles(table) {
    if (!desktop.matches) {
      return;
    }
    // No handle on the columns pinned right (Power, row actions): they are as wide as their content.
    // In a table of shares the last column has none either (it takes the remaining width); in a table
    // with pixel widths it has one like any other (#828: the room it gives up becomes spare room).
    var all = visible(table),
      last = all.length - 1;
    while (last >= 0 && all[last].side === "r") {
      last--;
    }
    if (pxTable(table)) {
      last++;
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
    var dx = 0;
    var drag = {
      key: keyOf(table),
      apply: function (t) {
        setWidths(t, snap, idx, dx);
      },
    };
    resizing = drag;
    var move = function (ev) {
      if (ev.pointerId === e.pointerId) {
        dx = ev.clientX - x0;
        setWidths(live(), snap, idx, dx);
      }
    };
    var up = function (ev) {
      if (ev.pointerId !== e.pointerId) {
        return;
      }
      if (resizing === drag) {
        resizing = null;
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
        var s = open.querySelector(":scope > summary");
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
    var px = layoutWidths(table).px,
      sumPx = 0,
      sumDef = 0;
    if (pxTable(table)) {
      // #828: its default pixels (the weights are tenths of them); the others keep theirs
      px[v.id] = v.def * 10;
      markResized(table, v.id);
      write(table, hiddenIdx(table), px);
      return;
    }
    vis.forEach(function (x) {
      sumPx += px[x.id];
      sumDef += x.def;
    });
    var weight = {};
    var target = (v.def / sumDef) * sumPx;
    var others = sumPx - px[v.id];
    var scale = (sumPx - target) / others;
    vis.forEach(function (x) {
      weight[x.id] = x === v ? target : px[x.id] * scale;
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

  // A swap of the device list replaces the Columns menu with a closed one, and enhance() opens
  // it again from openKey. The toggle event below sets openKey a task after the menu opened, and
  // a menu that is replaced before that task ran never reports it (the event of a detached
  // element does not reach the document), so the menu stayed shut for good. The open state is
  // therefore read from the page itself just before the swap.
  document.addEventListener("htmx:beforeSwap", function (e) {
    var target = e.detail && e.detail.target;
    var open = document.querySelector(
      "details.colmenu[open]:not([data-static])",
    );
    if (open && target instanceof Element && target.contains(open)) {
      openKey = open.getAttribute("data-key") || "";
      var fold = open.querySelector(".colorder");
      orderOpen = !!fold && fold.open;
    }
  });

  document.addEventListener(
    "toggle",
    function (e) {
      var d = e.target;
      if (d instanceof HTMLDetailsElement && d.classList.contains("colmenu")) {
        var key = d.getAttribute("data-key") || "";
        openKey = d.open ? key : openKey === key ? "" : openKey;
        if (!d.open) {
          // a closed menu forgets its fold: the next opening shows the checkboxes only
          var fold = d.querySelector(".colorder");
          if (fold) {
            fold.open = false;
          }
          orderOpen = false;
        }
      } else if (
        d instanceof HTMLDetailsElement &&
        d.classList.contains("colorder")
      ) {
        orderOpen = d.open;
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
    if (reopen) {
      var menu = document.querySelector(
        'details.colmenu[data-key="' + reopen + '"]',
      );
      if (menu && desktop.matches) {
        menu.open = true;
        openKey = reopen;
        // only the buttons of the fold ask for this reload (a pin, a move, a row drag): it opens with them
        orderOpen = true;
      }
      reopen = "";
    }
    if (openKey) {
      var wrap = document.querySelector(
        'details.colmenu[data-key="' + openKey + '"]',
      );
      if (wrap && !wrap.open) {
        wrap.open = true;
      }
      var fold = wrap && wrap.querySelector(".colorder");
      if (fold && orderOpen && !fold.open) {
        fold.open = true;
      }
    }
  }
  document.addEventListener("DOMContentLoaded", enhance);
  document.body && enhance();
  // widths measured before the fonts are in are wrong (offsets of the pinned columns included)
  if (document.fonts && document.fonts.ready) {
    document.fonts.ready.then(function () {
      minWidths = new WeakMap();
      maxWidths = new WeakMap();
      legacyFloors = {};
      enhance();
    });
  }
  // A swap of a whole table is enhanced at once. The live cells (sse-swap: status, uptime, power,
  // ... of every device) settle one by one, several per device and second while a fleet comes
  // online, and each enhance measures every row: run per cell, that kept the device list's main
  // thread busy for minutes with 60 devices on a slow machine. Their swaps are coalesced into one
  // enhance at most every CELL_REFIT_MS (a throttle, not a debounce, so a steady stream of
  // updates cannot postpone it forever). The live cells of the phone cards (li.dcard) swap just as
  // often and are coalesced too: left out, each of them ran a full enhance of the table.
  var CELL_REFIT_MS = 250,
    refit = 0;
  document.addEventListener("htmx:afterSettle", function (e) {
    var t = e.target;
    if (
      t instanceof Element &&
      !t.querySelector("table[data-table]") &&
      t.closest("table[data-table], li.dcard")
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
