// Timestamps in the browser's locale and time zone. The server renders
// <time datetime="RFC 3339" data-ts="KEY">fallback text</time>; the fallback is in the server's
// zone and is what shows without JavaScript. Here the same instant is formatted with
// Intl.DateTimeFormat. KEY picks the fields (same as the fallback's, see ui/stamp.go). An
// element that carries a title gets the full date, time and zone name as its title.
// Until paint() has handled an element, app.css keeps it hidden (data-painted), and reveals the server's text after
// one second if this script never runs; noscript.css shows it at once without JavaScript (#635).
// No inline script: CSP forbids it.
(function () {
  "use strict";
  // One table of every key (ui/stamp.go has the same keys, stamp_test.go compares them). A value is Intl
  // options, or a function of the instant for a key with its own rule. Every visible date is one of:
  //   date       "20 Nov 2026"
  //   date-time  "20 Nov 2026, 14:17"
  //   month-day  "20 Nov"
  //   relative   "Today 14:17", "Yesterday 21:10", else the date-time (this year without the year)
  //   time       "14:17", a clock time on a line that already names the day
  // The browser's locale decides the order and 12 or 24 hours; the hour has no leading zero, the minute
  // always two digits. clock-24 (a log column) and the future start of a scheduled update keep their own
  // shape; title only sets the tooltip.
  var DATE = { day: "numeric", month: "short", year: "numeric" };
  var HM = { hour: "numeric", minute: "2-digit" };
  var SPECS = {
    date: DATE,
    "date-time": Object.assign({}, DATE, HM),
    "month-day": { month: "short", day: "numeric" },
    relative: relativeText,
    time: HM,
    // a log column: 24-hour like the device's own console, and no AM/PM to widen the fixed column
    "clock-24": { hourCycle: "h23", hour: "2-digit", minute: "2-digit", second: "2-digit" },
    // a scheduled update's start (#604): "Sat 3:00 AM" and the sentence "Sat, Nov 7 at 3:00 AM"
    "weekday-time": Object.assign({ weekday: "short" }, HM),
    start: startText,
    // the tooltip only: the text stays (see FULL)
    title: null,
  };
  // the tooltip of an element that carries a title: the full instant with seconds and the zone's name
  var FULL = { year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit", timeZoneName: "short" };
  var cache = {};
  function fmt(key, spec) {
    if (!cache[key]) cache[key] = new Intl.DateTimeFormat(undefined, spec);
    return cache[key];
  }
  // relative: "Today 09:41", "Yesterday 21:10", else "19 Aug, 20:02" ("19 Aug 2025, 20:02" in another
  // year). Today and Yesterday are the calendar days of the browser's zone, now; the same rule as
  // ui.StampRelativeText, which is the server's fallback.
  function relativeText(d) {
    var now = new Date();
    var days = Math.round((new Date(now.getFullYear(), now.getMonth(), now.getDate()) -
      new Date(d.getFullYear(), d.getMonth(), d.getDate())) / 86400000);
    // The clock time is bound with no-break spaces: "10:17 PM" must not wrap before the meridiem.
    var clock = fmt("time", HM).format(d);
    var hm = clock.replace(/\s/g, "\u00a0");
    if (days === 0) return "Today " + hm;
    if (days === 1) return "Yesterday " + hm;
    var sameYear = d.getFullYear() === now.getFullYear();
    var t = fmt(sameYear ? "relative-year" : "date-time", sameYear
      ? Object.assign({ day: "numeric", month: "short" }, HM)
      : SPECS["date-time"]).format(d);
    // only the clock time at the end is bound: the date part may still wrap
    return t.slice(-clock.length) === clock ? t.slice(0, -clock.length) + hm : t;
  }
  // start: "Sat, Nov 7 at 3:00 AM", the start of a scheduled update in a sentence (#604); the server's fallback is
  // ui.StampStart.
  function startText(d) {
    return fmt("start-day", { weekday: "short", month: "short", day: "numeric" }).format(d) + " at " + fmt("time", HM).format(d);
  }
  // textOf is the text for a format key and an instant ("" when the key is unknown). "title"
  // leaves the text alone, so it is "" here and handled by the caller.
  function textOf(key, d) {
    var spec = Object.prototype.hasOwnProperty.call(SPECS, key) ? SPECS[key] : null;
    if (!spec) return "";
    return typeof spec === "function" ? spec(d) : fmt(key, spec).format(d);
  }
  function paint(root) {
    root = root || document;
    var els = Array.prototype.slice.call(root.querySelectorAll("time[data-ts]"));
    if (root.matches && root.matches("time[data-ts]")) els.push(root); // a swapped <time> itself
    els.forEach(function (el) {
      // handled, whatever comes out: app.css hides a time until this mark is set, so that the server's text is
      // never drawn on a full page load while this script is still to run (#635). The mark is also set where the
      // server's text stays (unknown key or date, unsupported locale or zone): that text then shows.
      el.setAttribute("data-painted", "");
      var key = el.getAttribute("data-ts");
      var d = new Date(el.getAttribute("datetime"));
      if (isNaN(d.getTime())) return; // keep the server's text
      try {
        if (key !== "title") {
          var text = textOf(key, d);
          if (!text) return;
          el.textContent = text;
        }
        if (el.hasAttribute("title")) el.title = fmt("full", FULL).format(d);
      } catch (err) {
        // an unsupported locale or zone: the server's text stays
      }
    });
  }
  // formatFor is the same text for code that builds a sentence (dialog.js); "" when unknown.
  window.tasmofleetFormatTime = function (iso, key) {
    var d = new Date(iso);
    try {
      return isNaN(d.getTime()) ? "" : textOf(key, d);
    } catch (err) {
      return "";
    }
  };
  window.tasmofleetPaintTimes = paint;
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", function () { paint(); });
  } else {
    paint();
  }
  // A swap brings the server's fallback text again. Repaint it as soon as htmx has put the content in
  // (afterSwap, and oobAfterSwap for out-of-band parts), in the same task and so before the browser draws a
  // frame: htmx:afterSettle comes ~20 ms later, and the fallback was visible in between (#594). The
  // event's target is the swapped element (the new content, or the target of an innerHTML swap); only that
  // is painted, as the live device list swaps single cells many times a second.
  function paintSwapped(e) {
    var t = e.target;
    paint(t instanceof Element && t.isConnected ? t : document);
  }
  document.body.addEventListener("htmx:afterSwap", paintSwapped);
  // htmx fires oobAfterSwap on the swap's own elements, not on the out-of-band part, so e.target is no use
  // here: paint the part's target (detail.target), which for an "innerHTML:" part is the container that
  // holds the new content, wherever on the page it lies.
  document.body.addEventListener("htmx:oobAfterSwap", function (e) {
    var t = e.detail && e.detail.target;
    if (t instanceof Element && t.isConnected) paint(t);
  });
  // "Today" turns into "Yesterday" at the browser's midnight, and a hidden tab's timers sleep:
  // repaint at the next midnight and when the tab comes back.
  function untilMidnight() {
    var n = new Date();
    return new Date(n.getFullYear(), n.getMonth(), n.getDate() + 1, 0, 0, 1) - n;
  }
  function paintAtMidnight() {
    setTimeout(function () { paint(); paintAtMidnight(); }, Math.max(untilMidnight(), 1000));
  }
  paintAtMidnight();
  document.addEventListener("visibilitychange", function () { if (!document.hidden) paint(); });
  // timers also pause while the machine sleeps over midnight
  window.addEventListener("focus", function () { paint(); });
  window.addEventListener("pageshow", function () { paint(); });
})();
