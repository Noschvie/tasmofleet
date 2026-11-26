package web

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/store"
)

// The Transactions pages of a scheduled update (#604, package 2). "Now" is the mockup's Friday 10:21; the
// server's zone is UTC in these tests (main_test.go), so the fallback texts of the <time> elements read in UTC.
var (
	fri = time.Date(2026, 11, 6, 10, 21, 0, 0, time.UTC)
	sat = time.Date(2026, 11, 7, 3, 0, 0, 0, time.UTC)
	sun = time.Date(2026, 11, 8, 3, 0, 0, 0, time.UTC)
)

// schedJob is one device of a scheduled update transaction.
func schedJob(id model.JobID, batch model.BatchID, dev model.DeviceID, state model.JobState, start time.Time) model.Job {
	return model.Job{ID: id, BatchID: batch, DeviceID: dev, State: state, StartAt: start, TargetRelease: "16.1.0",
		IdentityBefore: model.Identity{Version: "16.0.0"}, Policy: model.PolicyOnlyNewer, CreatedBy: "adam", CreatedAt: fri, UpdatedAt: fri}
}

func eventOf(job model.JobID, at time.Time, from, to model.JobState, ev, detail string) model.JobEvent {
	return model.JobEvent{JobID: job, At: at, From: from, To: to, Event: ev, Detail: detail}
}

var stateBadge = regexp.MustCompile(`id="txn-state" class="([^"]*)">([^<]*)<`)

// txnState is the transaction-level badge: its tone class and its word.
func txnState(t *testing.T, body string) (class, word string) {
	t.Helper()
	m := stateBadge.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no transaction badge in: %s", body)
	}
	return m[1], m[2]
}

// Why (#604): a plan nobody can see is a plan nobody can cancel. Transactions lists what is planned, soonest
// first, with its start and a Cancel that says what it does; it is not a running transaction, so it is not in
// the running cards and nothing on the page calls it queued or running.
func TestTransactionsListsScheduledJobsSoonestFirstAboveHistory(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	jobs.jobs = []model.Job{
		schedJob(22, 9, 3, model.JobScheduled, sun), // newer, but starts later
		schedJob(20, 10, 1, model.JobScheduled, sat),
		schedJob(21, 10, 2, model.JobScheduled, sat),
		schedJob(1, 7, 1, model.JobSucceeded, time.Time{}),
	}
	jobs.jobs[3].CreatedAt = fri.Add(-48 * time.Hour)
	_, body := e.do(t, "GET", "/transactions", "operator", nil)
	text := plain(body)
	for _, want := range []string{"Scheduled · 2", "TX-0010 Update to Tasmota 16.1.0 2 devices", "Starts Sat 03:00", "Nov 7 · in 17 h · adam", "Starts Sun 03:00", "Nov 8 · in 1 d 17 h"} {
		if !strings.Contains(text, want) {
			t.Errorf("the Scheduled section lacks %q in: %s", want, text)
		}
	}
	if i, j, k := strings.Index(text, "TX-0010"), strings.Index(text, "TX-0009"), strings.Index(text, "History"); i < 0 || i >= j || j >= k {
		t.Errorf("order: TX-0010 at %d, TX-0009 at %d, History at %d: soonest start first, above History", i, j, k)
	}
	for _, bad := range []string{"Running", "Queued", "queued", "in progress", "upd-run"} {
		if strings.Contains(body, bad) {
			t.Errorf("a scheduled update reads as active (%q) on the list", bad)
		}
	}
	cancel := tagOf(t, body, `data-dlg-title="Cancel TX-0010?"`)
	for _, want := range []string{"It will not start and nothing on the devices changes", `data-dlg-action="/tf/transactions/10/cancel"`, `data-dlg-verb="Cancel schedule"`, `data-dlg-dismiss="Keep schedule"`} {
		if !strings.Contains(cancel, want) {
			t.Errorf("the Cancel of TX-0010 lacks %q in: %s", want, cancel)
		}
	}
	// the start is a <time> for time.js: the browser's zone, like every other time of the app
	if !strings.Contains(body, `data-ts="weekday-time"`) {
		t.Errorf("the start is plain text: it would read in the server's zone")
	}
}

