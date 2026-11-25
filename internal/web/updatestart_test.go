package web

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/ota"
	"github.com/hellwand/tasmofleet/internal/store"
)

// fixedClock is a handler clock that stands still.
type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time                     { return c.now }
func (fixedClock) After(time.Duration) <-chan time.Time { return nil }

// The server's zone is the zone of its clock; the tests use a fixed one so nothing depends on the machine.
var (
	zoneCET = time.FixedZone("CET", 3600)
	// friday is "now" of the mockup: Fri, Nov 6, 10:21.
	friday = time.Date(2026, 11, 6, 10, 21, 0, 0, zoneCET)
)

func startForm(extra url.Values) url.Values {
	f := url.Values{"id": {"1", "2"}, "release": {"16.1.0"}, "policy": {"only-newer"}}
	for k, v := range extra {
		f[k] = v
	}
	return f
}

var (
	tagRe   = regexp.MustCompile(`<[^>]*>`)
	punctRe = regexp.MustCompile(` ([,.])`)
)

// plain is the visible text of a page: tags gone, white space collapsed.
func plain(body string) string {
	text := strings.Join(strings.Fields(tagRe.ReplaceAllString(body, " ")), " ")
	return punctRe.ReplaceAllString(text, "$1") // "03:00 ," where a tag stood before the comma
}

// tagOf is the first opening tag that contains marker, or "" when there is none.
func tagOf(t *testing.T, body, marker string) string {
	t.Helper()
	for _, m := range regexp.MustCompile(`<[a-z]+ [^>]*>`).FindAllString(body, -1) {
		if strings.Contains(m, marker) {
			return m
		}
	}
	return ""
}

