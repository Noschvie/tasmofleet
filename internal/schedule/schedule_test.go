package schedule

import (
	"testing"
	"time"
)

func berlin(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("tzdata: %v", err)
	}
	return loc
}

func mustNew(t *testing.T, days, clock string) Schedule {
	t.Helper()
	s, err := New(days, clock)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Why: a person who ticks Tuesday expects Tuesday, not the day the bit happens to sit at. Every
// weekday is tried alone from the same Monday noon, so a shifted or swapped weekday fails.
func TestNextForEveryWeekdayChoice(t *testing.T) {
	loc := berlin(t)
	from := time.Date(2026, 11, 2, 12, 0, 0, 0, loc) // Monday
	for i, name := range DayNames() {
		s := mustNew(t, name, "03:00")
		got := s.Next(from)
		want := time.Date(2026, 11, 2+i, 3, 0, 0, 0, loc)
		if i == 0 {
			want = time.Date(2026, 11, 9, 3, 0, 0, 0, loc) // Monday 03:00 is already past: next week
		}
		if !got.Equal(want) {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
		if _, ok := map[string]bool{name: true}[names[got.Weekday()]]; !ok {
			t.Errorf("%s: ran on a %s", name, got.Weekday())
		}
	}
}

func TestNextPicksTheNearestChosenDay(t *testing.T) {
	loc := berlin(t)
	s := mustNew(t, "fri,mon,wed", "06:30") // order and case of the list do not matter
	for _, c := range []struct{ from, want time.Time }{
		{time.Date(2026, 11, 2, 6, 29, 0, 0, loc), time.Date(2026, 11, 2, 6, 30, 0, 0, loc)}, // Mon, a minute before
		{time.Date(2026, 11, 2, 6, 30, 0, 0, loc), time.Date(2026, 11, 4, 6, 30, 0, 0, loc)}, // at the run: strictly after
		{time.Date(2026, 11, 5, 9, 0, 0, 0, loc), time.Date(2026, 11, 6, 6, 30, 0, 0, loc)},  // Thu -> Fri
		{time.Date(2026, 11, 6, 7, 0, 0, 0, loc), time.Date(2026, 11, 9, 6, 30, 0, 0, loc)},  // Fri after -> Mon
	} {
		if got := s.Next(c.from); !got.Equal(c.want) {
			t.Errorf("Next(%v) = %v, want %v", c.from, got, c.want)
		}
	}
}

func TestPrevIsTheLatestRunAtOrBefore(t *testing.T) {
	loc := berlin(t)
	s := mustNew(t, "mon,wed", "03:00")
	for _, c := range []struct{ at, want time.Time }{
		{time.Date(2026, 11, 4, 3, 0, 0, 0, loc), time.Date(2026, 11, 4, 3, 0, 0, 0, loc)},  // exactly at the run
		{time.Date(2026, 11, 4, 2, 59, 0, 0, loc), time.Date(2026, 11, 2, 3, 0, 0, 0, loc)}, // a minute before: the Monday
		{time.Date(2026, 11, 8, 12, 0, 0, 0, loc), time.Date(2026, 11, 4, 3, 0, 0, 0, loc)},
	} {
		if got := s.Prev(c.at); !got.Equal(c.want) {
			t.Errorf("Prev(%v) = %v, want %v", c.at, got, c.want)
		}
	}
}

func TestOffScheduleHasNoRuns(t *testing.T) {
	s := mustNew(t, "", "03:00")
	if !s.Off() || !s.Next(time.Now()).IsZero() || !s.Prev(time.Now()).IsZero() {
		t.Fatal("a schedule without weekdays is manual only: no run, ever")
	}
}

// Why: "daily 03:00" must stay 03:00 on the wall clock when the clocks change. A 24-hour interval
// would drift to 02:00 or 04:00; this is the reason the schedule counts in wall-clock time.
func TestDailyStaysOnTheWallClockAcrossBothClockChanges(t *testing.T) {
	loc := berlin(t)
	s := mustNew(t, "mon,tue,wed,thu,fri,sat,sun", "03:00")
	for _, start := range []time.Time{
		time.Date(2026, 3, 26, 12, 0, 0, 0, loc),  // spring forward on 29 March
		time.Date(2026, 10, 22, 12, 0, 0, 0, loc), // fall back on 25 October
	} {
		at := start
		var last time.Time
		for i := 0; i < 8; i++ {
			next := s.Next(at)
			if h, m, _ := next.Clock(); h != 3 || m != 0 {
				t.Fatalf("run %d after %v is at %v, not 03:00 local", i, start, next)
			}
			if !last.IsZero() && next.Format("2006-01-02") == last.Format("2006-01-02") {
				t.Fatalf("two runs on one day: %v and %v", last, next)
			}
			last, at = next, next
		}
	}
	// The gap between the two runs around the change is 23 h (spring) and 25 h (fall).
	spring := s.Next(time.Date(2026, 3, 28, 12, 0, 0, 0, loc)) // 29 March 03:00
	if got := s.Next(spring).Sub(spring); got != 24*time.Hour {
		t.Errorf("29 -> 30 March: %v", got)
	}
	if got := spring.Sub(s.Prev(spring.Add(-time.Second))); got != 23*time.Hour {
		t.Errorf("28 -> 29 March 03:00: %v, want 23h because the night is an hour short", got)
	}
	fall := s.Next(time.Date(2026, 10, 24, 12, 0, 0, 0, loc)) // 25 October 03:00
	if got := fall.Sub(s.Prev(fall.Add(-time.Second))); got != 25*time.Hour {
		t.Errorf("24 -> 25 Oct 03:00: %v, want 25h because the night is an hour long", got)
	}
}

// Why: on the night the clocks jump from 02:00 to 03:00 a 02:30 run has no wall-clock time. It must
// still run once that day (shifted to 03:30), not be skipped and not run twice.
func TestSpringForwardGapRunsOnceShifted(t *testing.T) {
	loc := berlin(t)
	s := mustNew(t, "mon,tue,wed,thu,fri,sat,sun", "02:30")
	got := s.Next(time.Date(2026, 3, 28, 12, 0, 0, 0, loc))
	if want := time.Date(2026, 3, 29, 1, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("29 March run at %v (%v UTC), want 03:30 CEST = 01:30 UTC", got, got.UTC())
	}
	if h, m, _ := got.Clock(); h != 3 || m != 30 {
		t.Errorf("shown as %v", got)
	}
	if next := s.Next(got); !next.Equal(time.Date(2026, 3, 30, 2, 30, 0, 0, loc)) {
		t.Errorf("the day after: %v", next)
	}
	if prev := s.Prev(time.Date(2026, 3, 29, 12, 0, 0, 0, loc)); !prev.Equal(got) {
		t.Errorf("Prev on the gap day: %v, want %v", prev, got)
	}
}

// Why: on the night the clocks go back from 03:00 to 02:00, 02:30 happens twice. A backup that ran
// twice would hit every device twice; it runs once, at the first 02:30.
func TestFallBackOverlapRunsOnce(t *testing.T) {
	loc := berlin(t)
	s := mustNew(t, "mon,tue,wed,thu,fri,sat,sun", "02:30")
	first := s.Next(time.Date(2026, 10, 24, 12, 0, 0, 0, loc))
	if want := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC); !first.Equal(want) {
		t.Fatalf("25 October run at %v UTC, want the first 02:30 (00:30 UTC)", first.UTC())
	}
	next := s.Next(first)
	if want := time.Date(2026, 10, 26, 1, 30, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("after the first 02:30 the next run is %v UTC, want 26 Oct 02:30 CET (01:30 UTC), not the repeated 02:30", next.UTC())
	}
	// The second 02:30 (01:30 UTC) is not a run either.
	if prev := s.Prev(time.Date(2026, 10, 25, 1, 45, 0, 0, time.UTC).In(loc)); !prev.Equal(first) {
		t.Errorf("Prev in the repeated hour: %v, want the first 02:30", prev.UTC())
	}
}

func TestParseDaysAndClock(t *testing.T) {
	d, err := ParseDays(" Sun, MON ,wed,mon")
	if err != nil || d.String() != "mon,wed,sun" {
		t.Errorf("ParseDays = %q, %v; want the canonical week order mon,wed,sun", d.String(), err)
	}
	if _, err := ParseDays("mon,someday"); err == nil {
		t.Error("an unknown weekday must be refused, not ignored")
	}
	if d, err := ParseDays(""); err != nil || d != 0 {
		t.Errorf("an empty list is manual only: %v %v", d, err)
	}
	for _, ok := range []string{"00:00", "03:00", "23:59"} {
		if _, _, err := ParseClock(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "3:00", "24:00", "12:60", "03:00:00", "noon", "0300"} {
		if _, _, err := ParseClock(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

// Why: the three outcomes of looking at the schedule are the whole missed-run rule (#601): a run
// that is due starts, one that is up to the grace late still starts, an older one is skipped.
func TestPlan(t *testing.T) {
	loc := berlin(t)
	s := mustNew(t, "mon,tue,wed,thu,fri,sat,sun", "03:00")
	slot := time.Date(2026, 11, 3, 3, 0, 0, 0, loc)
	yesterday := slot.Add(-24 * time.Hour)
	const grace = 60 * time.Minute
	for _, c := range []struct {
		name    string
		now     time.Time
		handled time.Time
		want    Action
	}{
		{"before the run: wait", slot.Add(-time.Minute), yesterday, Wait},
		{"the run's time has come", slot, yesterday, Run},
		{"late within the grace", slot.Add(grace), yesterday, Run},
		{"later than the grace: skipped", slot.Add(grace + time.Second), yesterday, Missed},
		{"after a long downtime only the newest run counts", slot.Add(5 * time.Hour), yesterday.Add(-72 * time.Hour), Missed},
		{"already ran for this slot", slot.Add(10 * time.Minute), slot.Add(time.Second), Wait},
		{"a skip settles the slot", slot.Add(2 * time.Hour), slot.Add(2 * time.Hour), Wait},
		{"never handled, within the grace", slot.Add(time.Minute), time.Time{}, Run},
	} {
		got := Plan(s, c.now, c.handled, grace)
		if got.Action != c.want {
			t.Errorf("%s: action %v, want %v", c.name, got.Action, c.want)
		}
		if c.want != Wait && !got.Slot.Equal(slot) {
			t.Errorf("%s: slot %v, want %v", c.name, got.Slot, slot)
		}
		if next := s.Next(c.now); !got.Next.Equal(next) {
			t.Errorf("%s: next %v, want %v", c.name, got.Next, next)
		}
	}
	if got := Plan(Schedule{}, slot, time.Time{}, grace); got.Action != Wait || !got.Next.IsZero() {
		t.Errorf("an off schedule never runs: %+v", got)
	}
}

// Why: the Backups page calls a backup "old" after twice the longest wait between runs; a weekly
// schedule must not make every backup look old after a day, nor a Friday-to-Monday weekend gap count as one day.
func TestLongestGap(t *testing.T) {
	day := 24 * time.Hour
	for _, c := range []struct {
		days string
		want time.Duration
	}{
		{"", 0},
		{"mon,tue,wed,thu,fri,sat,sun", day},
		{"mon", 7 * day},
		{"mon,tue,wed,thu,fri", 3 * day}, // Friday to Monday
		{"mon,wed,fri", 3 * day},
		{"sat,sun", 6 * day},
	} {
		if got := mustNew(t, c.days, "03:00").LongestGap(); got != c.want {
			t.Errorf("%q: %v, want %v", c.days, got, c.want)
		}
	}
}