// Why (#604): the three ways a planned update ends read differently in History. Not started is a warning
// (nobody cancelled it and it did not run), cancelled is neutral, and an update that ran says it was planned.
func TestTransactionsHistoryWordsTheOutcomesOfPlannedUpdates(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	due := time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)
	ran := time.Date(2026, 11, 3, 3, 0, 0, 0, time.UTC)
	j8 := schedJob(30, 8, 1, model.JobCancelled, due)
	j8.NotStarted, j8.UpdatedAt = true, due.Add(72*time.Minute)
	j7 := schedJob(31, 7, 2, model.JobCancelled, time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC))
	j7.UpdatedAt = time.Date(2026, 11, 3, 18, 40, 0, 0, time.UTC) // cancelled before its start
	j6 := schedJob(32, 6, 3, model.JobSucceeded, ran)
	j6.UpdatedAt = ran.Add(10 * time.Second)
	jobs.jobs = []model.Job{j8, j7, j6}
	_, body := e.do(t, "GET", "/transactions", "operator", nil)
	if strings.Contains(body, "sched-h") {
		t.Errorf("nothing waits for its start, yet an empty Scheduled section is drawn")
	}
	row := func(id string) string { return txHistoryRow(t, body, id) }
	if r := row("TX-0008"); !strings.Contains(r, `bdg t-warn">Not started<`) || !strings.Contains(plain(r), "was due") {
		t.Errorf("TX-0008 (not started) reads: %s", r)
	}
	if r := row("TX-0007"); !strings.Contains(r, `bdg t-neutral">Cancelled<`) || !strings.Contains(plain(r), "was due") {
		t.Errorf("TX-0007 (cancelled before its start) reads: %s", r)
	}
	if r := row("TX-0006"); !strings.Contains(r, `bdg t-ok">`) || !strings.Contains(plain(r), "· scheduled") || strings.Contains(plain(r), "was due") {
		t.Errorf("TX-0006 (ran as planned) reads: %s", r)
	}
}

// Why (#604): the page of a plan says when it starts and what happens if TasmoFleet is down then (the words of
// the preview), shows its devices as "Planned" (they are not queued: they hold no slot) and offers only Cancel.
// The transaction as a whole reads "Scheduled".
func TestScheduledTransactionPage(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	jobs.jobs = []model.Job{schedJob(20, 10, 1, model.JobScheduled, sat), schedJob(21, 10, 2, model.JobScheduled, sat)}
	_, body := e.do(t, "GET", "/transactions/10", "operator", nil)
	text := plain(body)
	if class, word := txnState(t, body); word != "Scheduled" || !strings.Contains(class, "t-accent") {
		t.Errorf("transaction badge %q (%s), want Scheduled in the accent tone", word, class)
	}
	for _, want := range []string{
		"Starts Sat, Nov 7 at 03:00 · in 17 h",
		"If TasmoFleet is down at 03:00, the update starts as soon as TasmoFleet is back, but only until 04:00 (60 minutes after the start time). If TasmoFleet is back later than that, the update is not started at all and TasmoFleet sends a notification. Devices are checked again when the update starts.",
		"Until it starts you can cancel it here.",
		"planned by adam",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the page lacks %q in: %s", want, text)
		}
	}
	if n := strings.Count(body, `bdg t-neutral">Planned<`); n != 2 {
		t.Errorf("%d devices read Planned in the neutral tone, want 2", n)
	}
	for _, bad := range []string{"Queued", "queued", "Waiting for a free slot", "Retry failed", "/retry", "Cancel remaining", "Log ·", "in progress", `every 3s`} {
		if strings.Contains(body, bad) {
			t.Errorf("the page of a plan shows %q", bad)
		}
	}
	cancel := tagOf(t, body, `data-dlg-title="Cancel TX-0010?"`)
	for _, want := range []string{"It will not start and nothing on the devices changes", `data-dlg-action="/tf/transactions/10/cancel"`, `data-dlg-verb="Cancel schedule"`} {
		if !strings.Contains(cancel, want) {
			t.Errorf("Cancel lacks %q in: %s", want, cancel)
		}
	}
	if strings.Contains(body, "Change time") {
		t.Errorf("a plan is changed by cancelling and planning again, not by a Change time")
	}
}

// Why (#604): the cancel of a plan (its own page without JavaScript) must not promise what a running update
// does ("finish their update").
func TestCancelConfirmOfAPlanSaysNothingChanges(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	jobs.jobs = []model.Job{schedJob(20, 10, 1, model.JobScheduled, sat)}
	for _, path := range []string{"/transactions/10/cancel", "/jobs/20/cancel"} {
		resp, body := e.do(t, "GET", path, "operator", nil)
		if resp.StatusCode != http.StatusOK || !strings.Contains(plain(body), "It will not start and nothing on the devices changes") || strings.Contains(body, "finish their update") || strings.Contains(body, "running flash") {
			t.Errorf("%s: %d %s", path, resp.StatusCode, body)
		}
	}
}