// goButton is the label of the button that starts the update (it belongs to the form "start").
func goButton(t *testing.T, body string) string {
	t.Helper()
	m := regexp.MustCompile(`<button[^>]*form="start"[^>]*>([^<]*)</button>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no button of the start form in: %s", body)
	}
	return m[1]
}

// Why (#604): the person asked for nothing but the update, so nothing may start later by accident. "Start
// now" is the default even when the last job was scheduled, and the plan says the update starts when the
// button is pressed.
func TestPreviewStartsNowByDefault(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{friday})
	jobs.jobs = append(jobs.jobs, model.Job{ID: 5, BatchID: 5, DeviceID: 1, CreatedBy: "test:operator",
		StartAt: friday.AddDate(0, 0, -2), State: model.JobSucceeded})
	resp, body := e.do(t, "POST", "/updates/preview", "operator", startForm(nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if now := tagOf(t, body, `id="when-now"`); !strings.Contains(now, " checked") {
		t.Fatalf("Start now must be checked: %q", now)
	}
	if at := tagOf(t, body, `id="when-at"`); at == "" || strings.Contains(at, " checked") {
		t.Fatalf("Start at a chosen time must be offered and unchecked: %q", at)
	}
	if got := goButton(t, body); got != "Start update" {
		t.Errorf("button %q, want Start update", got)
	}
	if !strings.Contains(plain(body), "Starts Now · when you press Start update") {
		t.Errorf("the plan must say the update starts now: %s", plain(body))
	}
	if !strings.Contains(plain(body), "5 · When") {
		t.Error("the When card is step 5")
	}
}

// Why (#604, mockup 1b): the quick picks are what makes planning for the night one click. Tonight is the next
// 03:00, tomorrow night the one after, and each pick shows its date so nobody has to work out which night
// "tonight" is after midnight.
func TestPreviewOffersTheQuickPicksWithTheirDates(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{friday})
	// Wed, Nov 4 02:00: this person's last choice.
	jobs.jobs = append(jobs.jobs, model.Job{ID: 5, BatchID: 5, DeviceID: 1, CreatedBy: "test:operator",
		StartAt: time.Date(2026, 11, 4, 2, 0, 0, 0, zoneCET), State: model.JobSucceeded})
	_, body := e.do(t, "POST", "/updates/preview", "operator", startForm(nil))
	text := plain(body)
	for _, want := range []string{
		"Tonight 03:00 Sat, Nov 7", "Tomorrow night 03:00 Sun, Nov 8", "Like last time: Wed 02:00 Wed, Nov 11", "Other time",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("quick picks must read %q: %s", want, text)
		}
	}
	// It asked for the newest scheduled job of this person, not of everybody and not a job that started at once.
	asked := false
	for _, f := range jobs.filters {
		if f.Scheduled && f.CreatedBy == "test:operator" {
			asked = true
		}
	}
	if !asked {
		t.Errorf("the last choice is the person's own newest scheduled job: filters %+v", jobs.filters)
	}
}

// Why (#604): "Like last time" is the person's own habit. Another person's schedule, or a job that was not
// scheduled at all, is not a habit of theirs, and a pick that equals Tonight or Tomorrow would be a duplicate.
func TestLikeLastTimeIsLeftOutWhenThereIsNothingToRepeat(t *testing.T) {
	for name, jobs := range map[string][]model.Job{
		"nothing scheduled": nil,
		"only another person's schedule": {{ID: 5, BatchID: 5, DeviceID: 1, CreatedBy: "test:admin",
			StartAt: time.Date(2026, 11, 4, 2, 0, 0, 0, zoneCET), State: model.JobSucceeded}},
		"only a job that started at once": {{ID: 5, BatchID: 5, DeviceID: 1, CreatedBy: "test:operator", State: model.JobSucceeded}},
		"it equals Tonight": {{ID: 5, BatchID: 5, DeviceID: 1, CreatedBy: "test:operator",
			StartAt: time.Date(2026, 10, 31, 3, 0, 0, 0, zoneCET), State: model.JobSucceeded}},
		"it equals Tomorrow night": {{ID: 5, BatchID: 5, DeviceID: 1, CreatedBy: "test:operator",
			StartAt: time.Date(2026, 11, 1, 3, 0, 0, 0, zoneCET), State: model.JobSucceeded}},
	} {
		t.Run(name, func(t *testing.T) {
			e, _, fj, _ := setupOTAAt(t, fixedClock{friday})
			fj.jobs = append(fj.jobs, jobs...)
			_, body := e.do(t, "POST", "/updates/preview", "operator", startForm(nil))
			if strings.Contains(body, "Like last time") {
				t.Errorf("no Like last time pick expected: %s", plain(body))
			}
			if !strings.Contains(plain(body), "Tonight 03:00") {
				t.Error("the other picks stay")
			}
		})
	}
}

// Why (#604): after 03:00 the next 03:00 is tomorrow's, and the pick must say so; a "Tonight" that pointed
// into the past would be refused, or worse, start at once.
func TestTonightIsTheNextThreeOClock(t *testing.T) {
	at := time.Date(2026, 11, 7, 3, 30, 0, 0, zoneCET) // half past three on Saturday
	e, _, _, _ := setupOTAAt(t, fixedClock{at})
	_, body := e.do(t, "POST", "/updates/preview", "operator", startForm(nil))
	text := plain(body)
	if !strings.Contains(text, "Tonight 03:00 Sun, Nov 8") || !strings.Contains(text, "Tomorrow night 03:00 Mon, Nov 9") {
		t.Errorf("picks after 03:00: %s", text)
	}
}

// Why (#604): 03:00 is the maintenance hour, and a clock-change night is the easy place to get it wrong: a
// day is not always 24 hours. Tonight and Tomorrow night must be 03:00 on the wall clock with the offset
// that is in force at that time, and a time typed into the hour the clocks skip must still be planned at
// the instant the Starts row shows; without JavaScript, where nothing shows it, it is refused (#792).
func TestNightPicksOnAClockChangeNightAreThreeOClockLocal(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("tzdata: %v", err)
	}
	for name, c := range map[string]struct {
		now               time.Time
		tonight, tomorrow string
	}{
		"spring forward": {time.Date(2027, 3, 27, 22, 0, 0, 0, berlin), "2027-03-28T03:00:00+02:00", "2027-03-29T03:00:00+02:00"},
		"fall back":      {time.Date(2026, 10, 24, 22, 0, 0, 0, berlin), "2026-10-25T03:00:00+01:00", "2026-10-26T03:00:00+01:00"},
		// Tomorrow night lies across the change: a day that is 23 or 25 hours long.
		"the night before spring forward": {time.Date(2027, 3, 26, 22, 0, 0, 0, berlin), "2027-03-27T03:00:00+01:00", "2027-03-28T03:00:00+02:00"},
		"the night before fall back":      {time.Date(2026, 10, 23, 22, 0, 0, 0, berlin), "2026-10-24T03:00:00+02:00", "2026-10-25T03:00:00+01:00"},
	} {
		t.Run(name, func(t *testing.T) {
			e, _, _, _ := setupOTAAt(t, fixedClock{c.now})
			_, body := e.do(t, "POST", "/updates/preview", "operator", startForm(nil))
			for label, want := range map[string]string{"Tonight 03:00": c.tonight, "Tomorrow night 03:00": c.tomorrow} {
				m := regexp.MustCompile(`value="([^"]+)"[^>]*><span class="chip-l">` + label).FindStringSubmatch(body)
				if m == nil || m[1] != want {
					t.Errorf("%s must be %s, got %v", label, want, m)
				}
			}
		})
	}

	// Without JavaScript the person never sees the instant their typed time becomes: a successful submit goes
	// straight to the transaction page. Go would read 02:30 on this night as 03:30 CEST and plan that, a time
	// the person did not choose, so the server refuses a time that the clock never shows (#792).
	t.Run("a time in the skipped hour is refused without JavaScript", func(t *testing.T) {
		e, o, _, _ := setupOTAAt(t, fixedClock{time.Date(2027, 3, 27, 22, 0, 0, 0, berlin)})
		form := startForm(url.Values{"when": {"at"}, "at": {"custom"}, "at_date": {"2027-03-28"}, "at_time": {"02:30"}})
		w := parseStart(form, time.Date(2027, 3, 27, 22, 0, 0, 0, berlin))
		if w.Err != "That time does not exist on this date (clock change)." || !w.Start.IsZero() || w.Mode != startsUnset {
			t.Fatalf("the skipped hour must be refused, not moved: err %q start %v mode %q", w.Err, w.Start, w.Mode)
		}
		resp, body := e.do(t, "POST", "/updates", "operator", form)
		if resp.StatusCode != http.StatusUnprocessableEntity || len(o.submitReqs) != 0 {
			t.Fatalf("%d, submitted %v: %s", resp.StatusCode, o.submitReqs, body)
		}
		if text := plain(body); !strings.Contains(text, "That time does not exist on this date (clock change).") || !strings.Contains(body, `value="02:30"`) {
			t.Errorf("the refusal must say why and keep the typed time: %s", text)
		}
	})
	// A time that exists on that night, and the repeated hour of the autumn change, are planned as typed.
	t.Run("the hours around a clock change are planned as typed", func(t *testing.T) {
		for name, c := range map[string]struct {
			now        time.Time
			date, hhmm string
			want       time.Time
		}{
			"after the spring change":     {time.Date(2027, 3, 27, 22, 0, 0, 0, berlin), "2027-03-28", "03:30", time.Date(2027, 3, 28, 3, 30, 0, 0, berlin)},
			"before the spring change":    {time.Date(2027, 3, 27, 22, 0, 0, 0, berlin), "2027-03-28", "01:59", time.Date(2027, 3, 28, 1, 59, 0, 0, berlin)},
			"a repeated hour in autumn":   {time.Date(2026, 10, 24, 22, 0, 0, 0, berlin), "2026-10-25", "02:30", time.Date(2026, 10, 25, 2, 30, 0, 0, berlin)},
			"a one-digit hour as typed":   {time.Date(2027, 3, 27, 22, 0, 0, 0, berlin), "2027-03-28", "3:30", time.Date(2027, 3, 28, 3, 30, 0, 0, berlin)},
			"an ordinary night as before": {time.Date(2027, 3, 20, 22, 0, 0, 0, berlin), "2027-03-21", "02:30", time.Date(2027, 3, 21, 2, 30, 0, 0, berlin)},
		} {
			w := parseStart(startForm(url.Values{"when": {"at"}, "at": {"custom"}, "at_date": {c.date}, "at_time": {c.hhmm}}), c.now)
			if w.Err != "" || !w.Start.Equal(c.want) {
				t.Errorf("%s: err %q start %v, want %v", name, w.Err, w.Start, c.want)
			}
		}
	})
	// With JavaScript the browser posts the instant and the Starts row shows it (03:30) before the submit, so it
	// is planned as shown: the refusal is for the path where the person cannot see what became of the time.
	t.Run("the instant the browser posts is planned", func(t *testing.T) {
		e, o, _, _ := setupOTAAt(t, fixedClock{time.Date(2027, 3, 27, 22, 0, 0, 0, berlin)})
		form := startForm(url.Values{"when": {"at"}, "at": {"custom"}, "start_at": {"2027-03-28T03:30:00+02:00"}, "at_date": {"2027-03-28"}, "at_time": {"02:30"}})
		resp, body := e.do(t, "POST", "/updates", "operator", form)
		want := time.Date(2027, 3, 28, 3, 30, 0, 0, berlin)
		if resp.StatusCode != http.StatusSeeOther || len(o.submitReqs) != 2 || !o.submitReqs[0].StartAt.Equal(want) {
			t.Fatalf("%d %v want %v: %s", resp.StatusCode, o.submitReqs, want, body)
		}
	})
}

