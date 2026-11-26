package web

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/hellwand/tasmofleet/internal/schedule"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// The "5 · When" card of the update preview (#604): start now (the default) or at a chosen time, with
// quick picks. The server computes the picks and checks the time; static/updatestart.js does the same sums in the
// browser's zone, because only the browser knows the zone the person means. Without JavaScript the fields are
// read in the server's zone, and the card says so.

const (
	pickTonight  = "tonight"
	pickTomorrow = "tomorrow"
	pickLast     = "last"
	pickCustom   = "custom" // the "Other time" radio: the date and time fields

	nightHour = 3 // "Tonight 03:00": the hour of the night picks
)

// Modes of the plan's "Starts" row.
const (
	startsNow   = "now"   // when the button is pressed
	startsSet   = "set"   // at a valid future time
	startsPast  = "past"  // the chosen time is not in the future
	startsUnset = "unset" // "at a chosen time" without a usable time
)

// startPick is one quick pick. At is its instant in the server's zone; the picks are offered at their next
// occurrence, so a pick never lies in the past.
type startPick struct {
	Kind  string
	Label string
	At    time.Time
}

// Value is what the radio posts: the instant, so the pick works without JavaScript.
func (p startPick) Value() string { return p.At.Format(time.RFC3339) }

// Date is the pick's date: "Sat, Nov 7".
func (p startPick) Date() string { return p.At.Format("Mon, Jan 2") }

// startPicks are the quick picks at now: Tonight 03:00 (the next 03:00), Tomorrow night 03:00 (the 03:00
// after that) and "Like last time" (the weekday and time of the person's last choice, at its next
// occurrence). The last one is left out when there is none (last is zero) or when it equals one of the first two.
func startPicks(now, last time.Time) []startPick {
	tonight := nextAt(now, -1, nightHour, 0)
	tomorrow := time.Date(tonight.Year(), tonight.Month(), tonight.Day()+1, nightHour, 0, 0, 0, now.Location())
	picks := []startPick{
		{pickTonight, fmt.Sprintf("Tonight %02d:00", nightHour), tonight},
		{pickTomorrow, fmt.Sprintf("Tomorrow night %02d:00", nightHour), tomorrow},
	}
	if !last.IsZero() {
		l := last.In(now.Location())
		at := nextAt(now, int(l.Weekday()), l.Hour(), l.Minute())
		if !at.Equal(tonight) && !at.Equal(tomorrow) {
			picks = append(picks, startPick{pickLast, "Like last time: " + l.Format("Mon 15:04"), at})
		}
	}
	return picks
}

// nextAt is the first time after now that is hour:minute on the given weekday (-1: any day), in now's zone.
func nextAt(now time.Time, weekday, hour, minute int) time.Time {
	for d := 0; ; d++ {
		t := time.Date(now.Year(), now.Month(), now.Day()+d, hour, minute, 0, 0, now.Location())
		if t.After(now) && (weekday < 0 || int(t.Weekday()) == weekday) {
			return t
		}
	}
}

// whenState is the "When" card and the plan's "Starts" row at one moment.
type whenState struct {
	At         bool   // "Start at a chosen time" is selected
	Picked     string // the value of the quick pick radio that is checked ("custom": Other time)
	Date, Time string // the typed fields of Other time
	Start      time.Time
	Mode       string // startsNow, startsSet, startsPast, startsUnset
	Err        string // why the server refused the time; "" when there is nothing to refuse

	Now   time.Time // the server's clock when the page was made
	Picks []startPick
}

// parseStart reads the When card from a posted form. The radio decides: "now" (or nothing posted) never
// schedules, however stale the browser's instant is. For "at", the instant the browser posted (start_at, RFC 3339 with
// its offset) wins; without JavaScript the pick's own value or the date and time fields are used, the fields in
// now's zone. A time that is not after now is refused.
func parseStart(form url.Values, now time.Time) whenState {
	w := whenState{Now: now, Mode: startsNow}
	if form.Get("when") != "at" {
		return w
	}
	w.At, w.Mode = true, startsUnset
	w.Picked = strings.TrimSpace(form.Get("at"))
	w.Date, w.Time = strings.TrimSpace(form.Get("at_date")), strings.TrimSpace(form.Get("at_time"))
	var t time.Time
	var err error
	switch raw := strings.TrimSpace(form.Get("start_at")); {
	case raw != "":
		t, err = time.Parse(time.RFC3339, raw)
	case w.Picked == pickCustom:
		if w.Date != "" && w.Time != "" {
			const layout = "2006-01-02 15:04"
			t, err = time.ParseInLocation(layout, w.Date+" "+w.Time, now.Location())
			// Go moves a time that the clocks skip (02:30 on the night they go forward) to the hour after it. With
			// JavaScript the Starts row shows that moved time before the submit; here the person is sent straight to the
			// transaction page and would never see it, so a time the clock never shows is refused (#792).
			if wall, werr := time.Parse(layout, w.Date+" "+w.Time); err == nil && werr == nil && t.Format(layout) != wall.Format(layout) {
				w.Err = "That time does not exist on this date (clock change)."
				return w
			}
		}
	case w.Picked != "":
		t, err = time.Parse(time.RFC3339, w.Picked)
	}
	switch {
	case err != nil || t.IsZero():
		w.Err = "Choose a quick pick, or a date and a time."
	case !t.After(now):
		w.Mode, w.Err = startsPast, "Choose a time in the future. It is now "+now.Format("Mon, Jan 2, 15:04")+"."
	default:
		w.Mode, w.Start = startsSet, t
	}
	return w
}

