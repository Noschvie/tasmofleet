package ui

import (
	"time"

	"github.com/a-h/templ"
)

// StampFmt is one way to show a point in time. Layout is the server-side fallback (the server's
// zone); Key names the entry of static/time.js's SPECS table that shows the same instant in the
// browser's locale and time zone. The two lists of keys are the same set (TestStampKeysMatchTimeJS),
// and the fallback has the same fields as the browser's text: no seconds, a 24-hour clock, the day first.
//
// Every visible date is one of date, date-time, month-day and relative (docs/spec/design-system.md,
// Wording); time is a clock time on a line that already names the day.
type StampFmt struct{ Key, Layout string }

var (
	// StampDate is a day: "20 Nov 2026".
	StampDate = StampFmt{"date", "2 Jan 2006"}
	// StampDateTime is an instant with its day: "20 Nov 2026, 14:17". The title of the element carries the seconds and the zone.
	StampDateTime = StampFmt{"date-time", "2 Jan 2006, 15:04"}
	// StampMonthDay is the day of a scheduled update's start, "Nov 20"; the fallback is in English, time.js writes the browser's locale.
	StampMonthDay = StampFmt{"month-day", "Jan 2"}
	// StampRelative is a relative day: "Today 09:41", "Yesterday 21:10", else "19 Aug, 20:02" (with the
	// year when it is not this year). time.js decides today and yesterday in the browser's zone and
	// repaints across midnight; the fallback text comes from StampRelativeText, not from a Layout.
	StampRelative = StampFmt{"relative", ""}
	// StampTime is a clock time on a line that already names the day ("Paused by ann at 09:41").
	StampTime = StampFmt{"time", "15:04"}
	// StampLog is a log column: 24-hour with seconds in any locale (time.js "clock-24").
	StampLog = StampFmt{"clock-24", "15:04:05"}
	// The start of a scheduled update (#604): "Sat 03:00" and the sentence form "Sat, Nov 7 at 03:00".
	// The fallbacks are in English; time.js writes the browser's locale.
	StampWeekdayTime = StampFmt{"weekday-time", "Mon 15:04"}
	StampStart       = StampFmt{"start", "Mon, Jan 2 at 15:04"}
	// StampTitle changes only the title of the element (full date, time and zone), not its text:
	// for a <time> whose text is relative ("5 min ago") or already worded.
	StampTitle = StampFmt{"title", "2006-01-02 15:04:05 MST"}
)

// StampText is the fallback text of a timestamp: what the page shows without JavaScript. It is
// in the server's zone (t.Local()), never in the zone the store happens to hand the time out in.
func StampText(t time.Time, f StampFmt) string { return t.Local().Format(f.Layout) }

// StampAttrs are the attributes of a <time> element that time.js rewrites in the browser's locale
// and zone: datetime (RFC 3339, the instant) and data-ts (the format key). Spread them:
// <time { ui.StampAttrs(t, ui.StampTime)... }>{ ui.StampText(t, ui.StampTime) }</time>.
func StampAttrs(t time.Time, f StampFmt) templ.Attributes {
	return templ.Attributes{"datetime": t.UTC().Format(time.RFC3339), "data-ts": f.Key}
}

// StampISO is the instant as RFC 3339 in UTC, for data attributes that time.js reads.
func StampISO(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// StampRelativeText is the fallback text of a StampRelative: t as a calendar day relative to now, in now's
// zone. Today and Yesterday carry the time; other days are a date and time, with the year only
// when it is not now's. A later day (an expiry) is a date, never "Today".
func StampRelativeText(now, t time.Time) string {
	t = t.In(now.Location())
	const day = "2006-01-02"
	switch t.Format(day) {
	case now.Format(day):
		return "Today " + t.Format("15:04")
	case now.AddDate(0, 0, -1).Format(day):
		return "Yesterday " + t.Format("15:04")
	}
	if t.Year() == now.Year() {
		return t.Format("2 Jan, 15:04")
	}
	return t.Format("2 Jan 2006, 15:04")
}