// Why (#604): the rule is the same words as for scheduled backups, shown before the person commits, with no
// setting named because the 60 minutes are fixed.
func TestPlanStatesTheRuleOfAMissedStart(t *testing.T) {
	e, _, _, _ := setupOTAAt(t, fixedClock{friday})
	_, body := e.do(t, "POST", "/updates/preview", "operator", startForm(nil))
	want := "If TasmoFleet is down at 03:00, the update starts as soon as TasmoFleet is back, but only until 04:00 " +
		"(60 minutes after the start time). If TasmoFleet is back later than that, the update is not started at all " +
		"and TasmoFleet sends a notification. Devices are checked again when the update starts."
	if !strings.Contains(plain(body), want) {
		t.Errorf("rule text missing in: %s", plain(body))
	}
}

// Why (#604): the quick picks are computed from the server's clock, but a person without JavaScript types the
// time in the server's zone, and the page has to say that or the update starts at a different hour than meant.
func TestWithoutJavaScriptTheZoneIsTheServers(t *testing.T) {
	e, _, _, _ := setupOTAAt(t, fixedClock{friday})
	_, body := e.do(t, "POST", "/updates/preview", "operator", startForm(nil))
	if !strings.Contains(plain(body), "Times are in the server's time zone (CET, UTC+1).") {
		t.Errorf("zone sentence: %s", plain(body))
	}
}

