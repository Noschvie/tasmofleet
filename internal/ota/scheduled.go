package ota

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/schedule"
)

// Scheduled starts (#603). A job submitted with Request.StartAt is stored as model.JobScheduled. It holds its
// device against other TasmoFleet jobs (ota_jobs_one_active, activeStates) but gets no worker and no slot. The
// engine loop (Run, schedule) looks at the scheduled jobs whenever it wakes and wakes through the engine
// clock at the earliest start. When a start has come (schedule.At is Run, which includes a late start within
// schedule.LateStart after TasmoFleet was down), the device must pass the start check of the application
// (Options.StartCheck: the device lock and the firmware holds, as a person's Submit) and must not be excluded
// from OTA; the job then becomes queued (event start_due) and runs the normal preflight, so identity, Guard
// and plan are checked again. A start that passed by more than the window ends the job cancelled and
// NotStarted (event not_started); it never starts later.

// startRetry is how soon a start the device refused (a person's operation or TasmoFleet's own short read
// holds the device, or a firmware update typed in the console is still running) is tried again, inside the
// window.
const startRetry = 30 * time.Second

// scheduledRecheck caps every wait for a start: the engine reads the clock again at least this often. A
// timer counts monotonic time, which stands still while the host sleeps; the start must follow the wall clock.
const scheduledRecheck = time.Minute

// startRefusal is the last refusal of a due start: when to try again and why it was refused.
type startRefusal struct {
	next   time.Time
	reason string
}

// startDue handles the scheduled jobs among active, in place: a job whose start has come and passes the
// checks becomes queued (so the caller starts it like any queued job), one whose window has passed ends. It
// returns when to look again: the earliest start or retry still ahead (zero: no scheduled job waits).
func (e *Engine) startDue(ctx context.Context, active []model.Job) time.Time {
	now := e.clk.Now()
	var next time.Time
	for i, j := range active {
		if j.State != model.JobScheduled {
			continue
		}
		var at time.Time
		switch schedule.At(j.StartAt, now, schedule.LateStart) {
		case schedule.Wait:
			at = j.StartAt
		case schedule.Run:
			active[i], at = e.startScheduled(ctx, j, now)
		case schedule.Missed:
			active[i] = e.endScheduled(ctx, j)
			at = retryIfWaiting(active[i], now)
		}
		if !at.IsZero() && (next.IsZero() || at.Before(next)) {
			next = at
		}
	}
	return next
}

// startScheduled runs the start check for a due job and queues it. It returns the job as it is now and,
// when the device refused, when to try again.
func (e *Engine) startScheduled(ctx context.Context, j model.Job, now time.Time) (model.Job, time.Time) {
	e.mu.Lock()
	r, refusedBefore := e.refusedStarts[j.ID]
	e.mu.Unlock()
	if refusedBefore && now.Before(r.next) {
		return j, r.next
	}
	d, err := e.devices.GetDevice(ctx, j.DeviceID)
	if err != nil {
		return e.startRefused(j, now, "inventory: "+err.Error())
	}
	if d.OTAExcluded {
		// The preview refused excluded devices; one excluded after the plan is refused now, for good.
		got := e.applyScheduled(ctx, j, Event{Kind: EvGuardRefused, Err: "excluded from OTA since the update was planned; it was not started"})
		return got, retryIfWaiting(got, now)
	}
	if e.opts.StartCheck != nil {
		release, err := e.opts.StartCheck(ctx, j.DeviceID)
		if err != nil {
			return e.startRefused(j, now, err.Error())
		}
		// Held until the queued state is stored: from then on the active job keeps the others away.
		defer release()
	}
	late := now.Sub(j.StartAt).Round(time.Second)
	got := e.applyScheduled(ctx, j, Event{Kind: EvStartDue, Err: fmt.Sprintf("planned for %s, started %s after it", WhenText(j.StartAt), late)})
	if got.State == model.JobQueued {
		e.auditScheduledStart(ctx, got, d.Name, late)
	}
	return got, retryIfWaiting(got, now)
}

