package web

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/ota"
)

// Display faults of a scheduled update seen on a real device (#825).

// planAgainForm is what "Plan again…" posts for a plan with the given start.
func planAgainForm(start time.Time) url.Values {
	return startForm(url.Values{"when": {"at"}, "start_at": {start.Format(time.RFC3339)}})
}

// Why: "Plan again…" opened the preview on "Start now", so the main button flashed the devices at once. The
// person had planned the update for later; whatever else the preview shows, it must open on that choice, with
// the time of the plan when that is still ahead, and never on "Start now".
func TestPlanAgainOpensOnTheTimeChoiceOfThePlan(t *testing.T) {
	e, _, _, _ := setupOTAAt(t, fixedClock{friday})
	planned := time.Date(2026, 11, 8, 3, 30, 0, 0, zoneCET) // a night that is still ahead, not one of the quick picks
	resp, body := e.do(t, "POST", "/updates/preview", "operator", planAgainForm(planned))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if now := tagOf(t, body, `id="when-now"`); strings.Contains(now, " checked") {
		t.Errorf("Start now is preselected for a plan that was made for later: %q", now)
	}
	if at := tagOf(t, body, `id="when-at"`); !strings.Contains(at, " checked") {
		t.Errorf("Start at a chosen time is not preselected: %q", at)
	}
	if got := goButton(t, body); got != "Schedule update" {
		t.Errorf("the main button reads %q: it must plan, not start", got)
	}
	if other := tagOf(t, body, `value="custom"`); !strings.Contains(other, " checked") {
		t.Errorf("Other time is not checked: %q", other)
	}
	for _, want := range []string{`name="at_date"`, `value="2026-11-08"`, `name="at_time"`, `value="03:30"`, `data-preset="2026-11-08T03:30:00+01:00"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the preview lacks %q: the time of the plan is not preselected", want)
		}
	}
	if !strings.Contains(plain(body), "Starts Sun, Nov 8 at 03:30") {
		t.Errorf("the plan does not name the preselected start: %s", plain(body))
	}
}

// Why: a plan that was not started, or cancelled after its time, has a time that is behind us. Planning it
// again must not turn that into "Start now" (the update would start at once) and must not scold the person
// before they chose anything: the time choice stays, empty, and the button still plans.
func TestPlanAgainOfAPastPlanKeepsTheChoiceWithoutATime(t *testing.T) {
	e, _, _, _ := setupOTAAt(t, fixedClock{friday})
	past := friday.Add(-24 * time.Hour)
	_, body := e.do(t, "POST", "/updates/preview", "operator", planAgainForm(past))
	if now := tagOf(t, body, `id="when-now"`); strings.Contains(now, " checked") {
		t.Errorf("Start now is preselected for a plan whose time has passed: %q", now)
	}
	if at := tagOf(t, body, `id="when-at"`); !strings.Contains(at, " checked") {
		t.Errorf("Start at a chosen time is not preselected: %q", at)
	}
	if got := goButton(t, body); got != "Schedule update" {
		t.Errorf("the main button reads %q", got)
	}
	if strings.Contains(body, "data-preset") {
		t.Errorf("a time in the past was preselected")
	}
	if strings.Contains(tagOf(t, body, `id="when-err"`), "hidden") == false {
		t.Errorf("the preview complains about a time nobody chose: %s", plain(body))
	}
	if !strings.Contains(plain(body), "Not set yet: choose a quick pick or a time") {
		t.Errorf("the plan does not say that no time is set yet: %s", plain(body))
	}
}

// Why (fail safe): nothing may make a plan start earlier than chosen. The date and time fields hold whole
// minutes, so a planned time with seconds is rounded up, never down.
func TestPlanAgainNeverRoundsTheTimeEarlier(t *testing.T) {
	w := planAgainStart(url.Values{"start_at": {"2026-11-08T03:30:20+01:00"}}, friday)
	if w.Time != "03:31" || w.Start.Before(time.Date(2026, 11, 8, 3, 30, 20, 0, zoneCET)) {
		t.Errorf("time %q start %v: a rounding moved the start earlier", w.Time, w.Start)
	}
}

// Why: a fresh preview (not "Plan again…") still opens on "Start now": a plain update is not a plan.
func TestPreviewWithoutAPlanStillStartsNow(t *testing.T) {
	e, _, _, _ := setupOTAAt(t, fixedClock{friday})
	_, body := e.do(t, "POST", "/updates/preview", "operator", startForm(url.Values{"start_at": {friday.Add(time.Hour).Format(time.RFC3339)}}))
	if now := tagOf(t, body, `id="when-now"`); !strings.Contains(now, " checked") {
		t.Errorf("a preview that is not Plan again must open on Start now: %q", now)
	}
}

// Why: History listed a late-started update under the time it was planned for ("Today 08:49 · scheduled")
// although it began at 08:53; whoever reads History wants to know when it really ran.
func TestHistoryDatesALateStartedUpdateByItsRealStart(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	planned := time.Date(2026, 11, 6, 8, 49, 0, 0, time.UTC)
	began := time.Date(2026, 11, 6, 8, 53, 0, 0, time.UTC)
	j := schedJob(30, 6, 1, model.JobSucceeded, planned)
	j.UpdatedAt = began.Add(time.Minute)
	jobs.jobs = []model.Job{j}
	jobs.events = map[model.JobID][]model.JobEvent{
		30: {eventOf(30, began, model.JobScheduled, model.JobQueued, "start_due", "planned for Fri, Nov 6 at 08:49, started 4m0s after it")},
	}
	_, body := e.do(t, "GET", "/transactions", "operator", nil)
	r := txHistoryRow(t, body, "TX-0006")
	if !strings.Contains(r, `datetime="2026-11-06T08:53:00Z"`) || strings.Contains(r, `08:49`) {
		t.Errorf("the row of a late start shows the planned time: %s", r)
	}
	if !strings.Contains(plain(r), "Today 08:53") || !strings.Contains(plain(r), "· scheduled") {
		t.Errorf("the row reads %q, want Today 08:53 · scheduled", plain(r))
	}
}

// Why: a plan that never started (the update never began) keeps its planned time: there is no real start.
func TestHistoryKeepsThePlannedTimeOfAnUpdateThatNeverBegan(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	due := time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)
	j := schedJob(31, 8, 1, model.JobCancelled, due)
	j.NotStarted, j.UpdatedAt = true, due.Add(72*time.Minute)
	jobs.jobs = []model.Job{j}
	_, body := e.do(t, "GET", "/transactions", "operator", nil)
	r := txHistoryRow(t, body, "TX-0008")
	if !strings.Contains(r, `datetime="2026-11-05T03:00:00Z"`) {
		t.Errorf("the row of a plan that never began lost its planned time: %s", r)
	}
}

// Why: a batch where one device began late and another never began is labelled "was due <planned time>". The
// real start of the first would sit under that label as the time it was due, which it was not.
func TestHistoryOfAMixedBatchKeepsThePlannedTimeUnderWasDue(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	planned := time.Date(2026, 11, 6, 8, 49, 0, 0, time.UTC)
	began := time.Date(2026, 11, 6, 8, 53, 0, 0, time.UTC)
	ran := schedJob(30, 6, 1, model.JobSucceeded, planned)
	ran.UpdatedAt = began.Add(time.Minute)
	missed := schedJob(31, 6, 2, model.JobCancelled, planned)
	missed.NotStarted, missed.UpdatedAt = true, planned.Add(72*time.Minute)
	jobs.jobs = []model.Job{ran, missed}
	jobs.events = map[model.JobID][]model.JobEvent{
		30: {eventOf(30, began, model.JobScheduled, model.JobQueued, "start_due", "planned for Fri, Nov 6 at 08:49, started 4m0s after it")},
	}
	_, body := e.do(t, "GET", "/transactions", "operator", nil)
	r := txHistoryRow(t, body, "TX-0006")
	if !strings.Contains(r, `datetime="2026-11-06T08:49:00Z"`) || strings.Contains(r, `08:53`) {
		t.Errorf("a \"was due\" row shows a time that is not the planned one: %s", r)
	}
	if !strings.Contains(plain(r), "was due") {
		t.Errorf("the row lost its \"was due\" label: %s", plain(r))
	}
}

// Why (#825): the engine writes what the device ran on the identity read that sends a job back to waiting.
// With it the log names the loader image of Tasmota's own two-step update, so the first restart (into that
// image) and the second (into the new firmware) no longer read the same.
func TestJobLogOfATwoStepUpdateNamesTheLoaderLeg(t *testing.T) {
	j := model.Job{TargetRelease: "16.0.0", Plan: model.PlanDirect, State: model.JobSucceeded}
	const q, pf, bk, so, ui, ar, vf, ro, ok = model.JobQueued, model.JobPreflight, model.JobBackup, model.JobSetOtaURL, model.JobUpgradeIssued, model.JobAwaitReboot, model.JobVerify, model.JobRestoreOtaURL, model.JobSucceeded
	seq := []logEv{
		{q, pf, "start", "", "Start: commands over HTTP, the image over HTTP from TasmoFleet's firmware server"},
		{pf, bk, "planned", "", "Planned"},
		{bk, so, "backup_done", "", "Backup done"},
		{so, ui, "command_ok", "", "Command ok"},
		{ui, ar, "probe_offline", "", "Rebooting into the safeboot image..."},
		{ar, vf, "probe_online", "", "Waiting for the safeboot image to answer..."},
		{vf, ar, "identity", ota.RunningNote(model.Identity{Variant: "safeboot"}, true), "Not on the new version yet: the device runs the safeboot image, which continues the update by itself"},
		{ar, ar, "probe_offline", "", "Rebooting into 16.0.0..."},
		{ar, vf, "probe_online", "", "Waiting for 16.0.0 to answer..."},
		{vf, ro, "identity", "", "New version verified, restoring the device's OtaUrl"},
		{ro, ok, "command_ok", "", "Updated to 16.0.0"},
	}
	evs := make([]model.JobEvent, len(seq))
	for i, s := range seq {
		evs[i] = model.JobEvent{From: s.from, To: s.to, Event: s.event, Detail: s.detail}
	}
	for i, s := range seq {
		got := eventText(evs[i], j, newPrincipalDir(nil), eventStageMinimal(evs, i, j))
		if t2, ok := loaderText(evs[i]); ok && eventLoader(evs, i) == "safeboot" {
			got = t2
		}
		if got != s.want {
			t.Errorf("#%d %s: %q, want %q", i, s.event, got, s.want)
		}
	}
}

// Why: an identity read that found the device on the firmware it had says so (with its version), and an update
// that restarted once through the minimal image words its first leg as the minimal firmware.
func TestJobLogSaysWhatTheDeviceRanWhenItWasNotOnTheNewVersion(t *testing.T) {
	j := model.Job{TargetRelease: "16.0.0", Plan: model.PlanDirect}
	still := model.JobEvent{From: model.JobVerify, To: model.JobAwaitReboot, Event: "identity", Detail: ota.RunningNote(model.Identity{Version: "15.1.0"}, false)}
	if got := eventText(still, j, newPrincipalDir(nil), false); got != "Not on the new version yet: the device still runs 15.1.0" {
		t.Errorf("got %q", got)
	}
	evs := []model.JobEvent{
		{From: model.JobUpgradeIssued, To: model.JobAwaitReboot, Event: "probe_offline"},
		{From: model.JobAwaitReboot, To: model.JobVerify, Event: "probe_online"},
		{From: model.JobVerify, To: model.JobAwaitReboot, Event: "identity", Detail: ota.RunningNote(model.Identity{Variant: "minimal"}, true)},
		{From: model.JobAwaitReboot, To: model.JobAwaitReboot, Event: "probe_offline"},
	}
	if eventLoader(evs, 0) != "minimal" || eventLoader(evs, 1) != "minimal" || eventLoader(evs, 3) != "" {
		t.Errorf("legs: %q %q %q", eventLoader(evs, 0), eventLoader(evs, 1), eventLoader(evs, 3))
	}
	if got := eventText(evs[0], j, newPrincipalDir(nil), eventLoader(evs, 0) == "minimal"); got != "Rebooting into the minimal firmware..." {
		t.Errorf("got %q", got)
	}
}

// Why: Tasmota's loader image and a device that never restarted are different facts; the engine must write
// the one that happened on the event, from what the device reported, nothing else.
func TestRunningNoteNamesTheLoaderOrTheOldVersion(t *testing.T) {
	for _, c := range []struct {
		id     model.Identity
		loader bool
		want   string
	}{
		{model.Identity{Variant: "safeboot", Version: "16.0.0"}, true, "device runs the safeboot image"},
		{model.Identity{Variant: "minimal"}, true, "device runs the minimal image"},
		{model.Identity{Variant: "tasmota", Version: "15.1.0"}, false, "device still runs 15.1.0 tasmota"},
		{model.Identity{Version: "15.1.0"}, false, "device still runs 15.1.0"},
		// the minimal image the machine did not wait on is the old image of its stage, not a loader leg
		{model.Identity{Variant: "minimal", Version: "16.1.0"}, false, "device still runs 16.1.0 minimal"},
		{model.Identity{Variant: "tasmota32", Version: "16.0.0", Language: "DE", LanguageKnown: true}, false, "device still runs 16.0.0 tasmota32 DE"},
		{model.Identity{Variant: "tasmota32", Version: "16.0.0", LanguageKnown: true}, false, "device still runs 16.0.0 tasmota32 EN"},
	} {
		if got := ota.RunningNote(c.id, c.loader); got != c.want {
			t.Errorf("%+v loader=%v: %q, want %q", c.id, c.loader, got, c.want)
		}
	}
}

// Why: a person who leaves the page while it loads cancels the request; the handler's work stops with
// "context canceled". That is nobody's failure and must not fill the log with errors (#825), while a
// cancellation the client did not cause, and every other error, still is one.
func TestFailOfACancelledRequestIsNotAnError(t *testing.T) {
	var buf bytes.Buffer
	h := &handler{log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/devices", nil).WithContext(gone)
	h.fail(httptest.NewRecorder(), req, context.Canceled)
	h.fail(httptest.NewRecorder(), req, errors.Join(errors.New("settings"), context.Canceled))
	if strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("a request the client cancelled was logged as an error:\n%s", buf.String())
	}

	buf.Reset()
	live := httptest.NewRequest("GET", "/devices", nil) // its client is still there
	w := httptest.NewRecorder()
	h.fail(w, live, context.Canceled)
	if !strings.Contains(buf.String(), "level=ERROR") || w.Code != http.StatusInternalServerError {
		t.Errorf("a cancellation the client did not cause was hidden (status %d):\n%s", w.Code, buf.String())
	}
	buf.Reset()
	h.fail(httptest.NewRecorder(), req, errors.New("database is busy"))
	if !strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("an ordinary failure on a cancelled request was hidden:\n%s", buf.String())
	}
}

// Why (#824): the new audit entries read as a person reads them: the scheduler's start says what was planned and
// how late it began, the cancel names how many devices and whether a plan was ended.
func TestAuditSummaryOfTheSchedulerStartAndTheCancel(t *testing.T) {
	ev := func(action, detail string) model.AuditEvent {
		return model.AuditEvent{Actor: "system", Action: action, Target: "transaction:12", Result: "ok", Detail: detail}
	}
	for _, c := range []struct {
		e            model.AuditEvent
		label, short string
	}{
		{ev("transaction.scheduled_start", `{"device":"plug-1","planned_start":"2026-11-14T08:49:00Z","late_seconds":240}`), "Scheduled start", "plug-1 · planned Sat, Nov 14 at 08:49 · 4 min late"},
		{ev("transaction.cancel", `{"devices":2,"waiting":2,"running":0,"planned":2,"planned_start":"2026-11-14T08:49:00Z"}`), "Cancel transaction", "2 devices · planned Sat, Nov 14 at 08:49"},
		{ev("transaction.cancel", `{"devices":1,"waiting":0,"running":1}`), "Cancel transaction", "1 device · 1 running, stops after its step"},
		// One job of a transaction names its device, so the entry does not read as the whole transaction stopped.
		{ev("transaction.cancel", `{"devices":1,"waiting":1,"running":0,"job":7,"device_id":3,"device":"plug-1"}`), "Cancel transaction", "1 device of the transaction: plug-1"},
		{ev("transaction.cancel", `{"devices":1,"waiting":1,"running":0,"job":7,"device_id":3}`), "Cancel transaction", "1 device of the transaction: #3"},
	} {
		if got := auditLabel(c.e.Action); got != c.label {
			t.Errorf("%s label %q, want %q", c.e.Action, got, c.label)
		}
		if got := auditSummary(c.e); got != c.short {
			t.Errorf("%s summary %q, want %q", c.e.Action, got, c.short)
		}
	}
}

// Why (#825): the page of a two-step update must show the restart into the loader image once and the restart
// into the new firmware once, each in its own words, instead of "Rebooting into 16.0.0..." twice after one
// command.
func TestTransactionPageLogDoesNotRepeatTheRestartOfATwoStepUpdate(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	j := schedJob(70, 9, 1, model.JobSucceeded, time.Time{})
	j.Plan = model.PlanDirect
	jobs.jobs = []model.Job{j}
	at := fri.Add(-time.Hour)
	ev := func(from, to model.JobState, name, detail string) model.JobEvent {
		at = at.Add(10 * time.Second)
		return eventOf(70, at, from, to, name, detail)
	}
	const ui, ar, vf, ro, ok = model.JobUpgradeIssued, model.JobAwaitReboot, model.JobVerify, model.JobRestoreOtaURL, model.JobSucceeded
	jobs.events = map[model.JobID][]model.JobEvent{70: {
		ev(ui, ar, "probe_offline", ""),
		ev(ar, vf, "probe_online", ""),
		ev(vf, ar, "identity", ota.RunningNote(model.Identity{Variant: "safeboot"}, true)),
		ev(ar, ar, "probe_offline", ""),
		ev(ar, vf, "probe_online", ""),
		ev(vf, ro, "identity", ""),
		ev(ro, ok, "command_ok", ""),
	}}
	_, body := e.do(t, "GET", "/transactions/9", "operator", nil)
	text := plain(body)
	if n := strings.Count(text, "Rebooting into 16.1.0..."); n != 1 {
		t.Errorf("\"Rebooting into 16.1.0...\" appears %d times, want once: %s", n, text)
	}
	for _, want := range []string{"Rebooting into the safeboot image...", "Waiting for the safeboot image to answer...", "Not on the new version yet: the device runs the safeboot image, which continues the update by itself"} {
		if !strings.Contains(text, want) {
			t.Errorf("the log lacks %q: %s", want, text)
		}
	}
}

// Why (#825): a direct update of an ESP8266 goes through Tasmota's own minimal firmware. The log words that
// stop as "Rebooting into the minimal firmware..." and the restart into the new version once; without the
// wiring of the minimal loader the page named "Rebooting into 16.1.0..." twice after one fetch and one reboot.
func TestTransactionPageLogNamesTheMinimalFirmwareOfADirectUpdateOnce(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	j := schedJob(70, 9, 1, model.JobSucceeded, time.Time{})
	j.Plan = model.PlanDirect
	jobs.jobs = []model.Job{j}
	at := fri.Add(-time.Hour)
	ev := func(from, to model.JobState, name, detail string) model.JobEvent {
		at = at.Add(10 * time.Second)
		return eventOf(70, at, from, to, name, detail)
	}
	const ui, ar, vf, ro, ok = model.JobUpgradeIssued, model.JobAwaitReboot, model.JobVerify, model.JobRestoreOtaURL, model.JobSucceeded
	jobs.events = map[model.JobID][]model.JobEvent{70: {
		ev(ui, ar, "probe_offline", ""),
		ev(ar, vf, "probe_online", ""),
		ev(vf, ar, "identity", ota.RunningNote(model.Identity{Variant: "minimal"}, true)),
		ev(ar, ar, "probe_offline", ""),
		ev(ar, vf, "probe_online", ""),
		ev(vf, ro, "identity", ""),
		ev(ro, ok, "command_ok", ""),
	}}
	_, body := e.do(t, "GET", "/transactions/9", "operator", nil)
	text := plain(body)
	if n := strings.Count(text, "Rebooting into the minimal firmware..."); n != 1 {
		t.Errorf("\"Rebooting into the minimal firmware...\" appears %d times, want once: %s", n, text)
	}
	if n := strings.Count(text, "Rebooting into 16.1.0..."); n != 1 {
		t.Errorf("\"Rebooting into 16.1.0...\" appears %d times, want once: %s", n, text)
	}
}

// Tasmota keeps its web server up while it downloads the image and answers on the old one for a few seconds
// after the download, before the one restart (support_tasmota.ino). With a probe every 5 s the engine sees:
// quiet (the download), an answer on the old version, quiet again (the restart), the new version. The log
// used to word both quiet legs "Rebooting into 16.1.0...", so one fetch and one reboot read as two reboots
// (#825). The restart is the only reboot the log may name.
func TestTransactionPageLogDoesNotCallTheDownloadARebootWhenTheOldImageAnswersFirst(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	j := schedJob(70, 9, 1, model.JobSucceeded, time.Time{})
	j.Plan = model.PlanDirect
	jobs.jobs = []model.Job{j}
	at := fri.Add(-time.Hour)
	ev := func(from, to model.JobState, name, detail string) model.JobEvent {
		at = at.Add(10 * time.Second)
		return eventOf(70, at, from, to, name, detail)
	}
	const ui, ar, vf, ro, ok = model.JobUpgradeIssued, model.JobAwaitReboot, model.JobVerify, model.JobRestoreOtaURL, model.JobSucceeded
	jobs.events = map[model.JobID][]model.JobEvent{70: {
		ev(ui, ar, "probe_offline", ""),
		ev(ar, vf, "probe_online", ""),
		ev(vf, ar, "identity", ota.RunningNote(model.Identity{Version: "15.2.0"}, false)),
		ev(ar, ar, "probe_offline", ""),
		ev(ar, vf, "probe_online", ""),
		ev(vf, ro, "identity", ""),
		ev(ro, ok, "command_ok", ""),
	}}
	_, body := e.do(t, "GET", "/transactions/9", "operator", nil)
	text := plain(body)
	if n := strings.Count(text, "Rebooting into 16.1.0..."); n != 1 {
		t.Errorf("\"Rebooting into 16.1.0...\" appears %d times, want once: %s", n, text)
	}
	for _, want := range []string{"Downloading firmware 16.1.0...", "Not on the new version yet: the device still runs 15.2.0"} {
		if !strings.Contains(text, want) {
			t.Errorf("the log lacks %q: %s", want, text)
		}
	}
}

// A staged update (ESP8266 with little room) reboots into the minimal firmware once, then downloads the final
// image. The minimal image can answer during that download; the engine sends that read back to waiting like any
// old image of the stage. The log must not word it as a second loader leg: one fetch and one reboot stay one
// fetch and one reboot (#825). "Rebooting into the minimal firmware..." belongs to stage 1 only.
func TestTransactionPageLogOfAStagedUpdateDoesNotRepeatTheMinimalRebootInTheFinalStage(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	j := schedJob(70, 9, 1, model.JobSucceeded, time.Time{})
	j.Plan = model.PlanStaged
	jobs.jobs = []model.Job{j}
	at := fri.Add(-time.Hour)
	ev := func(from, to model.JobState, name, detail string) model.JobEvent {
		at = at.Add(10 * time.Second)
		return eventOf(70, at, from, to, name, detail)
	}
	const so, ui, ar, vf, ro, ok = model.JobSetOtaURL, model.JobUpgradeIssued, model.JobAwaitReboot, model.JobVerify, model.JobRestoreOtaURL, model.JobSucceeded
	minimal := model.Identity{Version: "16.1.0", Variant: "minimal"}
	jobs.events = map[model.JobID][]model.JobEvent{70: {
		ev(ui, ar, "probe_offline", ""), // stage 1: the restart into the minimal firmware
		ev(ar, vf, "probe_online", ""),
		ev(vf, so, "identity", ""), // minimal reached, the final stage starts
		ev(so, ui, "command_ok", ""),
		ev(ui, ar, "probe_offline", ""), // the final image downloads
		ev(ar, vf, "probe_online", ""),  // the minimal image answers during the download
		ev(vf, ar, "identity", ota.RunningNote(minimal, false)),
		ev(ar, ar, "probe_offline", ""), // the one restart into 16.1.0
		ev(ar, vf, "probe_online", ""),
		ev(vf, ro, "identity", ""),
		ev(ro, ok, "command_ok", ""),
	}}
	_, body := e.do(t, "GET", "/transactions/9", "operator", nil)
	text := plain(body)
	if n := strings.Count(text, "Rebooting into the minimal firmware..."); n != 1 {
		t.Errorf("\"Rebooting into the minimal firmware...\" appears %d times, want once: %s", n, text)
	}
	if n := strings.Count(text, "Rebooting into 16.1.0..."); n != 1 {
		t.Errorf("\"Rebooting into 16.1.0...\" appears %d times, want once: %s", n, text)
	}
	for _, want := range []string{"Downloading the final firmware (16.1.0)...", "Not on the new version yet: the device still runs 16.1.0 minimal"} {
		if !strings.Contains(text, want) {
			t.Errorf("the log lacks %q: %s", want, text)
		}
	}
	if n := strings.Count(text, "Waiting for the minimal firmware to answer"); n != 1 {
		t.Errorf("\"Waiting for the minimal firmware to answer\" appears %d times, want once: %s", n, text)
	}
	if strings.Contains(text, "the device runs the minimal image") {
		t.Errorf("the final stage's old image was worded as a loader leg: %s", text)
	}
}

// Why (#825, the live test of 0.37.0): an update to the version the device already runs, in another build or
// language, is a real update. Its identity read "not on the new version yet" must name what differs, or the line
// "still runs 16.0.0" on an update to 16.0.0 contradicts the target and reads like a no-op.
func TestTransactionPageLogNamesTheBuildWhenTheTargetVersionIsTheOneTheDeviceRuns(t *testing.T) {
	e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
	j := schedJob(70, 9, 1, model.JobSucceeded, time.Time{})
	j.Plan = model.PlanDirect
	j.TargetRelease = "16.0.0"
	j.IdentityBefore = model.Identity{Version: "16.0.0", Variant: "tasmota32", Language: "DE", LanguageKnown: true}
	jobs.jobs = []model.Job{j}
	at := fri.Add(-time.Hour)
	ev := func(from, to model.JobState, name, detail string) model.JobEvent {
		at = at.Add(10 * time.Second)
		return eventOf(70, at, from, to, name, detail)
	}
	const ui, ar, vf, ro, ok = model.JobUpgradeIssued, model.JobAwaitReboot, model.JobVerify, model.JobRestoreOtaURL, model.JobSucceeded
	jobs.events = map[model.JobID][]model.JobEvent{70: {
		ev(ui, ar, "probe_offline", ""),
		ev(ar, vf, "probe_online", ""),
		ev(vf, ar, "identity", ota.RunningNote(j.IdentityBefore, false)),
		ev(ar, ar, "probe_offline", ""),
		ev(ar, vf, "probe_online", ""),
		ev(vf, ro, "identity", ""),
		ev(ro, ok, "command_ok", ""),
	}}
	_, body := e.do(t, "GET", "/transactions/9", "operator", nil)
	text := plain(body)
	want := "Not on the new version yet: the device still runs the firmware it had (16.0.0 tasmota32 DE)"
	if !strings.Contains(text, want) {
		t.Errorf("the log lacks %q: %s", want, text)
	}
	if strings.Contains(text, "still runs 16.0.0 ") && !strings.Contains(text, "firmware it had") {
		t.Errorf("the line names no build: %s", text)
	}
}

// Why: a device whose language cannot be read over MQTT needs a language choice and a Recheck on the very
// preview that "Plan again…" opened. The recheck form carries only the page's hidden fields; without the time
// choice among them the page came back on "Start now" with the main button armed to flash at once, which is
// the hazard #825 is about. The choice, and the time of the plan, must survive every recheck.
func TestPlanAgainTimeChoiceSurvivesARecheck(t *testing.T) {
	e, _, _, _ := setupOTAAt(t, fixedClock{friday})
	planned := time.Date(2026, 11, 8, 3, 30, 0, 0, zoneCET)
	_, body := e.do(t, "POST", "/updates/preview", "operator", planAgainForm(planned))
	body = replayRecheck(t, e, body, url.Values{"language_1": {"DE"}})
	if now := tagOf(t, body, `id="when-now"`); strings.Contains(now, " checked") {
		t.Errorf("a recheck of a planned update opened on Start now: %q", now)
	}
	if at := tagOf(t, body, `id="when-at"`); !strings.Contains(at, " checked") {
		t.Errorf("a recheck lost the time choice: %q", at)
	}
	if got := goButton(t, body); got != "Schedule update" {
		t.Errorf("the main button reads %q after a recheck: it must plan, not start", got)
	}
	if !strings.Contains(body, `data-preset="2026-11-08T03:30:00+01:00"`) {
		t.Errorf("a recheck moved the planned time: %s", plain(body))
	}
}