// Why (#604): an update that started at its time says so, and how late: the person who planned it wants to
// know it ran when it should.
func TestStartedOnTimeHasItsLine(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	tue := time.Date(2026, 11, 3, 3, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		late time.Duration
		want string
	}{
		{0, "Started on time, at the planned 03:00."},
		{2 * time.Second, "Started on time, 2 seconds after the planned 03:00."},
		{12 * time.Minute, "Started 12 minutes after the planned 03:00, within the 60 minutes a late start is allowed."},
	} {
		j1, j2 := schedJob(40, 6, 1, model.JobSucceeded, tue), schedJob(41, 6, 2, model.JobSucceeded, tue)
		jobs.jobs = []model.Job{j1, j2}
		jobs.events = map[model.JobID][]model.JobEvent{
			40: {eventOf(40, tue.Add(c.late), model.JobScheduled, model.JobQueued, "start_due", "planned for Tue, Nov 3 at 03:00, started "+c.late.String()+" after it")},
			41: {eventOf(41, tue.Add(c.late), model.JobScheduled, model.JobQueued, "start_due", "planned for Tue, Nov 3 at 03:00")},
		}
		_, body := e.do(t, "GET", "/transactions/6", "operator", nil)
		text := plain(body)
		if !strings.Contains(text, c.want) {
			t.Errorf("late %s: the page lacks %q in: %s", c.late, c.want, text)
		}
		if !strings.Contains(text, "planned by adam for Tue, Nov 3 at 03:00") || !strings.Contains(text, "started 03:") {
			t.Errorf("late %s: the header does not say what was planned and when it started: %s", c.late, text)
		}
		if _, word := txnState(t, body); word != "Succeeded" {
			t.Errorf("late %s: badge %q: an update that ran is a normal transaction", c.late, word)
		}
		if !strings.Contains(text, "Started as planned (planned for 03:00") && c.late < time.Minute {
			t.Errorf("the log lacks the line of the start: %s", text)
		}
	}
}

// Why (#604): TasmoFleet was down and nobody cancelled this: the page is a warning with the reason, the
// text of the notification that went out, and the one way on: plan it again. Retrying it would start it at
// once, which is not what the person planned, so there is no Retry.
func TestNotStartedTransactionPage(t *testing.T) {
	e, o, jobs, _ := setupOTAAt(t, fixedClock{fri})
	due := time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)
	back := due.Add(72 * time.Minute)
	jobs.jobs = nil
	for _, id := range []model.JobID{50, 51} {
		j := schedJob(id, 8, model.DeviceID(id-49), model.JobCancelled, due)
		j.NotStarted, j.UpdatedAt, j.SkipBackup = true, back, true
		j.LastError = "TasmoFleet was down at the planned start"
		jobs.jobs = append(jobs.jobs, j)
	}
	jobs.events = map[model.JobID][]model.JobEvent{
		50: {eventOf(50, back, model.JobScheduled, model.JobCancelled, "not_started", "x")},
		51: {eventOf(51, back, model.JobScheduled, model.JobCancelled, "not_started", "x")},
	}
	_, body := e.do(t, "GET", "/transactions/8", "operator", nil)
	text := plain(body)
	if class, word := txnState(t, body); word != "Not started" || !strings.Contains(class, "t-warn") {
		t.Errorf("badge %q (%s), want Not started in the warn tone", word, class)
	}
	banner := between(body, `class="banner t-warn"`, `</div></div>`)
	for _, want := range []string{"Not started.", "TasmoFleet was down at the planned start", "Thu, Nov 5 at 03:00", "was not back within 60 minutes", "came back at", "04:12", "No device was changed, and this update will not start later."} {
		if !strings.Contains(plain(banner), want) {
			t.Errorf("the warning lacks %q: %s", want, plain(banner))
		}
	}
	for _, want := range []string{
		"Scheduled update not started",
		"TX-0008 (Update to Tasmota 16.1.0, 2 devices) was planned for Thu, Nov 5 at 03:00. TasmoFleet was down and was not back within 60 minutes, so the update was not started and no device was changed. Plan it again to run it.",
		"2 not started",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the page lacks %q in: %s", want, text)
		}
	}
	if n := len(regexp.MustCompile(`t-warn(?: bdg-lg)?">Not started<`).FindAllString(body, -1)); n < 3 { // the transaction (large) and two devices
		t.Errorf("%d \"Not started\" badges in the warn tone, want the transaction and both devices", n)
	}
	if strings.Contains(body, "Retry failed") || strings.Contains(body, "/transactions/8/retry") || strings.Contains(body, "Log ·") {
		t.Errorf("a batch that never started offers a Retry or a log: %s", text)
	}
	// Plan again opens the preview with the same devices and the same release, and the time choice of the plan
	// (never the default "Start now": see TestPlanAgainOpensOnTheTimeChoiceOfThePlan)
	form := between(body, `<form method="post" action="/tf/updates/preview"`, `</form>`)
	for _, want := range []string{`name="id" value="1"`, `name="id" value="2"`, `name="release" value="16.1.0"`, `name="skip_backup" value="1"`, `Plan again…`} {
		if !strings.Contains(form, want) {
			t.Errorf("Plan again lacks %q in: %s", want, form)
		}
	}
	if !strings.Contains(form, `name="when" value="at"`) || !strings.Contains(form, `name="start_at" value="2026-11-05T03:00:00Z"`) {
		t.Errorf("Plan again does not carry the time choice of the plan: %s", form)
	}
	// the retry itself is refused too: it would start the update at once
	resp, rbody := e.do(t, "POST", "/transactions/8/retry", "admin", url.Values{})
	if resp.StatusCode != http.StatusConflict || len(o.retryCalls) != 0 || !strings.Contains(plain(rbody), "Plan again") {
		t.Errorf("retry of a not-started batch: %d, calls %v: %s", resp.StatusCode, o.retryCalls, plain(rbody))
	}
}

