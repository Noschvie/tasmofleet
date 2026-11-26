package web

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/config"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/notify"
	"github.com/hellwand/tasmofleet/internal/schedule"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// Scheduled updates in Transactions (#604). A job planned for later is in the state model.JobScheduled until
// its start. It holds its device against other TasmoFleet jobs, but it is neither queued (it waits for no
// slot) nor running (nothing happens on the device), and it is not in activeJobStates, which the status bar, the
// About page and the console read. Transactions lists plans; the Dashboard and the status bar say how many wait
// and when the next one starts (#707, planSummary), and count nothing as running for it.

// neverRan says from the job alone that it ended without ever starting: its start passed while TasmoFleet was
// down (NotStarted), or a person cancelled the plan before its start (the engine never starts one earlier, so
// a cancelled job with UpdatedAt before StartAt never ran). It cannot see a plan cancelled after its start
// time but before the engine queued it (the device was refused and the engine was retrying inside the
// late-start window): that cancel is only known by its event. Where the events are at hand (jobViews) the
// decision is jobView.NeverRan; neverRan is the answer for the callers that have only jobs (the dashboard's
// list of transactions).
func neverRan(j model.Job) bool {
	if j.NotStarted {
		return true
	}
	return j.State == model.JobCancelled && !j.StartAt.IsZero() && j.UpdatedAt.Before(j.StartAt)
}

// holdsDevice says that a job is at work on its device now. A plan holds its device against other TasmoFleet
// jobs, but until its start the device stays usable (console, power, settings), so for the pages that switch
// things off while "the device is being updated" it does not count.
func holdsDevice(j model.Job) bool { return !j.State.Terminal() && j.State != model.JobScheduled }

// planWaits is the one rule for "this transaction is still only a plan": it has jobs that wait for their start
// and none that began (started: its running and queued jobs). A transaction of which some devices began is a
// running transaction, not a plan; Transactions, the Dashboard and the status bar all ask here.
func planWaits(planned, started int) bool { return planned > 0 && started == 0 }

// openJobStates are the states of the jobs that are or will be at work: the active ones and the plans.
var openJobStates = append(slices.Clone(activeJobStates), model.JobScheduled)

// planSummary says how many plans wait and when the first starts, from the open jobs (openJobStates; the
// others are not needed: a transaction is a plan by its planned and its running or queued jobs alone). The
// count is per transaction, like "Scheduled · N" in Transactions, not per device. N is 0 without a plan.
func planSummary(now time.Time, open []model.Job) ui.PlanSummary {
	var p ui.PlanSummary
	for _, t := range groupTransactions(open) {
		if !planWaits(t.Planned, t.Running+t.Queued) {
			continue
		}
		p.N++
		if p.Next.IsZero() || t.StartAt.Before(p.Next) {
			p.Next = t.StartAt
		}
	}
	if p.N > 0 {
		p.In = planIn(now, p.Next)
	}
	return p
}

// scheduledHref is the Scheduled section of Transactions, where the Dashboard's and the bar's plan lines lead.
func scheduledHref(base string) string { return base + "/transactions#scheduled" }

// schedCounts are the counts of a transaction that decide how a plan reads.
type schedCounts struct{ Total, Planned, Running, Queued, NotStarted, Early int }

// schedResult words the result of a transaction that has plans in it: res and tone are what the counts of a
// running update say. Waiting for its start the transaction is Scheduled; when nothing of it ever ran
// because TasmoFleet was down it is Not started (a warning: nobody cancelled it).
func schedResult(res string, tone ui.Tone, c schedCounts) (string, ui.Tone) {
	switch {
	case planWaits(c.Planned, c.Running+c.Queued):
		return "Scheduled", ui.ToneAccent
	case c.NotStarted > 0 && c.NotStarted+c.Early == c.Total:
		return "Not started", ui.ToneWarn
	}
	return res, tone
}

// plannedAt is the start of a plan as a sentence: "Sat, Nov 7 at 03:00", in the server's zone like every
// text; the <time> elements of the pages say it in the browser's.
func plannedAt(t time.Time) string { return ui.StampText(t, ui.StampStart) }

// txnSched is what the page of a planned update adds to a transaction page.
type txnSched struct {
	Kind        string // schedWaiting, schedRan, schedNotStarted or schedCancelled
	Who         string // who planned it
	Created     time.Time
	Start       time.Time
	In          string // schedWaiting: "in 17 h"
	Started     time.Time
	Late        time.Duration // schedRan: how long after the start it started
	BackAt      time.Time     // schedNotStarted: when TasmoFleet came back
	CancelledBy string
	CancelledAt time.Time
	Tail        string // "2 at a time · backup first"
	Minutes     int    // the late-start window
	CancelText  string // the dialog of Cancel
	Notice      string // schedNotStarted: the text of the notification
	NoticeTitle string
	Again       *checkAgain // "Plan again…" (never for a plan that is waiting or ran)
}