// Why (#604, pitfall 'A UI that differed from the approved mockup'): on a phone the plan sits under the
// device list, so the order is preview, When, plan, buttons. The buttons are the last thing, not a button row
// between the cards.
func TestPhoneOrderIsPreviewWhenPlanButtons(t *testing.T) {
	e, _, _, _ := setupOTAAt(t, fixedClock{friday})
	_, body := e.do(t, "POST", "/updates/preview", "operator", startForm(nil))
	order := []string{`id="prev-h"`, `id="when-h"`, `id="what-h"`, `class="upd-go"`}
	last := -1
	for _, m := range order {
		i := strings.Index(body, m)
		if i < 0 || i < last {
			t.Fatalf("%s is missing or out of order (at %d, after %d)", m, i, last)
		}
		last = i
	}
}

// Why (#604): a start time that is not in the future would start the update at once without the person
// having said so. The server refuses it whatever the browser did, keeps the person's input, and the plan says
// the time is not set.
func TestSubmitRefusesAStartInThePast(t *testing.T) {
	for name, form := range map[string]url.Values{
		"posted by the browser as an instant": {"when": {"at"}, "at": {"custom"}, "start_at": {"2026-11-06T08:00:00+01:00"},
			"at_date": {"2026-11-06"}, "at_time": {"08:00"}},
		// the browser's clock is ahead of the server's: 14:30 in UTC+5:30 is 10:00 in CET, past, although the
		// same digits read in the server's zone would be 14:30 and in the future
		"typed in a zone ahead of the server's": {"when": {"at"}, "at": {"custom"}, "start_at": {"2026-11-06T14:30:00+05:30"},
			"at_date": {"2026-11-06"}, "at_time": {"14:30"}},
		"typed without JavaScript, in the server's zone": {"when": {"at"}, "at": {"custom"}, "at_date": {"2026-11-06"}, "at_time": {"08:00"}},
		"exactly now": {"when": {"at"}, "at": {"custom"}, "start_at": {friday.Format(time.RFC3339)}, "at_date": {"2026-11-06"}, "at_time": {"10:21"}},
	} {
		t.Run(name, func(t *testing.T) {
			e, o, _, _ := setupOTAAt(t, fixedClock{friday})
			o.items[2] = ota.PreviewItem{DeviceID: 2, Decision: "refuse", Reason: "unknown hardware"}
			form["picked"] = []string{"1", "2"}
			resp, body := e.do(t, "POST", "/updates", "operator", startForm(form))
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("%d %s", resp.StatusCode, body)
			}
			if len(o.submitReqs) != 0 {
				t.Fatalf("nothing may be submitted: %+v", o.submitReqs)
			}
			text := plain(body)
			for _, want := range []string{"Choose a time in the future. It is now Fri, Nov 6, 10:21.", "Starts Not set: the time is in the past",
				"attic Refuse Unknown hardware", "Other time"} {
				if !strings.Contains(text, want) {
					t.Errorf("answer must contain %q: %s", want, text)
				}
			}
			if got := goButton(t, body); got != "Schedule update" {
				t.Errorf("button %q, want Schedule update", got)
			}
			if !strings.Contains(body, `value="2026-11-06"`) {
				t.Error("the typed date must stay in the field")
			}
		})
	}
}