// SchedulerActor is who the audit log and the backup of an update name for what TasmoFleet's own scheduler does:
// the person planned the update, but the start and every step after it are not their action (#824).
const SchedulerActor = "system"

// stepActor is who a step of the job's run is booked to. A job started at once is its creator's action; a
// planned one starts by itself, so its steps are the scheduler's.
func stepActor(j model.Job) string {
	if j.StartAt.IsZero() {
		return j.CreatedBy
	}
	return SchedulerActor
}

// auditScheduledStart records that a planned update started, as the scheduler, with the time that was planned
// and how late it began (#824). The person who planned it is in the entry of the plan ("transaction.start").
func (e *Engine) auditScheduledStart(ctx context.Context, j model.Job, device string, late time.Duration) {
	if e.audit == nil {
		return
	}
	detail, _ := json.Marshal(map[string]any{ // plain strings and numbers always encode
		"device": device, "device_id": j.DeviceID, "release": j.TargetRelease,
		"planned_start": j.StartAt.UTC().Format(time.RFC3339), "late_seconds": int64(late / time.Second),
	})
	e.audit.Record(ctx, model.AuditEvent{Actor: SchedulerActor, Action: "transaction.scheduled_start",
		Target: "transaction:" + strconv.FormatInt(int64(j.BatchID), 10), Detail: string(detail), Result: "ok"})
}

// retryIfWaiting is when to look at j again: soon when it still waits although it was due (its job could
// not be read or its transition not stored), never when it has left the state.
func retryIfWaiting(j model.Job, now time.Time) time.Time {
	if j.State == model.JobScheduled {
		return now.Add(startRetry)
	}
	return time.Time{}
}

// startRefused remembers why a due start was refused and when to try again.
func (e *Engine) startRefused(j model.Job, now time.Time, reason string) (model.Job, time.Time) {
	next := now.Add(startRetry)
	e.mu.Lock()
	e.refusedStarts[j.ID] = startRefusal{next: next, reason: reason}
	e.mu.Unlock()
	e.log.Info("ota: scheduled start refused, trying again", "job", j.ID, "device", j.DeviceID, "reason", reason, "next", next)
	return j, next
}

// endScheduled ends a job whose window has passed. A job this engine tried to start and the device refused
// until the end of the window failed with that reason; any other was not started because TasmoFleet was down.
func (e *Engine) endScheduled(ctx context.Context, j model.Job) model.Job {
	e.mu.Lock()
	r, refusedBefore := e.refusedStarts[j.ID]
	e.mu.Unlock()
	ev := Event{Kind: EvNotStarted}
	if refusedBefore {
		ev = Event{Kind: EvGuardRefused, Err: fmt.Sprintf("the device was not free from the planned start (%s) until %d minutes after it, so the update was not started: %s",
			WhenText(j.StartAt), int(schedule.LateStart/time.Minute), r.reason)}
	}
	return e.applyScheduled(ctx, j, ev)
}

// applyScheduled applies ev to the stored job if it still waits for its start, and returns the job as it is
// now: seen unchanged when it could not be read or its transition not stored (the caller tries again). It
// holds e.mu like Cancel, so a cancel and a start never both act on the same scheduled job.
func (e *Engine) applyScheduled(ctx context.Context, seen model.Job, ev Event) model.Job {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, err := e.jobs.GetJob(ctx, seen.ID)
	if err != nil {
		e.log.Warn("ota: scheduled job not read, trying again", "job", seen.ID, "err", err)
		return seen
	}
	if j.State != model.JobScheduled {
		return j // cancelled meanwhile
	}
	next, _, ok := e.apply(ctx, j, ev)
	if ok && next.State != model.JobScheduled {
		delete(e.refusedStarts, j.ID)
	}
	return next
}