const (
	schedWaiting    = "waiting"
	schedRan        = "ran"
	schedNotStarted = "notstarted"
	schedCancelled  = "cancelled"
)

// OnTime: a start within a minute of its time is on time.
func (s *txnSched) OnTime() bool { return s.Late < time.Minute }

// Instant: the start came within the second of its time, which "0 seconds after" would say badly.
func (s *txnSched) Instant() bool { return s.Late < time.Second }

// LateText is how late a start was: "2 seconds", "12 minutes".
func (s *txnSched) LateText() string {
	if s.Late < time.Minute {
		return plural(int(s.Late.Round(time.Second)/time.Second), "second")
	}
	return plural(int(s.Late/time.Minute), "minute")
}

// BackFmt is how the time TasmoFleet came back reads: a clock time on the day of the start, else with its day.
func (s *txnSched) BackFmt() ui.StampFmt {
	if s.BackAt.Local().YearDay() == s.Start.Local().YearDay() && s.BackAt.Local().Year() == s.Start.Local().Year() {
		return ui.StampTime
	}
	return ui.StampStart
}

// Deadline is the end of the window in which a late start is still made.
func (s *txnSched) Deadline() time.Time { return s.Start.Add(schedule.LateStart) }

// txnSchedOf reads the story of a planned update from its jobs, or nil when there is none to tell: the update
// started at once.
func (h *handler) txnSchedOf(r *http.Request, b batchPage, first model.Job, who, tail string, now time.Time) *txnSched {
	s := &txnSched{Who: who, Created: first.CreatedAt, Start: first.StartAt, Tail: tail, Minutes: int(schedule.LateStart / time.Minute)}
	var cancelled int
	for _, j := range b.Jobs {
		if !j.StartedAt.IsZero() && (s.Started.IsZero() || j.StartedAt.Before(s.Started)) {
			s.Started = j.StartedAt
		}
		if j.NotStartedAt.After(s.BackAt) {
			s.BackAt = j.NotStartedAt
		}
		if !j.CancelledAt.IsZero() && (s.CancelledAt.IsZero() || j.CancelledAt.Before(s.CancelledAt)) {
			s.CancelledAt, s.CancelledBy = j.CancelledAt, h.pr.Text(j.CancelledBy)
		}
		if j.NeverRan && !j.Job.NotStarted {
			cancelled++
		}
	}
	switch {
	case b.Planned > 0 && b.Running+b.Queued+b.Done == 0:
		s.Kind, s.In = schedWaiting, planIn(now, s.Start)
		// The devices that will not start: one cancelled by hand is no longer part of the plan.
		s.CancelText = planCancelText(txTitle(first, b.Planned)+" ("+plural(b.Planned, "device")+")", s.Start)
	case b.NotStarted > 0 && b.NotStarted+b.Early == b.Total:
		s.Kind = schedNotStarted
		if s.BackAt.IsZero() {
			s.BackAt = first.UpdatedAt
		}
		n := notify.UpdateNotStarted(config.Config{}, int64(b.Batch), b.NotStarted, first.TargetRelease, s.Start, s.BackAt)
		s.NoticeTitle, s.Notice = n.Title, n.Text
	case b.Early > 0 && b.Early == b.Total:
		s.Kind = schedCancelled
	default:
		s.Kind = schedRan
		if !s.Started.IsZero() {
			s.Late = max(s.Started.Sub(s.Start), 0)
		}
	}
	if s.Kind == schedNotStarted || s.Kind == schedCancelled {
		s.Again = planAgain(b.Jobs)
	}
	return s
}

// planAgain is the form of "Plan again…": the update preview with the same devices and the same release and
// options, and the time choice of the plan: "Start at a chosen time", with the planned time when it still lies
// ahead. It never opens on "Start now": the main button of the preview would then start the update at once,
// which nobody who planned it for later meant (#825). The time is only a preselection; the update is planned
// when the person presses the button.
func planAgain(jobs []jobView) *checkAgain {
	a := &checkAgain{Action: "/updates/preview"}
	add := func(k, v string) { a.Fields = append(a.Fields, ui.KVRow{Key: k, Value: v}) }
	first := jobs[0].Job
	add("when", "at")
	if !first.StartAt.IsZero() {
		add("start_at", first.StartAt.Format(time.RFC3339))
	}
	for _, j := range jobs {
		add("id", strconv.FormatInt(int64(j.Job.DeviceID), 10))
	}
	if first.TargetCustom != "" {
		add("release", customReleasePrefix+first.TargetCustom)
	} else {
		add("release", first.TargetRelease)
	}
	add("policy", string(first.Policy))
	add("delivery", string(first.Delivery))
	if first.SkipBackup {
		add("skip_backup", "1")
	}
	for _, j := range jobs {
		if j.Job.TargetBuild != "" {
			add("build_"+strconv.FormatInt(int64(j.Job.DeviceID), 10), j.Job.TargetBuild)
		}
	}
	return a
}

