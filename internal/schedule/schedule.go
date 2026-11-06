// Package schedule is the weekday-and-time-of-day schedule that TasmoFleet's timed jobs share
// (scheduled device backups, #601; the scheduled scan reuses it later, #326). It is pure: it
// takes times and returns times, reads no clock and touches no files.
//
// A schedule is a set of weekdays and one time of day on the wall clock of the location of the
// time it is asked about (the server's local time). Counting in wall-clock time, not in fixed
// intervals, is the point: "03:00" stays 03:00 when the clocks change.
//
// Daylight saving time: a time of day that does not exist on the day the clocks go forward
// (02:30 when 02:00 jumps to 03:00) runs at the same offset as the day before, which is the
// shifted wall-clock time (03:30). A time of day that exists twice on the day the clocks go back
// runs once, at its first occurrence. A schedule never fires twice a day and never skips a day.
package schedule

import (
	"fmt"
	"strings"
	"time"
)

// Days is a set of weekdays: bit n is time.Weekday(n) (Sunday is bit 0).
type Days uint8

// order is the order weekdays are listed in (Monday first); names are indexed by time.Weekday.
var (
	order = [7]time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday, time.Sunday}
	names = [7]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}
)

// DayNames lists the weekday names the schedule accepts, Monday first.
func DayNames() []string {
	out := make([]string, 0, len(order))
	for _, w := range order {
		out = append(out, names[w])
	}
	return out
}

// Has reports whether the set contains the weekday.
func (d Days) Has(w time.Weekday) bool { return d&(1<<uint(w)) != 0 }

// String is the canonical list: names in week order, comma separated ("mon,wed,fri"); empty for none.
func (d Days) String() string {
	var out []string
	for _, w := range order {
		if d.Has(w) {
			out = append(out, names[w])
		}
	}
	return strings.Join(out, ",")
}

// ParseDays reads a comma separated list of weekday names ("mon,wed,fri"), in any order and case.
// An empty list is the empty set. An unknown name is an error.
func ParseDays(s string) (Days, error) {
	var d Days
	for part := range strings.SplitSeq(s, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		found := false
		for w, n := range names {
			if n == part {
				d |= 1 << uint(w)
				found = true
			}
		}
		if !found {
			return 0, fmt.Errorf("unknown weekday %q (use %s)", part, strings.Join(DayNames(), ", "))
		}
	}
	return d, nil
}

// ParseClock reads a time of day as 24-hour "HH:MM".
func ParseClock(s string) (hour, minute int, err error) {
	s = strings.TrimSpace(s)
	t, err := time.Parse("15:04", s)
	if err != nil || len(s) != 5 {
		return 0, 0, fmt.Errorf("must be a time of day as HH:MM, for example 03:00")
	}
	return t.Hour(), t.Minute(), nil
}

// Schedule runs on Days at Hour:Minute. With no days it is off.
type Schedule struct {
	Days         Days
	Hour, Minute int
}

// New builds a schedule from the stored texts: a weekday list and a "HH:MM" time of day.
func New(days, clock string) (Schedule, error) {
	d, err := ParseDays(days)
	if err != nil {
		return Schedule{}, err
	}
	h, m, err := ParseClock(clock)
	if err != nil {
		return Schedule{}, err
	}
	return Schedule{Days: d, Hour: h, Minute: m}, nil
}

// Off reports whether no weekday is chosen (manual only).
func (s Schedule) Off() bool { return s.Days == 0 }

// Clock is the time of day as "HH:MM".
func (s Schedule) Clock() string { return fmt.Sprintf("%02d:%02d", s.Hour, s.Minute) }

// LongestGap is the longest time between two runs of the week (seven days for a single weekday),
// counted in whole days; zero when off. Pages use it to judge how old is too old.
func (s Schedule) LongestGap() time.Duration {
	if s.Off() {
		return 0
	}
	var gap, longest int
	seen := false
	for i := 0; i < 14; i++ { // two weeks, so the gap that wraps around the week end is whole
		if s.Days.Has(time.Weekday(i % 7)) {
			if seen {
				longest = max(longest, gap)
			}
			seen, gap = true, 0
		}
		gap++
	}
	return time.Duration(longest) * 24 * time.Hour
}

