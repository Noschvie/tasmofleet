// The "5 · When" card of the update preview (#604). The server offers the quick picks in its own zone; only the
// browser knows the zone the person means, so this file does the same sums in the browser's zone: it moves the
// picks to their next occurrence here, turns the chosen pick or the typed date and time into one instant (RFC 3339
// with the browser's offset, in the hidden field start_at, which the server prefers over its own reading of the
// fields), and keeps the plan's "Starts" row, the rule, the button and the zone sentence in step with the choice.
// Without this file the card still works: the server reads the pick's own value, or the fields in its zone.
// The server refuses a time that is not in the future either way. No inline script: CSP.
(function () {
  "use strict";
  var when = document.querySelector(".upd-when");
  if (!when) return;
  var DAYS = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
  var MONTHS = [
    "Jan",
    "Feb",
    "Mar",
    "Apr",
    "May",
    "Jun",
    "Jul",
    "Aug",
    "Sep",
    "Oct",
    "Nov",
    "Dec",
  ];
  var NIGHT = 3; // Tonight 03:00: the same hour as the server's
  var starts = document.querySelector("[data-starts]");
  var rule = document.querySelector("[data-starts-rule]");
  var go = document.getElementById("start-go");
  var err = document.getElementById("when-err");
  var hidden = document.querySelector("[data-start-at]");
  var tz = when.querySelector("[data-tz]");
  var radioAt = document.getElementById("when-at");
  var picks = Array.prototype.slice.call(
    when.querySelectorAll('input[name="at"]'),
  );
  var dateField = when.querySelector('input[name="at_date"]');
  var timeField = when.querySelector('input[name="at_time"]');
  var lateMinutes = rule
    ? parseInt(rule.getAttribute("data-late"), 10) || 60
    : 60;

  function two(n) {
    return (n < 10 ? "0" : "") + n;
  }
  function clock(d) {
    return two(d.getHours()) + ":" + two(d.getMinutes());
  }
  function dayText(d) {
    return DAYS[d.getDay()] + ", " + MONTHS[d.getMonth()] + " " + d.getDate();
  }
  // RFC 3339 with the offset of the date itself (the offset can differ between summer and winter)
  function iso(d) {
    var off = -d.getTimezoneOffset();
    var sign = off < 0 ? "-" : "+";
    off = Math.abs(off);
    return (
      d.getFullYear() +
      "-" +
      two(d.getMonth() + 1) +
      "-" +
      two(d.getDate()) +
      "T" +
      two(d.getHours()) +
      ":" +
      two(d.getMinutes()) +
      ":" +
      two(d.getSeconds()) +
      sign +
      two(Math.floor(off / 60)) +
      ":" +
      two(off % 60)
    );
  }
  // the first time after now that is hh:mm on weekday wd (-1: any day)
  function nextAt(now, wd, hh, mm) {
    for (var i = 0; i < 9; i++) {
      var d = new Date(
        now.getFullYear(),
        now.getMonth(),
        now.getDate() + i,
        hh,
        mm,
        0,
        0,
      );
      if (d > now && (wd < 0 || d.getDay() === wd)) return d;
    }
    return null;
  }
  function relative(now, d) {
    var mins = Math.floor((d - now) / 60000);
    if (mins < 60) return "in " + Math.max(mins, 1) + " min";
    var hours = Math.floor((mins + 30) / 60);
    var days = Math.floor(hours / 24);
    return "in " + (days ? days + " d " + (hours % 24) + " h" : hours + " h");
  }
  function zoneSentence() {
    var name = "";
    try {
      new Intl.DateTimeFormat(undefined, { timeZoneName: "short" })
        .formatToParts(new Date())
        .forEach(function (p) {
          if (p.type === "timeZoneName") name = p.value;
        });
    } catch (e) {
      name = "";
    }
    var off = -new Date().getTimezoneOffset();
    var utc = "UTC";
    if (off !== 0) {
      var a = Math.abs(off);
      utc +=
        (off < 0 ? "-" : "+") +
        Math.floor(a / 60) +
        (a % 60 ? ":" + two(a % 60) : "");
    }
    var named = name && !/^(GMT|UTC)?[+-]/.test(name) && name !== "UTC";
    return (
      "Times are in your time zone (" + (named ? name + ", " + utc : utc) + ")."
    );
  }

  // moves the quick picks to their next occurrence in the browser's zone: texts, dates and the instants they carry
  function refreshPicks() {
    var now = new Date();
    var tonight = nextAt(now, -1, NIGHT, 0);
    var tomorrow = new Date(
      tonight.getFullYear(),
      tonight.getMonth(),
      tonight.getDate() + 1,
      NIGHT,
      0,
      0,
      0,
    );
    picks.forEach(function (input) {
      var kind = input.getAttribute("data-kind");
      var label = input.parentNode;
      var d = null;
      if (kind === "tonight") d = tonight;
      else if (kind === "tomorrow") d = tomorrow;
      else if (kind === "last") {
        // the pick's own value is the instant of the server's reading; its weekday and time in this zone are the habit
        var seen = new Date(input.value);
        if (!isNaN(seen))
          d = nextAt(now, seen.getDay(), seen.getHours(), seen.getMinutes());
        if (d && (+d === +tonight || +d === +tomorrow)) {
          label.hidden = true; // the same night as one of the first two
          if (input.checked) input.checked = false;
          return;
        }
        label.hidden = false;
        if (d)
          label.querySelector(".chip-l").textContent =
            "Like last time: " + DAYS[d.getDay()] + " " + clock(d);
      }
      if (!d) return;
      input.value = iso(d);
      label.querySelector(".chip-sub").textContent = dayText(d);
    });
  }

  // the instant "Plan again…" preselected and the date and time it wrote into the fields (see chosen)
  var presetAt = null;
  var presetFields = [];

  // the chosen start in this zone: a Date, null for nothing usable, or "now"
  function chosen() {
    if (!radioAt.checked) return "now";
    var pick = picks.filter(function (p) {
      return p.checked;
    })[0];
    if (!pick) return null;
    if (pick.getAttribute("data-kind") === "custom") {
      var dv = /^(\d{4})-(\d{2})-(\d{2})$/.exec(dateField.value);
      var tv = /^(\d{2}):(\d{2})$/.exec(timeField.value);
      if (!dv || !tv) return null;
      // while the fields still hold what the preset wrote, the preset is the answer: the wall clock of the night
      // the clocks go back is ambiguous, and parsing it picks the earlier of its two hours, which would plan
      // the update an hour before the plan it repeats
      if (
        presetAt &&
        dateField.value === presetFields[0] &&
        timeField.value === presetFields[1]
      )
        return presetAt;
      return new Date(+dv[1], +dv[2] - 1, +dv[3], +tv[1], +tv[2], 0, 0);
    }
    var d = new Date(pick.value);
    return isNaN(d) ? null : d;
  }

  function text(parent, tag, s, cls) {
    var el = document.createElement(tag);
    if (cls) el.className = cls;
    el.textContent = s;
    parent.appendChild(el);
    return el;
  }

  // keepServerError: on load the server's refusal stays until the person changes something
  function update(keepServerError) {
    var now = new Date();
    var c = chosen();
    var bad = c instanceof Date && c <= now;
    var invalid = false;
    if (!keepServerError || !err || err.hidden) {
      if (err) {
        err.hidden = !bad;
        err.textContent = bad
          ? "Choose a time in the future. It is now " +
            dayText(now) +
            ", " +
            clock(now) +
            "."
          : "";
      }
      invalid = bad;
    } else {
      invalid = true;
    }
    [dateField, timeField].forEach(function (f) {
      if (invalid) {
        f.setAttribute("aria-invalid", "true");
        f.setAttribute("aria-describedby", "when-err");
      } else {
        f.removeAttribute("aria-invalid");
        f.removeAttribute("aria-describedby");
      }
    });
    // a past time is posted too: the server refuses the instant. Without it, it would read the typed digits in its
    // own zone, and a time the page has just called past could be planned for another hour.
    hidden.value = c instanceof Date ? iso(c) : "";
    if (go)
      go.textContent = radioAt.checked ? "Schedule update" : "Start update";
    if (keepServerError && invalid) return; // the server's own words for "Starts" are already on the page
    starts.textContent = "";
    var set = c instanceof Date && !bad;
    if (c === "now") {
      text(starts, "b", "Now");
      starts.appendChild(
        document.createTextNode(" · when you press Start update"),
      );
    } else if (set) {
      text(starts, "b", dayText(c) + " at " + clock(c));
      starts.appendChild(document.createTextNode(" · " + relative(now, c)));
    } else {
      text(
        starts,
        "span",
        bad
          ? "Not set: the time is in the past"
          : "Not set yet: choose a quick pick or a time",
        "upd-warn-text",
      );
    }
    if (rule) {
      rule.hidden = !set;
      if (set) {
        rule.querySelector("[data-clock]").textContent = clock(c);
        rule.querySelector("[data-deadline]").textContent = clock(
          new Date(+c + lateMinutes * 60000),
        );
      }
    }
  }

  // typing a date or a time means "Other time"
  function useCustom() {
    picks.forEach(function (p) {
      if (p.getAttribute("data-kind") === "custom") p.checked = true;
    });
    radioAt.checked = true;
  }
  [dateField, timeField].forEach(function (f) {
    f.addEventListener("input", useCustom);
  });
  when.addEventListener("change", function () {
    update(false);
  });
  when.addEventListener("input", function () {
    update(false);
  });
  if (tz) tz.textContent = zoneSentence();
  // "Plan again…" preselects the time of the plan: the server sends the instant, and the fields show it in this
  // zone (the server can only write its own zone's clock into them). A time that is not ahead is left alone.
  var preset = new Date(when.getAttribute("data-preset") || NaN);
  if (!isNaN(preset) && preset > new Date()) {
    dateField.value =
      preset.getFullYear() +
      "-" +
      two(preset.getMonth() + 1) +
      "-" +
      two(preset.getDate());
    timeField.value = two(preset.getHours()) + ":" + two(preset.getMinutes());
    presetAt = preset;
    presetFields = [dateField.value, timeField.value];
  }
  // A recheck (a language, a build, Change, Forget) posts its own form, which has no part of this card. It must
  // carry the person's current choice, or the page comes back on "Start now" with the button armed to start at
  // once, or on the time the server preset, not the one the person moved to. Without this file the form keeps
  // what the server wrote into it (the preset of "Plan again…") and nothing else.
  var recheck = document.getElementById("recheck");
  function carry(name, value) {
    var f = recheck.querySelector('input[type="hidden"][name="' + name + '"]');
    if (value === null) {
      if (f) f.parentNode.removeChild(f);
      return;
    }
    if (!f) {
      f = document.createElement("input");
      f.type = "hidden";
      f.name = name;
      recheck.appendChild(f);
    }
    f.value = value;
  }
  if (recheck)
    recheck.addEventListener("submit", function () {
      carry("when", radioAt.checked ? "at" : null);
      carry("start_at", radioAt.checked && hidden.value ? hidden.value : null);
    });
  refreshPicks();
  update(true);
})();
