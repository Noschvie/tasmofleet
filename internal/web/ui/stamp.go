package ui

import (
	"time"

	"github.com/a-h/templ"
)

// StampFmt is one way to show a point in time. Layout is the server-side fallback (the server's
// zone); Key names the Intl.DateTimeFormat options static/time.js uses to show the same instant in
// the browser's locale and time zone. The two stay close on purpose: same fields, same precision.
type StampFmt struct{ Key, Layout string }

var (
	StampDateTimeSec = StampFmt{"datetime-s", "2006-01-02 15:04:05"}
	StampDateTime    = StampFmt{"datetime", "2006-01-02 15:04"}
	StampClockSec    = StampFmt{"time-s", "15:04:05"}
	StampClock       = StampFmt{"time", "15:04"}
	StampShort       = StampFmt{"short", "2 Jan 2006, 15:04"}
)

// StampText is the fallback text of a timestamp: what the page shows without JavaScript. It is
// in the server's zone (t.Local()), never in the zone the store happens to hand the time out in.
func StampText(t time.Time, f StampFmt) string { return t.Local().Format(f.Layout) }

// StampAttrs are the attributes of a <time> element that time.js rewrites in the browser's locale
// and zone: datetime (RFC 3339, the instant) and data-ts (the format key). Spread them:
// <time { ui.StampAttrs(t, ui.StampClock)... }>{ ui.StampText(t, ui.StampClock) }</time>.
func StampAttrs(t time.Time, f StampFmt) templ.Attributes {
	return templ.Attributes{"datetime": t.UTC().Format(time.RFC3339), "data-ts": f.Key}
}