// Why (#604): a plan that a person cancelled before its time is not an error and not a retry: it says who
// cancelled it and when, and offers to plan it again.
func TestCancelledBeforeStartTransactionPage(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	start := time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)
	at := time.Date(2026, 11, 3, 18, 40, 0, 0, time.UTC)
	jobs.jobs = nil
	for _, id := range []model.JobID{60, 61} {
		j := schedJob(id, 7, model.DeviceID(id-59), model.JobCancelled, start)
		j.CreatedBy, j.UpdatedAt = "olga", at
		jobs.jobs = append(jobs.jobs, j)
		jobs.events = map[model.JobID][]model.JobEvent{
			60: {eventOf(60, at, model.JobScheduled, model.JobCancelled, "cancel", "cancelled by olga")},
			61: {eventOf(61, at, model.JobScheduled, model.JobCancelled, "cancel", "cancelled by olga")},
		}
	}
	_, body := e.do(t, "GET", "/transactions/7", "operator", nil)
	text := plain(body)
	if class, word := txnState(t, body); word != "Cancelled before start" || !strings.Contains(class, "t-neutral") {
		t.Errorf("badge %q (%s), want Cancelled before start in the neutral tone", word, class)
	}
	for _, want := range []string{"planned by olga for Wed, Nov 4 at 03:00", "cancelled by olga", "2 cancelled", "Plan again…"} {
		if !strings.Contains(text, want) {
			t.Errorf("the page lacks %q in: %s", want, text)
		}
	}
	if strings.Contains(body, "Retry failed") || strings.Contains(body, "/transactions/7/retry") || strings.Contains(text, "Not started") {
		t.Errorf("a plan cancelled by a person offers a retry or reads Not started: %s", text)
	}
}

// Why (#604): a plan that is half cancelled (one device by hand) is still a plan: the others are waiting.
func TestHalfCancelledPlanIsStillScheduled(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	c := schedJob(71, 10, 2, model.JobCancelled, sat)
	jobs.jobs = []model.Job{schedJob(70, 10, 1, model.JobScheduled, sat), c}
	_, body := e.do(t, "GET", "/transactions", "operator", nil)
	if text := plain(body); !strings.Contains(text, "Scheduled · 1") || !strings.Contains(text, "TX-0010 Update to Tasmota 16.1.0 1 device Starts") {
		t.Errorf("the plan with one device left is not in the Scheduled section: %s", text)
	}
	if n := strings.Count(body, "TX-0010"); n > 4 { // id link, title link, cancel title, aria-label: once in History would add more
		t.Errorf("TX-0010 is listed twice (%d mentions)", n)
	}
}

// Why (#604, #603): the engine marks a device that was busy for the whole late-start window, or excluded from
// updates after the plan, with the event guard_refused. Drawn in red as "Guard refused" it reads like a safety
// stop of the firmware check, which it is not: say what happened.
func TestGuardRefusedAtAScheduledStartIsWordedForWhatItIs(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	busy := "the device was not free from the planned start (Sat, Nov 7 at 03:00) until 60 minutes after it, so the update was not started: another job holds the device"
	excluded := "excluded from OTA since the update was planned; it was not started"
	for _, c := range []struct{ detail, want string }{
		{busy, "Device was busy"},
		{excluded, "Excluded from updates since the plan"},
	} {
		j := schedJob(80, 10, 1, model.JobFailed, sat)
		j.LastError, j.UpdatedAt = c.detail, sat.Add(61*time.Minute)
		jobs.jobs = []model.Job{j}
		jobs.events = map[model.JobID][]model.JobEvent{80: {eventOf(80, sat.Add(61*time.Minute), model.JobScheduled, model.JobFailed, "guard_refused", c.detail)}}
		_, body := e.do(t, "GET", "/transactions/10", "operator", nil)
		text := plain(body)
		if !strings.Contains(text, c.want) || strings.Contains(text, "Guard refused") {
			t.Errorf("%q: the log reads: %s", c.want, text)
		}
	}
	// the firmware check's own refusal keeps its name
	j := schedJob(81, 11, 1, model.JobFailed, time.Time{})
	jobs.jobs = []model.Job{j}
	jobs.events = map[model.JobID][]model.JobEvent{81: {eventOf(81, fri, model.JobPreflight, model.JobFailed, "guard_refused", "family mismatch")}}
	if _, body := e.do(t, "GET", "/transactions/11", "operator", nil); !strings.Contains(plain(body), "Guard refused") {
		t.Errorf("the refusal of the firmware check lost its name: %s", plain(body))
	}
}

// Why (#604): until its start a plan holds its device against other TasmoFleet jobs, but the device stays
// usable (console, power, settings). Nothing on the device page may say "being updated" or switch the tabs off.
func TestADeviceWithAPlannedUpdateStaysUsable(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	jobs.jobs = []model.Job{schedJob(20, 10, 1, model.JobScheduled, sat)}
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if strings.Contains(body, "Unavailable while") || strings.Contains(body, "is being updated") {
		t.Errorf("a planned update blocks the device page: %s", body)
	}
}