// Why (#604): "Start at a chosen time" with nothing chosen must not fall back to starting now.
func TestSubmitRefusesAnUnsetStart(t *testing.T) {
	for name, form := range map[string]url.Values{
		"nothing chosen":     {"when": {"at"}},
		"a date only":        {"when": {"at"}, "at": {"custom"}, "at_date": {"2026-11-07"}},
		"a nonsense instant": {"when": {"at"}, "start_at": {"tonight-ish"}},
	} {
		t.Run(name, func(t *testing.T) {
			e, o, _, _ := setupOTAAt(t, fixedClock{friday})
			resp, body := e.do(t, "POST", "/updates", "operator", startForm(form))
			if resp.StatusCode != http.StatusUnprocessableEntity || len(o.submitReqs) != 0 {
				t.Fatalf("%d, submitted %+v", resp.StatusCode, o.submitReqs)
			}
			if !strings.Contains(plain(body), "Starts Not set yet: choose a quick pick or a time") {
				t.Errorf("plan: %s", plain(body))
			}
		})
	}
}

// Why (#604): the whole batch starts at one moment. Devices of one transaction that were planned for
// different times would be one transaction in name only, and "every device at 03:00" is what was chosen.
func TestSubmitPassesTheSameStartToEveryRequest(t *testing.T) {
	want := time.Date(2026, 11, 7, 3, 0, 0, 0, zoneCET)
	e, o, _, _ := setupOTAAt(t, fixedClock{friday})
	// The browser's zone is not the server's: the instant is what counts.
	resp, body := e.do(t, "POST", "/updates", "operator", startForm(url.Values{"when": {"at"}, "at": {"custom"},
		"start_at": {"2026-11-07T02:00:00Z"}, "at_date": {"2026-11-07"}, "at_time": {"03:00"}}))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if len(o.submitReqs) != 2 {
		t.Fatalf("submitted %+v", o.submitReqs)
	}
	for _, r := range o.submitReqs {
		if !r.StartAt.Equal(want) {
			t.Errorf("device %d starts at %v, want %v", r.DeviceID, r.StartAt, want)
		}
	}
	// The browser keeps its last instant in a hidden field when the person goes back to "Start now"; the
	// radio is the choice, so a stale instant must never schedule the update.
	o.submitReqs = nil
	resp, body = e.do(t, "POST", "/updates", "operator", startForm(url.Values{"when": {"now"}, "start_at": {"2026-11-07T03:00:00+01:00"}}))
	if resp.StatusCode != http.StatusSeeOther || len(o.submitReqs) != 2 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	for _, r := range o.submitReqs {
		if !r.StartAt.IsZero() {
			t.Errorf("start now, device %d: StartAt %v, want zero", r.DeviceID, r.StartAt)
		}
	}
}

// Why (#604): without JavaScript the quick picks still work (their value is the instant), and the date and
// time fields are read in the server's zone; with the browser's instant present, that wins over the fields.
func TestSubmitReadsPicksAndFieldsWithoutJavaScript(t *testing.T) {
	e, o, _, _ := setupOTAAt(t, fixedClock{friday})
	_, page := e.do(t, "POST", "/updates/preview", "operator", startForm(nil))
	tonight := regexp.MustCompile(`value="([^"]+)"[^>]*><span class="chip-l">Tonight 03:00`).FindStringSubmatch(page)
	if tonight == nil {
		t.Fatalf("no Tonight pick in: %s", page)
	}
	for name, c := range map[string]struct {
		form url.Values
		want time.Time
	}{
		"quick pick": {url.Values{"when": {"at"}, "at": {tonight[1]}}, time.Date(2026, 11, 7, 3, 0, 0, 0, zoneCET)},
		"fields":     {url.Values{"when": {"at"}, "at": {"custom"}, "at_date": {"2026-11-09"}, "at_time": {"23:45"}}, time.Date(2026, 11, 9, 23, 45, 0, 0, zoneCET)},
	} {
		t.Run(name, func(t *testing.T) {
			o.submitReqs = nil
			resp, body := e.do(t, "POST", "/updates", "operator", startForm(c.form))
			if resp.StatusCode != http.StatusSeeOther || len(o.submitReqs) != 2 || !o.submitReqs[0].StartAt.Equal(c.want) {
				t.Fatalf("%d %v want %v: %s", resp.StatusCode, o.submitReqs, c.want, body)
			}
		})
	}
}