// planAgainStart reads the time choice that "Plan again…" posts to the preview: "Start at a chosen time", and the
// planned time as "Other time" when it is still ahead. A time that has passed (a plan that was not started, or
// cancelled after its time) leaves the choice at "a chosen time" with no time picked and no complaint: the
// person has not chosen anything wrong yet. Nothing here ever selects "Start now", and nothing starts without
// the person pressing the button of the preview.
func planAgainStart(form url.Values, now time.Time) whenState {
	w := whenState{Now: now, At: true, Mode: startsUnset}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(form.Get("start_at")))
	if err != nil || !t.After(now) {
		return w
	}
	if t.Truncate(time.Minute) != t {
		t = t.Truncate(time.Minute).Add(time.Minute) // the time fields hold whole minutes; a rounding must never move the start earlier
	}
	t = t.In(now.Location())
	w.Mode, w.Start, w.Picked = startsSet, t, pickCustom
	w.Date, w.Time = t.Format("2006-01-02"), t.Format("15:04") // the values of <input type=date> and <input type=time>
	return w
}

// ButtonText is the label of the button that starts the update.
func (w whenState) ButtonText() string {
	if w.At {
		return "Schedule update"
	}
	return "Start update"
}

// StartsText is the plan's start: "Sat, Nov 7 at 03:00".
func (w whenState) StartsText() string {
	return ui.StampTextIn(w.Start.In(w.Now.Location()), ui.StampStart)
}

// StartsIn is how far away the start is: "in 17 h", "in 5 d 16 h", "in 12 min".
func (w whenState) StartsIn() string { return startsIn(w.Now, w.Start) }

// startsIn words how far start lies after now.
func startsIn(now, start time.Time) string {
	mins := int(start.Sub(now).Minutes())
	if mins < 60 {
		return "in " + strconv.Itoa(max(mins, 1)) + " min"
	}
	hours := (mins + 30) / 60 // rounded, so 23 h 40 min does not read "in 0 d 23 h"
	if days := hours / 24; days > 0 {
		return "in " + strconv.Itoa(days) + " d " + strconv.Itoa(hours%24) + " h"
	}
	return "in " + strconv.Itoa(hours) + " h"
}

// ruleStart is the start the rule text is written for: the chosen one, or Tonight's while nothing is chosen
// (the text is then hidden and the browser fills in the real one).
func (w whenState) ruleStart() time.Time {
	if !w.Start.IsZero() {
		return w.Start.In(w.Now.Location())
	}
	return nextAt(w.Now, -1, nightHour, 0)
}

// RuleClock and RuleDeadline are the two clock times the rule names: the start and the end of the window in
// which TasmoFleet still starts a missed job.
func (w whenState) RuleClock() string { return ui.StampTextIn(w.ruleStart(), ui.StampTime) }
func (w whenState) RuleDeadline() string {
	return ui.StampTextIn(w.ruleStart().Add(schedule.LateStart), ui.StampTime)
}

// LateMinutes is the window in minutes; it is schedule.LateStart, fixed, not a setting.
func (w whenState) LateMinutes() int { return int(schedule.LateStart / time.Minute) }

// ShowRule: the rule is only true for a timed start.
func (w whenState) ShowRule() bool { return w.Mode == startsSet }

// Preset is the instant that "Plan again…" preselected, for updatestart.js: it writes it into the date and time
// fields in the browser's zone, where the server can only write the wall clock of its own. "" when nothing
// was preselected.
func (w whenState) Preset() string {
	if w.Start.IsZero() || w.Picked != pickCustom {
		return ""
	}
	return w.Start.Format(time.RFC3339)
}

// cardAttrs is data-preset on the When card, only when a preset exists (a key the card does not have stays absent).
func (w whenState) cardAttrs() templ.Attributes {
	if p := w.Preset(); p != "" {
		return templ.Attributes{"data-preset": p}
	}
	return nil
}

// Invalid marks the fields of a time the server refused.
func (w whenState) Invalid() bool { return w.Err != "" }

// ZoneText names the server's zone: "CET, UTC+1" (what a time without JavaScript is read in).
func (w whenState) ZoneText() string { return zoneText(w.Now) }

// zoneText is the zone of t as "CET, UTC+1"; a zone without a name of letters is just its offset.
func zoneText(t time.Time) string {
	name, secs := t.Zone()
	off := "UTC"
	if secs != 0 {
		sign, a := "+", secs
		if a < 0 {
			sign, a = "-", -a
		}
		off += sign + strconv.Itoa(a/3600)
		if m := a % 3600 / 60; m != 0 {
			off += fmt.Sprintf(":%02d", m)
		}
	}
	if name == "UTC" || strings.HasPrefix(name, "+") || strings.HasPrefix(name, "-") {
		return off
	}
	return name + ", " + off
}