// Why (#604): the picker disables a device the engine would refuse; a device with a plan is refused, and the
// picker says which plan, so it does not read like a bug.
func TestPickerNamesThePlanThatHoldsADevice(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	jobs.jobs = []model.Job{schedJob(20, 10, 1, model.JobScheduled, sat)}
	_, body := e.do(t, "GET", "/updates/new", "operator", nil)
	if !strings.Contains(body, `aria-label="Select kitchen" disabled`) || !strings.Contains(plain(body), "Planned in TX-0010") {
		t.Errorf("the device with a plan is selectable or unexplained: %s", body)
	}
}

// Why (#604): "Make compliant" is one of the TasmoFleet jobs a plan keeps off its device. The fix page lists
// the device as planned, with the transaction, instead of offering a change that would be refused.
func TestPolicyFixShowsADeviceWithAPlannedUpdateAsPlanned(t *testing.T) {
	fix := &fakeFix{}
	e := fixEnv(t, fix, &fakeJobs{jobs: []model.Job{schedJob(20, 10, 1, model.JobScheduled, sat)}})
	body := previewFor(t, e, url.Values{"all": {"1"}})
	if !strings.Contains(plain(body), "Planned: update TX-0010 starts Sat, Nov 7 at 03:00") || strings.Contains(body, "is running on this device") {
		t.Fatalf("preview %s", plain(body))
	}
}

// planFixture is the jobs of the plan tests: two plans (TX-0010 for three devices on Saturday, TX-0011 for one on
// Sunday), a cancelled plan (TX-0012), and a transaction that began (TX-0013: one device is queued, one still
// waits), which must not count as a plan although its start is earlier than both.
func planFixture() []model.Job {
	cancelled := schedJob(24, 12, 1, model.JobCancelled, sat)
	cancelled.UpdatedAt = fri.Add(time.Hour)
	return []model.Job{
		schedJob(20, 10, 1, model.JobScheduled, sat), schedJob(21, 10, 2, model.JobScheduled, sat), schedJob(22, 10, 3, model.JobScheduled, sat),
		schedJob(23, 11, 1, model.JobScheduled, sun),
		cancelled,
		schedJob(25, 13, 1, model.JobScheduled, fri.Add(30*time.Minute)), schedJob(26, 13, 2, model.JobQueued, fri.Add(30*time.Minute)),
	}
}

// planLine is the plan line of the Dashboard card, as text.
func planLine(body string) string {
	return plain(between(body, `<h2>Transactions</h2>`, `</section>`))
}

// Why (#707): someone who never opens Transactions must still see that an update starts tonight. The Dashboard
// and both parts of the status bar (the wide bar and the phone sheet) say how many plans wait and when the next
// one starts, and link to the Scheduled section. The count is per transaction (as "Scheduled · 2" in
// Transactions), not per device, and a transaction that began is a running job, not a plan.
func TestDashboardAndStatusBarShowScheduledPlans(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	jobs.jobs = planFixture()
	_, body := e.do(t, "GET", "/", "operator", nil)
	card := planLine(body)
	if !strings.Contains(card, "2 scheduled") || !strings.Contains(card, "next Sat, Nov 7 at 03:00") || !strings.Contains(card, "in 17 h") {
		t.Errorf("the Dashboard card does not say two plans and the next start: %s", card)
	}
	if strings.Contains(card, "4 scheduled") || strings.Contains(card, "TX-0010") {
		t.Errorf("the Dashboard counts devices, or lists the plan as a transaction: %s", card)
	}
	if !strings.Contains(card, "TX-0013") {
		t.Errorf("the transaction that began is not a running job on the Dashboard: %s", card)
	}
	if !strings.Contains(between(body, `<h2>Transactions</h2>`, `</section>`), `href="/tf/transactions#scheduled"`) {
		t.Errorf("the Dashboard line does not link to the Scheduled section")
	}
	bar := barOf(body)
	wide, sheet := between(bar, `id="sb-wide"`, `<details`), between(bar, `<details`, `</details>`)
	for name, part := range map[string]string{"wide bar": wide, "phone sheet": sheet} {
		text := plain(part)
		if !strings.Contains(text, "2 scheduled") || !strings.Contains(text, "Sat, Nov 7 at 03:00") {
			t.Errorf("%s does not say two plans and the next start: %s", name, text)
		}
		if strings.Contains(text, "4 scheduled") {
			t.Errorf("%s counts devices: %s", name, text)
		}
		if !strings.Contains(part, `href="/tf/transactions#scheduled"`) {
			t.Errorf("%s does not link to the Scheduled section: %s", name, part)
		}
	}
	if !strings.Contains(plain(wide), "Update 0 of 2 devices") {
		t.Errorf("the running transaction left the bar's jobs: %s", plain(wide))
	}
	// the one-line summary of the phone says what runs, not that something is planned
	if sum := plain(between(bar, `<summary>`, `</summary>`)); strings.Contains(sum, "scheduled") {
		t.Errorf("the phone line talks of plans: %s", sum)
	}
}