// startedText words the start_due event of a job in its log.
func startedText(e model.JobEvent, j model.Job) string {
	late := e.At.Sub(j.StartAt)
	at := j.StartAt.Local().Format("15:04")
	if late < time.Minute {
		return "Started as planned (planned for " + at + ")"
	}
	return fmt.Sprintf("Started late (planned for %s, %s late)", at, plural(int(late/time.Minute), "minute"))
}

// scheduledRefusalText words the refusal of a scheduled start. The engine ends a job at its start with the
// event guard_refused when the device was busy for the whole late-start window, or was excluded from updates
// after the plan was made. Drawn as "Guard refused" that reads like a stop of the firmware check, which it is
// not; the texts below are the engine's (ota/scheduled.go), matched by their beginnings.
func scheduledRefusalText(detail string) (string, bool) {
	if rest, ok := strings.CutPrefix(detail, "the device was not free"); ok {
		return "Device was busy" + rest, true
	}
	if strings.HasPrefix(detail, "excluded from OTA since the update was planned") {
		return "Excluded from updates since the plan: the update was not started", true
	}
	return "", false
}

// dueText is what a plan reads once its start has come and it has not started: the engine retries a start the
// device refused every 30 seconds until the late-start window is over (schedule.LateStart).
const dueText = "due, waiting for the device"

// planIn is how far the start of a plan that is still scheduled lies ahead, or that it is due. startsIn
// stays for the preview, where the start is always ahead.
func planIn(now, start time.Time) string {
	if !start.After(now) {
		return dueText
	}
	return startsIn(now, start)
}

// txSchedRow is one plan in the Scheduled section of Transactions.
type txSchedRow struct {
	ID, Title, Devices, By, In, Href, CancelHref, CancelText string
	Start                                                    time.Time
}

// scheduledRows are the plans that still wait for their start, soonest first. running are the transactions
// of the main list: a transaction of which some devices have started is running, not a plan. The result holds
// the batches shown here so the History leaves them out.
func (h *handler) scheduledRows(r *http.Request, device model.DeviceID, base string, now time.Time, items []txView) ([]txSchedRow, map[model.BatchID]bool, error) {
	jobs, err := h.d.Jobs.ListJobs(r.Context(), store.JobFilter{DeviceID: device, States: []model.JobState{model.JobScheduled}})
	if err != nil {
		return nil, nil, err
	}
	started := map[model.BatchID]int{}
	for _, t := range items {
		started[t.ID] = t.Running + t.Queued
	}
	var rows []txSchedRow
	shown := map[model.BatchID]bool{}
	for _, t := range groupTransactions(jobs) {
		if !planWaits(t.Planned, started[t.ID]) {
			continue
		}
		shown[t.ID] = true
		href := base + "/transactions/" + bid(t.ID)
		title := t.Title
		rows = append(rows, txSchedRow{ID: txID(t.ID), Title: title, Devices: plural(t.Total, "device"), By: h.pr.Text(t.By),
			In: planIn(now, t.StartAt), Href: href, CancelHref: href + "/cancel", Start: t.StartAt,
			CancelText: planCancelText(title, t.StartAt)})
	}
	sort.SliceStable(rows, func(a, b int) bool {
		if !rows[a].Start.Equal(rows[b].Start) {
			return rows[a].Start.Before(rows[b].Start)
		}
		return rows[a].ID < rows[b].ID
	})
	return rows, shown, nil
}

// planCancelText is what cancelling a plan does, in the dialog and on its page without JavaScript. what names the
// update ("Update to Tasmota 16.1.0 (7 devices)"); the start is replaced by the browser's time (dialog.js).
func planCancelText(what string, start time.Time) string {
	return what + " is planned for " + plannedAt(start) + ". It will not start and nothing on the devices changes. You can plan it again later."
}

// planStartText names a plan and its start, for the pages that refuse something because of it:
// "TX-0010 starts Sat, Nov 7 at 03:00".
func planStartText(j model.Job) string { return txID(j.BatchID) + " starts " + plannedAt(j.StartAt) }

// plannedOn is the plan that holds a device, if any (own query: the shared activeJobStates stay without
// scheduled jobs).
func (h *handler) plannedOn(ctx context.Context, id model.DeviceID) (model.Job, bool, error) {
	if h.d.Jobs == nil {
		return model.Job{}, false, nil
	}
	jobs, err := h.d.Jobs.ListJobs(ctx, store.JobFilter{DeviceID: id, States: []model.JobState{model.JobScheduled}, Limit: 1})
	if err != nil || len(jobs) == 0 {
		return model.Job{}, false, err
	}
	return jobs[0], true, nil
}