// lookDays is how far Next and Prev search: a week plus a day for the day the search starts on.
const lookDays = 8

// Next is the first run strictly after the given time, in that time's location; zero when off.
func (s Schedule) Next(after time.Time) time.Time {
	if s.Off() {
		return time.Time{}
	}
	loc := after.Location()
	y, mo, d := after.Date()
	for n := 0; n <= lookDays; n++ {
		if t, ok := s.on(loc, y, mo, d+n); ok && t.After(after) {
			return t
		}
	}
	return time.Time{}
}

// Prev is the latest run at or before the given time, in that time's location; zero when off.
func (s Schedule) Prev(at time.Time) time.Time {
	if s.Off() {
		return time.Time{}
	}
	loc := at.Location()
	y, mo, d := at.Date()
	for n := 0; n >= -lookDays; n-- {
		if t, ok := s.on(loc, y, mo, d+n); ok && !t.After(at) {
			return t
		}
	}
	return time.Time{}
}

// on is the run of the calendar day y-mo-d (out-of-range days roll over), if the day is chosen.
func (s Schedule) on(loc *time.Location, y int, mo time.Month, d int) (time.Time, bool) {
	wall := time.Date(y, mo, d, s.Hour, s.Minute, 0, 0, time.UTC) // the wall clock, written as a UTC time
	if !s.Days.Has(wall.Weekday()) {
		return time.Time{}, false
	}
	return atWall(loc, wall), true
}

// atWall is the instant at which the wall clock of loc shows the given wall time (written as a UTC
// time). It does not rely on time.Date, whose choice in a gap or an overlap is unspecified.
func atWall(loc *time.Location, wall time.Time) time.Time {
	_, before := wall.Add(-24 * time.Hour).In(loc).Zone()
	_, after := wall.Add(24 * time.Hour).In(loc).Zone()
	var best time.Time
	for _, off := range []int{before, after} {
		t := wall.Add(-time.Duration(off) * time.Second)
		if _, got := t.In(loc).Zone(); got != off {
			continue // the clock in force at t is not the one assumed: that wall time never showed
		}
		if best.IsZero() || t.Before(best) {
			best = t // twice on the day the clocks go back: the first time
		}
	}
	if best.IsZero() {
		// The clocks jumped over this wall time: use the offset from before the jump, which lands on
		// the same minute after the gap (02:30 becomes 03:30).
		best = wall.Add(-time.Duration(before) * time.Second)
	}
	return best.In(loc)
}

// Action is what a timed job does at the moment it looks at its schedule.
type Action int

// Actions.
const (
	// Wait: nothing is due; look again at Decision.Next.
	Wait Action = iota
	// Run: the latest run is due and not yet handled, and it is not too late. This includes a run
	// whose time has just come and a late start after downtime.
	Run
	// Missed: the latest run was not handled and is older than the grace; it is skipped.
	Missed
)

// Decision is Plan's answer.
type Decision struct {
	Action Action
	Slot   time.Time // the run Action is about (zero for Wait)
	Next   time.Time // the first run after now: when to look again
}

// Plan decides what is due at now. handled is the time of the newest thing that settled a run:
// a run is settled when handled is not before its time (a run that started at or after the slot,
// a skip, or a settings change that moved the schedule). A run that is not settled starts if now
// is within grace of its time (a late start after downtime) and is skipped when it is older.
// Only the latest run counts: after a long downtime one late run is due, not one per missed day.
func Plan(s Schedule, now, handled time.Time, grace time.Duration) Decision {
	d := Decision{Next: s.Next(now)}
	slot := s.Prev(now)
	if slot.IsZero() || !handled.Before(slot) {
		return d
	}
	d.Slot = slot
	if now.Sub(slot) <= grace {
		d.Action = Run
	} else {
		d.Action = Missed
	}
	return d
}