// Why (#707): a plan on the Dashboard that no longer exists is worse than none. Once a plan is cancelled, has
// started, or was not started, nothing of it is left on the Dashboard or in the bar; and without any plan the
// card's "No transactions yet" is back, so the card never says both.
func TestPlansLeaveDashboardAndStatusBarWhenCancelledOrStarted(t *testing.T) {
	notStarted := func(j *model.Job) {
		j.State, j.NotStarted, j.UpdatedAt = model.JobCancelled, true, sat.Add(2*time.Hour)
	}
	for _, c := range []struct {
		name   string
		change func(*model.Job)
	}{
		{"cancelled", func(j *model.Job) { j.State, j.UpdatedAt = model.JobCancelled, fri.Add(time.Hour) }},
		{"started", func(j *model.Job) { j.State = model.JobQueued }},
		{"not started", notStarted},
		{"failed at the start", func(j *model.Job) { j.State = model.JobFailed }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
			jobs.jobs = []model.Job{schedJob(20, 10, 1, model.JobScheduled, sat)}
			_, body := e.do(t, "GET", "/", "operator", nil)
			if !strings.Contains(planLine(body), "1 scheduled") || !strings.Contains(plain(barOf(body)), "1 scheduled") {
				t.Fatalf("the plan is not shown before it changes: %s", planLine(body))
			}
			if strings.Contains(planLine(body), "No transactions yet") {
				t.Errorf("the card says there are no transactions while a plan waits: %s", planLine(body))
			}
			c.change(&jobs.jobs[0])
			_, body = e.do(t, "GET", "/", "operator", nil)
			if strings.Contains(planLine(body), "scheduled") || strings.Contains(plain(barOf(body)), "scheduled") {
				t.Errorf("the plan is still shown after it was %s: %s / %s", c.name, planLine(body), plain(barOf(body)))
			}
		})
	}
	t.Run("none", func(t *testing.T) {
		e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
		jobs.jobs = nil
		_, body := e.do(t, "GET", "/", "operator", nil)
		if !strings.Contains(planLine(body), "No transactions yet") || strings.Contains(planLine(body)+plain(barOf(body)), "scheduled") {
			t.Errorf("nothing is planned, but: %s / %s", planLine(body), plain(barOf(body)))
		}
	})
}

// Why (#707): a failed read of the plans must not take a page down. The bar already shows nothing for a job it
// cannot read; the Dashboard drops the line and stays up.
func TestAFailedPlanReadLeavesTheDashboardAndTheBarUp(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	jobs.jobs = planFixture()
	if _, body := e.do(t, "GET", "/", "operator", nil); !strings.Contains(planLine(body), "2 scheduled") || !strings.Contains(plain(barOf(body)), "2 scheduled") {
		t.Fatalf("the plans are not shown while the read works: %s", planLine(body))
	}
	jobs.listErr = func(f store.JobFilter) error {
		if slices.Contains(f.States, model.JobScheduled) {
			return errors.New("database is locked")
		}
		return nil
	}
	resp, body := e.do(t, "GET", "/", "operator", nil)
	if resp.StatusCode != http.StatusOK || strings.Contains(planLine(body), "scheduled") || strings.Contains(plain(barOf(body)), "scheduled") {
		t.Errorf("status %d, a failed read shows a plan or no page: %s", resp.StatusCode, planLine(body))
	}
	if strings.Contains(body, "database is locked") {
		t.Error("the error text reaches the page")
	}
}

// Why (#792, #707): a plan whose start has come waits for its device up to 60 minutes. The Dashboard says "due",
// not a countdown to a time that has passed; the bar keeps to the start time, which stays right.
func TestAPlanPastItsStartIsDueOnTheDashboard(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{sat.Add(40 * time.Minute)})
	jobs.jobs = []model.Job{schedJob(20, 10, 1, model.JobScheduled, sat)}
	_, body := e.do(t, "GET", "/", "operator", nil)
	if card := planLine(body); !strings.Contains(card, "1 scheduled") || !strings.Contains(card, "due, waiting for the device") || strings.Contains(card, " in ") {
		t.Errorf("the Dashboard counts down to a start that has passed: %s", card)
	}
	if !strings.Contains(plain(barOf(body)), "Sat, Nov 7 at 03:00") {
		t.Errorf("the bar lost the start of a plan that is due: %s", plain(barOf(body)))
	}
}

// Why (#707): the Dashboard and the bar link to "the Scheduled section", so it needs an address of its own.
func TestTransactionsScheduledSectionIsALinkTarget(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	jobs.jobs = []model.Job{schedJob(20, 10, 1, model.JobScheduled, sat)}
	_, body := e.do(t, "GET", "/transactions", "operator", nil)
	if tag := tagOf(t, body, "upd-sched"); !strings.Contains(tag, `id="scheduled"`) {
		t.Errorf("the Scheduled section has no id to link to: %q", tag)
	}
}