// Why (#604): the audit trail says when a transaction was planned to start, or a later look at "who started
// the update at 03:00" finds only the moment the button was pressed.
func TestScheduledSubmitIsAuditedWithItsStart(t *testing.T) {
	e, _, _, _ := setupOTAAt(t, fixedClock{friday})
	e.do(t, "POST", "/updates", "operator", startForm(url.Values{"when": {"at"}, "start_at": {"2026-11-07T03:00:00+01:00"}}))
	for _, ev := range e.aud.events {
		if ev.Action == "transaction.start" && strings.Contains(ev.Detail, `"start_at":"2026-11-07T02:00:00Z"`) {
			return
		}
	}
	t.Fatalf("no transaction.start with the start time in %+v", e.aud.events)
}

// Why (#604): a job that did not start because TasmoFleet was down was cancelled by nobody. The step label
// of its device says "Not started" like the badge does, not "Cancelled".
func TestNotStartedJobReadsNotStartedOnTheProgressBar(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{friday})
	jobs.jobs = []model.Job{
		{ID: 1, BatchID: 7, DeviceID: 1, State: model.JobCancelled, NotStarted: true, StartAt: friday.Add(-time.Hour), CreatedAt: friday},
		{ID: 2, BatchID: 7, DeviceID: 2, State: model.JobCancelled, CreatedAt: friday},
	}
	h := &handler{d: Deps{Jobs: jobs, Inventory: e.inv}, log: slog.Default()}
	views, _, err := h.jobViews(httptest.NewRequest(http.MethodGet, "/transactions/7", nil), store.JobFilter{BatchID: 7})
	if err != nil || len(views) != 2 {
		t.Fatalf("%v %d", err, len(views))
	}
	if views[0].StepLabel != "Not started" || views[1].StepLabel != "Cancelled" {
		t.Errorf("step labels %q and %q, want \"Not started\" and \"Cancelled\"", views[0].StepLabel, views[1].StepLabel)
	}
}

// Why (#604, #186): the Start form posts a remembered build as build_<id>, only so that Start flashes what the
// screen showed. When the server refuses the time and draws the preview again, that echo must not count as the
// person's own pick: the row has to stay "remembered from your last update" with Change and Forget, and the
// Back (change) form must not turn the memory into a posted pick.
func TestRefusedStartKeepsARememberedBuildRemembered(t *testing.T) {
	e, _, _, _ := setupOTAAt(t, fixedClock{friday})
	e.inv.devs[4] = model.Device{ID: 4, Name: "s2-board", Address: "10.0.0.4", BuildMemory: "s2cdc",
		Identity: model.Identity{LanguageKnown: true, Family: model.ChipESP32S2, Hardware: "ESP32-S2", Version: "16.0.0", Variant: "unknown"}}
	form := url.Values{"id": {"4"}, "picked": {"4"}, "release": {"16.1.0"}, "policy": {"only-newer"}, "build_4": {"s2cdc"},
		"when": {"at"}, "at": {"custom"}, "at_date": {"2026-11-06"}, "at_time": {"08:00"}}
	resp, body := e.do(t, "POST", "/updates", "operator", form)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "remembered from your last update") || strings.Contains(body, "chosen by you") {
		t.Errorf("the build must stay remembered, not become a pick: %s", plain(body))
	}
	if !strings.Contains(body, `name="forget_build"`) {
		t.Error("Forget must stay on the remembered row")
	}
	change, _, _ := strings.Cut(body[strings.Index(body, `<form id="change"`):], "</form>")
	if strings.Contains(change, "build_4") {
		t.Errorf("Back (change) must not carry the memory as a pick: %s", change)
	}
}