// Why (#604): the About page counts jobs at work; a plan is not at work, not even if a list that includes it
// is handed over. It must never read "running".
func TestAboutDoesNotCountAPlanAsRunning(t *testing.T) {
	c := jobsComp([]model.Job{schedJob(20, 10, 1, model.JobScheduled, sat)}, nil)
	if c.Detail != "Idle" || strings.Contains(c.Detail, "running") {
		t.Errorf("Background jobs reads %q with only a plan", c.Detail)
	}
	c = jobsComp([]model.Job{schedJob(20, 10, 1, model.JobScheduled, sat), schedJob(21, 11, 2, model.JobPreflight, time.Time{}), schedJob(22, 11, 3, model.JobQueued, time.Time{})}, nil)
	if c.Detail != "1 job running · 1 job queued" {
		t.Errorf("Background jobs reads %q, want the plan left out", c.Detail)
	}
}

// Why (#604): a bulk result row words a job by its state; a plan is "Planned", never "Running".
func TestBulkResultRowDoesNotCallAPlanRunning(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	h := &handler{d: Deps{Jobs: jobs, Inventory: e.inv}, log: slog.Default()}
	var rv bulkResultView
	row := h.jobResultRow(httptest.NewRequest(http.MethodGet, "/", nil), jobView{Job: schedJob(20, 10, 1, model.JobScheduled, sat)}, &rv)
	if row.Label != "Planned" {
		t.Errorf("a plan is labelled %q in a bulk result", row.Label)
	}
}

// Why (#604, #466): a delete under a plan makes the store refuse after "Delete the backups too" has run.
// The pre-check names the plan and its start, and nothing is deleted.
func TestDeletingADeviceWithAPlannedUpdateIsRefusedBeforeAnyBackupGoes(t *testing.T) {
	e, k := busyEnv(t, []model.Job{schedJob(20, 10, 1, model.JobScheduled, sat)}, nil)
	_, body := e.do(t, "GET", "/devices/1/delete", "admin", nil)
	text := plain(body)
	if !strings.Contains(text, "TX-0010") || !strings.Contains(text, "Sat, Nov 7 at 03:00") || !strings.Contains(text, "planned") || !strings.Contains(body, `href="/tf/transactions/10"`) {
		t.Fatalf("the delete page does not name the plan and its start:\n%.1800s", text)
	}
	if b := deleteButton.FindString(body); !strings.Contains(b, "disabled") {
		t.Errorf("Delete device stays enabled: %q", b)
	}
	resp, rbody := e.do(t, "POST", "/devices/1/delete", "admin", url.Values{"backups": {"delete"}, "ack": {"1"}})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(plain(rbody), "TX-0010") {
		t.Fatalf("status %d: %.800s", resp.StatusCode, plain(rbody))
	}
	if len(k.all) != 0 {
		t.Fatalf("the backups were deleted (%v) although the device stayed", k.all)
	}
	if _, err := e.inv.Get(t.Context(), 1); err != nil {
		t.Fatal("the refused delete removed the device")
	}
}

// Why (#792): the engine retries a start the device refused every 30 seconds for up to 60 minutes, so a plan
// can be past its start and still scheduled for a long time. "in 1 min" for that whole time tells the person
// the start is about to come when it is already late; the start is due and waits for the device.
func TestAPlanPastItsStartSaysItIsDueNotInOneMinute(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{sat.Add(40 * time.Minute)}) // 03:40, the start was 03:00
	jobs.jobs = []model.Job{schedJob(20, 10, 1, model.JobScheduled, sat), schedJob(21, 10, 2, model.JobScheduled, sat)}
	for _, path := range []string{"/transactions", "/transactions/10"} {
		_, body := e.do(t, "GET", path, "operator", nil)
		text := plain(body)
		if !strings.Contains(text, "due, waiting for the device") {
			t.Errorf("%s: a plan past its start does not say it is due: %s", path, text)
		}
		if strings.Contains(text, "in 1 min") || strings.Contains(text, " in 0 ") {
			t.Errorf("%s: a plan 40 minutes past its start still counts down: %s", path, text)
		}
	}
}

// Why (#792): a plan whose start is still ahead keeps its countdown; the due wording is only for a start
// that has come.
func TestAPlanBeforeItsStartKeepsItsCountdown(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{sat.Add(-5 * time.Minute)})
	jobs.jobs = []model.Job{schedJob(20, 10, 1, model.JobScheduled, sat)}
	_, body := e.do(t, "GET", "/transactions", "operator", nil)
	if text := plain(body); !strings.Contains(text, "in 5 min") || strings.Contains(text, "due, waiting") {
		t.Errorf("a plan 5 minutes ahead reads: %s", text)
	}
}

// Why (#792): a plan that a person cancelled after its start time, before the engine queued it (the device
// was refused at the start and the engine was retrying), never ran. Judged by its UpdatedAt it looked like
// an ordinary cancellation: Retry was offered and would have started the update at once, which is what the
// plan was never allowed to do. What decides is the fact that the cancel came out of the scheduled state.
func TestAPlanCancelledAfterItsStartTimeNeverRanAndOffersNoRetry(t *testing.T) {
	e, o, jobs, _ := setupOTAAt(t, fixedClock{fri.Add(24 * time.Hour)})
	at := sat.Add(10 * time.Minute)
	jobs.jobs = nil
	jobs.events = map[model.JobID][]model.JobEvent{}
	for _, id := range []model.JobID{60, 61} {
		j := schedJob(id, 7, model.DeviceID(id-59), model.JobCancelled, sat)
		j.CreatedBy, j.UpdatedAt = "olga", at
		jobs.jobs = append(jobs.jobs, j)
		jobs.events[id] = []model.JobEvent{eventOf(id, at, model.JobScheduled, model.JobCancelled, "cancel", "cancelled by olga")}
	}
	_, body := e.do(t, "GET", "/transactions/7", "operator", nil)
	text := plain(body)
	if class, word := txnState(t, body); word != "Cancelled before start" || !strings.Contains(class, "t-neutral") {
		t.Errorf("badge %q (%s), want Cancelled before start in the neutral tone", word, class)
	}
	if !strings.Contains(text, "Plan again…") || strings.Contains(body, "Retry failed") || strings.Contains(body, "/transactions/7/retry") {
		t.Errorf("a plan cancelled after its start time offers a retry or no Plan again: %s", text)
	}
	if strings.Contains(body, `/jobs/60/retry`) || strings.Contains(body, `/jobs/61/retry`) {
		t.Errorf("a device that never ran offers its own Retry: %s", text)
	}
	_, list := e.do(t, "GET", "/transactions", "operator", nil)
	if r := txHistoryRow(t, list, "TX-0007"); !strings.Contains(plain(r), "was due") || strings.Contains(plain(r), "· scheduled") {
		t.Errorf("History reads the cancelled plan as one that ran: %s", plain(r))
	}
	resp, rbody := e.do(t, "POST", "/transactions/7/retry", "admin", url.Values{})
	if resp.StatusCode != http.StatusConflict || len(o.retryCalls) != 0 || !strings.Contains(plain(rbody), "Plan again") {
		t.Errorf("retry of a plan cancelled late: %d, calls %v: %s", resp.StatusCode, o.retryCalls, plain(rbody))
	}
}

// Why (#792): a job cancelled while it ran is an ordinary cancellation and keeps its Retry; only the cancel
// that came out of the scheduled state marks a job that never ran (a cancel after the start time must not
// be mistaken for one by its time alone).
func TestAJobCancelledWhileRunningStillOffersRetry(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri.Add(24 * time.Hour)})
	at := sat.Add(10 * time.Minute)
	j := schedJob(62, 7, 1, model.JobCancelled, sat)
	j.UpdatedAt = at
	jobs.jobs = []model.Job{j}
	jobs.events = map[model.JobID][]model.JobEvent{62: {
		eventOf(62, sat.Add(time.Second), model.JobScheduled, model.JobQueued, "start_due", "x"),
		eventOf(62, at, model.JobApplying, model.JobCancelled, "cancel", "cancelled by olga"),
	}}
	_, body := e.do(t, "GET", "/transactions/7", "operator", nil)
	if !strings.Contains(body, "/transactions/7/retry") {
		t.Errorf("a job that ran and was cancelled lost its Retry: %s", plain(body))
	}
}

// Why (#792): after one device of a plan was cancelled, the dialog of Cancel (and the page without JavaScript)
// names the devices that will not start. The first count was the plan's original size: "3 devices" for a plan of
// which two are left made the person think the cancelled one was still part of it.
func TestCancelOfAHalfCancelledPlanNamesTheDevicesStillPlanned(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	jobs.jobs = []model.Job{schedJob(70, 10, 1, model.JobScheduled, sat), schedJob(71, 10, 2, model.JobScheduled, sat), schedJob(72, 10, 3, model.JobCancelled, sat)}
	jobs.jobs[2].UpdatedAt = fri.Add(time.Hour) // cancelled by hand before the start
	_, body := e.do(t, "GET", "/transactions/10", "operator", nil)
	dlg := tagOf(t, body, `data-dlg-title="Cancel TX-0010?"`)
	if !strings.Contains(dlg, "(2 devices)") || strings.Contains(dlg, "3 devices") {
		t.Errorf("the dialog of Cancel names the wrong count: %s", dlg)
	}
	_, page := e.do(t, "GET", "/transactions/10/cancel", "operator", nil)
	if text := plain(page); !strings.Contains(text, "(2 devices)") || strings.Contains(text, "3 devices") {
		t.Errorf("the page of Cancel without JavaScript names the wrong count: %s", text)
	}
}
